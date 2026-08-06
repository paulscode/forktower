package bitcoindview

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/redact"
)

// View reads one chain from a Bitcoin node.
//
// Safe for concurrent use. Subscriptions are added separately; everything here is
// request/response.
type View struct {
	c *client
	// stall records a consumer that stopped keeping up, so Health can report it
	// rather than leaving a silent gap that looks like a quiet chain.
	stall stallState
	// mempoolStall is the same for unconfirmed transactions, kept apart because
	// only the block one says anything about whether the chain can be seen.
	mempoolStall stallState
	// now exists so the stall reporter's interval can be driven in a test
	// rather than waited out.
	now func() time.Time
	// catchingUp is the node's own initial-block-download flag, remembered from
	// the last Health call so that the notification reader can consult it
	// without an RPC round trip per message. See skipMempoolWhileSyncing.
	catchingUp atomic.Bool
}

// catchUpMargin is how far blocks may trail headers before the node counts as
// replaying history rather than keeping up.
//
// Two, not zero: a node at the tip can be a block or so behind its own headers
// for a moment during ordinary propagation, and treating that as a sync would
// drop the sightings that arrive exactly then — which are the ones worth having.
const catchUpMargin = 2

// skipMempoolWhileSyncing reports whether published transactions are worth
// reading at all right now.
//
// **A node in initial block download republishes the entire chain's
// transactions.** Core's `rawtx` topic fires for every transaction added to the
// mempool *and* for every transaction in a newly connected block, so a sync
// replays hundreds of millions of them. On real hardware this produced 12.3
// million deserialized-and-immediately-discarded transactions in four days, on
// the same four cores the node was trying to sync with.
//
// None of them could matter. The whole value of watching the mempool is seeing a
// spend *before* it confirms, and a transaction arriving inside a block being
// connected during a sync has been confirmed for years. So while the node says
// it is catching up, the bytes are dropped before they are parsed.
//
// Deliberately reading the *node's* own flag rather than our own progress guess:
// it is the node that knows, and it is the same field the health check reports.
func (v *View) skipMempoolWhileSyncing() bool { return v.catchingUp.Load() }

// New builds a view over the node described by opts.
//
// Nothing is contacted here: construction succeeding says the options are usable,
// not that the node is. Callers learn about the node from Health, which is
// designed to report trouble rather than fail.
func New(opts Options) (*View, error) {
	c, err := newClient(opts)
	if err != nil {
		return nil, err
	}
	return &View{c: c, now: time.Now}, nil
}

// BestBlock returns the node's current tip.
func (v *View) BestBlock(ctx context.Context) (chainview.BlockMeta, error) {
	var hashHex string
	if err := v.c.call(ctx, &hashHex, "getbestblockhash"); err != nil {
		return chainview.BlockMeta{}, mapError(err)
	}
	h, err := chainhash.NewHashFromStr(hashHex)
	if err != nil {
		return chainview.BlockMeta{}, fmt.Errorf("node returned an unreadable best block hash %q: %w",
			hashHex, err)
	}
	return v.BlockHeaderByHash(ctx, *h)
}

// blockHeaderJSON is the subset of `getblockheader` this package needs.
type blockHeaderJSON struct {
	Hash              string `json:"hash"`
	Height            int32  `json:"height"`
	Time              int64  `json:"time"`
	PreviousBlockHash string `json:"previousblockhash"`
}

// BlockHeaderByHash returns a header the node knows, on its active chain or not.
func (v *View) BlockHeaderByHash(ctx context.Context, h chainhash.Hash) (chainview.BlockMeta, error) {
	var hdr blockHeaderJSON
	// Verbose form: the header's own bytes do not carry its height, and height is
	// what every caller actually wants.
	if err := v.c.call(ctx, &hdr, "getblockheader", h.String(), true); err != nil {
		return chainview.BlockMeta{}, mapError(err)
	}
	return toBlockMeta(hdr)
}

func toBlockMeta(hdr blockHeaderJSON) (chainview.BlockMeta, error) {
	hash, err := chainhash.NewHashFromStr(hdr.Hash)
	if err != nil {
		return chainview.BlockMeta{}, fmt.Errorf("node returned an unreadable block hash %q: %w",
			hdr.Hash, err)
	}
	meta := chainview.BlockMeta{
		BlockRef: chainview.BlockRef{Hash: *hash, Height: hdr.Height},
		Time:     time.Unix(hdr.Time, 0).UTC(),
	}
	// Absent on the genesis block, which is the one block with no predecessor.
	if hdr.PreviousBlockHash != "" {
		prev, err := chainhash.NewHashFromStr(hdr.PreviousBlockHash)
		if err != nil {
			return chainview.BlockMeta{}, fmt.Errorf(
				"node returned an unreadable previous block hash %q: %w", hdr.PreviousBlockHash, err)
		}
		meta.PrevHash = *prev
	}
	return meta, nil
}

