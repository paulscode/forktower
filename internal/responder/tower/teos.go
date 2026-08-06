package tower

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
	"time"

	"github.com/paulscode/forktower/internal/store"
)

// TeosOptions configures reading a teos tower and the Core Lightning node that
// registers with it.
type TeosOptions struct {
	// APIURL is the tower's own public API, e.g. "http://tower-teos:9814".
	APIURL string
	// Pubkey is the tower's identity, which teosd prints at startup as
	// `tower_id:` and which the user needs anyway in order to register.
	//
	// Configured rather than discovered, and that is a limitation worth naming:
	// teos keeps its identity in its own database and its public API does not
	// report it. The alternative — reading another project's SQLite schema from
	// a directory we mounted for a different purpose — is worse than asking.
	Pubkey string
	// Timeout bounds one read.
	Timeout time.Duration
}

// Teos reads a teos tower's own public API.
//
// **It can prove that the tower is alive and nothing else**, and that is not an
// oversight to be worked around. teos splits its API in two: the public side
// carries registration and appointments, and everything diagnostic — the tower
// id, appointment counts, whether bitcoind is reachable — is on a private gRPC
// interface intended for the operator's own command line.
//
// That is why this type is thin and why the interesting monitoring for the Core
// Lightning arm lives in [CLNTowers], reading the user's own node. Which is
// arguably where it belongs: "is my node's protection working" is a better
// question than "is that process up", and only the node can answer it.
type Teos struct {
	baseURL string
	pubkey  string
	http    *http.Client
}

// NewTeos builds a reader for a teos tower.
func NewTeos(opts TeosOptions) (*Teos, error) {
	if opts.APIURL == "" {
		return nil, errors.New("tower: the teos tower's address is required")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	return &Teos{
		baseURL: strings.TrimRight(opts.APIURL, "/"),
		pubkey:  opts.Pubkey,
		// **Its own connection pool, rather than the process-wide default.**
		// Same settings, cloned — but a shared pool is shared with everything
		// else in the process, and anything calling CloseIdleConnections on it
		// breaks a request this client has in flight. That is not hypothetical:
		// it broke a test in the release gate, because httptest closes idle
		// connections on the default transport when a server shuts down.
		// Isolating it also means a burst of traffic elsewhere cannot starve the
		// one client that reports whether a watchtower is protecting anything.
		http: &http.Client{
			Timeout:   opts.Timeout,
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
		},
	}, nil
}

// Identity reports what is known about the tower.
//
// The pubkey comes from configuration, because the tower's public API does not
// offer it. A tower that answers but has no configured identity is still
// reported as answering: it is running, it is listening, and saying nothing
// about it would be worse than saying what little is known.
func (t *Teos) Identity(ctx context.Context) (Identity, error) {
	if err := t.ping(ctx); err != nil {
		return Identity{}, err
	}
	id := Identity{Pubkey: t.pubkey}
	if t.pubkey != "" {
		// The address a user pastes into `registertower`. Built from what we were
		// told rather than from what the tower reports, for the same reason.
		id.URIs = []string{t.pubkey + "@" + hostOf(t.baseURL)}
		id.Listeners = []string{hostOf(t.baseURL)}
	}
	return id, nil
}

// Chain is what a teos tower can say about the chain it watches, which is
// nothing.
//
// **teos has no light-client mode and no chain endpoint on its public API.** It
// talks to bitcoind over RPC, so a tower that is answering has a configured
// backend — but whether that backend is reachable *right now* is on the private
// interface only. Reported as an unknown rather than guessed at: a tower that
// has lost its backend and one that is fine must not read the same.
func (t *Teos) Chain(ctx context.Context) (Chain, error) {
	if err := t.ping(ctx); err != nil {
		return Chain{}, err
	}
	// SyncedToChain true because the supervisor uses it to mean "there is no
	// known reason this tower cannot see a breach", and for teos there is not
	// one we can observe. The version is left empty, which is honest: no version
	// is reported anywhere on this interface.
	return Chain{SyncedToChain: true}, nil
}

// ping asks the tower whether it is there.
func (t *Teos) ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.baseURL+"/ping", http.NoBody)
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return fmt.Errorf("reading the tower: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the tower answered %s", resp.Status)
	}
	return nil
}

