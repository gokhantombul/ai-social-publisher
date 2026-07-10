package approval_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"ai-social-publisher/internal/account"
	"ai-social-publisher/internal/ai"
	"ai-social-publisher/internal/approval"
	"ai-social-publisher/internal/config"
	"ai-social-publisher/internal/database"
	"ai-social-publisher/internal/instagram"
	"ai-social-publisher/internal/media"
	"ai-social-publisher/internal/news"
	"ai-social-publisher/internal/outbox"
	"ai-social-publisher/internal/post"
	"ai-social-publisher/internal/storage"
	"ai-social-publisher/internal/telegram"
)

// TestScheduledPublishWorkflow drives a job to READY_TO_PUBLISH, schedules it for
// the future, confirms it does not publish early, then simulates its time
// arriving and confirms the scheduler publishes it (dry-run) exactly once.
func TestScheduledPublishWorkflow(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := database.Connect(ctx, config.DatabaseConfig{URL: dsn, MaxOpenConns: 8, MaxIdleConns: 2, ConnMaxLifetimeMinutes: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	lockConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockConn.ExecContext(ctx, `SELECT pg_advisory_lock(6599187)`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = lockConn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(6599187)`)
		_ = lockConn.Close()
	}()
	if err := database.Migrate(db, logger); err != nil {
		t.Fatal(err)
	}

	suffix := time.Now().Format("150405.000000000")
	cfg := &config.Config{
		App:             config.AppConfig{PublicBaseURL: "http://localhost/static"},
		PostGeneration:  config.PostGenerationConfig{DefaultVariantCount: 2, MaxVariantCount: 5},
		TelegramService: config.TelegramServiceConfig{BaseURL: "http://127.0.0.1:1", AuthToken: strings.Repeat("x", 32), TimeoutSeconds: 2},
		Instagram:       config.InstagramConfig{PublishEnabled: false},
		Storage:         config.StorageConfig{Driver: "local", BaseDir: t.TempDir()},
		Scheduler:       config.SchedulerConfig{StaleJobTimeoutMinutes: 10},
		Accounts: []config.AccountConfig{{
			Code: "schedwf-" + suffix, Name: "Schedule Workflow", Category: "technology",
			VariantCount: 2, NotifyThreshold: 80,
		}},
	}
	accountRepo := account.NewRepository(db)
	if err := accountRepo.SyncFromConfig(ctx, cfg.Accounts); err != nil {
		t.Fatal(err)
	}
	newsRepo := news.NewRepository(db)
	postRepo := post.NewRepository(db)
	outboxRepo := outbox.NewRepository(db)
	renderer, err := media.NewTemplateRenderer(cfg.Storage.BaseDir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewLocalStorage(cfg.Storage, cfg.App.PublicBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	service := approval.NewService(approval.Deps{
		Config: cfg, Accounts: accountRepo, News: newsRepo, Posts: postRepo,
		AI: ai.NewService(logger, workflowAI{}), Telegram: telegram.NewClient(cfg.TelegramService),
		Renderer: renderer, Storage: store, Publisher: instagram.NewPublisher(cfg.Instagram, logger),
		Outbox: outboxRepo, Logger: logger,
	})

	candidate, _, err := newsRepo.Upsert(ctx, news.Candidate{
		ExternalNewsID: "schedwf-" + suffix, Title: "Scheduled workflow test", Summary: "summary",
		Source: "test", SourceURL: "https://example.com/news", Category: "technology",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), `DELETE FROM news_candidates WHERE id=$1`, candidate.ID); err != nil {
			t.Errorf("cleanup candidate: %v", err)
		}
		if _, err := db.ExecContext(context.Background(), `DELETE FROM social_accounts WHERE code=$1`, cfg.Accounts[0].Code); err != nil {
			t.Errorf("cleanup account: %v", err)
		}
	})

	// Drive the pipeline to READY_TO_PUBLISH with a rendered preview.
	if err := service.ProcessCandidate(ctx, *candidate); err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessScoringQueue(ctx); err != nil {
		t.Fatal(err)
	}
	acct, _ := accountRepo.GetByCategory(ctx, "technology")
	job, _, err := postRepo.GetOrCreate(ctx, candidate.ID, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	// This test intentionally leaves its notifications undelivered, so remove the
	// outbox rows it produced (keys "...:<jobID>" and "...:<jobID>:...") to keep the
	// shared notification_outbox clean for the outbox integration test. This runs as
	// a defer (before the advisory lock is released) rather than t.Cleanup so the
	// rows are gone before another package's integration test can observe them.
	defer func() {
		suffixKey := fmt.Sprintf("%%:%d", job.ID)   // e.g. "first-approval:7", "published:7"
		infixKey := fmt.Sprintf("%%:%d:%%", job.ID) // e.g. "variant-content:7:3", "scheduled:7:..."
		if _, err := db.ExecContext(context.Background(),
			`DELETE FROM notification_outbox WHERE dedupe_key LIKE $1 OR dedupe_key LIKE $2`,
			suffixKey, infixKey); err != nil {
			t.Errorf("cleanup outbox: %v", err)
		}
	}()
	if err := service.GenerateVariants(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessVariantQueue(ctx); err != nil {
		t.Fatal(err)
	}
	variants, err := postRepo.ListVariants(ctx, job.ID)
	if err != nil || len(variants) != 2 {
		t.Fatalf("expected two variants, got %d err=%v", len(variants), err)
	}
	if err := service.SelectVariantForReview(ctx, job.ID, variants[0].ID); err != nil {
		t.Fatal(err)
	}

	// Schedule for the future: the job must move to SCHEDULED and not publish yet.
	if err := service.SchedulePublish(ctx, job.ID, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	scheduled, err := postRepo.GetByID(ctx, job.ID)
	if err != nil || scheduled.Status != post.StatusScheduled || scheduled.ScheduledPublishAt == nil {
		t.Fatalf("expected SCHEDULED with a time, got %+v err=%v", scheduled, err)
	}
	if err := service.ProcessScheduledPublishes(ctx); err != nil {
		t.Fatal(err)
	}
	stillScheduled, err := postRepo.GetByID(ctx, job.ID)
	if err != nil || stillScheduled.Status != post.StatusScheduled {
		t.Fatalf("future schedule must not publish early, got %+v err=%v", stillScheduled, err)
	}

	// Rejecting a past time is a client error, not a state change.
	if err := service.SchedulePublish(ctx, job.ID, time.Now().Add(-time.Hour)); err == nil {
		t.Fatal("scheduling in the past should be rejected")
	}

	// Simulate the target time arriving, then let the scheduler publish it.
	if err := postRepo.Reschedule(ctx, job.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessScheduledPublishes(ctx); err != nil {
		t.Fatal(err)
	}
	published, err := postRepo.GetByID(ctx, job.ID)
	if err != nil || published.Status != post.StatusPublished {
		t.Fatalf("expected PUBLISHED after due, got %+v err=%v", published, err)
	}
	if published.InstagramMediaID == "" {
		t.Fatal("expected a media id after publish")
	}
	if published.ScheduledPublishAt == nil {
		t.Fatal("scheduled time should be retained after publish for audit")
	}
}
