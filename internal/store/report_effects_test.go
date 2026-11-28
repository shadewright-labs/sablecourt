package store_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func TestReportRelicEffectFollowsCommit(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "report.db")
	st, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	limits := store.ConclaveLimits{MaxMessages: 10, MaxPingPong: 10, MaxRuns: 6, Now: func() time.Time { return now }}
	msg := domain.ConclaveMessage{ConclaveID: "room", FromRole: "verifier", ToRole: "operator", Kind: "review", Body: "checked", CreatedAt: now}
	relic := domain.ConclaveRelic{ConclaveID: "room", Name: "report.txt", AuthorRole: "verifier", ContentType: "text/plain", Body: "ok", SHA256: relicHash("ok"), CreatedAt: now}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER fail_report BEFORE INSERT ON conclave_messages
		WHEN NEW.kind = 'review' BEGIN SELECT RAISE(ABORT, 'message insert failed'); END`); err != nil {
		t.Fatal(err)
	}
	rctx, effects := store.TrackEffects(ctx)
	if _, _, err := store.AppendConclaveReport(rctx, st, msg, limits, &relic); err == nil {
		t.Fatal("report stored despite failing message insert")
	}
	if got := effects.List(); len(got) != 0 {
		t.Fatalf("rolled-back report recorded effects %q", got)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_report`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AppendConclaveReport(ctx, st, msg, limits, &relic); err != nil {
		t.Fatal(err)
	}
	rctx, effects = store.TrackEffects(ctx)
	if _, _, err := store.AppendConclaveReport(rctx, st, msg, limits, &relic); err != nil {
		t.Fatal(err)
	}
	for _, e := range effects.List() {
		if e == "relic report.txt was written to conclave room" {
			t.Fatalf("unchanged relic recorded as written: %q", effects.List())
		}
	}
}
