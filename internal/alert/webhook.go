package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultSendTimeout bounds one delivery attempt.
//
// Short on purpose: an alert that has not arrived within a few seconds is not
// going to help, and a transport that hangs must not hold up the ones behind it.
const DefaultSendTimeout = 10 * time.Second

// maxErrorBodyBytes is how much of a failing response is read back for the error
// message. Enough to see "invalid token" or a proxy's explanation; far short of
// an HTML error page.
const maxErrorBodyBytes = 256

// Webhook posts alerts as JSON to a URL the user chose.
type Webhook struct {
	name   string
	url    string
	client *http.Client
}

// Compile-time proof this satisfies the contract the alerter delivers through.
var _ Transport = (*Webhook)(nil)

// NewWebhook builds a webhook transport, rejecting a URL it could not post to.
//
// Validated here rather than at the first alert: a typo in a notification URL
// that only surfaces during a chain split has cost the user the one thing this
// software is for.
func NewWebhook(name, rawURL string, timeout time.Duration) (*Webhook, error) {
	if name == "" {
		return nil, fmt.Errorf("webhook transport needs a name")
	}
	if rawURL == "" {
		return nil, fmt.Errorf("webhook %q needs a url", name)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("webhook %q has an unusable url: %w", name, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf(
			"webhook %q must post to an http or https url, not %q", name, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("webhook %q has a url with no host", name)
	}
	if timeout <= 0 {
		timeout = DefaultSendTimeout
	}
	return &Webhook{name: name, url: rawURL, client: &http.Client{Timeout: timeout}}, nil
}

// Name implements Transport.
func (w *Webhook) Name() string { return w.name }

// Send implements Transport.
func (w *Webhook) Send(ctx context.Context, p Payload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("building the webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building the webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "forktower/"+PayloadVersion)

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("posting to the webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Drain what is there so the connection can be reused, and ignore it:
		// a success is a success.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil
	}

	// The URL is deliberately not in this message — it may carry a token, and the
	// transport's name identifies it well enough. The body is included because
	// "invalid token" from the far end is usually the whole diagnosis; scrubError
	// removes anything it echoed back at us.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	detail := strings.TrimSpace(string(snippet))
	if detail == "" {
		return fmt.Errorf("the webhook replied %s", resp.Status)
	}
	return fmt.Errorf("the webhook replied %s: %s", resp.Status, detail)
}
