package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func TestCouncilCheckoutAtDispatch(t *testing.T) {
	commit := strings.Repeat("a", 40)
	realDir := t.TempDir()
	canonDir, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		repoPath string
		checkout store.CouncilCheckout
		allowed  bool
		wantRepo string
	}{
		{"working tree", "project", store.CouncilCheckout{}, true, "project"},
		{"pinned reader", "project", store.CouncilCheckout{Commit: commit, Source: "project"}, true, "project"},
		{"wrong project", "project", store.CouncilCheckout{Commit: commit, Source: "other"}, false, ""},
		{"writer", "project", store.CouncilCheckout{Commit: commit, Source: "project", WriterRole: "reader"}, true, "project"},
		{"writer peer", "project", store.CouncilCheckout{Commit: commit, Source: "project", WriterRole: "author"}, true, "project"},
		{"stale writer fields", "project", store.CouncilCheckout{Commit: commit, Source: "project", WriterRole: "author"}, true, "project"},
		{"builtin panel binds checkout source", "", store.CouncilCheckout{Commit: commit, Source: canonDir}, true, canonDir},
		{"builtin panel rejects empty source", "", store.CouncilCheckout{Commit: commit}, false, ""},
		{"builtin panel rejects relative source", "", store.CouncilCheckout{Commit: commit, Source: "relative/repo"}, false, ""},
		{"symlinked repo matches canonical source", linkDir, store.CouncilCheckout{Commit: commit, Source: canonDir}, true, linkDir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			r, st, _, _ := regressionFixture(t)
			w := r.bundle.Workloads["worker"]
			w.Metadata.Zone = domain.ZoneSandbox
			w.Spec.Class = domain.ClassCodingAgent
			w.Spec.Coding = &domain.CodingConfig{RepoPath: tc.repoPath}
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: w.Metadata.Zone, Class: w.Spec.Class}); err != nil {
				t.Fatal(err)
			}
			spec := store.CouncilSpec{Question: "Review?", Checkout: tc.checkout, Members: []store.CouncilMember{{Vassal: domain.ConclaveVassal{Role: "reader", Workload: "worker"}, Zone: w.Metadata.Zone}}}
			if tc.checkout.WriterRole == "author" {
				spec.Members = append(spec.Members, store.CouncilMember{Vassal: domain.ConclaveVassal{Role: "author", Workload: "worker"}, Zone: w.Metadata.Zone})
			}
			if err := st.CreateCouncil(ctx, domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive}, spec); err != nil {
				t.Fatal(err)
			}
			a := store.ConclaveAssignment{Round: 1, Attempt: 1, ConclaveID: "room", Role: "reader"}
			mode := "live"
			switch {
			case tc.checkout.WriterRole != "":
				mode = "writer"
			case tc.checkout.Commit != "":
				mode = "ref"
			}
			input, err := json.Marshal(struct {
				store.ConclaveAssignment
				Packet string `json:"context_packet"`
				Mode   string `json:"checkoutMode"`
				Ref    string `json:"checkoutRef,omitempty"`
				Writer string `json:"checkoutWriter,omitempty"`
			}{a, "Review?", mode, tc.checkout.Commit, tc.checkout.WriterRole})
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "stale writer fields" {
				input = []byte(`{"conclaveId":"room","role":"reader","round":1,"attempt":1,"context_packet":"review","checkoutMode":null,"checkoutRef":false,"checkoutWriter":"reader"}`)
			}
			task, err := store.SubmitConclaveTurn(ctx, "room", a.TurnKey(), store.ConclaveLimits{}, func(ctx context.Context) (domain.Task, error) {
				return r.SubmitTask(ctx, "worker", a.TurnKey(), string(input))
			})
			if err != nil {
				t.Fatal(err)
			}
			adapter := &pinnedAdapter{}
			err = launchForTest(ctx, r, task, w, adapter)
			if tc.allowed {
				if err != nil || adapter.pin != "" || adapter.lastSpec.TaskID != task.ID {
					t.Fatalf("pinned checkout %q, read-only %t, err %v", adapter.pin, adapter.readOnly, err)
				}
				if adapter.lastSpec.Workload.Spec.Coding == nil || adapter.lastSpec.Workload.Spec.Coding.RepoPath != tc.wantRepo {
					t.Fatalf("launch repo = %+v, want %q", adapter.lastSpec.Workload.Spec.Coding, tc.wantRepo)
				}
				var got struct {
					Role, CheckoutMode, CheckoutRef, CheckoutWriter string
				}
				if err := json.Unmarshal([]byte(adapter.lastSpec.InputJSON), &got); err != nil || got.CheckoutMode != mode ||
					got.Role != a.Role || got.CheckoutRef != tc.checkout.Commit || got.CheckoutWriter != tc.checkout.WriterRole {
					t.Fatalf("launch input %s, err %v", adapter.lastSpec.InputJSON, err)
				}
			} else if err == nil || adapter.lastSpec.TaskID != "" {
				t.Fatal("council launched with an unverified checkout")
			}
		})
	}
}

