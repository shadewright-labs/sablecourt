package contextpack

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/modelfamily"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func TestPrecheckGitUsesSafeConfiguration(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "core.fsmonitor", "unused-fixture-helper"}} {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("set up Git fixture: %v (%s)", err, out)
		}
	}
	bin := t.TempDir()
	trace := filepath.Join(t.TempDir(), "calls")
	// The stand-in records the invocation without executing repository helpers.
	script := fmt.Sprintf(`#!/bin/sh
{ printf '%%s\t' "$@"; printf '\n'; } >> %q
if test -n "${DAEMON_FIXTURE_VALUE+x}${GIT_TRACE+x}"; then printf 'inherited environment\n' >> %q; fi
case " $* " in
  *' status '*) printf ' M notes\n' ;;
  *' diff '*) printf ' notes | 1 +\n' ;;
esac
`, trace, trace)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DAEMON_FIXTURE_VALUE", "present")
	t.Setenv("GIT_TRACE", "present")
	pkt := Assemble(t.Context(), nil, domain.Task{}, domain.Workload{
		Spec: domain.WorkloadSpec{Coding: &domain.CodingConfig{RepoPath: repo}},
	})
	if !pkt.PrecheckRan || !slices.Equal(pkt.ChangedPaths, []string{"notes"}) || !strings.Contains(pkt.Markdown, "notes | 1 +") {
		t.Errorf("missing Git precheck output: %+v", pkt)
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "inherited environment") {
		t.Error("Git inherited daemon environment")
	}
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		args := strings.Split(line, "\t")
		for _, flag := range []string{"--no-pager", "core.fsmonitor=false", "core.hooksPath=" + os.DevNull} {
			if line != "" && line != "inherited environment" && !slices.Contains(args, flag) {
				t.Errorf("Git call missing %q: %q", flag, line)
			}
		}
		for _, name := range []string{"status", "diff"} {
			seen[name] = seen[name] || slices.Contains(args, name)
		}
		if slices.Contains(args, "diff") && (!slices.Contains(args, "--no-ext-diff") || !slices.Contains(args, "--no-textconv")) {
			t.Error("Git diff allows external helpers")
		}
	}
	if !seen["status"] || !seen["diff"] {
		t.Error("missing Git status or diff invocation")
	}
}

func TestDecreesRequireOperatorSender(t *testing.T) {
	for _, kind := range []string{"decree", "decision"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			st := newTestStore(t)
			if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive}); err != nil {
				t.Fatal(err)
			}
			for _, role := range []string{"operator", "coder", "system"} {
				if _, err := st.AppendConclaveMessage(ctx, domain.ConclaveMessage{ConclaveID: "room", FromRole: role, Kind: kind, Body: role + "-instruction"}); err != nil {
					t.Fatal(err)
				}
			}
			pkt := Assemble(ctx, st, domain.Task{InputJSON: `{"prompt":"work","conclaveId":"room"}`}, domain.Workload{})
			if !strings.Contains(pkt.Markdown, "operator-instruction") || strings.Contains(pkt.Markdown, "coder-instruction") || strings.Contains(pkt.Markdown, "system-instruction") {
				t.Fatalf("only operator messages belong in decrees:\n%s", pkt.Markdown)
			}
		})
	}
}

func workloadWithRoleCard(t *testing.T, class domain.WorkloadClass) domain.Workload {
	t.Helper()
	var wl domain.Workload
	raw := fmt.Sprintf(`{"spec":{"class":%q,"roleCard":"Review changes. Do not authorize execution. Return findings."}}`, class)
	if err := json.Unmarshal([]byte(raw), &wl); err != nil {
		t.Fatal(err)
	}
	return wl
}

func TestRoleCardComesFirstForEveryClass(t *testing.T) {
	for _, class := range []domain.WorkloadClass{domain.ClassCodingAgent, domain.ClassLongLivedAgent, domain.ClassDeterministicController} {
		t.Run(string(class), func(t *testing.T) {
			pkt := Assemble(context.Background(), nil, domain.Task{InputJSON: `{"prompt":"task intent"}`}, workloadWithRoleCard(t, class))
			if !strings.HasPrefix(pkt.Markdown, "## Role Card\n\nReview changes. Do not authorize execution. Return findings.\n") {
				t.Fatalf("role card must come first:\n%s", pkt.Markdown)
			}
		})
	}
}

