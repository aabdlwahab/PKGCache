package router

import (
	"net"
	"net/url"
	"strings"
)

// PlainProxyForm is how an https URL travels through this system's plain-HTTP forward
// proxies: https://host/path becomes http://host:443/path.
//
// apt can reach an https repository through a proxy only by tunnelling it (CONNECT), and a
// tunnel is bytes a cache can neither read nor keep — the proxy refuses it, apt skips the
// repository with "Invalid response from proxy", and an image installing cuda-toolkit or
// docker-ce from its vendor's repository fails where a plain docker build succeeds. Written
// this way the request is an ordinary proxied GET, and the cache makes the TLS connection
// upstream itself; UpgradePlainProxyForm is the other half.
//
// Only a URL on the default https port has this form. Anything else is returned as it was.
func PlainProxyForm(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Port() != "" || u.User != nil {
		return raw
	}
	u.Scheme = "http"
	u.Host = net.JoinHostPort(u.Hostname(), "443")
	return u.String()
}

// UpgradePlainProxyForm turns http://host:443/... back into the https URL it stands for,
// and reports whether it did.
//
// The port is an unambiguous marker: nothing serves plain HTTP on 443, so a request for it
// can only be one of these.
func UpgradePlainProxyForm(u *url.URL) bool {
	if u.Scheme != "http" || u.Port() != "443" {
		return false
	}
	host := u.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	u.Scheme, u.Host = "https", host
	return true
}
