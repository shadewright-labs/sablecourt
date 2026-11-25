package councilproxy

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/operatortoken"
)

const (
	ContainerGateSocket = "/gate.sock"
	PreflightPath       = "/gate/_preflight"
	LaunchHeader        = "X-Sablecourt-Gate-Launch"
	ChallengeHeader     = "X-Sablecourt-Gate-Challenge"
	ProofHeader         = "X-Sablecourt-Gate-Proof"
)

type GateMode string

const (
	GateModeNone        GateMode = "none"
	GateModeUnixSocket  GateMode = "unix-socket"
	GateModeHostGateway GateMode = "host-gateway"
)

type LaunchEvidence struct {
	Network      string   `json:"network"`
	Proxy        string   `json:"proxy"`
	ProxyImageID string   `json:"proxy_image_id"`
	GateMode     GateMode `json:"gate_mode"`
}

type registryKey struct {
	authority string
	launchID  string
}

type Registry struct {
	mu      sync.RWMutex
	entries map[registryKey][32]byte
}

var DefaultRegistry = NewRegistry()

func NewRegistry() *Registry {
	return &Registry{entries: make(map[registryKey][32]byte)}
}

func (r *Registry) Register(authority, launchID string, secret [32]byte) {
	authority = strings.ToLower(strings.TrimSpace(authority))
	if r == nil || authority == "" || !validLaunchID(launchID) {
		return
	}
	r.mu.Lock()
	r.entries[registryKey{authority: authority, launchID: launchID}] = secret
	r.mu.Unlock()
}

func (r *Registry) Unregister(authority, launchID string) {
	authority = strings.ToLower(strings.TrimSpace(authority))
	if r == nil || authority == "" || launchID == "" {
		return
	}
	r.mu.Lock()
	delete(r.entries, registryKey{authority: authority, launchID: launchID})
	r.mu.Unlock()
}

func IsPreflightRequest(r *http.Request) bool {
	if r == nil || r.Method != http.MethodHead || r.URL == nil {
		return false
	}
	u := r.URL
	return !u.IsAbs() && u.Host == "" && u.Opaque == "" && u.User == nil &&
		u.RawPath == "" && u.RawQuery == "" && u.Path == PreflightPath &&
		r.Header.Get("Upgrade") == ""
}

func (r *Registry) HandlePreflight(w http.ResponseWriter, req *http.Request, authority string) {
	w.Header().Set("Cache-Control", "no-store")
	authority = strings.ToLower(strings.TrimSpace(authority))
	if r == nil || authority == "" || !IsPreflightRequest(req) || len(req.Header.Values("Authorization")) != 0 {
		http.Error(w, "council gate preflight refused", http.StatusForbidden)
		return
	}
	launches := req.Header.Values(LaunchHeader)
	challenges := req.Header.Values(ChallengeHeader)
	if len(launches) != 1 || len(challenges) != 1 ||
		!validLaunchID(launches[0]) || !validHexBytes(challenges[0], 16, 64) {
		http.Error(w, "council gate preflight refused", http.StatusForbidden)
		return
	}
	r.mu.RLock()
	secret, ok := r.entries[registryKey{authority: authority, launchID: launches[0]}]
	r.mu.RUnlock()
	if !ok {
		http.Error(w, "council gate preflight refused", http.StatusForbidden)
		return
	}
	w.Header().Set(ProofHeader, ComputeGateProof(secret, authority, launches[0], challenges[0]))
	w.WriteHeader(http.StatusNoContent)
}

func HandleGatePreflight(w http.ResponseWriter, req *http.Request, authority string) {
	DefaultRegistry.HandlePreflight(w, req, authority)
}

func ComputeGateProof(secret [32]byte, authority, launchID, challenge string) string {
	mac := hmac.New(sha256.New, secret[:])
	_, _ = io.WriteString(mac, strings.ToLower(strings.TrimSpace(authority))+"\n"+launchID+"\n"+challenge)
	return hex.EncodeToString(mac.Sum(nil))
}

func validLaunchID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c <= 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

func validHexBytes(raw string, minBytes, maxBytes int) bool {
	decoded, err := hex.DecodeString(raw)
	return err == nil && len(decoded) >= minBytes && len(decoded) <= maxBytes
}

func SocketRoot(stateDir string) string {
	return filepath.Join(stateDir, "gates")
}

