package conclave

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// council returns nil for an ordinary room or a backend without council support.
func (b *Broker) council(ctx context.Context, id string) (*store.CouncilSnapshot, error) {
	cs, err := store.Councils(b.store)
	if errors.Is(err, store.ErrCouncilUnsupported) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	snap, err := cs.CouncilSnapshot(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if snap.Session.ID != id {
		return nil, store.ErrConflict
	}
	return &snap, nil
}

// ReconcileCouncils follows task recovery at startup and task observation on each tick.
func (b *Broker) ReconcileCouncils(ctx context.Context, allowStarts bool) error {
	if _, err := store.Councils(b.store); err != nil {
		if errors.Is(err, store.ErrCouncilUnsupported) {
			return nil
		}
		return err
	}
	rooms, err := b.store.ListConclaves(ctx)
	if err != nil {
		return err
	}
	var passErr error
	for _, room := range rooms {
		if err := ctx.Err(); err != nil {
			return errors.Join(passErr, err)
		}
		snap, err := b.council(ctx, room.ID)
		if err == nil && snap != nil {
			err = b.driveCouncil(ctx, snap, allowStarts)
		}
		if err != nil {
			passErr = errors.Join(passErr, fmt.Errorf("reconcile council: %w", err))
		}
	}
	return passErr
}

func councilClosed(s *store.CouncilSnapshot) bool {
	return s.Session.State == domain.ConclaveStateCompleted || s.Session.State == domain.ConclaveStateCancelled
}

// Explicit template Wards can tighten the policy pinned when the room opened.
// The store supplies omitted council limits inside its reservation transaction.
func (b *Broker) councilLimits(bundle *config.Bundle, room domain.ConclaveSession) store.ConclaveLimits {
	tpl, ok := template(bundle, room.TemplateName)
	if !ok {
		return store.ConclaveLimits{BudgetMissing: room.TemplateName != ""}
	}
	limits := b.limits(bundle, tpl, true)
	limits.MaxMessages = tpl.Spec.Wards.MaxMessages
	limits.MaxRuns = tpl.Spec.Wards.MaxRuns
	limits.MaxPingPong = tpl.Spec.Wards.MaxPingPong
	return limits
}

// Plan only from a snapshot whose completed turns have each been inspected.
// An exit committed during collection must not be mistaken for a silent turn.
func (b *Broker) collectCouncil(ctx context.Context, snap *store.CouncilSnapshot, ownerClosing bool) (*store.CouncilSnapshot, error) {
	checked := map[string]bool{}
	for !councilClosed(snap) {
		refresh := false
		for _, turn := range snap.Turns {
			if turn.State != domain.TaskStateCompleted || turn.Answer != nil || checked[turn.TaskID] {
				continue
			}
			_, err := b.RecordCouncilAnswer(ctx, snap.Session.ID, turn.TaskID)
			if err != nil {
				expired := !b.now().Before(snap.Deadline)
				refused := errors.Is(err, ErrMessageDenied) || errors.Is(err, ErrWardTripped) || errors.Is(err, ErrConclavePaused) || errors.Is(err, ErrConclaveClosed)
				if refused && (ownerClosing || expired) {
					// A forced close preserves admitted posts and marks refused seats missing.
					continue
				}
				if expired {
					// The deadline blocks reservations while collection retries.
					err = errors.Join(err, b.cancelCouncilTasks(ctx, snap, "deadline"))
				}
				return nil, err
			}
			checked[turn.TaskID] = true
			refresh = true
		}
		if !refresh {
			break
		}
		fresh, err := b.council(ctx, snap.Session.ID)
		if err != nil {
			return nil, err
		}
		if fresh == nil {
			return nil, store.ErrNotFound
		}
		snap = fresh
	}
	return snap, nil
}

func (b *Broker) driveCouncil(ctx context.Context, initial *store.CouncilSnapshot, allowStarts bool) error {
	s, err := b.collectCouncil(ctx, initial, false)
	if errors.Is(err, ErrWardTripped) || errors.Is(err, ErrConclavePaused) || errors.Is(err, ErrConclaveClosed) {
		// The next pass will inspect the unposted answer again, even if a
		// concurrent Decree resumes the room after the refused post.
		return nil
	}
	if err != nil {
		return err
	}
	if councilClosed(s) {
		return b.cancelCouncilTasks(ctx, s, s.Session.WardReason)
	}
	plan := CouncilSnapshot{Session: s.Session, Members: s.Members, Question: s.Question, Decrees: s.Decrees, MaxRounds: s.MaxRounds, MaxStarts: s.MaxStarts, Deadline: s.Deadline, Sequential: s.Sequential, Turns: s.Turns}
	step, err := plan.Next(b.now())
	if err != nil {
		return err
	}
	switch step.Action {
	case "wait":
		return nil
	case "cancel", "close":
		return b.finishCouncil(ctx, s.Session.ID, step.Reason)
	case "start":
		if !allowStarts {
			return nil
		}
		return b.reserveCouncilStarts(ctx, s, step)
	default:
		return errors.New("unexpected council planner action")
	}
}

func (b *Broker) reserveCouncilStarts(ctx context.Context, s *store.CouncilSnapshot, step CouncilStep) error {
	if b.reconciler == nil {
		return errors.New("council task reconciliation unavailable")
	}
	members := make(map[string]domain.ConclaveVassal, len(s.Members))
	for _, m := range s.Members {
		members[m.Vassal.Role] = m.Vassal
	}
	previous := map[string]store.CouncilTurn{}
	for _, turn := range s.Turns {
		if turn.Round == step.Round {
			previous[turn.Role] = turn
		}
	}
	limits := b.councilLimits(b.bundle(), s.Session)
	for _, start := range step.Starts {
		if turn, ok := previous[start.Role]; ok {
			ready, err := b.reconciler.CouncilRetryReady(ctx, turn.TaskID)
			if err != nil {
				return err
			}
			if !ready {
				continue
			}
		}
		input := turnInput{ConclaveAssignment: store.ConclaveAssignment{
			ConclaveID: s.Session.ID, Round: step.Round, Role: start.Role, Attempt: start.Attempt,
		}, Packet: store.RedactCouncilAnswer(start.Packet.Markdown)}
		_, err := b.submitTurn(ctx, members[start.Role], input, limits)
		if errors.Is(err, ErrConclavePaused) || errors.Is(err, ErrConclaveClosed) || errors.Is(err, store.ErrConclaveRunLimit) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Close under the reservation lock first, then read back all admitted attempts.
// A later pass finishes cancellation if the daemon stops between the two steps.
func (b *Broker) finishCouncil(ctx context.Context, id, reason string) error {
	if _, err := b.CloseCouncil(ctx, id, reason); err != nil {
		return err
	}
	s, err := b.council(ctx, id)
	if err != nil {
		return err
	}
	if s == nil || !councilClosed(s) {
		return store.ErrConflict
	}
	return b.cancelCouncilTasks(ctx, s, s.Session.WardReason)
}

func (b *Broker) cancelCouncilTasks(ctx context.Context, s *store.CouncilSnapshot, reason string) error {
	var stops sync.WaitGroup
	errs := make([]error, len(s.Attempts))
	for i, a := range s.Attempts {
		switch a.State {
		case domain.TaskStateFailed, domain.TaskStateCancelled, domain.TaskStateCompleted:
			continue
		}
		if b.reconciler == nil {
			return errors.New("council task cancellation unavailable")
		}
		stops.Go(func() {
			err := b.reconciler.CancelTask(ctx, a.TaskID, reason)
			if errors.Is(err, store.ErrConflict) {
				task, readErr := b.store.GetTask(ctx, a.TaskID)
				if readErr == nil && (task.State == domain.TaskStateCancelled || task.State == domain.TaskStateCompleted || task.State == domain.TaskStateFailed) {
					return
				}
				err = errors.Join(err, readErr)
			}
			errs[i] = err
		})
	}
	stops.Wait()
	return errors.Join(errs...)
}
