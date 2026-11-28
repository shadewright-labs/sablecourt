package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	msqlite "modernc.org/sqlite"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/storetest"
)

var (
	faultOnce   sync.Once
	faultDriver *storetest.FaultDriver
)

// openFaultStore opens a migrated store whose commits fail while
// faultDriver.Fail is on.
func openFaultStore(t *testing.T) store.Store {
	t.Helper()
	registerFaultDriver()
	db, err := sql.Open("sqlite-commit-fault", filepath.Join(t.TempDir(), "fault.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, p := range []string{"PRAGMA journal_mode=WAL;", "PRAGMA foreign_keys=ON;", "PRAGMA busy_timeout=5000;"} {
		if _, err := db.Exec(p); err != nil {
			t.Fatal(err)
		}
	}
	s := &SQLiteStore{db: db}
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s
}

// No store method records an effect before the transaction that makes it
// commits: with every commit failing, a write must fail and record nothing.
func TestEffectsFollowCommit(t *testing.T) {
	scenarios := append(storetest.StoreScenarios(), councilScenarios()...)
	scenarios = append(scenarios, learningScenarios()...)
	storetest.CheckEffectsFollowCommit(t, registerFaultDriver(), openFaultStore, scenarios)
}

func registerFaultDriver() *storetest.FaultDriver {
	faultOnce.Do(func() {
		faultDriver = storetest.NewFaultDriver(&msqlite.Driver{})
		sql.Register("sqlite-commit-fault", faultDriver)
	})
	return faultDriver
}

func councilScenarios() []storetest.Scenario {
	open := func(ctx context.Context, st store.Store) error {
		if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneSandbox}); err != nil {
			return err
		}
		room, spec := councilFixture()
		return st.(*SQLiteStore).CreateCouncil(ctx, room, spec)
	}
	task := councilTask("author", 1, 1)
	attempt := func(ctx context.Context, st store.Store) error {
		limits := store.ConclaveLimits{Now: time.Now, MaxMessages: 8, MaxPingPong: 8, MaxRuns: 6}
		_, err := store.SubmitConclaveTurn(ctx, "panel", task.IdempotencyKey, limits, func(ctx context.Context) (domain.Task, error) {
			return task, st.CreateTask(ctx, task)
		})
		return err
	}
	answered := func(ctx context.Context, st store.Store) error {
		if err := open(ctx, st); err != nil {
			return err
		}
		if err := attempt(ctx, st); err != nil {
			return err
		}
		return st.UpdateTaskState(ctx, task.ID, domain.TaskStateSubmitted, domain.TaskStateCompleted, "done", "{}")
	}
	msg := domain.ConclaveMessage{ConclaveID: "panel", TaskID: task.ID, FromRole: "author", ToRole: "all", Kind: "review", Body: "result", CreatedAt: time.Now().UTC()}
	var posted int64
	return []storetest.Scenario{
		{Name: "open council", Methods: []string{"CreateCouncil"}, Commits: true,
			Setup: func(ctx context.Context, st store.Store) error {
				return st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "worker", Zone: domain.ZoneSandbox})
			},
			Run: func(ctx context.Context, st store.Store) error {
				room, spec := councilFixture()
				return st.(*SQLiteStore).CreateCouncil(ctx, room, spec)
			}},
		{Name: "council attempt", Methods: []string{"CreateTask"}, Commits: true, Setup: open, Run: attempt},
		{Name: "council answer", Methods: []string{"RecordCouncilAnswer"}, Commits: true, Setup: answered,
			Run: func(ctx context.Context, st store.Store) error {
				_, _, err := st.(*SQLiteStore).RecordCouncilAnswer(ctx, msg, false)
				return err
			}},
		{Name: "accept council answer", Methods: []string{"AcceptCouncilAnswer"}, Commits: true,
			Setup: func(ctx context.Context, st store.Store) error {
				if err := answered(ctx, st); err != nil {
					return err
				}
				var err error
				posted, err = st.AppendConclaveMessage(ctx, msg)
				return err
			},
			Run: func(ctx context.Context, st store.Store) error {
				return st.(*SQLiteStore).AcceptCouncilAnswer(ctx, task.ID, posted, false)
			}},
		{Name: "council relic", Methods: []string{"PutCouncilRelic"}, Commits: true,
			Setup: func(ctx context.Context, st store.Store) error {
				if err := open(ctx, st); err != nil {
					return err
				}
				return attempt(ctx, st)
			},
			Run: func(ctx context.Context, st store.Store) error {
				return st.(*SQLiteStore).PutCouncilRelic(ctx, task.ID, domain.ConclaveRelic{ConclaveID: "panel", Name: "notes.txt", AuthorRole: "author", Body: "notes", CreatedAt: time.Now().UTC()})
			}},
		{Name: "close council", Methods: []string{"CloseCouncil"}, Commits: true, Setup: open,
			Run: func(ctx context.Context, st store.Store) error {
				_, err := st.(*SQLiteStore).CloseCouncil(ctx, "panel", domain.ConclaveStateCancelled, "owner_stop")
				return err
			}},
		{Name: "stop council", Methods: []string{"UpdateConclaveState"}, Commits: true, Setup: open,
			Run: func(ctx context.Context, st store.Store) error {
				return st.UpdateConclaveState(ctx, "panel", domain.ConclaveStateCancelled, "")
			}},
		{Name: "council cleanup", Methods: []string{"MarkCouncilCleanedUp"},
			Setup: func(ctx context.Context, st store.Store) error {
				if err := open(ctx, st); err != nil {
					return err
				}
				_, err := st.(*SQLiteStore).db.ExecContext(ctx, "UPDATE councils SET cleanup_pending = 1")
				return err
			},
			Run: func(ctx context.Context, st store.Store) error {
				return st.(*SQLiteStore).MarkCouncilCleanedUp(ctx, "panel")
			}},
	}
}

