package councilproxy

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
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
	ContainerGateSocket            = "/gate.sock"
	ProbeSocketUnsupportedExitCode = 42
	launchCertLifetime             = 2 * time.Hour
	launchCertRenewBefore          = launchCertLifetime / 2
	probeDrainTimeout              = 5 * time.Second
)

// ErrGateSocketUnsupported indicates that a bind-mounted host Unix socket cannot be connected from a container.
var ErrGateSocketUnsupported = errors.New("council gate unix socket is not shareable with container")

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
			if err := os.Chmod(stateDir, 0o700); err != nil { // #nosec G302 -- a directory needs the execute bit; 0700 keeps it private to the daemon user
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
		if err := os.Chmod(clean, 0o700); err != nil { // #nosec G302 -- a directory needs the execute bit; 0700 keeps it private to the daemon user
			return "", err
		}
	}
	root, err := openPrivateRoot(clean)
	if err != nil {
		return "", err
	}
	_ = root.Close()
	// Bind and Docker mount both resolve a pathname, so every ancestor must be
	// one that only this user or root can replace, and later steps use the
	// resolved path so no symlink is followed again.
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", err
	}
	if err := verifyTrustedAncestors(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func trustedOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	uid := os.Geteuid()
	return int64(stat.Uid) == 0 || (uid >= 0 && int64(stat.Uid) == int64(uid))
}

// verifyTrustedAncestors requires every directory above path to be owned by root
// or the current user and not replaceable by anyone else. A directory that group
// or others can write is accepted only when it is sticky and the entry beneath it
// is owned by a trusted user, as with a shared temporary directory.
func verifyTrustedAncestors(path string) error {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return errors.New("council gate path must be absolute")
	}
	child, err := os.Lstat(clean)
	if err != nil {
		return err
	}
	for dir := filepath.Dir(clean); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() || !trustedOwner(info) {
			return fmt.Errorf("council gate path ancestor %q is not a directory owned by root or the current user", dir)
		}
		if info.Mode().Perm()&0o022 != 0 && (info.Mode()&os.ModeSticky == 0 || !trustedOwner(child)) {
			return fmt.Errorf("council gate path ancestor %q can be replaced by another user", dir)
		}
		if dir == filepath.Dir(dir) {
			return nil
		}
		child = info
	}
}

// verifyGateSocketMountSource rechecks the host socket path immediately before
// it is bind-mounted: it must be canonical, a socket, and sit under trusted ancestors.
func verifyGateSocketMountSource(sockPath string) error {
	resolved, err := filepath.EvalSymlinks(sockPath)
	if err != nil {
		return err
	}
	if resolved != sockPath || filepath.Clean(sockPath) != sockPath {
		return errors.New("council gate socket mount source is not a canonical path")
	}
	info, err := os.Lstat(sockPath)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("council gate socket mount source is not a socket")
	}
	return verifyTrustedAncestors(sockPath)
}

