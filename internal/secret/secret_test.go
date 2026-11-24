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

type faultSecretStore struct {
	inner     *MemoryStore
	setErr    error
	deleteErr error
	setCalls  int
	delCalls  int
}

func (f *faultSecretStore) Get(ctx context.Context, name string) (string, error) {
	return f.inner.Get(ctx, name)
}

func (f *faultSecretStore) Set(ctx context.Context, name, value string) error {
	f.setCalls++
	if f.setErr != nil {
		return f.setErr
	}
	return f.inner.Set(ctx, name, value)
}

func (f *faultSecretStore) Delete(ctx context.Context, name string) error {
	f.delCalls++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.inner.Delete(ctx, name)
}

func TestChainStoreSetAndDelete(t *testing.T) {
	errPrimaryDown := errors.New("keyring locked")
	errSecondaryDown := errors.New("disk read-only")

	t.Run("Set", func(t *testing.T) {
		cases := []struct {
			name         string
			stores       func() ([]*faultSecretStore, *ChainStore)
			wantErr      error
			wantNonNil   bool
			wantInStore  int
			wantSetCalls []int
		}{
			{
				name: "writes to first store when it accepts",
				stores: func() ([]*faultSecretStore, *ChainStore) {
					s0 := &faultSecretStore{inner: NewMemoryStore(nil)}
					s1 := &faultSecretStore{inner: NewMemoryStore(nil)}
					return []*faultSecretStore{s0, s1}, &ChainStore{Stores: []SecretStore{s0, s1}}
				},
				wantInStore:  0,
				wantSetCalls: []int{1, 0},
			},
			{
				name: "falls back to second store when first rejects",
				stores: func() ([]*faultSecretStore, *ChainStore) {
					s0 := &faultSecretStore{inner: NewMemoryStore(nil), setErr: errPrimaryDown}
					s1 := &faultSecretStore{inner: NewMemoryStore(nil)}
					return []*faultSecretStore{s0, s1}, &ChainStore{Stores: []SecretStore{s0, s1}}
				},
				wantInStore:  1,
				wantSetCalls: []int{1, 1},
			},
			{
				name: "skips nil store and writes to fallback",
				stores: func() ([]*faultSecretStore, *ChainStore) {
					s1 := &faultSecretStore{inner: NewMemoryStore(nil)}
					return []*faultSecretStore{s1}, &ChainStore{Stores: []SecretStore{nil, s1}}
				},
				wantInStore:  0,
				wantSetCalls: []int{1},
			},
			{
				name: "fails closed when all stores reject",
				stores: func() ([]*faultSecretStore, *ChainStore) {
					s0 := &faultSecretStore{inner: NewMemoryStore(nil), setErr: errPrimaryDown}
					s1 := &faultSecretStore{inner: NewMemoryStore(nil), setErr: errSecondaryDown}
					return []*faultSecretStore{s0, s1}, &ChainStore{Stores: []SecretStore{s0, s1}}
				},
				wantNonNil:   true,
				wantInStore:  -1,
				wantSetCalls: []int{1, 1},
			},
			{
				name: "fails closed when no stores configured",
				stores: func() ([]*faultSecretStore, *ChainStore) {
					return nil, &ChainStore{}
				},
				wantNonNil:  true,
				wantInStore: -1,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				ctx := context.Background()
				mocks, chain := tc.stores()
				err := chain.Set(ctx, "k", "v")
				if tc.wantErr != nil {
					if !errors.Is(err, tc.wantErr) {
						t.Fatalf("Set err = %v, want %v", err, tc.wantErr)
					}
				} else if tc.wantNonNil {
					if err == nil {
						t.Fatalf("expected non-nil error from Set")
					}
				} else if err != nil {
					t.Fatalf("unexpected Set err: %v", err)
				}

				for i, m := range mocks {
					if m.setCalls != tc.wantSetCalls[i] {
						t.Errorf("store[%d] setCalls = %d, want %d", i, m.setCalls, tc.wantSetCalls[i])
					}
					_, getErr := m.Get(ctx, "k")
					if i == tc.wantInStore && getErr != nil {
						t.Errorf("expected key in store[%d], got err=%v", i, getErr)
					}
					if i != tc.wantInStore && getErr == nil {
						t.Errorf("did not expect key in store[%d]", i)
					}
				}
			})
		}
	})

	t.Run("Delete", func(t *testing.T) {
		cases := []struct {
			name         string
			stores       func() ([]*faultSecretStore, *ChainStore)
			wantErr      error
			wantNonNil   bool
			wantDelCalls []int
		}{
			{
				name: "deletes from every store",
				stores: func() ([]*faultSecretStore, *ChainStore) {
					s0 := &faultSecretStore{inner: NewMemoryStore(map[string]string{"k": "v1"})}
					s1 := &faultSecretStore{inner: NewMemoryStore(map[string]string{"k": "v2"})}
					return []*faultSecretStore{s0, s1}, &ChainStore{Stores: []SecretStore{s0, s1}}
				},
				wantDelCalls: []int{1, 1},
			},
			{
				name: "deletes from fallback when missing in primary",
				stores: func() ([]*faultSecretStore, *ChainStore) {
					s0 := &faultSecretStore{inner: NewMemoryStore(nil)}
					s1 := &faultSecretStore{inner: NewMemoryStore(map[string]string{"k": "v2"})}
					return []*faultSecretStore{s0, s1}, &ChainStore{Stores: []SecretStore{s0, s1}}
				},
				wantDelCalls: []int{1, 1},
			},
			{
				name: "attempts all stores and returns first real error",
				stores: func() ([]*faultSecretStore, *ChainStore) {
					s0 := &faultSecretStore{inner: NewMemoryStore(nil)} // ErrSecretNotFound
					s1 := &faultSecretStore{inner: NewMemoryStore(nil), deleteErr: errPrimaryDown}
					s2 := &faultSecretStore{inner: NewMemoryStore(map[string]string{"k": "v3"})}
					s3 := &faultSecretStore{inner: NewMemoryStore(nil), deleteErr: errSecondaryDown}
					return []*faultSecretStore{s0, s1, s2, s3}, &ChainStore{Stores: []SecretStore{s0, s1, s2, s3}}
				},
				wantErr:      errPrimaryDown,
				wantDelCalls: []int{1, 1, 1, 1},
			},
			{
				name: "returns ErrSecretNotFound when absent in all stores",
				stores: func() ([]*faultSecretStore, *ChainStore) {
					s0 := &faultSecretStore{inner: NewMemoryStore(nil)}
					s1 := &faultSecretStore{inner: NewMemoryStore(nil)}
					return []*faultSecretStore{s0, s1}, &ChainStore{Stores: []SecretStore{s0, s1}}
				},
				wantErr:      ErrSecretNotFound,
				wantDelCalls: []int{1, 1},
			},
			{
				name: "fails closed when no stores configured",
				stores: func() ([]*faultSecretStore, *ChainStore) {
					return nil, &ChainStore{Stores: []SecretStore{nil}}
				},
				wantNonNil: true,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				ctx := context.Background()
				mocks, chain := tc.stores()
				err := chain.Delete(ctx, "k")
				if tc.wantErr != nil {
					if !errors.Is(err, tc.wantErr) {
						t.Fatalf("Delete err = %v, want %v", err, tc.wantErr)
					}
				} else if tc.wantNonNil {
					if err == nil {
						t.Fatalf("expected non-nil error from Delete")
					}
				} else if err != nil {
					t.Fatalf("unexpected Delete err: %v", err)
				}

				for i, m := range mocks {
					if m.delCalls != tc.wantDelCalls[i] {
						t.Errorf("store[%d] delCalls = %d, want %d", i, m.delCalls, tc.wantDelCalls[i])
					}
					if m.deleteErr == nil {
						if _, getErr := m.Get(ctx, "k"); !errors.Is(getErr, ErrSecretNotFound) {
							t.Errorf("store[%d] still has key after Delete: getErr=%v", i, getErr)
						}
					}
				}
			})
		}
	})
}