func learningScenarios() []storetest.Scenario {
	base := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	operator := func(ctx context.Context) context.Context { return store.WithLearningOperator(ctx, "operator") }
	var proposed domain.Learning
	var outcome int64
	evidence := func(ctx context.Context, st store.Store) error {
		if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "reviewer", AgentCardJSON: "{}"}); err != nil {
			return err
		}
		raw, err := json.Marshal([]map[string]string{{"name": "record", "text": "recorded observation"}})
		if err != nil {
			return err
		}
		return st.CreateTask(ctx, domain.Task{ID: "source", IdempotencyKey: "source", WorkloadName: "reviewer", State: domain.TaskStateCompleted, ArtifactsJSON: string(raw)})
	}
	submit := func(ctx context.Context, st store.Store) error {
		var err error
		proposed, err = st.(*SQLiteStore).ProposeLearning(ctx, domain.Learning{ID: "proposal", Author: "reviewer", Statement: "Check observation",
			Scope: domain.LearningScope{Global: true}, SourceKind: domain.LearningSourceTask, SourceID: "source", EvidenceRef: "artifact:record", CreatedAt: base.Add(-time.Hour)})
		return err
	}
	propose := func(ctx context.Context, st store.Store) error {
		if err := evidence(ctx, st); err != nil {
			return err
		}
		return submit(ctx, st)
	}
	accepted := func(ctx context.Context, st store.Store) error {
		if err := propose(ctx, st); err != nil {
			return err
		}
		var err error
		proposed, err = st.(*SQLiteStore).AcceptLearning(operator(ctx), proposed.ID, proposed.VersionDigest, base)
		return err
	}
	supportingOutcome := func() domain.LearningOutcome { return supporting(proposed, base.Add(time.Hour)) }
	return []storetest.Scenario{
		{Name: "propose learning", Methods: []string{"ProposeLearning"}, Commits: true, Setup: evidence, Run: submit},
		{Name: "accept learning", Methods: []string{"AcceptLearning"}, Commits: true, Setup: propose,
			Run: func(ctx context.Context, st store.Store) error {
				_, err := st.(*SQLiteStore).AcceptLearning(operator(ctx), proposed.ID, proposed.VersionDigest, base)
				return err
			}},
		{Name: "learning outcome", Methods: []string{"RecordLearningOutcome"}, Commits: true, Setup: accepted,
			Run: func(ctx context.Context, st store.Store) error {
				_, err := st.(*SQLiteStore).RecordLearningOutcome(operator(ctx), supportingOutcome())
				return err
			}},
		{Name: "confirm learning", Methods: []string{"ConfirmLearning"}, Commits: true,
			Setup: func(ctx context.Context, st store.Store) error {
				if err := accepted(ctx, st); err != nil {
					return err
				}
				out, err := st.(*SQLiteStore).RecordLearningOutcome(operator(ctx), supportingOutcome())
				outcome = out.ID
				return err
			},
			Run: func(ctx context.Context, st store.Store) error {
				_, err := st.(*SQLiteStore).ConfirmLearning(operator(ctx), proposed.ID, outcome, base.Add(2*time.Hour))
				return err
			}},
		{Name: "retire learning", Methods: []string{"RetireLearning"}, Commits: true, Setup: accepted,
			Run: func(ctx context.Context, st store.Store) error {
				_, err := st.(*SQLiteStore).RetireLearning(operator(ctx), proposed.ID, "obsolete", base.Add(time.Hour))
				return err
			}},
		{Name: "learning injection", Methods: []string{"RecordLearningInjection"}, Commits: true, Setup: accepted,
			Run: func(ctx context.Context, st store.Store) error {
				injected, err := st.(*SQLiteStore).RecordLearningInjection(ctx, proposed.ID, "packet", "source", base.Add(time.Hour))
				if err == nil && !injected {
					err = errors.New("not injected")
				}
				return err
			}},
		{Name: "expire learnings", Methods: []string{"ExpireLearnings"}, Commits: true, Setup: accepted,
			Run: func(ctx context.Context, st store.Store) error {
				n, err := st.(*SQLiteStore).ExpireLearnings(ctx, base.Add(domain.LearningReviewTTL+time.Hour))
				if err == nil && n == 0 {
					err = errors.New("nothing expired")
				}
				return err
			}},
	}
}
