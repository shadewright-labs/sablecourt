package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/operatortoken"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func TestCouncilOpenRefusals(t *testing.T) {
	repo, bin, token := councilTestInputs(t)
	councilTestGit(t, repo, "tag", "refs/heads/ghost")
	valid := fmt.Sprintf(`"question":"Review this","repo":%s,"ref":"HEAD"`, strconv.Quote(repo))
	for _, tc := range []struct {
		name, body, credential, message              string
		want                                         int
		missingBinary, noSandbox, citadel, noRuntime bool
	}{
		{name: "missing credential", credential: "-", want: 401, message: "operator credentials required"},
		{name: "task credential", credential: model.TokenPrefix + strings.Repeat("a", 64), want: 401, message: "operator credentials required"},
		{name: "observer credential", credential: "fixture-observer", want: 401, message: "operator credentials required"},
		{name: "invalid operator credential", credential: "op_" + strings.Repeat("b", 64), want: 401, message: "operator credentials required"},
		{name: "blank question", body: strings.Replace(valid, "Review this", "  ", 1), want: 400, message: "question"},
		{name: "missing question", body: fmt.Sprintf(`"repo":%q,"ref":"HEAD"`, repo), want: 400, message: "question"},
		{name: "wrong question type", body: strings.Replace(valid, `"Review this"`, "42", 1), want: 400, message: "JSON"},
		{name: "null question", body: strings.Replace(valid, `"Review this"`, "null", 1), want: 400, message: "JSON"},
		{name: "NUL question", body: strings.Replace(valid, "Review this", `Review\u0000this`, 1), want: 400, message: "question"},
		{name: "question over packet budget", body: strings.Replace(valid, "Review this", strings.Repeat("q", 32<<10), 1), want: 400, message: "question"},
		{name: "escaped question over launch budget", body: strings.Replace(valid, "Review this", strings.Repeat("<", 12<<10), 1), want: 400, message: "question"},
		{name: "escaped question exceeds round2 budget", body: strings.Replace(valid, "Review this", strings.Repeat("<", 10240), 1), want: 400, message: "question"},
		{name: "body too big", body: valid + strings.Repeat(" ", 1<<20), want: 413, message: "too large"},
		{name: "zero rounds", body: valid + `,"rounds":0`, want: 400, message: "rounds"},
		{name: "negative rounds", body: valid + `,"rounds":-1`, want: 400, message: "rounds"},
		{name: "zero starts", body: valid + `,"starts":0`, want: 400, message: "starts"},
		{name: "negative starts", body: valid + `,"starts":-1`, want: 400, message: "starts"},
		{name: "starts beyond ward", body: valid + `,"starts":7`, want: 400, message: "starts"},
		{name: "zero duration", body: valid + `,"durationSeconds":0`, want: 400, message: "durationSeconds"},
		{name: "negative duration", body: valid + `,"durationSeconds":-1`, want: 400, message: "durationSeconds"},
		{name: "duration overflow", body: valid + `,"durationSeconds":9223372036854775807`, want: 400, message: "durationSeconds"},
		{name: "null ceiling", body: valid + `,"rounds":null`, want: 400, message: "JSON"},
		{name: "fractional ceiling", body: valid + `,"rounds":1.5`, want: 400, message: "JSON"},
		{name: "wrong ceiling type", body: valid + `,"starts":"2"`, want: 400, message: "JSON"},
		{name: "blank repo", body: strings.Replace(valid, strconv.Quote(repo), `""`, 1), want: 400, message: "repo"},
		{name: "explicit live false without ref", body: strings.TrimSuffix(valid, `,"ref":"HEAD"`) + `,"live":false`, want: 400, message: "ref"},
		{name: "blank ref", body: strings.Replace(valid, "HEAD", " ", 1), want: 400, message: "ref"},
		{name: "invalid ref", body: strings.Replace(valid, "HEAD", "missing", 1), want: 400, message: "ref"},
		{name: "qualified branch cannot resolve as tag", body: strings.Replace(valid, "HEAD", "refs/heads/ghost", 1), want: 400, message: "ref"},
		{name: "git revision expression", body: strings.Replace(valid, "HEAD", "HEAD^{commit}", 1), want: 400, message: "ref"},
		{name: "ref and live", body: valid + `,"live":true`, want: 400, message: "live"},
		{name: "writer and live", body: strings.TrimSuffix(valid, `,"ref":"HEAD"`) + `,"live":true,"writer":"claude"`, want: 400, message: "live"},
		{name: "blank writer", body: valid + `,"writer":" "`, want: 400, message: "writer"},
		{name: "unknown writer", body: valid + `,"writer":"other"`, want: 400, message: "writer"},
		{name: "blank ref with writer", body: strings.Replace(valid, "HEAD", " ", 1) + `,"writer":"claude"`, want: 400, message: "ref"},
		{name: "empty panel", body: valid + `,"with":[]`, want: 400, message: "panel"},
		{name: "null panel member", body: valid + `,"with":["claude","codex",null]`, want: 400, message: "panel"},
		{name: "partial panel", body: valid + `,"with":["claude","codex"]`, want: 400, message: "panel"},
		{name: "duplicate role", body: valid + `,"with":["claude","codex","codex"]`, want: 400, message: "panel"},
		{name: "different opening decree", body: valid + `,"decree":"Different question"`, want: 400, message: "decree"},
		{name: "duplicate field", body: valid + `,"rounds":0,"rounds":2`, want: 400, message: "JSON"},
		{name: "wrong case field", body: valid + `,"Rounds":2`, want: 400, message: "JSON"},
		{name: "trailing document", body: valid + `} {`, want: 400, message: "JSON"},
		{name: "missing binary", missingBinary: true, want: 503, message: "agy"},
		{name: "unavailable confinement", noSandbox: true, want: 503, message: "confinement"},
		{name: "unsupported daemon mode", citadel: true, want: 503, message: "Sanctum"},
		{name: "unavailable dispatch", noRuntime: true, want: 503, message: "dispatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := daemonTestStore(t)
			opts := handlerConfig{operatorToken: token, councilsDisabled: tc.citadel,
				checkCouncilSandbox: func(context.Context) error {
					if tc.noSandbox {
						return errors.New("probe refused")
					}
					return nil
				},
			}
			if !tc.noRuntime {
				opts.checkCouncilRuntime = func(context.Context, store.CouncilCheckout) error {
					t.Error("refused request reached runtime admission")
					return errors.New("unexpected admission")
				}
			}
			if tc.missingBinary {
				if err := os.Rename(filepath.Join(bin, "agy"), filepath.Join(bin, "agy-disabled")); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Rename(filepath.Join(bin, "agy-disabled"), filepath.Join(bin, "agy")); err != nil {
						t.Error(err)
					}
				})
			}
			h := councilTestHandler(st, nil, opts)
			body := tc.body
			if body == "" {
				body = valid
			}
			auth := tc.credential
			switch auth {
			case "":
				auth = token
			case "-":
				auth = ""
			}
			response := councilTestRequest(h, http.MethodPost, "/api/v1/councils", auth, "{"+body+"}")
			if response.Code != tc.want || !strings.Contains(response.Body.String(), tc.message) {
				t.Errorf("POST = %d %s, want %d containing %q", response.Code, response.Body, tc.want, tc.message)
			}
			if tc.want == 401 {
				response := councilTestRequest(h, http.MethodPost, "/api/v1/councils", token, "{}")
				if response.Code != 400 {
					t.Errorf("operator could not reach council validation: %d %s", response.Code, response.Body)
				}
			}
			councilAssertEmpty(t, st)
		})
	}
	for _, body := range []string{"", "null", "[]", "{", "{\"question\":\"\xff\"}"} {
		t.Run(fmt.Sprintf("invalid document %q", body), func(t *testing.T) {
			st := daemonTestStore(t)
			h := councilTestHandler(st, nil, handlerConfig{operatorToken: token})
			response := councilTestRequest(h, http.MethodPost, "/api/v1/councils", token, body)
			if response.Code != 400 {
				t.Errorf("POST = %d %s, want 400", response.Code, response.Body)
			}
			councilAssertEmpty(t, st)
		})
	}
}

