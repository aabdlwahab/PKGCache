// Package router matches request paths without ever unescaping them behind the
// handler's back.
//
// http.ServeMux cannot serve this project, for two measured reasons. Both are
// verified against the live standard library in TestServeMuxLimitations, so if a
// future Go release removes them the test says so rather than this comment going
// quietly stale.
//
//  1. OCI needs "/v2/{name...}/manifests/{ref}" — a greedy wildcard that is not
//     terminal, discriminated by the literal suffix after it. ServeMux PANICS at
//     registration: "{...} wildcard not at end". There is no way to express it.
//  2. The apt forward proxy relays whatever path a client asked for, and ServeMux
//     cleans paths and 307-redirects: "/a/../b" becomes "/b" and "/a//b" becomes
//     "/a/b". Proxying must be byte-faithful, so rewriting is wrong.
//
// A third reason was expected and turned out NOT to hold: npm's "/@babel%2Fcore"
// is handled correctly by modern ServeMux, which keeps it as one segment and
// decodes it in PathValue. That case is still tested here, as a property this
// router must not regress.
//
// One consequence of ServeMux's approach is worth keeping even so: PathValue only
// ever yields the DECODED value, so "%2B" and a literal "+" become indistinguishable.
// This router hands back the raw segment and makes decoding explicit, which is what
// lets the apt adapter reconstruct an upstream URL exactly as the client wrote it.
package router

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Params holds captured path segments, still percent-escaped.
//
// Raw by default is the whole point: an npm package name and a file path have
// different opinions about what "%2F" means, and only the adapter knows which.
type Params struct {
	names  []string
	values []string
}

// Get returns a capture exactly as it appeared in the request path.
func (p Params) Get(name string) string {
	for i, n := range p.names {
		if n == name {
			return p.values[i]
		}
	}
	return ""
}

// Unescape returns a capture with percent-decoding applied. A malformed encoding
// yields the raw value rather than an error: it is a client's problem to diagnose,
// and the adapter's 404 is a better answer than a 500.
func (p Params) Unescape(name string) string {
	raw := p.Get(name)
	if decoded, err := url.PathUnescape(raw); err == nil {
		return decoded
	}
	return raw
}

// Has reports whether a capture was set.
func (p Params) Has(name string) bool {
	for _, n := range p.names {
		if n == name {
			return true
		}
	}
	return false
}

// Handler receives the request plus its path captures.
type Handler func(http.ResponseWriter, *http.Request, Params)

// segment is one element of a compiled pattern.
type segment struct {
	literal string // when capture == ""
	capture string // parameter name
	greedy  bool   // "{name...}": matches zero or more segments
}

// Route is a compiled pattern bound to a handler.
type Route struct {
	Methods []string
	pattern string
	prefix  []segment // before the greedy wildcard
	suffix  []segment // after it
	greedy  string    // capture name, "" when the pattern has none
	// trailingSlash records whether the pattern ended in "/", which is significant:
	// PyPI's simple index is "/simple/{project}/" and the un-slashed form is a
	// different resource.
	trailingSlash bool
	handler       Handler
}

// Pattern returns the source pattern, for diagnostics.
func (r *Route) Pattern() string { return r.pattern }

// Mux matches routes in registration order.
//
// First match wins, deliberately: adapters register their admin routes ("+indexes",
// "+progress") before greedy catch-alls, and specificity ordering would make that
// depend on a scoring rule nobody can predict.
type Mux struct {
	routes   []*Route
	NotFound Handler
}

// New returns an empty mux.
func New() *Mux { return &Mux{} }

// Handle compiles and registers a pattern.
//
// Syntax:
//
//	/literal/path          literal segments
//	/{name}                exactly one segment, captured raw
//	/{name...}             zero or more segments, captured raw and slash-joined;
//	                       may be followed by literal or single-capture segments
//	/prefix/               a trailing slash is significant
//
// At most one greedy wildcard per pattern. That is not a limitation in practice —
// two would need backtracking to disambiguate, and no real protocol requires it.
func (m *Mux) Handle(methods []string, pattern string, h Handler) {
	route, err := compile(methods, pattern, h)
	if err != nil {
		// A malformed pattern is a programming error discovered at startup, not a
		// runtime condition. Failing loudly here beats a route that silently never
		// matches.
		panic(fmt.Sprintf("router: %v", err))
	}
	m.routes = append(m.routes, route)
}

func compile(methods []string, pattern string, h Handler) (*Route, error) {
	if !strings.HasPrefix(pattern, "/") {
		return nil, fmt.Errorf("pattern %q must start with /", pattern)
	}
	r := &Route{
		Methods: methods,
		pattern: pattern,
		// "/" is the root resource, which splitPath reports as trailing; every other
		// pattern is trailing only if it actually ends in a slash.
		trailingSlash: strings.HasSuffix(pattern, "/"),
		handler:       h,
	}

	trimmed := strings.Trim(pattern, "/")
	var parts []string
	if trimmed != "" {
		parts = strings.Split(trimmed, "/")
	}

	seenGreedy := false
	for _, part := range parts {
		seg, err := parseSegment(part, pattern)
		if err != nil {
			return nil, err
		}
		switch {
		case seg.greedy && seenGreedy:
			return nil, fmt.Errorf("pattern %q has more than one {...} wildcard", pattern)
		case seg.greedy:
			seenGreedy = true
			r.greedy = seg.capture
		case seenGreedy:
			r.suffix = append(r.suffix, seg)
		default:
			r.prefix = append(r.prefix, seg)
		}
	}
	return r, nil
}