// launchDirName reports whether name matches a per-launch directory created by
// createPrivateUnixSocket: a "g-" or "p-" prefix and 16 lowercase hex digits.
func launchDirName(name string) bool {
	rest, ok := strings.CutPrefix(name, "g-")
	if !ok {
		rest, ok = strings.CutPrefix(name, "p-")
	}
	if !ok || len(rest) != 16 {
		return false
	}
	for _, c := range rest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// SweepStaleSockets removes leftover per-launch socket directories from prior daemon runs.
// The caller must hold the state directory lock so a live daemon is never swept.
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
	defer root.Close() //nolint:errcheck // directory handle (os.Root); closing it finalizes no write
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
		// Only directories shaped like per-launch socket directories are stale
		// launches; any other entry in the socket root is left alone.
		if !entry.IsDir() || !launchDirName(entry.Name()) {
			continue
		}
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
	closeRootOnErr := true
	defer func() {
		if closeRootOnErr {
			_ = root.Close()
		}
	}()
	if err := root.Mkdir(subName, 0o700); err != nil {
		return "", nil, nil, err
	}
	if err := root.Chmod(subName, 0o700); err != nil {
		_ = root.RemoveAll(subName)
		return "", nil, nil, err
	}
	subInfo, err := root.Lstat(subName)
	if err != nil || !ownedPrivateDir(subInfo) {
		_ = root.RemoveAll(subName)
		return "", nil, nil, errors.New("council gate launch socket directory failed permission check")
	}
	launchDir := filepath.Join(rootPath, subName)
	sockPath = filepath.Join(launchDir, "gate.sock")
	uln, err := net.Listen("unix", sockPath)
	if err != nil {
		_ = root.RemoveAll(subName)
		return "", nil, nil, fmt.Errorf("listen council gate unix socket: %w", err)
	}
	// Closing the listener must not unlink by pathname, where a replacement
	// could be removed instead; rooted cleanup below removes the directory.
	if unixLn, ok := uln.(*net.UnixListener); ok {
		unixLn.SetUnlinkOnClose(false)
	}
	// The parent directory is 0700 and owned by the current user, blocking other host users;
	// the socket inode itself needs write permission so unprivileged container UID 10001 can connect
	// when the single socket file is bind-mounted into the proxy container.
	if err := root.Chmod(filepath.Join(subName, "gate.sock"), 0o666); err != nil { // #nosec G302 -- parent launchDir and sockRoot are 0700 owned by daemon user
		_ = uln.Close()
		_ = root.RemoveAll(subName)
		return "", nil, nil, err
	}
	closeRootOnErr = false
	var cleanupMu sync.Mutex
	cleaned := false
	// cleanup is retryable: the verified root handle stays open until the launch
	// directory is gone, so a failed removal can be repeated by a later call.
	cleanup = func() error {
		cleanupMu.Lock()
		defer cleanupMu.Unlock()
		if cleaned {
			return nil
		}
		_ = uln.Close()
		if err := root.RemoveAll(subName); err != nil {
			return err
		}
		cleaned = true
		return root.Close()
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
	var sentinelRaw [16]byte
	if _, err := rand.Read(sentinelRaw[:]); err != nil {
		return false, err
	}
	sentinel := hex.EncodeToString(sentinelRaw[:])
	var state probeState
	outcome, acceptDone := serveProbeListener(ln, sentinel, probeToken, &state)

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
	// Connections queue in order, so once the host's own marker connection has
	// been served, any connection the probe container made was served before it.
	// Closing the listener first could drop a queued connection unseen and let a
	// container that reached the host socket pass for one that could not.
	// An accept error or a timeout is not a drain: only the sentinel having been
	// consumed proves earlier queued connections were seen.
	drainErr := drainProbeListener(ctx, sockPath, sentinel)
	if drainErr == nil {
		drainErr = awaitProbeDrain(outcome, probeDrainTimeout)
	}
	_ = ln.Close()
	<-acceptDone
	if cleanErr := cleanup(); cleanErr != nil {
		return false, cleanErr
	}
	if drainErr != nil {
		return false, drainErr
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if errors.Is(probeErr, ErrGateSocketUnsupported) {
		if state.accepted.Load() {
			return false, errors.New("council gate socket probe accepted a host connection before reporting unsupported socket")
		}
		return false, nil
	}
	if probeErr != nil {
		return false, fmt.Errorf("council gate socket probe failed: %w", probeErr)
	}
	if !state.verified.Load() {
		return false, errors.New("council gate socket probe did not complete token exchange")
	}
	return true, nil
}

type probeState struct {
	accepted, verified atomic.Bool
}

// probeOutcome reports how the accept loop ended: drained is true only when the
// host's own sentinel connection was consumed; err carries an accept failure.
type probeOutcome struct {
	drained bool
	err     error
}

// serveProbeListener serves probe connections on ln until the sentinel is consumed
// or Accept fails. done closes when the loop has exited.
func serveProbeListener(ln net.Listener, sentinel, probeToken string, state *probeState) (<-chan probeOutcome, <-chan struct{}) {
	outcome := make(chan probeOutcome, 1)
	done := make(chan struct{})
	serve := func(conn net.Conn) (drained bool) {
		defer conn.Close() //nolint:errcheck // probe connection teardown
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		line, readErr := bufio.NewReader(io.LimitReader(conn, 128)).ReadString('\n')
		got := strings.TrimSpace(line)
		if subtle.ConstantTimeCompare([]byte(got), []byte(sentinel)) == 1 {
			return true
		}
		state.accepted.Store(true)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return false
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(probeToken)) == 1 {
			if _, writeErr := io.WriteString(conn, "ok\n"); writeErr == nil {
				state.verified.Store(true)
			}
		}
		return false
	}
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				outcome <- probeOutcome{err: err}
				return
			}
			if serve(conn) {
				outcome <- probeOutcome{drained: true}
				return
			}
		}
	}()
	return outcome, done
}

