package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

type ModelInfo struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Enabled  bool   `json:"enabled"`
}

type ProviderStatus struct {
	ID            string      `json:"id"`
	DisplayName   string      `json:"displayName"`
	Kind          string      `json:"kind"` // api_key | local | cli
	SecretName    string      `json:"secretName,omitempty"`
	BaseURL       string      `json:"baseUrl,omitempty"`
	Connected     bool        `json:"connected"`
	Detail        string      `json:"detail,omitempty"`
	DefaultModel  string      `json:"defaultModel,omitempty"`
	EnabledModels []string    `json:"enabledModels,omitempty"`
	Models        []ModelInfo `json:"models,omitempty"`
	LastSyncedAt  *time.Time  `json:"lastSyncedAt,omitempty"`
}

type providerDef struct {
	ID            string
	DisplayName   string
	Kind          string
	SecretName    string
	BaseURL       string
	DefaultModels []string
}

var builtinProviders = []providerDef{
	{
		ID:            "openai",
		DisplayName:   "OpenAI (GPT)",
		Kind:          "api_key",
		SecretName:    "openai_api_key",
		BaseURL:       "https://api.openai.com",
		DefaultModels: []string{"gpt-5", "gpt-5-mini", "gpt-4.1", "o3"},
	},
	{
		ID:            "anthropic",
		DisplayName:   "Anthropic (Claude)",
		Kind:          "api_key",
		SecretName:    "anthropic_api_key",
		BaseURL:       "https://api.anthropic.com",
		DefaultModels: []string{"claude-sonnet-4-5", "claude-opus-4-1", "claude-haiku-3-5"},
	},
	{
		ID:            "gemini",
		DisplayName:   "Google Gemini",
		Kind:          "api_key",
		SecretName:    "gemini_api_key",
		BaseURL:       "https://generativelanguage.googleapis.com",
		DefaultModels: []string{"gemini-2.5-pro", "gemini-2.5-flash"},
	},
	{
		ID:            "local",
		DisplayName:   "Local (Ollama / LM Studio)",
		Kind:          "local",
		SecretName:    "local_model_api_key",
		BaseURL:       "http://127.0.0.1:11434",
		DefaultModels: []string{"qwen2.5-coder:32b", "llama3.3:70b"},
	},
}

type cliDef struct {
	ID            string
	DisplayName   string
	Bin           string
	DefaultModels []string
}

var builtinCLIs = []cliDef{
	{
		ID:            "cli:claude",
		DisplayName:   "Claude Code CLI (claude)",
		Bin:           "claude",
		DefaultModels: []string{"claude-sonnet-4-6", "claude-opus-4-6", "claude-haiku-4-5", "sonnet", "opus"},
	},
	{
		ID:            "cli:codex",
		DisplayName:   "Codex CLI (codex)",
		Bin:           "codex",
		DefaultModels: []string{"gpt-5.3-codex", "gpt-5.2-codex", "o3", "gpt-5-mini"},
	},
	{
		ID:            "cli:agy",
		DisplayName:   "Antigravity CLI (agy)",
		Bin:           "agy",
		DefaultModels: []string{"gemini-2.5-pro", "gemini-2.5-flash", "claude-sonnet-4-6"},
	},
}

func findCLIDef(id string) (cliDef, bool) {
	for _, c := range builtinCLIs {
		if c.ID == id {
			return c, true
		}
	}
	return cliDef{}, false
}

type Registry struct {
	store   store.Store
	secrets secret.SecretStore
	client  *http.Client
	baseMap map[string]string
	look    func(string) (string, error)

	mu         sync.RWMutex
	synced     map[string][]ModelInfo
	syncedAt   map[string]time.Time
	lastErrors map[string]string
	prefs      map[string]domain.ProviderPrefs
}

type Options struct {
	Store   store.Store
	Secrets secret.SecretStore
	Client  *http.Client
	BaseURL map[string]string
}

func NewRegistry(opts Options) *Registry {
	cli := opts.Client
	if cli == nil {
		cli = &http.Client{Timeout: 10 * time.Second}
	}
	baseMap := make(map[string]string)
	for _, p := range builtinProviders {
		baseMap[p.ID] = p.BaseURL
	}
	for k, v := range opts.BaseURL {
		if v != "" {
			baseMap[k] = strings.TrimRight(v, "/")
		}
	}
	prefs := make(map[string]domain.ProviderPrefs)
	if opts.Store != nil {
		if loaded, err := opts.Store.ListProviderPrefs(context.Background()); err == nil {
			prefs = loaded
		}
	}
	return &Registry{
		store:      opts.Store,
		secrets:    opts.Secrets,
		client:     cli,
		baseMap:    baseMap,
		look:       exec.LookPath,
		synced:     make(map[string][]ModelInfo),
		syncedAt:   make(map[string]time.Time),
		lastErrors: make(map[string]string),
		prefs:      prefs,
	}
}