func TestCouncilOpenPersistsPolicy(t *testing.T) {
	repo, _, token := councilTestInputs(t)
	head := councilTestGit(t, repo, "rev-parse", "HEAD")
	for _, tc := range []struct {
		name, bodyExtra string
		omitRef         bool
		rounds, starts  int
		duration        time.Duration
		roles           string
		sequential      bool
		wantRef         string
		wantCommit      string
		wantWriter      string
	}{
		{name: "defaults", rounds: 2, starts: 6, duration: 15 * time.Minute, roles: "claude,codex,agy", wantRef: "HEAD", wantCommit: head},
		{name: "default live", omitRef: true, rounds: 2, starts: 6, duration: 15 * time.Minute, roles: "claude,codex,agy"},
		{name: "explicit live", omitRef: true, bodyExtra: `,"live":true`, rounds: 2, starts: 6, duration: 15 * time.Minute, roles: "claude,codex,agy"},
		{name: "writer with ref", bodyExtra: `,"writer":"claude"`, rounds: 2, starts: 6, duration: 15 * time.Minute, roles: "claude,codex,agy", wantRef: "HEAD", wantCommit: head, wantWriter: "claude"},
		{name: "writer default HEAD", omitRef: true, bodyExtra: `,"writer":"codex"`, rounds: 2, starts: 6, duration: 15 * time.Minute, roles: "claude,codex,agy", wantRef: "HEAD", wantCommit: head, wantWriter: "codex"},
		{name: "client fields", bodyExtra: `,"title":"Review this","decree":"Review this","rounds":1,"starts":3,"durationSeconds":7,"with":["agy","claude","codex"],"sequential":true`, rounds: 1, starts: 3, duration: 7 * time.Second, roles: "agy,claude,codex", sequential: true, wantRef: "HEAD", wantCommit: head},
		{name: "redaction", bodyExtra: `,"title":"-----BEGIN PRIVATE KEY----- concealed"`, rounds: 2, starts: 6, duration: 15 * time.Minute, roles: "claude,codex,agy", wantRef: "HEAD", wantCommit: head},
		{name: "embedded credential", bodyExtra: fmt.Sprintf(`,"title":%q`, "prefix"+token+"suffix"), rounds: 2, starts: 6, duration: 15 * time.Minute, roles: "claude,codex,agy", wantRef: "HEAD", wantCommit: head},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := daemonTestStore(t)
			h := councilTestHandler(st, nil, councilTestOptions(token))
			question := "Review this"
			if tc.name == "redaction" {
				question += " -----BEGIN PRIVATE KEY----- concealed"
			}
			if tc.name == "embedded credential" {
				question += " prefix" + token + "suffix"
			}
			refField := `,"ref":"HEAD"`
			if tc.omitRef {
				refField = ""
			}
			body := fmt.Sprintf(`{"question":%q,"repo":%q%s%s}`, question, repo, refField, tc.bodyExtra)
			room := councilTestOpen(t, h, token, body)
			cs, _ := store.Councils(st)
			snap, err := cs.CouncilSnapshot(t.Context(), room.ID)
			if err != nil {
				t.Fatal(err)
			}
			if snap.MaxRounds != tc.rounds || snap.MaxStarts != tc.starts || snap.Deadline.Sub(snap.Session.CreatedAt) != tc.duration ||
				snap.Sequential != tc.sequential || snap.Checkout.Commit != tc.wantCommit || snap.Checkout.Ref != tc.wantRef || snap.Checkout.WriterRole != tc.wantWriter {
				t.Fatalf("policy or checkout differs from request: %+v", snap.CouncilPolicy)
			}
			var roles []string
			for _, m := range snap.Members {
				roles = append(roles, m.Vassal.Role)
				if m.Zone != domain.ZoneSandbox || m.Vassal.Workload != m.Vassal.Role {
					t.Fatalf("unexpected panel member: %+v", m)
				}
			}
			if strings.Join(roles, ",") != tc.roles || len(snap.Attempts) != 0 {
				t.Fatal("unexpected persisted panel or premature start")
			}
			if tc.name == "redaction" && (strings.Contains(snap.Question, "concealed") || strings.Contains(room.Title, "concealed")) {
				t.Fatal("unclosed private key was persisted")
			}
			if tc.name == "embedded credential" && (strings.Contains(snap.Question, token) || strings.Contains(room.Title, token)) {
				t.Fatal("operator credential was persisted")
			}
		})
	}
}