func awaitProbeDrain(outcome <-chan probeOutcome, timeout time.Duration) error {
	select {
	case o := <-outcome:
		if o.err != nil {
			return fmt.Errorf("council gate socket probe accept: %w", o.err)
		}
		if !o.drained {
			return errors.New("council gate socket probe did not consume its drain marker")
		}
		return nil
	case <-time.After(timeout):
		return errors.New("council gate socket probe did not drain pending connections")
	}
}

func drainProbeListener(ctx context.Context, sockPath, sentinel string) error {
	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(context.WithoutCancel(ctx), "unix", sockPath)
	if err != nil {
		return fmt.Errorf("council gate socket probe drain: %w", err)
	}
	defer conn.Close() //nolint:errcheck // probe connection teardown
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, sentinel+"\n"); err != nil {
		return fmt.Errorf("council gate socket probe drain: %w", err)
	}
	return nil
}

func isSocketNotShareable(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENOTSOCK) ||
		errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.ENOTSUP)
}

// IsUnshareableSocketBindError reports whether a failed Docker bind-mount probe
// specifically shows that the runtime cannot see or forward an existing host
// Unix socket (such as Docker Desktop rewriting socket bind mounts under
// /socket_mnt/... or denying host file sharing). Any other error returns false
// so callers fail closed.
func IsUnshareableSocketBindError(hostSocketPath string, exitCode int, output string) bool {
	if exitCode != 125 || hostSocketPath == "" {
		return false
	}
	info, err := os.Lstat(hostSocketPath)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return false
	}
	candidates := []string{filepath.Clean(hostSocketPath)}
	if resolved, err := filepath.EvalSymlinks(hostSocketPath); err == nil {
		resolved = filepath.Clean(resolved)
		if resolved != "" && resolved != candidates[0] {
			candidates = append(candidates, resolved)
		}
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if idx := strings.Index(line, "invalid mount config for type "); idx >= 0 {
			const marker = "bind source path does not exist:"
			if _, rest, ok := strings.Cut(line[idx:], marker); ok {
				mounted := strings.TrimSpace(rest)
				for _, cand := range candidates {
					if mounted == cand || mounted == "/socket_mnt"+cand || mounted == "/host_mnt"+cand {
						return true
					}
				}
			}
		}
	}
	if strings.Contains(output, "Mounts denied:") {
		for _, line := range strings.Split(output, "\n") {
			_, rest, ok := strings.Cut(line, "The path ")
			if !ok {
				continue
			}
			denied, _, ok := strings.Cut(rest, " is not shared from the host")
			if !ok {
				continue
			}
			denied = filepath.Clean(strings.TrimSpace(denied))
			for _, cand := range candidates {
				dir := filepath.Dir(cand)
				if denied == cand || (dir != string(filepath.Separator) && dir != "." && denied == dir) {
					return true
				}
			}
		}
	}
	return false
}

