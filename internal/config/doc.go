// Package config loads and validates the daemon's configuration.
//
// Validation collects every problem and reports them together, rather than
// stopping at the first, because a user fixing a config file should learn about
// all of it in one pass.
package config
