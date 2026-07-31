package tower

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultTimeout bounds one read of a tower or a node.
const DefaultTimeout = 15 * time.Second

// maxErrorSnippet bounds how much of a failing response is quoted back.
const maxErrorSnippet = 256

// LNDOptions configures a reader for an LND over REST.
type LNDOptions struct {
	// BaseURL is the REST address, e.g. "https://tower-lnd:8080".
	BaseURL string
	// TLSCertPath is LND's self-signed certificate, pinned rather than trusted
	// through the system roots: the certificate is the identity here, and
	// accepting any certificate for this address would leave no way to notice
	// being pointed somewhere else.
	TLSCertPath string
	// MacaroonPath is the credential. `info:read` and `offchain:read` are all
	// that anything in this file needs — every call here is a GET, and the
	// mutating watchtower endpoints are deliberately absent.
	MacaroonPath string
	Timeout      time.Duration
}

// LND reads a watchtower, or a node's watchtower client, over REST.
//
// Read-only by construction. LND's API has endpoints that add and remove towers
// and terminate sessions; none of them appear here, because registering a tower
// is the user's action on their own node and doing it for them would mean
// holding a write credential for the life of the daemon.
type LND struct {
	baseURL string
	macHex  string
	http    *http.Client
}

// NewLND builds a reader.
func NewLND(opts LNDOptions) (*LND, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("tower: the address to read is required")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}

	raw, err := os.ReadFile(opts.MacaroonPath)
	if err != nil {
		return nil, fmt.Errorf("reading the tower credential: %w", err)
	}

	tlsCfg, err := pinnedTLS(opts.TLSCertPath)
	if err != nil {
		return nil, err
	}

	return &LND{
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		macHex:  hex.EncodeToString(raw),
		http: &http.Client{
			Timeout:   opts.Timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

// pinnedTLS trusts exactly the certificate at the given path, and nothing else.
//
// An empty path means the endpoint is plain HTTP, which is normal for a service
// on a private compose network and is the caller's decision to make.
func pinnedTLS(certPath string) (*tls.Config, error) {
	if certPath == "" {
		return nil, nil //nolint:nilnil // no certificate pinned means plain http
	}
	pem, err := os.ReadFile(certPath) //nolint:gosec // an operator-supplied path, by design
	if err != nil {
		return nil, fmt.Errorf("reading the tower's certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s does not hold a certificate we can read", certPath)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// get performs one read.
func (l *LND) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.baseURL+path, http.NoBody)
	if err != nil {
		return fmt.Errorf("building the request for %s: %w", path, err)
	}
	req.Header.Set("Grpc-Metadata-macaroon", l.macHex)

	resp, err := l.http.Do(req)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorSnippet))
		body := strings.TrimSpace(string(snippet))
		// The watchtower subservers answer an ordinary read with "not active"
		// rather than failing to answer, which is what makes a plain GET a clean
		// probe for whether the feature is switched on at all. No mutating call,
		// no guessing.
		if IsNotActive(errors.New(body)) {
			return ErrTowerNotActive
		}
		if isClientNotActive(body) {
			return ErrClientNotActive
		}
		return fmt.Errorf("answered %s for %s: %s", resp.Status, path, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("reading the answer to %s: %w", path, err)
	}
	return nil
}

// ErrClientNotActive means the node is running but its watchtower *client* is
// switched off, so nothing is being backed up anywhere.
//
// The single largest onboarding obstacle: `wtclient.active` is a configuration
// setting rather than something that can be switched on at runtime, and
// Forktower cannot change it — it will not hold a credential that could. So this
// is reported as a thing for the user to do, with the instructions, rather than
// as an error.
var ErrClientNotActive = errors.New("the Lightning node's watchtower client is switched off")

const clientNotActiveMarker = "watchtower client not active"

func isClientNotActive(msg string) bool {
	return strings.Contains(strings.ToLower(msg), clientNotActiveMarker)
}

// --- The tower's own side ---

type towerInfoJSON struct {
	Pubkey    string   `json:"pubkey"`
	Listeners []string `json:"listeners"`
	URIs      []string `json:"uris"`
}

type nodeInfoJSON struct {
	Version       string `json:"version"`
	BlockHeight   int32  `json:"block_height"`
	SyncedToChain bool   `json:"synced_to_chain"`
}

// Identity asks the tower what it is and where it listens.
//
// `GET /v2/watchtower/server` needs only `info:read`, so the read-only
// credential this daemon already asks for is enough — no additional permission,
// and no reason to widen the bake.
func (l *LND) Identity(ctx context.Context) (Identity, error) {
	var info towerInfoJSON
	if err := l.get(ctx, "/v2/watchtower/server", &info); err != nil {
		return Identity{}, err
	}
	// LND returns the pubkey base64-encoded over REST, because it is `bytes` in
	// the proto. Everything else in this project speaks hex.
	pubkey := info.Pubkey
	if raw, err := base64.StdEncoding.DecodeString(info.Pubkey); err == nil {
		pubkey = hex.EncodeToString(raw)
	}
	return Identity{
		Pubkey:    pubkey,
		Listeners: info.Listeners,
		URIs:      info.URIs,
	}, nil
}