// ProbeGateSocketClient runs inside the short-lived probe container to verify Unix socket reachability.
func ProbeGateSocketClient(socketPath, token string) error {
	if socketPath == "" || token == "" {
		return errors.New("probe requires socket path and token")
	}
	info, err := os.Stat(socketPath)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return ErrGateSocketUnsupported
	}
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		if isSocketNotShareable(err) {
			return ErrGateSocketUnsupported
		}
		return err
	}
	defer conn.Close() //nolint:errcheck // probe connection teardown
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

type gateResponseGuard struct {
	w           http.ResponseWriter
	wroteHeader bool
	blocked     bool
}

func (g *gateResponseGuard) Header() http.Header { return g.w.Header() }

func (g *gateResponseGuard) Unwrap() http.ResponseWriter { return g.w }

func (g *gateResponseGuard) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	g.w.Header().Del("Location")
	g.w.Header().Del("Set-Cookie")
	if code >= http.StatusMultipleChoices && code < http.StatusBadRequest {
		g.blocked = true
		http.Error(g.w, "council gate unavailable", http.StatusBadGateway)
		return
	}
	g.w.WriteHeader(code)
}

func (g *gateResponseGuard) Write(p []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.blocked {
		return len(p), nil
	}
	return g.w.Write(p)
}

func (g *gateResponseGuard) Flush() {
	if g.blocked {
		return
	}
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if f, ok := g.w.(http.Flusher); ok {
		f.Flush()
	}
}

func sanitizeGateHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := r.Clone(r.Context())
		req.Header.Del("Cookie")
		guard := &gateResponseGuard{w: w}
		next.ServeHTTP(guard, req)
		if !guard.wroteHeader {
			guard.WriteHeader(http.StatusOK)
		}
	})
}

type unixGate struct {
	sockPath string
	server   *http.Server
	done     chan struct{}
	cleanup  func() error
}

func startUnixGate(sockRoot string, cfg Config, limits GateLimits) (*unixGate, error) {
	if cfg.GateHandler == nil {
		return nil, errors.New("council unix gate requires an in-process gate handler")
	}
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
	handler := gateRouteHandler(gate, digest, WrapGateHandler(sanitizeGateHandler(cfg.GateHandler), limits))
	srv := NewGateServer(handler, limits)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(LimitListener(ln, limits.MaxConns))
	}()
	return &unixGate{
		sockPath: sockPath,
		server:   srv,
		done:     done,
		cleanup:  cleanup,
	}, nil
}

func (g *unixGate) close() error {
	if g == nil {
		return nil
	}
	_ = g.server.Close()
	<-g.done
	return g.cleanup()
}

type tlsGate struct {
	addr       string
	port       string
	spkiSHA256 string
	key        *ecdsa.PrivateKey
	renewMu    sync.Mutex
	cert       atomic.Pointer[tls.Certificate]
	tlsConfig  *tls.Config
	server     *http.Server
	done       chan struct{}
	closeOnce  sync.Once
}

func newLaunchKey() (*ecdsa.PrivateKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate council gate P-256 key: %w", err)
	}
	return priv, nil
}

// signLaunchCertificate issues a certificate for priv. Reissuing with the same
// key keeps the SPKI digest that the proxy received at launch.
func signLaunchCertificate(priv *ecdsa.PrivateKey, now time.Time, validity time.Duration) (tls.Certificate, error) {
	serialMax := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialMax)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate council gate certificate serial: %w", err)
	}
	serial.Add(serial, big.NewInt(1))

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{Authority},
	}
	derCert, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create council gate certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(derCert)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse council gate certificate: %w", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{derCert},
		PrivateKey:  priv,
		Leaf:        leaf,
	}, nil
}

