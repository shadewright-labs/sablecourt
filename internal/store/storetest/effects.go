package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// Scenario drives one or more store writes to their commit on a fresh store.
type Scenario struct {
	Name string
	// Methods names the store methods the scenario calls to write.
	Methods []string
	// Commits says the scenario writes in a transaction on every backend, so
	// its commit must fail while the fault is on.
	Commits bool
	// Err is the error the run returns when nothing fails (nil for success);
	// a refusal that commits a pause returns its refusal.
	Err   error
	Setup func(ctx context.Context, st store.Store) error
	Run   func(ctx context.Context, st store.Store) error
}

// readMethods are the store interface methods that write nothing an operator
// needs to hear about (AppendAudit writes the audit record itself).
var readMethods = map[string]bool{
	"Close": true, "Migrate": true, "AppendAudit": true, "ListAudit": true, "VerifyAuditChain": true,
	"ListWorkloads": true, "GetTask": true, "GetTaskByIdempotencyKey": true, "ListTasks": true,
	"ListReconcileTasks": true, "ListTaskEvents": true, "ListTaskMessages": true,
	"GetApproval": true, "ListApprovals": true, "GetActiveLeaseByTokenHash": true,
	"TaskBudgetTotals": true, "WorkloadDailyBudgetTotals": true, "TotalBudgetTotals": true,
	"LatestCheckpoint": true, "ListProviderPrefs": true, "GetConclave": true, "ListConclaves": true,
	"ListConclaveMessages": true, "GetConclaveRelic": true, "ListConclaveRelics": true,
	"GetDevice": true, "ListDevices": true, "GetMemory": true, "SearchMemory": true,
	"CouncilByIdempotencyKey": true, "ListReconcileCouncils": true, "CouncilSnapshot": true,
	"GetLearning": true, "ListLearnings": true, "ListLearningHistory": true, "ListLearningOutcomes": true,
}

// CheckEffectsFollowCommit runs every scenario on a fresh store from open,
// whose database goes through d, with every commit failing. A scenario whose
// commit failed must return an error and must have recorded no effect, since
// nothing it did is durable; it then runs again with commits working. The
// run that commits (the first one for a write outside a transaction) must
// record at least one effect, so the scenario does reach its write.
// Every write method of the store interfaces the store implements must be
// named by some scenario, so a new write cannot skip the check.
func CheckEffectsFollowCommit(t *testing.T, d *FaultDriver, open func(t *testing.T) store.Store, scenarios []Scenario) {
	t.Helper()
	covered := map[string]bool{}
	for _, sc := range scenarios {
		for _, m := range sc.Methods {
			covered[m] = true
		}
	}
	probe := open(t)
	for _, iface := range []reflect.Type{
		reflect.TypeFor[store.Store](), reflect.TypeFor[store.CouncilStore](), reflect.TypeFor[store.LearningStore](),
	} {
		if !reflect.TypeOf(probe).Implements(iface) {
			continue
		}
		for i := range iface.NumMethod() {
			if name := iface.Method(i).Name; !readMethods[name] && !covered[name] {
				t.Errorf("no scenario writes through %s", name)
			}
		}
	}
	for _, sc := range scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			ctx := t.Context()
			st := open(t)
			if sc.Setup != nil {
				if err := sc.Setup(ctx, st); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			if err := seedTask(ctx, st, flushTask, domain.TaskStateSubmitted); err != nil {
				t.Fatalf("seed flush task: %v", err)
			}
			before := d.Failed()
			fctx, faulted := store.TrackEffects(ctx)
			// The run starts inside effects a caller already holds, as a
			// guarded path or a caller with several writes does. A write that
			// fails must leave nothing in them, so the flush below, which
			// commits on the same context, must release no line of the run.
			fctx = store.HoldEffects(fctx)
			d.Fail(true)
			err := sc.Run(fctx, st)
			d.Fail(false)
			effects, ferr := flushEffects(fctx, st, faulted)
			if ferr != nil {
				t.Fatalf("flush: %v", ferr)
			}
			if failed := d.Failed() - before; failed > 0 {
				if err == nil {
					t.Errorf("succeeded although %d commits failed", failed)
				}
				if len(effects) != 0 {
					t.Errorf("recorded %q although its commit failed (%v)", effects, err)
				}
				// Nothing was committed, so the same run again must succeed.
				cctx, clean := store.TrackEffects(ctx)
				err = sc.Run(cctx, st)
				effects = clean.List()
			} else if sc.Commits {
				t.Errorf("never reached a commit (%v)", err)
			}
			if sc.Err == nil && err != nil || sc.Err != nil && !errors.Is(err, sc.Err) {
				t.Fatalf("run: %v, want %v", err, sc.Err)
			}
			if len(effects) == 0 {
				t.Errorf("run recorded no effect")
			}
		})
	}
}

