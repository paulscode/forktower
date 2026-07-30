package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Node names, as they appear in the compose file and on the command line.
const (
	nodeSF = "sf"
	nodeSQ = "sq"
)

// walletName is the wallet each node mines into. Modern Core does not create one
// on its own, and a node with no wallet cannot produce an address to mine to —
// which is the first thing anyone hits building a world like this.
const walletName = "forkbench"

// node is one Bitcoin node in the world.
type node struct {
	name    string
	service string
	rpcURL  string
	// p2p is how the other node reaches this one, from inside the compose
	// network. Container name, not localhost: the nodes talk to each other
	// directly, not through the host.
	p2p string
}

func nodes() []node {
	return []node{
		{name: nodeSF, service: "sf-node", rpcURL: "http://127.0.0.1:18443", p2p: "sf-node:18445"},
		{name: nodeSQ, service: "sq-node", rpcURL: "http://127.0.0.1:18444", p2p: "sq-node:18445"},
	}
}

func nodeByName(name string) (node, error) {
	for _, n := range nodes() {
		if n.name == name {
			return n, nil
		}
	}
	return node{}, fmt.Errorf("no node called %q; use %q or %q", name, nodeSF, nodeSQ)
}

// The credentials in the compose file. Hard-coded on purpose: this is a
// throwaway regtest world with no money in it, and a world that needs secrets
// managed before it will start is a world nobody uses.
const (
	rpcUser = "forkbench"
	rpcPass = "forkbench"
)

// rpcError is what the node said when it refused.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// Error codes this tool reacts to rather than merely reports.
const (
	codeWalletExists   = -4
	codeWalletNotFound = -18
	codeBlockNotFound  = -5
)

// client talks to one node.
//
// Deliberately its own small client rather than the daemon's chain backend: this
// tool exists to test that backend, and a tool sharing its code would share its
// mistakes. It also needs calls the backend has no business exposing —
// invalidateblock, setban, generatetoaddress.
type client struct {
	url  string
	http *http.Client
}

func newClient(url string) *client {
	return &client{url: url, http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *client) call(ctx context.Context, method string, params []any, out any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "1.0", "id": "forkbench", "method": method, "params": params,
	})
	if err != nil {
		return fmt.Errorf("building the %s request: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building the %s request: %w", method, err)
	}
	req.SetBasicAuth(rpcUser, rpcPass)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("reading the answer to %s: %w", method, err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("%s: %w", method, envelope.Error)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return fmt.Errorf("reading the answer to %s: %w", method, err)
	}
	return nil
}

// wallet returns a client scoped to the mining wallet.
func (c *client) wallet() *client {
	return &client{url: c.url + "/wallet/" + walletName, http: c.http}
}

// chainInfo is the part of getblockchaininfo this tool uses.
type chainInfo struct {
	Chain  string `json:"chain"`
	Blocks int32  `json:"blocks"`
	Best   string `json:"bestblockhash"`
}

func (c *client) chainInfo(ctx context.Context) (chainInfo, error) {
	var info chainInfo
	err := c.call(ctx, "getblockchaininfo", nil, &info)
	return info, err
}

// chainTip is one branch the node knows about.
type chainTip struct {
	Height    int32  `json:"height"`
	Hash      string `json:"hash"`
	BranchLen int32  `json:"branchlen"`
	Status    string `json:"status"`
}

func (c *client) chainTips(ctx context.Context) ([]chainTip, error) {
	var tips []chainTip
	err := c.call(ctx, "getchaintips", nil, &tips)
	return tips, err
}

// ready reports whether the node is answering yet.
func (c *client) ready(ctx context.Context) bool {
	_, err := c.chainInfo(ctx)
	return err == nil
}

// ensureWallet creates the mining wallet, or loads it if it is already there.
//
// Both outcomes are success. A world you can bring up twice is worth more than
// one that refuses the second time, and every command here is written to be run
// again.
func (c *client) ensureWallet(ctx context.Context) error {
	err := c.call(ctx, "createwallet", []any{walletName}, nil)
	if err == nil {
		return nil
	}

	var rpcErr *rpcError
	if errors.As(err, &rpcErr) && rpcErr.Code == codeWalletExists {
		loadErr := c.call(ctx, "loadwallet", []any{walletName}, nil)
		if loadErr == nil {
			return nil
		}
		// Already loaded is also fine.
		var loadRPCErr *rpcError
		if errors.As(loadErr, &loadRPCErr) && loadRPCErr.Code == codeWalletNotFound {
			return nil
		}
		if isAlreadyLoaded(loadErr) {
			return nil
		}
		return loadErr
	}
	return err
}

