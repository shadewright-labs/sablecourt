package councilproxy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUnixSocketCloseNeverUnlinksByPathname(t *testing.T) {
	root := filepath.Join(shortSocketRoot(t), "gates")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sockPath, ln, cleanup, err := createPrivateUnixSocket(root, "g-")
	if err != nil {
		t.Fatal(err)
	}
	// A replacement at the same pathname must survive the listener closing.
	if err := os.Remove(sockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sockPath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	if _, err := os.Lstat(sockPath); err != nil {
		t.Fatalf("listener close unlinked a replacement by pathname: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Dir(sockPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rooted cleanup left the launch directory: %v", err)
	}
}

func TestSocketRootRequiresTrustedAncestors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    os.FileMode
		wantErr bool
	}{
		{"owner only", 0o700, false},
		{"group and other read", 0o755, false},
		{"group writable", 0o775, true},
		{"world writable", 0o777, true},
		{"world writable sticky", 0o777 | os.ModeSticky, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := shortSocketRoot(t)
			mid := filepath.Join(base, "mid")
			if err := os.Mkdir(mid, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(mid, tc.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(mid, 0o700) })
			// The base directory is private; the loosened ancestor sits between it and the root.
			explicit := filepath.Join(mid, "gates")
			_, ln, cleanup, err := createPrivateUnixSocket(explicit, "g-")
			if ln != nil {
				_ = ln.Close()
				_ = cleanup()
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("explicit root: err = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestGateSocketMountSourceIsRevalidated(t *testing.T) {
	base := shortSocketRoot(t)
	mid := filepath.Join(base, "mid")
	if err := os.Mkdir(mid, 0o700); err != nil {
		t.Fatal(err)
	}
	sockPath, ln, cleanup, err := createPrivateUnixSocket(filepath.Join(mid, "gates"), "g-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close(); _ = cleanup() }()
	if err := verifyGateSocketMountSource(sockPath); err != nil {
		t.Fatalf("fresh socket refused: %v", err)
	}
	if err := os.Chmod(mid, 0o777); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(mid, 0o700) }()
	if err := verifyGateSocketMountSource(sockPath); err == nil {
		t.Fatal("mount source accepted after an ancestor became replaceable")
	}
	if err := os.Chmod(mid, 0o700); err != nil {
		t.Fatal(err)
	}
	// filepath.Join would clean the dot component away, so build the alias by hand.
	alias := filepath.Dir(sockPath) + string(filepath.Separator) + "." + string(filepath.Separator) + filepath.Base(sockPath)
	if err := verifyGateSocketMountSource(alias); err == nil {
		t.Fatal("non-canonical mount source accepted")
	}
}

type errListener struct{ err error }

func (l errListener) Accept() (net.Conn, error) { return nil, l.err }
func (errListener) Close() error                { return nil }
func (errListener) Addr() net.Addr              { return fakeAddr("err-listener") }

func TestProbeDrainRequiresConsumedSentinel(t *testing.T) {
	t.Run("accept error is returned separately", func(t *testing.T) {
		var st probeState
		out, done := serveProbeListener(errListener{err: errors.New("accept failed")}, "sentinel", "token", &st)
		<-done
		err := awaitProbeDrain(out, time.Second)
		if err == nil || !strings.Contains(err.Error(), "accept failed") {
			t.Fatalf("accept error not surfaced: %v", err)
		}
	})
	t.Run("closed listener without sentinel", func(t *testing.T) {
		root := shortSocketRoot(t)
		_, ln, cleanup, err := createPrivateUnixSocket(filepath.Join(root, "gates"), "p-")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cleanup() }()
		var st probeState
		out, done := serveProbeListener(ln, "sentinel", "token", &st)
		_ = ln.Close()
		<-done
		if err := awaitProbeDrain(out, time.Second); err == nil {
			t.Fatal("drain reported success without consuming the sentinel")
		}
	})
	t.Run("sentinel consumed", func(t *testing.T) {
		root := shortSocketRoot(t)
		sockPath, ln, cleanup, err := createPrivateUnixSocket(filepath.Join(root, "gates"), "p-")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close(); _ = cleanup() }()
		var st probeState
		out, done := serveProbeListener(ln, "sentinel", "token", &st)
		if err := drainProbeListener(t.Context(), sockPath, "sentinel"); err != nil {
			t.Fatal(err)
		}
		if err := awaitProbeDrain(out, 2*time.Second); err != nil {
			t.Fatalf("consumed sentinel refused: %v", err)
		}
		_ = ln.Close()
		<-done
		if st.accepted.Load() {
			t.Fatal("the host's own marker counted as a probe connection")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		out := make(chan probeOutcome)
		if err := awaitProbeDrain(out, 20*time.Millisecond); err == nil {
			t.Fatal("drain wait did not time out")
		}
	})
}

func TestConnectTunnelsAreCappedBelowListenerCeiling(t *testing.T) {
	if MaxTunnels >= MaxGateConns || MaxTunnels <= 0 {
		t.Fatalf("MaxTunnels %d must be positive and below the listener ceiling %d", MaxTunnels, MaxGateConns)
	}
	const token = "sc_task_fixture"
	digest := sha256.Sum256([]byte(token))
	public := netip.AddrFrom4([4]byte{8, 8, 4, 4})
	release := make(chan struct{})
	var lookups atomic.Int32
	lookup := func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
		lookups.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return []netip.Addr{public}, nil
	}
	dial := func(context.Context, string, string) (net.Conn, error) { return nil, io.ErrClosedPipe }
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
	})
	h, err := newProxy(Config{
		Harness: "claude", DaemonListen: loopbackAddress(),
		LeaseSHA256: hex.EncodeToString(digest[:]), GateSocket: ContainerGateSocket,
	}, transport, lookup, dial)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", ipv4LoopbackZero())
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(LimitListener(ln, MaxGateConns)) }()
	t.Cleanup(func() { _ = srv.Close() })

	connect := func() (net.Conn, *bufio.Reader) {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		_, err = io.WriteString(conn, "CONNECT claude.ai:443 HTTP/1.1\r\nHost: claude.ai:443\r\n\r\n")
		if err != nil {
			t.Fatal(err)
		}
		return conn, bufio.NewReader(conn)
	}
	var wg sync.WaitGroup
	for range MaxTunnels {
		_, br := connect()
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = http.ReadResponse(br, nil) }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for lookups.Load() < int32(MaxTunnels) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := lookups.Load(); got != int32(MaxTunnels) {
		t.Fatalf("lookups = %d, want %d", got, MaxTunnels)
	}

	_, br := connect()
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || lookups.Load() != int32(MaxTunnels) {
		t.Fatalf("tunnel over the cap: status %d, lookups %d", resp.StatusCode, lookups.Load())
	}

	// A gate call still gets a listener slot while every tunnel slot is held.
	req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/gate/task/messages", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	gateResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = gateResp.Body.Close()
	if gateResp.StatusCode != http.StatusOK {
		t.Fatalf("gate call starved by tunnels: status %d", gateResp.StatusCode)
	}

	close(release)
	wg.Wait()
	_, br = connect()
	resp, err = http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("tunnel slot not released: status %d", resp.StatusCode)
	}
}
