package model

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

// trickleReader returns its text a few bytes per Read, so a value echoed in a
// response body crosses read boundaries.
type trickleReader struct {
	s string
	n int
}

func (r *trickleReader) Read(p []byte) (int, error) {
	if r.s == "" {
		return 0, io.EOF
	}
	n := min(r.n, len(r.s), len(p))
	copy(p, r.s[:n])
	r.s = r.s[n:]
	return n, nil
}

func responseGate(t *testing.T, secrets map[string]string, rt roundTripFunc) (*Gate, string) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "mg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "agent-1", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent})
	if err := st.CreateTask(ctx, domain.Task{ID: "t-resp", WorkloadName: "agent-1", State: domain.TaskStateWorking, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	tok, err := MintTaskToken(ctx, st, "t-resp", "agent-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bundle := &config.Bundle{
		Budgets: map[string]domain.Budget{"b1": {Metadata: domain.ObjectMeta{Name: "b1"}, Spec: domain.BudgetSpec{PerTask: domain.PerTaskBudget{MaxCostUSD: 5}}}},
		Workloads: map[string]domain.Workload{"agent-1": {
			Metadata: domain.ObjectMeta{Name: "agent-1", Zone: domain.ZoneRuntime},
			Spec: domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Grants: domain.GrantSet{
				ModelGate: domain.ModelGateGrant{Allowed: true, Providers: []string{"openai", "local"}, BudgetRef: "b1"},
			}},
		}},
	}
	gate := New(Options{Store: st, Secrets: secret.NewMemoryStore(secrets), Bundle: bundle, Client: &http.Client{Transport: rt}})
	return gate, tok
}

// Third review: the gate injected the provider key, then copied upstream
// headers and body chunks to the workload unchanged. An upstream that echoes
// the key must not hand it to the workload, on the primary or the fallback
// path, in a header or across body reads.
func TestModelGateRedactsProviderKeyInResponses(t *testing.T) {
	openaiKey := "sk-" + "echo_openai_" + strings.Repeat("k1", 10)
	localKey := "local_" + "echo_key_" + strings.Repeat("k2", 10)
	echo := func(key string) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
				"X-Echo":       []string{"key=" + key},
			},
			Body: io.NopCloser(&trickleReader{s: `{"model":"m","echo":"` + key + `","usage":{"prompt_tokens":1,"completion_tokens":1}}`, n: 3}),
		}
	}
	for _, tc := range []struct {
		name string
		rt   roundTripFunc
	}{
		{"primary", func(r *http.Request) (*http.Response, error) {
			return echo(openaiKey), nil
		}},
		{"primary error body", func(r *http.Request) (*http.Response, error) {
			resp := echo(openaiKey)
			resp.StatusCode = http.StatusUnauthorized
			return resp, nil
		}},
		{"fallback", func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Host, "openai.com") {
				return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			}
			resp := echo(localKey)
			resp.Header.Add("X-Echo", openaiKey)
			resp.Body = io.NopCloser(&trickleReader{s: `{"model":"m","a":"` + openaiKey + `","b":"` + localKey + `"}`, n: 5})
			return resp, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate, tok := responseGate(t, map[string]string{"openai_api_key": openaiKey, "local_model_api_key": localKey}, tc.rt)
			req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			gate.Handler().ServeHTTP(rec, req)
			var headers strings.Builder
			for k, vs := range rec.Header() {
				headers.WriteString(k + ": " + strings.Join(vs, ",") + "\n")
			}
			for _, key := range []string{openaiKey, localKey} {
				if strings.Contains(rec.Body.String(), key) || strings.Contains(headers.String(), key) {
					t.Fatalf("response to the workload shows a provider key:\n%s\n%s", headers.String(), rec.Body.String())
				}
			}
			if !strings.Contains(rec.Body.String(), `"model":"m"`) {
				t.Fatalf("response body lost: %q", rec.Body.String())
			}
		})
	}
}