const worker = "worker"

// flushTask is the task whose transition commits on the run's context after
// the run, so that effects the run left held are released and seen.
const flushTask = "effects-flush"

// flushEffects commits a transition of flushTask on ctx and returns the
// effects of rec other than the flush's own.
func flushEffects(ctx context.Context, st store.Store, rec *store.Effects) ([]string, error) {
	if err := st.UpdateTaskState(ctx, flushTask, domain.TaskStateSubmitted, domain.TaskStateWorking, "test", "{}"); err != nil {
		return nil, err
	}
	var out []string
	flushed := false
	for _, e := range rec.List() {
		if !flushed && strings.Contains(e, flushTask) {
			flushed = true
			continue
		}
		out = append(out, e)
	}
	if !flushed {
		return nil, errors.New("the flush transition recorded no effect")
	}
	return out, nil
}

func seedTask(ctx context.Context, st store.Store, id string, state domain.TaskState) error {
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: worker, Zone: domain.ZoneSandbox}); err != nil {
		return err
	}
	if err := st.CreateTask(ctx, domain.Task{ID: id, IdempotencyKey: id, WorkloadName: worker, State: domain.TaskStateSubmitted}); err != nil {
		return err
	}
	if state == domain.TaskStateSubmitted {
		return nil
	}
	return st.UpdateTaskState(ctx, id, domain.TaskStateSubmitted, state, "test", "{}")
}

func seedRoom(ctx context.Context, st store.Store, id string, state domain.ConclaveState, reason string, age time.Duration) error {
	created := time.Now().UTC().Add(-age)
	return st.CreateConclave(ctx, domain.ConclaveSession{ID: id, State: state, WardReason: reason, CreatedAt: created, UpdatedAt: created})
}

func roomLimits(maxMessages int) store.ConclaveLimits {
	return store.ConclaveLimits{MaxMessages: maxMessages, MaxPingPong: 10, MaxRuns: 6, MaxAge: store.ConclaveWallTime, Now: time.Now}
}

func turnTask(room, role string) (domain.Task, error) {
	a := store.ConclaveAssignment{ConclaveID: room, Role: role, Activation: domain.ActivationAlwaysOn}
	raw, err := json.Marshal(a)
	if err != nil {
		return domain.Task{}, err
	}
	return domain.Task{ID: "turn-" + role, IdempotencyKey: a.TurnKey(), WorkloadName: worker, State: domain.TaskStateSubmitted, InputJSON: string(raw)}, nil
}

func submitTurn(ctx context.Context, st store.Store, room, role string, limits store.ConclaveLimits) error {
	task, err := turnTask(room, role)
	if err != nil {
		return err
	}
	_, err = store.SubmitConclaveTurn(ctx, room, task.IdempotencyKey, limits, func(ctx context.Context) (domain.Task, error) {
		return task, st.CreateTask(ctx, task)
	})
	return err
}