func newLaunchCertificate(now time.Time, validity time.Duration) (tls.Certificate, string, error) {
	priv, err := newLaunchKey()
	if err != nil {
		return tls.Certificate{}, "", err
	}
	cert, err := signLaunchCertificate(priv, now, validity)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	spkiSum := sha256.Sum256(cert.Leaf.RawSubjectPublicKeyInfo)
	return cert, hex.EncodeToString(spkiSum[:]), nil
}

func startTLSGate(cfg Config, limits GateLimits) (*tlsGate, error) {
	if cfg.GateHandler == nil {
		return nil, errors.New("council TLS gate requires an in-process gate handler")
	}
	gate, err := GateAuthority(cfg.DaemonListen)
	if err != nil {
		return nil, err
	}
	loopbackHost, _, err := net.SplitHostPort(cfg.DaemonListen)
	if err != nil {
		return nil, err
	}
	digest, err := hex.DecodeString(cfg.LeaseSHA256)
	if err != nil || len(digest) != sha256.Size {
		return nil, errors.New("invalid council task lease digest")
	}
	limits = limits.normalized()

	key, err := newLaunchKey()
	if err != nil {
		return nil, err
	}
	cert, err := signLaunchCertificate(key, time.Now(), launchCertLifetime)
	if err != nil {
		return nil, err
	}
	spkiSum := sha256.Sum256(cert.Leaf.RawSubjectPublicKeyInfo)
	spkiHex := hex.EncodeToString(spkiSum[:])
	rawLn, err := net.Listen("tcp", net.JoinHostPort(loopbackHost, "0"))
	if err != nil {
		return nil, fmt.Errorf("listen council gate TLS loopback: %w", err)
	}
	addr := rawLn.Addr().String()
	_, port, err := net.SplitHostPort(addr)
	if err != nil || !validPort(port) {
		_ = rawLn.Close()
		return nil, errors.New("invalid council gate TLS listen port")
	}

	g := &tlsGate{
		addr:       addr,
		port:       port,
		spkiSHA256: spkiHex,
		key:        key,
	}
	g.cert.Store(&cert)
	tlsCfg := &tls.Config{
		MinVersion:             tls.VersionTLS13,
		MaxVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return g.currentCertificate(time.Now())
		},
	}
	handler := gateRouteHandler(gate, digest, WrapGateHandler(sanitizeGateHandler(cfg.GateHandler), limits))
	srv := NewGateServer(handler, limits)
	srv.ErrorLog = log.New(io.Discard, "", 0)
	tlsLn := tls.NewListener(LimitListener(rawLn, limits.MaxConns), tlsCfg)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(tlsLn)
	}()
	g.tlsConfig = tlsCfg
	g.server = srv
	g.done = done
	return g, nil
}

// currentCertificate returns the launch certificate, reissuing it with the same
// key once half its lifetime has passed. Leases outlive a single certificate, so
// gate requests must keep working after the first one expires, and the unchanged
// key keeps the SPKI pin the proxy was started with valid.
func (g *tlsGate) currentCertificate(now time.Time) (*tls.Certificate, error) {
	c := g.cert.Load()
	if c == nil {
		return nil, errors.New("council gate TLS launch closed")
	}
	if now.Before(c.Leaf.NotAfter.Add(-launchCertRenewBefore)) {
		return c, nil
	}
	g.renewMu.Lock()
	defer g.renewMu.Unlock()
	c = g.cert.Load()
	if c == nil {
		return nil, errors.New("council gate TLS launch closed")
	}
	if now.Before(c.Leaf.NotAfter.Add(-launchCertRenewBefore)) {
		return c, nil
	}
	fresh, err := signLaunchCertificate(g.key, now, launchCertLifetime)
	if err != nil {
		return nil, err
	}
	g.cert.Store(&fresh)
	return &fresh, nil
}

func (g *tlsGate) close() error {
	if g == nil {
		return nil
	}
	g.closeOnce.Do(func() {
		g.renewMu.Lock()
		g.cert.Store(nil)
		g.renewMu.Unlock()
		_ = g.server.Close()
		<-g.done
	})
	return nil
}
