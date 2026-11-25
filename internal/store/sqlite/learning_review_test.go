package sqlite

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const testLearningRemoteBase = "https://repository.invalid/"

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
			raw := testLearningRemoteBase + suffix
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

func TestLearningRemoteRoundTrip(t *testing.T) {
	st, _ := openLearningStore(t)
	ctx := t.Context()
	remotes := []string{"plugin.git", "plugin.git.git", "plugin.git.git.git", "a%3Fb.git"}
	for _, remote := range remotes {
		_, err := st.ProposeLearning(ctx, domain.Learning{
			ID: remote, Statement: "Check observation", Author: "reviewer",
			SourceKind: domain.LearningSourceTask, Scope: domain.LearningScope{RepoRemoteURL: testLearningRemoteBase + remote},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, remote := range remotes {
		t.Run(remote, func(t *testing.T) {
			rec, err := st.GetLearning(ctx, remote)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := st.ListLearnings(ctx, store.LearningFilter{
				Status: domain.LearningStatusProposed, RepoRemoteURL: rec.Scope.RepoRemoteURL,
			})
			if err != nil || len(rows) != 1 || rows[0].ID != remote {
				t.Fatalf("lookup of %q returned %d rows, error %v", rec.Scope.RepoRemoteURL, len(rows), err)
			}
		})
	}
}

func TestLearningScopeKeyLookup(t *testing.T) {
	maxGlobs := make([]string, store.MaxLearningPathGlobs)
	for i := range maxGlobs {
		// Nonbreaking spaces inside a glob expand to escape sequences.
		maxGlobs[i] = fmt.Sprintf("%02d%s*", i, strings.Repeat("\u00a0", (store.MaxLearningPathGlobBytes-3)/2))
	}
	for _, tc := range []struct {
		name   string
		scopes []domain.LearningScope
	}{
		{"quoted globs", []domain.LearningScope{{PathGlobs: []string{"a,b"}}, {PathGlobs: []string{`"a`, `b"`}}}},
		{"three long globs", []domain.LearningScope{{PathGlobs: []string{
			"a" + strings.Repeat("x", 199), "b" + strings.Repeat("x", 199), "c" + strings.Repeat("x", 199),
		}}}},
		{"compound scope", []domain.LearningScope{{
			RepoRemoteURL: testLearningRemoteBase + "repo", PathGlobs: maxGlobs,
			Cluster: strings.Repeat("c", store.MaxLearningSelectorBytes), Namespace: strings.Repeat("n", store.MaxLearningSelectorBytes),
			WorkloadKind: strings.Repeat("w", store.MaxLearningSelectorBytes), Symptom: strings.Repeat("s", store.MaxLearningSymptomBytes),
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := openLearningStore(t)
			ctx := t.Context()
			var proposed []domain.Learning
			for i, scope := range tc.scopes {
				rec, err := st.ProposeLearning(ctx, domain.Learning{
					ID: fmt.Sprint(i), Statement: "Check observation", Author: "reviewer",
					SourceKind: domain.LearningSourceTask, Scope: scope,
				})
				if err != nil {
					t.Fatal(err)
				}
				proposed = append(proposed, rec)
			}
			for _, rec := range proposed {
				rows, err := st.ListLearnings(ctx, store.LearningFilter{
					Status: domain.LearningStatusProposed, ScopeKey: rec.ScopeKey,
				})
				if err != nil || len(rows) != 1 || rows[0].ID != rec.ID {
					t.Errorf("exact lookup of %s with %d-byte key returned %d rows, error %v", rec.ID, len(rec.ScopeKey), len(rows), err)
				}
			}
		})
	}
}

func TestLearningReacceptReplacement(t *testing.T) {
	for _, review := range []string{"expiry", "contradiction"} {
		t.Run(review, func(t *testing.T) {
			st, _ := openLearningStore(t)
			ctx := store.WithLearningOperator(t.Context(), "operator")
			now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
			pred := reviewProposal(t, st, domain.LearningScope{Global: true}, now)
			replacement := domain.Learning{
				ID: "replacement", PredecessorID: pred.ID, Scope: pred.Scope, Statement: "Check revised observation",
				Author: pred.Author, SourceKind: pred.SourceKind, SourceID: pred.SourceID, EvidenceRef: pred.EvidenceRef,
				CreatedAt: now,
			}
			first, err := st.ProposeLearning(ctx, replacement)
			if err != nil {
				t.Fatal(err)
			}
			replacement.ID = "other-replacement"
			other, err := st.ProposeLearning(ctx, replacement)
			if err != nil {
				t.Fatal(err)
			}
			accepted, err := st.AcceptLearning(ctx, first.ID, first.VersionDigest, now)
			if err != nil {
				t.Fatal(err)
			}
			reviewAt := now.Add(time.Hour)
			switch review {
			case "expiry":
				reviewAt = *accepted.ExpiresAt
				if n, err := st.ExpireLearnings(ctx, reviewAt); err != nil || n != 1 {
					t.Fatalf("ExpireLearnings = %d, %v", n, err)
				}
			case "contradiction":
				_, err := st.RecordLearningOutcome(ctx, domain.LearningOutcome{
					LearningID: first.ID, Kind: domain.LearningOutcomeContradiction, Actor: "reviewer",
					SourceKind: first.SourceKind, SourceID: first.SourceID, EvidenceRef: first.EvidenceRef, CreatedAt: reviewAt,
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			reacceptAt := reviewAt.Add(time.Minute)
			if _, err := st.AcceptLearning(ctx, other.ID, other.VersionDigest, reacceptAt); !errors.Is(err, store.ErrConflict) {
				t.Errorf("accepting another replacement returned %v, want ErrConflict", err)
			}
			got, err := st.AcceptLearning(ctx, first.ID, first.VersionDigest, reacceptAt)
			if err != nil || got.Status != domain.LearningStatusAccepted || got.ExpiresAt == nil ||
				!got.ExpiresAt.Equal(reacceptAt.Add(domain.LearningReviewTTL)) {
				t.Errorf("reaccepting replacement returned %+v, error %v", got, err)
			}
			history, err := st.ListLearningHistory(ctx, pred.ID)
			if err != nil || len(history) != 2 || history[1].ToStatus != domain.LearningStatusRetired ||
				history[1].SupersededBy != first.ID {
				t.Fatalf("predecessor history = %+v, %v", history, err)
			}
		})
	}
}

func TestLearningConfirmationTiming(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		outcomeAt, priorConfirmation, confirmAt time.Duration
		wantConflict                            bool
	}{
		{"before acceptance", time.Hour, 0, 5 * time.Hour, true},
		{"previous review window", 3 * time.Hour, 4 * time.Hour, 5 * time.Hour, true},
		{"backdated confirmation", 3 * time.Hour, 4 * time.Hour, 3 * time.Hour, true},
		{"after acceptance", 5 * time.Hour, 0, 6 * time.Hour, false},
		{"after renewal", 5 * time.Hour, 4 * time.Hour, 6 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := openLearningStore(t)
			ctx := store.WithLearningOperator(t.Context(), "operator")
			now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
			rec := reviewProposal(t, st, domain.LearningScope{Global: true}, now)
			out := domain.LearningOutcome{
				LearningID: rec.ID, Kind: domain.LearningOutcomeSupporting, Actor: "reviewer",
				SourceKind: rec.SourceKind, SourceID: rec.SourceID, EvidenceRef: rec.EvidenceRef, CreatedAt: now.Add(tc.outcomeAt),
			}
			var outcomeID int64
			if tc.name == "before acceptance" {
				if _, err := st.RecordLearningOutcome(ctx, out); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("recording while proposed returned %v, want ErrConflict", err)
				}
				// Exercise confirmation against a pre-existing outcome the current API rejects.
				res, err := st.db.ExecContext(ctx, `
					INSERT INTO learning_outcomes (learning_id, kind, source_kind, source_id, evidence_ref, actor, created_at)
					VALUES (?, ?, ?, ?, ?, ?, ?)
				`, out.LearningID, out.Kind, out.SourceKind, out.SourceID, out.EvidenceRef, out.Actor, store.FormatLearningTime(out.CreatedAt))
				if err != nil {
					t.Fatal(err)
				}
				outcomeID, err = res.LastInsertId()
				if err != nil {
					t.Fatal(err)
				}
			}
			accepted, err := st.AcceptLearning(ctx, rec.ID, rec.VersionDigest, now.Add(2*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if outcomeID == 0 {
				stored, err := st.RecordLearningOutcome(ctx, out)
				if err != nil {
					t.Fatal(err)
				}
				outcomeID = stored.ID
			}
			if tc.priorConfirmation != 0 {
				out.CreatedAt = now.Add(tc.priorConfirmation)
				newer, err := st.RecordLearningOutcome(ctx, out)
				if err != nil {
					t.Fatal(err)
				}
				accepted, err = st.ConfirmLearning(ctx, rec.ID, newer.ID, out.CreatedAt)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err = st.ConfirmLearning(ctx, rec.ID, outcomeID, now.Add(tc.confirmAt))
			if (tc.wantConflict && !errors.Is(err, store.ErrConflict)) || (!tc.wantConflict && err != nil) {
				t.Errorf("confirmation error = %v, want conflict %v", err, tc.wantConflict)
			}
			got, err := st.GetLearning(ctx, rec.ID)
			wantExpiry := *accepted.ExpiresAt
			if !tc.wantConflict {
				wantExpiry = now.Add(tc.confirmAt).Add(domain.LearningReviewTTL)
			}
			if err != nil || got.ExpiresAt == nil || !got.ExpiresAt.Equal(wantExpiry) {
				t.Errorf("expiry = %v, want %v, error %v", got.ExpiresAt, wantExpiry, err)
			}
		})
	}
}

func TestLearningIncludeExpired(t *testing.T) {
	st, _ := openLearningStore(t)
	ctx := store.WithLearningOperator(t.Context(), "operator")
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	rec := reviewProposal(t, st, domain.LearningScope{Global: true}, now)
	accepted, err := st.AcceptLearning(ctx, rec.ID, rec.VersionDigest, now)
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.ProposeLearning(ctx, domain.Learning{
		ID: "contradicted", Statement: "Check another observation", Scope: rec.Scope, Author: rec.Author,
		SourceKind: rec.SourceKind, SourceID: rec.SourceID, EvidenceRef: rec.EvidenceRef, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcceptLearning(ctx, other.ID, other.VersionDigest, now); err != nil {
		t.Fatal(err)
	}
	// Simulate a persisted contradiction before the status changes.
	if _, err := st.db.ExecContext(ctx, `
		INSERT INTO learning_outcomes (learning_id, kind, source_kind, source_id, evidence_ref, actor, created_at)
		VALUES (?, 'contradiction', 'task', ?, ?, 'reviewer', ?)
	`, other.ID, rec.SourceID, rec.EvidenceRef, store.FormatLearningTime(now.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	for _, status := range []domain.LearningStatus{domain.LearningStatusAccepted, ""} {
		t.Run(string(status)+"_status", func(t *testing.T) {
			rows, err := st.ListLearnings(ctx, store.LearningFilter{Status: status, IncludeExpired: true, Now: *accepted.ExpiresAt})
			if err != nil || len(rows) != 1 || rows[0].ID != rec.ID {
				t.Fatalf("expired lookup returned %v, error %v", rows, err)
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
		{"distinct keys", []string{" key", "key", "key "}, "rule", "label"},
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
			keys := make(map[string]bool)
			for _, row := range rows {
				keys[row.LegacyKey] = true
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
			if tc.name == "distinct keys" {
				for _, key := range tc.keys {
					if !keys[key] {
						t.Errorf("lost original key %q", key)
					}
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
