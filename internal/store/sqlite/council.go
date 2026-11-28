package sqlite

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

var _ store.CouncilStore = (*SQLiteStore)(nil)

func validPolicy(p *store.CouncilPolicy) error {
	if !store.ValidCouncilText(p.Question) || p.MaxRounds < 1 || p.MaxStarts < 1 || p.MaxStarts > domain.ConclaveRunCeiling || p.Deadline.IsZero() || len(p.Members) == 0 ||
		p.Wards.MaxRuns <= 0 || p.Wards.MaxRuns > domain.ConclaveRunCeiling || p.Wards.MaxMessages < 1 || p.Wards.MaxPingPong < 1 {
		return errors.New("invalid council policy or ceilings")
	}
	if b := p.Budget; b != nil && (b.MaxCostUSD < 0 || b.MaxInputTokens < 0 || b.MaxOutputTokens < 0) {
		return errors.New("invalid council budget")
	}
	zones := map[string]domain.WorkloadZone{}
	hasRuntime := false
	for _, m := range p.Members {
		v := m.Vassal
		if !store.ValidCouncilText(v.Role) || strings.TrimSpace(v.Role) != v.Role || strings.HasPrefix(v.Role, "@") ||
			v.Role == "all" || v.Role == "system" || v.Role == "operator" || zones[v.Role] != "" || !store.ValidCouncilText(v.Workload) ||
			(m.Zone != domain.ZoneSandbox && m.Zone != domain.ZoneRuntime) || len(m.RoleCard) > store.MaxCouncilText || !utf8.ValidString(m.RoleCard) ||
			(v.Activation != "" && v.Activation != domain.ActivationAlwaysOn && v.Activation != domain.ActivationOnMention) {
			return errors.New("invalid council member")
		}
		zones[v.Role] = m.Zone
		hasRuntime = hasRuntime || m.Zone == domain.ZoneRuntime
	}
	for _, m := range p.Members {
		for _, to := range m.Vassal.CanMessage {
			if to != "all" && to != "operator" && zones[to] == "" ||
				m.Zone == domain.ZoneSandbox && (zones[to] == domain.ZoneRuntime || to == "all" && hasRuntime) {
				return errors.New("invalid council route")
			}
		}
	}
	c := p.Checkout
	if c.WriterRole != "" && zones[c.WriterRole] == "" {
		return errors.New("unknown council writer")
	}
	if c.Ref != "" || c.Commit != "" || c.WriterRole != "" {
		if len(c.Commit) != 40 && len(c.Commit) != 64 {
			return errors.New("checkout requires a full commit id")
		}
		if _, err := hex.DecodeString(c.Commit); err != nil {
			return errors.New("invalid checkout commit")
		}
	}
	return nil
}

func councilPolicy(created time.Time, spec store.CouncilSpec) (store.CouncilPolicy, error) {
	p := store.CouncilPolicy{Question: spec.Question, Members: slices.Clone(spec.Members), Sequential: spec.Sequential, Checkout: spec.Checkout,
		MaxRounds: 2, MaxStarts: domain.ConclaveRunCeiling, Wards: spec.Wards, Budget: spec.Budget}
	if spec.MaxRounds != nil {
		p.MaxRounds = *spec.MaxRounds
	}
	if spec.MaxStarts != nil {
		p.MaxStarts = *spec.MaxStarts
	}
	duration := store.ConclaveWallTime
	if spec.Duration != nil {
		duration = *spec.Duration
	}
	if duration <= 0 {
		return p, errors.New("invalid council duration")
	}
	p.Deadline = created.UTC().Add(duration)
	if p.Wards.MaxRuns == 0 {
		p.Wards.MaxRuns = domain.ConclaveRunCeiling
	}
	defaultMessageCeiling := max(2*len(p.Members), p.MaxStarts+1)
	if p.Wards.MaxMessages == 0 {
		p.Wards.MaxMessages = defaultMessageCeiling
	}
	if p.Wards.MaxPingPong == 0 {
		p.Wards.MaxPingPong = defaultMessageCeiling
	}
	if err := validPolicy(&p); err != nil {
		return p, err
	}
	p.Question = redact.String(p.Question)
	for i := range p.Members {
		p.Members[i].RoleCard = redact.String(p.Members[i].RoleCard)
	}
	return p, validPolicy(&p)
}

