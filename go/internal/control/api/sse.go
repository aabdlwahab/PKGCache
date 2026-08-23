package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control/auth"
	"github.com/aabdlwahab/PKGCache/internal/obs"
)

// eventFilter decides which bus events one subscriber may receive.
//
// The bus is deliberately unfiltered — one publisher, every consumer — and until now
// the stream inherited that: the console discarded other projects' frames client-side,
// which is a rendering decision standing in for an authorization one. Two consequences
// were live.
//
// The audit trail was the serious one. Every audit record is published to the bus, so
// any authenticated caller could watch account creation, project deletion and token
// minting arrive in real time, while GET /api/v1/audit answering the same question is
// superuser-only. Filtering here is what makes those two agree.
//
// The other is what prompted this: a guest could not be given the stream at all while
// it carried every tenant's activity, so live updates were simply switched off for
// them. Scoping by project is what lets a read-only visitor watch the cache fill.
type eventFilter struct {
	// project limits frames to one project. Empty means every project. Frames with no
	// project of their own — health, most notably — are never limited by it.
	project string
	// audit and jobs gate whole event kinds that correspond to REST endpoints the
	// subscriber may not call.
	audit bool
	jobs  bool
}

func (f eventFilter) permits(event obs.Event) bool {
	switch event.Kind {
	case obs.EventAudit:
		return f.audit
	case obs.EventJobUpdate:
		if !f.jobs {
			return false
		}
	}
	if f.project != "" && event.Project != "" && event.Project != f.project {
		return false
	}
	return true
}

// filterFor derives a subscriber's filter from its identity, once, at subscribe time.
//
// Once rather than per frame: this stream carries a progress event per 64 KiB of every
// download, and a permission lookup on that path would be the most-executed database
// query in the process. The cost is that a role change does not reach an open stream;
// sessions are process-local and a console reload re-derives it, which is the same
// bound the rest of the session model already has.
func (a *API) filterFor(r *http.Request) eventFilter {
	if !a.Accounts.Enabled() {
		return eventFilter{audit: true, jobs: true}
	}
	actor, found := a.guard.Actor(r)
	switch {
	case !found:
		// Reached only under anon_read, which is the unscoped opt-in. Withhold the
		// audit trail even so: nothing about "anonymous reads are fine here" implies
		// the account log is.
		return eventFilter{jobs: true}
	case auth.IsGuest(actor):
		return eventFilter{project: config.GlobalProject}
	case actor.Role == "superuser":
		return eventFilter{audit: true, jobs: true}
	default:
		return eventFilter{jobs: true}
	}
}

func (a *API) events(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.guard.RequireAuthed(r); err != nil {
		return err
	}
	filter := a.filterFor(r)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("api: streaming is unsupported by the response writer")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	subscription := a.Events.Subscribe(256)
	defer subscription.Close()
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-a.closing:
			// The process is going. Ending the stream here is what lets Shutdown return
			// promptly instead of waiting out its whole grace period on a handler that
			// would never finish by itself.
			return nil
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return nil
			}
			flusher.Flush()
		case event := <-subscription.C:
			if subscription.Dropped() > 0 {
				_, _ = fmt.Fprintf(w, "event: dropped\ndata: {\"error\":\"slow client\"}\n\n")
				flusher.Flush()
				return nil
			}
			// Filtered before marshalling, and before the frame reaches the socket:
			// a withheld event must cost nothing and, more to the point, must leave
			// no trace a client could time.
			if !filter.permits(event) {
				continue
			}
			body, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n",
				sseKind(event.Kind), body); err != nil {
				return nil
			}
			flusher.Flush()
		}
	}
}

func sseKind(kind obs.EventKind) string {
	switch kind {
	case obs.EventJobUpdate:
		return "job"
	case obs.EventHealth:
		return "health"
	case obs.EventAudit:
		return "audit"
	default:
		return "progress"
	}
}