func TestCouncilOpenRetriesAfterStoreFailure(t *testing.T) {
	repo, _, token := councilTestInputs(t)
	for _, stage := range []string{"before insert", "during insert"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.db")
			st, err := sqlite.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if err := st.Migrate(t.Context()); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			cs, _ := store.Councils(st)
			wrapped := &councilFailOnceStore{Store: st, CouncilStore: cs, fail: stage == "before insert"}
			if stage == "during insert" {
				_, err := db.ExecContext(t.Context(), `
					CREATE TRIGGER fixture_failure BEFORE INSERT ON councils
					BEGIN SELECT RAISE(ABORT, 'fixture write failure'); END`)
				if err != nil {
					t.Fatal(err)
				}
			}
			h := councilTestHandler(wrapped, nil, councilTestOptions(token))
			body := fmt.Sprintf(`{"question":"Review this","repo":%q,"ref":"HEAD"}`, repo)
			response := councilTestRequest(h, http.MethodPost, "/api/v1/councils", token, body)
			if response.Code != 500 {
				t.Errorf("failed insert returned %d %s", response.Code, response.Body)
			}
			councilAssertEmpty(t, st)
			if stage == "during insert" {
				if _, err := db.ExecContext(t.Context(), "DROP TRIGGER fixture_failure"); err != nil {
					t.Fatal(err)
				}
			}
			room := councilTestOpen(t, h, token, body)
			rooms, err := st.ListConclaves(t.Context())
			if err != nil || len(rooms) != 1 || rooms[0].ID != room.ID {
				t.Fatalf("retry rooms = %d, error = %v", len(rooms), err)
			}
		})
	}
}

