package upstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Credential authenticates to an upstream.
//
// The stored form is sealed at rest by the control plane; by the time it reaches
// here it is plaintext and must never be logged. obs.NewLogger redacts the header
// names these produce, which is the second line of defence.
type Credential struct {
	Kind     string // "basic" | "bearer"
	Username string
	Password string
	Token    string
}

func (c *Credential) apply(req *http.Request) {
	switch c.Kind {
	case "basic":
		req.SetBasicAuth(c.Username, c.Password)
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

// ---- anonymous bearer tokens (OCI) ---------------------------------------
//
// OCI registries answer an unauthenticated pull with 401 plus a WWW-Authenticate
// challenge naming a token endpoint. Fetching a token from that endpoint with no
// credentials yields an anonymous token scoped to the requested repository. Docker
// Hub, ghcr and quay all work this way, so this is the ordinary path for a public
// pull rather than an error branch.

type tokenCache struct {
	mu     sync.Mutex
	tokens map[string]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

func newTokenCache() *tokenCache {
	return &tokenCache{tokens: make(map[string]cachedToken)}
}

// resolve turns a WWW-Authenticate challenge into a bearer token, caching by
// (realm, service, scope). Returns false when the challenge is not a bearer
// challenge or the token endpoint refuses — the caller then surfaces the original
// 401 rather than retrying blindly.
func (c *tokenCache) resolve(ctx context.Context, p *Pool, challenge string) (string, bool) {
	params, ok := parseChallenge(challenge)
	if !ok {
		return "", false
	}
	realm := params["realm"]
	if realm == "" {
		return "", false
	}
	key := realm + "|" + params["service"] + "|" + params["scope"]

	c.mu.Lock()
	if t, ok := c.tokens[key]; ok && time.Now().Before(t.expires) {
		c.mu.Unlock()
		return t.token, true
	}
	c.mu.Unlock()

	token, ttl, ok := fetchToken(ctx, p, realm, params)
	if !ok {
		return "", false
	}
	c.mu.Lock()
	// Expire early: a token that lapses mid-transfer of a multi-gigabyte blob would
	// fail the whole download, and re-minting one is cheap.
	c.tokens[key] = cachedToken{token: token, expires: time.Now().Add(ttl * 8 / 10)}
	c.mu.Unlock()
	return token, true
}

func fetchToken(ctx context.Context, p *Pool, realm string, params map[string]string) (string, time.Duration, bool) {
	u, err := url.Parse(realm)
	if err != nil {
		return "", 0, false
	}
	q := u.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	if s := params["scope"]; s != "" {
		q.Set("scope", s)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return "", 0, false
	}
	req.Header.Set("User-Agent", p.ua)
	resp, err := p.client.Load().Do(req)
	if err != nil {
		return "", 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", 0, false
	}

	// A token endpoint returns a small JSON document; 1 MiB is generous and bounds a
	// misbehaving or hostile endpoint.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, false
	}
	var doc struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", 0, false
	}
	token := doc.Token
	if token == "" {
		token = doc.AccessToken
	}
	if token == "" {
		return "", 0, false
	}
	ttl := time.Duration(doc.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 300 * time.Second // the registry spec's default
	}
	return token, ttl, true
}

// parseChallenge extracts the parameters of a Bearer WWW-Authenticate header:
//
//	Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull"
//
// Hand-parsed rather than regex-matched so an unquoted or unusually spaced value
// from a non-Docker registry still yields the realm we need.
func parseChallenge(h string) (map[string]string, bool) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(h), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return nil, false
	}
	out := map[string]string{}
	for _, part := range splitUnquoted(rest) {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return out, true
}

// splitUnquoted splits on commas that are not inside a quoted string. A scope value
// legitimately contains commas ("repository:a:pull,push"), so a plain Split breaks it.
func splitUnquoted(s string) []string {
	var out []string
	var start int
	inQuote := false
	for i := range len(s) {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case ',':
			if !inQuote {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// BasicAuthHeader builds the header value for a basic credential. Exposed for the
// peer protocol, which authenticates the same way.
func BasicAuthHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}
