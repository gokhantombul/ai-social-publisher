// Package admin provides the embedded, server-rendered operator console.
package admin

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"ai-social-publisher/internal/account"
	"ai-social-publisher/internal/approval"
	"ai-social-publisher/internal/config"
	"ai-social-publisher/internal/outbox"
	"ai-social-publisher/internal/post"

	"github.com/go-chi/chi/v5"
)

//go:embed assets/* templates/*
var uiFS embed.FS

type Deps struct {
	Config   *config.Config
	DB       *sql.DB
	Approval *approval.Service
	Accounts *account.Repository
	Posts    *post.Repository
	Outbox   *outbox.Repository
	Logger   *slog.Logger
}

type Handler struct {
	cfg       *config.Config
	repo      *Repository
	approval  *approval.Service
	accounts  *account.Repository
	posts     *post.Repository
	outbox    *outbox.Repository
	logger    *slog.Logger
	templates *template.Template
	router    http.Handler
	sessions  sessionCodec
	logins    *loginLimiter
}

func New(d Deps) (*Handler, error) {
	if d.Config == nil || d.DB == nil || d.Approval == nil || d.Accounts == nil || d.Posts == nil || d.Outbox == nil {
		return nil, errors.New("admin console dependencies are incomplete")
	}
	h := &Handler{
		cfg: d.Config, repo: NewRepository(d.DB), approval: d.Approval,
		accounts: d.Accounts, posts: d.Posts, outbox: d.Outbox,
		logger: d.Logger.With("component", "admin"), logins: newLoginLimiter(),
		sessions: newSessionCodec(d.Config.Security.APIToken,
			d.Config.App.Env == "production" || strings.HasPrefix(strings.ToLower(d.Config.App.PublicBaseURL), "https://")),
	}
	tmpl, err := template.New("console").Funcs(template.FuncMap{
		"statusLabel": statusLabel,
		"statusClass": statusClass,
		"formatTime":  formatTime,
		"formatDate":  formatDate,
		"maskID":      maskID,
		"add":         func(a, b int) int { return a + b },
		"sub":         func(a, b int) int { return a - b },
		"eqStatus":    func(a post.Status, b string) bool { return string(a) == b },
	}).ParseFS(uiFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse admin templates: %w", err)
	}
	h.templates = tmpl
	h.router = h.routes()
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.router.ServeHTTP(w, r) }

func (h *Handler) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(h.securityHeaders)

	assets, _ := fs.Sub(uiFS, "assets")
	r.Handle("/assets/*", http.StripPrefix("/assets/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.FileServer(http.FS(assets)).ServeHTTP(w, req)
	})))
	r.Get("/login", h.loginPage)
	r.Post("/login", h.login)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/admin", http.StatusSeeOther) })

	r.Group(func(r chi.Router) {
		r.Use(h.requireSession)
		r.Get("/admin", h.dashboard)
		r.Get("/admin/fragments/dashboard", h.dashboardFragment)
		r.Get("/admin/posts", h.postsPage)
		r.Get("/admin/posts/{id}", h.postPage)
		r.Get("/admin/posts/{id}/fragment", h.postFragment)
		r.Get("/admin/news", h.newsPage)
		r.Get("/admin/accounts", h.accountsPage)
		r.Get("/admin/system", h.systemPage)

		r.With(h.requireCSRF).Post("/logout", h.logout)
		r.With(h.requireCSRF).Post("/admin/news/sync", h.syncNews)
		r.With(h.requireCSRF).Post("/admin/posts/{id}/generate", h.generate)
		r.With(h.requireCSRF).Post("/admin/posts/{id}/regenerate", h.regenerate)
		r.With(h.requireCSRF).Post("/admin/posts/{id}/select", h.selectVariant)
		r.With(h.requireCSRF).Post("/admin/posts/{id}/variants/{variantID}/caption", h.updateCaption)
		r.With(h.requireCSRF).Post("/admin/posts/{id}/publish", h.publish)
		r.With(h.requireCSRF).Post("/admin/posts/{id}/skip", h.skip)
	})
	return r
}

type ViewData struct {
	Page        string
	Title       string
	CSRF        string
	Flash       string
	FlashError  bool
	Dashboard   DashboardData
	Jobs        JobPage
	JobFilters  JobFilters
	News        NewsPage
	NewsFilters NewsFilters
	JobDetail   JobDetailData
	Accounts    []account.Account
	System      SystemData
	Statuses    []StatusOption
	Categories  []string
	AccountList []account.Account
	PrevURL     string
	NextURL     string
}

