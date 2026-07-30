package cln

import (
	"bytes"
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

// DefaultTimeout bounds one request.
const DefaultTimeout = 20 * time.Second

// Options is what a client needs to reach a Core Lightning node.
type Options struct {
	// BaseURL is the clnrest address, e.g. "https://127.0.0.1:3010".
	BaseURL string
	// RunePath is the credential file. Restricted to reads is what it should be,
	// and the client says so loudly when it is not.
	RunePath string
	// TLSCertPath pins the node's certificate when it serves https. Optional:
	// clnrest is often plain http on the loopback address, where there is
	// nothing to pin and nothing in between.
	TLSCertPath string
	Timeout     time.Duration
	Logger      *slog.Logger
}

// Client reads channels from a Core Lightning node.
//
// Read-only by construction: every method here calls one of the node's list or
// info commands, and nothing in this package builds a request to anything else.
//
// Poll-only. Core Lightning has no channel subscription of the kind LND offers,
// so there is no push to lose — which makes this the simpler of the two
// adapters and, for the same reason, the one with fewer ways to go quiet.
type Client struct {
	baseURL string
	rune    Rune
	http    *http.Client
	log     *slog.Logger
}

// New builds a client and inspects the credential it was given.
//
// An unrestricted rune is reported, never refused — same rule as the LND
// adapter, and for the same reason: a user whose credential is broader than we
// would like is still a user who needs protecting.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("cln: the node's address is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(discardHandler{})
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}

	credential, err := LoadRune(opts.RunePath)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{}
	if opts.TLSCertPath != "" {
		tlsConfig, tlsErr := pinnedTLS(opts.TLSCertPath)
		if tlsErr != nil {
			return nil, tlsErr
		}
		transport.TLSClientConfig = tlsConfig
	}

	c := &Client{
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		rune:    credential,
		http:    &http.Client{Timeout: opts.Timeout, Transport: transport},
		log:     opts.Logger,
	}

	switch {
	case !credential.Readable:
		c.log.Warn("could not read what your Lightning credential permits, so " +
			"Forktower cannot confirm it is restricted to reading")
	case credential.Unrestricted():
		c.log.Warn("the Lightning credential is unrestricted; Forktower only ever " +
			"reads. Consider a rune limited to getinfo and the list commands")
	case !credential.RestrictsToReads():
		c.log.Warn("the Lightning credential may permit more than reading; " +
			"Forktower only ever reads")
	}
	return c, nil
}

// Credential returns what was established about the rune, for the readiness
// check that surfaces it.
func (c *Client) Credential() Rune { return c.rune }

func pinnedTLS(certPath string) (*tls.Config, error) {
	pem, err := os.ReadFile(certPath) //nolint:gosec // an operator-supplied path, by design
	if err != nil {
		return nil, fmt.Errorf("reading the Lightning node's certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("cln: the file at tls_cert_path is not a certificate")
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// call performs one read. clnrest takes a POST per method, with the rune in a
// header and any parameters as a JSON body.
func (c *Client) call(ctx context.Context, method string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/"+method, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return fmt.Errorf("building the request for %s: %w", method, err)
	}
	req.Header.Set("Rune", c.rune.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s on your Lightning node: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("your Lightning node answered %s for %s: %s",
			resp.Status, method, strings.TrimSpace(string(snippet)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("reading the answer to %s: %w", method, err)
	}
	return nil
}

// Info identifies the node.
func (c *Client) Info(ctx context.Context) (registry.NodeInfo, error) {
	var info getinfoJSON
	if err := c.call(ctx, "getinfo", &info); err != nil {
		return registry.NodeInfo{}, err
	}
	if info.ID == "" {
		return registry.NodeInfo{}, errors.New("cln: the node did not say who it is")
	}
	return registry.NodeInfo{Pubkey: info.ID, Alias: info.Alias, Impl: store.ImplCLN}, nil
}

// Snapshot reads everything Forktower needs from the node in one pass.
func (c *Client) Snapshot(ctx context.Context) (registry.Snapshot, error) {
	node, err := c.Info(ctx)
	if err != nil {
		return registry.Snapshot{}, err
	}

	var channels listPeerChannelsJSON
	if err := c.call(ctx, "listpeerchannels", &channels); err != nil {
		return registry.Snapshot{}, err
	}

	snap := registry.Snapshot{Node: node}
	for _, ch := range channels.Channels {
		rec, mapErr := mapChannel(ch)
		if mapErr != nil {
			// One unreadable channel must not lose the others: the rest still
			// need watching.
			c.log.Warn("could not read one of your channels",
				slog.String("error", mapErr.Error()))
			continue
		}
		snap.Channels = append(snap.Channels, rec)
	}
	return snap, nil
}
