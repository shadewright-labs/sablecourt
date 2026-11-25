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
		{name: "missing ref", body: strings.TrimSuffix(valid, `,"ref":"HEAD"`), want: 400, message: "ref"},
		{name: "blank ref", body: strings.Replace(valid, "HEAD", " ", 1), want: 400, message: "ref"},
		{name: "invalid ref", body: strings.Replace(valid, "HEAD", "missing", 1), want: 400, message: "ref"},
		{name: "qualified branch cannot resolve as tag", body: strings.Replace(valid, "HEAD", "refs/heads/ghost", 1), want: 400, message: "ref"},
		{name: "git revision expression", body: strings.Replace(valid, "HEAD", "HEAD^{commit}", 1), want: 400, message: "ref"},
		{name: "live checkout", body: strings.TrimSuffix(valid, `,"ref":"HEAD"`) + `,"live":true`, want: 400, message: "live"},
		{name: "ref and live", body: valid + `,"live":true`, want: 400, message: "live"},
		{name: "writer checkout", body: valid + `,"writer":"claude"`, want: 400, message: "writer"},
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
	for _, tc := range []struct {
		name, fields   string
		rounds, starts int
		duration       time.Duration
		roles          string
		sequential     bool
	}{
		{"defaults", "", 2, 6, 15 * time.Minute, "claude,codex,agy", false},
		{"client fields", `,"title":"Review this","decree":"Review this","rounds":1,"starts":3,"durationSeconds":7,"with":["agy","claude","codex"],"sequential":true`, 1, 3, 7 * time.Second, "agy,claude,codex", true},
		{"redaction", `,"title":"-----BEGIN PRIVATE KEY----- concealed"`, 2, 6, 15 * time.Minute, "claude,codex,agy", false},
		{"embedded credential", fmt.Sprintf(`,"title":%q`, "prefix"+token+"suffix"), 2, 6, 15 * time.Minute, "claude,codex,agy", false},
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
			body := fmt.Sprintf(`{"question":%q,"repo":%q,"ref":"HEAD"%s}`, question, repo, tc.fields)
			room := councilTestOpen(t, h, token, body)
			cs, _ := store.Councils(st)
			snap, err := cs.CouncilSnapshot(t.Context(), room.ID)
			if err != nil {
				t.Fatal(err)
			}
			if snap.MaxRounds != tc.rounds || snap.MaxStarts != tc.starts || snap.Deadline.Sub(snap.Session.CreatedAt) != tc.duration ||
				snap.Sequential != tc.sequential || snap.Checkout.Commit != councilTestGit(t, repo, "rev-parse", "HEAD") || snap.Checkout.Ref != "HEAD" {
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
		if !ok || msg.FromRole != launch.role || msg.Round != launch.round || !strings.Contains(digest.Body, msg.Body) {
			t.Errorf("answer lacks stored attribution or digest evidence: %+v", msg)
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

func (a *councilTestAdapter) StartAtCommit(ctx context.Context, spec scruntime.LaunchSpec, commit string, readOnly bool) (scruntime.Handle, error) {
	var input struct {
		store.ConclaveAssignment
		Mode   string `json:"checkoutMode"`
		Commit string `json:"checkoutRef"`
	}
	if err := json.Unmarshal([]byte(spec.InputJSON), &input); err != nil {
		return scruntime.Handle{}, err
	}
	if !readOnly || a.commit != commit || input.Commit != commit || input.Mode != "ref" ||
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
	t.Setenv("PATH", bin+string(os.PathListSeparator)+filepath.Dir(git))
	token, err = operatortoken.LoadOrCreate(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	return repo, bin, token
}

func councilTestGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", repo, "-c", "core.fsmonitor=false", "-c", "commit.gpgsign=false", "-c", "user.name=Fixture"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "EMAIL=fixture", "GIT_ALLOW_PROTOCOL=")
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