type DashboardData struct {
	Total          int
	NeedsAttention int
	Processing     int
	Published      int
	Failed         int
	WaitingAI      int
	OutboxPending  int
	OutboxDead     int
	Counts         []StatusCount
	Recent         []JobView
}

type StatusCount struct {
	Status post.Status
	Count  int
}

type StatusOption struct {
	Value string
	Label string
}

type VariantView struct {
	post.Variant
	Selected    bool
	PreviewPath string
}

type JobDetailData struct {
	Job           JobView
	Variants      []VariantView
	Selected      *VariantView
	CanGenerate   bool
	CanEdit       bool
	CanSelect     bool
	CanRegenerate bool
	CanSkip       bool
	CanPublish    bool
	IsActive      bool
}

type SystemData struct {
	Environment       string
	SchedulerEnabled  bool
	NewsSyncMinutes   int
	AIRetryMinutes    int
	PublishMinutes    int
	WorkSeconds       int
	NotifySeconds     int
	TgptEnabled       bool
	OllamaEnabled     bool
	OllamaModel       string
	PublishEnabled    bool
	StorageDriver     string
	RetentionDays     int
	OutboxPending     int
	OutboxDead        int
	DatabaseConnected bool
}

func (h *Handler) baseData(r *http.Request, page, title string) ViewData {
	return ViewData{Page: page, Title: title, CSRF: sessionFromContext(r.Context()).CSRF,
		Statuses: statusOptions(), Categories: h.categories()}
}

func (h *Handler) render(w http.ResponseWriter, name string, data ViewData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.templates.ExecuteTemplate(w, name, data); err != nil {
		h.logger.Error("render admin template", "template", name, "error", err)
	}
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if _, ok := h.sessions.verify(cookie.Value, time.Now()); ok {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}
	}
	h.render(w, "login", ViewData{Page: "login", Title: "Giriş"})
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if !h.logins.allow(r.RemoteAddr, time.Now()) {
		w.WriteHeader(http.StatusTooManyRequests)
		h.render(w, "login", ViewData{Page: "login", Title: "Giriş", Flash: authErrorMessage(true), FlashError: true})
		return
	}
	if err := r.ParseForm(); err != nil || !constantTimeStringEqual(strings.TrimSpace(r.Form.Get("token")), h.cfg.Security.APIToken) {
		w.WriteHeader(http.StatusUnauthorized)
		h.render(w, "login", ViewData{Page: "login", Title: "Giriş", Flash: authErrorMessage(false), FlashError: true})
		return
	}
	value, _, err := h.sessions.create(time.Now())
	if err != nil {
		http.Error(w, "oturum oluşturulamadı", http.StatusInternalServerError)
		return
	}
	h.sessions.setCookie(w, value)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	h.sessions.clearCookie(w)
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	data := h.baseData(r, "dashboard", "Yayın Masası")
	dashboard, err := h.loadDashboard(r)
	if err != nil {
		h.serverError(w, err)
		return
	}
	data.Dashboard = dashboard
	h.render(w, "layout", data)
}

func (h *Handler) dashboardFragment(w http.ResponseWriter, r *http.Request) {
	data := h.baseData(r, "dashboard", "Yayın Masası")
	dashboard, err := h.loadDashboard(r)
	if err != nil {
		h.serverError(w, err)
		return
	}
	data.Dashboard = dashboard
	h.render(w, "dashboard-fragment", data)
}

func (h *Handler) loadDashboard(r *http.Request) (DashboardData, error) {
	counts, err := h.posts.StatusCounts(r.Context())
	if err != nil {
		return DashboardData{}, err
	}
	recent, err := h.repo.RecentJobs(r.Context(), 8)
	if err != nil {
		return DashboardData{}, err
	}
	pending, dead, err := h.outbox.Counts(r.Context())
	if err != nil {
		return DashboardData{}, err
	}
	d := DashboardData{Recent: recent, OutboxPending: pending, OutboxDead: dead}
	for _, opt := range statusOptions() {
		status := post.Status(opt.Value)
		count := counts[opt.Value]
		d.Total += count
		if count > 0 {
			d.Counts = append(d.Counts, StatusCount{Status: status, Count: count})
		}
	}
	d.NeedsAttention = counts[string(post.StatusWaitingFirstApproval)] + counts[string(post.StatusWaitingVariantApproval)] + counts[string(post.StatusReadyToPublish)]
	d.Processing = counts[string(post.StatusScoringQueued)] + counts[string(post.StatusScoring)] + counts[string(post.StatusVariantsQueued)] + counts[string(post.StatusGeneratingVariants)] + counts[string(post.StatusApproved)] + counts[string(post.StatusPublishing)]
	d.Published = counts[string(post.StatusPublished)]
	d.Failed = counts[string(post.StatusFailed)]
	d.WaitingAI = counts[string(post.StatusWaitingAI)]
	return d, nil
}

