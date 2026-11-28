package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

// Issue 265: the Model Gate forwarded request bodies to the provider and
// relayed response bodies to the workload with only the exact matcher on the
// response. These tests build secret-shaped values and the bodies that carry
// them together, so what must be absent is known by construction, and check
// every route the gate serves.

// bodyKeys are the provider keys the daemon holds in these tests. They have no
// pattern shape, so only the exact matcher can hide them.
var bodyKeys = map[string]string{
	"openai_api_key":      "zq" + "openai_held_" + strings.Repeat("o1", 12),
	"anthropic_api_key":   "zq" + "anthropic_held_" + strings.Repeat("a2", 12),
	"gemini_api_key":      "zq" + "gemini_held_" + strings.Repeat("g3", 12),
	"local_model_api_key": "zq" + "local_held_" + strings.Repeat("l4", 12),
}

// corpusSecret is one generated secret: value is the full text a rule or the
// exact matcher covers, and hidden is the part that must not appear.
type corpusSecret struct {
	name   string
	value  string
	hidden string
}

func genAlnum(r *rand.Rand, n int) string {
	const set = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = set[r.IntN(len(set))]
	}
	return string(b)
}

func genFrom(r *rand.Rand, set string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = set[r.IntN(len(set))]
	}
	return string(b)
}

