package councilproxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	cleanupTimeout           = 10 * time.Second
	defaultHostGatewayTarget = "host-gateway"
)

// ResourceLabel marks each generated proxy and network with its own name.
const ResourceLabel = "sablecourt.io/council-proxy"

// Docker is implemented by the shared runtime. EgressGateway returns the egress
// bridge's gateway IP when known, or an empty string when Docker exposes no local
// bridge gateway. CreateProxy uses a local image ID, joins only InternalNetwork
// initially, maps HostGateway to HostGatewayTarget, and sets no mounts or
// published ports. ConnectProxyEgress joins Docker's egress bridge and gives it
// default gateway priority. StartProxy returns success once the proxy listens on
// the private network. Remove methods must verify the generated resource's
// ownership label and treat an absent resource as success. Connection errors can
// leave creations committed.
type Docker interface {
	EgressGateway(ctx context.Context) (string, error)
	CreateInternalNetwork(ctx context.Context, name string) error
	CreateProxy(ctx context.Context, spec ProxyContainer) error
	ConnectProxyEgress(ctx context.Context, name string) error
	StartProxy(ctx context.Context, name string) error
	RemoveProxy(ctx context.Context, name string) error
	RemoveInternalNetwork(ctx context.Context, name string) error
}

type ProxyContainer struct {
	Name, InternalNetwork, ImageID string
	User, HostGateway              string
	HostGatewayTarget              string
	ReadOnlyRootFS                 bool
	DropAllCapabilities            bool
	NoNewPrivileges                bool
	Args                           []string
}

type WorkloadEndpoint struct {
	Network, ProxyURL, DataGateURL string
	proxyName                      string
}

// Env supplies proxy settings for a workload that joins Network alone and
// starts with a clean environment.
func (w WorkloadEndpoint) Env() []string {
	if w.ProxyURL == "" {
		return nil
	}
	env := []string{
		"HTTP_PROXY=" + w.ProxyURL, "HTTPS_PROXY=" + w.ProxyURL,
		"http_proxy=" + w.ProxyURL, "https_proxy=" + w.ProxyURL,
		"NO_PROXY=" + w.proxyName, "no_proxy=" + w.proxyName,
	}
	if w.DataGateURL != "" {
		env = append(env, "SABLECOURT_DATA_GATE_URL="+w.DataGateURL)
	}
	return env
}

type Session struct {
	mu       sync.Mutex
	docker   Docker
	network  string
	proxy    string
	bridge   *gateBridge
	endpoint WorkloadEndpoint
}

type gateBridge struct {
	port      string
	server    *http.Server
	transport *http.Transport
	done      chan struct{}
}

func (b *gateBridge) close() {
	if b == nil {
		return
	}
	_ = b.server.Close()
	<-b.done
	b.transport.CloseIdleConnections()
}

var (
	isLocalHostIP = localHostInterfaceIP
	listenBridge  = net.Listen
)

func localHostInterfaceIP(target netip.Addr) bool {
	target = target.Unmap()
	if !target.IsValid() || target.IsLoopback() || target.IsUnspecified() {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if parsed, ok := netip.AddrFromSlice(ip); ok && parsed.Unmap() == target {
			return true
		}
	}
	return false
}

func startGateBridge(gateway netip.Addr, cfg Config) (*gateBridge, error) {
	gate, err := GateAuthority(cfg.DaemonListen)
	if err != nil {
		return nil, err
	}
	digest, err := hex.DecodeString(cfg.LeaseSHA256)
	if err != nil || len(digest) != sha256.Size {
		return nil, errors.New("invalid council task lease digest")
	}
	ln, err := listenBridge("tcp", net.JoinHostPort(gateway.String(), "0"))
	if err != nil {
		return nil, fmt.Errorf("listen council gate bridge: %w", err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil || !validPort(port) {
		_ = ln.Close()
		return nil, errors.New("resolve council gate bridge port")
	}
	transport := newGateTransport()
	reverse := newGateReverseProxy(&url.URL{Scheme: "http", Host: cfg.DaemonListen}, gate, transport)
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != gate || !AllowsGateRequest(r) || r.Header.Get("Upgrade") != "" {
				http.Error(w, "council route refused", http.StatusForbidden)
				return
			}
			if validLeaseHeader(r, digest) {
				reverse.ServeHTTP(w, r)
				return
			}
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "task lease required", http.StatusUnauthorized)
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(ln)
	}()
	return &gateBridge{port: port, server: server, transport: transport, done: done}, nil
}

