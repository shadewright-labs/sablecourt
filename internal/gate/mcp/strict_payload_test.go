package mcpgate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// kv is one key and its raw JSON value, kept in wire order.
type kv struct{ k, v string }

func object(pairs ...kv) string {
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = fmt.Sprintf("%q:%s", p.k, p.v)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// ambiguousPayloads builds, from one valid payload, the payloads whose meaning
// differs between a JSON map and Go's struct decoder: an exact duplicate, a
// case variant, the same variants reordered, a duplicate ending in null, a key
// spelled only in another case, and an unknown field.
func ambiguousPayloads(valid []kv, key, other string) map[string]string {
	with := func(extra ...kv) []kv {
		out := append([]kv(nil), valid...)
		return append(out, extra...)
	}
	idx := -1
	for i, p := range valid {
		if p.k == key {
			idx = i
		}
	}
	upper := strings.ToUpper(key[:1]) + key[1:]
	reordered := append([]kv(nil), valid[:idx]...)
	reordered = append(reordered, kv{upper, other}, valid[idx])
	reordered = append(reordered, valid[idx+1:]...)
	onlyCase := append([]kv(nil), valid...)
	onlyCase[idx] = kv{upper, valid[idx].v}
	return map[string]string{
		"duplicate key":           object(with(kv{key, other})...),
		"case colliding key":      object(with(kv{upper, other})...),
		"reordered case variants": object(reordered...),
		"trailing null duplicate": object(with(kv{key, "null"})...),
		"key only in other case":  object(onlyCase...),
		"unknown field":           object(with(kv{"extra", `"x"`})...),
		"all upper key":           object(with(kv{strings.ToUpper(key), other})...),
	}
}

// H1: every ambiguous payload is refused before an approval exists.
func TestAmbiguousPayloadsAreRefusedBeforeApproval(t *testing.T) {
	actions := []struct {
		name  string
		valid []kv
		key   string
		other string
	}{
		{"git.open_pr", []kv{{"owner", `"acme"`}, {"repo", `"widgets"`}, {"head", `"sablecourt/fix-1"`}, {"base", `"main"`}, {"title", `"Fix"`}}, "head", `"sablecourt/evil"`},
		{"git.open_pr", []kv{{"owner", `"acme"`}, {"repo", `"widgets"`}, {"head", `"sablecourt/fix-1"`}, {"base", `"main"`}, {"title", `"Fix"`}}, "base", `"release"`},
		{"git.create_branch", []kv{{"branch", `"sablecourt/fix-1"`}, {"base", `"main"`}}, "branch", `"sablecourt/evil"`},
		{"git.create_branch", []kv{{"branch", `"sablecourt/fix-1"`}, {"base", `"main"`}}, "base", `"release"`},
		{"sandbox.propose_promotion", []kv{{"workload", `"exp-sandbox"`}, {"branch", `"sablecourt/promote"`}, {"reason", `"ready"`}}, "workload", `"other"`},
	}
	for _, a := range actions {
		for name, payload := range ambiguousPayloads(a.valid, a.key, a.other) {
			t.Run(a.name+"/"+a.key+"/"+name, func(t *testing.T) {
				tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectApprove})
				rec := tg.post("ops-agent", a.name, `{"payload":`+payload+`}`)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("want 400 for %s, got %d: %s", payload, rec.Code, rec.Body.String())
				}
				if n := len(tg.approvals(t)); n != 0 {
					t.Fatalf("an approval was created for %s (%d rows)", payload, n)
				}
			})
		}
	}
}

// The request envelope gets the same strict reading as the payload.
func TestAmbiguousEnvelopeIsRefused(t *testing.T) {
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectApprove})
	good := `{"branch":"sablecourt/fix-1","base":"main"}`
	for _, body := range []string{
		`{"payload":` + good + `,"payload":{"branch":"sablecourt/evil","base":"main"}}`,
		`{"Payload":` + good + `}`,
		`{"payload":` + good + `,"approvalId":"a","approvalID":"b"}`,
	} {
		if rec := tg.post("ops-agent", "git.create_branch", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("want 400 for %s, got %d: %s", body, rec.Code, rec.Body.String())
		}
	}
	if n := len(tg.approvals(t)); n != 0 {
		t.Fatalf("an approval was created (%d rows)", n)
	}
}

