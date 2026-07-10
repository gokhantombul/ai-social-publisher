// Package scheduler runs the background workers in-process: periodic news sync,
// WAITING_AI retries and publishing of approved jobs.
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"ai-social-publisher/internal/approval"
	"ai-social-publisher/internal/config"
	"ai-social-publisher/internal/storage"
)

// Scheduler owns the background goroutines.
type Scheduler struct {
	cfg           config.SchedulerConfig
	approval      *approval.Service
	logger        *slog.Logger
	storage       *storage.LocalStorage
	retentionDays int
	wg            sync.WaitGroup
}

func New(cfg config.SchedulerConfig, svc *approval.Service, store *storage.LocalStorage, retentionDays int, logger *slog.Logger) *Scheduler {
	return &Scheduler{cfg: cfg, approval: svc, storage: store, retentionDays: retentionDays, logger: logger.With("component", "scheduler")}
}

// Start launches the worker loops. They stop when ctx is cancelled. Start
// returns immediately; call Wait to block until all loops have exited.
func (s *Scheduler) Start(ctx context.Context) {
	if !s.cfg.Enabled {
		s.logger.Info("scheduler disabled")
		return
	}

	s.run(ctx, "news-sync", time.Duration(s.cfg.NewsSyncIntervalMinutes)*time.Minute, true, func(ctx context.Context) {
		n, err := s.approval.SyncNews(ctx)
		if err != nil {
			s.logger.Error("news sync error", "error", err)
			return
		}
		if n > 0 {
			s.logger.Info("news sync completed", "new_candidates", n)
		}
	})

	s.run(ctx, "waiting-ai-retry", time.Duration(s.cfg.WaitingAIRetryIntervalMinutes)*time.Minute, false, func(ctx context.Context) {
		if err := s.approval.RetryWaitingAI(ctx); err != nil {
			s.logger.Error("waiting-ai retry error", "error", err)
		}
	})

	s.run(ctx, "scoring-work", time.Duration(s.cfg.WorkIntervalSeconds)*time.Second, true, func(ctx context.Context) {
		if err := s.approval.ProcessScoringQueue(ctx); err != nil {
			s.logger.Error("scoring queue error", "error", err)
		}
	})

	s.run(ctx, "variant-work", time.Duration(s.cfg.WorkIntervalSeconds)*time.Second, true, func(ctx context.Context) {
		if err := s.approval.ProcessVariantQueue(ctx); err != nil {
			s.logger.Error("variant queue error", "error", err)
		}
	})

	s.run(ctx, "notifications", time.Duration(s.cfg.NotificationIntervalSeconds)*time.Second, true, func(ctx context.Context) {
		if err := s.approval.RepairNotifications(ctx); err != nil {
			s.logger.Error("notification repair error", "error", err)
		}
		if err := s.approval.DeliverNotifications(ctx); err != nil {
			s.logger.Error("notification delivery error", "error", err)
		}
	})

	s.run(ctx, "stale-job-recovery", time.Minute, false, func(ctx context.Context) {
		if err := s.approval.RecoverStaleJobs(ctx); err != nil {
			s.logger.Error("stale job recovery error", "error", err)
		}
	})

	s.run(ctx, "media-cleanup", 24*time.Hour, false, func(ctx context.Context) {
		cutoff := time.Now().Add(-time.Duration(s.retentionDays) * 24 * time.Hour)
		removed, err := s.storage.Cleanup(ctx, cutoff)
		if err != nil {
			s.logger.Error("media cleanup error", "error", err)
		} else if removed > 0 {
			s.logger.Info("expired media removed", "count", removed)
		}
	})

	s.run(ctx, "publish-approved", time.Duration(s.cfg.PublishIntervalMinutes)*time.Minute, false, func(ctx context.Context) {
		// Promote any scheduled jobs whose publish time has arrived, then flush
		// the approved queue (which now includes those just promoted).
		if err := s.approval.ProcessScheduledPublishes(ctx); err != nil {
			s.logger.Error("scheduled publish error", "error", err)
		}
		if err := s.approval.PublishApproved(ctx); err != nil {
			s.logger.Error("publish-approved error", "error", err)
		}
	})

	s.logger.Info("scheduler started",
		"news_sync_min", s.cfg.NewsSyncIntervalMinutes,
		"waiting_ai_retry_min", s.cfg.WaitingAIRetryIntervalMinutes,
		"publish_min", s.cfg.PublishIntervalMinutes)
}

// run starts a ticker loop for a single job. If runNow is true the job fires
// once immediately on startup.
func (s *Scheduler) run(ctx context.Context, name string, interval time.Duration, runNow bool, job func(context.Context)) {
	if interval <= 0 {
		interval = time.Minute
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		if runNow {
			s.safe(ctx, name, job)
		}
		for {
			select {
			case <-ctx.Done():
				s.logger.Info("worker stopped", "worker", name)
				return
			case <-ticker.C:
				s.safe(ctx, name, job)
			}
		}
	}()
}

// safe runs a job and recovers from panics so one bad run never kills the loop.
func (s *Scheduler) safe(ctx context.Context, name string, job func(context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("worker panic recovered", "worker", name, "panic", r)
		}
	}()
	job(ctx)
}

// Wait blocks until all worker goroutines have returned.
func (s *Scheduler) Wait() { s.wg.Wait() }