// corpusSecrets returns the generated secrets for one seed. Prefixes are split
// so no key-shaped literal sits in the source.
func corpusSecrets(seed uint64) []corpusSecret {
	r := rand.New(rand.NewPCG(seed, seed^0x265))
	tok := func(name, prefix string, body string) corpusSecret {
		v := prefix + body
		return corpusSecret{name: name, value: v, hidden: v}
	}
	pemBody := genFrom(r, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/", 48)
	kvValue := "v4l" + genAlnum(r, 18)
	textValue := "pw" + genAlnum(r, 14)
	return []corpusSecret{
		tok("github_classic", "gh"+"p_", genAlnum(r, 36)),
		tok("github_fine_grained", "github"+"_pat_", genAlnum(r, 40)),
		tok("anthropic", "sk-"+"ant-", "api03-"+genAlnum(r, 40)),
		tok("openai_project", "sk-"+"proj-", genAlnum(r, 40)),
		tok("google", "AI"+"za", genAlnum(r, 35)),
		tok("aws", "AK"+"IA", genFrom(r, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", 16)),
		tok("other_task_lease", "sc_"+"task_", genFrom(r, "0123456789abcdef", 64)),
		{name: "private_key", value: "-----BEGIN " + "PRIVATE KEY-----\n" + pemBody + "\n-----END " + "PRIVATE KEY-----", hidden: pemBody},
		{name: "password_in_text", value: "password=" + textValue, hidden: textValue},
		{name: "kv_member", value: kvValue, hidden: kvValue}, // placed as "api_key":value by the placements
		{name: "held_provider_key", value: bodyKeys["openai_api_key"], hidden: bodyKeys["openai_api_key"]},
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// escapeFirstLetter writes s as a JSON string literal whose first letter is a
// \u escape, a spelling a provider decodes but a raw-text rule does not see.
func escapeFirstLetter(s string) string {
	lit := jsonString(s)
	for i := 1; i < len(lit)-1; i++ {
		c := lit[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			return lit[:i] + fmt.Sprintf(`\u%04x`, c) + lit[i+1:]
		}
	}
	return lit
}

// placement puts a secret into a request body as a JSON member list (without
// braces) and a content string literal.
type placement struct {
	name    string
	content func(s, n corpusSecret) string // raw JSON string literal
	extra   func(s, n corpusSecret) string // raw JSON members, may be empty
	kvOnly  bool                           // only for the kv_member secret
	noKV    bool                           // not for the kv_member secret
	request bool                           // only checked on requests
}

var bodyPlacements = []placement{
	{name: "in_text", noKV: true, content: func(s, _ corpusSecret) string {
		return jsonString("keepme before " + s.value + " keepme after")
	}},
	{name: "alone", noKV: true, content: func(s, _ corpusSecret) string { return jsonString(s.value) }},
	{name: "neighbour_comma", noKV: true, content: func(s, n corpusSecret) string {
		return jsonString("keepme " + s.value + "," + n.value + " keepme")
	}},
	{name: "neighbour_space", noKV: true, content: func(s, n corpusSecret) string {
		return jsonString(n.value + " " + s.value)
	}},
	{name: "member_name", noKV: true, content: func(_, _ corpusSecret) string { return jsonString("keepme") },
		extra: func(s, _ corpusSecret) string { return jsonString(s.value) + `:"keepme"` }},
	{name: "nested_array", noKV: true, content: func(_, _ corpusSecret) string { return jsonString("keepme") },
		extra: func(s, _ corpusSecret) string { return `"meta":{"list":["keepme",[` + jsonString(s.value) + `]]}` }},
	{name: "escaped_letter", noKV: true, request: true, content: func(s, _ corpusSecret) string { return escapeFirstLetter(s.value) }},
	{name: "kv_member", kvOnly: true, content: func(_, _ corpusSecret) string { return jsonString("keepme") },
		extra: func(s, _ corpusSecret) string { return `"api_key":` + jsonString(s.value) }},
	{name: "kv_member_nested", kvOnly: true, content: func(_, _ corpusSecret) string { return jsonString("keepme") },
		extra: func(s, _ corpusSecret) string { return `"meta":{"client_secret":` + jsonString(s.value) + `}` }},
	{name: "kv_member_escaped", kvOnly: true, request: true, content: func(_, _ corpusSecret) string { return jsonString("keepme") },
		extra: func(s, _ corpusSecret) string { return `"password":` + jsonString(s.value) }},
}

type bodyCase struct {
	name    string
	content string
	extra   string
	hidden  []string
}

func bodyCases(seed uint64, request bool) []bodyCase {
	secrets := corpusSecrets(seed)
	var out []bodyCase
	for i, s := range secrets {
		n := secrets[(i+1)%len(secrets)]
		if n.name == "kv_member" {
			n = secrets[(i+2)%len(secrets)]
		}
		for _, p := range bodyPlacements {
			if p.request && !request {
				continue
			}
			isKV := s.name == "kv_member"
			if (p.kvOnly && !isKV) || (p.noKV && isKV) {
				continue
			}
			c := bodyCase{name: s.name + "/" + p.name, content: p.content(s, n), hidden: []string{s.hidden}}
			if p.extra != nil {
				c.extra = p.extra(s, n)
			}
			if strings.HasPrefix(p.name, "neighbour") {
				c.hidden = append(c.hidden, n.hidden)
			}
			out = append(out, c)
		}
	}
	return out
}

// requestTemplate builds a request body of the route's own shape.
func requestTemplate(route, content, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	switch {
	case strings.HasPrefix(route, "/anthropic/"):
		return `{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":` + content + `}]` + extra + `}`
	case strings.HasPrefix(route, "/gemini/"):
		return `{"contents":[{"role":"user","parts":[{"text":` + content + `}]}]` + extra + `}`
	case strings.HasSuffix(route, "/v1/responses"):
		return `{"model":"gpt-test","input":` + content + extra + `}`
	default:
		return `{"model":"gpt-test","messages":[{"role":"user","content":` + content + `}]` + extra + `}`
	}
}

// gateRoutes returns one concrete request path per route the gate serves.
func gateRoutes() []string {
	var out []string
	for _, r := range Routes() {
		out = append(out, strings.Replace(r, modelPlaceholder, "gemini-test", 1))
	}
	return out
}

func bodyGate(t *testing.T, rt roundTripFunc) (*Gate, *sqlite.SQLiteStore, string) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "bodies.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_ = st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "agent-1", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent})
	if err := st.CreateTask(ctx, domain.Task{ID: "t-bodies", WorkloadName: "agent-1", State: domain.TaskStateWorking, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	tok, err := MintTaskToken(ctx, st, "t-bodies", "agent-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bundle := &config.Bundle{
		Budgets: map[string]domain.Budget{"b1": {Metadata: domain.ObjectMeta{Name: "b1"}}},
		Workloads: map[string]domain.Workload{"agent-1": {
			Metadata: domain.ObjectMeta{Name: "agent-1", Zone: domain.ZoneRuntime},
			Spec: domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Grants: domain.GrantSet{
				ModelGate: domain.ModelGateGrant{Allowed: true, BudgetRef: "b1"},
			}},
		}},
	}
	gate := New(Options{Store: st, Secrets: secret.NewMemoryStore(bodyKeys), Bundle: bundle, Client: &http.Client{Transport: rt}})
	return gate, st, tok
}

func callGate(gate *Gate, tok, route, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gate.Handler().ServeHTTP(rec, req)
	return rec
}

func jsonOK(status int) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"model":"m","usage":{"prompt_tokens":1,"completion_tokens":1}}`))}
}

// A secret in a request body never reaches the upstream, on every route, in
// any spelling the provider would decode, and the body stays valid JSON.
func TestModelGateRequestBodyCorpus(t *testing.T) {
	for _, seed := range []uint64{1, 2} {
		cases := bodyCases(seed, true)
		for _, route := range gateRoutes() {
			t.Run(fmt.Sprintf("seed%d%s", seed, route), func(t *testing.T) {
				t.Parallel()
				var mu sync.Mutex
				var got []byte
				gate, _, tok := bodyGate(t, func(r *http.Request) (*http.Response, error) {
					b, _ := io.ReadAll(r.Body)
					mu.Lock()
					got = b
					mu.Unlock()
					return jsonOK(http.StatusOK), nil
				})
				for _, c := range cases {
					mu.Lock()
					got = nil
					mu.Unlock()
					body := requestTemplate(route, c.content, c.extra)
					if !json.Valid([]byte(body)) {
						t.Fatalf("%s: test body is not JSON: %s", c.name, body)
					}
					rec := callGate(gate, tok, route, body)
					if rec.Code != http.StatusOK {
						t.Errorf("%s: status %d: %s", c.name, rec.Code, rec.Body.String())
						continue
					}
					mu.Lock()
					sent := string(got)
					mu.Unlock()
					for _, h := range c.hidden {
						if strings.Contains(sent, h) {
							t.Errorf("%s: upstream received the secret:\n%s", c.name, sent)
						}
					}
					var decoded any
					if err := json.Unmarshal([]byte(sent), &decoded); err != nil {
						t.Errorf("%s: upstream body is not JSON: %v\n%s", c.name, err, sent)
						continue
					}
					// A decoded view catches escaped spellings of the secret.
					flat, _ := json.Marshal(decoded)
					for _, h := range c.hidden {
						if strings.Contains(string(flat), h) || strings.Contains(fmt.Sprint(decoded), h) {
							t.Errorf("%s: upstream body decodes to the secret:\n%s", c.name, sent)
						}
					}
					if strings.Contains(body, "keepme") && !strings.Contains(sent, "keepme") {
						t.Errorf("%s: clean text lost: %s", c.name, sent)
					}
				}
			})
		}
	}
}

// responseBody builds an upstream response that carries the case, as one JSON
// object or as server-sent events, with usage the parsers read.
func responseBody(sse bool, c bodyCase) string {
	extra := ""
	if c.extra != "" {
		extra = "," + c.extra
	}
	if !sse {
		return `{"id":"keepme","model":"m","text":` + c.content + extra + `,"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	}
	return "data: {\"id\":\"keepme\",\"text\":" + c.content + "}\n\n" +
		"data: {\"x\":1" + extra + "}\n\n" +
		"data: [DONE]\n\n"
}

// A secret in an upstream response body never reaches the workload, on every
// route, for JSON and streamed replies split at any read size, and for error
// bodies.
func TestModelGateResponseBodyCorpus(t *testing.T) {
	cases := bodyCases(3, false)
	type variant struct {
		sse    bool
		status int
		chunk  int
	}
	var variants []variant
	for _, sse := range []bool{false, true} {
		for _, chunk := range []int{1, 3, 7, 4096} {
			variants = append(variants, variant{sse, http.StatusOK, chunk})
		}
		variants = append(variants, variant{sse, http.StatusUnauthorized, 5})
	}
	for _, route := range gateRoutes() {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			var cur struct {
				sync.Mutex
				body, ctype   string
				status, chunk int
			}
			gate, _, tok := bodyGate(t, func(r *http.Request) (*http.Response, error) {
				cur.Lock()
				defer cur.Unlock()
				return &http.Response{
					StatusCode: cur.status,
					Header:     http.Header{"Content-Type": []string{cur.ctype}},
					Body:       io.NopCloser(&trickleReader{s: cur.body, n: cur.chunk}),
				}, nil
			})
			// Each case gets three of the variants, in turn, so every route
			// sees every variant with every secret shape over the cases
			// without running the full product (the race build is slow).
			for i, c := range cases {
				for _, v := range []variant{variants[i%len(variants)], variants[(i+3)%len(variants)], variants[(i+7)%len(variants)]} {
					cur.Lock()
					cur.body = responseBody(v.sse, c)
					cur.ctype = "application/json"
					if v.sse {
						cur.ctype = "text/event-stream"
					}
					cur.status, cur.chunk = v.status, v.chunk
					cur.Unlock()
					rec := callGate(gate, tok, route, requestTemplate(route, `"hi"`, ""))
					name := fmt.Sprintf("%s sse=%v status=%d chunk=%d", c.name, v.sse, v.status, v.chunk)
					if rec.Code != v.status {
						t.Errorf("%s: status %d: %s", name, rec.Code, rec.Body.String())
						continue
					}
					out := rec.Body.String()
					for _, h := range c.hidden {
						if strings.Contains(out, h) {
							t.Errorf("%s: workload received the secret:\n%s", name, out)
						}
					}
					if !strings.Contains(out, "keepme") {
						t.Errorf("%s: clean text lost: %q", name, out)
					}
				}
			}
		})
	}
}

// Model names from the request and the response reach the audit log and the
// budget ledger; a secret in either is redacted there.
func TestModelGateAuditCarriesNoBodySecrets(t *testing.T) {
	secrets := corpusSecrets(4)
	ghp, ant := secrets[0].value, secrets[2].value
	gate, st, tok := bodyGate(t, func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"model":"m ` + ant + `","usage":{"prompt_tokens":1,"completion_tokens":1}}`))}, nil
	})
	for _, model := range []string{"gpt " + ghp, ""} {
		body := `{"model":` + jsonString(model) + `,"messages":[]}`
		if rec := callGate(gate, tok, "/openai/v1/chat/completions", body); rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}
	recs, err := st.ListAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		t.Fatal("no audit rows")
	}
	for _, rec := range recs {
		row, _ := json.Marshal(rec)
		if bytes.Contains(row, []byte(ghp)) || bytes.Contains(row, []byte(ant)) {
			t.Fatalf("audit row carries a body secret: %s", row)
		}
	}
}