// H1: for accepted payloads, the dialog input, the digest input and the request
// the executor sends are one value in one encoding, whatever the key order.
func TestApprovedValueIsTheExecutedValue(t *testing.T) {
	rng := rand.New(rand.NewSource(111))
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectApprove})
	titles := []string{"Fix", "Fix <b>&amp;</b>", "Zażółć", "line\nbreak", `quote " and \ slash`}
	for i := 0; i < 40; i++ {
		pairs := []kv{
			{"owner", fmt.Sprintf("%q", []string{"acme", "ACME", "Acme"}[rng.Intn(3)])},
			{"repo", fmt.Sprintf("%q", []string{"widgets", "Widgets"}[rng.Intn(2)])},
			{"head", fmt.Sprintf("%q", fmt.Sprintf("sablecourt/fix-%d", rng.Intn(1000)))},
			{"base", fmt.Sprintf("%q", []string{"main", "release/1.0", "dev"}[rng.Intn(3)])},
			{"title", mustJSON(t, titles[rng.Intn(len(titles))])},
		}
		if rng.Intn(2) == 0 {
			pairs = append(pairs, kv{"body", mustJSON(t, fmt.Sprintf("body %d <script>", i))})
		}
		rng.Shuffle(len(pairs), func(a, b int) { pairs[a], pairs[b] = pairs[b], pairs[a] })
		requested := object(pairs...)
		rec := tg.post("ops-agent", "git.open_pr", `{"payload":`+requested+`}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("request %s: want 202, got %d: %s", requested, rec.Code, rec.Body.String())
		}
		var pending struct {
			ApprovalID    string `json:"approval_id"`
			PayloadSHA256 string `json:"payload_sha256"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil {
			t.Fatal(err)
		}
		if _, err := tg.queue.Approve(t.Context(), pending.ApprovalID, pending.PayloadSHA256, "operator"); err != nil {
			t.Fatalf("Approve: %v", err)
		}
		shown := tg.approver.lastRequest()

		// The agent executes with the keys in another order.
		rng.Shuffle(len(pairs), func(a, b int) { pairs[a], pairs[b] = pairs[b], pairs[a] })
		executedBody := object(pairs...)
		rec = tg.post("ops-agent", "git.open_pr", `{"approvalId":"`+pending.ApprovalID+`","payload":`+executedBody+`}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("execute %s: want 200, got %d: %s", executedBody, rec.Code, rec.Body.String())
		}
		calls := tg.pr.calls()
		executed := calls[len(calls)-1]
		encoded, err := json.Marshal(executed)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != shown.Summary {
			t.Fatalf("dialog input differs from the executed request:\n shown    %s\n executed %s", shown.Summary, encoded)
		}
		sum := sha256.Sum256(encoded)
		if hex.EncodeToString(sum[:]) != shown.PayloadSHA256 || shown.PayloadSHA256 != pending.PayloadSHA256 {
			t.Fatalf("digest is not the hash of the executed request")
		}
		var target map[string]string
		if err := json.Unmarshal([]byte(shown.Summary), &target); err != nil {
			t.Fatal(err)
		}
		if target["owner"] != executed.Owner || target["repo"] != executed.Repo || target["head"] != executed.Head || target["base"] != executed.Base {
			t.Fatalf("shown target %v differs from executed %+v", target, executed)
		}
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Low: base and head are Git ref names checked with a strict pattern; the value
// checked is the value sent, and nothing reaches GitHub when one is malformed.
func TestOpenPRRefsAreStrict(t *testing.T) {
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectAllow})
	cases := []struct{ name, head, base string }{
		{"head with trailing space", "sablecourt/x ", "main"},
		{"head with leading space", " sablecourt/x", "main"},
		{"head with control char", "sablecourt/x\u0007", "main"},
		{"head ending in .lock", "sablecourt/x.lock", "main"},
		{"head with @{", "sablecourt/x@{1}", "main"},
		{"head ending in slash", "sablecourt/x/", "main"},
		{"head with double slash", "sablecourt//x", "main"},
		{"base with space", "sablecourt/x", "ma in"},
		{"base with trailing newline", "sablecourt/x", "main\n"},
		{"base starting with dash", "sablecourt/x", "-main"},
		{"base with dot dot", "sablecourt/x", "a..b"},
		{"base with colon", "sablecourt/x", "main:evil"},
		{"base ending in dot", "sablecourt/x", "main."},
		{"base component starting with dot", "sablecourt/x", "release/.hidden"},
		{"empty base", "sablecourt/x", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(tg.pr.calls())
			body, _ := json.Marshal(map[string]any{"payload": map[string]string{
				"owner": "acme", "repo": "widgets", "head": tc.head, "base": tc.base, "title": "Fix",
			}})
			rec := tg.post("ops-agent", "git.open_pr", string(body))
			if rec.Code == http.StatusOK || len(tg.pr.calls()) != before {
				t.Fatalf("want refusal before GitHub, got %d (sent=%v): %s", rec.Code, len(tg.pr.calls()) != before, rec.Body.String())
			}
		})
	}
	rec := tg.post("ops-agent", "git.open_pr", `{"payload":{"owner":"acme","repo":"widgets","head":"sablecourt/x","title":"Fix"}}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("omitted base: want refusal, got 200")
	}
}

// Low: git.create_branch needs an explicit, valid base instead of the
// checkout's HEAD at execution time.
func TestCreateBranchRequiresAValidBase(t *testing.T) {
	tg := setupTypedGate(t, typedGateOptions{effect: domain.EffectAllow})
	for _, body := range []string{
		`{"payload":{"branch":"sablecourt/no-base"}}`,
		`{"payload":{"branch":"sablecourt/no-base","base":""}}`,
		`{"payload":{"branch":"sablecourt/no-base","base":" main"}}`,
		`{"payload":{"branch":"sablecourt/no-base","base":"HEAD@{1}"}}`,
	} {
		if rec := tg.post("ops-agent", "git.create_branch", body); rec.Code == http.StatusOK {
			t.Fatalf("%s: want refusal, got 200", body)
		}
	}
	if out, err := exec.Command("git", "-C", tg.repo, "rev-parse", "--verify", "--quiet", "refs/heads/sablecourt/no-base").CombinedOutput(); err == nil {
		t.Fatalf("branch was created: %s", out)
	}
	base := strings.TrimSpace(gitOut(t, tg.repo, "symbolic-ref", "--short", "HEAD"))
	if rec := tg.post("ops-agent", "git.create_branch", `{"payload":{"branch":"sablecourt/with-base","base":"`+base+`"}}`); rec.Code != http.StatusOK {
		t.Fatalf("valid base: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func gitOut(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
	return string(out)
}
