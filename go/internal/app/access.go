package app

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/router"
)

// accessWriter records what a data-plane response turned out to be, for the access log.
//
// It must not cost the path it watches anything: Flush passes through for progressive
// delivery, and ReadFrom passes through so a committed blob is still sent with sendfile
// rather than copied through user space a buffer at a time.
type accessWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

// WriteHeader records the status the handler chose.
func (w *accessWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write counts the body as it goes out.
func (w *accessWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// ReadFrom keeps the connection's own ReadFrom, and with it sendfile, in the path.
func (w *accessWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	var n int64
	var err error
	if from, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err = from.ReadFrom(r)
	} else {
		n, err = io.Copy(w.ResponseWriter, r)
	}
	w.bytes += n
	return n, err
}

// Flush passes through, for a response streamed as it arrives.
func (w *accessWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection underneath.
func (w *accessWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// statusClientClosed is logged for a request whose client left before any response: 499,
// the convention nginx made common for exactly this.
const statusClientClosed = 499

// logAccess writes the one line `log.access` promises for a data-plane request.
//
// The setting was read and documented and then never acted on, so an operator who turned
// it on to find out what a client had asked for got a log with nothing in it — which is
// exactly the question a chain of caches makes hard to answer from anywhere else.
func logAccess(w *accessWriter, r *http.Request, target router.Target, started time.Time) {
	status := w.status
	switch {
	case status == 0 && r.Context().Err() != nil:
		// The client left before anything was sent — a chained cache giving up on a
		// header, usually. Logged as the 200 net/http would have sent, it read as a
		// success that delivered nothing.
		status = statusClientClosed
	case status == 0:
		status = http.StatusOK // nothing written: net/http sends an empty 200
	}
	requested := r.URL.RequestURI()
	if router.IsProxyRequest(r) {
		requested = r.URL.String()
	}
	slog.Info("access",
		"project", target.Project, "eco", target.Eco, "method", r.Method,
		"url", requested, "status", status, "bytes", w.bytes,
		"ms", time.Since(started).Milliseconds(), "remote", r.RemoteAddr)
}
