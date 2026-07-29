// Package watcher detects spends of watched outpoints on a chain.
//
// It scans blocks as they arrive, rescans historical ranges on demand, handles
// chain reorganisations, and classifies what it finds as far as is possible
// without channel keys.
package watcher