// BlockHashByHeight returns the active-chain hash at a height.
func (v *View) BlockHashByHeight(ctx context.Context, height int32) (chainhash.Hash, error) {
	var hashHex string
	if err := v.c.call(ctx, &hashHex, "getblockhash", height); err != nil {
		return chainhash.Hash{}, mapError(err)
	}
	h, err := chainhash.NewHashFromStr(hashHex)
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf("node returned an unreadable hash %q for height %d: %w",
			hashHex, height, err)
	}
	return *h, nil
}

// Block returns a full block.
//
// Fetched as raw hex and decoded here rather than asking the node to expand it
// into JSON: the JSON form is much larger, slower to produce and parse, and drops
// detail that matters when inspecting how an output is being spent.
func (v *View) Block(ctx context.Context, h chainhash.Hash) (*wire.MsgBlock, error) {
	var raw string
	if err := v.c.call(ctx, &raw, "getblock", h.String(), 0); err != nil {
		return nil, mapError(err)
	}
	blockBytes, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("decoding block %s: %w", h, err)
	}
	var blk wire.MsgBlock
	if err := blk.Deserialize(bytes.NewReader(blockBytes)); err != nil {
		return nil, fmt.Errorf("deserialising block %s: %w", h, err)
	}
	return &blk, nil
}

// MatchBlock always reports a possible match.
//
// Correct rather than lazy: a full node has no test cheaper than reading the
// block, and the contract makes false a promise while true is only a hint. Saying
// false without checking would be a false promise in the direction that loses
// money, so this backend never says it.
func (v *View) MatchBlock(context.Context, chainhash.Hash, chainview.WatchSet) (bool, error) {
	return true, nil
}

// Broadcast submits a raw transaction.
//
// Already-known and already-mined both count as success. This is called on retry
// paths and after restarts, where "it is already there" is exactly the outcome
// wanted — treating it as a failure would turn a working mirror into a source of
// spurious alerts.
func (v *View) Broadcast(ctx context.Context, tx *wire.MsgTx) error {
	if tx == nil {
		return errors.New("bitcoindview: nothing to broadcast")
	}
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return fmt.Errorf("serialising transaction: %w", err)
	}
	if err := v.c.call(ctx, nil, "sendrawtransaction", hex.EncodeToString(buf.Bytes())); err != nil {
		if isAlreadyKnown(err) {
			return nil
		}
		return mapError(err)
	}
	return nil
}

// isAlreadyKnown reports whether an error means the transaction is already in the
// memory pool or already mined.
//
// Matched on the numeric code where there is one, and on the message otherwise:
// a node signals an already-mined transaction with a stable code, but an
// already-in-pool one arrives as prose whose exact wording has varied between
// versions.
func isAlreadyKnown(err error) bool {
	e, ok := asRPCError(err)
	if !ok {
		return false
	}
	if e.Code == codeVerifyAlreadyInChain {
		return true
	}
	msg := strings.ToLower(e.Message)
	for _, known := range []string{
		"txn-already-in-mempool",
		"txn-already-known",
		"transaction already in block chain",
	} {
		if strings.Contains(msg, known) {
			return true
		}
	}
	return false
}

// blockchainInfoJSON is the subset of `getblockchaininfo` this package needs.
type blockchainInfoJSON struct {
	Chain                string  `json:"chain"`
	Blocks               int32   `json:"blocks"`
	Headers              int32   `json:"headers"`
	BestBlockHash        string  `json:"bestblockhash"`
	VerificationProgress float64 `json:"verificationprogress"`
	InitialBlockDownload bool    `json:"initialblockdownload"`
	PruneHeight          int32   `json:"pruneheight"`
	Pruned               bool    `json:"pruned"`
}