func (h *Handler) postsPage(w http.ResponseWriter, r *http.Request) {
	f := JobFilters{Status: r.URL.Query().Get("status"), Category: r.URL.Query().Get("category"), Account: r.URL.Query().Get("account"), Query: r.URL.Query().Get("q"), Page: positiveInt(r.URL.Query().Get("page"), 1)}
	page, err := h.repo.ListJobs(r.Context(), f)
	if err != nil {
		h.serverError(w, err)
		return
	}
	accounts, err := h.accounts.List(r.Context())
	if err != nil {
		h.serverError(w, err)
		return
	}
	data := h.baseData(r, "posts", "Post Kuyruğu")
	data.Jobs, data.JobFilters, data.AccountList = page, f, accounts
	data.PrevURL, data.NextURL = pageLinks(r.URL, page.Page)
	h.render(w, "layout", data)
}

func (h *Handler) postPage(w http.ResponseWriter, r *http.Request) {
	id, ok := routeID(w, r, "id")
	if !ok {
		return
	}
	detail, err := h.loadJobDetail(r, id)
	if err != nil {
		h.handleNotFound(w, err)
		return
	}
	data := h.baseData(r, "post-detail", fmt.Sprintf("Post #%d", id))
	data.JobDetail = detail
	h.render(w, "layout", data)
}

func (h *Handler) postFragment(w http.ResponseWriter, r *http.Request) {
	id, ok := routeID(w, r, "id")
	if !ok {
		return
	}
	h.renderJobFragment(w, r, id, "", false)
}

func (h *Handler) loadJobDetail(r *http.Request, id int64) (JobDetailData, error) {
	job, err := h.repo.GetJob(r.Context(), id)
	if err != nil {
		return JobDetailData{}, err
	}
	variants, err := h.posts.ListVariants(r.Context(), id)
	if err != nil {
		return JobDetailData{}, err
	}
	d := JobDetailData{Job: *job}
	for _, variant := range variants {
		v := VariantView{Variant: variant, Selected: job.SelectedVariantID.Valid && job.SelectedVariantID.Int64 == variant.ID, PreviewPath: mediaPath(variant.ImageURL)}
		d.Variants = append(d.Variants, v)
		if v.Selected {
			selected := v
			d.Selected = &selected
		}
	}
	d.CanGenerate = job.Status == post.StatusWaitingFirstApproval
	d.CanEdit = job.Status == post.StatusWaitingVariantApproval || job.Status == post.StatusReadyToPublish
	d.CanSelect = d.CanEdit
	d.CanRegenerate = d.CanEdit
	d.CanSkip = job.Status == post.StatusWaitingFirstApproval || d.CanEdit
	d.CanPublish = job.Status == post.StatusReadyToPublish && d.Selected != nil
	d.IsActive = job.Status == post.StatusScoringQueued || job.Status == post.StatusScoring || job.Status == post.StatusVariantsQueued || job.Status == post.StatusGeneratingVariants || job.Status == post.StatusApproved || job.Status == post.StatusPublishing
	return d, nil
}

func (h *Handler) newsPage(w http.ResponseWriter, r *http.Request) {
	f := NewsFilters{Category: r.URL.Query().Get("category"), Query: r.URL.Query().Get("q"), Page: positiveInt(r.URL.Query().Get("page"), 1)}
	page, err := h.repo.ListNews(r.Context(), f)
	if err != nil {
		h.serverError(w, err)
		return
	}
	data := h.baseData(r, "news", "Haber Adayları")
	data.News, data.NewsFilters = page, f
	data.PrevURL, data.NextURL = pageLinks(r.URL, page.Page)
	h.render(w, "layout", data)
}

func (h *Handler) accountsPage(w http.ResponseWriter, r *http.Request) {
	items, err := h.accounts.List(r.Context())
	if err != nil {
		h.serverError(w, err)
		return
	}
	data := h.baseData(r, "accounts", "Kanallar")
	data.Accounts = items
	h.render(w, "layout", data)
}

