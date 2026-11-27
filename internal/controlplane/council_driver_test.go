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
	"github.com/shadewright-labs/sablecourt/internal/redact"
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
	relParent := t.TempDir()
	relProjectDir := filepath.Join(relParent, "project")
	if err := os.Mkdir(relProjectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	canonRelProject, err := filepath.EvalSymlinks(relProjectDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(relParent)
	for _, tc := range []struct {
		name     string
		repoPath string
		checkout store.CouncilCheckout
		allowed  bool
		wantRepo string
	}{
		{"working tree", "project", store.CouncilCheckout{}, true, "project"},
		{"pinned reader", "project", store.CouncilCheckout{Commit: commit, Source: "project"}, true, "project"},
		{"relative repo matches absolute source", "project", store.CouncilCheckout{Commit: commit, Source: canonRelProject}, true, canonRelProject},
		{"wrong project", "project", store.CouncilCheckout{Commit: commit, Source: "other"}, false, ""},
		{"writer", "project", store.CouncilCheckout{Commit: commit, Source: "project", WriterRole: "reader"}, true, "project"},
		{"writer peer", "project", store.CouncilCheckout{Commit: commit, Source: "project", WriterRole: "author"}, true, "project"},
		{"stale writer fields", "project", store.CouncilCheckout{Commit: commit, Source: "project", WriterRole: "author"}, true, "project"},
		{"builtin panel binds checkout source", "", store.CouncilCheckout{Commit: commit, Source: canonDir}, true, canonDir},
		{"builtin panel rejects empty source", "", store.CouncilCheckout{Commit: commit}, false, ""},
		{"builtin panel rejects relative source", "", store.CouncilCheckout{Commit: commit, Source: "relative/repo"}, false, ""},
		{"symlinked repo matches canonical source", linkDir, store.CouncilCheckout{Commit: commit, Source: canonDir}, true, canonDir},
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

func TestCouncilDispatchUsesAdmittedResolvedDir(t *testing.T) {
	ctx := t.Context()
	repoA := t.TempDir()
	canonA, err := filepath.EvalSymlinks(repoA)
	if err != nil {
		t.Fatal(err)
	}
	repoB := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "manifest-link")
	if err := os.Symlink(canonA, linkDir); err != nil {
		t.Fatal(err)
	}

	r, st, _, _ := regressionFixture(t)
	w := r.bundle.Workloads["worker"]
	w.Metadata.Zone = domain.ZoneSandbox
	w.Spec.Class = domain.ClassCodingAgent
	w.Spec.Coding = &domain.CodingConfig{RepoPath: linkDir}
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: w.Metadata.Zone, Class: w.Spec.Class}); err != nil {
		t.Fatal(err)
	}
	spec := store.CouncilSpec{
		Question: "Review?",
		Checkout: store.CouncilCheckout{Source: canonA},
		Members:  []store.CouncilMember{{Vassal: domain.ConclaveVassal{Role: "reader", Workload: "worker"}, Zone: w.Metadata.Zone}},
	}
	if err := st.CreateCouncil(ctx, domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive}, spec); err != nil {
		t.Fatal(err)
	}
	a := store.ConclaveAssignment{Round: 1, Attempt: 1, ConclaveID: "room", Role: "reader"}
	input, err := json.Marshal(struct {
		store.ConclaveAssignment
		Packet string `json:"context_packet"`
		Mode   string `json:"checkoutMode"`
	}{a, "Review?", "live"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.SubmitConclaveTurn(ctx, "room", a.TurnKey(), store.ConclaveLimits{}, func(ctx context.Context) (domain.Task, error) {
		return r.SubmitTask(ctx, "worker", a.TurnKey(), string(input))
	})
	if err != nil {
		t.Fatal(err)
	}
	// Retarget the manifest symlink to another checkout before the adapter reads RepoPath.
	adapter := &retargetLinkAdapter{link: linkDir, target: repoB}
	if err := launchForTest(ctx, r, task, w, adapter); err != nil {
		t.Fatalf("launchForTest: %v", err)
	}
	if adapter.seenRepo != canonA {
		t.Fatalf("adapter received RepoPath %q (resolved to %q), want admitted %q", adapter.seenRepo, adapter.resolvedInStart, canonA)
	}
	if adapter.resolvedInStart != canonA {
		t.Fatalf("adapter resolved RepoPath to %q after symlink retarget, want %q", adapter.resolvedInStart, canonA)
	}

	// Swapping the admitted directory for a different inode must fail closed.
	t.Run("replaced directory identity refused", func(t *testing.T) {
		dirPath := filepath.Join(t.TempDir(), "admitted")
		if err := os.Mkdir(dirPath, 0o700); err != nil {
			t.Fatal(err)
		}
		canon, err := filepath.EvalSymlinks(dirPath)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(canon)
		if err != nil {
			t.Fatal(err)
		}
		// Remove and recreate at the same path to obtain a new inode (or force mismatched identity).
		if err := os.Remove(canon); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(canon, 0o700); err != nil {
			t.Fatal(err)
		}
		_ = info
		r2, st2, _, _ := regressionFixture(t)
		w2 := r2.bundle.Workloads["worker"]
		w2.Metadata.Zone = domain.ZoneSandbox
		w2.Spec.Class = domain.ClassCodingAgent
		w2.Spec.Coding = &domain.CodingConfig{}
		if err := st2.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: w2.Metadata.Zone, Class: w2.Spec.Class}); err != nil {
			t.Fatal(err)
		}
		spec2 := store.CouncilSpec{
			Question: "Review?",
			Checkout: store.CouncilCheckout{Source: canon, SourceDev: 1, SourceIno: 1},
			Members:  []store.CouncilMember{{Vassal: domain.ConclaveVassal{Role: "reader", Workload: "worker"}, Zone: w2.Metadata.Zone}},
		}
		if err := st2.CreateCouncil(ctx, domain.ConclaveSession{ID: "room2", State: domain.ConclaveStateActive}, spec2); err != nil {
			t.Fatal(err)
		}
		a2 := store.ConclaveAssignment{Round: 1, Attempt: 1, ConclaveID: "room2", Role: "reader"}
		in2, err := json.Marshal(struct {
			store.ConclaveAssignment
			Packet string `json:"context_packet"`
			Mode   string `json:"checkoutMode"`
		}{a2, "Review?", "live"})
		if err != nil {
			t.Fatal(err)
		}
		task2, err := store.SubmitConclaveTurn(ctx, "room2", a2.TurnKey(), store.ConclaveLimits{}, func(ctx context.Context) (domain.Task, error) {
			return r2.SubmitTask(ctx, "worker", a2.TurnKey(), string(in2))
		})
		if err != nil {
			t.Fatal(err)
		}
		adapter2 := &pinnedAdapter{}
		if err := launchForTest(ctx, r2, task2, w2, adapter2); err == nil || adapter2.lastSpec.TaskID != "" {
			t.Fatalf("expected changed directory identity to refuse launch, got err=%v task=%q", err, adapter2.lastSpec.TaskID)
		}
	})
}

