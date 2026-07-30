package alert

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/paulscode/forktower/internal/store"
)

// ntfy priorities. The scale is 1–5; 3 is the default, and 5 is the one that
// overrides a phone's quiet hours — which is why nothing below an urgent alert
// gets it.
const (
	ntfyPriorityUrgent  = 5
	ntfyPriorityHigh    = 4
	ntfyPriorityDefault = 3
)

// Ntfy delivers through an ntfy server: the user's own, or a public one.
//
// The public instance is a third party who can read every topic they can guess,
// so this transport is content-free by default like any other third-party one.
// A self-hosted server is the documented private option.
type Ntfy struct {
	name   string
	url    string
	token  string
	client *http.Client
}

var _ Transport = (*Ntfy)(nil)

// NewNtfy builds an ntfy transport, rejecting a URL it could not post to.
func NewNtfy(name, rawURL, token string, timeout time.Duration) (*Ntfy, error) {
	if name == "" {
		return nil, fmt.Errorf("ntfy transport needs a name")
	}
	if rawURL == "" {
		return nil, fmt.Errorf("ntfy transport %q needs a url, including the topic", name)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("ntfy transport %q has an unusable url: %w", name, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf(
			"ntfy transport %q must post to an http or https url, not %q", name, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("ntfy transport %q has a url with no host", name)
	}
	// A topic is the whole address: posting to a bare server URL publishes
	// nowhere, and the user would see nothing and have no idea why.
	if strings.Trim(u.Path, "/") == "" {
		return nil, fmt.Errorf(
			"ntfy transport %q needs the topic in its url, like https://ntfy.sh/your-topic", name)
	}
	if timeout <= 0 {
		timeout = DefaultSendTimeout
	}
	return &Ntfy{name: name, url: rawURL, token: token, client: &http.Client{Timeout: timeout}}, nil
}

// Name implements Transport.
func (n *Ntfy) Name() string { return n.name }

// Send implements Transport.
func (n *Ntfy) Send(ctx context.Context, p Payload) error {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, n.url, bytes.NewReader([]byte(p.Message)))
	if err != nil {
		return fmt.Errorf("building the ntfy request: %w", err)
	}

	// Headers must be Latin-1, so everything put in one is drawn from constants
	// and from the alert kind, which is ASCII by construction. The message goes in
	// the body, where UTF-8 is fine.
	req.Header.Set("Title", Headline(store.Tier(p.Tier)))
	req.Header.Set("Priority", strconv.Itoa(ntfyPriority(store.Tier(p.Tier))))
	req.Header.Set("Tags", p.Kind)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("User-Agent", "forktower/"+PayloadVersion)
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("posting to ntfy: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil
	}

	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if detail := strings.TrimSpace(string(snippet)); detail != "" {
		return fmt.Errorf("ntfy replied %s: %s", resp.Status, detail)
	}
	return fmt.Errorf("ntfy replied %s", resp.Status)
}

// ntfyPriority maps a tier onto ntfy's 1–5 scale.
func ntfyPriority(t store.Tier) int {
	switch tierRank(t) {
	case 3:
		return ntfyPriorityUrgent
	case 2:
		return ntfyPriorityHigh
	default:
		return ntfyPriorityDefault
	}
}
