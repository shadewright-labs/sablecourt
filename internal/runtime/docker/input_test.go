package docker

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact/redacttest"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

type inputConn struct {
	net.Conn
	body      bytes.Buffer
	halfClose bool
	closed    bool
}

func (c *inputConn) Write(p []byte) (int, error) { return c.body.Write(p) }
func (c *inputConn) CloseWrite() error           { c.halfClose = true; return nil }
func (c *inputConn) Close() error                { c.closed = true; return nil }

type inputEngine struct {
	fakeEngine
	conn    inputConn
	attach  client.ContainerAttachOptions
	removed bool
}

func (f *inputEngine) ContainerAttach(_ context.Context, _ string, o client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	f.attach = o
	return client.ContainerAttachResult{HijackedResponse: client.HijackedResponse{Conn: &f.conn}}, nil
}

func (f *inputEngine) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.removed = true
	return client.ContainerRemoveResult{}, nil
}

type noAttachEngine struct {
	fakeEngine
	removed bool
}

func (f *noAttachEngine) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.removed = true
	return client.ContainerRemoveResult{}, nil
}

func inputSpec(input string) scruntime.LaunchSpec {
	return scruntime.LaunchSpec{
		TaskID: "task-input",
		Workload: domain.Workload{
			Metadata: domain.ObjectMeta{Name: "agent", Zone: domain.ZoneRuntime},
			Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Runtime: domain.RuntimeConfig{Type: "docker", Image: "agent:local"}},
		},
		InputJSON: input,
		TaskToken: "lease",
	}
}

// TestTaskInputStaysOutOfContainerConfig checks that task input never lands in
// the container environment, which Docker stores and shows on inspect, and
// reaches the process on stdin instead.
func TestTaskInputStaysOutOfContainerConfig(t *testing.T) {
	for _, tc := range redacttest.Corpus() {
		t.Run(tc.Name, func(t *testing.T) {
			eng := &inputEngine{}
			if _, err := NewAdapter(eng, DefaultNetworkName).Start(context.Background(), inputSpec(tc.Text)); err != nil {
				t.Fatalf("Start: %v", err)
			}
			cfg := eng.createdOpts.Config
			for _, kv := range cfg.Env {
				if strings.HasPrefix(kv, "SABLECOURT_INPUT_JSON=") {
					t.Fatalf("task input in container env: %q", kv)
				}
				if frag, ok := redacttest.Leak(kv, tc.Secrets); ok {
					t.Fatalf("container env holds %q", frag)
				}
			}
			if got := eng.conn.body.String(); got != tc.Text+"\x00" {
				t.Fatalf("stdin got %q, want the input and a NUL", got)
			}
			if !cfg.OpenStdin || !cfg.StdinOnce || !eng.attach.Stdin || !eng.conn.halfClose {
				t.Fatalf("stdin not delivered once: open=%t once=%t attach=%t eof=%t", cfg.OpenStdin, cfg.StdinOnce, eng.attach.Stdin, eng.conn.halfClose)
			}
		})
	}
}

func TestTaskInputDeliveryFailsClosed(t *testing.T) {
	eng := &noAttachEngine{}
	if _, err := NewAdapter(eng, DefaultNetworkName).Start(context.Background(), inputSpec(`{"prompt":"x"}`)); err == nil {
		t.Fatal("started without a way to deliver task input")
	}
	if eng.startedID != "" || !eng.removed {
		t.Fatalf("container left behind: started=%q removed=%t", eng.startedID, eng.removed)
	}
	nul := &inputEngine{}
	if _, err := NewAdapter(nul, DefaultNetworkName).Start(context.Background(), inputSpec("a\x00b")); err == nil {
		t.Fatal("input with a NUL byte was accepted")
	}
	if nul.conn.body.Len() != 0 {
		t.Fatal("input with a NUL byte reached stdin")
	}
}

// stuckConn is the stdin of a process that never reads: writes block until
// the connection is closed.
type stuckConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *stuckConn) Write([]byte) (int, error) { <-c.closed; return 0, net.ErrClosed }
func (c *stuckConn) CloseWrite() error         { return nil }
func (c *stuckConn) Close() error              { c.once.Do(func() { close(c.closed) }); return nil }

type stuckEngine struct {
	fakeEngine
	conn            *stuckConn
	mu              sync.Mutex
	cleanupDeadline []bool
}

func (f *stuckEngine) ContainerAttach(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	return client.ContainerAttachResult{HijackedResponse: client.HijackedResponse{Conn: f.conn}}, nil
}

func (f *stuckEngine) record(ctx context.Context) {
	_, ok := ctx.Deadline()
	f.mu.Lock()
	f.cleanupDeadline = append(f.cleanupDeadline, ok)
	f.mu.Unlock()
}

func (f *stuckEngine) ContainerStop(ctx context.Context, _ string, _ client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.record(ctx)
	return client.ContainerStopResult{}, nil
}

func (f *stuckEngine) ContainerRemove(ctx context.Context, _ string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.record(ctx)
	return client.ContainerRemoveResult{}, nil
}

// TestTaskInputNotReadFailsWithinTheLaunchDeadline starts an image whose
// process never reads stdin. Start must give up when the launch context
// ends, report the input as not delivered, and clean up under a deadline.
func TestTaskInputNotReadFailsWithinTheLaunchDeadline(t *testing.T) {
	eng := &stuckEngine{conn: &stuckConn{closed: make(chan struct{})}}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := NewAdapter(eng, DefaultNetworkName).Start(ctx, inputSpec(`{"prompt":"x"}`))
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		eng.conn.Close()
		err = <-done
		t.Fatalf("Start ignored the launch deadline: returned after %s with %v", time.Since(began).Round(time.Second), err)
	}
	var notDelivered *scruntime.InputDeliveryError
	if !errors.As(err, &notDelivered) {
		t.Fatalf("Start error = %v, want an input delivery error", err)
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.cleanupDeadline) == 0 {
		t.Fatal("container not cleaned up")
	}
	for i, ok := range eng.cleanupDeadline {
		if !ok {
			t.Fatalf("cleanup call %d ran without a deadline", i)
		}
	}
}
