package sqlite

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func TestApprovalDecisionIsFinal(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	base := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "task", IdempotencyKey: "task", WorkloadName: "worker", State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"ap-1", "ap-2", "ap-3"} {
		if err := st.CreateApproval(ctx, domain.Approval{
			ID: id, TaskID: "task", WorkloadName: "worker", Capability: "git.push",
			PayloadCanonicalJSON: `{"n":` + fmt.Sprint(i) + `}`, PayloadSHA256: "sha-" + id,
			ExpiresAt: base.Add(time.Hour), CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DecideApproval(ctx, "ap-1", "sha-other", domain.ApprovalApproved, "operator", `{"sig":"x"}`); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("decision against another payload digest = %v", err)
	}
	if err := st.DecideApproval(ctx, "ap-missing", "sha-ap-1", domain.ApprovalApproved, "operator", ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("decision on a missing approval = %v", err)
	}
	if err := st.DecideApproval(ctx, "ap-1", "sha-ap-1", domain.ApprovalApproved, "operator", `{"sig":"x"}`); err != nil {
		t.Fatal(err)
	}
	if err := st.DecideApproval(ctx, "ap-2", "sha-ap-2", domain.ApprovalDenied, "operator", ""); err != nil {
		t.Fatal(err)
	}
	// A decided approval cannot be decided again, in either direction.
	for _, status := range []domain.ApprovalStatus{domain.ApprovalDenied, domain.ApprovalApproved, domain.ApprovalPending} {
		if err := st.DecideApproval(ctx, "ap-1", "sha-ap-1", status, "intruder", "{}"); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("re-deciding as %s = %v", status, err)
		}
	}
	got, err := st.GetApproval(ctx, "ap-1")
	if err != nil || got.Status != domain.ApprovalApproved || got.DecidedBy != "operator" || got.ProofJSON != `{"sig":"x"}` || got.DecidedAt == nil {
		t.Fatalf("decided approval = %+v, %v", got, err)
	}
	if denied, _ := st.GetApproval(ctx, "ap-2"); denied.ProofJSON != "{}" {
		t.Fatalf("empty proof stored as %q", denied.ProofJSON)
	}
	if _, err := st.GetApproval(ctx, "ap-missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing approval = %v", err)
	}

	for _, tc := range []struct {
		status domain.ApprovalStatus
		want   []string
	}{
		{"", []string{"ap-3", "ap-2", "ap-1"}},
		{domain.ApprovalPending, []string{"ap-3"}},
		{domain.ApprovalApproved, []string{"ap-1"}},
		{domain.ApprovalDenied, []string{"ap-2"}},
		{domain.ApprovalExpired, nil},
	} {
		list, err := st.ListApprovals(ctx, tc.status)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, a := range list {
			ids = append(ids, a.ID)
			if a.Status == domain.ApprovalPending && a.DecidedAt != nil {
				t.Fatalf("pending approval has a decision time: %+v", a)
			}
			if a.ExpiresAt.IsZero() || a.CreatedAt.IsZero() {
				t.Fatalf("listed approval lost its times: %+v", a)
			}
		}
		if fmt.Sprint(ids) != fmt.Sprint(tc.want) {
			t.Fatalf("ListApprovals(%q) = %v, want %v", tc.status, ids, tc.want)
		}
	}
}