// Chain asks the tower's node how it is doing against the chain it watches.
func (l *LND) Chain(ctx context.Context) (Chain, error) {
	var info nodeInfoJSON
	if err := l.get(ctx, "/v1/getinfo", &info); err != nil {
		return Chain{}, err
	}
	return Chain(info), nil
}

// --- The user's side: what their node is actually backing up ---

// Session is one negotiated watchtower session, as the user's node reports it.
type Session struct {
	Policy PolicyType
	// NumBackups is states successfully uploaded; NumPending is states accepted
	// locally and not yet acknowledged by the tower. A pending count that never
	// clears is a tower that is taking backups and not confirming them.
	NumBackups uint32
	NumPending uint32
	MaxBackups uint32
	// SweepSatPerVByte is the fee rate baked into every justice transaction this
	// session holds. Fixed when the session was negotiated and unbumpable
	// afterwards, because the tower holds no keys.
	SweepSatPerVByte uint32
}

// RegisteredTower is one tower the user's node knows about.
type RegisteredTower struct {
	Pubkey    string
	Addresses []string
	Sessions  []Session
}

// ClientStats is the node's own summary of its backing up.
type ClientStats struct {
	NumBackups        uint32
	NumPendingBackups uint32
	NumFailedBackups  uint32
	NumSessionsAcq    uint32
	NumSessionsExh    uint32
}

// ClientReader is what the coverage monitor needs from the user's node.
type ClientReader interface {
	// Towers lists the towers the node has registered, with their sessions, or
	// ErrClientNotActive if the watchtower client is switched off.
	Towers(ctx context.Context) ([]RegisteredTower, error)
	// Stats is the node's own summary.
	Stats(ctx context.Context) (ClientStats, error)
	// Version is the node's LND version, which decides which session types it
	// can create at all.
	Version(ctx context.Context) (Version, error)
}

type listTowersJSON struct {
	Towers []struct {
		Pubkey      string   `json:"pubkey"`
		Addresses   []string `json:"addresses"`
		SessionInfo []struct {
			PolicyType string `json:"policy_type"`
			Sessions   []struct {
				NumBackups        uint32 `json:"num_backups"`
				NumPendingBackups uint32 `json:"num_pending_backups"`
				MaxBackups        uint32 `json:"max_backups"`
				SweepSatPerVByte  uint32 `json:"sweep_sat_per_vbyte"`
			} `json:"sessions"`
		} `json:"session_info"`
	} `json:"towers"`
}

type statsJSON struct {
	NumBackups        uint32 `json:"num_backups"`
	NumPendingBackups uint32 `json:"num_pending_backups"`
	NumFailedBackups  uint32 `json:"num_failed_backups"`
	NumSessionsAcq    uint32 `json:"num_sessions_acquired"`
	NumSessionsExh    uint32 `json:"num_sessions_exhausted"`
}

// policyFromWire maps LND's policy-type enum onto ours.
//
// Over REST the enum arrives as its name, and LND has used both the bare form
// and a prefixed one across versions, so both are accepted rather than one being
// guessed at.
func policyFromWire(s string) PolicyType {
	switch strings.ToUpper(strings.TrimPrefix(strings.ToUpper(s), "POLICY_TYPE_")) {
	case "LEGACY":
		return PolicyLegacy
	case "ANCHOR":
		return PolicyAnchor
	case "TAPROOT":
		return PolicyTaproot
	default:
		return PolicyUnknown
	}
}

// Towers lists what the user's node has registered.
//
// `GET /v2/watchtower/client` needs `offchain:read`, which the read-only
// macaroon this daemon already asks for grants. The endpoints that would
// register or remove a tower need `offchain:write`, which it does not ask for
// and must not.
func (l *LND) Towers(ctx context.Context) ([]RegisteredTower, error) {
	var body listTowersJSON
	if err := l.get(ctx, "/v2/watchtower/client?include_sessions=true", &body); err != nil {
		return nil, err
	}

	out := make([]RegisteredTower, 0, len(body.Towers))
	for _, t := range body.Towers {
		pubkey := t.Pubkey
		if raw, err := base64.StdEncoding.DecodeString(t.Pubkey); err == nil {
			pubkey = hex.EncodeToString(raw)
		}
		tower := RegisteredTower{Pubkey: pubkey, Addresses: t.Addresses}
		for _, info := range t.SessionInfo {
			policy := policyFromWire(info.PolicyType)
			for _, s := range info.Sessions {
				tower.Sessions = append(tower.Sessions, Session{
					Policy:           policy,
					NumBackups:       s.NumBackups,
					NumPending:       s.NumPendingBackups,
					MaxBackups:       s.MaxBackups,
					SweepSatPerVByte: s.SweepSatPerVByte,
				})
			}
		}
		out = append(out, tower)
	}
	return out, nil
}

// Stats is the node's own summary of its backing up.
func (l *LND) Stats(ctx context.Context) (ClientStats, error) {
	var body statsJSON
	if err := l.get(ctx, "/v2/watchtower/client/stats", &body); err != nil {
		return ClientStats{}, err
	}
	return ClientStats(body), nil
}

// Version reads the node's LND version.
func (l *LND) Version(ctx context.Context) (Version, error) {
	var info nodeInfoJSON
	if err := l.get(ctx, "/v1/getinfo", &info); err != nil {
		return Version{}, err
	}
	return ParseVersion(info.Version), nil
}
