// Package http wires the chi router, middleware and handlers for the HTTP API.
package http

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"ai-social-publisher/internal/account"
	"ai-social-publisher/internal/approval"
	"ai-social-publisher/internal/config"
	"ai-social-publisher/internal/news"
	"ai-social-publisher/internal/outbox"
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
	db       *sql.DB
	outbox   *outbox.Repository
	apiLimit *ipRateLimiter
	cbLimit  *ipRateLimiter
}

// Deps bundles handler dependencies.
type Deps struct {
	Config   *config.Config
	Accounts *account.Repository
	News     *news.Repository
	Posts    *post.Repository
	Approval *approval.Service
	Logger   *slog.Logger
	DB       *sql.DB
	Outbox   *outbox.Repository
	// Admin is the embedded operator console mounted at the root.
	Admin http.Handler
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
		db:       d.DB,
		outbox:   d.Outbox,
		apiLimit: newIPRateLimiter(120, time.Minute),
		cbLimit:  newIPRateLimiter(60, time.Minute),
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(requestLogger(h.logger))
	r.Use(middleware.Timeout(60 * time.Second))
	r.Use(baseSecurityHeaders(d.Config.App.Env == "production"))

	r.Get("/live", h.live)
	r.Get("/health", h.health)
	r.Get("/ready", h.health)

	r.Route("/api", func(r chi.Router) {
		// telegram-service authenticates callbacks with a body HMAC rather than
		// the administrative bearer token.
		r.With(h.cbLimit.middleware, h.telegramCallbackAuth).Post("/telegram/callback", h.telegramCallback)

		r.Group(func(r chi.Router) {
			r.Use(h.apiLimit.middleware)
			r.Use(h.apiAuth)
			r.Use(maxBodyBytes(1 << 20))
			r.Get("/accounts", h.listAccounts)

			r.Post("/news/sync", h.syncNews)
			r.Get("/news/candidates", h.listCandidates)

			r.Get("/posts", h.listPosts)
			r.Get("/posts/{id}", h.getPost)
			r.Post("/posts/{id}/generate", h.generatePost)
			r.Post("/posts/{id}/approve", h.approvePost)
			r.Post("/posts/{id}/reject", h.rejectPost)
			r.Post("/posts/{id}/publish", h.publishPost)
			r.Post("/posts/{id}/schedule", h.schedulePost)
			r.Post("/posts/{id}/unschedule", h.unschedulePost)

			r.Get("/analytics/posts", h.analyticsPosts)
		})
	})

	// Serve rendered media so Instagram (and humans) can fetch the public URL.
	if d.StaticDir != "" {
		fs := http.StripPrefix("/static/", staticFileServer(d.StaticDir))
		r.Handle("/static/*", fs)
	}
	if d.Admin != nil {
		r.Mount("/", d.Admin)
	}

	return &http.Server{
		Addr:              fmt.Sprintf(":%d", d.Config.App.HTTPPort),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      65 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// baseSecurityHeaders applies headers every response should carry, including
// the API and rendered media. The admin console layers its CSP on top.
func baseSecurityHeaders(production bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "same-origin")
			if production {
				h.Set("Strict-Transport-Security", "max-age=31536000")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// staticFileServer serves rendered media files. Unlike a bare http.FileServer
// it refuses directory listings, dotfiles and in-progress upload temp files so
// unauthenticated clients cannot enumerate unpublished drafts.
func staticFileServer(dir string) http.Handler {
	root := http.Dir(dir)
	files := http.FileServer(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		clean := path.Clean("/" + r.URL.Path)
		if strings.HasPrefix(path.Base(clean), ".") || clean == "/" {
			http.NotFound(w, r)
			return
		}
		f, err := root.Open(clean)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		info, err := f.Stat()
		_ = f.Close()
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		// Rendered file names are unique per render, so contents never change.
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		files.ServeHTTP(w, r)
	})
}

// requestLogger logs each request at info level.
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			logger.Info("request",
				"request_id", middleware.GetReqID(r.Context()),
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
