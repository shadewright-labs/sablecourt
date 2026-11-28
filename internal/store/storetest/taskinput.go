package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// TaskInputSecret is one secret of the task input corpus. Body is the part
// that must never be stored or delivered; Text is how it appears in input
// (Body plus whatever label the shape needs, such as a bearer header).
type TaskInputSecret struct {
	Name string
	Body string
	Text string
	// Key, when set, carries the secret as the value of a JSON field with
	// this name instead of inside free text.
	Key string
}

// TaskInputCase is one generated input and the secret it carries.
type TaskInputCase struct {
	Name   string
	Input  string
	Secret TaskInputSecret
}

func randomHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)[:n]
}

// alnum maps random hex to a run of letters and digits of length n, so a
// body holds no fixed literal a test could copy from a rule.
func alnum(n int, upper bool) string {
	h := randomHex(n)
	if !upper {
		return h
	}
	return strings.ToUpper(h)
}

// TaskInputSecrets returns one secret per pattern rule in internal/redact and
// one value the daemon holds (registered in the default registry here). The
// bodies are random and built from split literals, so the answer is known by
// construction and no key-shaped literal is in the source.
func TaskInputSecrets() []TaskInputSecret {
	held := "held-" + randomHex(24)
	redact.Register(held)
	pemBody := "MIIB" + alnum(40, false)
	bearer := "bearer" + alnum(20, false)
	return []TaskInputSecret{
		{Name: "private_key", Body: pemBody, Text: "-----BEGIN " + "RSA PRIVATE KEY-----\n" + pemBody + "\n-----END RSA PRIVATE KEY-----"},
		{Name: "task_token", Body: randomHex(32), Text: ""},
		{Name: "operator_token", Body: randomHex(64), Text: ""},
		{Name: "anthropic_key", Body: "sk-" + "ant-" + "api03-" + alnum(24, false)},
		{Name: "openai_key", Body: "sk-" + "proj-" + alnum(24, false)},
		{Name: "github_token", Body: "gh" + "p_" + alnum(36, false)},
		{Name: "github_pat", Body: "github" + "_pat_" + alnum(30, false)},
		{Name: "google_api_key", Body: "AI" + "za" + alnum(35, false)},
		{Name: "aws_access_key", Body: "AK" + "IA" + alnum(16, true)},
		{Name: "bearer_header", Body: bearer, Text: "Authorization: Bearer " + bearer},
		{Name: "secret_assignment", Body: "pw" + alnum(16, false), Text: ""},
		{Name: "json_password_field", Body: "pw" + alnum(16, false), Key: "password"},
		{Name: "json_api_key_field", Body: "ak" + alnum(16, false), Key: "api_key"},
		{Name: "daemon_held", Body: held},
	}
}

func (s TaskInputSecret) text() string {
	switch s.Name {
	case "task_token":
		return "sc_" + "task_" + s.Body
	case "operator_token":
		return "op" + "_" + s.Body
	case "secret_assignment":
		return "api_key=" + s.Body
	}
	if s.Text != "" {
		return s.Text
	}
	return s.Body
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// TaskInputCorpus places every secret of TaskInputSecrets in task input in
// each shape a submitter can send: the prompt field, a nested field, an
// array item, a field escaped with \u sequences, a duplicate JSON name
// (first and last), and plain text that is not JSON.
func TaskInputCorpus() []TaskInputCase {
	var out []TaskInputCase
	for _, s := range TaskInputSecrets() {
		text := s.text()
		add := func(shape, input string) {
			out = append(out, TaskInputCase{Name: s.Name + "/" + shape, Input: input, Secret: s})
		}
		if s.Key != "" {
			add("field", mustJSON(map[string]any{"prompt": "deploy", s.Key: s.Body}))
			add("nested field", mustJSON(map[string]any{"prompt": "deploy", "config": map[string]any{s.Key: s.Body}}))
			add("text", `{"prompt":"use {\"`+s.Key+`\":\"`+s.Body+`\"} now"}`)
			add("duplicate first", `{"`+s.Key+`":"`+s.Body+`","`+s.Key+`":"x"}`)
			continue
		}
		add("prompt", mustJSON(map[string]any{"prompt": "fix the build, the key is " + text + " thanks", "issue": 7}))
		add("nested", mustJSON(map[string]any{"workflow": "wf", "input": map[string]any{"notes": []any{"first", "creds " + text}}}))
		add("escaped", `{"prompt":"`+unicodeEscape(text)+`"}`)
		add("duplicate first", mustJSON(map[string]any{"prompt": text})[:len(mustJSON(map[string]any{"prompt": text}))-1]+`,"prompt":"clean"}`)
		add("duplicate last", `{"prompt":"clean",`+mustJSON(map[string]any{"prompt": text})[1:])
		add("plain text", "please rotate "+text+" today")
	}
	return out
}

// unicodeEscape writes every rune of s (all ASCII in the corpus) as a JSON
// \u escape.
func unicodeEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	return b.String()
}

