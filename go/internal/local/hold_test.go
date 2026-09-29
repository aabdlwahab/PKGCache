package local

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A held daemon hears from the command at every interval, on a path that counts as
// activity, and stops hearing from it once released.
func TestHoldDaemonKeepsTheDaemonBusyUntilReleased(t *testing.T) {
	previous := holdInterval
	holdInterval = 20 * time.Millisecond
	t.Cleanup(func() { holdInterval = previous })

	var calls atomic.Int64
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/me" {
			calls.Add(1)
		}
	}))
	t.Cleanup(daemon.Close)

	release := HoldDaemon(context.Background(), daemon.URL+"/")
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() < 3 {
		t.Fatalf("the daemon was reminded %d times", calls.Load())
	}
	release()
	time.Sleep(50 * time.Millisecond)
	after := calls.Load()
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != after {
		t.Fatal("the daemon kept being held after release")
	}
}
