package contextpack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const (
	acceptedAdvice = "Accepted advice for local agents"
	legacyAdvice   = "Legacy advice from Court Memory"
)

// seedAcceptedLearning proposes and accepts a learning backed by task evidence.
func seedAcceptedLearning(t *testing.T, st store.Store, id, statement string, scope domain.LearningScope, acceptedAt time.Time) {
	t.Helper()
	ctx := t.Context()
	ls, err := store.Learnings(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetTask(ctx, "evidence-task"); err != nil {
		if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "reviewer", AgentCardJSON: "{}"}); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal([]map[string]string{{"name": "record", "text": "recorded observation"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.CreateTask(ctx, domain.Task{ID: "evidence-task", IdempotencyKey: "evidence-task", WorkloadName: "reviewer", State: domain.TaskStateCompleted, ArtifactsJSON: string(raw)}); err != nil {
			t.Fatal(err)
		}
	}
	l, err := ls.ProposeLearning(ctx, domain.Learning{
		ID: id, Statement: statement, Scope: scope, SourceKind: domain.LearningSourceTask,
		SourceID: "evidence-task", EvidenceRef: "artifact:record", Author: "operator-1", CreatedAt: acceptedAt,
	})
	if err != nil {
		t.Fatalf("propose %s: %v", id, err)
	}
	if _, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator-1"), l.ID, l.VersionDigest, acceptedAt); err != nil {
		t.Fatalf("accept %s: %v", id, err)
	}
}

func seedLegacyLearning(t *testing.T, st store.Store) {
	t.Helper()
	if err := st.PutMemory(t.Context(), domain.MemoryEntry{
		Namespace: LearningNamespaceGlobal, Key: "legacy", Value: legacyAdvice, Author: "operator", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func agentWorkload(class domain.WorkloadClass, coding *domain.CodingConfig, grant domain.ModelGateGrant) domain.Workload {
	return domain.Workload{
		Metadata: domain.ObjectMeta{Name: "agent"},
		Spec:     domain.WorkloadSpec{Class: class, Coding: coding, Grants: domain.GrantSet{ModelGate: grant}},
	}
}

// TestLearningDestination checks every case of the 2026-11-27 decision on
// issue 99: no packet whose destination is remote or unknown carries a learning.
func TestLearningDestination(t *testing.T) {
	local := domain.ModelGateGrant{Allowed: true, Providers: []string{"local"}}
	docker := Destination{DockerAgents: true}
	coding := &domain.CodingConfig{Harness: "claude"}
	for _, tc := range []struct {
		name string
		dest Destination
		wl   domain.Workload
		want bool
	}{
		{"docker, no gateway, long-lived agent, local only", docker, agentWorkload(domain.ClassLongLivedAgent, nil, local), true},
		{"coding agent with local grant", docker, agentWorkload(domain.ClassCodingAgent, coding, local), false},
		{"coding agent class without coding block", docker, agentWorkload(domain.ClassCodingAgent, nil, local), false},
		{"long-lived agent with coding block", docker, agentWorkload(domain.ClassLongLivedAgent, coding, local), false},
		{"kubernetes backend or no runtime", Destination{}, agentWorkload(domain.ClassLongLivedAgent, nil, local), false},
		{"model gateway set", Destination{DockerAgents: true, ModelGateway: true}, agentWorkload(domain.ClassLongLivedAgent, nil, local), false},
		{"empty provider list allows every provider", docker, agentWorkload(domain.ClassLongLivedAgent, nil, domain.ModelGateGrant{Allowed: true}), false},
		{"local next to a remote provider", docker, agentWorkload(domain.ClassLongLivedAgent, nil, domain.ModelGateGrant{Allowed: true, Providers: []string{"local", "anthropic"}}), false},
		{"remote provider first", docker, agentWorkload(domain.ClassLongLivedAgent, nil, domain.ModelGateGrant{Allowed: true, Providers: []string{"openai", "local"}}), false},
		{"local listed twice", docker, agentWorkload(domain.ClassLongLivedAgent, nil, domain.ModelGateGrant{Allowed: true, Providers: []string{"local", "local"}}), false},
		{"only openai", docker, agentWorkload(domain.ClassLongLivedAgent, nil, domain.ModelGateGrant{Allowed: true, Providers: []string{"openai"}}), false},
		{"only anthropic", docker, agentWorkload(domain.ClassLongLivedAgent, nil, domain.ModelGateGrant{Allowed: true, Providers: []string{"anthropic"}}), false},
		{"only gemini", docker, agentWorkload(domain.ClassLongLivedAgent, nil, domain.ModelGateGrant{Allowed: true, Providers: []string{"gemini"}}), false},
		{"provider name not exactly local", docker, agentWorkload(domain.ClassLongLivedAgent, nil, domain.ModelGateGrant{Allowed: true, Providers: []string{"Local"}}), false},
		{"provider name with spaces", docker, agentWorkload(domain.ClassLongLivedAgent, nil, domain.ModelGateGrant{Allowed: true, Providers: []string{" local"}}), false},
		{"grant not allowed", docker, agentWorkload(domain.ClassLongLivedAgent, nil, domain.ModelGateGrant{Providers: []string{"local"}}), false},
		{"no model gate grant", docker, agentWorkload(domain.ClassLongLivedAgent, nil, domain.ModelGateGrant{}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.dest.LearningsAllowed(tc.wl); got != tc.want {
				t.Fatalf("LearningsAllowed = %v, want %v", got, tc.want)
			}
			st := newTestStore(t)
			seedAcceptedLearning(t, st, "advice", acceptedAdvice, domain.LearningScope{Global: true}, time.Now().UTC().Add(-time.Hour))
			seedLegacyLearning(t, st)
			pkt := Assemble(t.Context(), st, domain.Task{ID: "task", InputJSON: `{"prompt":"work"}`}, tc.wl, tc.dest)
			if strings.Contains(pkt.Markdown, legacyAdvice) {
				t.Fatalf("legacy Court Memory learning reached the packet:\n%s", pkt.Markdown)
			}
			if got := strings.Contains(pkt.Markdown, acceptedAdvice); got != tc.want {
				t.Fatalf("accepted learning in packet = %v, want %v:\n%s", got, tc.want, pkt.Markdown)
			}
			if tc.want != (pkt.LearningCount == 1) {
				t.Fatalf("LearningCount = %d", pkt.LearningCount)
			}
		})
	}
}

// TestPacketLearningEligibility checks that only accepted, unexpired,
// matching records from the dedicated store reach a local packet.
func TestPacketLearningEligibility(t *testing.T) {
	dest := Destination{DockerAgents: true}
	wl := agentWorkload(domain.ClassLongLivedAgent, nil, domain.ModelGateGrant{Allowed: true, Providers: []string{"local"}})
	now := time.Now().UTC()
	for _, tc := range []struct {
		name  string
		seed  func(t *testing.T, st store.Store)
		want  string
		count int
	}{
		{"legacy learning:global row only", seedLegacyLearning, "", 0},
		{"legacy repo and path rows", func(t *testing.T, st store.Store) {
			for _, ns := range []string{LearningNamespaceRepoPrefix + "sablecourt", LearningNamespacePathPrefix + "**"} {
				if err := st.PutMemory(t.Context(), domain.MemoryEntry{Namespace: ns, Key: "k", Value: legacyAdvice, Author: "operator", UpdatedAt: now}); err != nil {
					t.Fatal(err)
				}
			}
		}, "", 0},
		{"proposed learning", func(t *testing.T, st store.Store) {
			seedAcceptedLearning(t, st, "seed", "seed", domain.LearningScope{Cluster: "unused"}, now)
			ls, _ := store.Learnings(st)
			if _, err := ls.ProposeLearning(t.Context(), domain.Learning{ID: "proposed", Statement: acceptedAdvice, Scope: domain.LearningScope{Global: true}, SourceKind: domain.LearningSourceTask, SourceID: "evidence-task", EvidenceRef: "artifact:record", Author: "operator-1"}); err != nil {
				t.Fatal(err)
			}
		}, "", 0},
		{"expired learning", func(t *testing.T, st store.Store) {
			seedAcceptedLearning(t, st, "expired", acceptedAdvice, domain.LearningScope{Global: true}, now.Add(-domain.LearningReviewTTL-time.Hour))
		}, "", 0},
		{"repository scope without a remote URL", func(t *testing.T, st store.Store) {
			seedAcceptedLearning(t, st, "repo", acceptedAdvice, domain.LearningScope{RepoRemoteURL: "https://example.test/org/repo"}, now)
		}, "", 0},
		{"path scope without changed paths", func(t *testing.T, st store.Store) {
			seedAcceptedLearning(t, st, "path", acceptedAdvice, domain.LearningScope{PathGlobs: []string{"**"}}, now)
		}, "", 0},
		{"incident scope", func(t *testing.T, st store.Store) {
			seedAcceptedLearning(t, st, "incident", acceptedAdvice, domain.LearningScope{Cluster: "prod"}, now)
		}, "", 0},
		{"accepted global learning", func(t *testing.T, st store.Store) {
			seedAcceptedLearning(t, st, "global", acceptedAdvice, domain.LearningScope{Global: true}, now)
		}, "- [global] " + acceptedAdvice, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			tc.seed(t, st)
			pkt := Assemble(t.Context(), st, domain.Task{ID: "task", InputJSON: `{"prompt":"work"}`}, wl, dest)
			if strings.Contains(pkt.Markdown, legacyAdvice) {
				t.Fatalf("legacy learning reached the packet:\n%s", pkt.Markdown)
			}
			if pkt.LearningCount != tc.count {
				t.Fatalf("LearningCount = %d, want %d:\n%s", pkt.LearningCount, tc.count, pkt.Markdown)
			}
			if tc.want == "" && strings.Contains(pkt.Markdown, acceptedAdvice) {
				t.Fatalf("ineligible learning reached the packet:\n%s", pkt.Markdown)
			}
			if tc.want != "" && !strings.Contains(pkt.Markdown, tc.want) {
				t.Fatalf("packet lacks %q:\n%s", tc.want, pkt.Markdown)
			}
		})
	}
}

// TestLearningSectionCap checks the ten-statement and 4 KiB section limits.
func TestLearningSectionCap(t *testing.T) {
	entry := func(i, size int) domain.Learning {
		return domain.Learning{ID: fmt.Sprintf("l-%02d", i), Statement: strings.Repeat("a", size)}
	}
	for _, tc := range []struct {
		name      string
		learnings []domain.Learning
		want      int
	}{
		{"none", nil, 0},
		{"twelve short statements keep ten", func() []domain.Learning {
			var ls []domain.Learning
			for i := range 12 {
				ls = append(ls, entry(i, 10))
			}
			return ls
		}(), 10},
		{"three 1500-byte statements keep two", []domain.Learning{entry(0, 1500), entry(1, 1500), entry(2, 1500)}, 2},
		{"one statement of 4 KiB is omitted", []domain.Learning{entry(0, store.MaxLearningStatementBytes)}, 0},
		{"exact fit is kept", []domain.Learning{entry(0, MaxLearningSectionBytes-len("- [l-00] \n"))}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := learningSection(tc.learnings)
			if len(lines) != tc.want {
				t.Fatalf("kept %d entries, want %d", len(lines), tc.want)
			}
			total := 0
			for i, l := range lines {
				total += len(l.text)
				if !strings.HasPrefix(l.text, "- ["+tc.learnings[i].ID+"] ") {
					t.Fatalf("entry %d out of order: %q", i, l.text[:12])
				}
			}
			if total > MaxLearningSectionBytes {
				t.Fatalf("section is %d bytes", total)
			}
		})
	}
}

// TestPacketLearningsRedacted checks that a statement reaching a local
// packet passes the redactor.
func TestPacketLearningsRedacted(t *testing.T) {
	st := newTestStore(t)
	// Built from parts so the source holds no key-shaped literal.
	secret := "ghp_" + strings.Repeat("A1b2C3d4", 5)
	seedAcceptedLearning(t, st, "advice", "Rotate "+secret+" weekly", domain.LearningScope{Global: true}, time.Now().UTC())
	wl := agentWorkload(domain.ClassLongLivedAgent, nil, domain.ModelGateGrant{Allowed: true, Providers: []string{"local"}})
	pkt := Assemble(context.Background(), st, domain.Task{InputJSON: `{"prompt":"work"}`}, wl, Destination{DockerAgents: true})
	if pkt.LearningCount != 1 || strings.Contains(pkt.Markdown, secret) {
		t.Fatalf("count %d, packet:\n%s", pkt.LearningCount, pkt.Markdown)
	}
}