type councilFailOnceStore struct {
	store.Store
	store.CouncilStore
	fail bool
}

func (s *councilFailOnceStore) CreateCouncil(ctx context.Context, room domain.ConclaveSession, spec store.CouncilSpec) error {
	if s.fail {
		s.fail = false
		return errors.New("fixture write failure")
	}
	return s.CouncilStore.CreateCouncil(ctx, room, spec)
}

func TestCouncilDetailOverlappingAnswer(t *testing.T) {
	for _, phase := range []string{"snapshot older than answer", "message scan older than answer"} {
		t.Run(phase, func(t *testing.T) {
			st := &councilDetailOverlapStore{
				phase:  phase,
				answer: domain.ConclaveMessage{ID: 1, ConclaveID: "room", FromRole: "claude", TaskID: "task"},
			}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/conclaves/room", nil)
			req.SetPathValue("id", "room")
			w := httptest.NewRecorder()
			councilDetail(st, http.NotFound)(w, req)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"round":2`) {
				t.Fatalf("detail lost the answer's round: %d %s", w.Code, w.Body)
			}
		})
	}
}

type councilDetailOverlapStore struct {
	store.Store
	store.CouncilStore
	phase   string
	answer  domain.ConclaveMessage
	scanned bool
}

func (s *councilDetailOverlapStore) ListConclaveMessages(context.Context, string, int64) ([]domain.ConclaveMessage, error) {
	s.scanned = true
	if s.phase == "message scan older than answer" {
		return nil, nil
	}
	return []domain.ConclaveMessage{s.answer}, nil
}

func (s *councilDetailOverlapStore) CouncilSnapshot(context.Context, string) (store.CouncilSnapshot, error) {
	snap := store.CouncilSnapshot{Session: domain.ConclaveSession{ID: "room"}}
	if s.scanned || s.phase == "message scan older than answer" {
		snap.Attempts = []store.CouncilAttempt{{Round: 2, Answer: &s.answer}}
	}
	return snap, nil
}

func (s *councilDetailOverlapStore) ListConclaveRelics(context.Context, string) ([]domain.ConclaveRelic, error) {
	return nil, nil
}

func TestCouncilDetailAfterSixStarts(t *testing.T) {
	repo, _, token := councilTestInputs(t)
	st := daemonTestStore(t)
	bundle := &config.Bundle{Workloads: map[string]domain.Workload{}}
	for _, role := range []string{"claude", "codex", "agy"} {
		bundle.Workloads[role] = domain.Workload{
			Metadata: domain.ObjectMeta{Name: role, Zone: domain.ZoneSandbox},
			Spec: domain.WorkloadSpec{Class: domain.ClassCodingAgent,
				Coding: &domain.CodingConfig{Harness: role, RepoPath: repo, Sandbox: "seatbelt"}},
		}
		if err := st.UpsertWorkload(t.Context(), domain.WorkloadRecord{Name: role, Zone: domain.ZoneSandbox, Class: domain.ClassCodingAgent}); err != nil {
			t.Fatal(err)
		}
	}
	adapter := &councilTestAdapter{st: st, commit: councilTestGit(t, repo, "rev-parse", "HEAD"), launches: make(map[string]councilTestLaunch)}
	r := controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle,
		Adapters:     map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: adapter},
		ModelGateURL: "model-gate-fixture", DataGateURL: "data-gate-fixture",
	})
	h := councilTestHandler(st, r, councilTestOptions(token))
	// Pin the room before changing HEAD. The next opening should be free to see a new commit.
	room := councilTestOpen(t, h, token, fmt.Sprintf(`{"question":"Review this","repo":%q,"ref":"HEAD"}`, repo))
	councilTestGit(t, repo, "commit", "--allow-empty", "-qm", "move head")
	b := conclave.NewBroker(conclave.Options{Store: st, Reconciler: r})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		if err := reconcileDaemonPass(ctx, r.ReconcileOnce, b); err != nil {
			t.Fatal(err)
		}
		state, err := st.GetConclave(ctx, room.ID)
		if err != nil {
			t.Fatal(err)
		}
		if state.State == domain.ConclaveStateCompleted {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}

	res := councilTestRequest(h, http.MethodGet, "/api/v1/conclaves/"+room.ID, token, "")
	if res.Code != 200 {
		t.Fatalf("detail returned %d %s", res.Code, res.Body)
	}
	var detail struct {
		Conclave domain.ConclaveSession
		Messages []struct {
			domain.ConclaveMessage
			Round int
		}
		Relics []domain.ConclaveRelic
	}
	if err := json.Unmarshal(res.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Conclave.WardReason != "round_cap" || len(detail.Messages) != 6 || len(detail.Relics) != 1 {
		t.Fatalf("unexpected detail: %+v", detail)
	}
	digest := detail.Relics[0]
	if digest.Name != "digest" || digest.AuthorRole != "system" || digest.SHA256 == "" {
		t.Fatalf("unexpected digest metadata: %+v", digest)
	}
	adapter.mu.Lock()
	launches, starts := maps.Clone(adapter.launches), adapter.starts
	adapter.mu.Unlock()
	if len(launches) != 6 || starts != 6 {
		t.Fatalf("launch log = %d unique starts, %d total", len(launches), starts)
	}
	pairs := make(map[string]bool)
	for _, msg := range detail.Messages {
		launch, ok := launches[msg.TaskID]
		wantBody := fmt.Sprintf("Review from %s in round %d.", launch.role, launch.round)
		if !ok || msg.FromRole != launch.role || msg.Round != launch.round || msg.Body != wantBody || !strings.Contains(digest.Body, wantBody) {
			t.Errorf("answer lacks expected body, attribution, or digest evidence: %+v (want %q)", msg, wantBody)
		}
		pairs[fmt.Sprintf("%d/%s", msg.Round, msg.FromRole)] = true
		if _, err := st.GetActiveLeaseByTokenHash(ctx, launch.tokenHash, time.Now()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("task lease still usable or lookup failed: %v", err)
		}
	}
	for _, role := range []string{"claude", "codex", "agy"} {
		for _, round := range []int{1, 2} {
			if !pairs[fmt.Sprintf("%d/%s", round, role)] {
				t.Errorf("missing %s answer in round %d", role, round)
			}
		}
	}
}

type councilTestLaunch struct {
	role, tokenHash string
	round           int
}

type councilTestAdapter struct {
	scruntime.Adapter
	st       store.Store
	commit   string
	mu       sync.Mutex
	launches map[string]councilTestLaunch
	starts   int
}

func (a *councilTestAdapter) Start(ctx context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	var input struct {
		store.ConclaveAssignment
		Mode   string `json:"checkoutMode"`
		Commit string `json:"checkoutRef"`
	}
	if err := json.Unmarshal([]byte(spec.InputJSON), &input); err != nil {
		return scruntime.Handle{}, err
	}
	if a.commit != input.Commit || input.Mode != "ref" ||
		input.Role != spec.Workload.Metadata.Name || spec.ModelGateURL != "" || spec.ModelGateToken != "" || spec.DataGateURL == "" ||
		!strings.HasPrefix(spec.TaskToken, model.TokenPrefix) {
		return scruntime.Handle{}, errors.New("invalid council launch contract")
	}
	hash := model.HashToken(spec.TaskToken)
	lease, err := a.st.GetActiveLeaseByTokenHash(ctx, hash, time.Now())
	if err != nil || lease.TaskID != spec.TaskID || lease.WorkloadName != input.Role {
		return scruntime.Handle{}, errors.New("invalid task lease scope")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.starts++
	a.launches[spec.TaskID] = councilTestLaunch{input.Role, hash, input.Round}
	return scruntime.Handle{AdapterName: "fixture", ExternalID: spec.TaskID}, nil
}

func (a *councilTestAdapter) StartAtCommit(context.Context, scruntime.LaunchSpec, string, bool) (scruntime.Handle, error) {
	return scruntime.Handle{}, errors.New("StartAtCommit must not be called for council checkoutMode launches")
}

func (a *councilTestAdapter) Observe(context.Context, scruntime.Handle) (scruntime.Observation, error) {
	return scruntime.Observation{State: scruntime.StateCompleted, ObservedAt: time.Now().UTC()}, nil
}

func (a *councilTestAdapter) Stop(context.Context, scruntime.Handle) error { return nil }

func (a *councilTestAdapter) CollectArtifacts(_ context.Context, h scruntime.Handle) ([]scruntime.Artifact, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	launch := a.launches[h.ExternalID]
	return []scruntime.Artifact{{Name: "final-message", ContentType: "text/plain", Text: fmt.Sprintf("Review from %s in round %d.", launch.role, launch.round)}}, nil
}

func councilTestInputs(t *testing.T) (repo, bin, token string) {
	t.Helper()
	repo = t.TempDir()
	councilTestGit(t, repo, "init", "--template=", "-q")
	councilTestGit(t, repo, "commit", "--allow-empty", "-qm", "initial")
	bin = t.TempDir()
	for _, role := range []string{"claude", "codex", "agy"} {
		if err := os.WriteFile(filepath.Join(bin, role), []byte("#!/bin/sh\nexit 99\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	token, err = operatortoken.LoadOrCreate(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	return repo, bin, token
}

func councilTestGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", repo, "-c", "core.fsmonitor=false", "-c", "commit.gpgsign=false", "-c", "user.name=Fixture"}, args...)...)
	env := make([]string, 0, len(os.Environ())+4)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	cmd.Env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "EMAIL=fixture", "GIT_ALLOW_PROTOCOL=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v (%s)", args[0], err, out)
	}
	return strings.TrimSpace(string(out))
}

func councilTestOptions(token string) handlerConfig {
	return handlerConfig{operatorToken: token, checkCouncilSandbox: func(context.Context) error { return nil },
		checkCouncilRuntime: func(context.Context, store.CouncilCheckout) error { return nil }}
}

func councilTestHandler(st store.Store, r *controlplane.Reconciler, options handlerConfig) http.Handler {
	b := conclave.NewBroker(conclave.Options{Store: st, Reconciler: r})
	return newHandler(st, nil, nil, nil, nil, nil, nil, r, nil, b, nil, nil, nil, options)
}

func councilTestRequest(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func councilTestOpen(t *testing.T, h http.Handler, token, body string) domain.ConclaveSession {
	t.Helper()
	res := councilTestRequest(h, http.MethodPost, "/api/v1/councils", token, body)
	if res.Code != 201 {
		t.Fatalf("open returned %d %s", res.Code, res.Body)
	}
	var room domain.ConclaveSession
	if err := json.Unmarshal(res.Body.Bytes(), &room); err != nil || room.ID == "" || room.State != domain.ConclaveStateActive {
		t.Fatalf("missing created council session: %v", err)
	}
	return room
}

func councilAssertEmpty(t *testing.T, st store.Store) {
	t.Helper()
	rooms, roomErr := st.ListConclaves(t.Context())
	tasks, taskErr := st.ListTasks(t.Context())
	if roomErr != nil || taskErr != nil || len(rooms) != 0 || len(tasks) != 0 {
		t.Fatalf("refusal left %d rooms and %d tasks, errors %v, %v", len(rooms), len(tasks), roomErr, taskErr)
	}
}

func TestCouncilRuntimeCheckerAndPanelWorkloads(t *testing.T) {
	st := daemonTestStore(t)
	bundle, err := ensureCouncilPanelWorkloads(t.Context(), st, nil)
	if err != nil {
		t.Fatalf("ensureCouncilPanelWorkloads: %v", err)
	}
	for _, role := range councilPanel {
		wl, ok := bundle.Workloads[role]
		if !ok || wl.Metadata.Zone != domain.ZoneSandbox || wl.Spec.Class != domain.ClassCodingAgent ||
			wl.Spec.Coding == nil || wl.Spec.Coding.Harness != role || wl.Spec.Coding.Sandbox != "seatbelt" ||
			wl.Spec.Grants.ModelGate.Allowed {
			t.Fatalf("unexpected built-in workload for %s: %+v", role, wl)
		}
	}
	repo := t.TempDir()
	adapters := map[domain.WorkloadClass]scruntime.Adapter{
		domain.ClassCodingAgent: &councilTestAdapter{},
	}
	check := councilRuntimeChecker(func() *config.Bundle { return bundle }, adapters)
	if err := check(t.Context(), store.CouncilCheckout{Source: repo}); err != nil {
		t.Fatalf("expected built-in panel to pass runtime check: %v", err)
	}

	if err := councilRuntimeChecker(func() *config.Bundle { return bundle }, nil)(t.Context(), store.CouncilCheckout{Source: repo}); err == nil {
		t.Fatal("expected missing coding adapter to fail runtime check")
	}

	// Model Gate grant on a council workload must fail closed.
	badBundle := &config.Bundle{Workloads: maps.Clone(bundle.Workloads)}
	wl := badBundle.Workloads["claude"]
	wl.Spec.Grants.ModelGate.Allowed = true
	badBundle.Workloads["claude"] = wl
	if err := councilRuntimeChecker(func() *config.Bundle { return badBundle }, adapters)(t.Context(), store.CouncilCheckout{Source: repo}); err == nil {
		t.Fatal("expected Model Gate grant on council panel workload to fail runtime check")
	}

	// Relative RepoPath matching absolute checkout.Source must be accepted.
	relParent := t.TempDir()
	relRepo := filepath.Join(relParent, "project")
	if err := os.Mkdir(relRepo, 0o700); err != nil {
		t.Fatal(err)
	}
	canonRelRepo, err := filepath.EvalSymlinks(relRepo)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(relParent)
	relBundle := &config.Bundle{Workloads: maps.Clone(bundle.Workloads)}
	for _, role := range councilPanel {
		w := relBundle.Workloads[role]
		cfg := *w.Spec.Coding
		cfg.RepoPath = "project"
		w.Spec.Coding = &cfg
		relBundle.Workloads[role] = w
	}
	if err := councilRuntimeChecker(func() *config.Bundle { return relBundle }, adapters)(t.Context(), store.CouncilCheckout{Source: canonRelRepo}); err != nil {
		t.Fatalf("expected relative RepoPath matching absolute checkout.Source to pass: %v", err)
	}
}

func TestCouncilCheckoutAdmissionOwnershipAndPrivatePaths(t *testing.T) {
	_, _, token := councilTestInputs(t)

	initRepo := func(t *testing.T, dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		councilTestGit(t, dir, "init", "--template=", "-q")
		councilTestGit(t, dir, "commit", "--allow-empty", "-qm", "initial")
	}

	t.Run("checkout containing daemon state dir refused", func(t *testing.T) {
		repo := t.TempDir()
		initRepo(t, repo)
		stateDir := filepath.Join(repo, "state")
		t.Setenv("SABLECOURT_STATE_DIR", stateDir)
		if _, err := operatortoken.LoadOrCreate(stateDir); err != nil {
			t.Fatal(err)
		}
		st := daemonTestStore(t)
		h := councilTestHandler(st, nil, councilTestOptions(token))
		body := fmt.Sprintf(`{"question":"Review","repo":%q,"live":true}`, repo)
		res := councilTestRequest(h, http.MethodPost, "/api/v1/councils", token, body)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("POST = %d %s, want 400 rejecting state dir overlap", res.Code, res.Body)
		}
		councilAssertEmpty(t, st)
	})

	t.Run("checkout inside daemon state dir refused", func(t *testing.T) {
		stateDir := filepath.Join(t.TempDir(), "state")
		t.Setenv("SABLECOURT_STATE_DIR", stateDir)
		if _, err := operatortoken.LoadOrCreate(stateDir); err != nil {
			t.Fatal(err)
		}
		repo := filepath.Join(stateDir, "nested-repo")
		initRepo(t, repo)
		st := daemonTestStore(t)
		h := councilTestHandler(st, nil, councilTestOptions(token))
		body := fmt.Sprintf(`{"question":"Review","repo":%q,"live":true}`, repo)
		res := councilTestRequest(h, http.MethodPost, "/api/v1/councils", token, body)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("POST = %d %s, want 400 rejecting checkout inside state dir", res.Code, res.Body)
		}
		councilAssertEmpty(t, st)
	})

	t.Run("checkout overlapping home credential dirs refused", func(t *testing.T) {
		fakeHome := t.TempDir()
		t.Setenv("HOME", fakeHome)
		t.Setenv("SABLECOURT_STATE_DIR", filepath.Join(t.TempDir(), "state"))

		for _, rel := range []string{"", ".ssh", ".aws", ".gnupg", ".config/gh", ".kube"} {
			t.Run("rel="+rel, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				repo := home
				if rel != "" {
					repo = filepath.Join(home, rel)
				}
				initRepo(t, repo)
				st := daemonTestStore(t)
				h := councilTestHandler(st, nil, councilTestOptions(token))
				body := fmt.Sprintf(`{"question":"Review","repo":%q,"live":true}`, repo)
				res := councilTestRequest(h, http.MethodPost, "/api/v1/councils", token, body)
				if res.Code != http.StatusBadRequest {
					t.Fatalf("POST (%s) = %d %s, want 400 rejecting credential path overlap", rel, res.Code, res.Body)
				}
				councilAssertEmpty(t, st)
			})
		}
	})

	t.Run("checkout owned by another user refused", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root")
		}
		st := daemonTestStore(t)
		h := councilTestHandler(st, nil, councilTestOptions(token))
		body := `{"question":"Review","repo":"/usr","live":true}`
		res := councilTestRequest(h, http.MethodPost, "/api/v1/councils", token, body)
		if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "owned") {
			t.Fatalf("POST (/usr) = %d %s, want 400 mentioning ownership", res.Code, res.Body)
		}
		councilAssertEmpty(t, st)
	})
}

func TestReservedCouncilPanelNamesRefused(t *testing.T) {
	for _, role := range councilPanel {
		t.Run("ensureCouncilPanelWorkloads refuses "+role, func(t *testing.T) {
			st := daemonTestStore(t)
			b := &config.Bundle{
				Workloads: map[string]domain.Workload{
					role: {
						Metadata: domain.ObjectMeta{Name: role, Zone: domain.ZoneSandbox},
						Spec: domain.WorkloadSpec{
							Class:  domain.ClassCodingAgent,
							Coding: &domain.CodingConfig{Harness: role, Sandbox: "seatbelt"},
							Grants: domain.GrantSet{MCPTools: []string{"conclave.read"}},
						},
					},
				},
			}
			if _, err := ensureCouncilPanelWorkloads(t.Context(), st, b); err == nil {
				t.Fatalf("ensureCouncilPanelWorkloads accepted manifest workload named %q", role)
			}
		})
	}

	t.Run("runtime checker refuses extra grants on panel workload", func(t *testing.T) {
		st := daemonTestStore(t)
		bundle, err := ensureCouncilPanelWorkloads(t.Context(), st, nil)
		if err != nil {
			t.Fatal(err)
		}
		bad := &config.Bundle{Workloads: maps.Clone(bundle.Workloads)}
		wl := bad.Workloads["claude"]
		wl.Spec.Grants.MCPTools = []string{"conclave.read"}
		bad.Workloads["claude"] = wl
		adapters := map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: &councilTestAdapter{}}
		if err := councilRuntimeChecker(func() *config.Bundle { return bad }, adapters)(t.Context(), store.CouncilCheckout{Source: t.TempDir()}); err == nil {
			t.Fatal("expected extra MCP grants on council panel workload to fail runtime check")
		}
	})
}

func TestCouncilOpenIdempotency(t *testing.T) {
	repo, _, token := councilTestInputs(t)
	bodyA := fmt.Sprintf(`{"question":"Review A","repo":%q,"ref":"HEAD"}`, repo)
	bodyB := fmt.Sprintf(`{"question":"Review B","repo":%q,"ref":"HEAD"}`, repo)

	postWithKey := func(h http.Handler, key, body string, sendKey bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/councils", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		if sendKey {
			r.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	t.Run("same key and payload returns first council", func(t *testing.T) {
		st := daemonTestStore(t)
		h := councilTestHandler(st, nil, councilTestOptions(token))
		r1 := postWithKey(h, "idem-key-1", bodyA, true)
		if r1.Code != http.StatusCreated {
			t.Fatalf("first POST = %d %s", r1.Code, r1.Body)
		}
		var first domain.ConclaveSession
		if err := json.Unmarshal(r1.Body.Bytes(), &first); err != nil || first.ID == "" {
			t.Fatalf("decode first: %v", err)
		}
		r2 := postWithKey(h, "idem-key-1", bodyA, true)
		if r2.Code != http.StatusOK && r2.Code != http.StatusCreated {
			t.Fatalf("second POST = %d %s", r2.Code, r2.Body)
		}
		var second domain.ConclaveSession
		if err := json.Unmarshal(r2.Body.Bytes(), &second); err != nil {
			t.Fatalf("decode second: %v", err)
		}
		if second.ID != first.ID {
			t.Fatalf("repeated POST created %q instead of returning %q", second.ID, first.ID)
		}
		rooms, err := st.ListConclaves(t.Context())
		if err != nil || len(rooms) != 1 {
			t.Fatalf("rooms = %d, err = %v, want 1", len(rooms), err)
		}
	})

	t.Run("same key with different payload refused", func(t *testing.T) {
		st := daemonTestStore(t)
		h := councilTestHandler(st, nil, councilTestOptions(token))
		r1 := postWithKey(h, "idem-key-2", bodyA, true)
		if r1.Code != http.StatusCreated {
			t.Fatalf("first POST = %d %s", r1.Code, r1.Body)
		}
		r2 := postWithKey(h, "idem-key-2", bodyB, true)
		if r2.Code != http.StatusConflict {
			t.Fatalf("second POST with different payload = %d %s, want 409", r2.Code, r2.Body)
		}
		rooms, err := st.ListConclaves(t.Context())
		if err != nil || len(rooms) != 1 {
			t.Fatalf("rooms = %d, err = %v, want 1", len(rooms), err)
		}
	})

	t.Run("no key creates a new council each time", func(t *testing.T) {
		st := daemonTestStore(t)
		h := councilTestHandler(st, nil, councilTestOptions(token))
		r1 := postWithKey(h, "", bodyA, false)
		r2 := postWithKey(h, "", bodyA, false)
		if r1.Code != http.StatusCreated || r2.Code != http.StatusCreated {
			t.Fatalf("POSTs without key = %d, %d", r1.Code, r2.Code)
		}
		var first, second domain.ConclaveSession
		_ = json.Unmarshal(r1.Body.Bytes(), &first)
		_ = json.Unmarshal(r2.Body.Bytes(), &second)
		if first.ID == "" || second.ID == "" || first.ID == second.ID {
			t.Fatalf("expected two distinct councils without key, got %q and %q", first.ID, second.ID)
		}
		rooms, err := st.ListConclaves(t.Context())
		if err != nil || len(rooms) != 2 {
			t.Fatalf("rooms = %d, err = %v, want 2", len(rooms), err)
		}
	})

	t.Run("blank key refused", func(t *testing.T) {
		st := daemonTestStore(t)
		h := councilTestHandler(st, nil, councilTestOptions(token))
		r := postWithKey(h, "   ", bodyA, true)
		if r.Code != http.StatusBadRequest {
			t.Fatalf("blank Idempotency-Key = %d %s, want 400", r.Code, r.Body)
		}
		councilAssertEmpty(t, st)
	})
}