func ownedPrivateDir(info os.FileInfo) bool {
	if info == nil || info.Mode() != os.ModeDir|0o700 {
		return false
	}
	uid := os.Geteuid()
	if uid < 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int64(stat.Uid) == int64(uid)
}

func openPrivateRoot(dir string) (*os.Root, error) {
	clean := filepath.Clean(dir)
	if !filepath.IsAbs(clean) || clean == string(filepath.Separator) {
		return nil, errors.New("council gate socket directory must be an absolute path")
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return nil, err
	}
	if !ownedPrivateDir(info) {
		return nil, errors.New("council gate socket directory must be owned by the current user with mode 0700")
	}
	root, err := os.OpenRoot(clean)
	if err != nil {
		return nil, err
	}
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(info, actual) || !ownedPrivateDir(actual) {
		_ = root.Close()
		return nil, errors.New("council gate socket directory changed while opening it")
	}
	return root, nil
}

func ensureSocketRoot(sockRoot string) (string, error) {
	if strings.TrimSpace(sockRoot) == "" {
		stateDir, err := operatortoken.StateDir()
		if err != nil {
			return "", fmt.Errorf("resolve council gate state directory: %w", err)
		}
		if _, err := os.Lstat(stateDir); errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(stateDir, 0o700); err != nil {
				return "", err
			}
			if err := os.Chmod(stateDir, 0o700); err != nil {
				return "", err
			}
		}
		root, err := openPrivateRoot(stateDir)
		if err != nil {
			return "", err
		}
		_ = root.Close()
		sockRoot = SocketRoot(stateDir)
	}
	clean := filepath.Clean(sockRoot)
	if !filepath.IsAbs(clean) || clean == string(filepath.Separator) {
		return "", errors.New("council gate socket root must be an absolute path")
	}
	if _, err := os.Lstat(clean); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(clean, 0o700); err != nil {
			return "", err
		}
		if err := os.Chmod(clean, 0o700); err != nil {
			return "", err
		}
	}
	root, err := openPrivateRoot(clean)
	if err != nil {
		return "", err
	}
	_ = root.Close()
	return clean, nil
}

