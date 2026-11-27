package security_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/a2a"
	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	mcpgate "github.com/shadewright-labs/sablecourt/internal/gate/mcp"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/integrations"
	"github.com/shadewright-labs/sablecourt/internal/notify"
	"github.com/shadewright-labs/sablecourt/internal/operatortoken"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/providers"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
	"github.com/shadewright-labs/sablecourt/internal/voice"
	"github.com/shadewright-labs/sablecourt/internal/webhook"
)

func synthVal(parts ...string) string {
	return strings.Join(parts, "")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func httpClientFunc(fn func(*http.Request) *http.Response) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return fn(r), nil
		}),
	}
}

type heldSecretCase struct {
	name  string
	value string
}

type encodedForm struct {
	name   string
	encode func(string) (payload string, mustNotContain []string)
}

func secretEncodings() []encodedForm {
	return []encodedForm{
		{
			name: "raw",
			encode: func(s string) (string, []string) {
				return "begin " + s + " end", []string{s}
			},
		},
		{
			name: "quoted",
			encode: func(s string) (string, []string) {
				q := strconv.Quote(s)
				inner := q[1 : len(q)-1]
				return "quoted=" + q + " done", []string{s, inner}
			},
		},
		{
			name: "json_escaped",
			encode: func(s string) (string, []string) {
				jb, _ := json.Marshal(s)
				inner := string(jb[1 : len(jb)-1])
				return `{"item":` + string(jb) + `}`, []string{s, inner}
			},
		},
		{
			name: "split_crlf",
			encode: func(s string) (string, []string) {
				mid := len(s) / 2
				p1, p2 := s[:mid], s[mid:]
				return "lead " + p1 + "\r\n" + p2 + " tail", []string{s, p1, p2}
			},
		},
	}
}

