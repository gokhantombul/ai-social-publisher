// Package http wires the chi router, middleware and handlers for the HTTP API.
package http

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"ai-social-publisher/internal/account"
	"ai-social-publisher/internal/approval"
	"ai-social-publisher/internal/config"
	"ai-social-publisher/internal/news"
	"ai-social-publisher/internal/post"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Handler holds the dependencies the HTTP endpoints need.
type Handler struct {
	cfg      *config.Config
	accounts *account.Repository
	news     *news.Repository
	posts    *post.Repository
	approval *approval.Service
	logger   *slog.Logger
}

// Deps bundles handler dependencies.
type Deps struct {
	Config   *config.Config
	Accounts *account.Repository
	News     *news.Repository
	Posts    *post.Repository
	Approval *approval.Service
	Logger   *slog.Logger
	// StaticDir is served at /static so rendered media has a public URL.
	StaticDir string
}

// NewServer builds an *http.Server with the full router.
func NewServer(d Deps) *http.Server {
	h := &Handler{
		cfg:      d.Config,
		accounts: d.Accounts,
		news:     d.News,
		posts:    d.Posts,
		approval: d.Approval,
		logger:   d.Logger.With("component", "http"),
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(requestLogger(h.logger))
	r.Use(middleware.Timeout(60 * time.Second))

	r.Get("/health", h.health)

	r.Route("/api", func(r chi.Router) {
		r.Get("/accounts", h.listAccounts)
		r.Post("/accounts", h.createAccount)

		r.Post("/news/sync", h.syncNews)
		r.Get("/news/candidates", h.listCandidates)

		r.Get("/posts", h.listPosts)
		r.Get("/posts/{id}", h.getPost)
		r.Post("/posts/{id}/generate", h.generatePost)
		r.Post("/posts/{id}/approve", h.approvePost)
		r.Post("/posts/{id}/reject", h.rejectPost)
		r.Post("/posts/{id}/publish", h.publishPost)

		r.Post("/telegram/callback", h.telegramCallback)

		r.Get("/analytics/posts", h.analyticsPosts)
	})

	// Serve rendered media so Instagram (and humans) can fetch the public URL.
	if d.StaticDir != "" {
		fs := http.StripPrefix("/static/", http.FileServer(http.Dir(d.StaticDir)))
		r.Handle("/static/*", fs)
	}

	return &http.Server{
		Addr:              fmt.Sprintf(":%d", d.Config.App.HTTPPort),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// requestLogger logs each request at info level.
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

// shutdownTimeout is exported for the main package to reuse.
const shutdownTimeout = 10 * time.Second

// Shutdown gracefully stops the server.
func Shutdown(ctx context.Context, srv *http.Server) error {
	ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()
	return srv.Shutdown(ctx)
}
