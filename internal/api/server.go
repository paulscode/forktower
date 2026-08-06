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
	"github.com/paulscode/forktower/internal/deadline"
	"github.com/paulscode/forktower/internal/registry"
	"github.com/paulscode/forktower/internal/sentinel"
	"github.com/paulscode/forktower/internal/store"
	"github.com/paulscode/forktower/internal/watcher"
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
	// CodeDeadlinesCounting refuses to wind watching down while a clock is
	// running against the user.
	CodeDeadlinesCounting = "deadlines_counting"
)

// The routes the dashboard is told to call. Named because they appear both here
// and in the action a readiness item offers, and a path spelled twice is a path
// that can be spelled differently.
const (
	PathRescan    = "/api/v1/rescan"
	PathStandDown = "/api/v1/watch/stand-down"
	PathResume    = "/api/v1/watch/resume"
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

// Watcher is what the API needs from the engine that reads the other chain: how
// far it has got, and the ability to be asked to read some of it again.
type Watcher interface {
	Progress() watcher.Progress
	Rescan(ctx context.Context, from int32) (queuedFrom, queuedTo int32, queued bool)
	RescanFromFork(ctx context.Context) (queuedFrom, queuedTo int32, queued bool)
}

// StandDown is the switch that turns watching off on purpose.
type StandDown interface {
	Active() bool
	Set(ctx context.Context, down bool) error
}

// Deadlines is what the API needs from the countdown engine. Nil is allowed, and
// means the countdowns are not being reported yet.
type Deadlines interface {
	Status() deadline.Status
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
	// RunsOwnWatchtower says this installation brings up a watchtower of its
	// own, so that "there is no tower" can be told apart from "the tower has not
	// answered yet". Without it the readiness sees an empty list and concludes
	// the wrong one.
	RunsOwnWatchtower bool
	// Platform is which packaging this is, declared rather than guessed. It
	// decides which directions the setup guidance gives for the one thing
	// Forktower cannot do on the user's behalf.
	Platform config.Platform
	// SessionTTL is how long a session survives without use. Zero uses
	// DefaultSessionTTL.
	SessionTTL time.Duration
}

// Server holds everything the handlers need.
type Server struct {
	store     *store.Store
	sentinel  Sentinel
	alerter   Alerter
	ln        Lightning
	deadlines Deadlines
	watcher   Watcher
	standDown StandDown
	bootstrap Bootstrap
	cfg       Config
	now       func() time.Time
	log       *slog.Logger

	auth *authenticator
	mux  *http.ServeMux
}

// New builds a server. A nil logger discards and a nil clock reads the real one.
func New(
	st *store.Store,
	sen Sentinel,
	al Alerter,
	ln Lightning,
	dl Deadlines,
	wa Watcher,
	sd StandDown,
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
		store: st, sentinel: sen, alerter: al, ln: ln, deadlines: dl,
		watcher: wa, standDown: sd,
		cfg: cfg, now: now, log: log,
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
	s.mux.Handle("GET /api/v1/setup", s.guard(s.handleSetup))
	s.mux.Handle("GET /api/v1/bootstrap", s.guard(s.handleBootstrap))
	s.mux.Handle("POST "+PathBootstrapStart, s.guard(s.handleBootstrapStart))
	s.mux.Handle("POST "+PathBootstrapCancel, s.guard(s.handleBootstrapCancel))
	s.mux.Handle("GET /api/v1/timeline", s.guard(s.handleTimeline))
	s.mux.Handle("GET /api/v1/channels", s.guard(s.handleChannels))
	s.mux.Handle("GET /api/v1/spends", s.guard(s.handleSpends))
	s.mux.Handle("GET /api/v1/deadlines", s.guard(s.handleDeadlines))
	s.mux.Handle("GET /api/v1/towers", s.guard(s.handleTowers))
	s.mux.Handle("GET /api/v1/mirror", s.guard(s.handleMirror))
	s.mux.Handle("POST /api/v1/channels/{id}/mirror-funding",
		s.guard(s.handleChannelMirrorOptIn))
	s.mux.Handle("GET /api/v1/alerts", s.guard(s.handleAlerts))
	s.mux.Handle("POST /api/v1/alerts/{id}/ack", s.guard(s.handleAckAlert))
	s.mux.Handle("POST /api/v1/alerts/test", s.guard(s.handleTestAlerts))
	s.mux.Handle("POST /api/v1/split/confirm-resolution", s.guard(s.handleConfirmResolution))
	s.mux.Handle("POST "+PathRescan, s.guard(s.handleRescan))
	s.mux.Handle("POST "+PathStandDown, s.guard(s.handleStandDown))
	s.mux.Handle("POST "+PathResume, s.guard(s.handleResume))
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