func TestPacketLimitPreservesRequiredSectionsAndNewestContext(t *testing.T) {
	for _, class := range []domain.WorkloadClass{domain.ClassCodingAgent, domain.ClassLongLivedAgent} {
		t.Run(string(class), func(t *testing.T) {
			ctx := context.Background()
			st := newTestStore(t)
			if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive}); err != nil {
				t.Fatal(err)
			}
			for i := range 5 {
				body := fmt.Sprintf("context-%d: %s", i, strings.Repeat("context ", maxPacketBytes/24))
				if _, err := st.AppendConclaveMessage(ctx, domain.ConclaveMessage{ConclaveID: "room", FromRole: "operator", Kind: "decree", Body: body}); err != nil {
					t.Fatal(err)
				}
			}
			input, err := json.Marshal(map[string]string{"prompt": "keep entire intent", "failure_handoff": "keep entire failure handoff", "conclaveId": "room"})
			if err != nil {
				t.Fatal(err)
			}
			pkt := Assemble(ctx, st, domain.Task{InputJSON: string(input)}, workloadWithRoleCard(t, class))
			for _, want := range []string{"Review changes. Do not authorize execution. Return findings.", "keep entire intent", "keep entire failure handoff", "context-4:"} {
				if !strings.Contains(pkt.Markdown, want) {
					t.Errorf("missing %q", want)
				}
			}
			if len(pkt.Markdown) > maxPacketBytes {
				t.Errorf("packet exceeds limit: %d", len(pkt.Markdown))
			}
			if strings.Contains(pkt.Markdown, "context-0:") {
				t.Error("oldest context survived truncation")
			}
		})
	}
}

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func TestScopedLearningsAndAssemble(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if _, err := PutLearning(ctx, st, "global", "no-dashes", "Never use em dashes in docs", "operator"); err != nil {
		t.Fatalf("PutLearning global: %v", err)
	}
	if _, err := PutLearning(ctx, st, "repo:sablecourt", "go-tests", "Always run go test -race", "operator"); err != nil {
		t.Fatalf("PutLearning repo: %v", err)
	}
	if _, err := PutLearning(ctx, st, "path:internal/gate/**", "ssrf-guard", "Validate upstream URLs", "operator"); err != nil {
		t.Fatalf("PutLearning path: %v", err)
	}

	matched := MatchLearnings(ctx, st, "sablecourt", []string{"internal/gate/model/proxy.go"}, 10)
	if len(matched) != 3 {
		t.Fatalf("expected 3 matched learnings, got %d: %+v", len(matched), matched)
	}
	matchedNoPath := MatchLearnings(ctx, st, "sablecourt", []string{"cmd/sablectl/main.go"}, 10)
	if len(matchedNoPath) != 2 {
		t.Fatalf("expected 2 matched learnings without gate path, got %d", len(matchedNoPath))
	}

	pkt := Assemble(ctx, st, domain.Task{
		ID:           "task_1",
		WorkloadName: "coder",
		InputJSON:    `{"prompt":"Fix model proxy"}`,
	}, domain.Workload{
		Metadata: domain.ObjectMeta{Name: "coder"},
	})
	if !strings.Contains(pkt.Markdown, "Never use em dashes in docs") {
		t.Fatalf("expected global learning in context packet, got:\n%s", pkt.Markdown)
	}
}

func TestVerdictAndFailureHandoffAndTiers(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	verdict, _ := ParseVerdict(`[{"name":"session-log","text":"Missing nil check.\nVERDICT: CHANGES_REQUESTED"}]`, nil)
	if verdict != VerdictChangesRequested {
		t.Fatalf("expected CHANGES_REQUESTED, got %q", verdict)
	}

	handoff := BuildFailureHandoff(domain.Task{
		ID:            "task_fail",
		WorkloadName:  "coder",
		State:         domain.TaskStateFailed,
		ArtifactsJSON: `[{"name":"session-log","text":"panic: nil pointer"}]`,
	}, nil, nil, "runtime_failed (exit_code=1)")
	if !strings.Contains(handoff, "panic: nil pointer") || !strings.Contains(handoff, "# Failure Handoff") {
		t.Fatalf("unexpected handoff:\n%s", handoff)
	}

	if err := SaveModelTiers(ctx, st, []TierRoute{
		{Tier: "critic", Provider: "cli:agy", Model: "claude-opus-5-5-high"},
	}); err != nil {
		t.Fatalf("SaveModelTiers: %v", err)
	}
	prov, mod := ResolveTier(ctx, st, "critic")
	if prov != "cli:agy" || mod != "claude-opus-5-5-high" {
		t.Fatalf("expected cli:agy/claude-opus-5-5-high, got %s/%s", prov, mod)
	}
}

// The shipped critic route runs a model family other than the coding route's,
// so a default install can run a model verifier after a default author.
func TestDefaultCriticFamilyDiffersFromCoding(t *testing.T) {
	families := map[string]modelfamily.Family{}
	for _, r := range DefaultModelTiers {
		f, err := modelfamily.Classify(r.Model, r.Family)
		if err != nil {
			t.Fatalf("default %s route %s/%s: %v", r.Tier, r.Provider, r.Model, err)
		}
		families[r.Tier] = f
	}
	if families["critic"] == families["coding"] {
		t.Fatalf("default critic family %q equals the coding family", families["critic"])
	}
}

