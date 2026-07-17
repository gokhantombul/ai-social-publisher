// Command server is the single entry point for ai-social-publisher. It runs the
// HTTP API and background scheduler, and supports `migrate up|down`.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"ai-social-publisher/internal/account"
	"ai-social-publisher/internal/admin"
	"ai-social-publisher/internal/ai"
	"ai-social-publisher/internal/approval"
	"ai-social-publisher/internal/config"
	"ai-social-publisher/internal/database"
	httpserver "ai-social-publisher/internal/http"
	"ai-social-publisher/internal/instagram"
	"ai-social-publisher/internal/media"
	"ai-social-publisher/internal/news"
	"ai-social-publisher/internal/outbox"
	"ai-social-publisher/internal/post"
	"ai-social-publisher/internal/scheduler"
	"ai-social-publisher/internal/storage"
	"ai-social-publisher/internal/telegram"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	// Resolve subcommand (serve|migrate). Defaults to serve.
	args := os.Args[1:]
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}

	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file (default: config.yaml, falls back to config.example.yaml)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	loadDotEnv(".env", logger)

	cfg, err := config.Load(resolveConfigPath(*configPath, logger))
	if err != nil {
		return err
	}

	ctx := context.Background()
	db, err := database.Connect(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()

	switch command {
	case "migrate":
		return runMigrate(fs.Args(), db, logger)
	case "serve":
		return serve(cfg, db, logger)
	default:
		return fmt.Errorf("unknown command %q (use: serve | migrate up|down)", command)
	}
}

func runMigrate(rest []string, db *sql.DB, logger *slog.Logger) error {
	direction := "up"
	if len(rest) > 0 {
		direction = rest[0]
	}
	switch direction {
	case "up":
		return database.Migrate(db, logger)
	case "down":
		return database.MigrateDown(db, logger)
	default:
		return fmt.Errorf("unknown migrate direction %q (use up|down)", direction)
	}
}

func serve(cfg *config.Config, db *sql.DB, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.Database.AutoMigrate {
		if err := database.Migrate(db, logger); err != nil {
			return fmt.Errorf("auto-migrate: %w", err)
		}
		logger.Info("migrations applied")
	}

	// Repositories.
	accountRepo := account.NewRepository(db)
	newsRepo := news.NewRepository(db)
	postRepo := post.NewRepository(db)
	outboxRepo := outbox.NewRepository(db)

	if err := accountRepo.SyncFromConfig(ctx, cfg.Accounts); err != nil {
		return fmt.Errorf("sync accounts: %w", err)
	}
	logger.Info("accounts synced from config", "count", len(cfg.Accounts))

	// AI provider chain: tgpt primary, ollama fallback.
	aiSvc := ai.NewService(logger,
		ai.NewTgptProvider(cfg.AI.Providers.Tgpt, logger),
		ai.NewOllamaProvider(cfg.AI.Providers.Ollama, logger),
	)

	// External clients.
	newsClient := news.NewClient(cfg.NewsService)
	telegramClient := telegram.NewClient(cfg.TelegramService)
	publisher := instagram.NewPublisher(cfg.Instagram, logger)

	// Storage + media renderer.
	store, err := storage.NewLocalStorage(cfg.Storage, cfg.App.PublicBaseURL)
	if err != nil {
		return err
	}
	renderer, err := media.NewTemplateRenderer(cfg.Storage.BaseDir)
	if err != nil {
		return err
	}

	approvalSvc := approval.NewService(approval.Deps{
		Config:     cfg,
		Accounts:   accountRepo,
		News:       newsRepo,
		Posts:      postRepo,
		AI:         aiSvc,
		Telegram:   telegramClient,
		Renderer:   renderer,
		Storage:    store,
		Publisher:  publisher,
		NewsClient: newsClient,
		Outbox:     outboxRepo,
		Logger:     logger,
	})

	adminHandler, err := admin.New(admin.Deps{
		Config: cfg, DB: db, Approval: approvalSvc, Accounts: accountRepo,
		Posts: postRepo, Outbox: outboxRepo, Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("initialize admin console: %w", err)
	}

	// Background workers.
	sched := scheduler.New(cfg.Scheduler, approvalSvc, store, cfg.Storage.RetentionDays, logger)
	sched.Start(ctx)

	// HTTP server.
	srv := httpserver.NewServer(httpserver.Deps{
		Config:    cfg,
		Accounts:  accountRepo,
		News:      newsRepo,
		Posts:     postRepo,
		Approval:  approvalSvc,
		Logger:    logger,
		DB:        db,
		Outbox:    outboxRepo,
		StaticDir: store.BaseDir(),
		Admin:     adminHandler,
	})

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", srv.Addr)
		serverErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		// Listen failed: stop the background workers and drain them before
		// exiting so in-flight work is not killed mid-write.
		stop()
		sched.Wait()
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	if err := httpserver.Shutdown(context.Background(), srv); err != nil {
		logger.Error("http shutdown error", "error", err)
	}
	sched.Wait()
	logger.Info("shutdown complete")
	return nil
}

// resolveConfigPath returns the config path to load, preferring an explicit
// flag, then config.yaml, then config.example.yaml.
func resolveConfigPath(explicit string, logger *slog.Logger) string {
	if explicit != "" {
		return explicit
	}
	if _, err := os.Stat("config.yaml"); err == nil {
		return "config.yaml"
	}
	if _, err := os.Stat("config.example.yaml"); err == nil {
		logger.Warn("config.yaml not found, using config.example.yaml")
		return "config.example.yaml"
	}
	return "config.yaml"
}

// loadDotEnv loads KEY=VALUE pairs from a .env file into the process environment
// without overriding variables already set. It is best-effort.
func loadDotEnv(path string, logger *slog.Logger) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return // no .env is fine
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				logger.Warn("failed to set environment variable", "key", key, "error", err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		logger.Warn("failed while reading .env file", "path", path, "error", err)
	}
	logger.Info("loaded .env file", "path", path)
}