// Provision builds a private network and one hardened proxy per launch. On
// native Linux, where Docker's egress bridge gateway is a local host interface
// and cannot reach a loopback-only daemon directly, Provision binds a scoped
// per-session gate bridge on that gateway IP. A failed launch returns a session
// only if cleanup must be retried.
func Provision(ctx context.Context, docker Docker, imageID string, cfg Config) (*Session, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	id, ok := strings.CutPrefix(imageID, "sha256:")
	digest, err := hex.DecodeString(id)
	if !ok || err != nil || len(digest) != sha256.Size || strings.ToLower(id) != id {
		return nil, errors.New("council proxy requires a local image ID")
	}
	gatewayText, err := docker.EgressGateway(ctx)
	if err != nil {
		return nil, fmt.Errorf("inspect council egress gateway: %w", err)
	}
	hostGatewayTarget := defaultHostGatewayTarget
	gatePort := cfg.GatePort
	var bridge *gateBridge
	if gatewayText != "" {
		gw, err := netip.ParseAddr(strings.TrimSpace(gatewayText))
		gw = gw.Unmap()
		if err != nil || !gw.IsValid() || gw.IsLoopback() || gw.IsUnspecified() {
			return nil, errors.New("invalid council egress gateway address")
		}
		if isLocalHostIP(gw) {
			hostGatewayTarget = gw.String()
			if cfg.LeaseSHA256 != "" {
				bridge, err = startGateBridge(gw, cfg)
				if err != nil {
					return nil, err
				}
				gatePort = bridge.port
			}
		}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		bridge.close()
		return nil, fmt.Errorf("generate council proxy name: %w", err)
	}
	name := "sablecourt-council-" + hex.EncodeToString(nonce[:])
	s := &Session{docker: docker, network: name + "-net", bridge: bridge}
	if err := docker.CreateInternalNetwork(ctx, s.network); err != nil {
		return s.rollback(ctx, fmt.Errorf("create council network: %w", err))
	}
	s.proxy = name
	args := []string{
		"-harness", cfg.Harness, "-daemon-listen", cfg.DaemonListen,
		"-lease-sha256", cfg.LeaseSHA256, "-listen-name", name,
	}
	if gatePort != "" {
		args = append(args, "-gate-port", gatePort)
	}
	if err := docker.CreateProxy(ctx, ProxyContainer{
		Name: name, InternalNetwork: s.network, ImageID: imageID,
		User: "10001:10001", HostGateway: DockerHostGateway, HostGatewayTarget: hostGatewayTarget,
		ReadOnlyRootFS: true, DropAllCapabilities: true, NoNewPrivileges: true,
		Args: args,
	}); err != nil {
		return s.rollback(ctx, fmt.Errorf("create council proxy: %w", err))
	}
	if err := docker.ConnectProxyEgress(ctx, name); err != nil {
		return s.rollback(ctx, fmt.Errorf("attach council proxy egress: %w", err))
	}
	if err := docker.StartProxy(ctx, name); err != nil {
		return s.rollback(ctx, fmt.Errorf("start council proxy: %w", err))
	}
	s.endpoint = WorkloadEndpoint{
		Network: s.network, ProxyURL: "http://" + net.JoinHostPort(name, Port), proxyName: name,
	}
	if cfg.LeaseSHA256 != "" {
		s.endpoint.DataGateURL = s.endpoint.ProxyURL + "/gate/mcp"
	}
	return s, nil
}

func (s *Session) Workload() WorkloadEndpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endpoint
}

func (s *Session) rollback(ctx context.Context, launchErr error) (*Session, error) {
	if err := s.Close(ctx); err != nil {
		return s, errors.Join(launchErr, err)
	}
	return nil, launchErr
}

// Close closes any host-side gate bridge and removes the proxy before its
// network. Stop the workload before closing its session. Cleanup can be retried
// after a failed removal.
func (s *Session) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endpoint = WorkloadEndpoint{}
	if s.bridge != nil {
		s.bridge.close()
		s.bridge = nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if s.proxy != "" {
		if err := s.docker.RemoveProxy(ctx, s.proxy); err != nil {
			return fmt.Errorf("remove council proxy: %w", err)
		}
		s.proxy = ""
	}
	if s.network != "" {
		if err := s.docker.RemoveInternalNetwork(ctx, s.network); err != nil {
			return fmt.Errorf("remove council network: %w", err)
		}
		s.network = ""
	}
	return nil
}
