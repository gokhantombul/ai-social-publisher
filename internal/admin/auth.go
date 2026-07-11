package admin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookieName = "aisp_admin_session"
	sessionLifetime   = 8 * time.Hour
)

type sessionContextKey struct{}

type sessionData struct {
	Payload string
	CSRF    string
}

type sessionCodec struct {
	key    [32]byte
	secure bool
}

func newSessionCodec(apiToken string, secure bool) sessionCodec {
	return sessionCodec{key: sha256.Sum256([]byte("ai-social-publisher/admin-session/v1\x00" + apiToken)), secure: secure}
}

func (c sessionCodec) create(now time.Time) (value, csrf string, err error) {
	nonce := make([]byte, 18)
	if _, err := rand.Read(nonce); err != nil {
		return "", "", err
	}
	payload := strconv.FormatInt(now.Add(sessionLifetime).Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString(nonce)
	return c.sign(payload), c.csrf(payload), nil
}

func (c sessionCodec) sign(payload string) string {
	mac := hmac.New(sha256.New, c.key[:])
	_, _ = mac.Write([]byte("session\x00" + payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (c sessionCodec) csrf(payload string) string {
	mac := hmac.New(sha256.New, c.key[:])
	_, _ = mac.Write([]byte("csrf\x00" + payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (c sessionCodec) verify(value string, now time.Time) (sessionData, bool) {
	lastDot := strings.LastIndexByte(value, '.')
	if lastDot <= 0 || lastDot == len(value)-1 {
		return sessionData{}, false
	}
	payload, encodedSig := value[:lastDot], value[lastDot+1:]
	provided, err := base64.RawURLEncoding.DecodeString(encodedSig)
	if err != nil {
		return sessionData{}, false
	}
	mac := hmac.New(sha256.New, c.key[:])
	_, _ = mac.Write([]byte("session\x00" + payload))
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return sessionData{}, false
	}
	expiryRaw, _, ok := strings.Cut(payload, ".")
	if !ok {
		return sessionData{}, false
	}
	expiry, err := strconv.ParseInt(expiryRaw, 10, 64)
	if err != nil || !now.Before(time.Unix(expiry, 0)) {
		return sessionData{}, false
	}
	return sessionData{Payload: payload, CSRF: c.csrf(payload)}, true
}

func (c sessionCodec) setCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: value, Path: "/", MaxAge: int(sessionLifetime.Seconds()),
		HttpOnly: true, Secure: c.secure, SameSite: http.SameSiteStrictMode,
	})
}

func (c sessionCodec) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1,
		Expires: time.Unix(1, 0), HttpOnly: true, Secure: c.secure, SameSite: http.SameSiteStrictMode,
	})
}

func (h *Handler) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			h.redirectLogin(w, r)
			return
		}
		session, ok := h.sessions.verify(cookie.Value, time.Now())
		if !ok {
			h.sessions.clearCookie(w)
			h.redirectLogin(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionContextKey{}, session)))
	})
}

func (h *Handler) redirectLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *Handler) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := r.Context().Value(sessionContextKey{}).(sessionData)
		if !ok {
			h.redirectLogin(w, r)
			return
		}
		_ = r.ParseForm()
		provided := r.Header.Get("X-CSRF-Token")
		if provided == "" {
			provided = r.Form.Get("_csrf")
		}
		if !constantTimeStringEqual(provided, session.CSRF) {
			http.Error(w, "geçersiz istek doğrulaması", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sessionFromContext(ctx context.Context) sessionData {
	s, _ := ctx.Value(sessionContextKey{}).(sessionData)
	return s
}

func constantTimeStringEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

type loginWindow struct {
	started time.Time
	count   int
}

// maxTrackedLoginClients bounds limiter memory; new keys beyond the cap pass
// untracked because failing closed would lock every operator out.
const maxTrackedLoginClients = 50_000

type loginLimiter struct {
	mu        sync.Mutex
	clients   map[string]loginWindow
	lastSweep time.Time
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{clients: make(map[string]loginWindow)} }

// loginLimitKey buckets IPv6 clients by /64 so one attacker cannot mint
// unlimited limiter entries (or dodge the limit) by rotating within its prefix.
func loginLimitKey(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
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

func (l *loginLimiter) allow(remote string, now time.Time) bool {
	key := loginLimitKey(remote)
	l.mu.Lock()
	defer l.mu.Unlock()
	// Sweep expired windows at most once per window so a flood of distinct
	// sources never turns each login attempt into a full map scan.
	if now.Sub(l.lastSweep) >= time.Minute {
		for k, entry := range l.clients {
			if now.Sub(entry.started) >= time.Minute {
				delete(l.clients, k)
			}
		}
		l.lastSweep = now
	}
	w, tracked := l.clients[key]
	if w.started.IsZero() || now.Sub(w.started) >= time.Minute {
		w = loginWindow{started: now}
	}
	w.count++
	if tracked || len(l.clients) < maxTrackedLoginClients {
		l.clients[key] = w
	}
	return w.count <= 5
}

func (h *Handler) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if h.sessions.secure {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func authErrorMessage(rateLimited bool) string {
	if rateLimited {
		return "Çok fazla giriş denemesi. Bir dakika sonra tekrar deneyin."
	}
	return "Token doğrulanamadı."
}

func (c sessionCodec) String() string { return fmt.Sprintf("sessionCodec(secure=%t)", c.secure) }
