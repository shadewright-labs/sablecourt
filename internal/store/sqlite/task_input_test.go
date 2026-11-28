package sqlite

import (
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/store/storetest"
)

// TestTaskInputRedactedAtStoreWrite: task input and task messages are stored
// with secret patterns and held values replaced (issue 261).
func TestTaskInputRedactedAtStoreWrite(t *testing.T) {
	storetest.CheckTaskInputRedacted(t, openTestStore(t))
}