// Health reports what the node says about itself.
//
// It reports rather than fails: an unreachable node is a state the daemon must
// display and keep working around, not an error that stops it. Whether this view
// is on the right chain is not decided here — that cannot be seen from inside a
// single view.
func (v *View) Health(ctx context.Context) (chainview.BackendHealth, error) {
	var info blockchainInfoJSON
	if err := v.c.call(ctx, &info, "getblockchaininfo"); err != nil {
		return chainview.BackendHealth{
			State:  chainview.HealthDown,
			Detail: firstLine(err.Error()),
		}, nil
	}

	health := chainview.BackendHealth{
		State:           chainview.HealthOK,
		SyncProgress:    info.VerificationProgress,
		BehindOnMempool: v.mempoolStall.stalled.Load(),
	}

	if hdr, err := v.BlockHeaderByHash(ctx, mustHash(info.BestBlockHash)); err == nil {
		health.Tip = hdr
	}

	// Peer count is a separate call, and its absence is not itself unhealthy.
	var netInfo struct {
		Connections int `json:"connections"`
	}
	if err := v.c.call(ctx, &netInfo, "getnetworkinfo"); err == nil {
		health.PeerCount = netInfo.Connections
	}

	// Remembered for the notification reader, which cannot afford an RPC call per
	// published transaction.
	//
	// **Blocks behind headers, not the node's own initial-block-download flag.**
	// That flag was the obvious signal and the wrong one: a regtest node reports
	// itself in initial block download while sitting idle at its own tip, so the
	// gate stayed shut for ever and mempool watching was silently dead on every
	// regtest deployment — which is where all the scenario testing happens, and
	// where a lost pre-confirmation sighting would go unnoticed precisely because
	// the tests were the thing being broken.
	//
	// What the gate is actually for is the phase where a node is replaying
	// history: headers arrive fast and blocks follow slowly behind them, and
	// every transaction in every connected block is republished. Blocks trailing
	// headers *is* that phase, and at the tip the two are equal.
	replaying := info.Headers-info.Blocks > catchUpMargin
	v.catchingUp.Store(replaying)
	health.ReplayingHistory = replaying

	switch {
	case info.InitialBlockDownload:
		health.State = chainview.HealthSyncing
		health.Detail = fmt.Sprintf("catching up: %.1f%% verified, %d of %d blocks",
			info.VerificationProgress*100, info.Blocks, info.Headers)
	case info.Headers > info.Blocks:
		// Headers ahead of blocks means it knows of work it has not validated yet.
		health.State = chainview.HealthSyncing
		health.Detail = fmt.Sprintf("%d blocks behind the headers it has seen",
			info.Headers-info.Blocks)
	case v.stall.stalled.Load():
		// A dropped *block* notification is not a quiet chain, and the two look
		// identical from outside. Say so. Unconfirmed transactions are counted
		// apart and deliberately do not reach here — see emitTx.
		health.State = chainview.HealthDegraded
		health.Detail = fmt.Sprintf(
			"a consumer stopped keeping up and %d block notifications were dropped",
			v.stall.dropped.Load())
	case health.PeerCount == 0:
		// No peers means no new blocks will arrive. The node looks fine and is
		// blind, which is the shape of failure worth naming loudly.
		health.State = chainview.HealthDegraded
		health.Detail = "no peers connected, so no new blocks can arrive"
	}
	return health, nil
}

// Network returns the chain name the node reports, for the startup check that the
// two views are on the same network at all.
func (v *View) Network(ctx context.Context) (string, error) {
	var info blockchainInfoJSON
	if err := v.c.call(ctx, &info, "getblockchaininfo"); err != nil {
		return "", mapError(err)
	}
	return info.Chain, nil
}

// PruneHeight reports the lowest block the node still has, and whether it is
// pruning at all.
//
// Needed because a pruned node loses the ability to answer for old blocks, and a
// watcher whose history has been pruned away past the point the chains separated
// has a blind spot it must tell the user about rather than discover later.
func (v *View) PruneHeight(ctx context.Context) (height int32, pruned bool, err error) {
	var info blockchainInfoJSON
	if callErr := v.c.call(ctx, &info, "getblockchaininfo"); callErr != nil {
		return 0, false, mapError(callErr)
	}
	return info.PruneHeight, info.Pruned, nil
}

// chainTipJSON is one entry from `getchaintips`.
type chainTipJSON struct {
	Height    int32  `json:"height"`
	Hash      string `json:"hash"`
	BranchLen int32  `json:"branchlen"`
	Status    string `json:"status"`
}

// ChainTips returns every branch tip the node knows about, including ones it has
// rejected.
//
// The rejected ones are the point. A node that fetched a block from the other
// chain and refused it is direct, local evidence of a rule disagreement — it needs
// no agreement from any peer and cannot be fabricated by one.
func (v *View) ChainTips(ctx context.Context) ([]chainview.ChainTip, error) {
	var raw []chainTipJSON
	if err := v.c.call(ctx, &raw, "getchaintips"); err != nil {
		return nil, mapError(err)
	}
	out := make([]chainview.ChainTip, 0, len(raw))
	for _, t := range raw {
		h, err := chainhash.NewHashFromStr(t.Hash)
		if err != nil {
			return nil, fmt.Errorf("node returned an unreadable chain tip hash %q: %w", t.Hash, err)
		}
		out = append(out, chainview.ChainTip{
			Hash:      *h,
			Height:    t.Height,
			BranchLen: t.BranchLen,
			Status:    t.Status,
		})
	}
	return out, nil
}

