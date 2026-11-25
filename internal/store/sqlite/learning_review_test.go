package sqlite

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func seedTaskEvidence(t *testing.T, st *SQLiteStore, id, name string) {
	t.Helper()
	ctx := t.Context()
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "reviewer", AgentCardJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal([]map[string]string{{"name": name, "text": "recorded observation"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: id, IdempotencyKey: id, WorkloadName: "reviewer", State: domain.TaskStateCompleted, ArtifactsJSON: string(raw)}); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptanceEvidence(t *testing.T) {
	for _, tc := range []struct{ name, source, ref string }{
		{"absent", "", ""}, {"unknown task", "missing", "artifact:record"}, {"unknown artifact", "source", "artifact:missing"}, {"unrelated task", "other", "artifact:record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ls := openLearningStore(t)
			seedTaskEvidence(t, st, "source", "record")
			seedTaskEvidence(t, st, "other", "unrelated")
			ctx := t.Context()
			l, err := ls.ProposeLearning(ctx, domain.Learning{ID: "proposal", Author: "reviewer", Statement: "Check observation", Scope: domain.LearningScope{Global: true}, SourceKind: domain.LearningSourceTask, SourceID: tc.source, EvidenceRef: tc.ref})
			if err != nil {
				t.Fatal(err)
			}
			if l.ProvenanceVerified {
				t.Error("marked missing evidence verified")
			}
			if _, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator"), l.ID, l.VersionDigest, time.Now()); err == nil {
				t.Error("accepted missing evidence")
			}
			got, err := ls.GetLearning(ctx, l.ID)
			if err != nil || got.Status != domain.LearningStatusProposed {
				t.Fatalf("status %s, error %v", got.Status, err)
			}
		})
	}
}

func reviewProposal(t *testing.T, st *SQLiteStore, scope domain.LearningScope, now time.Time) domain.Learning {
	t.Helper()
	seedTaskEvidence(t, st, "source", "record")
	l, err := st.ProposeLearning(t.Context(), domain.Learning{ID: "proposal", Author: "reviewer", Statement: "Check observation", Scope: scope, SourceKind: domain.LearningSourceTask, SourceID: "source", EvidenceRef: "artifact:record", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestAcceptanceIdentity(t *testing.T) {
	st, _ := openLearningStore(t)
	now := time.Now()
	l := reviewProposal(t, st, domain.LearningScope{Global: true}, now)
	if _, err := st.AcceptLearning(t.Context(), l.ID, l.VersionDigest, now); err == nil {
		t.Fatal("caller label authorized acceptance")
	}
}

func TestOutcomeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   domain.LearningOutcomeKind
		ref    string
		offset time.Duration
	}{
		{"early confirmation", domain.LearningOutcomeSupporting, "artifact:record", -time.Second},
		{"simultaneous confirmation", domain.LearningOutcomeSupporting, "artifact:record", 0},
		{"future confirmation", domain.LearningOutcomeSupporting, "artifact:record", 3 * time.Hour},
		{"invented contradiction", domain.LearningOutcomeContradiction, "artifact:missing", time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := openLearningStore(t)
			ctx := t.Context()
			now := time.Now().UTC()
			l := reviewProposal(t, st, domain.LearningScope{Global: true}, now.Add(-time.Hour))
			accepted, err := st.AcceptLearning(store.WithLearningOperator(ctx, "operator"), l.ID, l.VersionDigest, now)
			if err != nil {
				t.Fatal(err)
			}
			out, err := st.RecordLearningOutcome(ctx, domain.LearningOutcome{LearningID: l.ID, Kind: tc.kind, Actor: "reviewer", SourceKind: domain.LearningSourceTask, SourceID: "source", EvidenceRef: tc.ref, CreatedAt: now.Add(tc.offset)})
			if tc.kind == domain.LearningOutcomeSupporting && err == nil {
				_, err = st.ConfirmLearning(store.WithLearningOperator(ctx, "operator"), l.ID, out.ID, now.Add(2*time.Hour))
			}
			if err == nil {
				t.Error("invalid outcome changed learning")
			}
			got, err := st.GetLearning(ctx, l.ID)
			if err != nil || got.Status != domain.LearningStatusAccepted || !got.ExpiresAt.Equal(*accepted.ExpiresAt) {
				t.Fatalf("status %s expiry %v error %v", got.Status, got.ExpiresAt, err)
			}
		})
	}
}

