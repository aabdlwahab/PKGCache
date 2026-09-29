package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// ErrBodyIdle is what reading a response body returns once its origin stopped sending it.
var ErrBodyIdle = errors.New("upstream: the response stopped arriving")

// idleBody abandons a response body that has stopped arriving.
//
// A body used to have only the request's whole deadline, and one number cannot serve both
// jobs it had. Sized for the largest artifact on a slow link, it let a transfer that had
// stopped dead hold for all of it: Docker Hub sent 28 MB of a 2 GB CUDA layer and then
// nothing, and the build waited nineteen minutes for the deadline before the transfer was
// picked up again and finished at a megabyte a second. Sized for a stall, it cuts transfers
// that are still moving. So the deadline bounds the whole request, and this bounds a
// silence.
//
// The clock runs only while a Read is waiting on the origin. Time the caller spends
// elsewhere — writing to a slow client, between reads — is not the origin's silence, and
// counting it would cut a relay whose client paused.
type idleBody struct {
	io.ReadCloser
	idle  time.Duration
	timer *time.Timer
	fired atomic.Bool
}

// watchIdle wraps body so that a Read which waits longer than idle ends the request.
// cancel is the request's own: stopping it is what makes the blocked Read return.
func watchIdle(body io.ReadCloser, idle time.Duration, cancel context.CancelFunc) io.ReadCloser {
	if idle <= 0 || body == nil || body == http.NoBody {
		return body
	}
	b := &idleBody{ReadCloser: body, idle: idle}
	b.timer = time.AfterFunc(idle, func() {
		b.fired.Store(true)
		cancel()
	})
	b.timer.Stop()
	return b
}

// Read arms the clock for as long as it waits on the origin, and no longer.
func (b *idleBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.idle)
	n, err := b.ReadCloser.Read(p)
	b.timer.Stop()
	if err != nil && b.fired.Load() {
		// Said plainly: the transport's own error for a cancelled request reads as though
		// somebody chose to stop, which is the opposite of what happened.
		return n, fmt.Errorf("%w: nothing for %s", ErrBodyIdle, b.idle)
	}
	return n, err
}

// Close stops the clock with the body, so an abandoned response cannot cancel anything.
func (b *idleBody) Close() error {
	b.timer.Stop()
	return b.ReadCloser.Close()
}
