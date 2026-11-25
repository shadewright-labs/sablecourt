package councilproxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cleanupTimeout           = 10 * time.Second
	defaultHostGatewayTarget = "host-gateway"
	BridgeGatewayModeIPv4    = "com.docker.network.bridge.gateway_mode_ipv4"
	BridgeGatewayModeIPv6    = "com.docker.network.bridge.gateway_mode_ipv6"
	GatewayModeIsolated      = "isolated"
)

// ResourceLabel marks each generated proxy and network with its own name.
const ResourceLabel = "sablecourt.io/council-proxy"

type GateSocketProbe struct {
	Name                string
	ImageID             string
	User                string
	HostSocketPath      string
	ContainerSocketPath string
	ReadOnlyRootFS      bool
	DropAllCapabilities bool
	NoNewPrivileges     bool
	Args                []string
}

type InternalNetworkSpec struct {
	Name    string
	Options map[string]string
}

type InternalNetworkInfo struct {
	EngineVersion string
	Driver        string
	Internal      bool
	Options       map[string]string
	Subnets       []string
	Gateways      []string
}

// Docker is implemented by the container runtime. ProbeGateSocket tests whether a
// read-only, non-root container can connect to a daemon-owned Unix socket on the host.
// CreateInternalNetwork creates an --internal bridge network with isolated gateway
// options so harness containers cannot reach host listeners via the bridge gateway.
// CreateProxy uses a local image ID, joins only InternalNetwork initially, mounts only
// GateSocketHostPath when set (never into harness containers), and sets no published ports.
type Docker interface {
	ProbeGateSocket(ctx context.Context, spec GateSocketProbe) error
	CreateInternalNetwork(ctx context.Context, spec InternalNetworkSpec) (InternalNetworkInfo, error)
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
	GateSocketHostPath             string
	GateSocketContainerPath        string
	ReadOnlyRootFS                 bool
	DropAllCapabilities            bool
	NoNewPrivileges                bool
	Args                           []string
}

type WorkloadEndpoint struct {
	Network, ProxyURL, DataGateURL string
	GateMode                       GateMode
	proxyName                      string
}

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
	unixGate *unixGate
	registry *Registry
	gateAuth string
	launchID string
	endpoint WorkloadEndpoint
	evidence LaunchEvidence
}

// Provision builds an isolated internal network and one hardened proxy per launch.
// When a task lease is present, Provision probes whether a container can dial a
// per-launch Unix socket on the host; when reachable it mounts that socket into the
// proxy container only, and otherwise falls back to host-gateway with per-launch
// preflight proof verification. A failed launch returns a session only if Docker
// cleanup must be retried.
func Provision(ctx context.Context, docker Docker, imageID string, cfg Config) (*Session, error) {
	if err := cfg.validateProvision(); err != nil {
		return nil, err
	}
	id, ok := strings.CutPrefix(imageID, "sha256:")
	digest, err := hex.DecodeString(id)
	if !ok || err != nil || len(digest) != sha256.Size || strings.ToLower(id) != id {
		return nil, errors.New("council proxy requires a local image ID")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("generate council proxy name: %w", err)
	}
	name := "sablecourt-council-" + hex.EncodeToString(nonce[:])
	gateAuth, _ := GateAuthority(cfg.DaemonListen)
	reg := cfg.Registry
	if reg == nil {
		reg = DefaultRegistry
	}

	mode := GateModeNone
	var ug *unixGate
	var gateSecret [32]byte
	var registeredLaunch string
	if cfg.LeaseSHA256 != "" {
		useUnix, err := probeUnixSocketMode(ctx, docker, imageID, name, cfg.SocketRoot)
		if err != nil {
			return nil, fmt.Errorf("probe council gate socket: %w", err)
		}
		if useUnix {
			ug, err = startUnixGate(cfg.SocketRoot, cfg, DefaultGateLimits())
			if err != nil {
				return nil, err
			}
			mode = GateModeUnixSocket
		} else {
			if _, err := rand.Read(gateSecret[:]); err != nil {
				return nil, fmt.Errorf("generate council gate secret: %w", err)
			}
			reg.Register(gateAuth, name, gateSecret)
			registeredLaunch = name
			mode = GateModeHostGateway
		}
	}

	s := &Session{
		docker:   docker,
		network:  name + "-net",
		unixGate: ug,
		registry: reg,
		gateAuth: gateAuth,
		launchID: registeredLaunch,
	}
	netInfo, err := docker.CreateInternalNetwork(ctx, InternalNetworkSpec{
		Name: s.network,
		Options: map[string]string{
			BridgeGatewayModeIPv4: GatewayModeIsolated,
			BridgeGatewayModeIPv6: GatewayModeIsolated,
		},
	})
	if err != nil {
		return s.rollback(ctx, fmt.Errorf("create council network: %w", err))
	}
	if err := validateIsolatedNetwork(netInfo); err != nil {
		return s.rollback(ctx, err)
	}

	s.proxy = name
	args := []string{
		"-harness", cfg.Harness, "-daemon-listen", cfg.DaemonListen,
		"-lease-sha256", cfg.LeaseSHA256, "-listen-name", name,
	}
	spec := ProxyContainer{
		Name: name, InternalNetwork: s.network, ImageID: imageID,
		User:           "10001:10001",
		ReadOnlyRootFS: true, DropAllCapabilities: true, NoNewPrivileges: true,
	}
	switch mode {
	case GateModeUnixSocket:
		spec.GateSocketHostPath = ug.sockPath
		spec.GateSocketContainerPath = ContainerGateSocket
		spec.Args = append(args, "-gate-socket", ContainerGateSocket)
	case GateModeHostGateway:
		spec.HostGateway = DockerHostGateway
		spec.HostGatewayTarget = defaultHostGatewayTarget
		spec.Args = append(args, "-gate-launch-id", name, "-gate-secret", hex.EncodeToString(gateSecret[:]))
	default:
		spec.Args = args
	}

	if err := docker.CreateProxy(ctx, spec); err != nil {
		return s.rollback(ctx, fmt.Errorf("create council proxy: %w", err))
	}
	if err := docker.ConnectProxyEgress(ctx, name); err != nil {
		return s.rollback(ctx, fmt.Errorf("attach council proxy egress: %w", err))
	}
	if err := docker.StartProxy(ctx, name); err != nil {
		return s.rollback(ctx, fmt.Errorf("start council proxy: %w", err))
	}
	s.endpoint = WorkloadEndpoint{
		Network:   s.network,
		ProxyURL:  "http://" + net.JoinHostPort(name, Port),
		GateMode:  mode,
		proxyName: name,
	}
	if cfg.LeaseSHA256 != "" {
		s.endpoint.DataGateURL = s.endpoint.ProxyURL + "/gate/mcp"
	}
	s.evidence = LaunchEvidence{
		Network:      s.network,
		Proxy:        name,
		ProxyImageID: imageID,
		GateMode:     mode,
	}
	return s, nil
}

