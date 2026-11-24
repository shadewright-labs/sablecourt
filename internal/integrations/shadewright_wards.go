package integrations

import (
	"context"
	"errors"

	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func requireActiveConclave(sess domain.ConclaveSession) error {
	switch sess.State {
	case domain.ConclaveStateActive:
		return nil
	case domain.ConclaveStateInputRequired:
		return conclave.ErrConclavePaused
	default:
		return conclave.ErrConclaveClosed
	}
}

func (h *ShadewrightHandler) postVerification(ctx context.Context, sess domain.ConclaveSession, body string, ensureRelic func() error) (domain.ConclaveMessage, error) {
	var bundle *config.Bundle
	if h.getBundle != nil {
		bundle = h.getBundle()
	}
	if bundle == nil {
		return domain.ConclaveMessage{}, errors.New("verification conclave template unavailable")
	}
	tpl, ok := bundle.Conclaves[sess.TemplateName]
	if !ok {
		return domain.ConclaveMessage{}, errors.New("verification conclave template unavailable")
	}
	if _, ok := bundle.Budgets[tpl.Spec.BudgetRef]; tpl.Spec.BudgetRef != "" && !ok {
		return domain.ConclaveMessage{}, errors.New("verification conclave budget unavailable")
	}

	// The broker runs the normal Wards with a request-local routing policy for
	// this integration. No workload or mention dispatcher belongs to this role.
	tpl.Spec.Vassals = []domain.ConclaveVassal{{Role: ShadewrightRole, CanMessage: []string{"operator"}}}
	policy := &config.Bundle{
		Conclaves: map[string]domain.Conclave{sess.TemplateName: tpl},
		Budgets:   bundle.Budgets,
	}
	broker := conclave.NewBroker(conclave.Options{
		Store:     &verificationReportStore{Store: h.store, ensureRelic: ensureRelic},
		GetBundle: func() *config.Bundle { return policy },
		Now:       h.now,
	})
	msg, err := broker.PostMessage(ctx, sess.ID, ShadewrightRole, "operator", ShadewrightReportKind, body, "")
	if err == nil {
		msg.Kind = ShadewrightReportKind
	}
	return msg, err
}

type verificationReportStore struct {
	store.Store
	ensureRelic func() error
	evidenceErr error
}

func (s *verificationReportStore) AppendConclaveMessage(ctx context.Context, msg domain.ConclaveMessage) (int64, error) {
	if msg.FromRole == ShadewrightRole {
		if s.evidenceErr != nil {
			return 0, s.evidenceErr
		}
		// This callback runs after the Ward checks pass, before the report is visible.
		if err := s.ensureRelic(); err != nil {
			return 0, err
		}
		msg.Kind = ShadewrightReportKind
	}
	return s.Store.AppendConclaveMessage(ctx, msg)
}

func (s *verificationReportStore) ListTasks(ctx context.Context) ([]domain.Task, error) {
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		s.evidenceErr = err
	}
	return tasks, err
}

func (s *verificationReportStore) TaskBudgetTotals(ctx context.Context, taskID string) (domain.BudgetTotals, error) {
	totals, err := s.Store.TaskBudgetTotals(ctx, taskID)
	if err != nil {
		s.evidenceErr = err
	}
	return totals, err
}