func parseSegment(part, pattern string) (segment, error) {
	if !strings.HasPrefix(part, "{") || !strings.HasSuffix(part, "}") {
		if strings.ContainsAny(part, "{}") {
			return segment{}, fmt.Errorf("pattern %q has a malformed segment %q", pattern, part)
		}
		return segment{literal: part}, nil
	}
	name := part[1 : len(part)-1]
	greedy := strings.HasSuffix(name, "...")
	if greedy {
		name = strings.TrimSuffix(name, "...")
	}
	if name == "" {
		return segment{}, fmt.Errorf("pattern %q has an unnamed capture", pattern)
	}
	return segment{capture: name, greedy: greedy}, nil
}

// ServeHTTP dispatches on the escaped path.
func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h, params, ok := m.Lookup(r.Method, r.URL.EscapedPath())
	if !ok {
		if m.NotFound != nil {
			m.NotFound(w, r, Params{})
			return
		}
		http.NotFound(w, r)
		return
	}
	h(w, r, params)
}

// Lookup finds the handler for a method and escaped path.
//
// A path matching a route registered for other methods yields no match; the caller
// decides between 404 and 405. Exposed separately from ServeHTTP so the project
// router can try several ecosystem muxes for one request.
func (m *Mux) Lookup(method, escapedPath string) (Handler, Params, bool) {
	segs, trailing := splitPath(escapedPath)
	for _, route := range m.routes {
		if !route.allows(method) {
			continue
		}
		if params, ok := route.match(segs, trailing); ok {
			return route.handler, params, true
		}
	}
	return nil, Params{}, false
}

func (r *Route) allows(method string) bool {
	if len(r.Methods) == 0 {
		return true
	}
	for _, m := range r.Methods {
		if m == method {
			return true
		}
		// HEAD is served by the GET handler unless one is registered explicitly;
		// net/http discards the body for us.
		if m == http.MethodGet && method == http.MethodHead {
			return true
		}
	}
	return false
}

// match tests one route against a split path.
//
// With at most one greedy wildcard, matching is linear and needs no backtracking:
// consume the fixed prefix from the front, the fixed suffix from the back, and
// whatever remains in the middle is the greedy capture.
func (r *Route) match(segs []string, trailing bool) (Params, bool) {
	if r.greedy == "" {
		if len(segs) != len(r.prefix) {
			return Params{}, false
		}
		// A trailing slash distinguishes two different resources, so it must agree.
		if trailing != r.trailingSlash {
			return Params{}, false
		}
		return matchFixed(r.prefix, segs)
	}

	if len(segs) < len(r.prefix)+len(r.suffix) {
		return Params{}, false
	}
	// Only a terminal wildcard is free about trailing slashes: "/files/{path...}"
	// must serve both "/files/dir/" and "/files/file.txt".
	if len(r.suffix) > 0 && trailing != r.trailingSlash {
		return Params{}, false
	}

	head, ok := matchFixed(r.prefix, segs[:len(r.prefix)])
	if !ok {
		return Params{}, false
	}
	tailStart := len(segs) - len(r.suffix)
	tail, ok := matchFixed(r.suffix, segs[tailStart:])
	if !ok {
		return Params{}, false
	}

	middle := strings.Join(segs[len(r.prefix):tailStart], "/")
	// A terminal greedy keeps the trailing slash, so a directory request stays
	// distinguishable from a file request of the same name.
	if len(r.suffix) == 0 && trailing && middle != "" {
		middle += "/"
	}

	params := Params{
		names:  append(append([]string{}, head.names...), r.greedy),
		values: append(append([]string{}, head.values...), middle),
	}
	params.names = append(params.names, tail.names...)
	params.values = append(params.values, tail.values...)
	return params, true
}

func matchFixed(pattern []segment, segs []string) (Params, bool) {
	var p Params
	for i, seg := range pattern {
		if seg.capture == "" {
			if !literalMatches(seg.literal, segs[i]) {
				return Params{}, false
			}
			continue
		}
		// A single-segment capture must not be empty: "/a//b" should not match
		// "/a/{x}/b" with an empty x.
		if segs[i] == "" {
			return Params{}, false
		}
		p.names = append(p.names, seg.capture)
		p.values = append(p.values, segs[i])
	}
	return p, true
}

// literalMatches compares one escaped request segment against a fixed pattern segment.
//
// The comparison is on the decoded form, because percent-encoding is not meaningful
// within a segment: RFC 3986 makes "%2Bf" and "+f" the same path segment, and pip
// re-encodes the "+" when it derives a PEP 658 ".metadata" URL from an index link the
// cache emitted with a literal "+". A byte comparison here made those two requests
// address different routes, so the sidecar 404'd and the install failed.
//
// Decoding is safe only because the path was already split on "/": a "%2F" inside a
// segment decodes to a slash that can never equal a pattern literal, so an escaped
// separator still cannot smuggle its way into becoming a real one. Captures keep their
// escaped form — that is what preserves "@babel%2Fcore" as one npm package name.
func literalMatches(literal, escaped string) bool {
	if literal == escaped {
		return true
	}
	decoded, err := url.PathUnescape(escaped)
	return err == nil && decoded == literal
}

// splitPath breaks an escaped path into segments without decoding or cleaning it.
//
// No path cleaning is deliberate: "." and ".." are ordinary characters in a package
// filename, and the safety boundary is the blob store's validated digests and the
// catalog's exact-match keys, not a normalisation pass here.
func splitPath(escaped string) (segs []string, trailing bool) {
	if escaped == "" || escaped == "/" {
		return nil, escaped == "/"
	}
	trailing = strings.HasSuffix(escaped, "/")
	trimmed := strings.Trim(escaped, "/")
	if trimmed == "" {
		return nil, true
	}
	return strings.Split(trimmed, "/"), trailing
}
