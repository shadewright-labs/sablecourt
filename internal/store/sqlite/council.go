package sqlite

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

var _ store.CouncilStore = (*SQLiteStore)(nil)

const maxCouncilText = 64 << 10

func councilText(s string) bool {
	return strings.TrimSpace(s) != "" && utf8.ValidString(s) && len(s) <= maxCouncilText
}

func validPolicy(p *store.CouncilPolicy) error {
	if !councilText(p.Question) || p.MaxRounds < 1 || p.MaxStarts < 1 || p.Deadline.IsZero() || len(p.Members) == 0 ||
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
		if !councilText(v.Role) || strings.TrimSpace(v.Role) != v.Role || strings.HasPrefix(v.Role, "@") ||
			v.Role == "all" || v.Role == "system" || v.Role == "operator" || zones[v.Role] != "" || !councilText(v.Workload) ||
			(m.Zone != domain.ZoneSandbox && m.Zone != domain.ZoneRuntime) || len(m.RoleCard) > maxCouncilText ||
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
	if p.Wards.MaxMessages == 0 {
		p.Wards.MaxMessages = 2 * len(p.Members)
	}
	if p.Wards.MaxPingPong == 0 {
		p.Wards.MaxPingPong = 2 * len(p.Members)
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

func (s *SQLiteStore) CreateCouncil(ctx context.Context, room domain.ConclaveSession, spec store.CouncilSpec) error {
	if !councilText(room.ID) || room.State != domain.ConclaveStateActive || room.WardReason != "" {
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := createConclave(ctx, tx, room); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO councils(id,policy) VALUES (?,?)", room.ID, string(raw)); err != nil {
		return err
	}
	return tx.Commit()
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
