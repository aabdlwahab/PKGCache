package local

import (
	"context"
	"fmt"
	"net/http"
)

// The two things the status bar can do that had no client-side path.
//
// Both exist because the tray talks to a *running* daemon. The command line's versions open
// the store directly and therefore require it stopped — right for `pkgcache prune` typed
// into a terminal, impossible from an icon whose whole context is a cache that is up.

// ToggleOffline flips whether a project serves only what it already holds.
//
// Read then write rather than a flag: the icon shows one state and offers its opposite, and
// deciding which from the click alone would mean the menu and the cache could disagree
// after anything else changed it — the console, another window, a command.
func ToggleOffline(ctx context.Context, state State, project string) error {
	api := newProjectAPI(state)
	var current struct {
		Offline bool `json:"offline"`
	}
	path := "/api/v1/projects/" + project
	if err := api.do(ctx, http.MethodGet, path, nil, &current); err != nil {
		return err
	}
	return api.do(ctx, http.MethodPatch, path,
		map[string]any{"offline": !current.Offline}, nil)
}

// CollectVia reclaims what nothing references, through a running daemon.
//
// Followed to completion rather than fired and forgotten: a menu item that returns before
// the work is done leaves the icon reporting the size it had a moment ago, and somebody
// clicking again because nothing appeared to happen.
func CollectVia(ctx context.Context, state State) error {
	var job Job
	if err := newProjectAPI(state).do(
		ctx, http.MethodPost, "/api/v1/maintenance/gc", map[string]any{}, &job); err != nil {
		return err
	}
	if _, err := FollowJob(ctx, state, job.ID, nil); err != nil {
		return fmt.Errorf("reclaim space: %w", err)
	}
	return nil
}
