package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/paulscode/forktower/internal/config"
)

// unsafeEndpoints is every request that changes something. A new one added
// without an origin check is exactly the gap this list exists to close, so the
// test enumerates them rather than sampling.
var unsafeEndpoints = []struct {
	method string
	path   string
	body   string
}{
	{http.MethodPost, "/api/v1/alerts/1/ack", ""},
	{http.MethodPost, "/api/v1/alerts/test", `{}`},
	{http.MethodPost, "/api/v1/split/confirm-resolution", `{"outcome":"SF_WON"}`},
	{http.MethodPost, "/api/v1/logout", ""},
	{http.MethodPost, "/api/v1/login", `{"password":"x"}`},
}

// Cookie authentication plus an unchecked POST is exploitable from any page the
// user happens to have open, and the dashboard sits at a guessable name on the
// local network. The attacker never needs to read the response: acknowledging
// every alert is damage enough.
func TestUnsafeRequestsFromAnotherSiteAreRefused(t *testing.T) {
	t.Parallel()

	for _, ep := range unsafeEndpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil)

			resp := h.doWith(t, ep.method, ep.path, ep.body, func(r *http.Request) {
				r.Header.Set("Origin", "https://evil.example.com")
			})
			if got := errorCode(t, resp); got != CodeBadOrigin {
				t.Errorf("got %q, want %q", got, CodeBadOrigin)
			}
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status %d, want 403", resp.StatusCode)
			}
		})
	}
}

// Every browser sends an Origin on an unsafe cross-origin request, so its absence
// is either a non-browser caller or an attempt to slip past the check. Treating
// it as same-origin would make the check trivially avoidable.
func TestUnsafeRequestsWithNoOriginAreRefused(t *testing.T) {
	t.Parallel()

	for _, ep := range unsafeEndpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil)

			resp := h.doWith(t, ep.method, ep.path, ep.body, nil)
			if got := errorCode(t, resp); got != CodeBadOrigin {
				t.Errorf("got %q, want %q", got, CodeBadOrigin)
			}
		})
	}
}

// A sandboxed frame sends `Origin: null`, which must never match.
func TestAnOpaqueOriginIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	resp := h.doWith(t, http.MethodPost, "/api/v1/alerts/test", `{}`, func(r *http.Request) {
		r.Header.Set("Origin", "null")
	})
	if got := errorCode(t, resp); got != CodeBadOrigin {
		t.Errorf("got %q, want %q", got, CodeBadOrigin)
	}
}

// Some browsers omit Origin on same-origin requests but always send Referer, so
// it is the documented fallback rather than a second chance for a stranger.
func TestRefererIsAcceptedWhenOriginIsAbsent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	resp := h.doWith(t, http.MethodPost, "/api/v1/alerts/test", `{}`, func(r *http.Request) {
		r.Header.Set("Referer", h.origin()+"/index.html")
	})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want a same-site Referer to be accepted", resp.StatusCode)
	}

	resp = h.doWith(t, http.MethodPost, "/api/v1/alerts/test", `{}`, func(r *http.Request) {
		r.Header.Set("Referer", "https://evil.example.com/page")
	})
	if got := errorCode(t, resp); got != CodeBadOrigin {
		t.Errorf("got %q, want %q for a cross-site Referer", got, CodeBadOrigin)
	}
}

// Reading is not changing. Requiring an origin on GET would break bookmarks and
// the platform's own health checks without preventing anything.
func TestReadingNeedsNoOrigin(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	for _, path := range []string{"/api/v1/status", "/api/v1/timeline", "/api/v1/alerts", "/api/v1/healthz"} {
		resp := h.doWith(t, http.MethodGet, path, "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s returned %d", path, resp.StatusCode)
		}
	}
}

// A configured origin covers a proxy that rewrites the Host header, which is the
// one case the same-host comparison cannot see.
func TestAConfiguredOriginIsAccepted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) {
		c.AllowedOrigins = []string{"https://forktower.example.com"}
	})

	resp := h.doWith(t, http.MethodPost, "/api/v1/alerts/test", `{}`, func(r *http.Request) {
		r.Header.Set("Origin", "https://forktower.example.com")
	})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want a configured origin to be accepted", resp.StatusCode)
	}
}

