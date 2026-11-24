package controlplane

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// Scheduler triggers declared Schedule manifests via robfig/cron/v3 using deterministic idempotency keys.
type Scheduler struct {
	reconciler *Reconciler
	now        func() time.Time

	mu     sync.Mutex
	runner *cron.Cron

	// fired tracks reserved and completed UTC minute slots per workflow schedule.
	// A zero timestamp is an in-flight reservation and is never pruned.
	firedMu sync.Mutex
	fired   map[string]map[string]time.Time
}

func NewScheduler(reconciler *Reconciler) *Scheduler {
	return &Scheduler{
		reconciler: reconciler,
		now:        func() time.Time { return time.Now().UTC() },
		fired:      make(map[string]map[string]time.Time),
	}
}

func (s *Scheduler) Reload(ctx context.Context, bundle *config.Bundle) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.runner != nil {
		s.runner.Stop()
		s.runner = nil
	}
	if bundle == nil || len(bundle.Schedules) == 0 {
		return nil
	}

	c := cron.New(cron.WithLocation(time.UTC))
	for _, sc := range bundle.Schedules {
		if !sc.Spec.Enabled {
			continue
		}
		captured := sc
		if _, err := c.AddFunc(captured.Spec.Cron, func() {
			s.Fire(ctx, captured)
		}); err != nil {
			c.Stop()
			return fmt.Errorf("register schedule %q: %w", captured.Metadata.Name, err)
		}
	}
	c.Start()
	s.runner = c
	return nil
}

func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runner != nil {
		s.runner.Stop()
		s.runner = nil
	}
}

// Fire executes one idempotent trigger of a Schedule manifest for the current UTC minute slot.
func (s *Scheduler) Fire(ctx context.Context, sc domain.Schedule) error {
	if s.reconciler == nil {
		return nil
	}
	now := s.now().UTC()
	slot := now.Format("200601021504")
	idemKey := fmt.Sprintf("sched:%s:%s", sc.Metadata.Name, slot)

	switch sc.Spec.TargetKind {
	case "workload":
		input := sc.Spec.Input
		if input == "" {
			input = "{}"
		}
		_, err := s.reconciler.SubmitTask(ctx, sc.Spec.TargetName, idemKey, input)
		return err
	case "workflow":
		s.firedMu.Lock()
		if s.fired == nil {
			s.fired = make(map[string]map[string]time.Time)
		}
		slots := s.fired[sc.Metadata.Name]
		if _, seen := slots[slot]; seen {
			s.firedMu.Unlock()
			return nil
		}
		if slots == nil {
			slots = make(map[string]time.Time)
			s.fired[sc.Metadata.Name] = slots
		}
		cutoff := now.Add(-24 * time.Hour)
		for k, ts := range slots {
			if !ts.IsZero() && ts.Before(cutoff) {
				delete(slots, k)
			}
		}
		slots[slot] = time.Time{}
		s.firedMu.Unlock()

		_, err := s.reconciler.StartWorkflow(ctx, sc.Spec.TargetName, sc.Spec.Input)

		s.firedMu.Lock()
		if err != nil {
			delete(slots, slot)
		} else {
			slots[slot] = now
		}
		s.firedMu.Unlock()
		return err
	default:
		return fmt.Errorf("unsupported schedule targetKind %q", sc.Spec.TargetKind)
	}
}

// TriggerNow executes a manual operator run of a declared Schedule with a unique idempotency key.
func (s *Scheduler) TriggerNow(ctx context.Context, sc domain.Schedule) error {
	if s.reconciler == nil {
		return nil
	}
	idemKey := fmt.Sprintf("sched-manual:%s:%d", sc.Metadata.Name, s.now().UnixNano())
	switch sc.Spec.TargetKind {
	case "workload":
		input := sc.Spec.Input
		if input == "" {
			input = "{}"
		}
		_, err := s.reconciler.SubmitTask(ctx, sc.Spec.TargetName, idemKey, input)
		return err
	case "workflow":
		_, err := s.reconciler.StartWorkflow(ctx, sc.Spec.TargetName, sc.Spec.Input)
		return err
	default:
		return fmt.Errorf("unsupported schedule targetKind %q", sc.Spec.TargetKind)
	}
}
