package api

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"

	"github.com/brightskies/pkgreg/internal/config"
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