// A provider key shorter than the matcher minimum is refused at load: the
// request fails, the key never goes upstream, and the error names the setting.
func TestModelGateRefusesShortProviderKey(t *testing.T) {
	short := "sk" + "-s7x"
	called := false
	gate, tok := responseGate(t, map[string]string{"openai_api_key": short}, func(r *http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	gate.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || called {
		t.Fatalf("status %d, upstream called %v: %s", rec.Code, called, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "openai_api_key") || strings.Contains(rec.Body.String(), short) {
		t.Fatalf("refusal must name the setting and not the value: %s", rec.Body.String())
	}
}

// observeLikeDaemon wires the gate to an ObservingStore that registers what it
// reads and refuses short values, as the daemon does.
func observeLikeDaemon(gate *Gate, values map[string]string) {
	gate.secrets = secret.NewObservingStore(secret.NewMemoryStore(values), func(name, v string) error {
		return redact.RegisterSetting(name, v)
	})
}

// Fix round: behind the daemon's ObservingStore a short key comes back as a
// *redact.ShortSecretError from Get. The primary path reported a missing
// credential without the setting, and the fallback path called a provider
// whose key is optional with no key at all.
func TestModelGateShortKeyBehindObservingStore(t *testing.T) {
	short := "lo" + "-s7x"
	openaiKey := "sk-" + "obs_openai_" + strings.Repeat("k3", 10)
	t.Run("primary names the setting", func(t *testing.T) {
		called := false
		gate, tok := responseGate(t, nil, func(r *http.Request) (*http.Response, error) {
			called = true
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		})
		observeLikeDaemon(gate, map[string]string{"openai_api_key": "sk" + "-s7x"})
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		gate.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadGateway || called {
			t.Fatalf("status %d, upstream called %v: %s", rec.Code, called, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "openai_api_key") || strings.Contains(rec.Body.String(), "sk"+"-s7x") {
			t.Fatalf("refusal must name the setting and not the value: %s", rec.Body.String())
		}
	})
	t.Run("fallback skips a refused optional key", func(t *testing.T) {
		localCalled := false
		gate, tok := responseGate(t, nil, func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Host, "openai.com") {
				return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			}
			localCalled = true
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"model":"m"}`))}, nil
		})
		observeLikeDaemon(gate, map[string]string{"openai_api_key": openaiKey, "local_model_api_key": short})
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		gate.Handler().ServeHTTP(rec, req)
		if localCalled {
			t.Fatalf("fallback called the provider whose key was refused: %d %s", rec.Code, rec.Body.String())
		}
	})
}

// Fix round: only the first Content-Encoding value was read, so a second
// header or a list after "identity" let a compressed body past the matcher.
// A header whose name shows a held value is dropped.
func TestModelGateRefusesEncodedResponsesAndDropsHeldHeaderNames(t *testing.T) {
	// Canonical header spelling, so the name survives as written.
	openaiKey := "Sk-" + "Enc-Openai-" + strings.Repeat("42", 10)
	for _, tc := range []struct {
		name   string
		header http.Header
		refuse bool
	}{
		{"gzip", http.Header{"Content-Encoding": {"gzip"}}, true},
		{"second header", http.Header{"Content-Encoding": {"identity", "gzip"}}, true},
		{"empty then gzip", http.Header{"Content-Encoding": {"", "br"}}, true},
		{"list", http.Header{"Content-Encoding": {"identity, gzip"}}, true},
		{"identity", http.Header{"Content-Encoding": {"identity"}}, false},
		{"held value in a header name", http.Header{"X-" + openaiKey: {"v"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate, tok := responseGate(t, map[string]string{"openai_api_key": openaiKey}, func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: tc.header.Clone(), Body: io.NopCloser(strings.NewReader(`{"model":"m"}`))}, nil
			})
			req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			gate.Handler().ServeHTTP(rec, req)
			if tc.refuse {
				if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), `"model":"m"`) {
					t.Fatalf("encoded response passed: %d %s", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			for k := range rec.Header() {
				if strings.Contains(k, openaiKey) {
					t.Fatalf("header name shows the key: %q", k)
				}
			}
		})
	}
}
