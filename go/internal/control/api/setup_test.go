package api

import (
	"crypto/tls"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/eco/files"
	"github.com/aabdlwahab/PKGCache/internal/eco/gomod"
)

func TestClientCoordinatesRespectListenerTopology(t *testing.T) {
	tests := []struct {
		name       string
		snapshot   config.Snapshot
		requestURL string
		host       string
		port       int
		proxy      int
	}{
		{
			name: "request host and single configured port", snapshot: config.Defaults(),
			requestURL: "http://console.local/api/setup.sh",
			host:       "console.local", port: 8443, proxy: 8443,
		},
		{
			name: "single reverse-proxied public origin",
			snapshot: func() config.Snapshot {
				value := config.Defaults()
				value.Auth.PublicOrigin = "https://packages.example.test"
				return value
			}(),
			requestURL: "http://internal.invalid/api/setup.sh",
			host:       "packages.example.test", port: 443, proxy: 443,
		},
		{
			name: "explicit listeners keep data ports",
			snapshot: func() config.Snapshot {
				value := config.Defaults()
				value.Server.SinglePort = false
				value.Auth.PublicOrigin = "https://console.example.test"
				return value
			}(),
			requestURL: "http://internal.invalid/api/setup.sh",
			host:       "console.example.test", port: 8443, proxy: 3142,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &API{Options: Options{Config: config.NewStore(&test.snapshot)}}
			host, port, proxy, err := api.clientCoordinates(
				httptest.NewRequest("GET", test.requestURL, nil))
			if err != nil {
				t.Fatal(err)
			}
			if host != test.host || port != test.port || proxy != test.proxy {
				t.Fatalf("coordinates = %s,%d,%d; want %s,%d,%d",
					host, port, proxy, test.host, test.port, test.proxy)
			}
		})
	}
}

func TestClientSchemeUsesPublicFacingConnection(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*config.Snapshot)
		requestURL string
		tls        bool
		forwarded  string
		want       string
	}{
		{
			name: "plain listener", requestURL: "http://cache.test/",
			want: "http",
		},
		{
			name: "direct TLS", requestURL: "https://cache.test/", tls: true,
			want: "https",
		},
		{
			name: "HTTPS public origin overrides internal HTTP",
			configure: func(value *config.Snapshot) {
				value.Auth.PublicOrigin = "https://packages.example.test"
			},
			requestURL: "http://internal.test/", want: "https",
		},
		{
			name: "trusted reverse proxy",
			configure: func(value *config.Snapshot) {
				value.Server.TrustProxy = true
			},
			requestURL: "http://internal.test/", forwarded: "https", want: "https",
		},
		{
			name:       "untrusted forwarded header ignored",
			requestURL: "http://internal.test/", forwarded: "https", want: "http",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := config.Defaults()
			if test.configure != nil {
				test.configure(&snapshot)
			}
			request := httptest.NewRequest("GET", test.requestURL, nil)
			if test.tls {
				request.TLS = &tls.ConnectionState{}
			}
			if test.forwarded != "" {
				request.Header.Set("X-Forwarded-Proto", test.forwarded)
			}
			if got := clientScheme(request, &snapshot); got != test.want {
				t.Fatalf("clientScheme() = %q; want %q", got, test.want)
			}
		})
	}
}

// TestSetupStepsFollowTheInstance renders the endpoints panel for a laptop's cache and
// for a server, and checks each gets its own scheme and its own advice. Descriptors used
// to write the scheme themselves, so gomod's http:// was wrong on every TLS server and
// files' https:// on every pkgcache.
func TestSetupStepsFollowTheInstance(t *testing.T) {
	registry := eco.NewRegistry()
	registry.Register(gomod.New())
	registry.Register(files.New(nil, 0))
	tests := []struct {
		name      string
		configure func(*config.Snapshot)
		request   string
		want      []string
		never     []string
	}{
		{
			name: "local cache is plain HTTP and uses the shell's variables",
			configure: func(value *config.Snapshot) {
				value.Local.Enabled = true
				value.Server.UnifiedAddr = "127.0.0.1:41780"
				value.Server.TLS = config.TLS{}
			},
			request: "http://127.0.0.1:41780/api/v1/projects/team-a/endpoints",
			want: []string{
				"export GOPROXY=http://127.0.0.1:41780/team-a/gomod/goproxy",
				`curl -fLO "${PKGCACHE_FILES_URL}<path>"`,
			},
			never: []string{"https://", "trust store"},
		},
		{
			// Reached over plain HTTP on the single sniffed port, which is exactly where
			// reading the request's own scheme would have said http.
			name: "server with a certificate is https",
			configure: func(value *config.Snapshot) {
				value.Server.TLS.CertFile = "server.crt"
				value.Server.TLS.KeyFile = "server.key"
			},
			request: "http://cache.example.test:8443/api/v1/projects/team-a/endpoints",
			want: []string{
				"export GOPROXY=https://cache.example.test:8443/team-a/gomod/goproxy",
				"curl -fLO https://cache.example.test:8443/team-a/files/<path>",
			},
			never: []string{"http://cache.example.test"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := config.Defaults()
			test.configure(&snapshot)
			api := &API{Options: Options{Config: config.NewStore(&snapshot), Ecos: registry}}
			endpoints, err := api.endpointMap(httptest.NewRequest("GET", test.request, nil), "team-a")
			if err != nil {
				t.Fatal(err)
			}
			var rendered strings.Builder
			for _, id := range registry.IDs() {
				for _, step := range endpoints[id].(map[string]any)["setup"].([]eco.SetupStep) {
					rendered.WriteString(step.Comment + step.Command + "\n")
				}
			}
			for _, want := range test.want {
				if !strings.Contains(rendered.String(), want) {
					t.Errorf("steps are missing %q:\n%s", want, rendered.String())
				}
			}
			for _, never := range test.never {
				if strings.Contains(rendered.String(), never) {
					t.Errorf("steps contain %q:\n%s", never, rendered.String())
				}
			}
		})
	}
}

func TestRequestHostnameRejectsScriptInjection(t *testing.T) {
	for _, authority := range []string{
		"", "cache.example;touch-pwned", "user@cache.example", "cache.example/path",
		"cache.example:invalid", "cache.example\nmalicious",
	} {
		if _, err := requestHostname(authority); err == nil {
			t.Errorf("requestHostname(%q) succeeded", authority)
		}
	}
	for _, authority := range []string{
		"cache.example.test", "cache.example.test:8088", "[::1]:8088", "127.0.0.1",
	} {
		if _, err := requestHostname(authority); err != nil {
			t.Errorf("requestHostname(%q): %v", authority, err)
		}
	}
}
