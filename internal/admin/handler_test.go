package admin

import (
	"bytes"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ai-social-publisher/internal/account"
	"ai-social-publisher/internal/approval"
	"ai-social-publisher/internal/config"
	"ai-social-publisher/internal/outbox"
	"ai-social-publisher/internal/post"
)

const adminTestToken = "12345678901234567890123456789012"

func testHandler(t *testing.T) *Handler {
	t.Helper()
	db := &sql.DB{}
	h, err := New(Deps{
		Config: &config.Config{
			App:      config.AppConfig{Env: "development", PublicBaseURL: "http://localhost:8080/static"},
			Security: config.SecurityConfig{APIToken: adminTestToken},
		},
		DB: db, Approval: &approval.Service{}, Accounts: account.NewRepository(db),
		Posts: post.NewRepository(db), Outbox: outbox.NewRepository(db),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestLoginSessionAndCSRF(t *testing.T) {
	h := testHandler(t)

	protected := httptest.NewRecorder()
	h.ServeHTTP(protected, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if protected.Code != http.StatusSeeOther || protected.Header().Get("Location") != "/login" {
		t.Fatalf("expected login redirect, got %d %q", protected.Code, protected.Header().Get("Location"))
	}

	badForm := url.Values{"token": {"wrong"}}
	badReq := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(badForm.Encode()))
	badReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	bad := httptest.NewRecorder()
	h.ServeHTTP(bad, badReq)
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", bad.Code)
	}

	form := url.Values{"token": {adminTestToken}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin" {
		t.Fatalf("unexpected login response: %d %q", w.Code, w.Header().Get("Location"))
	}
	result := w.Result()
	var sessionCookie *http.Cookie
	for _, cookie := range result.Cookies() {
		if cookie.Name == sessionCookieName {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil || !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteStrictMode || sessionCookie.Secure {
		t.Fatalf("unexpected session cookie: %+v", sessionCookie)
	}
	session, ok := h.sessions.verify(sessionCookie.Value, time.Now())
	if !ok || session.CSRF == "" {
		t.Fatal("created session did not verify")
	}

	missingCSRF := httptest.NewRequest(http.MethodPost, "/logout", nil)
	missingCSRF.AddCookie(sessionCookie)
	missingW := httptest.NewRecorder()
	h.ServeHTTP(missingW, missingCSRF)
	if missingW.Code != http.StatusForbidden {
		t.Fatalf("expected CSRF rejection, got %d", missingW.Code)
	}

	logoutForm := url.Values{"_csrf": {session.CSRF}}
	logoutReq := httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(logoutForm.Encode()))
	logoutReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	logoutReq.AddCookie(sessionCookie)
	logoutW := httptest.NewRecorder()
	h.ServeHTTP(logoutW, logoutReq)
	if logoutW.Code != http.StatusSeeOther || logoutW.Header().Get("Location") != "/login" {
		t.Fatalf("unexpected logout response: %d %q", logoutW.Code, logoutW.Header().Get("Location"))
	}
}

func TestParseAdminScheduleTime(t *testing.T) {
	if _, err := parseAdminScheduleTime("  "); err == nil {
		t.Fatal("empty value should be rejected")
	}
	if _, err := parseAdminScheduleTime("11-07-2026 09:00"); err == nil {
		t.Fatal("non datetime-local value should be rejected")
	}
	got, err := parseAdminScheduleTime("2026-07-11T09:30")
	if err != nil {
		t.Fatalf("valid datetime-local rejected: %v", err)
	}
	want := time.Date(2026, 7, 11, 9, 30, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v (local)", got, want)
	}
	if _, err := parseAdminScheduleTime("2026-07-11T09:30:45"); err != nil {
		t.Fatalf("datetime-local with seconds rejected: %v", err)
	}
}

func TestSessionRejectsTamperingAndExpiry(t *testing.T) {
	codec := newSessionCodec(adminTestToken, true)
	value, _, err := codec.create(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := codec.verify(value+"x", time.Now()); ok {
		t.Fatal("tampered session was accepted")
	}
	if _, ok := codec.verify(value, time.Now().Add(sessionLifetime+time.Minute)); ok {
		t.Fatal("expired session was accepted")
	}
}

func TestSecurityHeadersAndEmbeddedAssets(t *testing.T) {
	h := testHandler(t)
	for _, target := range []string{"/login", "/assets/app.css", "/assets/htmx.min.js"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", target, w.Code)
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatalf("%s: security headers missing", target)
		}
	}
}

func TestAllPageTemplatesExecute(t *testing.T) {
	h := testHandler(t)
	now := time.Now()
	job := JobView{ID: 7, Status: post.StatusReadyToPublish, Title: "<script>alert(1)</script>", Category: "technology", AccountCode: "teknoloji", UpdatedAt: now}
	selectedVariant := VariantView{Variant: post.Variant{ID: 4, Caption: "caption", VariantNo: 1}, Selected: true, PreviewPath: "/static/x.png"}
	scheduledJob := JobView{ID: 8, Status: post.StatusScheduled, Title: "Planlı içerik", Category: "technology", AccountCode: "teknoloji", UpdatedAt: now, ScheduledPublishAt: sql.NullTime{Time: now.Add(time.Hour), Valid: true}}
	data := []ViewData{
		{Page: "dashboard", Title: "Dashboard", Dashboard: DashboardData{Recent: []JobView{job}, Counts: []StatusCount{{Status: post.StatusReadyToPublish, Count: 1}}}},
		{Page: "posts", Title: "Posts", Jobs: JobPage{Items: []JobView{job}, Page: makePage(1, 1)}},
		{Page: "post-detail", Title: "Detail", JobDetail: JobDetailData{Job: job, Variants: []VariantView{{Variant: post.Variant{ID: 4, Caption: "caption", VariantNo: 1}}}, CanEdit: true, CanSelect: true}},
		// Ready to publish with a selected preview: renders the schedule form (nowInput func).
		{Page: "post-detail", Title: "Schedulable", JobDetail: JobDetailData{Job: job, Variants: []VariantView{selectedVariant}, Selected: &selectedVariant, CanPublish: true, CanSchedule: true}},
		// Scheduled job: renders the scheduled-status card (formatDate on the time) and unschedule form.
		{Page: "post-detail", Title: "Scheduled", JobDetail: JobDetailData{Job: scheduledJob, IsScheduled: true}},
		{Page: "news", Title: "News", News: NewsPage{Items: []NewsView{{ID: 1, Title: "Haber", JobStatus: post.StatusReadyToPublish}}, Page: makePage(1, 1)}},
		{Page: "accounts", Title: "Accounts", Accounts: []account.Account{{Code: "tech", Name: "Tech", Category: "technology", IsActive: true, UpdatedAt: now}}},
		{Page: "system", Title: "System", System: SystemData{DatabaseConnected: true}},
	}
	for _, view := range data {
		var out bytes.Buffer
		if err := h.templates.ExecuteTemplate(&out, "layout", view); err != nil {
			t.Fatalf("page %s: %v", view.Page, err)
		}
		if strings.Contains(out.String(), "<script>alert(1)</script>") {
			t.Fatalf("page %s did not escape untrusted content", view.Page)
		}
	}
}
