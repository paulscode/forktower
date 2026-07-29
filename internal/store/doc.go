// Package store is the only place SQL lives. Callers use typed methods.
//
// Recording is idempotent: replaying the same block or the same event produces
// no duplicate rows, which is what makes crash recovery safe.
package store
