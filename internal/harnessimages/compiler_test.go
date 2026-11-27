package harnessimages

import (
	"bytes"
	"context"
	"debug/elf"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalProxyCompiler(t *testing.T) {
	for _, tc := range []struct {
		name                                             string
		missing, symlink, invalid, dependency, cancelled bool
		oversized, directory                             bool
	}{
		{name: "isolated offline build"},
		{name: "missing entry point", missing: true},
		{name: "symlink", symlink: true},
		{name: "invalid source", invalid: true},
		{name: "external dependency", dependency: true},
		{name: "cancelled", cancelled: true},
		{name: "oversized", oversized: true},
		{name: "directory", directory: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := t.TempDir()
			dir := filepath.Join(source, "internal", "gate", "councilproxy")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			main := "//go:build ignore\n\npackage main\nimport p \"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy\"\nfunc main(){ p.Ready() }\n"
			if tc.oversized {
				main += "\n//" + strings.Repeat("x", 1<<20) + "\n"
			}
			policy := "package councilproxy\nfunc Ready(){}\n"
			if tc.invalid {
				policy = "invalid go source"
			}
			if tc.dependency {
				policy = "package councilproxy\nimport _ \"unavailable/module\"\nfunc Ready(){}\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "proxy.go"), []byte(policy), 0600); err != nil {
				t.Fatal(err)
			}
			if !tc.missing {
				if tc.directory {
					if err := os.Mkdir(filepath.Join(dir, "proxy_main.go"), 0700); err != nil {
						t.Fatal(err)
					}
				} else if tc.symlink {
					entry := filepath.Join(source, "entry.go")
					if err := os.WriteFile(entry, []byte(main), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(entry, filepath.Join(dir, "proxy_main.go")); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(filepath.Join(dir, "proxy_main.go"), []byte(main), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for _, file := range []string{"docker.go", "socket.go", "limits.go"} {
				if err := os.WriteFile(filepath.Join(dir, file), []byte("package councilproxy"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			tokenDir := filepath.Join(source, "internal", "operatortoken")
			if err := os.MkdirAll(tokenDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(tokenDir, "token.go"), []byte("package operatortoken"), 0600); err != nil {
				t.Fatal(err)
			}
			redactDir := filepath.Join(source, "internal", "redact")
			if err := os.MkdirAll(redactDir, 0700); err != nil {
				t.Fatal(err)
			}
			for _, file := range []string{"redact.go", "known.go"} {
				if err := os.WriteFile(filepath.Join(redactDir, file), []byte("package redact"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			t.Setenv("GOFLAGS", "-invalid-flag")
			t.Setenv("GOWORK", "missing-work-file")
			t.Setenv("GOTOOLCHAIN", "unavailable")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			binary, err := (LocalProxyCompiler{Source: source}).Compile(ctx)
			wantError := tc.missing || tc.symlink || tc.invalid || tc.dependency || tc.cancelled || tc.oversized || tc.directory
			if (err != nil) != wantError {
				t.Fatalf("error = %v, want error %v", err, wantError)
			}
			if (tc.symlink || tc.oversized || tc.directory) && !strings.Contains(err.Error(), "must be a regular file under 1 MiB") {
				t.Fatalf("wrong refusal: %v", err)
			}
			if !wantError {
				file, err := elf.NewFile(bytes.NewReader(binary))
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if file.Machine != elf.EM_AARCH64 {
					t.Fatalf("machine = %s", file.Machine)
				}
			}
			left, err := os.ReadDir(scratch)
			if err != nil || len(left) != 0 {
				t.Fatalf("compiler left temporary state: %v, %v", left, err)
			}
		})
	}
}

func TestCompileProductionProxyOffline(t *testing.T) {
	binary, err := (LocalProxyCompiler{Source: filepath.Join("..", "..")}).Compile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := elf.NewFile(bytes.NewReader(binary)); err != nil {
		t.Fatal(err)
	}
}
