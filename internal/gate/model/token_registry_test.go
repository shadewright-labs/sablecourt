package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
)

// A minted task token is registered for exact-match redaction until its lease
// expires plus redact.ExpiredSecretRetention, then dropped, so the registry
// does not keep every token the daemon has issued.
func TestMintTaskTokenRegistersUntilLeaseExpiry(t *testing.T) {
	ctx := context.Background()
	_, st := setupGateTest(t, nil, nil)
	for _, id := range []string{"t-live", "t-old"} {
		if err := st.CreateTask(ctx, domain.Task{ID: id, IdempotencyKey: "k-" + id, WorkloadName: "ai-agent", State: domain.TaskStateWorking}); err != nil {
			t.Fatal(err)
		}
	}
	exact := func(token string) bool {
		return slices.Contains(redact.Text("x "+token).Categories, redact.KnownSecretCategory)
	}

	live, err := MintTaskToken(ctx, st, "t-live", "ai-agent", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !exact(live) {
		t.Fatal("live task token is not registered for exact-match redaction")
	}

	past := time.Now().Add(-redact.ExpiredSecretRetention - 2*time.Hour)
	old, err := MintTaskTokenUntil(ctx, st, "t-old", "ai-agent", past, past.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// The next registration prunes values past expiry plus retention.
	redact.Register("prune_trigger_" + live[len(TokenPrefix):])
	if exact(old) {
		t.Fatal("task token stayed registered after its lease expired")
	}
	if !exact(live) {
		t.Fatal("live task token was dropped")
	}
}