func TestDeviceRevocationSticks(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	base := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	for i, id := range []string{"phone", "laptop"} {
		if err := st.CreateDevice(ctx, domain.PairedDevice{ID: id, Name: id, Platform: "cli", PublicKeyPEM: "key-" + id, CreatedAt: base.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateDevice(ctx, domain.PairedDevice{ID: "phone", Name: "again", CreatedAt: base}); err == nil {
		t.Fatal("duplicate device id accepted")
	}
	if err := st.RevokeDevice(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoking a missing device = %v", err)
	}
	if _, err := st.GetDevice(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing device = %v", err)
	}
	if err := st.RevokeDevice(ctx, "phone"); err != nil {
		t.Fatal(err)
	}
	first, err := st.GetDevice(ctx, "phone")
	if err != nil || first.RevokedAt == nil || first.PublicKeyPEM != "key-phone" {
		t.Fatalf("revoked device = %+v, %v", first, err)
	}
	// Revoking again keeps the device revoked.
	if err := st.RevokeDevice(ctx, "phone"); err != nil {
		t.Fatal(err)
	}
	again, err := st.GetDevice(ctx, "phone")
	if err != nil || again.RevokedAt == nil {
		t.Fatalf("device after a second revocation = %+v, %v", again, err)
	}
	devices, err := st.ListDevices(ctx)
	if err != nil || len(devices) != 2 {
		t.Fatalf("devices = %+v, %v", devices, err)
	}
	if devices[0].ID != "laptop" || devices[0].RevokedAt != nil || devices[1].ID != "phone" || devices[1].RevokedAt == nil {
		t.Fatalf("listed devices = %+v", devices)
	}
	if !devices[0].CreatedAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("created time = %v", devices[0].CreatedAt)
	}
}

func TestMigrateOnReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var applied int
	if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "kept", IdempotencyKey: "kept", WorkloadName: "worker", State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for range 2 {
		if err := reopened.Migrate(ctx); err != nil {
			t.Fatalf("migrate on reopen: %v", err)
		}
	}
	var after int
	if err := reopened.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != applied || applied == 0 {
		t.Fatalf("migrations applied %d, after reopen %d", applied, after)
	}
	if task, err := reopened.GetTask(ctx, "kept"); err != nil || task.State != domain.TaskStateWorking {
		t.Fatalf("task after reopen = %+v, %v", task, err)
	}
}

func TestListingsReturnStoredRows(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "t", IdempotencyKey: "t", WorkloadName: "worker", State: domain.TaskStateSubmitted}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTaskState(ctx, "t", domain.TaskStateSubmitted, domain.TaskStateWorking, "claimed", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTaskState(ctx, "t", domain.TaskStateSubmitted, domain.TaskStateFailed, "stale", ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("transition from a stale state = %v", err)
	}
	events, err := st.ListTaskEvents(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Reason != "claimed" || last.FromState != domain.TaskStateSubmitted || last.ToState != domain.TaskStateWorking || last.FactJSON != "{}" {
		t.Fatalf("events = %+v", events)
	}
	for _, e := range events {
		if e.Reason == "stale" {
			t.Fatal("refused transition recorded an event")
		}
	}
	for _, m := range []domain.TaskMessage{{TaskID: "t", Role: "user", Content: "hello"}, {TaskID: "t", Role: "agent", Content: "hi"}} {
		if err := st.AppendTaskMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := st.ListTaskMessages(ctx, "t")
	if err != nil || len(msgs) != 2 || msgs[0].Content != "hello" || msgs[1].Role != "agent" {
		t.Fatalf("messages = %+v, %v", msgs, err)
	}

	if err := st.UpsertProviderPrefs(ctx, domain.ProviderPrefs{ProviderID: "p1", DefaultModel: "m1", EnabledModels: []string{"m1", "m2"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertProviderPrefs(ctx, domain.ProviderPrefs{ProviderID: "p1", DefaultModel: "m2", EnabledModels: []string{"m2"}}); err != nil {
		t.Fatal(err)
	}
	prefs, err := st.ListProviderPrefs(ctx)
	if err != nil || len(prefs) != 1 || prefs["p1"].DefaultModel != "m2" || fmt.Sprint(prefs["p1"].EnabledModels) != "[m2]" {
		t.Fatalf("provider prefs = %+v, %v", prefs, err)
	}

	now := time.Now().UTC()
	for i, id := range []string{"c1", "c2"} {
		if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: id, Title: "room " + id, State: domain.ConclaveStateActive, CreatedAt: now.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpdateConclaveState(ctx, "c1", domain.ConclaveStateCompleted, "done"); err != nil {
		t.Fatal(err)
	}
	rooms, err := st.ListConclaves(ctx)
	if err != nil || len(rooms) != 2 {
		t.Fatalf("rooms = %+v, %v", rooms, err)
	}
	byID := map[string]domain.ConclaveSession{}
	for _, r := range rooms {
		byID[r.ID] = r
	}
	if byID["c1"].State != domain.ConclaveStateCompleted || byID["c1"].WardReason != "done" || byID["c2"].Title != "room c2" {
		t.Fatalf("rooms = %+v", rooms)
	}
	if err := st.UpdateConclaveState(ctx, "missing", domain.ConclaveStateCompleted, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("closing a missing room = %v", err)
	}
	for _, name := range []string{"b.txt", "a.txt"} {
		if err := st.PutConclaveRelic(ctx, domain.ConclaveRelic{ConclaveID: "c2", Name: name, AuthorRole: "r", ContentType: "text/plain", Body: name,
			SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(name))), CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	relics, err := st.ListConclaveRelics(ctx, "c2")
	if err != nil || len(relics) != 2 {
		t.Fatalf("relics = %+v, %v", relics, err)
	}
	if none, err := st.ListConclaveRelics(ctx, "c1"); err != nil || len(none) != 0 {
		t.Fatalf("relics of another room = %+v, %v", none, err)
	}
}

func TestCloseCouncil(t *testing.T) {
	s := councilDB(t, filepath.Join(t.TempDir(), "state.db"))
	ctx := t.Context()
	room, spec := councilFixture()
	spec.IdempotencyKey, spec.RequestSHA256 = "open-1", "req-digest"
	if err := s.CreateCouncil(ctx, room, spec); err != nil {
		t.Fatal(err)
	}
	got, digest, err := s.CouncilByIdempotencyKey(ctx, "open-1")
	if err != nil || got.ID != room.ID || digest != "req-digest" || got.State != domain.ConclaveStateActive {
		t.Fatalf("council by key = %+v, %q, %v", got, digest, err)
	}
	for _, key := range []string{"", "open-2"} {
		if _, _, err := s.CouncilByIdempotencyKey(ctx, key); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("council by key %q = %v", key, err)
		}
	}

	if err := s.CreateConclave(ctx, domain.ConclaveSession{ID: "plain", State: domain.ConclaveStateActive, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, id, reason string
		state            domain.ConclaveState
		err              error
	}{
		{"blank reason", room.ID, " ", domain.ConclaveStateCompleted, nil},
		{"non terminal state", room.ID, "done", domain.ConclaveStateInputRequired, nil},
		{"blank id", "", "done", domain.ConclaveStateCompleted, nil},
		{"secret id", "ghp_" + strings.Repeat("a1B2", 9), "done", domain.ConclaveStateCompleted, nil},
		{"plain room", "plain", "done", domain.ConclaveStateCompleted, store.ErrNotFound},
		{"missing room", "missing", "done", domain.ConclaveStateCompleted, store.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CloseCouncil(ctx, tc.id, tc.state, tc.reason)
			if err == nil || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Fatalf("CloseCouncil = %v, want %v", err, tc.err)
			}
		})
	}
	if c, _ := s.GetConclave(ctx, room.ID); c.State != domain.ConclaveStateActive {
		t.Fatalf("refused close changed the room: %+v", c)
	}

	relic, err := s.CloseCouncil(ctx, room.ID, domain.ConclaveStateCompleted, "operator_done")
	if err != nil || relic.Name != store.CouncilDigestRelicName || relic.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(relic.Body))) {
		t.Fatalf("close = %+v, %v", relic, err)
	}
	if c, _ := s.GetConclave(ctx, room.ID); c.State != domain.ConclaveStateCompleted || c.WardReason != "operator_done" {
		t.Fatalf("closed room = %+v", c)
	}
	// Closing again returns the stored digest, even with another state or reason.
	again, err := s.CloseCouncil(ctx, room.ID, domain.ConclaveStateCancelled, "other")
	if err != nil || again.Body != relic.Body || again.SHA256 != relic.SHA256 {
		t.Fatalf("second close = %+v, %v", again, err)
	}
	// The operator path requires the same terminal state and reason.
	if err := s.UpdateConclaveState(ctx, room.ID, domain.ConclaveStateCompleted, "operator_done"); err != nil {
		t.Fatalf("matching close through UpdateConclaveState = %v", err)
	}
	if err := s.UpdateConclaveState(ctx, room.ID, domain.ConclaveStateCancelled, ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("mismatched close through UpdateConclaveState = %v", err)
	}
	// The schema refuses to rewrite a closing digest.
	tamper := "UPDATE conclave_relics SET body = 'forged' WHERE conclave_id = ? AND name = ?"
	if _, err := s.db.ExecContext(ctx, tamper, room.ID, store.CouncilDigestRelicName); err == nil {
		t.Fatal("closing digest rewritten")
	}
	// A digest tampered with outside the schema guard is refused, not served.
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER council_digest_immutable"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, tamper, room.ID, store.CouncilDigestRelicName); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseCouncil(ctx, room.ID, domain.ConclaveStateCompleted, "operator_done"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("close over a tampered digest = %v", err)
	}
}

func TestReconcileCouncilsAndCheckpoints(t *testing.T) {
	s := councilDB(t, filepath.Join(t.TempDir(), "state.db"))
	ctx := t.Context()
	room, spec := councilFixture()
	if err := s.CreateCouncil(ctx, room, spec); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateConclave(ctx, domain.ConclaveSession{ID: "plain", State: domain.ConclaveStateActive, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ids, err := s.ListReconcileCouncils(ctx)
	if err != nil || fmt.Sprint(ids) != "[panel]" {
		t.Fatalf("open councils = %v, %v", ids, err)
	}
	if _, err := s.CloseCouncil(ctx, room.ID, domain.ConclaveStateCompleted, "done"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkCouncilCleanedUp(ctx, room.ID); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.ListReconcileCouncils(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("closed and cleaned councils = %v, %v", ids, err)
	}

	if _, err := s.LatestCheckpoint(ctx, "task"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("checkpoint before any save = %v", err)
	}
	if err := s.CreateTask(ctx, domain.Task{ID: "task", IdempotencyKey: "task", WorkloadName: "worker", State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{`{"step":1}`, ""} {
		if err := s.SaveCheckpoint(ctx, "task", "worker", ref); err != nil {
			t.Fatal(err)
		}
	}
	if ref, err := s.LatestCheckpoint(ctx, "task"); err != nil || ref != "{}" {
		t.Fatalf("latest checkpoint = %q, %v", ref, err)
	}
}
