package conclave

import (
	"context"
	"errors"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const CouncilDigestRelicName = store.CouncilDigestRelicName

// CloseCouncil persists the daemon's digest and returns the original Relic on replay.
func (b *Broker) CloseCouncil(ctx context.Context, roomID, reason string) (domain.ConclaveRelic, error) {
	state := domain.ConclaveStateCompleted
	switch reason {
	case "owner_stop", "deadline", "wall_time_reached":
		state = domain.ConclaveStateCancelled
	}
	return b.closeCouncil(ctx, roomID, state, reason)
}

func (b *Broker) closeCouncil(ctx context.Context, roomID string, state domain.ConclaveState, reason string) (domain.ConclaveRelic, error) {
	councils, err := store.Councils(b.store)
	if err != nil {
		return domain.ConclaveRelic{}, err
	}
	return councils.CloseCouncil(ctx, roomID, state, reason)
}

// PrepareCouncilDigest previews the digest from a consistent snapshot.
func (b *Broker) PrepareCouncilDigest(ctx context.Context, roomID, reason string) (domain.ConclaveRelic, error) {
	councils, err := store.Councils(b.store)
	if err != nil {
		return domain.ConclaveRelic{}, err
	}
	snap, err := councils.CouncilSnapshot(ctx, roomID)
	if err != nil {
		return domain.ConclaveRelic{}, err
	}
	if snap.Session.ID != roomID {
		return domain.ConclaveRelic{}, errors.New("wrong council in digest snapshot")
	}
	return snap.Digest(reason)
}
