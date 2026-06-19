package http

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const telegramSignatureHeader = "X-Telegram-Signature"
const telegramTimestampHeader = "X-Telegram-Timestamp"

func (h *Handler) apiAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, prefix) || !secureEqual(strings.TrimSpace(strings.TrimPrefix(header, prefix)), h.cfg.Security.APIToken) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ai-social-publisher"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) telegramCallbackAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid callback body")
			return
		}
		provided, err := hex.DecodeString(r.Header.Get(telegramSignatureHeader))
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid callback signature")
			return
		}
		timestampRaw := r.Header.Get(telegramTimestampHeader)
		timestamp, err := strconv.ParseInt(timestampRaw, 10, 64)
		if err != nil || time.Since(time.Unix(timestamp, 0)).Abs() > 5*time.Minute {
			writeError(w, http.StatusUnauthorized, "expired callback timestamp")
			return
		}
		mac := hmac.New(sha256.New, []byte(h.cfg.Security.TelegramCallbackSecret))
		_, _ = mac.Write([]byte(timestampRaw + "."))
		_, _ = mac.Write(body)
		if !hmac.Equal(provided, mac.Sum(nil)) {
			writeError(w, http.StatusUnauthorized, "invalid callback signature")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

func maxBodyBytes(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

func secureEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

type rateWindow struct {
	started time.Time
	count   int
}

type ipRateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	clients map[string]rateWindow
}

func newIPRateLimiter(limit int, window time.Duration) *ipRateLimiter {
	return &ipRateLimiter{limit: limit, window: window, clients: map[string]rateWindow{}}
}

func (l *ipRateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		now := time.Now()
		l.mu.Lock()
		entry := l.clients[host]
		if entry.started.IsZero() || now.Sub(entry.started) >= l.window {
			entry = rateWindow{started: now}
		}
		entry.count++
		l.clients[host] = entry
		allowed := entry.count <= l.limit
		// Opportunistic cleanup keeps the map bounded without another goroutine.
		if len(l.clients) > 10_000 {
			for key, value := range l.clients {
				if now.Sub(value.started) >= l.window {
					delete(l.clients, key)
				}
			}
		}
		l.mu.Unlock()
		if !allowed {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}
