// Package responder acts on what the watcher finds: it orchestrates a
// watchtower with a view of the other chain, and rebroadcasts transactions
// that are valid on both chains so the user's claims exist wherever they can.
//
// It never constructs or signs a transaction.
package responder
