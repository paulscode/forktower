package bootstrap

import "fmt"

// Part is one downloadable piece of a snapshot.
//
// The file is split because a GitHub release asset may not exceed two gibibytes,
// and the snapshot is nearly nine gigabytes. Parts concatenate in name order to
// reproduce the original byte for byte; there is no container format and no
// framing, so a plain `cat *.part` is equivalent to what this package does.
type Part struct {
	// Name is the asset's filename, appended to the snapshot's BaseURL.
	Name string
	// Bytes is its exact length. Known in advance so that progress can be
	// reported as a fraction rather than as a rising number with no end, and so
	// that a truncated response is caught before its hash is computed.
	Bytes int64
	// SHA256 is the hex digest of this part alone.
	SHA256 string
}

// Snapshot is a published UTXO set, described completely enough to fetch,
// reassemble and check it without asking anybody anything.
//
// **Every field here is compiled in.** Nothing about this description is
// downloaded, because a manifest fetched from the host serving the files
// describes whatever that host wants it to describe.
type Snapshot struct {
	// Network is the chain this belongs to, as a Bitcoin node names it. Checked
	// against the node rather than assumed: loading a mainnet UTXO set into a
	// node on another network is a mistake worth refusing early, and the error
	// Core gives for it is not one a user could act on.
	Network string

	// BaseHeight and BaseHash identify the block whose UTXO set this is.
	//
	// BaseHash is the load-bearing value, and it is not load-bearing here — it is
	// compiled into *Bitcoin Core*, which is what makes the whole arrangement
	// safe. It is carried in this struct so the interface can say which block is
	// being adopted, and so a mismatch can be reported in the daemon's own words
	// rather than as a node error nobody can parse.
	BaseHeight int32
	BaseHash   string

	// Coins is how many unspent outputs the set contains. Displayed, not
	// enforced.
	Coins uint64

	// BaseURL is where the parts live, including the trailing slash.
	BaseURL string

	// Parts are the pieces, in concatenation order.
	Parts []Part

	// SHA256 is the digest of the whole reassembled file. Checked after the last
	// part, which is redundant against the per-part digests and cheap, and which
	// catches the one thing they cannot: parts that are individually intact and
	// assembled in the wrong order.
	SHA256 string
}

// TotalBytes is the assembled file's size.
func (s Snapshot) TotalBytes() int64 {
	var total int64
	for _, p := range s.Parts {
		total += p.Bytes
	}
	return total
}

// BytesBefore is the offset at which part i begins in the assembled file.
func (s Snapshot) BytesBefore(i int) int64 {
	var total int64
	for n := 0; n < i && n < len(s.Parts); n++ {
		total += s.Parts[n].Bytes
	}
	return total
}

// PartAt reports which part covers a byte offset into the assembled file, and how
// far into that part the offset lies.
//
// This is what makes an interrupted download resumable: the only state that has
// to survive a restart is the length of the file on disk, and everything else is
// derived from it. A separate progress record could disagree with the file — this
// cannot.
func (s Snapshot) PartAt(offset int64) (index int, within int64, ok bool) {
	if offset < 0 {
		return 0, 0, false
	}
	for i, p := range s.Parts {
		if offset < p.Bytes {
			return i, offset, true
		}
		offset -= p.Bytes
	}
	// Exactly at the end is complete rather than out of range, and the caller
	// distinguishes the two by the offset it passed in.
	return len(s.Parts), 0, offset == 0
}

// URLFor is where to fetch a part from.
func (s Snapshot) URLFor(p Part) string {
	return s.BaseURL + p.Name
}

// Describe is the one-line summary shown to a user deciding whether to run this.
func (s Snapshot) Describe() string {
	return fmt.Sprintf("%s UTXO set at block %s, %s",
		s.Network, Commas(int64(s.BaseHeight)), HumanBytes(s.TotalBytes()))
}

// MainnetHeight935000 is the snapshot Forktower publishes.
//
// # Why this one, and why it will not need to move often
//
// The height is not ours to choose. Bitcoin Core only accepts a snapshot at a
// height whose UTXO hash it has hardcoded, so the set of valid snapshots is
// exactly the set Core ships — this tracks Core's release schedule and nothing
// else. When the pinned Core version changes its assumeutxo height, this becomes
// unusable and the plan below refuses it rather than wasting somebody's evening
// discovering that.
//
// # Why a node enforcing the new rules could produce it
//
// It was made with `dumptxoutset` on a fully synced Bitcoin Knots node, which
// enforces the new rules — and Bitcoin Core, which does not, accepts the result.
// Height 935,000 predates the point at which the two rule sets can produce
// different blocks, so the UTXO set there is shared history and both
// implementations serialise exactly the same bytes. That was verified rather than
// reasoned about: Core loaded this file and reported the base hash below.
var MainnetHeight935000 = Snapshot{
	Network:    ChainMain,
	BaseHeight: 935_000,
	BaseHash:   "0000000000000000000147034958af1652b2b91bba607beacc5e72a56f0fb5ee",
	Coins:      164_241_311,
	BaseURL: "https://github.com/paulscode/forktower/releases/download/" +
		"utxo-snapshot-935000/",
	SHA256: "e572ddbe456d254f05fb004cebe225bdb3656074b66f0e9b1c7fa83e1301d486",
	Parts: []Part{
		{
			Name:   "bitcoin-mainnet-utxo-935000.dat.00.part",
			Bytes:  1_992_294_400,
			SHA256: "2c2210f97eb6e197963058a4149e81feee3df0cae39ae213d5073e60d9569ad1",
		},
		{
			Name:   "bitcoin-mainnet-utxo-935000.dat.01.part",
			Bytes:  1_992_294_400,
			SHA256: "962645675ec2d66d4d12b0293ae6e8bac239bb6dcc2cd5a4e565fcbe59bf3691",
		},
		{
			Name:   "bitcoin-mainnet-utxo-935000.dat.02.part",
			Bytes:  1_992_294_400,
			SHA256: "26235c82b7778db0db867e7fc7aec999c5af37c2e8e8bd59e553bbe1ac98a7f3",
		},
		{
			Name:   "bitcoin-mainnet-utxo-935000.dat.03.part",
			Bytes:  1_992_294_400,
			SHA256: "c53c5cfa91db5ebd37d594783e156bb83dda3489a2bec75f0a11f6bfbeb6140a",
		},
		{
			Name:   "bitcoin-mainnet-utxo-935000.dat.04.part",
			Bytes:  1_418_812_706,
			SHA256: "52522d818b699b9ab4aab25c1cb52208f4ceb1e0c3db8ea704565403d8c710ac",
		},
	},
}

// StagedFileName is what the reassembled snapshot is called on disk. Named for
// its contents rather than generically, so that somebody who finds nine gigabytes
// in their data directory after a crash can tell what it is and delete it.
const StagedFileName = "utxo-snapshot.dat"
