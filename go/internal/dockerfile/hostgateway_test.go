package dockerfile

import (
	"strings"
	"testing"
)

const gatewaySource = `FROM python:3.12-slim
RUN pip install six
`

func rewriteFor(t *testing.T, options Options) string {
	t.Helper()
	result, err := Rewrite([]byte(gatewaySource), options)
	if err != nil {
		t.Fatal(err)
	}
	return string(result.Content)
}

// HostGateway is CacheAddress with every TLS part removed. Asserting the absence is the
// point: a build that mounted a secret it has no use for, or declared six certificate
// variables pointing at a file that will not exist, would fail in a way whose cause is
// nowhere in the Dockerfile the author wrote.
func TestHostGatewayCarriesNoCertificateMachinery(t *testing.T) {
	out := rewriteFor(t, Options{
		Mode: HostGateway, Project: "global",
		Base:     "http://host.docker.internal:41780",
		Registry: "host.docker.internal:41780",
	})
	for _, unwanted := range []string{
		SecretID, SecretTarget, "type=secret",
		"PIP_CERT", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS",
		"NPM_CONFIG_CAFILE", "GIT_SSL_CAINFO", "UV_NATIVE_TLS",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("HostGateway emitted %q, which only a TLS cache needs:\n%s", unwanted, out)
		}
	}
	for _, wanted := range []string{
		"FROM host.docker.internal:41780/dockerhub/library/python:3.12-slim",
		"ARG PIP_INDEX_URL=http://host.docker.internal:41780/global/pypi/root/pypi/+simple/",
		"ARG NPM_CONFIG_REGISTRY=http://host.docker.internal:41780/global/npm/",
	} {
		if !strings.Contains(out, wanted) {
			t.Errorf("HostGateway did not emit %q:\n%s", wanted, out)
		}
	}
}

// The CacheAddress mode is unchanged, which is what makes HostGateway an addition
// rather than a rewrite of behaviour pkgreg-client already ships.
func TestCacheAddressStillMountsTheCA(t *testing.T) {
	result, err := Rewrite([]byte(gatewaySource), Options{
		Mode: CacheAddress, Project: "global",
		Base: "https://cache:8443", Registry: "cache:8443",
	})
	if err != nil {
		t.Fatal(err)
	}
	out := string(result.Content)
	if !result.NeedsSecret {
		t.Error("CacheAddress no longer reports that it needs the CA secret")
	}
	for _, wanted := range []string{"type=secret", "PIP_CERT=", "UV_NATIVE_TLS=true"} {
		if !strings.Contains(out, wanted) {
			t.Errorf("CacheAddress lost %q:\n%s", wanted, out)
		}
	}
}

// no_proxy has to name the authority the tools actually reach the cache on. It listed
// only loopback, which is right for Bridge and wrong for HostGateway: with an apt proxy
// configured, pip and npm would have been sent to the cache through the cache's own
// forward proxy, which relays http:// and is not what either of them is talking to.
func TestNoProxyNamesTheAuthorityInUse(t *testing.T) {
	cases := []struct {
		name    string
		options Options
		want    string
	}{
		{
			name: "bridge",
			options: Options{
				Mode: Bridge, Project: "global", Base: "http://127.0.0.1:41780",
				Registry: "127.0.0.1:41780", AptProxy: "http://127.0.0.1:41780",
			},
			want: "ARG no_proxy=127.0.0.1,localhost",
		},
		{
			name: "host gateway",
			options: Options{
				Mode: HostGateway, Project: "global",
				Base: "http://host.docker.internal:41780", Registry: "host.docker.internal:41780",
				AptProxy: "http://host.docker.internal:41780",
			},
			want: "ARG no_proxy=127.0.0.1,localhost,host.docker.internal",
		},
		{
			name: "cache address",
			options: Options{
				Mode: CacheAddress, Project: "global",
				Base: "https://cache.internal:8443", Registry: "cache.internal:8443",
				AptProxy: "http://cache.internal:3142",
			},
			want: "ARG no_proxy=127.0.0.1,localhost,cache.internal",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := rewriteFor(t, tc.options)
			if !strings.Contains(out, tc.want) {
				t.Fatalf("want %q in:\n%s", tc.want, out)
			}
		})
	}
}

// Without an apt proxy there is no proxy to exempt anything from, and declaring an
// empty one is not the same as declaring none.
func TestNoProxyIsAbsentWithoutAnAptProxy(t *testing.T) {
	out := rewriteFor(t, Options{
		Mode: HostGateway, Project: "global",
		Base: "http://host.docker.internal:41780", Registry: "host.docker.internal:41780",
	})
	if strings.Contains(out, "no_proxy") || strings.Contains(out, "http_proxy") {
		t.Fatalf("proxy variables were declared with no proxy configured:\n%s", out)
	}
}