func TestResponseHeaders(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	resp := h.doWith(t, http.MethodGet, "/api/v1/status", "", nil)

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		// Channel balances and deadlines must not sit in a shared browser cache
		// or on a proxy in between.
		"Cache-Control": "no-store",
	}
	for header, value := range want {
		if got := resp.Header.Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}

	csp := resp.Header.Get("Content-Security-Policy")
	for _, directive := range []string{
		"default-src 'self'", "script-src 'self'", "object-src 'none'",
		"base-uri 'none'", "frame-ancestors 'self'",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("the policy is missing %q: %s", directive, csp)
		}
	}
}

// Both platforms embed their apps, so framing cannot simply be denied. Naming who
// may do it is the difference between that and letting any page overlay the
// dashboard to trick someone into clicking through it.
func TestFrameAncestorsCanNameThePlatform(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) {
		c.FrameAncestors = []string{"'self'", "https://umbrel.local"}
	})

	csp := h.doWith(t, http.MethodGet, "/api/v1/status", "", nil).
		Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'self' https://umbrel.local") {
		t.Errorf("the platform origin is not permitted to embed: %s", csp)
	}
}

// testPassword is the password every password-mode test signs in with.
const testPassword = "correct horse"

func passwordHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWithPassword(t, testPassword, nil)
}

func TestPasswordModeRefusesUntilSignedIn(t *testing.T) {
	t.Parallel()
	h := passwordHarness(t)

	resp := h.doWith(t, http.MethodGet, "/api/v1/status", "", nil)
	if got := errorCode(t, resp); got != CodeUnauthorized {
		t.Fatalf("got %q, want %q", got, CodeUnauthorized)
	}

	resp = h.do(t, http.MethodPost, "/api/v1/login", `{"password":"correct horse"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signing in returned %d", resp.StatusCode)
	}

	cookie := sessionCookie(t, resp)
	if !cookie.HttpOnly {
		t.Error("the session cookie is readable by scripts")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("Path = %q, want /", cookie.Path)
	}

	resp = h.doWith(t, http.MethodGet, "/api/v1/status", "", func(r *http.Request) {
		r.AddCookie(cookie)
	})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a signed-in request returned %d", resp.StatusCode)
	}
}

// The session id is replaced on sign-in, so an id an attacker planted in the
// browser beforehand is not the one that ends up authenticated.
func TestSigningInReplacesTheSessionId(t *testing.T) {
	t.Parallel()
	h := passwordHarness(t)

	first := sessionCookie(t, h.do(t, http.MethodPost, "/api/v1/login", `{"password":"correct horse"}`))

	second := sessionCookie(t, h.doWith(t, http.MethodPost, "/api/v1/login",
		`{"password":"correct horse"}`, func(r *http.Request) {
			r.Header.Set("Origin", h.origin())
			r.AddCookie(first)
		}))

	if first.Value == second.Value {
		t.Fatal("the session id was reused across sign-ins")
	}
	// And the planted one is gone, not merely superseded.
	resp := h.doWith(t, http.MethodGet, "/api/v1/status", "", func(r *http.Request) {
		r.AddCookie(first)
	})
	if resp.StatusCode == http.StatusOK {
		t.Error("the previous session id still works")
	}
}

func TestSigningOutInvalidatesTheSession(t *testing.T) {
	t.Parallel()
	h := passwordHarness(t)

	cookie := sessionCookie(t, h.do(t, http.MethodPost, "/api/v1/login", `{"password":"correct horse"}`))

	resp := h.doWith(t, http.MethodPost, "/api/v1/logout", "", func(r *http.Request) {
		r.Header.Set("Origin", h.origin())
		r.AddCookie(cookie)
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("signing out returned %d, want 204", resp.StatusCode)
	}

	resp = h.doWith(t, http.MethodGet, "/api/v1/status", "", func(r *http.Request) {
		r.AddCookie(cookie)
	})
	if resp.StatusCode == http.StatusOK {
		t.Error("the session still works after signing out")
	}
}

func TestASessionExpires(t *testing.T) {
	t.Parallel()
	h := newHarnessWithPassword(t, testPassword, func(c *Config) {
		c.SessionTTL = time.Hour
	})

	cookie := sessionCookie(t, h.do(t, http.MethodPost, "/api/v1/login", `{"password":"correct horse"}`))

	h.clock.Add(int64((2 * time.Hour).Seconds()))
	resp := h.doWith(t, http.MethodGet, "/api/v1/status", "", func(r *http.Request) {
		r.AddCookie(cookie)
	})
	if resp.StatusCode == http.StatusOK {
		t.Error("an expired session still works")
	}
}

// Behind both platforms' app proxies every request arrives from one address, so a
// per-IP limit is either global anyway or trivially defeated by forging a
// forwarding header. This proves varying that header does not widen the limit.
func TestLoginRateLimitIsGlobalAndIgnoresForwardingHeaders(t *testing.T) {
	t.Parallel()
	h := passwordHarness(t)

	var limited bool
	for i := range LoginAttemptsPerMinute + 3 {
		resp := h.doWith(t, http.MethodPost, "/api/v1/login", `{"password":"wrong"}`,
			func(r *http.Request) {
				r.Header.Set("Origin", h.origin())
				// A different claimed client on every attempt.
				r.Header.Set("X-Forwarded-For", fmt.Sprintf("10.0.0.%d", i+1))
				r.Header.Set("X-Real-IP", fmt.Sprintf("10.0.1.%d", i+1))
			})
		if resp.StatusCode == http.StatusTooManyRequests {
			if got := errorCode(t, resp); got != CodeRateLimited {
				t.Errorf("got %q, want %q", got, CodeRateLimited)
			}
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("forging a forwarding header defeated the login rate limit")
	}

	// The correct password is refused too while the limit holds: an attacker who
	// could keep guessing by racing the owner's own sign-in would gain nothing
	// from a limit that let the right answer through.
	resp := h.do(t, http.MethodPost, "/api/v1/login", `{"password":"correct horse"}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status %d during a rate limit, want 429", resp.StatusCode)
	}
}

