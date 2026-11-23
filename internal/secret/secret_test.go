package secret

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileStoreRoundTripAndModeCheck(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "secrets.json")
	fs := NewFileStore(path)

	if _, err := fs.Get(ctx, "openai_api_key"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound, got %v", err)
	}

	if err := fs.Set(ctx, "openai_api_key", "sk-test-123"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := fs.Get(ctx, "openai_api_key")
	if err != nil || got != "sk-test-123" {
		t.Fatalf("Get: val=%q err=%v", got, err)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := fs.Get(ctx, "openai_api_key"); !errors.Is(err, ErrInsecureFileMode) {
		t.Fatalf("expected ErrInsecureFileMode on 0644 file, got %v", err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod back: %v", err)
	}
	if err := fs.Delete(ctx, "openai_api_key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := fs.Get(ctx, "openai_api_key"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound after delete, got %v", err)
	}
}

func TestChainStoreFallsBack(t *testing.T) {
	ctx := context.Background()
	primary := NewMemoryStore(map[string]string{"a": "from-primary"})
	fallback := NewMemoryStore(map[string]string{"b": "from-fallback"})
	chain := &ChainStore{Stores: []SecretStore{primary, fallback}}

	if v, err := chain.Get(ctx, "a"); err != nil || v != "from-primary" {
		t.Fatalf("Get a: %q %v", v, err)
	}
	if v, err := chain.Get(ctx, "b"); err != nil || v != "from-fallback" {
		t.Fatalf("Get b: %q %v", v, err)
	}
	if _, err := chain.Get(ctx, "missing"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound, got %v", err)
	}
}
