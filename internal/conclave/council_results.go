package conclave

import (
	"context"
	"encoding/json/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// CouncilNothingToAdd is a vote only when it is the entire trimmed final answer.
const CouncilNothingToAdd = "NOTHING TO ADD"

type councilAnswerKey struct{ room, task string }

var councilAnswersInFlight sync.Map

// RecordCouncilAnswer links a completed current attempt's persisted final answer.
// The room driver must reconcile results before reserving retries or closing.
func (b *Broker) RecordCouncilAnswer(ctx context.Context, roomID, taskID string) (bool, error) {
	key := councilAnswerKey{roomID, taskID}
	if _, pending := councilAnswersInFlight.LoadOrStore(key, struct{}{}); pending {
		return false, store.ErrConflict
	}
	defer councilAnswersInFlight.Delete(key)
	councils, err := store.Councils(b.store)
	if err != nil {
		return false, err
	}
	snap, err := councils.CouncilSnapshot(ctx, roomID)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(snap.Turns, func(turn store.CouncilTurn) bool { return turn.TaskID == taskID })
	if i < 0 || snap.Turns[i].State != domain.TaskStateCompleted {
		return false, nil
	}
	turn := snap.Turns[i]
	if turn.Answer != nil {
		return true, nil
	}
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return false, err
	}
	a, ok := store.TaskConclaveAssignment(task)
	if !ok || task.ID != taskID || task.State != domain.TaskStateCompleted || a.ConclaveID != roomID ||
		a.Role != turn.Role || a.Round != turn.Round || a.Attempt != turn.Starts {
		return false, store.ErrConflict
	}
	body, vote := councilFinalAnswer(task.ArtifactsJSON)
	if body == "" {
		return false, nil
	}
	events, err := b.store.ListTaskEvents(ctx, taskID)
	if err != nil {
		return false, err
	}
	exitedAt := councilSuccessfulExit(events, taskID)
	if exitedAt.IsZero() {
		return false, nil
	}
	to := councilAnswerRecipient(snap.Members, turn.Role, task.WorkloadName)
	if to == "" {
		return false, ErrMessageDenied
	}
	msg := domain.ConclaveMessage{
		ConclaveID: roomID, FromRole: turn.Role, ToRole: to, TaskID: taskID,
		Kind: "review", Body: body, CreatedAt: exitedAt,
	}
	id, err := b.postCouncilAnswer(ctx, snap.Session, msg)
	if err != nil {
		return false, err
	}
	if id <= 0 {
		return false, store.ErrConflict
	}
	if err := councils.AcceptCouncilAnswer(ctx, taskID, id, vote); err != nil {
		return false, err
	}
	return true, nil
}

func councilFinalAnswer(raw string) (body string, vote bool) {
	if len(raw) > store.MaxConclaveTurnInput {
		return "", false
	}
	var artifacts []*scruntime.Artifact
	if json.Unmarshal([]byte(raw), &artifacts, json.RejectUnknownMembers(true)) != nil {
		return "", false
	}
	var final *scruntime.Artifact
	for _, a := range artifacts {
		if a == nil {
			return "", false
		}
		if a.Name == "final-message" {
			if final != nil {
				return "", false
			}
			final = a
		}
	}
	if final == nil || final.ContentType != "text/plain" || final.URI != "" || !store.ValidCouncilText(final.Text) {
		return "", false
	}
	rawBody := strings.TrimSpace(final.Text)
	body = redact.String(rawBody)
	if !store.ValidCouncilText(body) {
		return "", false
	}
	return body, rawBody == CouncilNothingToAdd
}

func councilSuccessfulExit(events []domain.TaskEvent, taskID string) time.Time {
	for _, event := range slices.Backward(events) {
		if event.ToState != domain.TaskStateCompleted || event.FromState == event.ToState {
			continue
		}
		if event.TaskID != taskID || event.Reason != "runtime_exit_observed" || len(event.FactJSON) > store.MaxCouncilText {
			break
		}
		var fact struct {
			Code *int `json:"exit_code"`
		}
		if json.Unmarshal([]byte(event.FactJSON), &fact, json.RejectUnknownMembers(true)) == nil && fact.Code != nil && *fact.Code == 0 {
			return event.CreatedAt
		}
		break
	}
	return time.Time{}
}

func councilAnswerRecipient(members []store.CouncilMember, role, workload string) string {
	i := slices.IndexFunc(members, func(m store.CouncilMember) bool { return m.Vassal.Role == role })
	if i < 0 || members[i].Vassal.Workload != workload || redact.String(role) != role {
		return ""
	}
	seat := members[i]
	routes := seat.Vassal.CanMessage
	if slices.Contains(routes, "all") && (seat.Zone == domain.ZoneRuntime ||
		!slices.ContainsFunc(members, func(m store.CouncilMember) bool { return m.Zone == domain.ZoneRuntime })) {
		return "all"
	}
	for _, m := range members {
		to := m.Vassal.Role
		if to != role && slices.Contains(routes, to) && redact.String(to) == to &&
			(seat.Zone == domain.ZoneRuntime || m.Zone == domain.ZoneSandbox) {
			return to
		}
	}
	if slices.Contains(routes, "operator") {
		return "operator"
	}
	if slices.Contains(routes, role) {
		return role
	}
	return ""
}

func (b *Broker) postCouncilAnswer(ctx context.Context, room domain.ConclaveSession, msg domain.ConclaveMessage) (int64, error) {
	msgs, err := b.store.ListConclaveMessages(ctx, room.ID, 0)
	if err != nil {
		return 0, err
	}
	var id int64
	for _, saved := range msgs {
		// The exit timestamp and exact payload identify a prior post when linking
		// failed. Earlier peer posts cannot satisfy the completion evidence.
		if saved.TaskID == msg.TaskID && saved.CreatedAt.Equal(msg.CreatedAt) &&
			saved.ConclaveID == msg.ConclaveID && saved.FromRole == msg.FromRole &&
			saved.ToRole == msg.ToRole && saved.Kind == msg.Kind && saved.Body == msg.Body {
			if id != 0 || saved.ID <= 0 {
				return 0, store.ErrConflict
			}
			id = saved.ID
		}
	}
	if id != 0 {
		return id, nil
	}
	// The store applies the pinned Wards. This post cannot wake a peer.
	id, reason, _, err := store.AppendConclavePost(ctx, b.store, msg, store.ConclaveLimits{Now: b.now}, false, false)
	if reason != "" && reason != room.WardReason {
		b.recordWard(ctx, room, msg.TaskID, reason)
	}
	return id, err
}
