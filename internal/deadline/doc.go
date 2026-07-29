// Package deadline computes how long the user has to respond to a channel
// close, and escalates as that window shrinks.
//
// All arithmetic is in block heights; wall-clock figures are projections from
// observed block cadence and are always presented as estimates.
package deadline
