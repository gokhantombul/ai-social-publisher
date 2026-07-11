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

// maxTrackedClients bounds limiter memory. When the table is full, requests
// from brand-new keys pass untracked: rotating source addresses defeats a
// per-client limit anyway, and failing closed would lock out legitimate users.
const maxTrackedClients = 50_000

type ipRateLimiter struct {
	mu        sync.Mutex
	limit     int
	window    time.Duration
	clients   map[string]rateWindow
	lastSweep time.Time
}

func newIPRateLimiter(limit int, window time.Duration) *ipRateLimiter {
	return &ipRateLimiter{limit: limit, window: window, clients: map[string]rateWindow{}}
}

// rateLimitKey buckets IPv6 clients by /64: one host effectively owns its /64,
// so tracking individual addresses would let a single attacker mint unlimited
// limiter entries (and dodge its own limit) by rotating within the prefix.
func rateLimitKey(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String()
}

func (l *ipRateLimiter) allow(remoteAddr string, now time.Time) bool {
	key := rateLimitKey(remoteAddr)
	l.mu.Lock()
	defer l.mu.Unlock()
	// Sweep expired windows at most once per window so a large client table
	// never turns every request into a full map scan.
	if now.Sub(l.lastSweep) >= l.window {
		for k, v := range l.clients {
			if now.Sub(v.started) >= l.window {
				delete(l.clients, k)
			}
		}
		l.lastSweep = now
	}
	entry, tracked := l.clients[key]
	if entry.started.IsZero() || now.Sub(entry.started) >= l.window {
		entry = rateWindow{started: now}
	}
	entry.count++
	if tracked || len(l.clients) < maxTrackedClients {
		l.clients[key] = entry
	}
	return entry.count <= l.limit
}

func (l *ipRateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(r.RemoteAddr, time.Now()) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}
