package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/secret"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestRegistryConnectSyncAndDisconnect(t *testing.T) {
	ctx := context.Background()
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Host {
		case "openai.test":
			if r.Header.Get("Authorization") != "Bearer sk-openai" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5"},{"id":"text-embedding-3-large"},{"id":"o3"}]}`))
		case "anthropic.test":
			if r.Header.Get("x-api-key") != "sk-ant" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-4-5"},{"id":"claude-opus-4-1"}]}`))
		case "gemini.test":
			if r.Header.Get("x-goog-api-key") != "AIza-gem" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent"]},{"name":"models/embedding-001","supportedGenerationMethods":["embedContent"]}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	cli := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, r)
			return rec.Result(), nil
		}),
	}

	secrets := secret.NewMemoryStore(nil)
	reg := NewRegistry(Options{
		Secrets: secrets,
		Client:  cli,
		BaseURL: map[string]string{
			"openai":    "https://openai.test",
			"anthropic": "https://anthropic.test",
			"gemini":    "https://gemini.test",
		},
	})

	if err := reg.Connect(ctx, "openai", "sk-openai"); err != nil {
		t.Fatalf("Connect openai: %v", err)
	}
	if err := reg.Connect(ctx, "anthropic", "sk-ant"); err != nil {
		t.Fatalf("Connect anthropic: %v", err)
	}
	if err := reg.Connect(ctx, "gemini", "AIza-gem"); err != nil {
		t.Fatalf("Connect gemini: %v", err)
	}

	oaModels, err := reg.SyncModels(ctx, "openai")
	if err != nil || len(oaModels) != 2 {
		t.Fatalf("expected 2 filtered OpenAI chat models, got %+v err=%v", oaModels, err)
	}
	antModels, err := reg.SyncModels(ctx, "anthropic")
	if err != nil || len(antModels) != 2 {
		t.Fatalf("expected 2 Anthropic models, got %+v err=%v", antModels, err)
	}
	gemModels, err := reg.SyncModels(ctx, "gemini")
	if err != nil || len(gemModels) != 1 || gemModels[0].ID != "gemini-2.5-pro" {
		t.Fatalf("expected gemini-2.5-pro only, got %+v err=%v", gemModels, err)
	}

	if err := reg.Disconnect(ctx, "openai"); err != nil {
		t.Fatalf("Disconnect openai: %v", err)
	}
	if _, err := reg.SyncModels(ctx, "openai"); err == nil {
		t.Fatal("expected SyncModels to fail after Disconnect")
	}
}

func TestParseAgyModelsAndCodexCacheAndCustomModels(t *testing.T) {
	raw := "\r⠋ Fetching available models...\rgemini-4-neon-high             Gemini 4 Neon (High)\nclaude-opus-5-5-high           Claude Opus 5.5 (High)\n"
	agy := parseAgyModelsOutput(raw)
	if len(agy) != 2 || agy[0].ID != "gemini-4-neon-high" || agy[1].ID != "claude-opus-5-5-high" {
		t.Fatalf("unexpected agy models: %+v", agy)
	}

	reg := NewRegistry(Options{})
	if err := reg.SavePrefs(context.Background(), "cli:claude", "claude-fable-5-2", []string{"claude-opus-5-5", "claude-fable-5-2", "custom-model-x"}); err != nil {
		t.Fatalf("SavePrefs: %v", err)
	}
	var found bool
	for _, p := range reg.List(context.Background()) {
		if p.ID == "cli:claude" {
			for _, m := range p.Models {
				if m.ID == "custom-model-x" && m.Enabled {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("expected custom-model-x to be preserved in cli:claude models")
	}
}