// TaskInputKeepCorpus is input that carries no secret. Storing it must keep
// it byte for byte, so idempotent replay and digests do not change.
func TaskInputKeepCorpus() []string {
	return []string{
		`{}`,
		`{"prompt":"fix the max_tokens setting","issue":7}`,
		`{"workflow":"wf","step_index":1,"prompt":"a <b> & c","big":12345678901234567890}`,
		"{\n  \"prompt\": \"keep my spacing\",\n  \"z\": 1, \"a\": 2\n}",
		`{"conclaveId":"ccl_1","role":"claude","checkoutMode":"live","context_packet":"Review."}`,
		"plain words without secrets",
		`"a bare JSON string"`,
	}
}

// AssertNoSecret fails when got shows the case's secret body, in its raw
// text or, when got is JSON, in any decoded string.
func AssertNoSecret(t *testing.T, where string, c TaskInputCase, got string) {
	t.Helper()
	if strings.Contains(got, c.Secret.Body) || strings.Contains(decodedJSON(got), c.Secret.Body) {
		t.Errorf("%s: %s secret reached %q", c.Name, where, got)
	}
}

// decodedJSON returns the decoded strings of a JSON text, including object
// names and values hidden behind \u escapes, or "" when s is not JSON.
func decodedJSON(s string) string {
	dec := json.NewDecoder(strings.NewReader(s))
	var b strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return b.String()
		}
		if str, ok := tok.(string); ok {
			b.WriteString(str)
			b.WriteByte(0)
		}
	}
}

// CheckTaskInputRedacted submits every corpus input to st as task input and
// as a task message, and fails if the stored row or message shows a secret,
// or if input without secrets is not stored byte for byte. Both backends run
// it, so the store write boundary is one rule.
func CheckTaskInputRedacted(t *testing.T, st store.Store) {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, ManifestDigest: "m", AgentCardJSON: "{}"}); err != nil {
		t.Fatalf("UpsertWorkload: %v", err)
	}
	now := time.Now().UTC()
	create := func(id, input string) (domain.Task, []domain.TaskMessage) {
		t.Helper()
		if err := st.CreateTask(ctx, domain.Task{ID: id, IdempotencyKey: id, WorkloadName: "worker", State: domain.TaskStateSubmitted, InputJSON: input, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		if err := st.AppendTaskMessage(ctx, domain.TaskMessage{TaskID: id, Role: "user", Content: input, CreatedAt: now}); err != nil {
			t.Fatalf("AppendTaskMessage %s: %v", id, err)
		}
		task, err := st.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("GetTask %s: %v", id, err)
		}
		msgs, err := st.ListTaskMessages(ctx, id)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("ListTaskMessages %s = %d, %v", id, len(msgs), err)
		}
		return task, msgs
	}
	for i, c := range TaskInputCorpus() {
		task, msgs := create("hide-"+randomHex(8)+"-"+string(rune('a'+i%26)), c.Input)
		AssertNoSecret(t, "stored input_json", c, task.InputJSON)
		AssertNoSecret(t, "stored task message", c, msgs[0].Content)
		if json.Valid([]byte(c.Input)) && !json.Valid([]byte(task.InputJSON)) {
			t.Errorf("%s: stored input is no longer JSON: %q", c.Name, task.InputJSON)
		}
	}
	for i, in := range TaskInputKeepCorpus() {
		task, msgs := create("keep-"+randomHex(8)+"-"+string(rune('a'+i)), in)
		if task.InputJSON != in || msgs[0].Content != in {
			t.Errorf("clean input changed on store: %q -> %q / %q", in, task.InputJSON, msgs[0].Content)
		}
	}
}