func (r *Registry) SavePrefs(ctx context.Context, providerID, defaultModel string, enabledModels []string) error {
	_, isAPI := findDef(providerID)
	_, isCLI := findCLIDef(providerID)
	if !isAPI && !isCLI {
		return fmt.Errorf("unknown provider %q", providerID)
	}
	cleaned := make([]string, 0, len(enabledModels))
	for _, m := range enabledModels {
		if s := strings.TrimSpace(m); s != "" && !slices.Contains(cleaned, s) {
			cleaned = append(cleaned, s)
		}
	}
	defModel := strings.TrimSpace(defaultModel)
	if defModel != "" && len(cleaned) > 0 && !slices.Contains(cleaned, defModel) {
		cleaned = append([]string{defModel}, cleaned...)
	}
	p := domain.ProviderPrefs{
		ProviderID:    providerID,
		DefaultModel:  defModel,
		EnabledModels: cleaned,
		UpdatedAt:     time.Now().UTC(),
	}
	if r.store != nil {
		if err := r.store.UpsertProviderPrefs(ctx, p); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.prefs[providerID] = p
	r.mu.Unlock()
	return nil
}

func (r *Registry) List(ctx context.Context) []ProviderStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]ProviderStatus, 0, len(builtinProviders)+2)
	for _, def := range builtinProviders {
		st := ProviderStatus{
			ID:          def.ID,
			DisplayName: def.DisplayName,
			Kind:        def.Kind,
			SecretName:  def.SecretName,
			BaseURL:     r.baseMap[def.ID],
		}
		if def.Kind == "api_key" {
			if r.secrets != nil {
				if val, err := r.secrets.Get(ctx, def.SecretName); err == nil && strings.TrimSpace(val) != "" {
					st.Connected = true
					st.Detail = "key stored in SecretStore"
				} else {
					st.Detail = "API key not configured"
				}
			}
		} else {
			st.Connected = len(r.synced[def.ID]) > 0
			if st.Connected {
				st.Detail = "local endpoint reachable"
			} else {
				st.Detail = "click Sync to query local endpoint"
			}
		}

		pref := r.prefs[def.ID]
		st.DefaultModel = pref.DefaultModel
		st.EnabledModels = append([]string(nil), pref.EnabledModels...)

		var rawModels []ModelInfo
		if models, ok := r.synced[def.ID]; ok && len(models) > 0 {
			rawModels = append([]ModelInfo(nil), models...)
			ts := r.syncedAt[def.ID]
			st.LastSyncedAt = &ts
		} else {
			for _, m := range def.DefaultModels {
				rawModels = append(rawModels, ModelInfo{ID: m, Provider: def.ID})
			}
		}
		for i := range rawModels {
			rawModels[i].Enabled = len(pref.EnabledModels) == 0 || slices.Contains(pref.EnabledModels, rawModels[i].ID)
		}
		st.Models = rawModels
		if st.DefaultModel == "" && len(st.Models) > 0 {
			for _, m := range st.Models {
				if m.Enabled {
					st.DefaultModel = m.ID
					break
				}
			}
		}
		if errMsg := r.lastErrors[def.ID]; errMsg != "" {
			st.Detail = errMsg
		}
		out = append(out, st)
	}

	// Local coding CLI detection (presence in PATH only; never scans host processes or transcripts).
	for _, cliDef := range builtinCLIs {
		st := ProviderStatus{
			ID:          cliDef.ID,
			DisplayName: cliDef.DisplayName,
			Kind:        "cli",
		}
		if path, err := r.look(cliDef.Bin); err == nil && path != "" {
			st.Connected = true
			st.Detail = "installed at " + path
		} else {
			st.Detail = "binary not found in PATH"
		}

		pref := r.prefs[cliDef.ID]
		st.DefaultModel = pref.DefaultModel
		st.EnabledModels = append([]string(nil), pref.EnabledModels...)

		rawModels := make([]ModelInfo, 0, len(cliDef.DefaultModels))
		for _, m := range cliDef.DefaultModels {
			rawModels = append(rawModels, ModelInfo{
				ID:       m,
				Provider: cliDef.ID,
				Enabled:  len(pref.EnabledModels) == 0 || slices.Contains(pref.EnabledModels, m),
			})
		}
		st.Models = rawModels
		if st.DefaultModel == "" && len(st.Models) > 0 {
			for _, m := range st.Models {
				if m.Enabled {
					st.DefaultModel = m.ID
					break
				}
			}
		}
		out = append(out, st)
	}

	return out
}