// deploymentInfoJSON is the shape of `getdeploymentinfo`.
type deploymentInfoJSON struct {
	Height      int32 `json:"height"`
	Deployments map[string]struct {
		Type   string `json:"type"`
		Active bool   `json:"active"`
		Height int32  `json:"height"`
		BIP9   *struct {
			Bit                 int32  `json:"bit"`
			StartTime           int64  `json:"start_time"`
			Timeout             int64  `json:"timeout"`
			MinActivationHeight int32  `json:"min_activation_height"`
			MaxActivationHeight int32  `json:"max_activation_height"`
			Status              string `json:"status"`
			Since               int32  `json:"since"`
			Statistics          *struct {
				Period    int32 `json:"period"`
				Elapsed   int32 `json:"elapsed"`
				Count     int32 `json:"count"`
				Threshold int32 `json:"threshold"`
			} `json:"statistics"`
		} `json:"bip9"`
	} `json:"deployments"`
}

// Deployment returns what the node knows about one named soft fork.
//
// Read from the node rather than configured, because this is where the values
// that matter actually live: the node's own view cannot go stale, cannot be a
// number copied wrongly out of a document, and generalises to any future fork
// deployed the same way. It also yields the current signalling share, which during
// a run-up is the single most informative number available to a user.
//
// Returns ErrNotFound when the node has no such deployment, and ErrUnsupported on
// a node too old to answer at all.
func (v *View) Deployment(ctx context.Context, name string) (*chainview.Deployment, error) {
	var info deploymentInfoJSON
	if err := v.c.call(ctx, &info, "getdeploymentinfo"); err != nil {
		return nil, mapError(err)
	}
	d, ok := info.Deployments[name]
	if !ok {
		return nil, fmt.Errorf("deployment %q: %w", name, chainview.ErrNotFound)
	}

	out := &chainview.Deployment{
		Name:                name,
		Type:                d.Type,
		Active:              d.Active,
		MinActivationHeight: d.Height,
	}
	if d.BIP9 != nil {
		out.Bit = d.BIP9.Bit
		out.StartTime = d.BIP9.StartTime
		out.Timeout = d.BIP9.Timeout
		out.MinActivationHeight = d.BIP9.MinActivationHeight
		out.MaxActivationHeight = d.BIP9.MaxActivationHeight
		out.Status = d.BIP9.Status
		out.Since = d.BIP9.Since
		if s := d.BIP9.Statistics; s != nil {
			out.Period = s.Period
			out.Elapsed = s.Elapsed
			out.Count = s.Count
			out.Threshold = s.Threshold
		}
	}
	return out, nil
}

// mapError translates a node's error codes into this package's sentinels, so
// callers can branch on meaning rather than on a number or a message.
func mapError(err error) error {
	e, ok := asRPCError(err)
	if !ok {
		return err
	}
	switch e.Code {
	case codeInvalidAddressOrKey, codeInvalidParameter:
		// An unknown hash, or a height past the tip. Both mean "not here", which
		// callers handle rather than treat as a fault.
		return fmt.Errorf("%s: %w", e.Message, chainview.ErrNotFound)
	case codeMethodNotFound:
		return fmt.Errorf("%s: %w", e.Message, chainview.ErrUnsupported)
	default:
		return err
	}
}

// mustHash parses a hash the node just gave us, yielding the zero hash if it is
// unreadable. Used only where a bad value degrades a health report rather than
// producing a wrong answer.
func mustHash(s string) chainhash.Hash {
	h, err := chainhash.NewHashFromStr(s)
	if err != nil {
		return chainhash.Hash{}
	}
	return *h
}

// firstLine trims an error to something short enough to show a user.
// firstLine trims an error to something short enough to show, and removes any
// credential in it on the way.
//
// **Every error string that becomes visible passes through here**, which is why
// the redaction belongs at this point rather than at each of the six call sites.
// A node's RPC address may carry its own credentials — `http://user:pass@host` is
// a perfectly natural way to write one — and Go's HTTP client echoes the request
// URL into its errors: `Post "http://forktower:hunter2@10.0.0.5:8332": dial tcp`.
// That string was reaching the dashboard, and from there any support bundle.
func firstLine(s string) string {
	s = redact.String(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const maxDetail = 200
	if len(s) > maxDetail {
		s = s[:maxDetail] + "…"
	}
	return s
}
