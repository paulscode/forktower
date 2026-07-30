// Package bus is the in-process publish/subscribe channel between engines.
//
// Events are notifications, not the source of truth: a subscriber that misses
// one reconciles from storage instead.
package bus

import (
	"log/slog"
	"sync"
)

// Event is anything publishable. Implementations are immutable value types, so
// that fanning one out to several subscribers cannot let one of them mutate what
// the others see.
type Event interface {
	// Kind returns the event's stable type name, used for filtering and recorded
	// in the timeline.
	Kind() string
}

// SubscriberBuffer is how many events are held for a subscriber that has fallen
// behind.
//
// Generous for this workload: the busiest source is a new block, roughly every
// ten minutes. A subscriber that fills this is not briefly busy, it is stuck, and
// the point of the buffer is to make that visible rather than to paper over it.
const SubscriberBuffer = 256

// Bus fans events out to subscribers. Safe for concurrent use.
//
// The ordering guarantee is per subscriber, not global: each one sees the events
// it asked for in the order they were published. There is no guarantee that two
// subscribers process the same event at the same time, which is why nothing here
// is used for coordination — engines coordinate through the store.
type Bus struct {
	log *slog.Logger

	mu     sync.RWMutex
	subs   []*subscription
	closed bool
}

type subscription struct {
	name string
	// kinds is nil when the subscriber wants everything.
	kinds map[string]struct{}
	ch    chan Event
	// dropped counts events discarded because this subscriber was not keeping up.
	// Reported when the bus closes, so a slow consumer leaves a trace even if
	// nobody was reading the logs at the time.
	dropped int
}

func (s *subscription) wants(kind string) bool {
	if s.kinds == nil {
		return true
	}
	_, ok := s.kinds[kind]
	return ok
}

// New creates an empty bus. A nil logger is replaced with one that discards, so
// callers are not obliged to supply one.
func New(logger *slog.Logger) *Bus {
	if logger == nil {
		logger = slog.New(discardHandler{})
	}
	return &Bus{log: logger.With("component", "bus")}
}

// Subscribe returns a channel of events. With no kinds, the subscriber receives
// everything; otherwise only those listed.
//
// The name appears in diagnostics, and is the only way to tell which consumer is
// falling behind, so it should identify the engine rather than describe it.
//
// The channel is closed when the bus closes. Subscribing to a closed bus returns
// an already-closed channel rather than failing, so a late subscriber terminates
// promptly instead of waiting forever.
func (b *Bus) Subscribe(name string, kinds ...string) <-chan Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan Event, SubscriberBuffer)

	if b.closed {
		close(ch)
		return ch
	}

	var set map[string]struct{}
	if len(kinds) > 0 {
		set = make(map[string]struct{}, len(kinds))
		for _, k := range kinds {
			set[k] = struct{}{}
		}
	}

	b.subs = append(b.subs, &subscription{name: name, kinds: set, ch: ch})
	return ch
}

// Publish delivers an event to every interested subscriber.
//
// It never blocks. A subscriber whose buffer is full loses its oldest pending
// event to make room for this one, because the newest description of the world is
// the useful one — a stale "the chains agree" is worth less than a current "they
// have separated". Every drop is logged at error level: engines are required to
// tolerate a missed notification by reconciling from the store, so a drop is not
// data loss, but it does mean something is stuck and that is worth knowing.
//
// Publishing to a closed bus is a no-op rather than a panic. Shutdown is not
// perfectly ordered, and an engine finishing its last write while the bus closes
// should not bring the process down.
func (b *Bus) Publish(e Event) {
	if e == nil {
		return
	}
	kind := e.Kind()

	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return
	}

	for _, s := range b.subs {
		if !s.wants(kind) {
			continue
		}
		b.deliver(s, e, kind)
	}
}

// deliver sends to one subscriber, displacing its oldest pending event if
// necessary. Called with at least a read lock held.
func (b *Bus) deliver(s *subscription, e Event, kind string) {
	select {
	case s.ch <- e:
		return
	default:
	}

	// Full. Discard the oldest to make room. The receive may find nothing if the
	// consumer drained concurrently, which is fine — the send below then succeeds.
	var displaced Event
	select {
	case displaced = <-s.ch:
	default:
	}

	select {
	case s.ch <- e:
		s.dropped++
		attrs := []any{
			slog.String("subscriber", s.name),
			slog.String("kind", kind),
			slog.Int("dropped_total", s.dropped),
		}
		if displaced != nil {
			attrs = append(attrs, slog.String("displaced_kind", displaced.Kind()))
		}
		b.log.Error("subscriber is not keeping up; discarded its oldest pending event", attrs...)
	default:
		// The consumer refilled the buffer between the two operations above. Drop
		// the new event rather than looping, since looping could spin indefinitely
		// against a fast publisher.
		s.dropped++
		b.log.Error("subscriber is not keeping up; discarded this event",
			slog.String("subscriber", s.name),
			slog.String("kind", kind),
			slog.Int("dropped_total", s.dropped))
	}
}

// Close shuts the bus down and closes every subscriber channel, which is how
// consumers learn to stop. Safe to call more than once.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}
	b.closed = true

	for _, s := range b.subs {
		if s.dropped > 0 {
			b.log.Warn("subscriber lost events over its lifetime",
				slog.String("subscriber", s.name),
				slog.Int("dropped_total", s.dropped))
		}
		close(s.ch)
	}
	b.subs = nil
}

// Dropped reports how many events a named subscriber lost. For diagnostics and
// tests; zero for an unknown name.
func (b *Bus) Dropped(name string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, s := range b.subs {
		if s.name == name {
			return s.dropped
		}
	}
	return 0
}
