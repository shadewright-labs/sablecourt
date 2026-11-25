package harnessimages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// LocalProxyCompiler builds only the proxy entry point and its standard-library
// policy implementation. No source checkout or client config enters the image.
type LocalProxyCompiler struct{ Source string }

func (c LocalProxyCompiler) Compile(ctx context.Context) (binary []byte, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp("", "sablecourt-proxy-build-")
	if err != nil {
		return nil, fmt.Errorf("create proxy build scratch: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(scratch); err != nil {
			binary = nil
			resultErr = errors.Join(resultErr, fmt.Errorf("remove proxy build scratch: %w", err))
		}
	}()
	packageDir := filepath.Join("internal", "gate", "councilproxy")
	for _, name := range []string{"gate/councilproxy/proxy.go", "gate/councilproxy/proxy_main.go",
		"gate/councilproxy/docker.go", "gate/councilproxy/socket.go", "gate/councilproxy/limits.go", "operatortoken/token.go"} {
		rel := filepath.Join("internal", filepath.FromSlash(name))
		source := filepath.Join(c.Source, rel)
		dest := filepath.Join(scratch, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return nil, err
		}
		info, err := os.Lstat(source)
		if err != nil {
			return nil, fmt.Errorf("council proxy source %s is unavailable; integrate issue #76 or select its checkout with --source", name)
		}
		if !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return nil, fmt.Errorf("council proxy source %s must be a regular file under 1 MiB", name)
		}
		body, err := os.ReadFile(source)
		if err != nil {
			return nil, fmt.Errorf("read council proxy source %s", name)
		}
		if err := os.WriteFile(dest, body, 0600); err != nil {
			return nil, err
		}
	}
	module := "module github.com/shadewright-labs/sablecourt\n\ngo 1.27.4\n"
	if err := os.WriteFile(filepath.Join(scratch, "go.mod"), []byte(module), 0600); err != nil {
		return nil, err
	}
	output := filepath.Join(scratch, "council-proxy")
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", output, filepath.Join(packageDir, "proxy_main.go"))
	cmd.Dir = scratch
	for _, key := range []string{"PATH", "HOME", "GOCACHE", "TMPDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64", "GOENV=off", "GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("compile isolated council proxy with the local Go toolchain: %w", err)
	}
	return os.ReadFile(output)
}