// SweepStaleSockets removes leftover per-launch socket directories from prior daemon runs.
func SweepStaleSockets(sockRoot string) error {
	clean := filepath.Clean(sockRoot)
	if !filepath.IsAbs(clean) || clean == string(filepath.Separator) {
		return errors.New("council gate socket root must be an absolute path")
	}
	if _, err := os.Lstat(clean); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	root, err := openPrivateRoot(clean)
	if err != nil {
		return err
	}
	defer root.Close()
	dirFile, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := dirFile.ReadDir(-1)
	_ = dirFile.Close()
	if readErr != nil {
		return readErr
	}
	for _, entry := range entries {
		if err := root.RemoveAll(entry.Name()); err != nil {
			return fmt.Errorf("remove stale council gate socket entry %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func createPrivateUnixSocket(sockRoot, prefix string) (sockPath string, ln net.Listener, cleanup func() error, err error) {
	rootPath, err := ensureSocketRoot(sockRoot)
	if err != nil {
		return "", nil, nil, err
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, nil, err
	}
	subName := prefix + hex.EncodeToString(raw[:])
	root, err := openPrivateRoot(rootPath)
	if err != nil {
		return "", nil, nil, err
	}
	defer root.Close()
	if err := root.Mkdir(subName, 0o700); err != nil {
		return "", nil, nil, err
	}
	launchDir := filepath.Join(rootPath, subName)
	if err := os.Chmod(launchDir, 0o700); err != nil {
		_ = root.RemoveAll(subName)
		return "", nil, nil, err
	}
	subInfo, err := root.Lstat(subName)
	if err != nil || !ownedPrivateDir(subInfo) {
		_ = root.RemoveAll(subName)
		return "", nil, nil, errors.New("council gate launch socket directory failed permission check")
	}
	sockPath = filepath.Join(launchDir, "gate.sock")
	uln, err := net.Listen("unix", sockPath)
	if err != nil {
		_ = root.RemoveAll(subName)
		return "", nil, nil, fmt.Errorf("listen council gate unix socket: %w", err)
	}
	// The parent directory is 0700 and owned by the current user, blocking other host users;
	// the socket inode itself needs write permission so unprivileged container UID 10001 can connect
	// when the single socket file is bind-mounted into the proxy container.
	if err := os.Chmod(sockPath, 0o666); err != nil { // #nosec G302 -- parent launchDir and sockRoot are 0700 owned by daemon user
		_ = uln.Close()
		_ = root.RemoveAll(subName)
		return "", nil, nil, err
	}
	var once sync.Once
	var cleanErr error
	cleanup = func() error {
		once.Do(func() {
			_ = uln.Close()
			r, openErr := openPrivateRoot(rootPath)
			if openErr != nil {
				cleanErr = errors.Join(openErr, os.RemoveAll(launchDir))
				return
			}
			defer r.Close()
			cleanErr = r.RemoveAll(subName)
		})
		return cleanErr
	}
	return sockPath, uln, cleanup, nil
}

func probeUnixSocketMode(ctx context.Context, docker Docker, imageID, proxyName, sockRoot string) (bool, error) {
	sockPath, ln, cleanup, err := createPrivateUnixSocket(sockRoot, "p-")
	if err != nil {
		return false, err
	}
	defer func() { _ = cleanup() }()

	var tokenRaw [16]byte
	if _, err := rand.Read(tokenRaw[:]); err != nil {
		return false, err
	}
	probeToken := hex.EncodeToString(tokenRaw[:])
	var verified atomic.Bool
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		line, readErr := bufio.NewReader(io.LimitReader(conn, 128)).ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return
		}
		got := strings.TrimSpace(line)
		if subtle.ConstantTimeCompare([]byte(got), []byte(probeToken)) == 1 {
			verified.Store(true)
			_, _ = io.WriteString(conn, "ok\n")
		}
	}()

	probeErr := docker.ProbeGateSocket(ctx, GateSocketProbe{
		Name:                proxyName + "-probe",
		ImageID:             imageID,
		User:                "10001:10001",
		HostSocketPath:      sockPath,
		ContainerSocketPath: ContainerGateSocket,
		ReadOnlyRootFS:      true,
		DropAllCapabilities: true,
		NoNewPrivileges:     true,
		Args:                []string{"-probe-gate-socket", ContainerGateSocket, "-probe-token", probeToken},
	})
	_ = ln.Close()
	<-acceptDone
	if cleanErr := cleanup(); cleanErr != nil {
		return false, cleanErr
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return probeErr == nil && verified.Load(), nil
}

// ProbeGateSocketClient runs inside the short-lived probe container to verify Unix socket reachability.
func ProbeGateSocketClient(socketPath, token string) error {
	if socketPath == "" || token == "" {
		return errors.New("probe requires socket path and token")
	}
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, token+"\n"); err != nil {
		return err
	}
	reply, err := bufio.NewReader(io.LimitReader(conn, 64)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.TrimSpace(reply) != "ok" {
		return errors.New("probe acknowledgement mismatch")
	}
	return nil
}

type unixGate struct {
	sockPath  string
	server    *http.Server
	transport *http.Transport
	done      chan struct{}
	cleanup   func() error
}

func startUnixGate(sockRoot string, cfg Config, limits GateLimits) (*unixGate, error) {
	gate, err := GateAuthority(cfg.DaemonListen)
	if err != nil {
		return nil, err
	}
	digest, err := hex.DecodeString(cfg.LeaseSHA256)
	if err != nil || len(digest) != sha256.Size {
		return nil, errors.New("invalid council task lease digest")
	}
	limits = limits.normalized()
	sockPath, ln, cleanup, err := createPrivateUnixSocket(sockRoot, "g-")
	if err != nil {
		return nil, err
	}
	transport := newGateTransport()
	reverse := newGateReverseProxy(&url.URL{Scheme: "http", Host: cfg.DaemonListen}, gate, transport)
	handler := gateRouteHandler(gate, digest, WrapGateHandler(reverse, limits))
	srv := NewGateServer(handler, limits)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(LimitListener(ln, limits.MaxConns))
	}()
	return &unixGate{
		sockPath:  sockPath,
		server:    srv,
		transport: transport,
		done:      done,
		cleanup:   cleanup,
	}, nil
}

func (g *unixGate) close() error {
	if g == nil {
		return nil
	}
	_ = g.server.Close()
	<-g.done
	g.transport.CloseIdleConnections()
	return g.cleanup()
}