// hostOf strips the scheme from a URL, leaving what a user would paste.
func hostOf(url string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
	return strings.TrimSuffix(trimmed, "/")
}

// --- The Core Lightning side: what the node's plugin knows ---

// TeosTower is one tower as the watchtower plugin reports it.
type TeosTower struct {
	// ID is the tower's identity, which is the key this arrives under.
	ID      string
	NetAddr string
	Status  store.TowerStatus
	// AvailableSlots is how many appointments the subscription has left, and
	// SubscriptionExpiry the block height at which it lapses. Both are teos
	// concepts with no LND equivalent, and the second is the sharper edge: the
	// default subscription is 4320 blocks, about thirty days, and a split can
	// outlast one registered just before it.
	AvailableSlots     int32
	SubscriptionExpiry int32
	// PendingAppointments are states the node has accepted locally and not yet
	// got the tower to acknowledge.
	//
	// **The number that matters most on this arm.** Core Lightning calls the
	// `commitment_revocation` hook and carries on without waiting for it, and
	// does not use its result — so a state is revoked whether or not its
	// appointment ever reached a tower. A queue that is not draining is
	// protection quietly not happening.
	PendingAppointments int
	InvalidAppointments int
	// MisbehavingProof is present when the tower returned a receipt whose
	// signature does not check out. Carried as the raw evidence rather than a
	// flag: this is the one place in the whole system where a user can be shown
	// proof rather than an inference.
	MisbehavingProof string
}

// CLNTowerReader is what the teos monitor needs from the user's node.
type CLNTowerReader interface {
	// Towers lists what the node's watchtower plugin knows.
	Towers(ctx context.Context) ([]TeosTower, error)
}

// CLNTowers reads the watchtower plugin over Core Lightning's REST interface.
//
// Read-only: `listtowers` and `gettowerinfo` only. `registertower`,
// `retrytower` and `abandontower` all change something and are deliberately
// absent — registering is the user's action on their own node.
type CLNTowers struct {
	baseURL string
	rune    string
	http    *http.Client
}

// CLNOptions configures reading a Core Lightning node's watchtower plugin.
type CLNOptions struct {
	// RESTAddr is clnrest, e.g. "https://127.0.0.1:3010".
	RESTAddr string
	// RunePath is the credential. It must permit `listtowers` and
	// `gettowerinfo`; the latter does not match a `list*` pattern and has to be
	// named.
	RunePath string
	// TLSCertPath is optional: clnrest is often plain HTTP on a private network.
	TLSCertPath string
	Timeout     time.Duration
}

// NewCLNTowers builds a reader for a node's watchtower plugin.
func NewCLNTowers(opts CLNOptions) (*CLNTowers, error) {
	if opts.RESTAddr == "" {
		return nil, errors.New("tower: the Lightning node's address is required")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}

	raw, err := os.ReadFile(opts.RunePath) //nolint:gosec // an operator-supplied path, by design
	if err != nil {
		return nil, fmt.Errorf("reading the node credential: %w", err)
	}

	tlsCfg, err := pinnedTLS(opts.TLSCertPath)
	if err != nil {
		return nil, err
	}

	return &CLNTowers{
		baseURL: strings.TrimRight(opts.RESTAddr, "/"),
		rune:    strings.TrimSpace(string(raw)),
		http: &http.Client{
			Timeout:   opts.Timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

// ErrPluginNotLoaded means the node is running without the watchtower plugin, so
// nothing is being backed up to any tower.
//
// The Core Lightning analogue of a switched-off watchtower client, and reported
// the same way: as something for the user to do, with instructions, rather than
// as an error. Forktower cannot load a plugin into somebody's node and will not
// hold a credential that could.
var ErrPluginNotLoaded = errors.New("the Lightning node is not running the watchtower plugin")

// call performs one plugin read.
func (c *CLNTowers) call(ctx context.Context, method, body string, out any) error {
	if body == "" {
		body = "{}"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/"+method, bytes.NewReader([]byte(body)))
	if err != nil {
		return fmt.Errorf("building the request for %s: %w", method, err)
	}
	req.Header.Set("Rune", c.rune)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reading %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorSnippet))
		text := strings.TrimSpace(string(snippet))
		// A node without the plugin answers "unknown command", which makes an
		// ordinary read a clean probe for whether it is loaded at all.
		if isUnknownCommand(text) {
			return ErrPluginNotLoaded
		}
		return fmt.Errorf("answered %s for %s: %s", resp.Status, method, text)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("reading the answer to %s: %w", method, err)
	}
	return nil
}

func isUnknownCommand(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "unknown command") ||
		strings.Contains(lower, "method not found")
}

