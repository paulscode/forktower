package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/paulscode/forktower/internal/alert"
	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/store"
)

// Session and login-protection settings.
const (
	// SessionCookie is the cookie name.
	SessionCookie = "forktower_session"
	// DefaultSessionTTL is how long a session survives without use.
	DefaultSessionTTL = 7 * 24 * time.Hour
	// LoginAttemptsPerMinute is the global ceiling on login attempts.
	//
	// Global, never per client address. Behind both platforms' app proxies every
	// request arrives from one address, so a per-IP limit is either global anyway
	// or trivially defeated by forging a forwarding header — and a header a
	// stranger controls must never widen a security decision.
	LoginAttemptsPerMinute = 5
	// LoginBackoffCap bounds the exponential delay after repeated failures.
	LoginBackoffCap = 15 * time.Minute
	// LoginFailuresBeforeAlert is how many consecutive failures are worth telling
	// the user about. A burst against this dashboard is worth knowing.
	LoginFailuresBeforeAlert = 10
	// KindLoginFailures is the alert kind for that burst.
	KindLoginFailures = "login_failures"
)

// authenticator holds session state and login protection.
//
// Sessions live in memory and do not survive a restart. That is a deliberate
// trade: persisting them would put a bearer credential in the same database the
// support bundle exports, and being asked to log in again after an upgrade is a
// small price beside that.
type authenticator struct {
	cfg     Config
	now     func() time.Time
	log     *slog.Logger
	alerter Alerter

	ttl time.Duration

	mu       sync.Mutex
	sessions map[string]time.Time
	// attempts is the global login-attempt window.
	attempts     []time.Time
	failures     int
	lockedUntil  time.Time
	alertedAtRun int
}

func newAuthenticator(cfg Config, now func() time.Time, log *slog.Logger, al Alerter) *authenticator {
	ttl := cfg.SessionTTL
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	return &authenticator{
		cfg: cfg, now: now, log: log, alerter: al, ttl: ttl,
		sessions: map[string]time.Time{},
	}
}

// checkOrigin rejects an unsafe request whose origin is not this dashboard.
//
// Cookie authentication plus an unchecked POST is exploitable from any page the
// user happens to have open, and the dashboard sits at a guessable name on the
// local network. The attacker never has to read the response: acknowledging every
// alert, or confirming a resolution, is damage enough.
//
// SameSite is not relied on alone, because both platforms embed the dashboard in
// a frame and cookie behaviour in frames is exactly where SameSite gets subtle.
func (s *Server) checkOrigin(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSafeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.originAllowed(r) {
			s.log.Warn("rejected a request from another site",
				slog.String("path", r.URL.Path),
				slog.String("origin", r.Header.Get("Origin")))
			writeError(w, http.StatusForbidden, CodeBadOrigin,
				"This request did not come from your Forktower dashboard, so it was refused.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// originAllowed compares the browser's stated origin against this dashboard's.
//
// Compared against the host the request arrived on rather than a configured
// value, so the ordinary case needs no configuration and stays correct when the
// dashboard is reached by a different name. An absent Origin *and* Referer is a
// refusal, not a pass: every browser sends one on an unsafe cross-origin request,
// so their absence is either a non-browser caller or an attempt to slip past this.
func (s *Server) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Some older browsers omit Origin on same-origin requests but always send
		// Referer, so it is the documented fallback rather than a second chance.
		if ref := r.Header.Get("Referer"); ref != "" {
			u, err := url.Parse(ref)
			if err != nil || u.Host == "" {
				return false
			}
			origin = u.Scheme + "://" + u.Host
		}
	}
	if origin == "" || origin == "null" {
		return false
	}

	for _, allowed := range s.cfg.AllowedOrigins {
		if strings.EqualFold(strings.TrimRight(allowed, "/"), origin) {
			return true
		}
	}

	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// requireAuth enforces the configured authentication mode.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch s.cfg.Auth {
		case config.AuthNone:
			// Confined to loopback by configuration validation, so the caller is
			// already someone with an account on this machine.
			next.ServeHTTP(w, r)

		case config.AuthPlatform:
			// The platform's own proxy authenticated this. Forktower deliberately
			// does not second-guess it: inventing a second check here would give
			// users two passwords for one thing and no more safety.
			next.ServeHTTP(w, r)

		case config.AuthPassword:
			if !s.auth.validSession(r) {
				writeError(w, http.StatusUnauthorized, CodeUnauthorized,
					"Please sign in to Forktower.")
				return
			}
			next.ServeHTTP(w, r)

		default:
			writeError(w, http.StatusUnauthorized, CodeUnauthorized, "Please sign in to Forktower.")
		}
	}
}