func (h *Handler) systemPage(w http.ResponseWriter, r *http.Request) {
	pending, dead, err := h.outbox.Counts(r.Context())
	if err != nil {
		h.serverError(w, err)
		return
	}
	ctx, cancel := contextWithTimeout(r, 2*time.Second)
	defer cancel()
	data := h.baseData(r, "system", "Sistem Durumu")
	data.System = SystemData{
		Environment: h.cfg.App.Env, SchedulerEnabled: h.cfg.Scheduler.Enabled,
		NewsSyncMinutes: h.cfg.Scheduler.NewsSyncIntervalMinutes, AIRetryMinutes: h.cfg.Scheduler.WaitingAIRetryIntervalMinutes,
		PublishMinutes: h.cfg.Scheduler.PublishIntervalMinutes, WorkSeconds: h.cfg.Scheduler.WorkIntervalSeconds,
		NotifySeconds: h.cfg.Scheduler.NotificationIntervalSeconds, TgptEnabled: h.cfg.AI.Providers.Tgpt.Enabled,
		OllamaEnabled: h.cfg.AI.Providers.Ollama.Enabled, OllamaModel: h.cfg.AI.Providers.Ollama.Model,
		PublishEnabled: h.cfg.Instagram.PublishEnabled, StorageDriver: h.cfg.Storage.Driver,
		RetentionDays: h.cfg.Storage.RetentionDays, OutboxPending: pending, OutboxDead: dead,
		DatabaseConnected: h.repo.db.PingContext(ctx) == nil,
	}
	h.render(w, "layout", data)
}

func (h *Handler) syncNews(w http.ResponseWriter, r *http.Request) {
	n, err := h.approval.SyncNews(r.Context())
	message := fmt.Sprintf("Senkron tamamlandı: %d yeni haber.", n)
	if err != nil {
		message = fmt.Sprintf("Senkron kısmen tamamlandı: %d yeni haber; bazı kaynaklar yanıt vermedi.", n)
	}
	data := h.baseData(r, "dashboard", "Yayın Masası")
	dashboard, loadErr := h.loadDashboard(r)
	if loadErr != nil {
		h.serverError(w, loadErr)
		return
	}
	data.Dashboard, data.Flash, data.FlashError = dashboard, message, err != nil
	h.render(w, "dashboard-fragment", data)
}

func (h *Handler) generate(w http.ResponseWriter, r *http.Request) {
	h.jobAction(w, r, func(id int64) error { return h.approval.GenerateVariants(r.Context(), id) }, "Alternatif üretimi kuyruğa alındı.")
}

func (h *Handler) regenerate(w http.ResponseWriter, r *http.Request) {
	h.jobAction(w, r, func(id int64) error { return h.approval.RegenerateVariants(r.Context(), id) }, "Alternatifler yeniden üretim kuyruğuna alındı.")
}

func (h *Handler) selectVariant(w http.ResponseWriter, r *http.Request) {
	variantID, err := strconv.ParseInt(r.Form.Get("variantId"), 10, 64)
	if err != nil || variantID <= 0 {
		h.jobActionError(w, r, errors.New("geçerli bir varyant seçin"))
		return
	}
	h.jobAction(w, r, func(id int64) error { return h.approval.SelectVariantForReview(r.Context(), id, variantID) }, "Varyant seçildi; gerçek görsel önizlemesi hazırlandı.")
}

func (h *Handler) updateCaption(w http.ResponseWriter, r *http.Request) {
	variantID, ok := routeID(w, r, "variantID")
	if !ok {
		return
	}
	h.jobAction(w, r, func(id int64) error {
		return h.approval.UpdateVariantCaption(r.Context(), id, variantID, r.Form.Get("caption"))
	}, "Caption kaydedildi.")
}

func (h *Handler) publish(w http.ResponseWriter, r *http.Request) {
	h.jobAction(w, r, func(id int64) error { return h.approval.QueuePublish(r.Context(), id) }, "Yayın Instagram kuyruğuna alındı.")
}

func (h *Handler) skip(w http.ResponseWriter, r *http.Request) {
	h.jobAction(w, r, func(id int64) error { return h.approval.SkipJob(r.Context(), id) }, "İş atlandı.")
}

func (h *Handler) jobAction(w http.ResponseWriter, r *http.Request, action func(int64) error, success string) {
	id, ok := routeID(w, r, "id")
	if !ok {
		return
	}
	if err := action(id); err != nil {
		h.renderJobFragment(w, r, id, err.Error(), true)
		return
	}
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, fmt.Sprintf("/admin/posts/%d", id), http.StatusSeeOther)
		return
	}
	h.renderJobFragment(w, r, id, success, false)
}

