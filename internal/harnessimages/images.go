// Package harnessimages defines the local council images and login state.
package harnessimages

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
)

const (
	Platform = "linux/arm64"
	User     = "10001:10001"
	StateDir = "/home/harness"
)

//go:embed Dockerfile
var definitions embed.FS

type Definition struct {
	Role      string
	Image     string
	Volume    string
	LoginArgv []string
	Version   string
}

// Docker is the only boundary that communicates with the local engine.
type Docker interface {
	Run(context.Context, []string, io.Reader) error
}

// ProxyCompiler supplies the executable maintained with the gate proxy.
type ProxyCompiler interface {
	Compile(context.Context) ([]byte, error)
}

func Lookup(role string) (Definition, error) {
	d := Definition{Role: role, Volume: "sablecourt-harness-" + role}
	switch role {
	case "claude":
		d.Version, d.LoginArgv = "2.1.332", []string{"claude", "auth", "login"}
	case "codex":
		d.Version, d.LoginArgv = "0.172.0", []string{"codex", "login", "--device-auth"}
	case "agy":
		d.Version, d.LoginArgv = "1.5.16", []string{"agy"}
	default:
		return Definition{}, fmt.Errorf("unknown harness role (choose claude, codex, or agy)")
	}
	d.Image = "sablecourt-harness-" + role + ":" + d.Version + "-" + revision()
	return d, nil
}

// ProxyImage identifies the proxy built from the same image definitions.
func ProxyImage() string { return "sablecourt-harness-proxy:" + revision() }

func revision() string {
	return fmt.Sprintf("%x", sha256.Sum256(buildContext()))
}

// buildContext never reads the checkout, client configuration, or login state.
// Fixed tar metadata makes the context and its image tags stable across retries.
func buildContext() []byte {
	return contextWithProxy(nil)
}

func contextWithProxy(proxy []byte) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = fs.WalkDir(definitions, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			panic(err)
		}
		if entry.IsDir() {
			return nil
		}
		body, err := definitions.ReadFile(name)
		if err != nil {
			panic(err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(body))}); err != nil {
			panic(err)
		}
		if _, err := tw.Write(body); err != nil {
			panic(err)
		}
		return nil
	})
	if len(proxy) != 0 {
		if err := tw.WriteHeader(&tar.Header{Name: "council-proxy", Mode: 0755, Size: int64(len(proxy))}); err != nil {
			panic(err)
		}
		if _, err := tw.Write(proxy); err != nil {
			panic(err)
		}
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func Build(ctx context.Context, docker Docker, compiler ProxyCompiler, role string) error {
	d, err := Lookup(role)
	if err != nil {
		return err
	}
	if docker == nil {
		return fmt.Errorf("Docker builder is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if compiler == nil {
		return fmt.Errorf("proxy compiler is required")
	}
	proxy, err := compiler.Compile(ctx)
	if err != nil {
		return fmt.Errorf("compile council proxy: %w", err)
	}
	if len(proxy) == 0 {
		return fmt.Errorf("proxy compiler returned an empty executable")
	}
	for _, image := range []struct{ target, tag string }{{"proxy", ProxyImage()}, {role, d.Image}} {
		if err := ctx.Err(); err != nil {
			return err
		}
		args := []string{"build", "--platform", Platform, "--pull=false", "--rm", "--force-rm", "--target", image.target, "--tag", image.tag, "--file", "Dockerfile", "-"}
		if err := docker.Run(ctx, args, bytes.NewReader(contextWithProxy(proxy))); err != nil {
			return fmt.Errorf("build %s: %w", image.target, err)
		}
	}
	return ctx.Err()
}

// CommandDocker invokes Docker without forwarding provider or task credentials.
// Build diagnostics stay in the client process and are not returned as errors.
type CommandDocker struct{}

func (CommandDocker) Run(ctx context.Context, args []string, input io.Reader) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = input
	for _, key := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Docker command failed: %w", err)
	}
	return nil
}