func (a *authenticator) validSession(r *http.Request) bool {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	expires, ok := a.sessions[c.Value]
	if !ok {
		return false
	}
	now := a.now()
	if now.After(expires) {
		delete(a.sessions, c.Value)
		return false
	}
	// Idle expiry, so an open dashboard stays open.
	a.sessions[c.Value] = now.Add(a.ttl)
	return true
}

type loginRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Auth != config.AuthPassword {
		writeError(w, http.StatusNotFound, CodeNotFound,
			"This Forktower is not set up to use a password.")
		return
	}

	if wait, limited := s.auth.rateLimited(); limited {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, CodeRateLimited,
			"Too many sign-in attempts. Please wait a moment and try again.")
		return
	}

	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "That sign-in request was not readable.")
		return
	}

	if err := bcrypt.CompareHashAndPassword(
		[]byte(s.cfg.PasswordHash), []byte(req.Password)); err != nil {
		s.auth.recordFailure(r.Context())
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "That password did not match.")
		return
	}

	token, err := s.auth.newSession(r)
	if err != nil {
		s.fail(w, r, "creating a session", err)
		return
	}
	http.SetCookie(w, s.auth.cookie(r, token, s.auth.now().Add(s.auth.ttl)))
	writeData(w, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		s.auth.invalidate(c.Value)
	}
	// Expired rather than merely dropped, so the browser stops sending it even if
	// this response is the last one it sees.
	http.SetCookie(w, s.auth.cookie(r, "", time.Unix(0, 0)))
	w.WriteHeader(http.StatusNoContent)
}

// newSession mints a session id and forgets any the caller was carrying.
//
// Regenerated on every login so that an id an attacker managed to plant in the
// browser beforehand is not the one that ends up authenticated.
func (a *authenticator) newSession(r *http.Request) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a session id: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	a.mu.Lock()
	defer a.mu.Unlock()

	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		delete(a.sessions, c.Value)
	}
	a.pruneLocked()
	a.sessions[token] = a.now().Add(a.ttl)
	a.failures = 0
	return token, nil
}

func (a *authenticator) invalidate(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
}

func (a *authenticator) pruneLocked() {
	now := a.now()
	for token, expires := range a.sessions {
		if now.After(expires) {
			delete(a.sessions, token)
		}
	}
}

// cookie builds the session cookie.
func (a *authenticator) cookie(r *http.Request, value string, expires time.Time) *http.Cookie {
	//nolint:gosec // Secure is set from the scheme below, deliberately.
	c := &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Expires:  expires,
		// Secure only over https: setting it on a plain-http LAN dashboard makes
		// the browser discard the cookie and the user can never sign in.
		Secure: isHTTPS(r),
	}
	if value == "" {
		c.MaxAge = -1
	}
	return c
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.URL.Scheme, "https")
}

// rateLimited reports whether the login endpoint is currently refusing attempts.
func (a *authenticator) rateLimited() (wait time.Duration, limited bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.now()
	if now.Before(a.lockedUntil) {
		return a.lockedUntil.Sub(now), true
	}

	cutoff := now.Add(-time.Minute)
	kept := a.attempts[:0]
	for _, at := range a.attempts {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	a.attempts = kept

	if len(a.attempts) >= LoginAttemptsPerMinute {
		return time.Minute, true
	}
	a.attempts = append(a.attempts, now)
	return 0, false
}

// recordFailure counts a wrong password, backs off, and eventually says so.
func (a *authenticator) recordFailure(ctx context.Context) {
	a.mu.Lock()
	a.failures++
	failures := a.failures

	// Exponential from the first failure past the per-minute allowance, capped so
	// a mistyped password does not lock the owner out for the afternoon.
	if failures > LoginAttemptsPerMinute {
		backoff := time.Duration(1<<min(failures-LoginAttemptsPerMinute, 10)) * time.Second
		backoff = min(backoff, LoginBackoffCap)
		a.lockedUntil = a.now().Add(backoff)
	}
	shouldAlert := failures >= LoginFailuresBeforeAlert && failures != a.alertedAtRun
	if shouldAlert {
		a.alertedAtRun = failures
	}
	a.mu.Unlock()

	if !shouldAlert || a.alerter == nil {
		return
	}
	a.log.Warn("repeated failed sign-in attempts", slog.Int("attempts", failures))
	a.alerter.Raise(ctx, alert.Candidate{
		Tier:     store.TierWarning,
		Kind:     KindLoginFailures,
		DedupKey: KindLoginFailures,
		Message: "Someone has tried and failed to sign in to Forktower several times. " +
			"If that was not you, make sure your dashboard is not reachable from the internet.",
	})
}
