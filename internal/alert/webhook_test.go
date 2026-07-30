package alert

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A typo in a notification URL that only surfaces during a chain split has cost
// the user the one thing this software is for, so it is caught at startup.
func TestNewWebhookRejectsUrlsItCouldNotPostTo(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"empty":           "",
		"no scheme":       "hooks.example.com/x",
		"wrong scheme":    "ftp://hooks.example.com/x",
		"a file path":     "file:///etc/passwd",
		"no host":         "https:///x",
		"unparseable":     "https://exa mple.com/\x7f",
		"shell-style var": "$WEBHOOK_URL",
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewWebhook("hook", raw, time.Second); err == nil {
				t.Errorf("accepted %q, which no alert could ever be posted to", raw)
			}
		})
	}

	if _, err := NewWebhook("", "https://hooks.example.com/x", time.Second); err == nil {
		t.Error("accepted a transport with no name, which every delivery is recorded against")
	}
}

func TestWebhookReportsWhatTheServerSaid(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid token"))
	}))
	t.Cleanup(srv.Close)

	hook, err := NewWebhook("hook", srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	err = hook.Send(context.Background(), Payload{Tier: "warning"})
	if err == nil {
		t.Fatal("a 401 was reported as a successful delivery")
	}
	// The far end's own explanation is usually the whole diagnosis.
	if !strings.Contains(err.Error(), "invalid token") {
		t.Errorf("the server's explanation was dropped: %v", err)
	}
	// The URL is not in the message: it may carry a token, and the transport's
	// name identifies it well enough.
	if strings.Contains(err.Error(), srv.URL) {
		t.Errorf("the error echoes the webhook url: %v", err)
	}
}

func TestWebhookAcceptsAnyTwoHundred(t *testing.T) {
	t.Parallel()

	for _, code := range []int{http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		hook, err := NewWebhook("hook", srv.URL, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err := hook.Send(context.Background(), Payload{}); err != nil {
			t.Errorf("status %d was treated as a failure: %v", code, err)
		}
		srv.Close()
	}
}

// A transport that hangs must not hold up the ones behind it: the point of
// configuring several is that they do not fail together.
func TestWebhookHonoursItsContext(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })

	hook, err := NewWebhook("hook", srv.URL, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := hook.Send(ctx, Payload{}); err == nil {
		t.Fatal("a request that never completed was reported as delivered")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Send waited %v, ignoring its context", elapsed)
	}
}

// The client's own timeout is the backstop for a caller that passes a context
// with no deadline.
func TestNewWebhookSuppliesADefaultTimeout(t *testing.T) {
	t.Parallel()

	hook, err := NewWebhook("hook", "https://hooks.example.com/x", 0)
	if err != nil {
		t.Fatal(err)
	}
	if hook.client.Timeout != DefaultSendTimeout {
		t.Errorf("timeout = %v, want %v", hook.client.Timeout, DefaultSendTimeout)
	}
}
