package lnd

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/paulscode/forktower/internal/registry"
	"github.com/paulscode/forktower/internal/store"
)

// DefaultTimeout bounds one request. A ceiling, not a target: callers pass their
// own deadlines and the shorter wins.
const DefaultTimeout = 20 * time.Second

// Options is what a client needs to reach an LND node.
type Options struct {
	// BaseURL is the node's REST address, e.g. "https://127.0.0.1:8080".
	BaseURL string
	// TLSCertPath is LND's self-signed certificate. Pinned rather than trusted
	// through the system roots: the certificate *is* the identity here, and a
	// daemon that would accept any certificate for this address has no way to
	// notice being pointed somewhere else.
	TLSCertPath string
	// MacaroonPath is the credential. Read-only is what it should be, and the
	// client says so loudly when it is not.
	MacaroonPath string
	Timeout      time.Duration
	Logger       *slog.Logger
}

// Client reads channels from an LND node.
//
// Read-only by construction: there is no method here that changes anything, and
// nothing in this package builds a request to an endpoint that would.
type Client struct {
	baseURL string
	cred    Credential
	http    *http.Client
	log     *slog.Logger
}

// New builds a client, and inspects the credential it was given.
//
// An over-privileged macaroon is reported, never refused. Both target platforms
// hand out admin macaroons; refusing one would mean no protection at all, which
// is a worse answer than working with more authority than we want and saying so.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("lnd: the node's address is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(discardHandler{})
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}

	cred, err := LoadMacaroon(opts.MacaroonPath)
	if err != nil {
		return nil, err
	}

	tlsConfig, err := pinnedTLS(opts.TLSCertPath)
	if err != nil {
		return nil, err
	}

	c := &Client{
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		cred:    cred,
		http: &http.Client{
			Timeout:   opts.Timeout,
			Transport: &http.Transport{TLSClientConfig: tlsConfig},
		},
		log: opts.Logger,
	}

	switch {
	case !cred.Readable:
		c.log.Warn("could not read what your Lightning credential permits, so " +
			"Forktower cannot confirm it is read-only")
	case cred.Overprivileged():
		// Named actions, never the credential itself.
		c.log.Warn("the Lightning credential allows more than Forktower needs; "+
			"it only ever reads. Consider: lncli bakemacaroon info:read "+
			"offchain:read onchain:read peers:read",
			slog.String("permissions", cred.summary()))
	}
	return c, nil
}

// Credential returns what was established about the macaroon, for the readiness
// check that surfaces it.
func (c *Client) Credential() Credential { return c.cred }

// summary lists the permissions, for a log line. Never the macaroon.
func (c Credential) summary() string {
	out := make([]string, 0, len(c.Permissions))
	for _, p := range c.Permissions {
		out = append(out, p.String())
	}
	return strings.Join(out, " ")
}