func TestScopedEligibility(t *testing.T) {
	st, ls := openLearningStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	l := reviewProposal(t, st, domain.LearningScope{Cluster: "fixture", Namespace: "review"}, now)
	if _, err := ls.AcceptLearning(store.WithLearningOperator(ctx, "operator"), l.ID, l.VersionDigest, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		filter store.LearningFilter
		want   int
	}{
		{"missing selector", store.LearningFilter{Status: domain.LearningStatusAccepted, IncidentCluster: "fixture", Now: now}, 0},
		{"both selectors", store.LearningFilter{IncidentCluster: "fixture", IncidentNamespace: "review", Now: now}, 1},
		{"expired any status", store.LearningFilter{IncidentCluster: "fixture", IncidentNamespace: "review", Now: now.Add(domain.LearningReviewTTL)}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := ls.ListLearnings(ctx, tc.filter)
			if err != nil || len(rows) != tc.want {
				t.Fatalf("got %d rows, want %d, error %v", len(rows), tc.want, err)
			}
		})
	}
}

func TestRemoteEscaping(t *testing.T) {
	for _, suffix := range []string{"encoded%23name", "encoded%3Fquestion", "escaped%2Fslash", "percent%25sign"} {
		t.Run(suffix, func(t *testing.T) {
			raw := "https://repository.invalid/" + suffix
			got, err := store.NormalizeRepoRemoteURL(raw + ".git")
			if err != nil || got != raw {
				t.Errorf("got %q, want %q, error %v", got, raw, err)
			}
			again, err := store.NormalizeRepoRemoteURL(got)
			if err != nil || got != again {
				t.Fatalf("round trip %q, error %v", again, err)
			}
		})
	}
}

