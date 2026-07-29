// Package registry keeps an inventory of the user's Lightning channels:
// funding outpoints, capacities, delay parameters, peers, and close state.
//
// The inventory is persisted, so watching continues even while the Lightning
// node is unreachable.
package registry
