package controlplane

import (
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
)

// TaskForRead returns task as it leaves the daemon to a reader (the task API,
// A2A tasks/get, the dashboard). InputJSON and ArtifactsJSON are declared JSON
// records: each is decoded once and refused whole, with none of its payload,
// when it shows a value the daemon holds now, even if the value was registered
// after the task was stored. The caller-supplied IdempotencyKey is plain text,
// so held values in it are replaced in place by literal match. The stored row
// is not changed, so idempotent replay still matches. Every read path calls
// this one function, as the workload gate does for its own copy.
func TaskForRead(task domain.Task) domain.Task {
	reg := redact.DefaultRegistry()
	task.InputJSON, _ = reg.RedactJSONRecord(task.InputJSON)
	task.ArtifactsJSON, _ = reg.RedactJSONRecord(task.ArtifactsJSON)
	task.IdempotencyKey = reg.RedactExact(task.IdempotencyKey)
	return task
}

// TasksForRead applies TaskForRead to each task, in place.
func TasksForRead(tasks []domain.Task) []domain.Task {
	for i := range tasks {
		tasks[i] = TaskForRead(tasks[i])
	}
	return tasks
}

// TaskMessagesForRead applies the record rule to each stored task message, in
// place: the first message is the task input, and a message that shows a held
// value is refused whole.
func TaskMessagesForRead(msgs []domain.TaskMessage) []domain.TaskMessage {
	reg := redact.DefaultRegistry()
	for i := range msgs {
		msgs[i].Content, _ = reg.RedactJSONRecord(msgs[i].Content)
	}
	return msgs
}