type retargetLinkAdapter struct {
	pinnedAdapter
	link            string
	target          string
	seenRepo        string
	resolvedInStart string
}

func (a *retargetLinkAdapter) Start(ctx context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	if err := os.Remove(a.link); err != nil {
		return scruntime.Handle{}, err
	}
	if err := os.Symlink(a.target, a.link); err != nil {
		return scruntime.Handle{}, err
	}
	a.seenRepo = spec.Workload.Spec.Coding.RepoPath
	resolved, err := filepath.EvalSymlinks(a.seenRepo)
	if err != nil {
		return scruntime.Handle{}, err
	}
	a.resolvedInStart = resolved
	return a.pinnedAdapter.Start(ctx, spec)
}

type failEventsOrHeartbeatStore struct {
	store.Store
	store.CouncilStore
	failEvents    bool
	failHeartbeat bool
}

func (s failEventsOrHeartbeatStore) ListTaskEvents(ctx context.Context, taskID string) ([]domain.TaskEvent, error) {
	if s.failEvents {
		return nil, errors.New("injected event read failure")
	}
	return s.Store.ListTaskEvents(ctx, taskID)
}

func (s failEventsOrHeartbeatStore) RecordHeartbeat(ctx context.Context, taskID string, at time.Time) error {
	if s.failHeartbeat {
		return errors.New("injected heartbeat write failure")
	}
	return s.Store.RecordHeartbeat(ctx, taskID, at)
}

