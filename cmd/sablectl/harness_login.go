package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
	"github.com/shadewright-labs/sablecourt/internal/harnessimages"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/docker"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// loginRuntime is the part of the Docker runtime the login command uses. The
// daemon is not involved: no terminal stream crosses its API.
type loginRuntime interface {
	CheckCouncilHarness(context.Context, string) error
	EnsureCouncilLoginVolume(context.Context, string) error
	ResolveLocalImage(context.Context, string) (string, error)
	ProvisionCouncilProxy(context.Context, string, councilproxy.Config) (councilproxy.LaunchSession, error)
	RunCouncilLogin(context.Context, rt.CouncilLaunch, func(context.Context, io.Reader, io.Writer) error, rt.CouncilRecorder) (rt.CouncilResult, error)
}

// newLoginRuntime builds the Docker runtime for one harness, with the same
// role, image, volume and login files the daemon launcher uses.
var newLoginRuntime = func(def harnessimages.Definition) (loginRuntime, error) {
	a, err := docker.NewFromEnv()
	if err != nil {
		return nil, fmt.Errorf("configure Docker runtime: %w", err)
	}
	a.CouncilHarnesses = map[string]docker.CouncilHarness{
		def.Role: {Image: def.Image, LoginVolume: def.Volume, LoginFiles: def.LoginFiles},
	}
	return a, nil
}

const loginWallTime = 30 * time.Minute

func loginHarnessCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "login <role>",
		Short:     "Log in to a harness inside its container and keep the login in its volume",
		ValidArgs: []string{"claude", "codex", "agy"},
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.ExactArgs(1)(cmd, args); err != nil {
				return err
			}
			_, err := harnessimages.Lookup(args[0])
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			in := cmd.InOrStdin()
			return runHarnessLogin(cmd.Context(), args[0], in, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

func runHarnessLogin(ctx context.Context, role string, in io.Reader, out, errOut io.Writer) error {
	def, err := harnessimages.Lookup(role)
	if err != nil {
		return err
	}
	runtime, err := newLoginRuntime(def)
	if err != nil {
		return err
	}
	missingImage := fmt.Errorf("image for %s is missing: run %s", role, harnessimages.BuildCommand(role))
	err = runtime.CheckCouncilHarness(ctx, role)
	var fault *docker.HarnessError
	if errors.As(err, &fault) {
		switch fault.Fault {
		case docker.FaultImageMissing:
			return missingImage
		case docker.FaultVolumeMissing, docker.FaultLoginMissing:
			err = nil
		case docker.FaultVolumeInvalid:
			return fmt.Errorf("volume %s exists but is not the labelled Sablecourt volume for %s: remove it by hand and run this command again", def.Volume, role)
		default:
			return fmt.Errorf("cannot check the %s harness: Docker or the harness configuration is unavailable", role)
		}
	} else if err != nil {
		return fmt.Errorf("cannot check the %s harness: %w", role, err)
	} else {
		fmt.Fprintf(errOut, "A login for %s already exists; continuing will replace it if you finish the flow.\n", role)
	}
	proxyID, err := runtime.ResolveLocalImage(ctx, harnessimages.ProxyImage())
	if err != nil {
		if docker.IsNotFound(err) {
			return fmt.Errorf("proxy image is missing: run %s", harnessimages.BuildCommand(role))
		}
		return fmt.Errorf("resolve proxy image: %w", err)
	}
	if err := runtime.EnsureCouncilLoginVolume(ctx, role); err != nil {
		return fmt.Errorf("prepare login volume: %w", err)
	}
	// A login proxy carries no lease, so it reaches only the provider's hosts.
	session, err := runtime.ProvisionCouncilProxy(ctx, proxyID, councilproxy.Config{Harness: role, DaemonListen: "127.0.0.1:7432"})
	if session != nil {
		defer func() {
			cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer done()
			if cerr := session.Close(cleanup); cerr != nil {
				fmt.Fprintf(errOut, "warning: could not remove the login proxy: %v\n", cerr)
			}
		}()
	}
	if err != nil {
		return fmt.Errorf("start login proxy: %w", err)
	}
	endpoint := session.Workload()
	_, err = runtime.RunCouncilLogin(ctx, rt.CouncilLaunch{
		TaskID:   "login-" + role + "-" + rand.Text()[:10],
		Workload: "harness-login",
		Harness:  role,
		Limits:   rt.CouncilLimits{NanoCPUs: 2_000_000_000, MemoryBytes: 2 << 30, PIDs: 256, WallTime: loginWallTime},
		Network:  endpoint.Network, NetworkID: endpoint.NetworkID, ProxyURL: endpoint.ProxyURL,
	}, terminalSession(in, out), func(context.Context, rt.CouncilResult) error { return nil })
	if err != nil {
		return fmt.Errorf("login session for %s failed: %w", role, err)
	}
	if err := runtime.CheckCouncilHarness(ctx, role); err != nil {
		if errors.As(err, &fault) && fault.Fault == docker.FaultLoginMissing {
			return fmt.Errorf("the %s session ended without a stored login: run %s again and finish the flow", role, harnessimages.LoginCommand(role))
		}
		return fmt.Errorf("cannot confirm the %s login: %w", role, err)
	}
	_, err = fmt.Fprintf(out, "\nLogin for %s is stored in volume %s\n", role, def.Volume)
	return err
}

// terminalSession connects the caller's terminal to the container terminal.
// A real terminal is put in raw mode for the session and restored after it.
func terminalSession(in io.Reader, out io.Writer) func(context.Context, io.Reader, io.Writer) error {
	return func(ctx context.Context, r io.Reader, w io.Writer) error {
		if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			if old, err := term.MakeRaw(int(f.Fd())); err == nil {
				defer func() { _ = term.Restore(int(f.Fd()), old) }()
			}
		}
		done := make(chan error, 1)
		go func() {
			_, err := io.Copy(out, r)
			done <- err
		}()
		// A blocked terminal read is released by process exit, so input written
		// after the session ends must not reach the closed container stream.
		gw := &guardedWriter{w: w}
		defer gw.stop()
		go func() { _, _ = io.Copy(gw, in) }()
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type guardedWriter struct {
	mu      sync.Mutex
	w       io.Writer
	stopped bool
}

func (g *guardedWriter) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return 0, io.ErrClosedPipe
	}
	return g.w.Write(p)
}

func (g *guardedWriter) stop() {
	g.mu.Lock()
	g.stopped = true
	g.mu.Unlock()
}
