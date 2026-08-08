package obs

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func TestBusFanOut(t *testing.T) {
	b := NewBus()
	subs := make([]*Subscription, 5)
	for i := range subs {
		subs[i] = b.Subscribe(16)
		defer subs[i].Close()
	}
	b.Publish(Event{Kind: EventCacheHit, ID: "k"})
	for i, s := range subs {
		select {
		case e := <-s.C:
			if e.Kind != EventCacheHit || e.ID != "k" {
				t.Fatalf("sub %d: got %+v", i, e)
			}
			if e.Time.IsZero() {
				t.Fatalf("sub %d: Publish must stamp Time", i)
			}
		default:
			t.Fatalf("sub %d received nothing", i)
		}
	}
}

func TestBusFiltersByKind(t *testing.T) {
	b := NewBus()
	s := b.Subscribe(4, EventJobUpdate)
	defer s.Close()

	b.Publish(Event{Kind: EventFetchProgress})
	b.Publish(Event{Kind: EventJobUpdate, ID: "job-1"})

	e := <-s.C
	if e.Kind != EventJobUpdate {
		t.Fatalf("filter leaked %s", e.Kind)
	}
	select {
	case extra := <-s.C:
		t.Fatalf("unexpected second event %+v", extra)
	default:
	}
}

// Publish must never block on a subscriber that is not draining — a stalled SSE
// client must not be able to slow down a download.
func TestBusDropsInsteadOfBlocking(t *testing.T) {
	b := NewBus()
	s := b.Subscribe(2)
	defer s.Close()

	for range 100 {
		b.Publish(Event{Kind: EventFetchProgress})
	}
	if got := s.Dropped(); got != 98 {
		t.Fatalf("dropped = %d, want 98", got)
	}
}

func TestSubscriptionCloseIsIdempotent(t *testing.T) {
	b := NewBus()
	s := b.Subscribe(1)
	s.Close()
	s.Close() // must not panic on a double close of the channel
	if b.Subscribers() != 0 {
		t.Fatalf("subscriber not removed")
	}
}

func TestBusConcurrentPublishAndSubscribe(t *testing.T) {
	b := NewBus()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := b.Subscribe(8)
			defer s.Close()
			for range 100 {
				b.Publish(Event{Kind: EventFetchProgress})
			}
		}()
	}
	wg.Wait()
	if b.Subscribers() != 0 {
		t.Fatalf("leaked subscribers: %d", b.Subscribers())
	}
}

func TestLoggerRedactsSecrets(t *testing.T) {
	var buf bytes.Buffer
	lg := NewLogger(LogOptions{Output: &buf, Level: slog.LevelInfo})
	lg.Info("req",
		"authorization", "Bearer supersecret",
		"Cookie", "session=abc",
		"x-auth-token", "tok",
		"project", "global",
	)

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	if strings.Contains(buf.String(), "supersecret") || strings.Contains(buf.String(), "session=abc") {
		t.Fatalf("secret leaked into log: %s", buf.String())
	}
	for _, k := range []string{"authorization", "Cookie", "x-auth-token"} {
		if got[k] != "[redacted]" {
			t.Fatalf("%s = %v, want [redacted]", k, got[k])
		}
	}
	if got["project"] != "global" {
		t.Fatalf("non-secret field was mangled: %v", got["project"])
	}
}
