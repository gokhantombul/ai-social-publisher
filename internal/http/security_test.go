package http

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"ai-social-publisher/internal/config"
)

func securityTestHandler() *Handler {
	return &Handler{
		cfg: &config.Config{Security: config.SecurityConfig{
			APIToken:               "12345678901234567890123456789012",
			TelegramCallbackSecret: "abcdefghijklmnopqrstuvxyz1234567",
		}},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestAPIAuth(t *testing.T) {
	h := securityTestHandler()
	next := h.apiAuth(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, _ *stdhttp.Request) { w.WriteHeader(stdhttp.StatusNoContent) }))

	rejected := httptest.NewRecorder()
	next.ServeHTTP(rejected, httptest.NewRequest(stdhttp.MethodGet, "/api/posts", nil))
	if rejected.Code != stdhttp.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rejected.Code)
	}

	acceptedReq := httptest.NewRequest(stdhttp.MethodGet, "/api/posts", nil)
	acceptedReq.Header.Set("Authorization", "Bearer "+h.cfg.Security.APIToken)
	accepted := httptest.NewRecorder()
	next.ServeHTTP(accepted, acceptedReq)
	if accepted.Code != stdhttp.StatusNoContent {
		t.Fatalf("expected 204, got %d", accepted.Code)
	}
}

func TestTelegramCallbackAuth(t *testing.T) {
	h := securityTestHandler()
	body := []byte(`{"action":"SKIP_NEWS","payload":"1","user":"tester"}`)
	next := h.telegramCallbackAuth(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, body) {
			t.Fatalf("body was not restored")
		}
		w.WriteHeader(stdhttp.StatusNoContent)
	}))

	mac := hmac.New(sha256.New, []byte(h.cfg.Security.TelegramCallbackSecret))
	timestamp := time.Now().Unix()
	_, _ = mac.Write([]byte(strconv.FormatInt(timestamp, 10) + "."))
	_, _ = mac.Write(body)
	req := httptest.NewRequest(stdhttp.MethodPost, "/api/telegram/callback", bytes.NewReader(body))
	req.Header.Set(telegramSignatureHeader, hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set(telegramTimestampHeader, strconv.FormatInt(timestamp, 10))
	w := httptest.NewRecorder()
	next.ServeHTTP(w, req)
	if w.Code != stdhttp.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
}

func TestIPRateLimiter(t *testing.T) {
	limiter := newIPRateLimiter(2, time.Minute)
	next := limiter.middleware(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, _ *stdhttp.Request) { w.WriteHeader(stdhttp.StatusNoContent) }))
	for i, want := range []int{stdhttp.StatusNoContent, stdhttp.StatusNoContent, stdhttp.StatusTooManyRequests} {
		req := httptest.NewRequest(stdhttp.MethodGet, "/", nil)
		req.RemoteAddr = "192.0.2.1:1234"
		w := httptest.NewRecorder()
		next.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("request %d: expected %d, got %d", i+1, want, w.Code)
		}
	}
}
