package ops

import (
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
)

// A rewritten lock is only as good as the address it names, and the one thing that
// address used to get wrong was the scheme: always https, including on a pkgcache that
// has no certificate and listens in plain HTTP. Every posture is pinned, because each is
// somebody's real deployment.
func TestLockBaseNamesTheSchemeThisInstanceServes(t *testing.T) {
	for _, test := range []struct {
		name   string
		server config.Server
		origin string
		want   string
	}{
		{
			name:   "a local cache has no certificate, so plain http",
			server: config.Server{SinglePort: true, UnifiedAddr: "127.0.0.1:41780"},
			want:   "http://cache.internal:41780/work/pypi",
		},
		{
			name: "a server with a certificate is https",
			server: config.Server{SinglePort: true, UnifiedAddr: ":8443",
				TLS: config.TLS{CertFile: "server.crt", KeyFile: "server.key"}},
			want: "https://cache.internal:8443/work/pypi",
		},
		{
			name:   "TLS terminated in front: the public origin decides, port and all",
			server: config.Server{SinglePort: true, UnifiedAddr: ":8080"},
			origin: "https://cache.example.com",
			want:   "https://cache.internal:443/work/pypi",
		},
		{
			name:   "a public origin with a port keeps it",
			server: config.Server{SinglePort: true, UnifiedAddr: ":8080"},
			origin: "https://cache.example.com:9443",
			want:   "https://cache.internal:9443/work/pypi",
		},
		{
			// With separate listeners the origin is the console's, so only its scheme
			// carries over; the indexes are still on the unified port.
			name:   "separate listeners take the origin's scheme and the unified port",
			server: config.Server{UnifiedAddr: ":8443"},
			origin: "https://cache.example.com",
			want:   "https://cache.internal:8443/work/pypi",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := &config.Snapshot{Server: test.server}
			snapshot.Auth.PublicOrigin = test.origin
			got, err := lockBase(snapshot, "cache.internal", "work")
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("lock names\n  %s\nwant\n  %s", got, test.want)
			}
		})
	}
}

func TestLockBaseRefusesAListenerWithNoPort(t *testing.T) {
	if _, err := lockBase(&config.Snapshot{}, "cache.internal", "work"); err == nil {
		t.Fatal("a lock was addressed to a listener with no port")
	}
}
