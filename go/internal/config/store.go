package config

import (
	"maps"
	"sync"
	"sync/atomic"
)

// Store publishes the current configuration.
//
// Readers call Current() on every request: an atomic pointer load, no lock, no
// contention. Writers build a complete replacement Snapshot and swap it in one
// operation, so a request can never observe a half-applied change — for example a
// project that exists in the routing table but not yet in the quota table.
//
// This is what makes a project creation take effect on the *next request* instead of
// within a five-second poll interval, and it is why nothing needs to re-read a file
// on the hot path.
type Store struct {
	cur atomic.Pointer[Snapshot]

	// Mutations serialise so two concurrent changes cannot lose one another's work
	// through a read-modify-write race.
	mu        sync.Mutex
	observers []func(*Snapshot)
}

// NewStore publishes an initial snapshot. It takes ownership: do not mutate s after
// this call.
func NewStore(s *Snapshot) *Store {
	st := &Store{}
	st.cur.Store(s)
	return st
}

// Current returns the live configuration. Treat the result as read-only.
func (s *Store) Current() *Snapshot { return s.cur.Load() }

// Observe registers a callback fired after each successful change, on the caller's
// goroutine. Used by subsystems that must react to a change rather than poll for it
// (rebinding a listener, reopening a certificate).
//
// Callbacks must not block and must not call back into Apply.
func (s *Store) Observe(fn func(*Snapshot)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observers = append(s.observers, fn)
}

// Apply mutates a copy of the current snapshot and publishes it atomically. If the
// mutation produces an invalid configuration, nothing is published and the error is
// returned — a bad write leaves the process serving its last good configuration
// rather than falling over.
func (s *Store) Apply(mutate func(*Snapshot) error) error {
	s.mu.Lock()
	next := s.cur.Load().clone()
	if err := mutate(next); err != nil {
		s.mu.Unlock()
		return err
	}
	if err := next.Validate(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.cur.Store(next)
	observers := make([]func(*Snapshot), len(s.observers))
	copy(observers, s.observers)
	s.mu.Unlock()

	for _, fn := range observers {
		fn(next)
	}
	return nil
}

// SetProjects replaces the project set wholesale. The control plane calls this after
// writing control.db, so the database stays the source of truth and the snapshot is
// its published projection.
func (s *Store) SetProjects(projects map[string]Project) error {
	return s.Apply(func(next *Snapshot) error {
		next.Projects = maps.Clone(projects)
		if next.Projects == nil {
			next.Projects = map[string]Project{}
		}
		return nil
	})
}

// SetControl publishes the complete control-plane projection in one atomic swap.
// Projects and their upstream overrides must never become visible separately.
func (s *Store) SetControl(
	projects map[string]Project,
	upstreams map[string]map[string]map[string][]Endpoint,
	peers map[string]map[string][]Peer,
) error {
	return s.Apply(func(next *Snapshot) error {
		next.Projects = maps.Clone(projects)
		if next.Projects == nil {
			next.Projects = map[string]Project{}
		}
		next.ProjectUpstreams = cloneUpstreams(upstreams)
		next.ProjectPeers = clonePeers(peers)
		return nil
	})
}

// clone deep-copies the parts of a Snapshot that Apply may mutate. Everything else is
// value-typed and copies with the struct.
func (s *Snapshot) clone() *Snapshot {
	next := *s
	next.Projects = maps.Clone(s.Projects)
	if next.Projects == nil {
		next.Projects = map[string]Project{}
	}
	if s.Server.ProxyAllowlist != nil {
		next.Server.ProxyAllowlist = append([]string(nil), s.Server.ProxyAllowlist...)
	}
	next.ProjectUpstreams = cloneUpstreams(s.ProjectUpstreams)
	next.ProjectPeers = clonePeers(s.ProjectPeers)
	if s.Auth.CookieSecure != nil {
		v := *s.Auth.CookieSecure
		next.Auth.CookieSecure = &v
	}
	return &next
}

func clonePeers(source map[string]map[string][]Peer) map[string]map[string][]Peer {
	if source == nil {
		return map[string]map[string][]Peer{}
	}
	out := make(map[string]map[string][]Peer, len(source))
	for project, ecosystems := range source {
		ecoCopy := make(map[string][]Peer, len(ecosystems))
		for ecosystem, peers := range ecosystems {
			ecoCopy[ecosystem] = append([]Peer(nil), peers...)
		}
		out[project] = ecoCopy
	}
	return out
}

func cloneUpstreams(
	source map[string]map[string]map[string][]Endpoint,
) map[string]map[string]map[string][]Endpoint {
	if source == nil {
		return map[string]map[string]map[string][]Endpoint{}
	}
	out := make(map[string]map[string]map[string][]Endpoint, len(source))
	for project, ecosystems := range source {
		ecoCopy := make(map[string]map[string][]Endpoint, len(ecosystems))
		for ecosystem, names := range ecosystems {
			nameCopy := make(map[string][]Endpoint, len(names))
			for name, chain := range names {
				nameCopy[name] = append([]Endpoint(nil), chain...)
			}
			ecoCopy[ecosystem] = nameCopy
		}
		out[project] = ecoCopy
	}
	return out
}