func (s *Session) Workload() WorkloadEndpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endpoint
}

func (s *Session) Evidence() LaunchEvidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evidence
}

func (s *Session) rollback(ctx context.Context, launchErr error) (*Session, error) {
	if err := s.Close(ctx); err != nil {
		return s, errors.Join(launchErr, err)
	}
	return nil, launchErr
}

// Close tears down the per-launch Unix socket or preflight secret first so any
// surviving proxy immediately fails closed, then removes the proxy before its network.
func (s *Session) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endpoint = WorkloadEndpoint{}
	s.evidence = LaunchEvidence{}
	var sockErr error
	if s.unixGate != nil {
		sockErr = s.unixGate.close()
		s.unixGate = nil
	}
	if s.launchID != "" && s.registry != nil {
		s.registry.Unregister(s.gateAuth, s.launchID)
		s.launchID = ""
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if s.proxy != "" {
		if err := s.docker.RemoveProxy(ctx, s.proxy); err != nil {
			return errors.Join(sockErr, fmt.Errorf("remove council proxy: %w", err))
		}
		s.proxy = ""
	}
	if s.network != "" {
		if err := s.docker.RemoveInternalNetwork(ctx, s.network); err != nil {
			return errors.Join(sockErr, fmt.Errorf("remove council network: %w", err))
		}
		s.network = ""
	}
	return sockErr
}

func validateIsolatedNetwork(info InternalNetworkInfo) error {
	if !supportsIsolatedGateway(info.EngineVersion) {
		return fmt.Errorf("docker engine %q does not support isolated internal network gateway mode (requires Docker Engine 27.1+)", strings.TrimSpace(info.EngineVersion))
	}
	if info.Driver != "bridge" || !info.Internal {
		return errors.New("council network must be an internal bridge network")
	}
	if info.Options[BridgeGatewayModeIPv4] != GatewayModeIsolated ||
		info.Options[BridgeGatewayModeIPv6] != GatewayModeIsolated {
		return errors.New("docker engine did not apply isolated gateway mode on council internal network (requires Docker Engine 27.1+)")
	}
	if len(info.Subnets) == 0 {
		return errors.New("council internal network has no isolated subnet")
	}
	for _, gw := range info.Gateways {
		if strings.TrimSpace(gw) != "" {
			return errors.New("docker engine assigned a gateway to an isolated council internal network (requires Docker Engine 27.1+)")
		}
	}
	return nil
}

func supportsIsolatedGateway(version string) bool {
	parts := strings.Split(strings.TrimSpace(version), ".")
	if len(parts) < 2 {
		return false
	}
	major, err := parseVersionNumber(parts[0], true)
	if err != nil {
		return false
	}
	minor, err := parseVersionNumber(parts[1], false)
	if err != nil {
		return false
	}
	return major > 27 || (major == 27 && minor >= 1)
}

func parseVersionNumber(part string, exact bool) (int, error) {
	end := 0
	for end < len(part) && part[end] >= '0' && part[end] <= '9' {
		end++
	}
	if end == 0 || (exact && end != len(part)) {
		return 0, errors.New("invalid version component")
	}
	return strconv.Atoi(part[:end])
}
