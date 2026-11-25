package councilproxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

const cleanupTimeout = 10 * time.Second

// ResourceLabel marks each generated proxy and network with its own name.
const ResourceLabel = "sablecourt.io/council-proxy"

// Docker is implemented by the shared runtime. CreateProxy uses a local image ID,
// joins only InternalNetwork initially, and sets no mounts or published ports.
// ConnectProxyEgress joins Docker's egress bridge and gives it default gateway priority.
// StartProxy returns success once the proxy listens on the private network.
// Remove methods must verify the generated resource's ownership label and treat
// an absent resource as success. Connection errors can leave creations committed.
type Docker interface {
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
	ReadOnlyRootFS                 bool
	DropAllCapabilities            bool
	NoNewPrivileges                bool
	Args                           []string
}

type WorkloadEndpoint struct {
	Network, ProxyURL, DataGateURL string
	proxyName                      string
}

// ProxyName returns the generated container name of the pinned proxy.
func (w WorkloadEndpoint) ProxyName() string {
	return w.proxyName
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
	endpoint WorkloadEndpoint
}

// Provision builds a private network and one hardened proxy per launch. A failed
// launch returns a session only if cleanup must be retried.
func Provision(ctx context.Context, docker Docker, imageID string, cfg Config) (*Session, error) {
	if err := cfg.validate(); err != nil {
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
	s := &Session{docker: docker, network: name + "-net"}
	if err := docker.CreateInternalNetwork(ctx, s.network); err != nil {
		return s.rollback(ctx, fmt.Errorf("create council network: %w", err))
	}
	s.proxy = name
	if err := docker.CreateProxy(ctx, ProxyContainer{
		Name: name, InternalNetwork: s.network, ImageID: imageID,
		User: "10001:10001", HostGateway: DockerHostGateway,
		ReadOnlyRootFS: true, DropAllCapabilities: true, NoNewPrivileges: true,
		Args: []string{
			"-harness", cfg.Harness, "-daemon-listen", cfg.DaemonListen,
			"-lease-sha256", cfg.LeaseSHA256, "-listen-name", name,
		},
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

// Close removes the proxy before its network. Stop the workload before closing
// its session. Cleanup can be retried after a failed removal.
func (s *Session) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endpoint = WorkloadEndpoint{}
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
