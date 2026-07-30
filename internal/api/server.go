// Package api serves the dashboard's HTTP interface.
//
// The web UI consumes exactly this API and nothing private, so every shape here
// is the shape a user's own tooling can rely on.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/paulscode/forktower/internal/alert"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/registry"
	"github.com/paulscode/forktower/internal/sentinel"
	"github.com/paulscode/forktower/internal/store"
)

// Error codes. Stable strings: a caller branches on these, not on the message,
// which is free to be reworded for clarity at any time.
const (
	CodeBadOrigin    = "bad_origin"
	CodeUnauthorized = "unauthorized"
	CodeBadRequest   = "bad_request"
	CodeNotFound     = "not_found"
	CodeWrongState   = "wrong_state"
	CodeRateLimited  = "rate_limited"
	CodeInternal     = "internal"
)

// Sentinel is what the API needs from the detection engine. An interface so the
// handlers can be tested against a scripted state rather than a running daemon.
type Sentinel interface {
	State() sentinel.State
	Checks() sentinel.Checks
	Paused() bool
	Views() (sf, sq chainview.BackendHealth)
	Identities() (sf, sq chainview.Identity)
}

// Alerter is what the API needs from the notification subsystem.
type Alerter interface {
	// TestTransports delivers a synthetic alert. With no names it tests every
	// transport; with names it tests those, and reports any it does not recognise
	// rather than silently testing nothing.
	TestTransports(ctx context.Context, names ...string) ([]alert.SelfTestResult, error)
	// TransportNames lists the configured transports, in configuration order.
	TransportNames() []string
	// Raise records an alert the API itself decided on.
	Raise(ctx context.Context, c alert.Candidate)
}

// Lightning is what the API needs from the channel registry: how the user's
// Lightning nodes are doing. Nil when none is configured, which is a supported
// arrangement rather than a fault.
type Lightning interface {
	Health() []registry.SourceHealth
}

// Config configures the server.
type Config struct {
	Auth           config.AuthMode
	PasswordHash   string
	AllowedOrigins []string
	FrameAncestors []string
	// PlatformNotifications says the surrounding platform raises alerts by
	// reading this API, which is how StartOS and Umbrel do it.
	PlatformNotifications bool
	// SessionTTL is how long a session survives without use. Zero uses
	// DefaultSessionTTL.
	SessionTTL time.Duration
}

// Server holds everything the handlers need.
type Server struct {
	store    *store.Store
	sentinel Sentinel
	alerter  Alerter
	ln       Lightning
	cfg      Config
	now      func() time.Time
	log      *slog.Logger

	auth *authenticator
	mux  *http.ServeMux
}

// New builds a server. A nil logger discards and a nil clock reads the real one.
func New(
	st *store.Store,
	sen Sentinel,
	al Alerter,
	ln Lightning,
	cfg Config,
	log *slog.Logger,
	now func() time.Time,
) (*Server, error) {
	if st == nil {
		return nil, errors.New("api: a store is required")
	}
	if sen == nil {
		return nil, errors.New("api: a sentinel is required")
	}
	switch cfg.Auth {
	case config.AuthNone, config.AuthPlatform:
	case config.AuthPassword:
		if cfg.PasswordHash == "" {
			return nil, errors.New("api: password authentication needs a password hash")
		}
	default:
		return nil, errors.New("api: unknown authentication mode " + string(cfg.Auth))
	}
	if log == nil {
		log = slog.New(discardHandler{})
	}
	if now == nil {
		now = time.Now
	}

	s := &Server{
		store: st, sentinel: sen, alerter: al, ln: ln, cfg: cfg, now: now, log: log,
		mux: http.NewServeMux(),
	}
	s.auth = newAuthenticator(cfg, now, log, al)
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	// Liveness carries no data and needs no session: a platform health check runs
	// before anyone has logged in, and locking it behind auth would report a
	// healthy daemon as unhealthy.
	s.mux.HandleFunc("GET /api/v1/healthz", s.handleHealthz)

	s.mux.Handle("POST /api/v1/login", s.open(s.handleLogin))
	s.mux.Handle("POST /api/v1/logout", s.open(s.handleLogout))

	s.mux.Handle("GET /api/v1/status", s.guard(s.handleStatus))
	s.mux.Handle("GET /api/v1/timeline", s.guard(s.handleTimeline))
	s.mux.Handle("GET /api/v1/alerts", s.guard(s.handleAlerts))
	s.mux.Handle("POST /api/v1/alerts/{id}/ack", s.guard(s.handleAckAlert))
	s.mux.Handle("POST /api/v1/alerts/test", s.guard(s.handleTestAlerts))
	s.mux.Handle("POST /api/v1/split/confirm-resolution", s.guard(s.handleConfirmResolution))
}

// Handler returns the server's routes with the response headers applied.
func (s *Server) Handler() http.Handler { return s.secure(s.mux) }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Handler().ServeHTTP(w, r)
}

// open wraps a handler that needs no session but still needs its origin checked.
//
// Logging in is exactly as worth protecting from a cross-site request as
// anything else: an attacker who can make the browser log in as someone else has
// changed what the user is looking at.
func (s *Server) open(h http.HandlerFunc) http.Handler {
	return s.checkOrigin(h)
}

// guard wraps a handler that needs both an origin check and a session.
func (s *Server) guard(h http.HandlerFunc) http.Handler {
	return s.checkOrigin(s.requireAuth(h))
}

// secure applies the response headers that every response carries.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", s.contentSecurityPolicy())
		if strings.HasPrefix(r.URL.Path, "/api/") {
			// Channel balances and deadlines must not sit in a shared browser
			// cache, or on a proxy between here and the browser.
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// contentSecurityPolicy builds the policy served with every response.
//
// frame-ancestors rather than a blanket denial: both platforms embed their apps
// in their own dashboards, so denying framing outright would break the normal way
// people reach this. Naming who may frame it is the difference between that and
// letting any page overlay the dashboard to trick someone into clicking through.
func (s *Server) contentSecurityPolicy() string {
	ancestors := "'self'"
	if len(s.cfg.FrameAncestors) > 0 {
		ancestors = strings.Join(s.cfg.FrameAncestors, " ")
	}
	return "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors " + ancestors
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeData(w, map[string]bool{"ok": true})
}

// envelope is the shape of every response.
type envelope struct {
	Data  any       `json:"data"`
	Error *apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeData sends a successful response. Always 200: every endpoint that
// succeeds with a body says 200, and the ones that succeed without a body write
// 204 directly.
func writeData(w http.ResponseWriter, data any) {
	write(w, http.StatusOK, envelope{Data: data})
}

// writeError sends a failure. The message is for a person; the code is what
// anything else should branch on.
func writeError(w http.ResponseWriter, status int, code, message string) {
	write(w, status, envelope{Error: &apiError{Code: code, Message: message}})
}

func write(w http.ResponseWriter, status int, body envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// Nothing useful can be done about a write that fails after the status is
	// sent: the connection is already gone.
	_ = json.NewEncoder(w).Encode(body)
}

// fail logs the underlying reason and tells the caller only that something went
// wrong. A storage error can carry a file path, and this audience is never shown
// a raw error string.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.Error("a request could not be served",
		slog.String("path", r.URL.Path), slog.String("doing", what),
		slog.String("error", err.Error()))
	writeError(w, http.StatusInternalServerError, CodeInternal,
		"Something went wrong inside Forktower. The details are in its log.")
}
