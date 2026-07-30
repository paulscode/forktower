package alert

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/store"
)

type ntfyRequest struct {
	body    string
	headers http.Header
}

func ntfyServer(t *testing.T, status int) (srv *httptest.Server, requests <-chan ntfyRequest) {
	t.Helper()
	got := make(chan ntfyRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- ntfyRequest{body: string(body), headers: r.Header.Clone()}
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server, got
}

// Priority is what decides whether a phone rings at 3am. Getting it wrong in
// either direction is a real failure: too low and the alert is missed, too high
// and the user turns the whole thing off.
func TestNtfyPriorityAndTitleFollowTheTier(t *testing.T) {
	t.Parallel()

	cases := []struct {
		tier         store.Tier
		wantPriority string
		wantTitle    string
	}{
		{store.TierCritical, "5", headlineUrgent},
		{store.TierLoss, "5", headlineUrgent},
		{store.TierWarning, "4", headlineAttention},
		{store.TierInfo, "3", headlineRoutine},
		{store.TierResolved, "3", headlineRoutine},
	}

	for _, tc := range cases {
		t.Run(string(tc.tier), func(t *testing.T) {
			t.Parallel()
			srv, got := ntfyServer(t, http.StatusOK)

			n, err := NewNtfy("my-ntfy", srv.URL+"/my-topic", "", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if err := n.Send(context.Background(), Payload{
				Tier: string(tc.tier), Kind: KindSplitDetected, Message: "something happened",
			}); err != nil {
				t.Fatal(err)
			}

			req := <-got
			if p := req.headers.Get("Priority"); p != tc.wantPriority {
				t.Errorf("priority = %q, want %q", p, tc.wantPriority)
			}
			if title := req.headers.Get("Title"); title != tc.wantTitle {
				t.Errorf("title = %q, want %q", title, tc.wantTitle)
			}
			// The kind travels as a tag so it is visible and sortable, and the
			// message is the body — a title alone is not actionable.
			if tag := req.headers.Get("Tags"); tag != KindSplitDetected {
				t.Errorf("tags = %q, want %q", tag, KindSplitDetected)
			}
			if req.body != "something happened" {
				t.Errorf("body = %q, want the message", req.body)
			}
		})
	}
}

func TestNtfySendsItsTokenOnlyWhenThereIsOne(t *testing.T) {
	t.Parallel()

	srv, got := ntfyServer(t, http.StatusOK)

	withToken, err := NewNtfy("n", srv.URL+"/topic", "tk_secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := withToken.Send(context.Background(), Payload{Tier: "info"}); err != nil {
		t.Fatal(err)
	}
	if auth := (<-got).headers.Get("Authorization"); auth != "Bearer tk_secret" {
		t.Errorf("Authorization = %q, want the bearer token", auth)
	}

	without, err := NewNtfy("n", srv.URL+"/topic", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := without.Send(context.Background(), Payload{Tier: "info"}); err != nil {
		t.Fatal(err)
	}
	// An empty Authorization header is rejected by some servers, so it must be
	// absent rather than blank.
	if auth := (<-got).headers.Get("Authorization"); auth != "" {
		t.Errorf("Authorization = %q, want no header at all", auth)
	}
}

// A URL with no topic publishes nowhere: the user sees nothing and has no way to
// tell why, which is worse than a startup error.
func TestNewNtfyRejectsUrlsThatWouldPublishNowhere(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"empty":          "",
		"no topic":       "https://ntfy.sh",
		"only a slash":   "https://ntfy.sh/",
		"no scheme":      "ntfy.sh/topic",
		"wrong scheme":   "ftp://ntfy.sh/topic",
		"no host":        "https:///topic",
		"unparseable":    "https://nt fy.sh/\x7f",
		"unexpanded env": "$NTFY_URL",
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewNtfy("n", raw, "", time.Second); err == nil {
				t.Errorf("accepted %q", raw)
			}
		})
	}

	if _, err := NewNtfy("", "https://ntfy.sh/topic", "", time.Second); err == nil {
		t.Error("accepted a transport with no name")
	}
}

func TestNtfyReportsWhatTheServerSaid(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("topic requires authentication"))
	}))
	t.Cleanup(srv.Close)

	n, err := NewNtfy("n", srv.URL+"/topic", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}

	err = n.Send(context.Background(), Payload{Tier: "info"})
	if err == nil {
		t.Fatal("a 403 was reported as delivered")
	}
	if !strings.Contains(err.Error(), "requires authentication") {
		t.Errorf("the server's explanation was dropped: %v", err)
	}
	// Whatever the error says, nothing that reaches the database may carry the
	// topic — it is a bearer secret.
	if strings.Contains(scrubError(err), "/topic") {
		t.Errorf("the topic survived scrubbing: %v", scrubError(err))
	}
}

// Every header value must be Latin-1. A message with an em dash in it — which
// several of the real messages have — must not end up in one.
func TestNtfyKeepsNonAsciiOutOfHeaders(t *testing.T) {
	t.Parallel()

	srv, got := ntfyServer(t, http.StatusOK)
	n, err := NewNtfy("n", srv.URL+"/topic", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}

	const message = "The split may be ending — one of the chains has stopped producing blocks."
	if err := n.Send(context.Background(), Payload{
		Tier: "info", Kind: KindSplitResolving, Message: message,
	}); err != nil {
		t.Fatal(err)
	}

	req := <-got
	for _, name := range []string{"Title", "Tags", "Priority"} {
		value := req.headers.Get(name)
		for _, r := range value {
			if r > 0xFF {
				t.Errorf("header %s carries %q, which is not Latin-1: %q", name, r, value)
			}
		}
	}
	if req.body != message {
		t.Errorf("body = %q, want the message unchanged", req.body)
	}
}
