package conclave

import (
	"context"
	"encoding/json/v2"
	"slices"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// CouncilNothingToAdd is a vote only when it is the entire trimmed final answer.
const CouncilNothingToAdd = "NOTHING TO ADD"

// RecordCouncilAnswer links a completed current attempt's persisted final answer.
// The room driver must reconcile results before reserving retries or closing.
func (b *Broker) RecordCouncilAnswer(ctx context.Context, roomID, taskID string) (bool, error) {
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
	artifacts := councilArtifacts(task.ArtifactsJSON)
	if artifacts == nil {
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
	if turn.Role == snap.Checkout.WriterRole {
		diff, valid := controlplane.CouncilWriterDiff(a, taskID, artifacts)
		if !valid || !writerDiffRecorded(snap, turn, diff) {
			return false, nil
		}
	}
	body, vote := councilFinalAnswer(artifacts)
	if body == "" {
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
	id, reason, err := store.AppendReservedCouncilAnswer(ctx, b.store, msg, vote, b.councilLimits(b.bundle(), snap.Session))
	if reason != "" && reason != snap.Session.WardReason {
		b.recordWard(ctx, snap.Session, taskID, reason)
	}
	if err != nil {
		return false, err
	}
	if id <= 0 {
		return false, store.ErrConflict
	}
	return true, nil
}

func writerDiffRecorded(s store.CouncilSnapshot, turn store.CouncilTurn, diff *domain.ConclaveRelic) bool {
	for _, a := range s.Attempts {
		if a.TaskID != turn.TaskID || a.Role != turn.Role || a.Round != turn.Round || a.Number != turn.Starts || a.State != domain.TaskStateCompleted {
			continue
		}
		for _, relic := range a.Relics {
			if relic.Name == controlplane.CouncilWriterDiffName(a.TaskID) {
				return diff != nil && relic.ConclaveID == diff.ConclaveID && relic.AuthorRole == diff.AuthorRole &&
					relic.ContentType == diff.ContentType && relic.Body == diff.Body
			}
		}
		return diff == nil
	}
	return false
}

func councilArtifacts(raw string) []*scruntime.Artifact {
	if len(raw) > store.MaxConclaveTurnInput {
		return nil
	}
	var artifacts []*scruntime.Artifact
	if json.Unmarshal([]byte(raw), &artifacts, json.RejectUnknownMembers(true)) != nil || slices.Contains(artifacts, nil) {
		return nil
	}
	return artifacts
}

func councilFinalAnswer(artifacts []*scruntime.Artifact) (body string, vote bool) {
	var final *scruntime.Artifact
	for _, a := range artifacts {
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
	body = store.RedactCouncilAnswer(rawBody)
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
