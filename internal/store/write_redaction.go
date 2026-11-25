package store

import (
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
)

// RedactTaskForWrite returns the task as it may be stored: its input passes
// the JSON redactor. Every store method that inserts a task row calls it
// first, so no caller can store raw input by forgetting to redact.
func RedactTaskForWrite(t domain.Task) domain.Task {
	t.InputJSON = redact.JSON(t.InputJSON)
	return t
}

// RedactTaskMessageForWrite returns the message as it may be stored. Every
// store method that inserts a task message calls it first.
func RedactTaskMessageForWrite(m domain.TaskMessage) domain.TaskMessage {
	m.Content = redact.String(m.Content)
	return m
}
