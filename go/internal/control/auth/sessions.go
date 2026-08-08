package auth

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

type session struct {
	username string
	expires  time.Time
}

type failures struct {
	count     int
	first     time.Time
	lockUntil time.Time
}

// maxThrottleEntries bounds the per-IP failure table so a source rotating addresses
// cannot grow it without limit.
const maxThrottleEntries = 16384

// Sessions is the process-local opaque session and login-throttle store. Restarting
// the process deliberately revokes every session.
type Sessions struct {
	mu          sync.Mutex
	ttl         time.Duration
	maxFailures int
	lockout     time.Duration
	// window is how long failures accumulate toward the threshold. Attempts spread
	// wider than this are not one guessing burst and must not add up to a lockout.
	window   time.Duration
	now      func() time.Time
	tokens   map[string]session
	failures map[string]failures
}

// NewSessions creates a session store.
func NewSessions(ttl time.Duration) *Sessions {
	return &Sessions{
		ttl: ttl, maxFailures: 5, lockout: 5 * time.Minute, window: 15 * time.Minute,
		now:    time.Now,
		tokens: make(map[string]session), failures: make(map[string]failures),
	}
}

// Create mints an opaque session.
func (s *Sessions) Create(username string) (string, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.tokens[token] = session{username: username, expires: s.now().Add(s.ttl)}
	s.mu.Unlock()
	return token, nil
}

// Resolve returns a live token's username.
func (s *Sessions) Resolve(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, found := s.tokens[token]
	if !found {
		return "", false
	}
	if !s.now().Before(entry.expires) {
		delete(s.tokens, token)
		return "", false
	}
	return entry.username, true
}

// Drop revokes a session.
func (s *Sessions) Drop(token string) {
	s.mu.Lock()
	delete(s.tokens, token)
	s.mu.Unlock()
}

// Blocked reports whether an IP is locked out. An elapsed lockout is forgotten here,
// so the address starts its next window with a full allowance.
func (s *Sessions) Blocked(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, found := s.failures[ip]
	if !found {
		return false
	}
	if entry.lockUntil.IsZero() || s.now().Before(entry.lockUntil) {
		return !entry.lockUntil.IsZero()
	}
	// The lockout has expired. Without clearing the counter it stays at the threshold
	// forever, so a single later typo re-locks the address for another full period —
	// which for a shared NAT egress address is an outage, not a defence.
	delete(s.failures, ip)
	return false
}

// RecordFailure increments an IP's failure counter.
func (s *Sessions) RecordFailure(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	entry, found := s.failures[ip]
	switch {
	case !found:
		entry = failures{}
	case !entry.lockUntil.IsZero() && !now.Before(entry.lockUntil):
		// First failure after a served lockout: start counting again from zero.
		entry = failures{}
	case entry.lockUntil.IsZero() && !entry.first.IsZero() && now.Sub(entry.first) > s.window:
		// The attempts are too spread out to be one burst; start a fresh window.
		entry = failures{}
	}
	if entry.first.IsZero() {
		entry.first = now
	}
	entry.count++
	if entry.count >= s.maxFailures {
		entry.lockUntil = now.Add(s.lockout)
	}
	s.failures[ip] = entry
	s.pruneFailuresLocked(now)
}

// pruneFailuresLocked bounds the throttle table. Without it the map grows once per
// distinct source address seen, which an attacker with an IPv6 range controls for free.
func (s *Sessions) pruneFailuresLocked(now time.Time) {
	if len(s.failures) < maxThrottleEntries {
		return
	}
	for ip, entry := range s.failures {
		expired := !entry.lockUntil.IsZero() && !now.Before(entry.lockUntil)
		stale := entry.lockUntil.IsZero() && now.Sub(entry.first) > s.window
		if expired || stale {
			delete(s.failures, ip)
		}
	}
	// Still full: the table is all live lockouts, so drop the one closest to expiry.
	if len(s.failures) >= maxThrottleEntries {
		var oldest string
		var at time.Time
		for ip, entry := range s.failures {
			if oldest == "" || entry.lockUntil.Before(at) {
				oldest, at = ip, entry.lockUntil
			}
		}
		delete(s.failures, oldest)
	}
}

// ClearFailures resets an IP after successful authentication.
func (s *Sessions) ClearFailures(ip string) {
	s.mu.Lock()
	delete(s.failures, ip)
	s.mu.Unlock()
}

func randomToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