// pinnedTLS builds a TLS configuration trusting exactly one certificate.
func pinnedTLS(certPath string) (*tls.Config, error) {
	if certPath == "" {
		return nil, errors.New("lnd: the node's TLS certificate is required; " +
			"Forktower pins it rather than trusting whatever answers")
	}
	pem, err := os.ReadFile(certPath) //nolint:gosec // an operator-supplied path, by design
	if err != nil {
		return nil, fmt.Errorf("reading the Lightning node's certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("lnd: the file at tls_cert_path is not a certificate")
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// get performs one read.
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, http.NoBody)
	if err != nil {
		return fmt.Errorf("building the request for %s: %w", path, err)
	}
	req.Header.Set("Grpc-Metadata-macaroon", c.cred.Hex)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reading %s from your Lightning node: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("your Lightning node answered %s for %s: %s",
			resp.Status, path, strings.TrimSpace(string(snippet)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("reading the answer to %s: %w", path, err)
	}
	return nil
}

// Info identifies the node.
func (c *Client) Info(ctx context.Context) (registry.NodeInfo, error) {
	var info infoJSON
	if err := c.get(ctx, "/v1/getinfo", &info); err != nil {
		return registry.NodeInfo{}, err
	}
	if info.IdentityPubkey == "" {
		return registry.NodeInfo{}, errors.New("lnd: the node did not say who it is")
	}
	return registry.NodeInfo{
		Pubkey: info.IdentityPubkey,
		Alias:  info.Alias,
		Impl:   store.ImplLND,
	}, nil
}

// Snapshot reads everything Forktower needs from the node in one pass.
//
// Open and pending channels together, because a channel closing is exactly when
// it matters most and a picture that omitted it would be reassuring and wrong.
func (c *Client) Snapshot(ctx context.Context) (registry.Snapshot, error) {
	node, err := c.Info(ctx)
	if err != nil {
		return registry.Snapshot{}, err
	}

	var open listChannelsJSON
	if err := c.get(ctx, "/v1/channels", &open); err != nil {
		return registry.Snapshot{}, err
	}

	snap := registry.Snapshot{Node: node}
	for _, ch := range open.Channels {
		rec, mapErr := mapChannel(ch)
		if mapErr != nil {
			// One unreadable channel must not lose the others: the rest of the
			// user's channels still need watching.
			c.log.Warn("could not read one of your channels",
				slog.String("error", mapErr.Error()))
			continue
		}
		snap.Channels = append(snap.Channels, rec)
	}

	pending, err := c.pending(ctx)
	if err != nil {
		// Pending channels are the ones closing. Losing them is worth a warning
		// but not the whole snapshot.
		c.log.Warn("could not read your channels that are closing",
			slog.String("error", err.Error()))
	}
	snap.Channels = append(snap.Channels, pending...)

	return snap, nil
}

type pendingJSON struct {
	PendingOpen []struct {
		Channel channelJSON `json:"channel"`
	} `json:"pending_open_channels"`
	PendingForceClosing []struct {
		Channel   channelJSON `json:"channel"`
		ClosingTx string      `json:"closing_txid"`
	} `json:"pending_force_closing_channels"`
	WaitingClose []struct {
		Channel channelJSON `json:"channel"`
	} `json:"waiting_close_channels"`
}

// pending reads the channels that are opening or closing.
func (c *Client) pending(ctx context.Context) ([]registry.ChannelRecord, error) {
	var p pendingJSON
	if err := c.get(ctx, "/v1/channels/pending", &p); err != nil {
		return nil, err
	}

	var out []registry.ChannelRecord
	add := func(ch channelJSON, state store.CloseState, txid string) {
		rec, err := mapChannel(ch)
		if err != nil {
			c.log.Warn("could not read one of your closing channels",
				slog.String("error", err.Error()))
			return
		}
		rec.CloseState = state
		rec.CloseTxID = txid
		out = append(out, rec)
	}

	for _, e := range p.PendingOpen {
		add(e.Channel, store.CloseOpen, "")
	}
	for _, e := range p.PendingForceClosing {
		add(e.Channel, store.ClosePending, e.ClosingTx)
	}
	for _, e := range p.WaitingClose {
		add(e.Channel, store.ClosePending, "")
	}
	return out, nil
}

// Watch calls notify whenever the node says a channel changed.
//
// Only a nudge: the caller re-reads everything, so a missed notification costs
// latency rather than correctness. That is deliberate, and it is why this
// returning is not fatal — the poll is what guarantees progress, exactly as it
// does for the chain views.
func (c *Client) Watch(ctx context.Context, notify func()) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/channels/subscribe", http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Grpc-Metadata-macaroon", c.cred.Hex)

	// No client timeout on a stream that is meant to stay open; the context is
	// what ends it.
	streamer := &http.Client{Transport: c.http.Transport}
	resp, err := streamer.Do(req)
	if err != nil {
		return fmt.Errorf("subscribing to channel events: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("your Lightning node answered %s when subscribing", resp.Status)
	}

	// Newline-delimited JSON. The contents are not read: any event at all means
	// the picture may have changed, and the caller re-reads it anyway.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		notify()
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("the channel event stream ended: %w", err)
	}
	return nil
}