func (s *SQLiteStore) CreateCouncil(ctx context.Context, room domain.ConclaveSession, spec store.CouncilSpec) (err error) {
	defer func() { store.NoteEffectIf(ctx, err, "council %s was opened", room.ID) }()
	if !store.ValidCouncilText(room.ID) || room.State != domain.ConclaveStateActive || room.WardReason != "" {
		return errors.New("invalid opening council state")
	}
	if room.CreatedAt.IsZero() {
		room.CreatedAt = time.Now().UTC()
	}
	if room.UpdatedAt.IsZero() {
		room.UpdatedAt = room.CreatedAt
	}
	p, err := councilPolicy(room.CreatedAt, spec)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if len(raw) > store.MaxConclaveTurnInput {
		return errors.New("council policy too large")
	}
	if spec.IdempotencyKey != "" {
		if len(spec.IdempotencyKey) > 256 || strings.TrimSpace(spec.IdempotencyKey) != spec.IdempotencyKey ||
			strings.ContainsRune(spec.IdempotencyKey, 0) || spec.RequestSHA256 == "" {
			return errors.New("invalid council idempotency metadata")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := createConclave(ctx, tx, room); err != nil {
		return err
	}
	idemKey := sql.NullString{String: spec.IdempotencyKey, Valid: spec.IdempotencyKey != ""}
	cleanupPending := 0
	if p.Checkout.Commit != "" || p.Checkout.WriterRole != "" {
		cleanupPending = 1
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO councils(id,policy,idempotency_key,request_sha256,cleanup_pending) VALUES (?,?,?,?,?)", room.ID, string(raw), idemKey, spec.RequestSHA256, cleanupPending); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return fmt.Errorf("%w: %v", store.ErrConflict, err)
		}
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) CouncilByIdempotencyKey(ctx context.Context, key string) (domain.ConclaveSession, string, error) {
	if key == "" {
		return domain.ConclaveSession{}, "", store.ErrNotFound
	}
	var room domain.ConclaveSession
	var created, updated, digest string
	err := s.db.QueryRowContext(ctx, `
		SELECT c.id, c.template_name, c.title, c.state, c.ward_reason, c.created_at, c.updated_at, p.request_sha256
		FROM councils p JOIN conclaves c ON c.id = p.id
		WHERE p.idempotency_key = ?
	`, key).Scan(&room.ID, &room.TemplateName, &room.Title, &room.State, &room.WardReason, &created, &updated, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ConclaveSession{}, "", store.ErrNotFound
	}
	if err != nil {
		return domain.ConclaveSession{}, "", err
	}
	if room.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return domain.ConclaveSession{}, "", err
	}
	if room.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return domain.ConclaveSession{}, "", err
	}
	return room, digest, nil
}

func (s *SQLiteStore) MarkCouncilCleanedUp(ctx context.Context, id string) (err error) {
	defer func() { store.NoteEffectIf(ctx, err, "council %s was cleaned up", id) }()
	_, err = s.db.ExecContext(ctx, "UPDATE councils SET cleanup_pending = 0 WHERE id = ?", id)
	return err
}

func readCouncilPolicy(ctx context.Context, tx *sql.Tx, id string) (*store.CouncilPolicy, error) {
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT policy FROM councils WHERE id=?", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > store.MaxConclaveTurnInput {
		return nil, errors.New("stored council policy too large")
	}
	var p store.CouncilPolicy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, err
	}
	return &p, validPolicy(&p)
}
