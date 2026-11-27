package secret

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestKeyringStoreMapsNotFound(t *testing.T) {
	keyring.MockInit()
	ctx := context.Background()
	ks := NewKeyringStore("")
	if ks.Service != "sablecourt" {
		t.Fatalf("default service = %q", ks.Service)
	}
	if _, err := ks.Get(ctx, "github_token"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("Get missing = %v, want ErrSecretNotFound", err)
	}
	if err := ks.Delete(ctx, "github_token"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("Delete missing = %v, want ErrSecretNotFound", err)
	}
	if err := ks.Set(ctx, "github_token", "v1"); err != nil {
		t.Fatal(err)
	}
	if v, err := ks.Get(ctx, "github_token"); err != nil || v != "v1" {
		t.Fatalf("Get = %q, %v", v, err)
	}
	if v, err := NewKeyringStore("other").Get(ctx, "github_token"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("another service read %q, %v", v, err)
	}
	if err := ks.Delete(ctx, "github_token"); err != nil {
		t.Fatal(err)
	}

	backendErr := errors.New("keychain locked")
	keyring.MockInitWithError(backendErr)
	t.Cleanup(keyring.MockInit)
	if _, err := ks.Get(ctx, "github_token"); !errors.Is(err, backendErr) || errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("Get with a failing keychain = %v, want the backend error", err)
	}
	if err := ks.Delete(ctx, "github_token"); !errors.Is(err, backendErr) {
		t.Fatalf("Delete with a failing keychain = %v", err)
	}
}

func TestFileStoreErrorPaths(t *testing.T) {
	ctx := context.Background()

	t.Run("corrupt file fails closed for every operation", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "secrets.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		fs := NewFileStore(path)
		if _, err := fs.Get(ctx, "a"); err == nil || !strings.Contains(err.Error(), "parse secret file") {
			t.Fatalf("Get = %v", err)
		}
		if err := fs.Set(ctx, "a", "b"); err == nil || !strings.Contains(err.Error(), "parse secret file") {
			t.Fatalf("Set = %v", err)
		}
		if err := fs.Delete(ctx, "a"); err == nil || !strings.Contains(err.Error(), "parse secret file") {
			t.Fatalf("Delete = %v", err)
		}
		raw, _ := os.ReadFile(path)
		if string(raw) != "{not json" {
			t.Fatalf("a failed Set rewrote the file: %q", raw)
		}
	})

	t.Run("insecure mode blocks writes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "secrets.json")
		if err := os.WriteFile(path, []byte(`{"a":"1"}`), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		fs := NewFileStore(path)
		if err := fs.Set(ctx, "b", "2"); !errors.Is(err, ErrInsecureFileMode) {
			t.Fatalf("Set = %v, want ErrInsecureFileMode", err)
		}
		if err := fs.Delete(ctx, "a"); !errors.Is(err, ErrInsecureFileMode) {
			t.Fatalf("Delete = %v, want ErrInsecureFileMode", err)
		}
	})

	t.Run("delete of a missing key or file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "secrets.json")
		fs := NewFileStore(path)
		if err := fs.Delete(ctx, "a"); !errors.Is(err, ErrSecretNotFound) {
			t.Fatalf("Delete without a file = %v", err)
		}
		if err := fs.Set(ctx, "a", "1"); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("new secret file mode = %v, %v", info.Mode(), err)
		}
		if err := fs.Delete(ctx, "b"); !errors.Is(err, ErrSecretNotFound) {
			t.Fatalf("Delete missing key = %v", err)
		}
		if v, err := fs.Get(ctx, "a"); err != nil || v != "1" {
			t.Fatalf("other key lost: %q, %v", v, err)
		}
	})

	t.Run("path through a file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "plain")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		fs := NewFileStore(filepath.Join(file, "secrets.json"))
		if _, err := fs.Get(ctx, "a"); err == nil || errors.Is(err, ErrSecretNotFound) {
			t.Fatalf("Get = %v, want a stat error", err)
		}
	})
}

type errStore struct{ err error }

func (e errStore) Get(context.Context, string) (string, error) { return "", e.err }
func (e errStore) Set(context.Context, string, string) error   { return e.err }
func (e errStore) Delete(context.Context, string) error        { return e.err }

func TestChainStoreGetPropagatesBackendErrors(t *testing.T) {
	ctx := context.Background()
	backendErr := errors.New("backend down")
	cases := []struct {
		name    string
		stores  []SecretStore
		want    string
		wantErr error
	}{
		{"later store answers after an error", []SecretStore{errStore{backendErr}, NewMemoryStore(map[string]string{"k": "v"})}, "v", nil},
		{"error wins over not found", []SecretStore{errStore{backendErr}, NewMemoryStore(nil)}, "", backendErr},
		{"nil stores are skipped", []SecretStore{nil, NewMemoryStore(nil)}, "", ErrSecretNotFound},
		{"no stores", nil, "", ErrSecretNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (&ChainStore{Stores: tc.stores}).Get(ctx, "k")
			if got != tc.want || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("Get = %q, %v; want %q, %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