func TestCouncilLeaseRevokedOnTerminalObservationAndCappedAtDeadline(t *testing.T) {
	setupCouncilTask := func(t *testing.T, duration time.Duration) (*Reconciler, store.Store, domain.Task, *writerObservation, time.Time) {
		t.Helper()
		ctx := t.Context()
		r, st, _, _ := regressionFixture(t)
		w := r.bundle.Workloads["worker"]
		w.Metadata.Zone = domain.ZoneSandbox
		w.Spec.Class = domain.ClassCodingAgent
		w.Spec.Coding = &domain.CodingConfig{RepoPath: "project"}
		if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: w.Metadata.Zone, Class: w.Spec.Class}); err != nil {
			t.Fatal(err)
		}
		now := r.now()
		seat := domain.ConclaveVassal{Role: "reader", Workload: "worker", CanMessage: []string{"all"}}
		spec := store.CouncilSpec{
			Question: "Review?",
			Members:  []store.CouncilMember{{Vassal: seat, Zone: domain.ZoneSandbox}},
			Checkout: store.CouncilCheckout{Source: "project"},
			Duration: &duration,
		}
		if err := st.CreateCouncil(ctx, domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive, CreatedAt: now}, spec); err != nil {
			t.Fatal(err)
		}
		a := store.ConclaveAssignment{ConclaveID: "room", Role: "reader", Round: 1, Attempt: 1}
		input := `{"conclaveId":"room","role":"reader","round":1,"attempt":1,"context_packet":"review"}`
		task, err := store.SubmitConclaveTurn(ctx, "room", a.TurnKey(), store.ConclaveLimits{}, func(ctx context.Context) (domain.Task, error) {
			return r.SubmitTask(ctx, "worker", a.TurnKey(), input)
		})
		if err != nil {
			t.Fatal(err)
		}
		adapter := &writerObservation{}
		adapter.nextState = scruntime.StateRunning
		if err := launchForTest(ctx, r, task, w, adapter); err != nil {
			t.Fatal(err)
		}
		task, err = st.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return r, st, task, adapter, now.Add(duration)
	}

	for _, tc := range []struct {
		name          string
		failEvents    bool
		failHeartbeat bool
	}{
		{name: "event read failure after exit", failEvents: true},
		{name: "heartbeat write failure after exit", failHeartbeat: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			r, st, task, adapter, _ := setupCouncilTask(t, 15*time.Minute)
			hash := model.HashToken(adapter.lastSpec.TaskToken)
			if _, err := st.GetActiveLeaseByTokenHash(ctx, hash, r.now()); err != nil {
				t.Fatalf("expected active lease before exit: %v", err)
			}
			adapter.nextState = scruntime.StateCompleted
			cs, _ := store.Councils(st)
			r.store = failEventsOrHeartbeatStore{Store: st, CouncilStore: cs, failEvents: tc.failEvents, failHeartbeat: tc.failHeartbeat}
			if _, err := r.observeTask(ctx, task); err == nil {
				t.Fatal("expected injected store failure from observeTask")
			}
			if _, err := st.GetActiveLeaseByTokenHash(ctx, hash, r.now()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("lease survived terminal observation after store failure: err=%v", err)
			}
		})
	}

	t.Run("lease expiry capped at council deadline on mint and renewal", func(t *testing.T) {
		ctx := t.Context()
		r, st, task, adapter, deadline := setupCouncilTask(t, 2*time.Minute)
		hash := model.HashToken(adapter.lastSpec.TaskToken)
		lease, err := st.GetActiveLeaseByTokenHash(ctx, hash, r.now())
		if err != nil {
			t.Fatalf("expected active lease: %v", err)
		}
		if lease.ExpiresAt.After(deadline) {
			t.Fatalf("minted lease expires at %v, after council deadline %v", lease.ExpiresAt, deadline)
		}
		if _, err := st.GetActiveLeaseByTokenHash(ctx, hash, deadline); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("minted lease still active at council deadline: %v", err)
		}
		// Advance observation while running; renewal must also stay capped at deadline.
		adapter.nextState = scruntime.StateRunning
		if _, err := r.observeTask(ctx, task); err != nil {
			t.Fatalf("observeTask running: %v", err)
		}
		lease, err = st.GetActiveLeaseByTokenHash(ctx, hash, r.now())
		if err != nil {
			t.Fatalf("expected active lease after renewal: %v", err)
		}
		if lease.ExpiresAt.After(deadline) {
			t.Fatalf("renewed lease expires at %v, after council deadline %v", lease.ExpiresAt, deadline)
		}
	})

	t.Run("renewal extends the exact-match expiry of the task token", func(t *testing.T) {
		ctx := t.Context()
		r, st, task, adapter, _ := setupCouncilTask(t, 3*time.Hour)
		// A value registered for the task that expires at once stands in for a
		// token minted earlier than the renewal.
		probe := "sc_task_probe_" + strings.Repeat("a", 40)
		redact.RegisterOwnedUntil(task.ID, r.now().Add(time.Second), probe)
		adapter.nextState = scruntime.StateRunning
		if _, err := r.observeTask(ctx, task); err != nil {
			t.Fatalf("observeTask running: %v", err)
		}
		lease, err := st.GetActiveLeaseByTokenHash(ctx, model.HashToken(adapter.lastSpec.TaskToken), r.now())
		if err != nil {
			t.Fatalf("expected active lease after renewal: %v", err)
		}
		got, ok := redact.DefaultRegistry().OwnedExpiry(task.ID)
		if !ok || got.Before(lease.ExpiresAt) {
			t.Fatalf("registry expiry %v is before the renewed lease expiry %v", got, lease.ExpiresAt)
		}
	})
}
