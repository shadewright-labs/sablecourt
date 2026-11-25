package conclave

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
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

type councilPostAction struct {
	snap    *store.CouncilSnapshot
	reason  string
	cleanup bool
}

// ReconcileCouncils follows task recovery at startup and task observation on each tick.
func (b *Broker) ReconcileCouncils(ctx context.Context, allowStarts bool) error {
	b.councilMu.Lock()
	councils, err := store.Councils(b.store)
	if err != nil {
		b.councilMu.Unlock()
		if errors.Is(err, store.ErrCouncilUnsupported) {
			return nil
		}
		return err
	}
	ids, err := councils.ListReconcileCouncils(ctx)
	if err != nil {
		b.councilMu.Unlock()
		return err
	}
	if b.councilStopping == nil {
		b.councilStopping = make(map[string]bool)
	}
	var passErr error
	var actions []councilPostAction
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			b.councilMu.Unlock()
			return errors.Join(passErr, err)
		}
		if b.councilStopping[id] {
			continue
		}
		snap, err := b.council(ctx, id)
		var action *councilPostAction
		if err == nil && snap != nil {
			action, err = b.planCouncil(ctx, snap, allowStarts)
		}
		if action != nil {
			b.councilStopping[action.snap.Session.ID] = true
			actions = append(actions, *action)
		}
		if err != nil {
			passErr = errors.Join(passErr, fmt.Errorf("reconcile council: %w", err))
		}
	}
	b.councilMu.Unlock()

	if len(actions) == 0 {
		return passErr
	}
	actionErrs := make([]error, len(actions))
	var wg sync.WaitGroup
	for i, action := range actions {
		wg.Go(func() {
			defer func() {
				b.councilMu.Lock()
				delete(b.councilStopping, action.snap.Session.ID)
				b.councilMu.Unlock()
			}()
			if err := b.runCouncilPostAction(ctx, action); err != nil {
				actionErrs[i] = fmt.Errorf("reconcile council: %w", err)
			}
		})
	}
	wg.Wait()
	return errors.Join(append([]error{passErr}, actionErrs...)...)
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
	s, cancelExpired, err := b.collectCouncilState(ctx, snap, ownerClosing)
	if cancelExpired {
		err = errors.Join(err, b.cancelCouncilTasks(ctx, s, "deadline"))
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (b *Broker) collectCouncilState(ctx context.Context, snap *store.CouncilSnapshot, ownerClosing bool) (*store.CouncilSnapshot, bool, error) {
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
				return snap, expired && !ownerClosing, err
			}
			checked[turn.TaskID] = true
			refresh = true
		}
		if !refresh {
			break
		}
		fresh, err := b.council(ctx, snap.Session.ID)
		if err != nil {
			return nil, false, err
		}
		if fresh == nil {
			return nil, false, store.ErrNotFound
		}
		snap = fresh
	}
	return snap, false, nil
}

func (b *Broker) planCouncil(ctx context.Context, initial *store.CouncilSnapshot, allowStarts bool) (*councilPostAction, error) {
	s, cancelExpired, err := b.collectCouncilState(ctx, initial, false)
	if cancelExpired {
		return &councilPostAction{snap: s, reason: "deadline"}, err
	}
	if errors.Is(err, ErrWardTripped) || errors.Is(err, ErrConclavePaused) || errors.Is(err, ErrConclaveClosed) {
		// The next pass will inspect the unposted answer again, even if a
		// concurrent Decree resumes the room after the refused post.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if councilClosed(s) {
		return &councilPostAction{snap: s, reason: s.Session.WardReason, cleanup: s.CleanupPending}, nil
	}
	plan := CouncilSnapshot{Session: s.Session, Members: s.Members, Question: s.Question, Decrees: s.Decrees, MaxRounds: s.MaxRounds, MaxStarts: s.MaxStarts, Deadline: s.Deadline, Sequential: s.Sequential, Turns: s.Turns}
	step, err := plan.Next(b.now())
	if err != nil {
		return nil, err
	}
	switch step.Action {
	case "wait":
		return nil, nil
	case "cancel":
		closed, err := b.recordCouncilClose(ctx, s.Session.ID, domain.ConclaveStateCancelled, step.Reason)
		if err != nil {
			return nil, err
		}
		return &councilPostAction{snap: closed, reason: closed.Session.WardReason, cleanup: closed.CleanupPending}, nil
	case "close":
		closed, err := b.recordCouncilClose(ctx, s.Session.ID, domain.ConclaveStateCompleted, step.Reason)
		if err != nil {
			return nil, err
		}
		return &councilPostAction{snap: closed, reason: closed.Session.WardReason, cleanup: closed.CleanupPending}, nil
	case "start":
		if !allowStarts {
			return nil, nil
		}
		if s.Checkout.WriterRole != "" {
			if err := prepareWriterPackets(s, plan, step); err != nil {
				return nil, err
			}
		}
		return nil, b.reserveCouncilStarts(ctx, s, step)
	default:
		return nil, errors.New("unexpected council planner action")
	}
}

