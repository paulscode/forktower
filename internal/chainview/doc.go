// Package chainview is the interface every chain backend implements, plus the
// types it exchanges.
//
// Backends supply blocks and match hints only. Outpoint matching, reorg
// bookkeeping and scan progress live above this layer so that they are
// implemented once, independently of which backend is in use.
package chainview
