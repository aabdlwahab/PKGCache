package engine

import (
	"io"
	"net/http"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/obs"
)

// StoreGuard decides whether a fill may be kept.
//
// This exists for pkgcache, whose cache budget is set by the person using it rather
// than by an operator, and whose answer to a full cache is deliberately not the
// server's. A server refuses the request — see checkQuota, where a project over its
// quota gets one clear 507 rather than an unbounded upstream bill. On a laptop that
// would mean a full cache breaks the build it was supposed to speed up, so pkgcache
// instead serves the artifact and does not keep it.
//
// Nil means everything may be stored, which is every pkgreg deployment.
type StoreGuard interface {
	// MayStore reports whether an artifact of this size may be written, and when it
	// may not, a sentence explaining why that a person can act on. A size of zero
	// means the size is not yet known.
	MayStore(size int64) (bool, string)
}

// OutcomeUncached is a request served from upstream and deliberately not kept.
//
// A sixth outcome rather than a variety of miss. A miss says "we did not have it and
// now we do", and reporting a request as one when nothing was stored would make the
// console's own numbers lie: the hit rate would keep improving as the cache filled up
// with nothing. The outcome column is free text, so this costs no migration.
const OutcomeUncached Outcome = "uncached"

// servePassThrough relays an upstream response to the client without storing it.
//
// Nothing is hashed, no blob is written, no catalog row is published, and the request
// is not coalesced with any in-flight fetch — there is no shared result to attach to.
// It is the plainest possible proxy, which is what makes it safe to run when the disk
// is the thing that went wrong.
func (e *Engine) servePassThrough(
	w http.ResponseWriter, r *http.Request, res Resolution, reason string, now time.Time,
) (Outcome, error) {
	request := res.Upstream
	request.Eco = res.Eco
	response, cancel, err := e.pool.Open(r.Context(), request)
	if err != nil {
		e.record(res, OutcomeFail, 0, now)
		return OutcomeFail, err
	}
	defer cancel()
	defer func() { _ = response.Body.Close() }()

	e.events.Publish(obs.Event{
		Kind: obs.EventFetchDone, Project: res.Project, Eco: res.Eco,
		ID: res.Key, Detail: "not cached: " + reason,
	})

	e.applyHeaders(w, res)
	for name, values := range response.Header {
		// Hop-by-hop and length headers belong to this connection, and Content-Length
		// would be wrong if the copy is cut short.
		if skipRelayHeader(name) {
			continue
		}
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	if mediaType := mediaType(res, response.Header.Get("Content-Type")); mediaType != "" {
		w.Header().Set("Content-Type", mediaType)
	}
	// The reason travels with the response so that a client transcript, not only the
	// terminal, records why this artifact will have to be fetched again next time.
	w.Header().Set("X-Pkgcache-Uncached", reason)
	w.WriteHeader(response.StatusCode)

	written, copyErr := io.Copy(w, response.Body)
	e.record(res, OutcomeUncached, written, now)
	return OutcomeUncached, copyErr
}

func skipRelayHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Content-Length", "Transfer-Encoding", "Connection", "Keep-Alive",
		"Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Upgrade":
		return true
	}
	return false
}