func (r *Registry) Connect(ctx context.Context, providerID, apiKey string) error {
	def, ok := findDef(providerID)
	if !ok || def.SecretName == "" {
		return fmt.Errorf("unknown API provider %q", providerID)
	}
	key := strings.TrimSpace(apiKey)
	if key == "" {
		return errors.New("API key cannot be empty")
	}
	if r.secrets == nil {
		return errors.New("secret store unavailable")
	}
	if err := r.secrets.Set(ctx, def.SecretName, key); err != nil {
		return fmt.Errorf("store %s: %w", def.SecretName, err)
	}
	r.mu.Lock()
	delete(r.lastErrors, providerID)
	r.mu.Unlock()
	return nil
}

func (r *Registry) Disconnect(ctx context.Context, providerID string) error {
	def, ok := findDef(providerID)
	if !ok || def.SecretName == "" {
		return fmt.Errorf("unknown API provider %q", providerID)
	}
	if r.secrets == nil {
		return errors.New("secret store unavailable")
	}
	err := r.secrets.Delete(ctx, def.SecretName)
	if err != nil && !errors.Is(err, secret.ErrSecretNotFound) {
		return err
	}
	r.mu.Lock()
	delete(r.synced, providerID)
	delete(r.syncedAt, providerID)
	delete(r.lastErrors, providerID)
	r.mu.Unlock()
	return nil
}

// SyncModels queries the upstream provider's /models endpoint using the stored credential
// and caches the available model IDs.
func (r *Registry) SyncModels(ctx context.Context, providerID string) ([]ModelInfo, error) {
	def, ok := findDef(providerID)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", providerID)
	}

	var apiKey string
	if def.Kind == "api_key" {
		if r.secrets == nil {
			return nil, errors.New("secret store unavailable")
		}
		val, err := r.secrets.Get(ctx, def.SecretName)
		if err != nil || strings.TrimSpace(val) == "" {
			return nil, fmt.Errorf("%s not configured in SecretStore", def.SecretName)
		}
		apiKey = strings.TrimSpace(val)
	} else if r.secrets != nil {
		if val, err := r.secrets.Get(ctx, def.SecretName); err == nil {
			apiKey = strings.TrimSpace(val)
		}
	}

	base := r.baseMap[providerID]
	var models []ModelInfo
	var err error
	switch providerID {
	case "openai", "local":
		models, err = r.fetchOpenAIModels(ctx, providerID, base, apiKey)
	case "anthropic":
		models, err = r.fetchAnthropicModels(ctx, base, apiKey)
	case "gemini":
		models, err = r.fetchGeminiModels(ctx, base, apiKey)
	default:
		return nil, fmt.Errorf("sync not supported for %q", providerID)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.lastErrors[providerID] = err.Error()
		return nil, err
	}
	delete(r.lastErrors, providerID)
	r.synced[providerID] = models
	r.syncedAt[providerID] = time.Now().UTC()
	return append([]ModelInfo(nil), models...), nil
}

func (r *Registry) fetchOpenAIModels(ctx context.Context, providerID, base, apiKey string) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s /v1/models returned HTTP %d", providerID, resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	var out []ModelInfo
	for _, m := range payload.Data {
		if providerID == "openai" && !isOpenAIChatModel(m.ID) {
			continue
		}
		out = append(out, ModelInfo{ID: m.ID, Provider: providerID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *Registry) fetchAnthropicModels(ctx context.Context, base, apiKey string) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("anthropic /v1/models returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	var out []ModelInfo
	for _, m := range payload.Data {
		out = append(out, ModelInfo{ID: m.ID, Provider: "anthropic"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *Registry) fetchGeminiModels(ctx context.Context, base, apiKey string) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1beta/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-goog-api-key", apiKey)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("gemini /v1beta/models returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Models []struct {
			Name                       string   `json:"name"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	var out []ModelInfo
	for _, m := range payload.Models {
		supportsGen := len(m.SupportedGenerationMethods) == 0
		for _, method := range m.SupportedGenerationMethods {
			if method == "generateContent" {
				supportsGen = true
				break
			}
		}
		if !supportsGen {
			continue
		}
		id := strings.TrimPrefix(m.Name, "models/")
		out = append(out, ModelInfo{ID: id, Provider: "gemini"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func isOpenAIChatModel(id string) bool {
	lower := strings.ToLower(id)
	for _, skip := range []string{"embedding", "whisper", "tts", "dall-e", "moderation", "babbage", "davinci", "audio", "realtime"} {
		if strings.Contains(lower, skip) {
			return false
		}
	}
	return strings.HasPrefix(lower, "gpt-") || strings.HasPrefix(lower, "o1") || strings.HasPrefix(lower, "o3") || strings.HasPrefix(lower, "o4") || strings.HasPrefix(lower, "chatgpt-")
}

func findDef(id string) (providerDef, bool) {
	for _, d := range builtinProviders {
		if d.ID == id {
			return d, true
		}
	}
	return providerDef{}, false
}