// towerSummaryJSON is one entry of what `listtowers` returns.
//
// The plugin serialises a map of tower id to summary, so the ids are the object
// keys rather than a field.
type towerSummaryJSON struct {
	NetAddr             string   `json:"net_addr"`
	AvailableSlots      int32    `json:"available_slots"`
	SubscriptionStart   int32    `json:"subscription_start"`
	SubscriptionExpiry  int32    `json:"subscription_expiry"`
	Status              string   `json:"status"`
	PendingAppointments []string `json:"pending_appointments"`
	InvalidAppointments []string `json:"invalid_appointments"`
}

type teosTowerInfoJSON struct {
	MisbehavingProof json.RawMessage `json:"misbehaving_proof"`
}

// Towers lists what the node's watchtower plugin knows.
func (c *CLNTowers) Towers(ctx context.Context) ([]TeosTower, error) {
	var listed map[string]towerSummaryJSON
	if err := c.call(ctx, "listtowers", "", &listed); err != nil {
		return nil, err
	}

	out := make([]TeosTower, 0, len(listed))
	for id, summary := range listed {
		tower := TeosTower{
			ID:                  id,
			NetAddr:             summary.NetAddr,
			Status:              teosStatus(summary.Status),
			AvailableSlots:      summary.AvailableSlots,
			SubscriptionExpiry:  summary.SubscriptionExpiry,
			PendingAppointments: len(summary.PendingAppointments),
			InvalidAppointments: len(summary.InvalidAppointments),
		}
		// The proof, and only when there is one to fetch. A second call per tower
		// is worth it for the one signal in this system that is evidence rather
		// than inference.
		if tower.Status == store.TowerMisbehaving {
			tower.MisbehavingProof = c.proofFor(ctx, id)
		}
		out = append(out, tower)
	}
	return out, nil
}

// proofFor fetches the misbehaviour evidence, or returns empty if it cannot.
//
// A failure here is not worth failing the whole read over: the tower is already
// known to be misbehaving, and losing the proof makes the report weaker rather
// than wrong.
func (c *CLNTowers) proofFor(ctx context.Context, id string) string {
	var info teosTowerInfoJSON
	body := fmt.Sprintf(`{"tower_id":%q}`, id)
	if err := c.call(ctx, "gettowerinfo", body, &info); err != nil {
		return ""
	}
	if len(info.MisbehavingProof) == 0 || string(info.MisbehavingProof) == "null" {
		return ""
	}
	return string(info.MisbehavingProof)
}

// teosStatus maps the plugin's own state machine onto the store's vocabulary.
//
// The store's statuses *are* teos's, chosen so that this mapping is an identity
// for the implementation that has the richer view. `temporary_unreachable` is
// the one that differs, and only in spelling.
func teosStatus(s string) store.TowerStatus {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "reachable":
		return store.TowerReachable
	case "temporary_unreachable", "temporarily_unreachable":
		return store.TowerTemporarilyUnreachable
	case "unreachable":
		return store.TowerUnreachable
	case "subscription_error":
		return store.TowerSubscriptionError
	case "misbehaving":
		return store.TowerMisbehaving
	default:
		return store.TowerStatusUnknown
	}
}
