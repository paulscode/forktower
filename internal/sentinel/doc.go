// Package sentinel decides whether the two chain views have diverged.
//
// It owns the authoritative answer to "are we in a split", the point at which
// the chains separated, and per-branch telemetry. Pure decision logic is kept
// separate from the I/O that feeds it so the logic is testable without a
// network or a database.
package sentinel