func TestSaveModelTiersFamily(t *testing.T) {
	cases := []struct {
		name       string
		route      TierRoute
		wantErr    string
		wantFamily string
	}{
		{"no family", TierRoute{Tier: "critic", Provider: "cli:codex", Model: "gpt-6.2-astra"}, "", ""},
		{"agreeing family", TierRoute{Tier: "critic", Provider: "cli:codex", Model: "gpt-6.2-astra", Family: "OpenAI"}, "", "openai"},
		{"family for an unknown model", TierRoute{Tier: "fast", Provider: "local", Model: "llama-3.3-70b", Family: "meta"}, "", "meta"},
		{"unknown model without family is stored", TierRoute{Tier: "fast", Provider: "local", Model: "llama-3.3-70b"}, "", ""},
		{"family disagrees with the prefix", TierRoute{Tier: "critic", Provider: "cli:agy", Model: "claude-opus-5-5-high", Family: "google"}, "model_family_override_conflict", ""},
		{"invalid family", TierRoute{Tier: "fast", Provider: "local", Model: "llama-3", Family: "my family"}, "model_family_override_invalid", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := newTestStore(t)
			err := SaveModelTiers(ctx, st, []TierRoute{tc.route})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("SaveModelTiers = %v, want %q", err, tc.wantErr)
				}
				for _, r := range GetModelTiers(ctx, st) {
					if r.Tier == tc.route.Tier && r.Model == tc.route.Model {
						t.Fatalf("refused route was stored: %+v", r)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("SaveModelTiers: %v", err)
			}
			for _, r := range GetModelTiers(ctx, st) {
				if r.Tier == tc.route.Tier {
					if r.Model != tc.route.Model || r.Family != tc.wantFamily {
						t.Fatalf("stored %+v, want model %q family %q", r, tc.route.Model, tc.wantFamily)
					}
				}
			}
		})
	}
}

// A refused route stops the whole save: no earlier route of the batch is written.
func TestSaveModelTiersAllOrNothing(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	err := SaveModelTiers(ctx, st, []TierRoute{
		{Tier: "fast", Provider: "local", Model: "llama-3", Family: "meta"},
		{Tier: "critic", Provider: "cli:codex", Model: "gpt-6.2-astra", Family: "anthropic"},
	})
	if err == nil {
		t.Fatal("SaveModelTiers accepted a disagreeing family")
	}
	if p, m := ResolveTier(ctx, st, "fast"); p == "local" || m == "llama-3" {
		t.Fatalf("fast route written before the refusal: %s/%s", p, m)
	}
}

func TestPeerPacketBoundaries(t *testing.T) {
	ctx := context.Background()
	for _, field := range []string{"body", "name", "author"} {
		t.Run(field, func(t *testing.T) {
			st := newTestStore(t)
			if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive}); err != nil {
				t.Fatal(err)
			}
			const sample = "notes\n```\n## log sample"
			r := domain.ConclaveRelic{ConclaveID: "room", Name: field, AuthorRole: "coder", Body: "excerpt"}
			switch field {
			case "body":
				r.Body = sample
			case "name":
				r.Name = sample
			case "author":
				r.AuthorRole = sample
			}
			if err := st.PutConclaveRelic(ctx, r); err != nil {
				t.Fatal(err)
			}
			task := domain.Task{InputJSON: `{"activation":"on-mention","fromRole":"coder","messageId":1,"conclaveId":"room","prompt":"legacy peer body"}`}
			md := Assemble(ctx, st, task, domain.Workload{}).Markdown
			if !strings.Contains(md, "log sample") || strings.Contains(md, "\n## log sample") {
				t.Errorf("Relic field escaped its block: %s", md)
			}
			if strings.Contains(md, "## Intent\n\nlegacy peer body") {
				t.Error("peer body used as intent")
			}
		})
	}
}

func TestMentionPacketKeepsMessage(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive}); err != nil {
		t.Fatal(err)
	}
	id, err := st.AppendConclaveMessage(ctx, domain.ConclaveMessage{ConclaveID: "room", FromRole: "coder", ToRole: "reader", Kind: "proposal", Body: "line count was five"})
	if err != nil {
		t.Fatal(err)
	}
	task := domain.Task{InputJSON: fmt.Sprintf(`{"activation":"on-mention","fromRole":"coder","role":"reader","messageId":%d,"prompt":"Read the message.","conclaveId":"room"}`, id)}
	md := Assemble(ctx, st, task, domain.Workload{}).Markdown
	_, data, ok := strings.Cut(md, "<untrusted-peer-message>")
	body, _, closed := strings.Cut(data, "</untrusted-peer-message>")
	if !ok || !closed || !strings.Contains(body, "line count was five") {
		t.Fatalf("missing fenced mention: %s", md)
	}
}
