package api

import (
	"testing"

	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/obs"
)

// The event bus is unfiltered by design, so every rule about who may see what lives in
// eventFilter. These are unit tests against that decision table rather than against a
// live stream: the stream's job is to apply the verdict, and the verdict is the part
// with the security content.

func TestEventFilterWithholdsAuditFromEveryoneButSuperusers(t *testing.T) {
	audit := obs.Event{Kind: obs.EventAudit, Name: "user.create", Detail: "newadmin"}

	superuser := eventFilter{audit: true, jobs: true}
	if !superuser.permits(audit) {
		t.Error("a superuser was denied the audit stream")
	}

	// Every other subscriber. GET /api/v1/audit is superuser-only, and before this
	// filter existed the same records arrived over SSE to any authenticated caller —
	// so a plain user could watch accounts being created in real time.
	for name, filter := range map[string]eventFilter{
		"ordinary user": {jobs: true},
		"guest":         {project: config.GlobalProject},
		"anon_read":     {jobs: true},
	} {
		if filter.permits(audit) {
			t.Errorf("%s received an audit event", name)
		}
	}
}

func TestEventFilterScopesAGuestToOneProject(t *testing.T) {
	guest := eventFilter{project: config.GlobalProject}

	cases := []struct {
		name  string
		event obs.Event
		want  bool
	}{
		{
			"global fetch",
			obs.Event{Kind: obs.EventFetchDone, Project: "global", Name: "left-pad"},
			true,
		},
		{
			"global progress",
			obs.Event{Kind: obs.EventFetchProgress, Project: "global", Size: 64 << 10},
			true,
		},
		{
			"another tenant's fetch",
			obs.Event{Kind: obs.EventFetchDone, Project: "secret-team", Name: "internal-lib"},
			false,
		},
		{
			"another tenant's progress",
			obs.Event{Kind: obs.EventFetchProgress, Project: "secret-team"},
			false,
		},
		{
			// Health carries no project and is about the instance, not a tenant.
			// Dropping it would leave a guest's status dot stuck on "reconnecting".
			"instance health",
			obs.Event{Kind: obs.EventHealth, Status: "ok"},
			true,
		},
		{
			"job update for global",
			obs.Event{Kind: obs.EventJobUpdate, Project: "global"},
			false, // guests cannot call /jobs, so the frame would be unusable
		},
		{
			"audit",
			obs.Event{Kind: obs.EventAudit, Name: "project.delete"},
			false,
		},
	}
	for _, tc := range cases {
		if got := guest.permits(tc.event); got != tc.want {
			t.Errorf("guest.permits(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestEventFilterDoesNotNarrowOrdinaryUsers: the audit fix must not turn into a
// silent reduction of what a normal console session sees.
func TestEventFilterDoesNotNarrowOrdinaryUsers(t *testing.T) {
	user := eventFilter{jobs: true}
	for _, event := range []obs.Event{
		{Kind: obs.EventFetchStart, Project: "global"},
		{Kind: obs.EventFetchProgress, Project: "team-a"},
		{Kind: obs.EventFetchDone, Project: "anything-at-all"},
		{Kind: obs.EventCacheHit, Project: "team-b"},
		{Kind: obs.EventJobUpdate, Project: "team-a"},
		{Kind: obs.EventHealth},
	} {
		if !user.permits(event) {
			t.Errorf("an ordinary user lost %s for project %q", event.Kind, event.Project)
		}
	}
}

// TestEmptyFilterPermitsEverything pins the zero value's meaning, because an empty
// project string means "every project" and a future reader could reasonably guess the
// opposite. The no-accounts case relies on it.
func TestEmptyFilterPermitsEverything(t *testing.T) {
	open := eventFilter{audit: true, jobs: true}
	for _, event := range []obs.Event{
		{Kind: obs.EventFetchDone, Project: "anything"},
		{Kind: obs.EventAudit},
		{Kind: obs.EventJobUpdate},
		{Kind: obs.EventHealth},
	} {
		if !open.permits(event) {
			t.Errorf("the open filter withheld %s", event.Kind)
		}
	}
}