func TestExactSecretRedactionMatrix(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "matrix.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Build a set of daemon-held secrets registered through their actual holders.
	providerKey := synthVal("prov", "_opaque_", "key_matrix_771199")
	rotatedProviderKey := synthVal("prov", "_rotated_", "key_matrix_882200")
	upstreamMCPSecret := synthVal("upmcp", "_opaque_", "cred_matrix_663311")
	ghToken := synthVal("ghtok", "_opaque_", "cred_matrix_554422")
	ghWebhookSecret := synthVal("whsec", "_opaque_", "sig_matrix_445533")
	shadewrightToken := synthVal("swtok", "_opaque_", "int_matrix_336644")
	a2aToken := synthVal("a2atok", "_opaque_", "rpc_matrix_227755")
	specialSecret := synthVal(`op"q\sec`, `<>&/`, "matrix_990011")

	// Capture outbound notification payloads in-process (no sockets needed).
	var (
		notifyMu   sync.Mutex
		notifyBody string
	)
	notifyClient := httpClientFunc(func(r *http.Request) *http.Response {
		raw, _ := io.ReadAll(r.Body)
		notifyMu.Lock()
		notifyBody = string(raw)
		notifyMu.Unlock()
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}
	})
	notifyWebhookURL := "https://hooks.example.invalid/" + synthVal("hook", "_token_", "matrix_118899")

	rawMem := secret.NewMemoryStore(map[string]string{
		"openai_api_key":           providerKey,
		"mcp_upstream_cred":        upstreamMCPSecret,
		"github_token":             ghToken,
		"github_webhook_secret":    ghWebhookSecret,
		"shadewright_bearer_token": shadewrightToken,
		"a2a_bearer_token":         a2aToken,
		"notify_webhook_url":       notifyWebhookURL,
		"custom_special_secret":    specialSecret,
	})
	secStore := secret.NewObservingStore(rawMem, func(name string, v string) error {
		return redact.RegisterSetting(name, v)
	})

	// 1. Register provider key via providers.Registry (initial and rotated).
	provReg := providers.NewRegistry(providers.Options{Store: st, Secrets: secStore})
	_ = provReg.List(ctx)
	if err := provReg.Connect(ctx, "openai", rotatedProviderKey); err != nil {
		t.Fatalf("providers.Connect: %v", err)
	}

	// 2. Register operator token via operatortoken.LoadOrCreate.
	stateDir := filepath.Join(t.TempDir(), "state")
	opToken, err := operatortoken.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatalf("operatortoken.LoadOrCreate: %v", err)
	}

	// 3. Register task token via model.MintTaskToken.
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{
		Name: "matrix-worker", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent,
		ManifestDigest: "d", AgentCardJSON: "{}",
	}); err != nil {
		t.Fatalf("UpsertWorkload: %v", err)
	}
	if err := st.CreateTask(ctx, domain.Task{
		ID: "matrix-task", IdempotencyKey: "matrix-idem", WorkloadName: "matrix-worker", State: domain.TaskStateWorking,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	taskToken, err := model.MintTaskToken(ctx, st, "matrix-task", "matrix-worker", time.Hour)
	if err != nil {
		t.Fatalf("MintTaskToken: %v", err)
	}

	// 4. Register binary signing key material via redact.RegisterBytes.
	signingBytes := []byte(synthVal("sign", "KeyMaterial", "Matrix32Bytes!!0"))
	redact.RegisterBytes(signingBytes)
	signingHex := hex.EncodeToString(signingBytes)
	signingB64 := base64.StdEncoding.EncodeToString(signingBytes)

	// 5. Register upstream MCP secret via UpstreamBridge.CallTool.
	var (
		upstreamMu      sync.Mutex
		upstreamPayload string
	)
	upstreamClient := httpClientFunc(func(_ *http.Request) *http.Response {
		upstreamMu.Lock()
		p := upstreamPayload
		upstreamMu.Unlock()
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": p})
		hdr := make(http.Header)
		hdr.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(raw))), Header: hdr}
	})
	bridge := mcpgate.NewUpstreamBridge(secStore, upstreamClient)
	upstreamCfg := domain.UpstreamMCPServer{
		Name: "up", URL: "https://mcp.example.invalid/rpc", SecretName: "mcp_upstream_cred", AllowedTools: []string{"echo"},
	}
	if _, err := bridge.CallTool(ctx, upstreamCfg, "echo", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("initial UpstreamBridge.CallTool: %v", err)
	}

	// 6. Register GitHub token via GitHubPROpener.OpenPR.
	ghClient := httpClientFunc(func(_ *http.Request) *http.Response {
		hdr := make(http.Header)
		hdr.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(strings.NewReader(`{"html_url":"https://example.invalid/pr/1"}`)), Header: hdr}
	})
	opener := &mcpgate.GitHubPROpener{Secrets: secStore, APIURL: "https://api.github.invalid", Client: ghClient}
	if _, err := opener.OpenPR(ctx, mcpgate.OpenPRInput{Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t"}); err != nil {
		t.Fatalf("GitHubPROpener.OpenPR: %v", err)
	}

	// 7. Register webhook secret via webhook.GitHubHandler.
	reconciler := controlplane.NewReconciler(controlplane.Options{Store: st})
	whHandler := webhook.GitHubHandler(webhook.GitHubOptions{Secrets: secStore, Reconciler: reconciler})
	whBody := []byte(`{"action":"closed"}`)
	mac := hmac.New(sha256.New, []byte(ghWebhookSecret))
	mac.Write(whBody)
	whReq := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/github", strings.NewReader(string(whBody)))
	whReq.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	whReq.Header.Set("X-GitHub-Event", "issues")
	whHandler.ServeHTTP(httptest.NewRecorder(), whReq)

	// 8. Register A2A bearer token via a2a.BrokerHandler.
	a2aHandler := a2a.BrokerHandler(a2a.Options{Store: st, Reconciler: reconciler, Secrets: secStore})
	a2aReq := httptest.NewRequest(http.MethodPost, "/a2a", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"unknown"}`))
	a2aReq.Header.Set("Authorization", "Bearer "+a2aToken)
	a2aHandler.ServeHTTP(httptest.NewRecorder(), a2aReq)

	// 9. Register notify webhook URL via notify.Notifier.Send.
	notifier := notify.New(notify.Options{Secrets: secStore, HTTPClient: notifyClient})
	if err := notifier.Send(ctx, notify.Event{Type: "init", Title: "init", Detail: "init"}); err != nil {
		t.Fatalf("initial notify.Send: %v", err)
	}

	// 10. Register custom special-character secret via ObservingStore.Get.
	if _, err := secStore.Get(ctx, "custom_special_secret"); err != nil {
		t.Fatalf("secStore.Get special: %v", err)
	}

	// 11. Set up Shadewright handler and register its bearer token.
	bundle := &config.Bundle{
		Policies: map[string]domain.Policy{
			"allow-matrix": {
				Metadata: domain.ObjectMeta{Name: "allow-matrix"},
				Spec: domain.PolicySpec{
					DefaultEffect: domain.EffectAllow,
					Rules: []domain.PolicyRule{
						{Workload: "matrix-worker", Capability: "mcp:matrix.echo", Effect: domain.EffectAllow},
						{Workload: "matrix-worker", Capability: "action:matrix.act", Effect: domain.EffectAllow},
					},
				},
			},
		},
		Workloads: map[string]domain.Workload{
			"matrix-worker": {
				Metadata: domain.ObjectMeta{Name: "matrix-worker", Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class: domain.ClassLongLivedAgent,
					Grants: domain.GrantSet{
						MCPTools: []string{"matrix.echo"},
						Actions:  []string{"matrix.act"},
					},
				},
			},
		},
		Conclaves: map[string]domain.Conclave{
			"incident-tpl": {
				Metadata: domain.ObjectMeta{Name: "incident-tpl"},
				Spec: domain.ConclaveSpec{
					Vassals: []domain.ConclaveVassal{{Role: "lead", Workload: "matrix-worker"}},
				},
			},
		},
	}
	broker := conclave.NewBroker(conclave.Options{
		Store: st, Reconciler: reconciler, GetBundle: func() *config.Bundle { return bundle },
	})
	swHandler := integrations.NewShadewright(integrations.ShadewrightOptions{
		Store: st, Secrets: secStore, GetBundle: func() *config.Bundle { return bundle },
		Broker: broker, Reconciler: reconciler, DefaultTemplate: "incident-tpl",
	})
	swInitReq := httptest.NewRequest(http.MethodPost, integrations.ShadewrightIncidentPath, strings.NewReader(`{"incidentId":"inc-init","summary":"warm"}`))
	swInitReq.Header.Set("Authorization", "Bearer "+shadewrightToken)
	swHandler.IncidentHandler().ServeHTTP(httptest.NewRecorder(), swInitReq)

	// Set up MCP Gate for tool & action paths.
	var (
		gateMu      sync.Mutex
		gatePayload string
	)
	eng := policy.NewEngine([]domain.Policy{bundle.Policies["allow-matrix"]}, nil)
	gate := mcpgate.New(mcpgate.Options{Store: st, Bundle: bundle, Policy: eng})
	gate.RegisterTool(mcpgate.ToolDef{
		Name:        "matrix.echo",
		Description: "echo test payload",
		Handler: func(_ context.Context, _ domain.Workload, _ string, _ json.RawMessage) (string, error) {
			gateMu.Lock()
			defer gateMu.Unlock()
			return gatePayload, nil
		},
	})
	gate.RegisterAction("matrix.act", func(_ context.Context, _ domain.Workload, _ string, _ json.RawMessage) (any, error) {
		gateMu.Lock()
		defer gateMu.Unlock()
		return map[string]string{"out": gatePayload}, nil
	})

	// Set up STT client for voice path.
	var (
		sttMu      sync.Mutex
		sttPayload string
	)
	sttClient := httpClientFunc(func(_ *http.Request) *http.Response {
		sttMu.Lock()
		p := sttPayload
		sttMu.Unlock()
		raw, _ := json.Marshal(map[string]string{"text": p})
		hdr := make(http.Header)
		hdr.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(raw))), Header: hdr}
	})
	voiceSvc := voice.New(voice.Options{STTURL: "https://stt.example.invalid/v1/audio/transcriptions", HTTPClient: sttClient})

	// Open an ordinary conclave for message/relic path.
	sess, err := broker.Open(ctx, "incident-tpl", "matrix-room", "")
	if err != nil {
		t.Fatalf("broker.Open: %v", err)
	}

	secrets := []heldSecretCase{
		{name: "provider_api_key", value: providerKey},
		{name: "rotated_provider_key", value: rotatedProviderKey},
		{name: "upstream_mcp_secret", value: upstreamMCPSecret},
		{name: "task_token", value: taskToken},
		{name: "operator_token", value: opToken},
		{name: "signing_key_hex", value: signingHex},
		{name: "signing_key_b64", value: signingB64},
		{name: "github_token", value: ghToken},
		{name: "github_webhook_secret", value: ghWebhookSecret},
		{name: "notify_webhook_url", value: notifyWebhookURL},
		{name: "shadewright_bearer_token", value: shadewrightToken},
		{name: "a2a_bearer_token", value: a2aToken},
		{name: "special_chars_secret", value: specialSecret},
	}

	type redactingPath struct {
		name string
		run  func(t *testing.T, payload string) string
	}

	swSeq := 0
	relicSeq := 0
	paths := []redactingPath{
		{
			name: "mcp_tool_result",
			run: func(t *testing.T, payload string) string {
				t.Helper()
				gateMu.Lock()
				gatePayload = payload
				gateMu.Unlock()
				reqBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"matrix.echo","arguments":{}}}`
				req := httptest.NewRequest(http.MethodPost, "http://local/gate/mcp", strings.NewReader(reqBody))
				req.Header.Set("Authorization", "Bearer "+taskToken)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				rec := httptest.NewRecorder()
				gate.MCPHandler().ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("MCPHandler status=%d body=%s", rec.Code, rec.Body.String())
				}
				return rec.Body.String()
			},
		},
		{
			name: "mcp_action_result",
			run: func(t *testing.T, payload string) string {
				t.Helper()
				gateMu.Lock()
				gatePayload = payload
				gateMu.Unlock()
				req := httptest.NewRequest(http.MethodPost, "http://local/matrix.act", strings.NewReader(`{"payload":{}}`))
				req.Header.Set("Authorization", "Bearer "+taskToken)
				rec := httptest.NewRecorder()
				gate.ActionHandler().ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("ActionHandler status=%d body=%s", rec.Code, rec.Body.String())
				}
				return rec.Body.String()
			},
		},
		{
			name: "mcp_upstream_result",
			run: func(t *testing.T, payload string) string {
				t.Helper()
				upstreamMu.Lock()
				upstreamPayload = payload
				upstreamMu.Unlock()
				out, err := bridge.CallTool(ctx, upstreamCfg, "echo", json.RawMessage(`{}`))
				if err != nil {
					t.Fatalf("UpstreamBridge.CallTool: %v", err)
				}
				return out
			},
		},
		{
			name: "attach_session_output",
			run: func(t *testing.T, payload string) string {
				t.Helper()
				hub := attach.NewHub()
				hub.Register("stream-task", nil)
				mid := len(payload) / 2
				hub.WriteOutput("stream-task", []byte(payload[:mid]))
				hub.WriteOutput("stream-task", []byte(payload[mid:]))
				hub.Close("stream-task")
				hist, _, _, unsub, err := hub.Subscribe("stream-task")
				if err != nil {
					t.Fatalf("Subscribe: %v", err)
				}
				unsub()
				return hist
			},
		},
		{
			name: "council_prompt",
			run: func(t *testing.T, payload string) string {
				t.Helper()
				pkt, err := contextpack.AssembleCouncil("card "+payload, "question "+payload, []string{"decree " + payload}, nil)
				if err != nil {
					t.Fatalf("AssembleCouncil: %v", err)
				}
				return pkt.Markdown
			},
		},
		{
			name: "context_pack_and_handoff",
			run: func(_ *testing.T, payload string) string {
				h := contextpack.BuildVerifierPrompt(domain.Task{ID: "t-ver", WorkloadName: "matrix-worker"}, payload)
				return h + "\n" + store.RedactCouncilAnswer(payload) + "\n" + store.RedactLearningText(payload)
			},
		},
		{
			name: "conclave_message_and_relic",
			run: func(t *testing.T, payload string) string {
				t.Helper()
				msg, err := broker.PostMessage(ctx, sess.ID, "operator", "lead", "decree", payload, "")
				if err != nil {
					t.Fatalf("PostMessage: %v", err)
				}
				relicSeq++
				rName := "relic-" + strconv.Itoa(relicSeq)
				rel, err := broker.PutRelic(ctx, sess.ID, rName, "operator", "text/plain", payload)
				if err != nil {
					t.Fatalf("PutRelic: %v", err)
				}
				return msg.Body + "\n" + rel.Body
			},
		},
		{
			name: "notification",
			run: func(t *testing.T, payload string) string {
				t.Helper()
				if err := notifier.Send(ctx, notify.Event{
					Type:   "matrix.alert",
					Title:  "alert " + payload,
					Detail: "detail " + payload,
				}); err != nil {
					t.Fatalf("notify.Send: %v", err)
				}
				notifyMu.Lock()
				defer notifyMu.Unlock()
				return notifyBody
			},
		},
		{
			name: "shadewright_integration",
			run: func(t *testing.T, payload string) string {
				t.Helper()
				swSeq++
				incID := "inc-m-" + strconv.Itoa(swSeq)
				body, _ := json.Marshal(map[string]any{
					"incidentId": incID,
					"summary":    "sum " + payload,
					"logs":       payload,
				})
				req := httptest.NewRequest(http.MethodPost, integrations.ShadewrightIncidentPath, strings.NewReader(string(body)))
				req.Header.Set("Authorization", "Bearer "+shadewrightToken)
				rec := httptest.NewRecorder()
				swHandler.IncidentHandler().ServeHTTP(rec, req)
				if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
					t.Fatalf("shadewright status=%d body=%s", rec.Code, rec.Body.String())
				}
				var resp struct {
					ConclaveID string `json:"conclaveId"`
					Relic      string `json:"relic"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &resp)
				var out strings.Builder
				out.WriteString(rec.Body.String())
				if resp.ConclaveID != "" && resp.Relic != "" {
					if rel, err := st.GetConclaveRelic(ctx, resp.ConclaveID, resp.Relic); err == nil {
						out.WriteString("\n" + rel.Body)
					}
					if msgs, err := st.ListConclaveMessages(ctx, resp.ConclaveID, 0); err == nil {
						for _, m := range msgs {
							out.WriteString("\n" + m.Body)
						}
					}
				}
				return out.String()
			},
		},
		{
			name: "voice_transcript",
			run: func(t *testing.T, payload string) string {
				t.Helper()
				sttMu.Lock()
				sttPayload = payload
				sttMu.Unlock()
				out, err := voiceSvc.Transcribe(ctx, []byte("fake-audio"), "sample.wav", "en")
				if err != nil {
					t.Fatalf("voice.Transcribe: %v", err)
				}
				return out
			},
		},
	}

	encodings := secretEncodings()
	for _, sc := range secrets {
		for _, enc := range encodings {
			payload, forbids := enc.encode(sc.value)
			for _, p := range paths {
				t.Run(sc.name+"/"+enc.name+"/"+p.name, func(t *testing.T) {
					got := p.run(t, payload)
					for _, f := range forbids {
						if len(f) < redact.MinSecretLength {
							continue
						}
						if strings.Contains(got, f) {
							t.Fatalf("path %s leaked %s (%s form) in output", p.name, sc.name, enc.name)
						}
					}
				})
			}
		}
	}

	// Verify a secret registered at runtime is immediately redacted even on an already-open stream.
	t.Run("runtime_registration_updates_open_stream", func(t *testing.T) {
		hub := attach.NewHub()
		hub.Register("live-runtime", nil)
		hub.WriteOutput("live-runtime", []byte("ready\n"))
		runtimeSecret := synthVal("rot", "_live_", "secret_99887766")
		redact.Register(runtimeSecret)
		hub.WriteOutput("live-runtime", []byte("out "+runtimeSecret[:9]+"\r\n"+runtimeSecret[9:]+"\n"))
		hub.Close("live-runtime")
		hist, _, _, unsub, err := hub.Subscribe("live-runtime")
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		unsub()
		if strings.Contains(hist, runtimeSecret[:9]) || strings.Contains(hist, runtimeSecret[9:]) {
			t.Fatalf("open stream leaked runtime-registered secret: %q", hist)
		}
	})
}
