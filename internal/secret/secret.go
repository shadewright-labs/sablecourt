package secret

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/zalando/go-keyring"
)

var (
	ErrSecretNotFound   = errors.New("secret not found")
	ErrInsecureFileMode = errors.New("secret file permissions must be 0600 or stricter")
)

type SecretStore interface {
	Get(ctx context.Context, name string) (string, error)
	Set(ctx context.Context, name, value string) error
	Delete(ctx context.Context, name string) error
}

// KeyringStore stores secrets in the OS keychain via zalando/go-keyring.
type KeyringStore struct {
	Service string
}

func NewKeyringStore(service string) *KeyringStore {
	if service == "" {
		service = "sablecourt"
	}
	return &KeyringStore{Service: service}
}

func (k *KeyringStore) Get(_ context.Context, name string) (string, error) {
	val, err := keyring.Get(k.Service, name)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrSecretNotFound
	}
	return val, err
}

func (k *KeyringStore) Set(_ context.Context, name, value string) error {
	return keyring.Set(k.Service, name, value)
}

func (k *KeyringStore) Delete(_ context.Context, name string) error {
	err := keyring.Delete(k.Service, name)
	if errors.Is(err, keyring.ErrNotFound) {
		return ErrSecretNotFound
	}
	return err
}

// FileStore reads and writes a 0600 JSON map of secret names to values.
// It fails closed if the file has group or world permission bits set.
type FileStore struct {
	Path string
	mu   sync.Mutex
}

func NewFileStore(path string) *FileStore {
	return &FileStore{Path: path}
}

func (f *FileStore) Get(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	data, err := f.loadLocked()
	if err != nil {
		return "", err
	}
	val, ok := data[name]
	if !ok {
		return "", ErrSecretNotFound
	}
	return val, nil
}

func (f *FileStore) Set(_ context.Context, name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	data, err := f.loadLocked()
	if err != nil && !errors.Is(err, ErrSecretNotFound) {
		return err
	}
	if data == nil {
		data = make(map[string]string)
	}
	data[name] = value
	return f.saveLocked(data)
}

func (f *FileStore) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	data, err := f.loadLocked()
	if err != nil {
		return err
	}
	if _, ok := data[name]; !ok {
		return ErrSecretNotFound
	}
	delete(data, name)
	return f.saveLocked(data)
}

func (f *FileStore) loadLocked() (map[string]string, error) {
	info, err := os.Stat(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrSecretNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("stat secret file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: got %04o", ErrInsecureFileMode, info.Mode().Perm())
	}

	raw, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, fmt.Errorf("read secret file: %w", err)
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse secret file: %w", err)
	}
	return out, nil
}

func (f *FileStore) saveLocked(data map[string]string) error {
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(f.Path, raw, 0o600)
}

// ChainStore tries stores in order on Get and Set, and removes from every store on Delete.
type ChainStore struct {
	Stores []SecretStore
}

func (c *ChainStore) Get(ctx context.Context, name string) (string, error) {
	var lastErr error
	for _, st := range c.Stores {
		if st == nil {
			continue
		}
		val, err := st.Get(ctx, name)
		if err == nil {
			return val, nil
		}
		if errors.Is(err, ErrSecretNotFound) {
			continue
		}
		lastErr = err
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", ErrSecretNotFound
}

func (c *ChainStore) Set(ctx context.Context, name, value string) error {
	var lastErr error
	for _, st := range c.Stores {
		if st == nil {
			continue
		}
		err := st.Set(ctx, name, value)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return lastErr
	}
	return errors.New("no primary secret store configured")
}

func (c *ChainStore) Delete(ctx context.Context, name string) error {
	var (
		hasStore bool
		deleted  bool
		firstErr error
	)
	for _, st := range c.Stores {
		if st == nil {
			continue
		}
		hasStore = true
		err := st.Delete(ctx, name)
		if err == nil {
			deleted = true
			continue
		}
		if errors.Is(err, ErrSecretNotFound) {
			continue
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if !hasStore {
		return errors.New("no primary secret store configured")
	}
	if firstErr != nil {
		return firstErr
	}
	if !deleted {
		return ErrSecretNotFound
	}
	return nil
}

// MemoryStore is an in-memory SecretStore for tests.
type MemoryStore struct {
	mu     sync.RWMutex
	values map[string]string
}

func NewMemoryStore(initial map[string]string) *MemoryStore {
	cp := make(map[string]string, len(initial))
	for k, v := range initial {
		cp[k] = v
	}
	return &MemoryStore{values: cp}
}

func (m *MemoryStore) Get(_ context.Context, name string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.values[name]
	if !ok {
		return "", ErrSecretNotFound
	}
	return v, nil
}

func (m *MemoryStore) Set(_ context.Context, name, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[name] = value
	return nil
}

func (m *MemoryStore) Delete(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.values[name]; !ok {
		return ErrSecretNotFound
	}
	delete(m.values, name)
	return nil
}
