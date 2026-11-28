package conclave

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func (b *Broker) readAssignment(ctx context.Context, room, workload, taskID string) (store.ConclaveAssignment, error) {
	if taskID == "" {
		return store.ConclaveAssignment{}, ErrMessageDenied
	}
	task, err := b.store.GetTask(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return store.ConclaveAssignment{}, ErrMessageDenied
	}
	if err != nil {
		return store.ConclaveAssignment{}, err
	}
	a, ok := store.TaskConclaveAssignment(task)
	if !ok || task.WorkloadName != workload || a.ConclaveID != room || !config.ValidVassalRole(a.Role) {
		return store.ConclaveAssignment{}, ErrMessageDenied
	}
	return a, nil
}

type councilRead struct {
	decrees  []domain.ConclaveMessage
	peers    []CouncilTurn
	attempts []store.CouncilAttempt
}

// A nil view denotes an authorized read of an ordinary Conclave.
func (b *Broker) readView(ctx context.Context, room, workload, taskID string) (*councilRead, error) {
	a, err := b.readAssignment(ctx, room, workload, taskID)
	if err != nil {
		return nil, err
	}
	cs, err := store.Councils(b.store)
	if err == nil {
		snap, err := cs.CouncilSnapshot(ctx, room)
		if err == nil {
			return councilViewForAssignment(snap, a, workload, taskID, b.now())
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	if a.Round != 0 {
		return nil, ErrMessageDenied
	}
	session, err := b.store.GetConclave(ctx, room)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrMessageDenied
	}
	if err != nil {
		return nil, err
	}
	_, err = templateRole(session, b.bundle(), workload, a.Role)
	return nil, err
}

func councilViewForAssignment(snap store.CouncilSnapshot, a store.ConclaveAssignment, workload, taskID string, now time.Time) (*councilRead, error) {
	plan := CouncilSnapshot{
		Session: snap.Session, Members: snap.Members, Question: snap.Question,
		MaxRounds: snap.MaxRounds, MaxStarts: snap.MaxStarts, Sequential: snap.Sequential,
		Turns: snap.Turns, Decrees: snap.Decrees, Deadline: snap.Deadline,
	}
	session := plan.Session
	if session.ID == "" || session.CreatedAt.IsZero() || now.Before(session.CreatedAt) || !plan.Deadline.After(session.CreatedAt) {
		return nil, errors.New("invalid council identity or deadline")
	}
	switch session.State {
	case domain.ConclaveStateActive:
		if session.WardReason != "" {
			return nil, errors.New("active council has an unresolved Ward")
		}
	case domain.ConclaveStateInputRequired:
		if session.WardReason == "" {
			return nil, errors.New("paused council has no Ward reason")
		}
		return nil, ErrMessageDenied
	case domain.ConclaveStateCompleted, domain.ConclaveStateCancelled:
		return nil, ErrMessageDenied
	default:
		return nil, errors.New("unknown council state")
	}
	if !now.Before(plan.Deadline) {
		return nil, ErrMessageDenied
	}
	turns, _, err := plan.indexTurns()
	if err != nil {
		return nil, err
	}
	turn := turns[a.Round][a.Role]
	if snap.Session.ID != a.ConclaveID || a.Round == 0 || turn.TaskID != taskID || turn.Starts != a.Attempt {
		return nil, ErrMessageDenied
	}
	if turn.State != domain.TaskStateWorking && turn.State != domain.TaskStateInputRequired {
		return nil, ErrMessageDenied
	}
	for _, m := range snap.Members {
		if m.Vassal.Role == a.Role && m.Vassal.Workload == workload {
			decrees, peers := plan.visible(a.Round, m, turns)
			return &councilRead{decrees, peers, snap.Attempts}, nil
		}
	}
	return nil, ErrMessageDenied
}

func (b *Broker) ReadMessagesForTask(ctx context.Context, room, workload, taskID string, since int64) ([]domain.ConclaveMessage, error) {
	view, err := b.readView(ctx, room, workload, taskID)
	if err != nil {
		return nil, err
	}
	if view == nil {
		msgs, err := b.store.ListConclaveMessages(ctx, room, since)
		if err != nil {
			return nil, err
		}
		for i := range msgs {
			msgs[i].Body = redact.String(msgs[i].Body)
			msgs[i].ToRole = redact.String(msgs[i].ToRole)
		}
		return msgs, nil
	}
	var msgs []domain.ConclaveMessage
	appendMessage := func(m domain.ConclaveMessage) {
		if m.ID > since {
			m.Body = redact.String(m.Body)
			m.ToRole = redact.String(m.ToRole)
			msgs = append(msgs, m)
		}
	}
	for _, d := range view.decrees {
		appendMessage(d)
	}
	for _, t := range view.peers {
		appendMessage(*t.Answer)
	}
	slices.SortFunc(msgs, func(a, b domain.ConclaveMessage) int { return cmp.Compare(a.ID, b.ID) })
	return msgs, nil
}

func (b *Broker) ReadRelicForTask(ctx context.Context, room, workload, taskID, name string) (domain.ConclaveRelic, error) {
	view, err := b.readView(ctx, room, workload, taskID)
	if err != nil {
		return domain.ConclaveRelic{}, err
	}
	if view == nil {
		relic, err := b.store.GetConclaveRelic(ctx, room, name)
		if err != nil {
			return domain.ConclaveRelic{}, err
		}
		return redactRelicForTask(relic), nil
	}
	for _, t := range view.peers {
		for _, a := range view.attempts {
			if a.TaskID != t.TaskID || a.Round != t.Round || a.Role != t.Role || a.Number != t.Starts {
				continue
			}
			for _, r := range a.Relics {
				if r.Name == name && r.ConclaveID == room && r.AuthorRole == t.Role {
					return redactRelicForTask(r), nil
				}
			}
		}
	}
	return domain.ConclaveRelic{}, store.ErrNotFound
}

// redactRelicForTask redacts a Relic copy on its way to a workload, so a value
// that became daemon-held after the Relic was stored does not leave in its
// metadata either. The stored row is not changed.
func redactRelicForTask(r domain.ConclaveRelic) domain.ConclaveRelic {
	r.Name = redact.String(r.Name)
	r.ContentType = redact.String(r.ContentType)
	r.Body = redact.String(r.Body)
	return r
}