// A burst of failed sign-ins against this dashboard is worth telling the user
// about: it usually means it is reachable from somewhere it should not be.
func TestRepeatedFailedSignInsRaiseAnAlert(t *testing.T) {
	t.Parallel()
	h := passwordHarness(t)

	for range LoginFailuresBeforeAlert + 2 {
		h.do(t, http.MethodPost, "/api/v1/login", `{"password":"wrong"}`)
		// Step past the backoff so the attempts actually reach the password check.
		h.clock.Add(int64(LoginBackoffCap.Seconds()) + 61)
	}

	var found bool
	for _, kind := range h.alerter.raisedKinds() {
		if kind == KindLoginFailures {
			found = true
		}
	}
	if !found {
		t.Errorf("no alert was raised after %d failed sign-ins; got %v",
			LoginFailuresBeforeAlert, h.alerter.raisedKinds())
	}
}

// Platform mode delegates to the app proxy that already authenticated the user.
// Inventing a second check here would give people two passwords for one thing and
// no more safety.
func TestPlatformModeDelegates(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.Auth = config.AuthPlatform })

	if resp := h.doWith(t, http.MethodGet, "/api/v1/status", "", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want the platform's own authentication to be trusted", resp.StatusCode)
	}
	// And there is no password to offer, so saying so is clearer than a refusal.
	if resp := h.do(t, http.MethodPost, "/api/v1/login", `{"password":"x"}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d for a login in platform mode, want 404", resp.StatusCode)
	}
}

// Liveness runs before anyone has signed in. Locking it behind authentication
// would make a healthy daemon report as unhealthy and get itself restarted.
func TestHealthzNeedsNoSession(t *testing.T) {
	t.Parallel()
	h := passwordHarness(t)

	resp := h.doWith(t, http.MethodGet, "/api/v1/healthz", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	got := decode[map[string]bool](t, resp)
	if !got["ok"] {
		t.Errorf("got %v, want ok", got)
	}
}

func TestNewRejectsAnUnusableConfiguration(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)

	if _, err := New(h.store, h.sen, h.alerter, Config{Auth: config.AuthPassword}, nil, nil); err == nil {
		t.Error("password mode was accepted with no password set, which nobody could ever sign in to")
	}
	if _, err := New(h.store, h.sen, h.alerter, Config{Auth: "something-else"}, nil, nil); err == nil {
		t.Error("an unknown authentication mode was accepted")
	}
	if _, err := New(nil, h.sen, h.alerter, Config{}, nil, nil); err == nil {
		t.Error("a server with no store was accepted")
	}
	if _, err := New(h.store, nil, h.alerter, Config{}, nil, nil); err == nil {
		t.Error("a server with no sentinel was accepted")
	}
}

func sessionCookie(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie was set")
	return nil
}

func newHarnessWithPassword(t *testing.T, password string, mutate func(*Config)) *harness {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return newHarness(t, func(c *Config) {
		c.Auth = config.AuthPassword
		c.PasswordHash = string(hash)
		if mutate != nil {
			mutate(c)
		}
	})
}
