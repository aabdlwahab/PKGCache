package upstream

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
)

func idlePool(t *testing.T, idle time.Duration) *Pool {
	t.Helper()
	return mustPool(t, config.Upstream{
		RequestTimeout: 10 * time.Second, ConnectTimeout: 2 * time.Second,
		ResponseHeaderTimeout: 2 * time.Second, BodyIdleTimeout: idle,
	})
}

// origin serves a body in pieces, each after its delay. Pieces that add up to less than
// total leave the response open until the client goes away — the origin that stops
// sending without closing anything.
func origin(t *testing.T, total int, pieces []piece) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		sent := 0
		for _, p := range pieces {
			select {
			case <-time.After(p.after):
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write([]byte(strings.Repeat("x", p.size)))
			w.(http.Flusher).Flush()
			sent += p.size
		}
		if sent < total {
			<-r.Context().Done()
		}
	}))
	t.Cleanup(server.Close)
	return server
}

type piece struct {
	after time.Duration
	size  int
}

// A body that stops arriving is given up on after the idle timeout, not after the whole
// request's, and says why.
func TestABodyThatStopsArrivingIsAbandoned(t *testing.T) {
	server := origin(t, 1<<20, []piece{{0, 1024}}) // 1 KB of a declared 1 MB, then nothing
	response, err := open(t, idlePool(t, 200*time.Millisecond), Request{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()

	started := time.Now()
	read, err := io.Copy(io.Discard, response.Body)
	if !errors.Is(err, ErrBodyIdle) {
		t.Fatalf("read %d bytes and then %v, want ErrBodyIdle", read, err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("gave up after %s: the idle timeout did not end the request", elapsed)
	}
	if read != 1024 {
		t.Fatalf("read %d bytes before giving up, want the 1024 that arrived", read)
	}
}

// A slow body that keeps arriving is never mistaken for one that stopped.
func TestABodyStillArrivingIsNotCut(t *testing.T) {
	pieces := make([]piece, 20)
	for i := range pieces {
		pieces[i] = piece{100 * time.Millisecond, 1024} // two seconds, well past the idle
	}
	server := origin(t, 20*1024, pieces)
	response, err := open(t, idlePool(t, 400*time.Millisecond), Request{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(readAll(t, response)); got != 20*1024 {
		t.Fatalf("read %d bytes, want %d", got, 20*1024)
	}
}

// The clock is the origin's silence, not the caller's: a relay whose client paused reads
// nothing for a while, and the origin has done nothing wrong.
func TestIdleClockRunsOnlyWhileWaitingOnTheOrigin(t *testing.T) {
	// The rest arrives at one second, after the caller's pause and 100 ms into its next
	// Read — well inside the idle timeout for that Read, far outside it since the first.
	server := origin(t, 2048, []piece{{0, 1024}, {time.Second, 1024}})
	response, err := open(t, idlePool(t, 300*time.Millisecond), Request{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()

	first := make([]byte, 1024)
	if _, err := io.ReadFull(response.Body, first); err != nil {
		t.Fatal(err)
	}
	time.Sleep(900 * time.Millisecond) // the caller is busy elsewhere
	rest, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("the caller's own pause was counted against the origin: %v", err)
	}
	if len(rest) != 1024 {
		t.Fatalf("read %d more bytes, want 1024", len(rest))
	}
}