type writerObservation struct {
	artifactAdapter
	exitCode int
}

func (a *writerObservation) Observe(context.Context, scruntime.Handle) (scruntime.Observation, error) {
	return scruntime.Observation{State: a.nextState, ExitCode: a.exitCode, ObservedAt: time.Now().UTC()}, nil
}

func TestObservedWriterDiff(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state scruntime.ObservedState
		code  int
	}{
		{"zero", scruntime.StateCompleted, 0},
		{"retry exit write", scruntime.StateCompleted, 0},
		{"failed", scruntime.StateFailed, 7},
		{"peer diagnostics", scruntime.StateFailed, 7},
		{"inconsistent exit", scruntime.StateCompleted, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			r, st, _, _ := regressionFixture(t)
			w := r.bundle.Workloads["worker"]
			w.Spec.Class = domain.ClassCodingAgent
			w.Spec.Coding = &domain.CodingConfig{RepoPath: "project"}
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneSandbox, Class: w.Spec.Class}); err != nil {
				t.Fatal(err)
			}
			seat := domain.ConclaveVassal{Role: "writer", Workload: "worker", CanMessage: []string{"all"}}
			spec := store.CouncilSpec{Question: "review", Members: []store.CouncilMember{{Vassal: seat, Zone: domain.ZoneSandbox}}, Checkout: store.CouncilCheckout{Source: "project", WriterRole: "writer", Commit: strings.Repeat("a", 40)}}
			role := "writer"
			if tc.name == "peer diagnostics" {
				role = "peer"
				peer := seat
				peer.Role = role
				spec.Members = append(spec.Members, store.CouncilMember{Vassal: peer, Zone: domain.ZoneSandbox})
			}
			if err := st.CreateCouncil(ctx, domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive}, spec); err != nil {
				t.Fatal(err)
			}
			a := store.ConclaveAssignment{ConclaveID: "room", Role: role, Round: 1, Attempt: 1}
			input := `{"conclaveId":"room","role":"` + role + `","round":1,"attempt":1,"context_packet":"review"}`
			task, err := store.SubmitConclaveTurn(ctx, "room", a.TurnKey(), store.ConclaveLimits{}, func(ctx context.Context) (domain.Task, error) { return r.SubmitTask(ctx, "worker", a.TurnKey(), input) })
			if err != nil {
				t.Fatal(err)
			}
			adapter := &writerObservation{exitCode: tc.code}
			adapter.nextState = tc.state
			adapter.artifacts = []scruntime.Artifact{{Name: "git-diff", ContentType: "text/x-diff", Text: "+edit\n+password=private-fixture"}}
			if err := launchForTest(ctx, r, task, w, adapter); err != nil {
				t.Fatal(err)
			}
			task, err = st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "retry exit write" {
				hash := model.HashToken(adapter.lastSpec.TaskToken)
				if _, err := st.GetActiveLeaseByTokenHash(ctx, hash, r.now()); err != nil {
					t.Fatal("missing live lease", err)
				}
				r.store = struct {
					exitWriteFailure
					store.CouncilStore
				}{exitWriteFailure{st}, st}
				if _, err := r.observeTask(ctx, task); err == nil {
					t.Fatal("exit write failure was hidden")
				}
				if _, err := st.GetActiveLeaseByTokenHash(ctx, hash, r.now()); !errors.Is(err, store.ErrNotFound) {
					t.Fatal("finished writer retained a live lease")
				}
				r.store = st
			}
			if _, err := r.observeTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			s, err := st.CouncilSnapshot(ctx, "room")
			if err != nil {
				t.Fatal(err)
			}
			relics := s.Attempts[0].Relics
			task, err = st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if role == "peer" {
				if len(relics) != 0 || !strings.Contains(task.ArtifactsJSON, "git-diff") {
					t.Fatal("peer diagnostic was dropped or published as a writer diff")
				}
			} else if tc.code != 0 {
				if len(relics) != 0 || strings.Contains(task.ArtifactsJSON, "git-diff") || task.State != domain.TaskStateFailed {
					t.Fatal("failed exit retained a diff or passed as completed")
				}
			} else if len(relics) != 1 || relics[0].Name != "git-diff-"+task.ID || relics[0].Body != "+edit\n+password=[REDACTED:secret]" || relics[0].AuthorRole != "writer" || task.State != domain.TaskStateCompleted {
				t.Fatal("missing attributed writer diff after success")
			}
		})
	}
}
