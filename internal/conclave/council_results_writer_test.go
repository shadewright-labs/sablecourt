package conclave

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func writerRoom(t *testing.T, starts int) *driverFixture {
	t.Helper()
	return driverRoomWithCheckout(t, false, starts, starts, store.CouncilCheckout{Source: "project", Commit: strings.Repeat("a", 40), WriterRole: "north"})
}

func TestWriterDiffBeforeAnswer(t *testing.T) {
	diff := rt.Artifact{Name: "git-diff", ContentType: "text/x-diff", Text: "+edit"}
	for _, tc := range []struct {
		name   string
		diffs  []rt.Artifact
		staged string
	}{
		{"missing relic", []rt.Artifact{diff}, ""},
		{"unexpected relic", nil, "+unverified"},
		{"mismatched relic", []rt.Artifact{diff}, "+unverified"},
		{"duplicate", []rt.Artifact{diff, diff}, ""},
		{"empty", []rt.Artifact{{Name: "git-diff", ContentType: "text/x-diff"}}, ""},
		{"oversize", []rt.Artifact{{Name: "git-diff", ContentType: "text/x-diff", Text: strings.Repeat("p", store.MaxCouncilText+1)}}, ""},
		{"wrong type", []rt.Artifact{{Name: "git-diff", ContentType: "text/html", Text: "+edit"}}, ""},
		{"indirect", []rt.Artifact{{Name: "git-diff", ContentType: "text/x-diff", URI: "elsewhere", Text: "+edit"}}, ""},
		{"null text", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := writerRoom(t, 6)
			task := reserveResult(t, f.Store, "room", "north", 1, 1)
			if tc.staged != "" {
				err := f.PutCouncilRelic(t.Context(), task.ID, domain.ConclaveRelic{ConclaveID: "room", AuthorRole: "north", Name: "git-diff-" + task.ID, ContentType: "text/x-diff", Body: tc.staged})
				if err != nil {
					t.Fatal(err)
				}
			}
			raw, err := json.Marshal(append([]rt.Artifact{{Name: "final-message", ContentType: "text/plain", Text: "review"}}, tc.diffs...))
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "null text" {
				raw = []byte(`[{"name":"final-message","contentType":"text/plain","text":"review"},{"name":"git-diff","contentType":"text/x-diff","text":null}]`)
			}
			finishResult(t, f.Store, task, domain.TaskStateCompleted, `{"exit_code":0}`, string(raw))
			for range 2 {
				ok, err := f.Broker.RecordCouncilAnswer(t.Context(), "room", task.ID)
				if err != nil || ok || f.snapshot(t, 1).Turns[0].Answer != nil {
					t.Fatalf("accepted unverified diff, err=%v", err)
				}
			}
		})
	}
}

type diffReviewStub struct {
	*councilStub
	st         store.Store
	retry      bool
	diff       string
	longAnswer bool
}

func (s *diffReviewStub) Observe(ctx context.Context, h rt.Handle) (rt.Observation, error) {
	t, err := s.st.GetTask(ctx, h.ExternalID)
	if err != nil {
		return rt.Observation{}, err
	}
	a, _ := store.TaskConclaveAssignment(t)
	obs := rt.Observation{State: rt.StateCompleted, ObservedAt: time.Now().UTC()}
	if s.retry && a.Role == "north" && a.Round == 1 && a.Attempt == 1 {
		obs.State, obs.ExitCode = rt.StateFailed, 7
	}
	return obs, nil
}

func (s *diffReviewStub) CollectArtifacts(ctx context.Context, h rt.Handle) ([]rt.Artifact, error) {
	t, err := s.st.GetTask(ctx, h.ExternalID)
	if err != nil {
		return nil, err
	}
	a, _ := store.TaskConclaveAssignment(t)
	artifacts := []rt.Artifact{{Name: "final-message", ContentType: "text/plain", Text: "review from " + a.Role}}
	if s.longAnswer && a.Role == "north" {
		artifacts[0].Text += strings.Repeat(" conclusion", 3000)
	}
	if a.Role == "north" && a.Attempt == 1 {
		artifacts = append(artifacts, rt.Artifact{Name: "git-diff", ContentType: "text/x-diff", Text: s.diff})
	}
	return artifacts, nil
}

func TestWriterDiffReviewPacket(t *testing.T) {
	const secret = "private-fixture"
	diff := "+first edit\n+password=" + secret + "\n" + strings.Repeat("context ", 4000) + "last edit"
	for _, tc := range []struct {
		name              string
		retry, longAnswer bool
	}{{"success", false, false}, {"long answer", false, true}, {"retry", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			retry := tc.retry
			starts := 6
			f := writerRoom(t, starts)
			ctx := t.Context()
			w := f.bundle.Workloads["worker"]
			w.Spec.Class = domain.ClassCodingAgent
			w.Spec.Coding = &domain.CodingConfig{RepoPath: "project"}
			f.bundle.Workloads["worker"] = w
			adapter := &diffReviewStub{f.stub, f.Store, retry, diff, tc.longAnswer}
			f.rec = controlplane.NewReconciler(controlplane.Options{Store: f.Store, Bundle: f.bundle, Adapters: map[domain.WorkloadClass]rt.Adapter{w.Spec.Class: adapter}})
			f.Broker = NewBroker(Options{Store: f.Store, Reconciler: f.rec, GetBundle: func() *config.Bundle { return f.bundle }})
			f.pass(t)
			f.launch(t, 3)
			if err := f.rec.ReconcileOnce(ctx); err != nil {
				t.Fatal(err)
			}
			f.pass(t)
			if retry {
				s := f.snapshot(t, 4)
				a := s.Attempts[1]
				if a.Role != "north" || a.Number != 2 || a.Round != 1 || len(s.Attempts[0].Relics) != 0 {
					t.Fatal("failed writer crossed barrier or left a Relic")
				}
				f.launch(t, 4)
				if err := f.rec.ReconcileOnce(ctx); err != nil {
					t.Fatal(err)
				}
				f.pass(t)
			}
			reviewers := 0
			for _, a := range f.snapshot(t, starts).Attempts {
				if a.Round != 2 || a.Role == "north" {
					continue
				}
				reviewers++
				task, err := f.GetTask(ctx, a.TaskID)
				var p struct {
					Packet string `json:"context_packet"`
				}
				if err != nil || json.Unmarshal([]byte(task.InputJSON), &p) != nil {
					t.Fatal("missing review packet", err)
				}
				if retry {
					if !strings.Contains(p.Packet, "writer attempt 1 produced nothing") {
						t.Fatal("missing failed attempt notice")
					}
				} else {
					i := strings.Index(p.Packet, "\n### Round")
					if i < 0 || len(p.Packet[i:]) > 16<<10 || !strings.Contains(p.Packet, "+first edit") || !strings.Contains(p.Packet, "[REDACTED:secret]") || !strings.Contains(p.Packet, "[peer text omitted]") || strings.Contains(p.Packet, "last edit") || strings.Contains(p.Packet, secret) {
						t.Fatal("writer excerpt missing, unbounded or unredacted")
					}
				}
			}
			if reviewers == 0 {
				t.Fatal("no reviewer received a second packet")
			}
		})
	}
}
