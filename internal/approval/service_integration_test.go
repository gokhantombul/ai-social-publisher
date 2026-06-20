package approval_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
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

type workflowAI struct{}

func (workflowAI) Name() string                     { return "workflow-test" }
func (workflowAI) IsAvailable(context.Context) bool { return true }
func (workflowAI) ScoreNews(context.Context, ai.NewsCandidate) (*ai.NewsScore, error) {
	return &ai.NewsScore{ImportanceScore: 95, ViralityScore: 90, AccountFit: "technology", ShouldNotify: true, RiskLevel: "low", Reason: "test"}, nil
}
func (workflowAI) GeneratePostVariants(_ context.Context, req ai.GeneratePostVariantsRequest) ([]ai.PostVariant, error) {
	out := make([]ai.PostVariant, req.VariantCount)
	for i := range out {
		out[i] = ai.PostVariant{VariantNo: i + 1, Style: "test", Caption: "caption"}
	}
	return out, nil
}

func TestDurableApprovalWorkflow(t *testing.T) {
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
	defer db.Close()
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

	var deliveries atomic.Int32
	telegramServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer 12345678901234567890123456789012" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var n telegram.Notification
		if err := json.NewDecoder(r.Body).Decode(&n); err != nil || n.Title == "" {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		deliveries.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer telegramServer.Close()

	suffix := time.Now().Format("150405.000000000")
	cfg := &config.Config{
		App:             config.AppConfig{PublicBaseURL: "http://localhost/static"},
		PostGeneration:  config.PostGenerationConfig{DefaultVariantCount: 2, MaxVariantCount: 5},
		TelegramService: config.TelegramServiceConfig{BaseURL: telegramServer.URL, AuthToken: "12345678901234567890123456789012", TimeoutSeconds: 2},
		Instagram:       config.InstagramConfig{PublishEnabled: false},
		Storage:         config.StorageConfig{Driver: "local", BaseDir: t.TempDir()},
		Scheduler:       config.SchedulerConfig{StaleJobTimeoutMinutes: 10},
		Accounts: []config.AccountConfig{{
			Code: "workflow-" + suffix, Name: "Workflow", Category: "technology",
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
		ExternalNewsID: "workflow-" + suffix, Title: "Workflow test", Summary: "summary",
		Source: "test", SourceURL: "https://example.com/news", Category: "technology",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := db.ExecContext(ctx, `DELETE FROM news_candidates WHERE id=$1`, candidate.ID); err != nil {
			t.Errorf("cleanup candidate: %v", err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM social_accounts WHERE code=$1`, cfg.Accounts[0].Code); err != nil {
			t.Errorf("cleanup account: %v", err)
		}
	}()

	if err := service.ProcessCandidate(ctx, *candidate); err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessScoringQueue(ctx); err != nil {
		t.Fatal(err)
	}
	acct, _ := accountRepo.GetByCategory(ctx, "technology")
	job, _, err := postRepo.GetOrCreate(ctx, candidate.ID, acct.ID)
	if err != nil || job.Status != post.StatusWaitingFirstApproval {
		t.Fatalf("expected first approval, job=%+v err=%v", job, err)
	}
	if err := service.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
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
	if err := service.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if err := service.SelectVariantForReview(ctx, job.ID, variants[0].ID); err != nil {
		t.Fatal(err)
	}
	ready, err := postRepo.GetByID(ctx, job.ID)
	if err != nil || ready.Status != post.StatusReadyToPublish {
		t.Fatalf("expected ready to publish, job=%+v err=%v", ready, err)
	}
	preview, err := postRepo.GetVariantByID(ctx, variants[0].ID)
	if err != nil || preview.ImageURL == "" {
		t.Fatalf("expected real preview URL, variant=%+v err=%v", preview, err)
	}
	if err := service.UpdateVariantCaption(ctx, job.ID, variants[0].ID, "  edited caption  "); err != nil {
		t.Fatal(err)
	}
	edited, err := postRepo.GetVariantByID(ctx, variants[0].ID)
	if err != nil || edited.Caption != "edited caption" || edited.ImageURL == "" {
		t.Fatalf("expected edited caption and regenerated preview, variant=%+v err=%v", edited, err)
	}
	if err := service.QueuePublish(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	approved, err := postRepo.GetByID(ctx, job.ID)
	if err != nil || approved.Status != post.StatusApproved {
		t.Fatalf("expected approved publish queue state, job=%+v err=%v", approved, err)
	}
	if err := service.PublishApproved(ctx); err != nil {
		t.Fatal(err)
	}
	published, err := postRepo.GetByID(ctx, job.ID)
	if err != nil || published.Status != post.StatusPublished {
		t.Fatalf("expected published, job=%+v err=%v", published, err)
	}
	if err := service.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if deliveries.Load() != 5 { // first approval + 2 captions + selection + published
		t.Fatalf("expected 5 durable notifications, got %d", deliveries.Load())
	}
}
