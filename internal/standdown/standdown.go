// Package standdown holds the one switch that turns watching off on purpose.
//
// Separate from every other reason watching stops, and that separation is the
// point. A view that has gone unreachable, a backend on the wrong chain, a block
// that cannot be read — those are faults, and the daemon says so and keeps
// trying. This is a person deciding, deliberately and reversibly, that they do
// not want the other chain watched any more: after a split has ended, when they
// are rebuilding a node, when they know something the software does not.
//
// It is persisted, because it is a decision rather than a condition. A daemon
// that quietly resumed watching after a restart would be overruling somebody who
// had said no.
package standdown

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/paulscode/forktower/internal/store"
)

// MetaKey is where the decision is kept.
const MetaKey = "watching_stood_down"

const (
	valueDown = "1"
	valueUp   = ""
)

// Switch is whether watching has been stood down.
//
// Held in memory and written through, because it is read on every block and
// changed about once a year.
type Switch struct {
	store *store.Store
	down  atomic.Bool
}

// New builds the switch and reads back whatever was decided last time.
func New(ctx context.Context, st *store.Store) (*Switch, error) {
	if st == nil {
		return nil, errors.New("standdown: a store is required")
	}
	s := &Switch{store: st}

	raw, err := st.GetMeta(ctx, MetaKey)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	s.down.Store(raw == valueDown)
	return s, nil
}

// Down reports whether watching has been stood down.
func (s *Switch) Down() bool { return s.down.Load() }

// Active is the same question asked the other way round, which is how the
// dashboard puts it.
func (s *Switch) Active() bool { return !s.down.Load() }

// Set records the decision, and only reports success once it is on disk.
//
// The order matters: written first, then believed. A switch that flipped in
// memory and failed to persist would resume watching at the next restart with
// nothing to say it had ever been turned off.
func (s *Switch) Set(ctx context.Context, down bool) error {
	value := valueUp
	if down {
		value = valueDown
	}
	if err := s.store.SetMeta(ctx, MetaKey, value); err != nil {
		return err
	}
	s.down.Store(down)
	return nil
}