// Add the diff as a separate peer excerpt so a long final answer cannot
// consume the writer's entire share before reviewers see any changes.
func prepareWriterPackets(s *store.CouncilSnapshot, plan CouncilSnapshot, step CouncilStep) error {
	turns, _, err := plan.indexTurns()
	if err != nil {
		return err
	}
	members := make(map[string]CouncilMember, len(s.Members))
	for _, m := range s.Members {
		members[m.Vassal.Role] = m
	}
	for i, start := range step.Starts {
		member := members[start.Role]
		decrees, peers := plan.visible(step.Round, member, turns)
		var rounds []contextpack.CouncilRound
		hasEvidence := false
		for _, peer := range peers {
			if len(rounds) == 0 || rounds[len(rounds)-1].Number != peer.Round {
				rounds = append(rounds, contextpack.CouncilRound{Number: peer.Round})
			}
			round := &rounds[len(rounds)-1]
			round.Answers = append(round.Answers, contextpack.CouncilAnswer{Role: peer.Role, Body: peer.Answer.Body})
			if peer.Role == s.Checkout.WriterRole {
				if evidence := writerEvidence(s, peer); evidence != "" {
					round.Answers = append(round.Answers, contextpack.CouncilAnswer{Role: peer.Role, Body: evidence})
					hasEvidence = true
				}
			}
		}
		if !hasEvidence {
			continue
		}
		var instructions []string
		for _, d := range decrees {
			instructions = append(instructions, d.Body)
		}
		packet, err := contextpack.AssembleCouncil(member.RoleCard, s.Question, instructions, rounds)
		if err != nil {
			return err
		}
		step.Starts[i].Packet = packet
	}
	return nil
}

func writerEvidence(s *store.CouncilSnapshot, turn CouncilTurn) string {
	var evidence strings.Builder
	for _, a := range s.Attempts {
		if a.Role != turn.Role || a.Round != turn.Round {
			continue
		}
		if a.State == domain.TaskStateFailed && a.Number < turn.Starts {
			fmt.Fprintf(&evidence, "writer attempt %d produced nothing.\n", a.Number)
		}
		if a.State != domain.TaskStateCompleted || a.TaskID != turn.TaskID || a.Number != turn.Starts {
			continue
		}
		for _, r := range a.Relics {
			if r.Name == controlplane.CouncilWriterDiffName(turn.TaskID) && r.ConclaveID == s.Session.ID &&
				r.AuthorRole == turn.Role && r.ContentType == "text/x-diff" && store.ValidCouncilText(r.Body) {
				evidence.WriteString("Writer diff\n")
				evidence.WriteString(store.RedactCouncilAnswer(r.Body))
			}
		}
	}
	return evidence.String()
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
	mode := "live"
	switch {
	case s.Checkout.WriterRole != "":
		mode = "writer"
	case s.Checkout.Commit != "":
		mode = "ref"
	}
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
		input := turnInput{
			ConclaveAssignment: store.ConclaveAssignment{
				ConclaveID: s.Session.ID, Round: step.Round, Role: start.Role, Attempt: start.Attempt,
			},
			Packet:         store.RedactCouncilAnswer(start.Packet.Markdown),
			CheckoutMode:   mode,
			CheckoutRef:    s.Checkout.Commit,
			CheckoutWriter: s.Checkout.WriterRole,
		}
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
// A later pass finishes cancellation and worktree cleanup if the daemon stops in between.
func (b *Broker) recordCouncilClose(ctx context.Context, id string, state domain.ConclaveState, reason string) (*store.CouncilSnapshot, error) {
	if _, err := b.closeCouncil(ctx, id, state, reason); err != nil {
		return nil, err
	}
	s, err := b.council(ctx, id)
	if err != nil {
		return nil, err
	}
	if s == nil || !councilClosed(s) {
		return nil, store.ErrConflict
	}
	return s, nil
}

func (b *Broker) finishCouncil(ctx context.Context, id string, state domain.ConclaveState, reason string) error {
	s, err := b.recordCouncilClose(ctx, id, state, reason)
	if err != nil {
		return err
	}
	return b.runCouncilPostAction(ctx, councilPostAction{snap: s, reason: s.Session.WardReason, cleanup: s.CleanupPending})
}

func (b *Broker) runCouncilPostAction(ctx context.Context, action councilPostAction) error {
	if err := b.cancelCouncilTasks(ctx, action.snap, action.reason); err != nil {
		return err
	}
	if !action.cleanup {
		return nil
	}
	return b.cleanupCouncilCheckout(ctx, action.snap.Session.ID)
}

func (b *Broker) cleanupCouncilCheckout(ctx context.Context, id string) error {
	s, err := b.council(ctx, id)
	if err != nil || s == nil || !councilClosed(s) || !s.CleanupPending {
		return err
	}
	if b.reconciler == nil {
		return errors.New("council checkout cleanup unavailable")
	}
	for _, a := range s.Attempts {
		switch a.State {
		case domain.TaskStateFailed, domain.TaskStateCancelled, domain.TaskStateCompleted:
		default:
			return nil
		}
		ready, err := b.reconciler.CouncilRetryReady(ctx, a.TaskID)
		if err != nil {
			return err
		}
		if !ready {
			return nil
		}
	}
	if err := b.reconciler.CleanupCouncil(ctx, id); err != nil {
		return err
	}
	councils, err := store.Councils(b.store)
	if err != nil {
		return err
	}
	return councils.MarkCouncilCleanedUp(ctx, id)
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