func (h *Handler) jobActionError(w http.ResponseWriter, r *http.Request, err error) {
	id, ok := routeID(w, r, "id")
	if !ok {
		return
	}
	h.renderJobFragment(w, r, id, err.Error(), true)
}

func (h *Handler) renderJobFragment(w http.ResponseWriter, r *http.Request, id int64, flash string, flashError bool) {
	detail, err := h.loadJobDetail(r, id)
	if err != nil {
		h.handleNotFound(w, err)
		return
	}
	data := h.baseData(r, "post-detail", fmt.Sprintf("Post #%d", id))
	data.JobDetail, data.Flash, data.FlashError = detail, flash, flashError
	h.render(w, "job-fragment", data)
}

func (h *Handler) categories() []string {
	seen := make(map[string]bool)
	var out []string
	for _, acct := range h.cfg.Accounts {
		if !seen[acct.Category] {
			seen[acct.Category] = true
			out = append(out, acct.Category)
		}
	}
	return out
}

func (h *Handler) serverError(w http.ResponseWriter, err error) {
	h.logger.Error("admin request failed", "error", err)
	http.Error(w, "panel verileri yüklenemedi", http.StatusInternalServerError)
}

func (h *Handler) handleNotFound(w http.ResponseWriter, err error) {
	if errors.Is(err, post.ErrNotFound) {
		http.Error(w, "post bulunamadı", http.StatusNotFound)
		return
	}
	h.serverError(w, err)
}

func routeID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "geçersiz kimlik", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func positiveInt(raw string, fallback int) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return fallback
	}
	return n
}

func pageLinks(current *url.URL, page PageInfo) (string, string) {
	build := func(number int) string {
		copyURL := *current
		q := copyURL.Query()
		q.Set("page", strconv.Itoa(number))
		copyURL.RawQuery = q.Encode()
		return copyURL.String()
	}
	var prev, next string
	if page.HasPrev {
		prev = build(page.Page - 1)
	}
	if page.HasNext {
		next = build(page.Page + 1)
	}
	return prev, next
}

func mediaPath(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	name := path.Base(u.Path)
	if name == "." || name == "/" || name == "" {
		return ""
	}
	return "/static/" + url.PathEscape(name)
}

func statusOptions() []StatusOption {
	statuses := []post.Status{
		post.StatusNew, post.StatusScoringQueued, post.StatusScoring, post.StatusWaitingAI,
		post.StatusScored, post.StatusWaitingFirstApproval, post.StatusVariantsQueued,
		post.StatusGeneratingVariants, post.StatusWaitingVariantApproval, post.StatusReadyToPublish,
		post.StatusApproved, post.StatusPublishing, post.StatusPublished, post.StatusSkipped, post.StatusFailed,
	}
	out := make([]StatusOption, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, StatusOption{Value: string(status), Label: statusLabel(status)})
	}
	return out
}

func statusLabel(status post.Status) string {
	labels := map[post.Status]string{
		post.StatusNew: "Yeni", post.StatusScoringQueued: "Skor kuyruğunda", post.StatusScoring: "Skorlanıyor",
		post.StatusWaitingAI: "AI bekleniyor", post.StatusScored: "Skorlandı", post.StatusWaitingFirstApproval: "İlk onay bekliyor",
		post.StatusVariantsQueued: "Alternatif kuyruğunda", post.StatusGeneratingVariants: "Alternatif üretiliyor",
		post.StatusWaitingVariantApproval: "Varyant onayı bekliyor", post.StatusReadyToPublish: "Yayına hazır",
		post.StatusApproved: "Yayın kuyruğunda", post.StatusPublishing: "Yayınlanıyor", post.StatusPublished: "Yayınlandı",
		post.StatusSkipped: "Atlandı", post.StatusFailed: "Başarısız",
	}
	if label := labels[status]; label != "" {
		return label
	}
	return string(status)
}

func statusClass(status post.Status) string {
	switch status {
	case post.StatusPublished:
		return "success"
	case post.StatusFailed:
		return "danger"
	case post.StatusWaitingFirstApproval, post.StatusWaitingVariantApproval, post.StatusReadyToPublish:
		return "attention"
	case post.StatusSkipped:
		return "muted"
	default:
		return "working"
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("02.01.2006 15:04")
}

func formatDate(t sql.NullTime) string {
	if !t.Valid {
		return "—"
	}
	return formatTime(t.Time)
}

func maskID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 4 {
		return "••••"
	}
	return "•••• " + value[len(value)-4:]
}

func contextWithTimeout(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), timeout)
}
