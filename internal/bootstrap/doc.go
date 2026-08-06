// Package bootstrap gets the second Bitcoin node to a usable state in under an
// hour instead of in days.
//
// The second node starts from nothing. On the hardware this ships to, a full
// initial block download of the status-quo chain has been measured at around
// three days — and for most of that time Forktower is installed, running, and
// unable to see the chain it exists to watch. A user who installs this because a
// split is imminent does not have three days.
//
// Bitcoin Core's assumeutxo mechanism removes almost all of that. A serialised
// UTXO set at a fixed height is handed to `loadtxoutset`; the node adopts that
// state immediately, syncs the remaining blocks to the tip, and validates
// everything below the snapshot in the background while already answering
// questions. Measured end to end on a fast machine: 48 minutes rather than three
// days.
//
// # Why downloading this is not the security hole it looks like
//
// The snapshot's base block hash is compiled into Bitcoin Core itself. When the
// file is loaded, Core recomputes the hash of the UTXO set it just read and
// compares it against its own hardcoded value. A corrupted download, a botched
// reassembly and a deliberately altered file all produce the same outcome:
// `loadtxoutset` refuses it. Whoever hosts the file cannot make the node accept
// a state Core does not already agree with.
//
// The per-part checksums in this package are therefore a convenience rather than
// the safeguard — they catch a bad part after two gigabytes instead of after
// nine. They are compiled in rather than fetched, because a checksum retrieved
// from the same host as the file it vouches for is not a check.
//
// # Why it is off by default
//
// Forktower otherwise fetches nothing, and that is a property worth keeping
// deliberate rather than eroding. This is the one code path that reaches out to
// something other than the user's own machine, so it is opt-in, it says what it
// costs before it runs, and it goes through the same Tor proxy the second node
// uses for its peering — because a clearnet request for this specific file, from
// a residential address, announces that whoever lives there runs Lightning
// channels they are defending across a split.
package bootstrap