func isAlreadyLoaded(err error) bool {
	var rpcErr *rpcError
	return errors.As(err, &rpcErr) &&
		(rpcErr.Code == -35 || rpcErr.Code == -4)
}

// mine adds blocks to this node's chain.
func (c *client) mine(ctx context.Context, blocks int) ([]string, error) {
	w := c.wallet()
	if err := w.ensureWallet(ctx); err != nil {
		return nil, fmt.Errorf("preparing the wallet: %w", err)
	}

	var address string
	if err := w.call(ctx, "getnewaddress", nil, &address); err != nil {
		return nil, fmt.Errorf("getting an address to mine to: %w", err)
	}

	var hashes []string
	if err := w.call(ctx, "generatetoaddress", []any{blocks, address}, &hashes); err != nil {
		return nil, fmt.Errorf("mining: %w", err)
	}
	return hashes, nil
}

// compose runs a docker compose command against the forkbench world.
func compose(ctx context.Context, args ...string) error {
	file, err := composeFile()
	if err != nil {
		return err
	}
	full := append([]string{"compose", "-f", file}, args...)

	// Fixed command, and the only variable among the arguments is the compose
	// file this tool located itself.
	//nolint:gosec // G204: the arguments are this tool's own
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose %s: %w", args[0], err)
	}
	return nil
}

// composeFile finds the world's definition.
//
// Looked up relative to the working directory rather than compiled in, so the
// tool works from a checkout without an install step — which is the only way
// anyone uses it.
func composeFile() (string, error) {
	candidates := []string{
		filepath.Join("deploy", "forkbench", "docker-compose.yml"),
		filepath.Join("..", "deploy", "forkbench", "docker-compose.yml"),
		filepath.Join("..", "..", "deploy", "forkbench", "docker-compose.yml"),
	}
	if fromEnv := os.Getenv("FORKBENCH_COMPOSE"); fromEnv != "" {
		candidates = append([]string{fromEnv}, candidates...)
	}

	for _, path := range candidates {
		// Reading a path an operator supplied is the whole point of the override.
		//nolint:gosec // G703: the path is an operator-supplied override, by design
		if _, err := os.Stat(path); err == nil {
			return filepath.Abs(path)
		}
	}
	return "", errors.New(
		"cannot find deploy/forkbench/docker-compose.yml — run forkbench from the " +
			"repository, or set FORKBENCH_COMPOSE to its path")
}

// waitReady polls both nodes until they answer, or gives up.
//
// Polled rather than slept: a fixed wait is either too short on a loaded machine
// or wasted time on a fast one, and both make a test suite worse.
func waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for _, n := range nodes() {
		c := newClient(n.rpcURL)
		for !c.ready(ctx) {
			if time.Now().After(deadline) {
				return fmt.Errorf("%s did not start answering within %s", n.name, timeout)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	return nil
}

// waitForBlock waits until a node has a specific block, which is how this tool
// knows one has propagated rather than merely been mined.
//
// By hash, never by height: two chains reaching the same height while disagreeing
// is exactly what this world produces, so a height comparison would report
// success for the situation it was meant to rule out.
func waitForBlock(ctx context.Context, c *client, hash string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var header struct {
			Confirmations int32 `json:"confirmations"`
		}
		err := c.call(ctx, "getblockheader", []any{hash}, &header)
		// Negative confirmations mean the node has the block but has decided
		// against it, which still counts as having seen it.
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the node did not see block %s within %s", short(hash), timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// waitForAgreement waits until both nodes have the same best block, and reports
// its height.
func waitForAgreement(
	ctx context.Context, a, b *client, timeout time.Duration,
) (int32, error) {
	deadline := time.Now().Add(timeout)
	for {
		aInfo, aErr := a.chainInfo(ctx)
		bInfo, bErr := b.chainInfo(ctx)
		if aErr == nil && bErr == nil && aInfo.Best == bInfo.Best {
			return aInfo.Blocks, nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf(
				"the nodes were still on different chains after %s (heights %d and %d)",
				timeout, aInfo.Blocks, bInfo.Blocks)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
