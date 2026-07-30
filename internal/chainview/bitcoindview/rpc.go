package bitcoindview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
)

// rpcError is the error object a node returns inside an otherwise successful
// HTTP response.
//
// The code matters more than the message: messages are prose and change between
// versions, whereas the codes are part of the interface. Both are kept, because
// the message is often the only explanation of *why* a transaction was refused.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("bitcoind rpc error %d: %s", e.Code, e.Message)
}

// Node error codes this package interprets. Taken from Bitcoin Core's protocol,
// where they are stable across versions in a way the messages are not.
const (
	// codeInvalidAddressOrKey is returned for an unknown block or transaction
	// hash, among other things.
	codeInvalidAddressOrKey = -5
	// codeInvalidParameter covers a height past the tip.
	codeInvalidParameter = -8
	// codeVerifyAlreadyInChain means the transaction being submitted is already
	// mined.
	codeVerifyAlreadyInChain = -27
	// codeMethodNotFound means this node is too old for the call, or was built
	// without it.
	codeMethodNotFound = -32601
)

// client is a minimal JSON-RPC client for a Bitcoin node.
//
// Hand-rolled rather than using the ecosystem's client because that one accepts
// no context, so a cancelled call would leave its request running — unacceptable
// in a daemon that has to keep working while a node misbehaves.
type client struct {
	opts Options
	http *http.Client

	// Credentials are cached and re-read on rejection. A node rewrites its cookie
	// file when it restarts, and a long-running watcher must survive that without
	// a human noticing.
	credMu sync.RWMutex
	cred   string
}

func newClient(opts Options) (*client, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: opts.timeout()}
	}
	return &client{opts: opts, http: hc}, nil
}

// credentials returns the basic-auth pair, reading the cookie file if that is how
// this node authenticates.
func (c *client) credentials(forceReload bool) (string, error) {
	if !forceReload {
		c.credMu.RLock()
		cached := c.cred
		c.credMu.RUnlock()
		if cached != "" {
			return cached, nil
		}
	}

	c.credMu.Lock()
	defer c.credMu.Unlock()

	if c.opts.CookiePath == "" {
		if c.opts.User == "" {
			return "", ErrNoAuth
		}
		c.cred = c.opts.User + ":" + c.opts.Pass
		return c.cred, nil
	}

	raw, err := os.ReadFile(c.opts.CookiePath)
	if err != nil {
		return "", fmt.Errorf("reading rpc cookie %s: %w", c.opts.CookiePath, err)
	}
	cookie := strings.TrimSpace(string(raw))
	if cookie == "" {
		return "", fmt.Errorf("rpc cookie %s is empty: %w", c.opts.CookiePath, ErrNoAuth)
	}
	c.cred = cookie
	return c.cred, nil
}

type rpcRequest struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// call makes one request and decodes its result into out, which may be nil when
// the result is not needed.
//
// On an authentication rejection it re-reads the credentials once and retries,
// which is what makes a node restart survivable rather than terminal.
func (c *client) call(ctx context.Context, out any, method string, params ...any) error {
	body, err := encodeParams(method, params)
	if err != nil {
		return err
	}

	raw, err := c.post(ctx, method, body, false)
	if errors.Is(err, errUnauthorized) {
		raw, err = c.post(ctx, method, body, true)
	}
	if err != nil {
		return err
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding %s result: %w", method, err)
	}
	return nil
}

// errUnauthorized signals that credentials were refused, so they are worth
// re-reading once before giving up.
var errUnauthorized = errors.New("bitcoindview: credentials refused")

func (c *client) post(ctx context.Context, method string, body []byte, reloadCreds bool) (json.RawMessage, error) {
	cred, err := c.credentials(reloadCreds)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.RPCURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.opts.userAgent())
	user, pass, _ := strings.Cut(cred, ":")
	req.SetBasicAuth(user, pass)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded read: a node is trusted to be honest, not to be well. A block is a
	// few megabytes; this ceiling is far above that and far below anything that
	// would exhaust memory.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("reading %s response: %w", method, err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		if reloadCreds {
			return nil, fmt.Errorf("%s: %w after re-reading credentials — check the "+
				"cookie path or user and password", method, errUnauthorized)
		}
		return nil, errUnauthorized
	}

	var parsed rpcResponse
	// A node reports its own errors inside a 500, so the body is worth decoding
	// whatever the status.
	if jsonErr := json.Unmarshal(payload, &parsed); jsonErr != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("calling %s: node returned %s", method, resp.Status)
		}
		return nil, fmt.Errorf("decoding %s response: %w", method, jsonErr)
	}
	if parsed.Error != nil {
		return nil, parsed.Error
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("calling %s: node returned %s with no error detail",
			method, resp.Status)
	}
	return parsed.Result, nil
}

// maxResponseBytes caps a single response. Blocks are the largest thing read here
// and are a few megabytes at most.
const maxResponseBytes = 128 << 20

func encodeParams(method string, params []any) ([]byte, error) {
	raw := make([]json.RawMessage, 0, len(params))
	for i, p := range params {
		b, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("encoding %s parameter %d: %w", method, i, err)
		}
		raw = append(raw, b)
	}
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "1.0",
		ID:      "forktower",
		Method:  method,
		Params:  raw,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding %s request: %w", method, err)
	}
	return body, nil
}

// asRPCError extracts a node error from err, if that is what it is.
func asRPCError(err error) (*rpcError, bool) {
	var e *rpcError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