func importReviewRows(t *testing.T, st *SQLiteStore) {
	t.Helper()
	tx, err := st.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := migrateLegacyLearningsTx(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyRows(t *testing.T) {
	for _, tc := range []struct {
		name          string
		keys          []string
		value, author string
	}{
		{"distinct keys", []string{" key", "key"}, "rule", "label"},
		{"label", []string{"key"}, "rule", " label "},
		{"blank", []string{"key"}, " \t ", "label"},
		{"credentials", []string{"key"}, "use postgres://reader:fixture-pass@db.invalid/catalog", "mysql://reader:fixture-pass@db.invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ls := openLearningStore(t)
			for _, key := range tc.keys {
				if err := st.PutMemory(t.Context(), domain.MemoryEntry{Namespace: "learning:global", Key: key, Value: tc.value, Author: tc.author, UpdatedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			importReviewRows(t, st)
			rows, err := ls.ListLearnings(t.Context(), store.LearningFilter{Status: domain.LearningStatusProposed})
			if err != nil || len(rows) != len(tc.keys) {
				t.Fatalf("got %d rows, want %d, error %v", len(rows), len(tc.keys), err)
			}
			for _, row := range rows {
				if tc.name == "distinct keys" && row.LegacyKey != " key" && row.LegacyKey != "key" {
					t.Errorf("lost original key %q", row.LegacyKey)
				}
				if tc.name == "credentials" {
					if strings.Contains(fmt.Sprint(row), "fixture-pass") {
						t.Fatal("connection password persisted")
					}
				} else if row.LegacyAuthor != tc.author {
					t.Errorf("label %q, want %q", row.LegacyAuthor, tc.author)
				}
				if tc.name == "blank" && !row.Scope.NeedsScopeMapping {
					t.Error("blank row escaped quarantine")
				}
			}
		})
	}
}

func TestMalformedLegacyMetadata(t *testing.T) {
	for _, tc := range []struct{ name, author, stamp string }{
		{"missing author", " ", "2026-01-01T00:00:00Z"},
		{"invalid timestamp", "fixture", "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := openLearningStore(t)
			if _, err := st.db.Exec(`INSERT INTO court_memory VALUES ('learning:global','key','rule',?,?)`, tc.author, tc.stamp); err != nil {
				t.Fatal(err)
			}
			importReviewRows(t, st)
			rows, err := st.ListLearnings(t.Context(), store.LearningFilter{Status: domain.LearningStatusProposed})
			if err != nil || len(rows) != 1 || !rows[0].Scope.NeedsScopeMapping ||
				rows[0].LegacyAuthor != tc.author || rows[0].LegacyUpdatedAt != tc.stamp {
				t.Fatalf("quarantine rows %+v, error %v", rows, err)
			}
		})
	}
}

func TestBoundedImport(t *testing.T) {
	st, _ := openLearningStore(t)
	const size = 8 << 20
	if _, err := st.db.Exec(`INSERT INTO court_memory VALUES ('learning:global','large',zeroblob(?),'fixture','')`, size); err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	importReviewRows(t, st)
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated >= size {
		t.Errorf("import allocated %d bytes for a %d-byte value", allocated, size)
	}
}

func TestLearningPathSelectors(t *testing.T) {
	for _, tc := range []struct {
		glob, path string
		want       bool
	}{
		{"nested/**/file?.go", "nested/file1.go", true},
		{"nested/**/file?.go", "nested/a/b/file2.go", true},
		{"nested/*.go", "nested/a/file.go", false},
		{"file.go", "nested/file.go", false},
		{"**", "../secret", false},
		{"**", "/outside", false},
		{"**", "", false},
	} {
		t.Run(tc.glob+"/"+tc.path, func(t *testing.T) {
			if got := store.MatchesLearningPaths([]string{tc.glob}, []string{tc.path}); got != tc.want {
				t.Fatalf("match %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBoundedImportPages(t *testing.T) {
	st, _ := openLearningStore(t)
	const count = 83
	for i := range count {
		if err := st.PutMemory(t.Context(), domain.MemoryEntry{Namespace: "learning:global", Key: fmt.Sprint(i), Value: "rule"}); err != nil {
			t.Fatal(err)
		}
	}
	importReviewRows(t, st)
	var records, history int
	if err := st.db.QueryRow(`SELECT (SELECT count(*) FROM learnings), (SELECT count(*) FROM learning_history)`).Scan(&records, &history); err != nil || records != count || history != count {
		t.Fatalf("imported %d records and %d history rows, want %d each, error %v", records, history, count, err)
	}
}

func TestStoredEvidence(t *testing.T) {
	st, _ := openLearningStore(t)
	ctx := t.Context()
	const council = "review-room"
	const incident = "review-incident"
	room := fmt.Sprintf("ccl-shadewright-%x", sha256.Sum256([]byte("shadewright\x00"+incident)))
	for _, c := range []struct{ id, title string }{{council, "Review"}, {room, "Shadewright Incident " + room}} {
		if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: c.id, Title: c.title, State: domain.ConclaveStateActive}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `INSERT INTO conclave_relics(conclave_id,name,author_role,content_type,body,sha256,created_at)
			VALUES (?,'report','system','text/plain','recorded observation','digest',?)`, c.id, nowRFC3339()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO councils(id,policy) VALUES (?,'{}')`, council); err != nil {
		t.Fatal(err)
	}
	seedTaskEvidence(t, st, "source", "record")
	if err := st.UpdateTaskArtifacts(ctx, "source", `[{"name":"pointer","uri":"external:unknown"}]`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		kind    domain.LearningSourceKind
		id, ref string
		valid   bool
	}{
		{"council", domain.LearningSourceCouncil, council, "relic:report", true},
		{"incident", domain.LearningSourceIncident, incident, "relic:report", true},
		{"unknown relic", domain.LearningSourceCouncil, council, "relic:absent", false},
		{"wrong source", domain.LearningSourceCouncil, room, "relic:report", false},
		{"external pointer", domain.LearningSourceTask, "source", "artifact:pointer", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := st.ProposeLearning(ctx, domain.Learning{ID: tc.name, Scope: domain.LearningScope{Global: true}, Statement: "Check report", Author: "reviewer", SourceKind: tc.kind, SourceID: tc.id, EvidenceRef: tc.ref})
			if err != nil {
				t.Fatal(err)
			}
			_, err = st.AcceptLearning(store.WithLearningOperator(ctx, "operator"), l.ID, l.VersionDigest, time.Now())
			if l.ProvenanceVerified != tc.valid || (err == nil) != tc.valid {
				t.Fatalf("verified %v, acceptance error %v", l.ProvenanceVerified, err)
			}
		})
	}
}
