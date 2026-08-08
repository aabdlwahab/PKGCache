package obs

import (
	"sync"
	"sync/atomic"
	"time"
)

// EventKind identifies what happened. Subscribers filter on it.
type EventKind string

// The event vocabulary. Progress events are high-frequency and lossy by design;
// job and audit events are low-frequency and matter individually.
const (
	EventFetchStart    EventKind = "fetch.start"
	EventFetchProgress EventKind = "fetch.progress"
	EventFetchDone     EventKind = "fetch.done"
	EventFetchError    EventKind = "fetch.error"
	EventCacheHit      EventKind = "cache.hit"
	EventJobUpdate     EventKind = "job.update"
	EventHealth        EventKind = "health"
	EventAudit         EventKind = "audit"
)

// Event is one thing that happened. It is deliberately flat and allocation-cheap:
// the fetch path emits one per chunk written, so this must not become a map.
type Event struct {
	Kind    EventKind `json:"kind"`
	Time    time.Time `json:"time"`
	Project string    `json:"project,omitempty"`
	Eco     string    `json:"eco,omitempty"`
	ID      string    `json:"id,omitempty"`   // cache key, job id, …
	Name    string    `json:"name,omitempty"` // human label
	Size    int64     `json:"size,omitempty"`
	Total   int64     `json:"total,omitempty"` // -1 when unknown
	Status  string    `json:"status,omitempty"`
	Detail  string    `json:"detail,omitempty"`
}

// Subscription is a consumer's view of the bus. Close it exactly once.
type Subscription struct {
	C       <-chan Event
	ch      chan Event
	bus     *Bus
	kinds   map[EventKind]bool
	dropped atomic.Uint64
	closed  atomic.Bool
}

// Dropped counts events discarded because this subscriber was not keeping up.
// Non-zero is informational for progress, and a bug for job or audit consumers.
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// Close unsubscribes. Safe to call more than once.
func (s *Subscription) Close() {
	if s.closed.Swap(true) {
		return
	}
	s.bus.remove(s)
	close(s.ch)
}

// Bus is an in-process publish/subscribe fan-out.
//
// Publish never blocks. A subscriber whose buffer is full has the event dropped and
// counted, because the alternative — back-pressuring the publisher — would let a
// stalled SSE client slow down every download in the process. Progress is a lossy
// signal by nature: the next chunk carries a fresher number anyway.
type Bus struct {
	mu   sync.RWMutex
	subs map[*Subscription]struct{}
}

// NewBus returns an empty bus.
func NewBus() *Bus { return &Bus{subs: make(map[*Subscription]struct{})} }

// Subscribe returns a subscription delivering the given kinds (all kinds when none
// are named). buffer sizes the per-subscriber queue.
func (b *Bus) Subscribe(buffer int, kinds ...EventKind) *Subscription {
	if buffer <= 0 {
		buffer = 256
	}
	s := &Subscription{ch: make(chan Event, buffer), bus: b}
	s.C = s.ch
	if len(kinds) > 0 {
		s.kinds = make(map[EventKind]bool, len(kinds))
		for _, k := range kinds {
			s.kinds[k] = true
		}
	}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

// Publish delivers e to every interested subscriber. Never blocks.
func (b *Bus) Publish(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		if s.kinds != nil && !s.kinds[e.Kind] {
			continue
		}
		select {
		case s.ch <- e:
		default:
			s.dropped.Add(1)
		}
	}
}

// Subscribers reports the current subscriber count (tests and /metrics).
func (b *Bus) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

func (b *Bus) remove(s *Subscription) {
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
}
