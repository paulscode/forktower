// Package bus is the in-process publish/subscribe channel between engines.
//
// Events are notifications, not the source of truth: a subscriber that misses
// one reconciles from storage instead.
package bus