func post(from, kind string) domain.ConclaveMessage {
	return domain.ConclaveMessage{ConclaveID: "room", FromRole: from, ToRole: "all", Kind: kind, Body: "from " + from, CreatedAt: time.Now().UTC()}
}

// StoreScenarios drive every write of store.Store, including the guarded
// conclave paths that commit several changes in one transaction.
func StoreScenarios() []Scenario {
	now := time.Now().UTC()
	task := func(state domain.TaskState) func(context.Context, store.Store) error {
		return func(ctx context.Context, st store.Store) error { return seedTask(ctx, st, "t1", state) }
	}
	room := func(state domain.ConclaveState, reason string, age time.Duration) func(context.Context, store.Store) error {
		return func(ctx context.Context, st store.Store) error {
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: worker, Zone: domain.ZoneSandbox}); err != nil {
				return err
			}
			return seedRoom(ctx, st, "room", state, reason, age)
		}
	}
	approval := func(status domain.ApprovalStatus) func(context.Context, store.Store) error {
		return func(ctx context.Context, st store.Store) error {
			if err := seedTask(ctx, st, "t1", domain.TaskStateWorking); err != nil {
				return err
			}
			if err := st.CreateApproval(ctx, domain.Approval{ID: "a1", TaskID: "t1", WorkloadName: worker, Capability: "git.push",
				PayloadCanonicalJSON: "{}", PayloadSHA256: "sum", ExpiresAt: now.Add(time.Hour)}); err != nil {
				return err
			}
			if status == domain.ApprovalPending {
				return nil
			}
			return st.DecideApproval(ctx, "a1", "sum", status, "operator", "{}")
		}
	}
	lease := func(ctx context.Context, st store.Store) error {
		if err := seedTask(ctx, st, "t1", domain.TaskStateWorking); err != nil {
			return err
		}
		// The expiry is read when the setup runs, not from now, so a slow run
		// cannot leave a lease that has expired before the renewal.
		return st.CreateLease(ctx, domain.Lease{TokenSHA256: "lease-sum", TaskID: "t1", WorkloadName: worker, ExpiresAt: time.Now().UTC().Add(time.Minute)})
	}
	reservation := ""
	var wake int64
	relic := domain.ConclaveRelic{ConclaveID: "room", Name: "report.txt", AuthorRole: "verifier", ContentType: "text/plain", Body: "ok", CreatedAt: now}
	sum := sha256.Sum256([]byte(relic.Body))
	relic.SHA256 = hex.EncodeToString(sum[:])
	return []Scenario{
		{Name: "upsert workload", Methods: []string{"UpsertWorkload"},
			Run: func(ctx context.Context, st store.Store) error {
				return st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "other", Zone: domain.ZoneSandbox})
			}},
		{Name: "create task", Methods: []string{"CreateTask"},
			Setup: func(ctx context.Context, st store.Store) error {
				return st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: worker, Zone: domain.ZoneSandbox})
			},
			Run: func(ctx context.Context, st store.Store) error {
				return st.CreateTask(ctx, domain.Task{ID: "t1", IdempotencyKey: "t1", WorkloadName: worker, State: domain.TaskStateSubmitted})
			}},
		{Name: "create task with registration", Methods: []string{"CreateTask"}, Commits: true,
			Setup: func(ctx context.Context, st store.Store) error {
				return st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: worker, Zone: domain.ZoneSandbox})
			},
			Run: func(ctx context.Context, st store.Store) error {
				return store.CreateTaskWithMemory(ctx, st, domain.Task{ID: "t1", IdempotencyKey: "t1", WorkloadName: worker, State: domain.TaskStateSubmitted},
					domain.MemoryEntry{Namespace: "registrations", Key: "t1", Value: "{}", Author: "test", UpdatedAt: now})
			}},
		{Name: "conclave turn", Methods: []string{"CreateTask"}, Commits: true,
			Setup: room(domain.ConclaveStateActive, "", 0),
			Run: func(ctx context.Context, st store.Store) error {
				return submitTurn(ctx, st, "room", "coder", roomLimits(10))
			}},
		{Name: "conclave turn refused at the run limit pauses", Methods: []string{"CreateTask"}, Commits: true, Err: store.ErrConclaveRunLimit,
			Setup: func(ctx context.Context, st store.Store) error {
				if err := room(domain.ConclaveStateActive, "", 0)(ctx, st); err != nil {
					return err
				}
				return st.CreateTask(ctx, domain.Task{ID: "earlier", IdempotencyKey: "conclave-room-init-earlier", WorkloadName: worker, State: domain.TaskStateCompleted})
			},
			Run: func(ctx context.Context, st store.Store) error {
				limits := roomLimits(10)
				limits.MaxRuns = 1
				return submitTurn(ctx, st, "room", "coder", limits)
			}},
		{Name: "claim recovery", Methods: []string{"ClaimRecovery"},
			Run: func(ctx context.Context, st store.Store) error {
				claimed, err := st.ClaimRecovery(ctx, "t1", "", "owner")
				if err == nil && !claimed {
					err = errors.New("not claimed")
				}
				return err
			}},
		{Name: "task message", Methods: []string{"AppendTaskMessage"}, Setup: task(domain.TaskStateWorking),
			Run: func(ctx context.Context, st store.Store) error {
				return st.AppendTaskMessage(ctx, domain.TaskMessage{TaskID: "t1", Role: "user", Content: "hello"})
			}},
		{Name: "task transition", Methods: []string{"UpdateTaskState"}, Commits: true, Setup: task(domain.TaskStateSubmitted),
			Run: func(ctx context.Context, st store.Store) error {
				return st.UpdateTaskState(ctx, "t1", domain.TaskStateSubmitted, domain.TaskStateWorking, "test", "{}")
			}},
		{Name: "claim in an expired conclave pauses it", Methods: []string{"UpdateTaskState"}, Commits: true, Err: store.ErrConflict,
			Setup: func(ctx context.Context, st store.Store) error {
				if err := seedTask(ctx, st, "t1", domain.TaskStateSubmitted); err != nil {
					return err
				}
				return seedRoom(ctx, st, "room", domain.ConclaveStateActive, "", time.Hour)
			},
			Run: func(ctx context.Context, st store.Store) error {
				return store.ClaimConclaveTask(ctx, st, "t1", "room", time.Now)
			}},
		{Name: "task artifacts", Methods: []string{"UpdateTaskArtifacts"}, Setup: task(domain.TaskStateWorking),
			Run: func(ctx context.Context, st store.Store) error { return st.UpdateTaskArtifacts(ctx, "t1", "[]") }},
		{Name: "heartbeat", Methods: []string{"RecordHeartbeat"}, Setup: task(domain.TaskStateWorking),
			Run: func(ctx context.Context, st store.Store) error { return st.RecordHeartbeat(ctx, "t1", now) }},
		{Name: "approval request", Methods: []string{"CreateApproval"}, Setup: task(domain.TaskStateWorking),
			Run: func(ctx context.Context, st store.Store) error {
				return st.CreateApproval(ctx, domain.Approval{ID: "a1", TaskID: "t1", WorkloadName: worker, Capability: "git.push",
					PayloadCanonicalJSON: "{}", PayloadSHA256: "sum", ExpiresAt: now.Add(time.Hour)})
			}},
		{Name: "approval decision", Methods: []string{"DecideApproval"}, Setup: approval(domain.ApprovalPending),
			Run: func(ctx context.Context, st store.Store) error {
				return st.DecideApproval(ctx, "a1", "sum", domain.ApprovalApproved, "operator", "{}")
			}},
		{Name: "approval spent", Methods: []string{"ConsumeApproval"}, Setup: approval(domain.ApprovalApproved),
			Run: func(ctx context.Context, st store.Store) error {
				return st.ConsumeApproval(ctx, "a1", "t1", "git.push", "sum")
			}},
		{Name: "lease", Methods: []string{"CreateLease"}, Setup: task(domain.TaskStateWorking),
			Run: func(ctx context.Context, st store.Store) error {
				return st.CreateLease(ctx, domain.Lease{TokenSHA256: "lease-sum", TaskID: "t1", WorkloadName: worker, ExpiresAt: now.Add(time.Minute)})
			}},
		{Name: "lease renewal", Methods: []string{"RenewTaskLeases"}, Setup: lease,
			Run: func(ctx context.Context, st store.Store) error {
				return st.RenewTaskLeases(ctx, "t1", time.Now(), time.Now().Add(time.Hour))
			}},
		{Name: "lease revocation", Methods: []string{"RevokeTaskLeases"}, Setup: lease,
			Run: func(ctx context.Context, st store.Store) error { return st.RevokeTaskLeases(ctx, "t1") }},
		{Name: "budget use", Methods: []string{"RecordBudgetUsage"}, Setup: task(domain.TaskStateWorking),
			Run: func(ctx context.Context, st store.Store) error {
				return st.RecordBudgetUsage(ctx, domain.BudgetEntry{TaskID: "t1", WorkloadName: worker, OutputTokens: 1})
			}},
		{Name: "budget reservation", Methods: []string{"ReserveBudget"}, Commits: true, Setup: task(domain.TaskStateWorking),
			Run: func(ctx context.Context, st store.Store) error {
				res, err := st.ReserveBudget(ctx, store.BudgetReservationRequest{TaskID: "t1", WorkloadName: worker, Reserve: domain.BudgetTotals{OutputTokens: 1}, Now: time.Now(), TTL: time.Minute})
				if err == nil && res.ReservationID == "" {
					err = errors.New("not reserved")
				}
				return err
			}},
		{Name: "budget settlement", Methods: []string{"SettleBudgetReservation"}, Commits: true,
			Setup: func(ctx context.Context, st store.Store) error {
				if err := seedTask(ctx, st, "t1", domain.TaskStateWorking); err != nil {
					return err
				}
				res, err := st.ReserveBudget(ctx, store.BudgetReservationRequest{TaskID: "t1", WorkloadName: worker, Reserve: domain.BudgetTotals{OutputTokens: 1}, Now: time.Now(), TTL: time.Minute})
				reservation = res.ReservationID
				return err
			},
			Run: func(ctx context.Context, st store.Store) error {
				return st.SettleBudgetReservation(ctx, reservation, &domain.BudgetEntry{TaskID: "t1", WorkloadName: worker, OutputTokens: 1})
			}},
		{Name: "checkpoint", Methods: []string{"SaveCheckpoint"}, Setup: task(domain.TaskStateWorking),
			Run: func(ctx context.Context, st store.Store) error { return st.SaveCheckpoint(ctx, "t1", worker, "{}") }},
		{Name: "provider preferences", Methods: []string{"UpsertProviderPrefs"},
			Run: func(ctx context.Context, st store.Store) error {
				return st.UpsertProviderPrefs(ctx, domain.ProviderPrefs{ProviderID: "provider", DefaultModel: "model"})
			}},
		{Name: "open conclave", Methods: []string{"CreateConclave"},
			Run: func(ctx context.Context, st store.Store) error {
				return seedRoom(ctx, st, "room", domain.ConclaveStateActive, "", 0)
			}},
		{Name: "close conclave", Methods: []string{"UpdateConclaveState"}, Commits: true, Setup: room(domain.ConclaveStateActive, "", 0),
			Run: func(ctx context.Context, st store.Store) error {
				return st.UpdateConclaveState(ctx, "room", domain.ConclaveStateCancelled, "")
			}},
		{Name: "import conclave message", Methods: []string{"AppendConclaveMessage"}, Setup: room(domain.ConclaveStateActive, "", 0),
			Run: func(ctx context.Context, st store.Store) error {
				_, err := st.AppendConclaveMessage(ctx, post("coder", "proposal"))
				return err
			}},
		{Name: "guarded post", Methods: []string{"AppendConclaveMessage"}, Commits: true, Setup: room(domain.ConclaveStateActive, "", 0),
			Run: func(ctx context.Context, st store.Store) error {
				_, _, _, err := store.AppendConclavePost(ctx, st, post("coder", "proposal"), roomLimits(10), false, false)
				return err
			}},
		{Name: "post at the message limit pauses", Methods: []string{"AppendConclaveMessage"}, Commits: true, Setup: room(domain.ConclaveStateActive, "", 0),
			Run: func(ctx context.Context, st store.Store) error {
				_, reason, _, err := store.AppendConclavePost(ctx, st, post("coder", "proposal"), roomLimits(1), false, false)
				if err == nil && reason == "" {
					err = errors.New("ward did not trip")
				}
				return err
			}},
		{Name: "decree resumes a paused conclave", Methods: []string{"AppendConclaveMessage"}, Commits: true,
			Setup: room(domain.ConclaveStateInputRequired, "max_messages_reached(1)", 0),
			Run: func(ctx context.Context, st store.Store) error {
				_, _, _, err := store.AppendConclavePost(ctx, st, post("operator", "decree"), roomLimits(1), true, false)
				return err
			}},
		{Name: "finish wake", Methods: []string{"FinishConclaveWake"},
			Setup: func(ctx context.Context, st store.Store) error {
				if err := room(domain.ConclaveStateActive, "", 0)(ctx, st); err != nil {
					return err
				}
				var err error
				wake, _, _, err = store.AppendConclavePost(ctx, st, post("operator", "decree"), roomLimits(10), false, true)
				return err
			},
			Run: func(ctx context.Context, st store.Store) error { return st.FinishConclaveWake(ctx, wake) }},
		{Name: "relic", Methods: []string{"PutConclaveRelic"}, Setup: room(domain.ConclaveStateActive, "", 0),
			Run: func(ctx context.Context, st store.Store) error { return st.PutConclaveRelic(ctx, relic) }},
		{Name: "report with relic", Methods: []string{"PutConclaveRelic", "AppendConclaveMessage"}, Commits: true, Setup: room(domain.ConclaveStateActive, "", 0),
			Run: func(ctx context.Context, st store.Store) error {
				msg := post("verifier", "review")
				msg.ToRole = "operator"
				_, _, err := store.AppendConclaveReport(ctx, st, msg, roomLimits(10), &relic)
				return err
			}},
		{Name: "pair device", Methods: []string{"CreateDevice"},
			Run: func(ctx context.Context, st store.Store) error {
				return st.CreateDevice(ctx, domain.PairedDevice{ID: "d1", Name: "laptop", Platform: "cli", PublicKeyPEM: "key"})
			}},
		{Name: "revoke device", Methods: []string{"RevokeDevice"},
			Setup: func(ctx context.Context, st store.Store) error {
				return st.CreateDevice(ctx, domain.PairedDevice{ID: "d1", Name: "laptop", Platform: "cli", PublicKeyPEM: "key"})
			},
			Run: func(ctx context.Context, st store.Store) error { return st.RevokeDevice(ctx, "d1") }},
		{Name: "store memory", Methods: []string{"PutMemory"},
			Run: func(ctx context.Context, st store.Store) error {
				return st.PutMemory(ctx, domain.MemoryEntry{Namespace: "notes", Key: "k", Value: "v"})
			}},
		{Name: "delete memory", Methods: []string{"DeleteMemory"},
			Setup: func(ctx context.Context, st store.Store) error {
				return st.PutMemory(ctx, domain.MemoryEntry{Namespace: "notes", Key: "k", Value: "v"})
			},
			Run: func(ctx context.Context, st store.Store) error { return st.DeleteMemory(ctx, "notes", "k") }},
	}
}
