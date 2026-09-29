package local

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// holdInterval is how often a held daemon is reminded it is in use: well inside the
// shortest idle timeout anyone would set. A variable so a test need not wait a minute.
var holdInterval = time.Minute

// HoldDaemon keeps a pkgcache daemon from exiting idle while a command that depends on it
// runs, and returns the function that lets it go.
//
// The daemon judges idleness by requests and in-flight downloads, and a build between
// them is invisible to it. One crate run spent ten minutes exporting 24 GB images with not
// a request to the cache; the daemon went idle at fifteen, and BuildKit's last lazy fetch
// of a base layer — needed only at export — found nothing listening, failing three builds
// after two and a half hours. A request every minute that is not a health probe counts as
// activity for as long as the command runs.
func HoldDaemon(ctx context.Context, base string) (release func()) {
	ctx, cancel := context.WithCancel(ctx)
	target := strings.TrimRight(base, "/") + "/api/v1/me"
	go func() {
		client := &http.Client{Timeout: 10 * time.Second}
		ticker := time.NewTicker(holdInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
			if err != nil {
				return
			}
			if response, err := client.Do(request); err == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
		}
	}()
	return cancel
}
