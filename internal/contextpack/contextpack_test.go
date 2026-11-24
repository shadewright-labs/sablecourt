package contextpack

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

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
			for i := 0; i < 5; i++ {
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
