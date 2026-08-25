// Package pypi implements a PEP 503/691 Python package index proxy.
//
// Simple-index documents are parsed into one protocol-neutral representation and
// rendered as either HTML or JSON according to the client Accept header. Wheels,
// source distributions, and PEP 658/714 metadata sidecars stream through the shared
// cache engine.
package pypi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/engine"
	"github.com/aabdlwahab/PKGCache/internal/router"
)

const (
	// ID is the ecosystem identifier.
	ID = "pypi"

	jsonMediaType = "application/vnd.pypi.simple.v1+json"
	simpleTTL     = 5 * time.Minute
)

var defaultIndexes = map[string]string{
	"root/pypi":          "https://pypi.org/simple",
	"root/pytorch-cu124": "https://download.pytorch.org/whl/cu124",
	"root/pytorch-cu128": "https://download.pytorch.org/whl/cu128",
	"root/pytorch-cpu":   "https://download.pytorch.org/whl/cpu",
}

// Repo is the PyPI ecosystem.
type Repo struct {
	indexes map[string]string
	ttl     time.Duration
}

// New builds an adapter with the public PyPI and PyTorch indexes.
func New() *Repo { return NewWithIndexes(defaultIndexes) }

// NewWithIndexes builds an adapter with an explicit index-name to simple-root map.
func NewWithIndexes(indexes map[string]string) *Repo {
	copied := make(map[string]string, len(indexes))
	for name, origin := range indexes {
		copied[strings.Trim(name, "/")] = strings.TrimRight(origin, "/")
	}
	return &Repo{indexes: copied, ttl: simpleTTL}
}

// Descriptor implements eco.Ecosystem.
func (r *Repo) Descriptor() eco.Descriptor {
	return eco.Descriptor{
		ID:               ID,
		Display:          "PyPI",
		Summary:          "Python wheels, source distributions, and package indexes.",
		Storage:          eco.StorageBlob,
		Listener:         eco.ListenerPathPrefixed,
		Upstreams:        eco.UpstreamNamedSet,
		DefaultUpstreams: cloneMap(r.indexes),
		Freshness: func(key string) eco.Freshness {
			if strings.HasPrefix(key, "simple/") {
				return eco.Revalidate(r.ttl)
			}
			return eco.Immutable
		},
		ParseArtifact: parseArtifactKey,
		Setup:         setupSteps,
	}
}

// Routes implements eco.Ecosystem.
func (r *Repo) Routes() []eco.Route {
	return []eco.Route{
		{
			Methods: []string{http.MethodGet}, Pattern: "/+indexes",
			Handler: r.listIndexes, Admin: true,
		},
		{
			Methods: []string{http.MethodGet},
			Pattern: "/{index...}/+simple/{project}/", Handler: r.simple,
		},
		{
			Methods: []string{http.MethodGet, http.MethodHead},
			Pattern: "/{index...}/+f/{project}/{filename}", Handler: r.file,
		},
	}
}

func (r *Repo) listIndexes(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	if err := c.JSON(http.StatusOK, c.Upstreams()); err != nil {
		c.WriteError(err)
	}
}

func (r *Repo) simple(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	index := strings.Trim(p.Unescape("index"), "/")
	project := NormalizeName(p.Unescape("project"))
	if index == "" || project == "" {
		_ = c.NotFound("invalid index or project")
		return
	}
	origin, ok := c.Upstream(index)
	if !ok {
		_ = c.NotFound("unknown index " + index)
		return
	}

	files, err := r.loadSimple(c, index, project, origin)
	if err != nil {
		if c.Offline() {
			c.WriteError(err)
		} else {
			_ = c.NotFound("no cached index for " + project)
		}
		return
	}
	prefix := strings.TrimRight(c.ExternalBase(), "/") + "/" +
		escapeIndex(index) + "/+f/" + url.PathEscape(project)
	if wantsJSON(req.Header.Get("Accept")) {
		body, err := renderJSON(project, files, prefix)
		if err != nil {
			c.WriteError(err)
			return
		}
		_ = c.ServeBytes(http.StatusOK, jsonMediaType, body)
		return
	}
	_ = c.ServeBytes(http.StatusOK, "text/html; charset=utf-8",
		renderHTML(project, files, prefix))
}

func (r *Repo) file(w http.ResponseWriter, req *http.Request, p router.Params) {
	c := eco.CtxFrom(w, req, p)
	index := strings.Trim(p.Unescape("index"), "/")
	project := NormalizeName(p.Unescape("project"))
	filename := p.Unescape("filename")
	if index == "" || project == "" || filename == "" || strings.Contains(filename, "/") {
		_ = c.NotFound("invalid file path")
		return
	}
	origin, ok := c.Upstream(index)
	if !ok {
		_ = c.NotFound("unknown index " + index)
		return
	}

	isMetadata := strings.HasSuffix(filename, ".metadata")
	lookup := strings.TrimSuffix(filename, ".metadata")
	files, err := r.loadSimple(c, index, project, origin)
	if err != nil {
		c.WriteError(err)
		return
	}
	var found *simpleFile
	for i := range files {
		if files[i].Filename == lookup {
			found = &files[i]
			break
		}
	}
	if found == nil {
		_ = c.NotFound("unknown file " + filename)
		return
	}

	upstreamURL := found.URL
	mediaType := "application/octet-stream"
	var expect engine.Expect
	var artifact *catalog.Artifact
	accessName := project
	if isMetadata {
		upstreamURL = addMetadataSuffix(upstreamURL)
		mediaType = "text/plain; charset=utf-8"
		accessName = ""
	} else {
		if raw := found.Hashes["sha256"]; raw != "" {
			if digest, err := blob.ParseDigest(raw); err == nil {
				expect.Digest = digest
			}
		}
		name, version, arch, artifactOK := ParseDistributionFilename(filename)
		if artifactOK {
			artifact = &catalog.Artifact{
				Name: name, Version: version, Arch: arch, Origin: upstreamURL,
				Extra: map[string]any{"index": index},
			}
		}
	}

	err = c.Serve(engine.Resolution{
		Key:        index + "/+f/" + project + "/" + filename,
		Upstream:   c.UpstreamRequest(upstreamURL, nil),
		Expect:     expect,
		MediaType:  mediaType,
		Artifact:   artifact,
		AccessName: accessName,
	})
	if err != nil {
		c.WriteError(err)
	}
}

func (r *Repo) loadSimple(
	c *eco.Ctx, index, project, origin string,
) ([]simpleFile, error) {
	headers := http.Header{}
	headers.Set("Accept", jsonMediaType+", text/html;q=0.9")
	pageURL := eco.JoinURL(origin, url.PathEscape(project)) + "/"
	doc, err := c.Document(engine.DocSpec{
		Name:    "simple/" + index + "/" + project,
		Key:     "simple/" + index + "/" + project,
		URL:     pageURL,
		TTL:     r.ttl,
		Headers: headers,
	})
	if err != nil {
		return nil, err
	}
	files, err := parseSimple(doc.Body, doc.MediaType, pageURL)
	if err != nil {
		return nil, fmt.Errorf("pypi: parse %s/%s: %w", index, project, err)
	}
	return files, nil
}

type simpleFile struct {
	Filename       string
	URL            string
	Hashes         map[string]string
	RequiresPython string
	Yanked         any // false, true, or a PEP 592 reason string
	CoreMetadata   any // false, true, or map[algorithm]digest
}

func parseSimple(body []byte, contentType, pageURL string) ([]simpleFile, error) {
	trimmed := bytes.TrimSpace(body)
	if strings.Contains(strings.ToLower(contentType), "json") ||
		(len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')) {
		return parseJSON(body, pageURL)
	}
	return parseHTML(body, pageURL)
}

func parseJSON(body []byte, pageURL string) ([]simpleFile, error) {
	type inputFile struct {
		Filename         string
		URL              string
		Hashes           map[string]string
		RequiresPython   string
		Yanked           json.RawMessage
		CoreMetadata     json.RawMessage
		DistInfoMetadata json.RawMessage
	}
	var inputs []inputFile
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
		// Python persisted its normalized cache as a private snake_case array,
		// rather than as the PEP 691 response object it rendered to clients.
		// The migration importer deliberately retains those bytes, so accept this
		// legacy on-disk representation as an input format.
		var legacy []struct {
			Filename       string            `json:"filename"`
			URL            string            `json:"url"`
			Hashes         map[string]string `json:"hashes"`
			RequiresPython string            `json:"requires_python"`
			Yanked         json.RawMessage   `json:"yanked"`
			CoreMetadata   json.RawMessage   `json:"core_metadata"`
		}
		if err := json.Unmarshal(body, &legacy); err != nil {
			return nil, err
		}
		inputs = make([]inputFile, 0, len(legacy))
		for _, f := range legacy {
			inputs = append(inputs, inputFile{
				Filename: f.Filename, URL: f.URL, Hashes: f.Hashes,
				RequiresPython: f.RequiresPython, Yanked: f.Yanked,
				CoreMetadata: f.CoreMetadata,
			})
		}
	} else {
		var doc struct {
			Files []struct {
				Filename         string            `json:"filename"`
				URL              string            `json:"url"`
				Hashes           map[string]string `json:"hashes"`
				RequiresPython   string            `json:"requires-python"`
				Yanked           json.RawMessage   `json:"yanked"`
				CoreMetadata     json.RawMessage   `json:"core-metadata"`
				DistInfoMetadata json.RawMessage   `json:"dist-info-metadata"`
			} `json:"files"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, err
		}
		inputs = make([]inputFile, 0, len(doc.Files))
		for _, f := range doc.Files {
			inputs = append(inputs, inputFile{
				Filename: f.Filename, URL: f.URL, Hashes: f.Hashes,
				RequiresPython: f.RequiresPython, Yanked: f.Yanked,
				CoreMetadata: f.CoreMetadata, DistInfoMetadata: f.DistInfoMetadata,
			})
		}
	}
	files := make([]simpleFile, 0, len(inputs))
	for _, f := range inputs {
		// Same rule as the HTML path: PEP 691 is JSON but no more trusted for it.
		if !validDistributionFilename(f.Filename) || f.URL == "" {
			continue
		}
		resolved, fragment, err := resolveFileURL(pageURL, f.URL)
		if err != nil {
			continue
		}
		hashes := cloneMap(f.Hashes)
		if hashes == nil {
			hashes = map[string]string{}
		}
		addFragmentHash(hashes, fragment)
		core := f.CoreMetadata
		if len(bytes.TrimSpace(core)) == 0 || bytes.Equal(bytes.TrimSpace(core), []byte("null")) {
			core = f.DistInfoMetadata
		}
		files = append(files, simpleFile{
			Filename:       f.Filename,
			URL:            resolved,
			Hashes:         hashes,
			RequiresPython: f.RequiresPython,
			Yanked:         normalizeYanked(f.Yanked),
			CoreMetadata:   normalizeCoreMetadata(core),
		})
	}
	return files, nil
}

func parseHTML(body []byte, pageURL string) ([]simpleFile, error) {
	source := string(body)
	// asciiLower, not strings.ToLower: every offset found in this folded copy is used
	// to slice `source`, and strings.ToLower rewrites invalid UTF-8 bytes into U+FFFD.
	// That changes the byte length, so a real index served with one stray byte would
	// desynchronise the two strings and slice out of range.
	lower := asciiLower(source)
	var files []simpleFile
	for pos := 0; ; {
		rel := strings.Index(lower[pos:], "<a")
		if rel < 0 {
			break
		}
		start := pos + rel
		if start+2 < len(source) && !unicode.IsSpace(rune(source[start+2])) &&
			source[start+2] != '>' {
			pos = start + 2
			continue
		}
		tagEnd := findTagEnd(source, start+2)
		if tagEnd < 0 {
			break
		}
		attrs := parseAttributes(source[start+2 : tagEnd])
		href, ok := attrs["href"]
		if !ok || href.value == "" {
			pos = tagEnd + 1
			continue
		}
		closeRel := strings.Index(lower[tagEnd+1:], "</a")
		if closeRel < 0 {
			break
		}
		textStart := tagEnd + 1
		closeStart := textStart + closeRel
		closeEnd := strings.Index(source[closeStart:], ">")
		if closeEnd < 0 {
			break
		}
		pos = closeStart + closeEnd + 1

		resolved, fragment, err := resolveFileURL(pageURL, html.UnescapeString(href.value))
		if err != nil {
			continue
		}
		filename := strings.TrimSpace(stripTags(source[textStart:closeStart]))
		filename = html.UnescapeString(filename)
		if filename == "" {
			u, _ := url.Parse(resolved)
			filename, _ = url.PathUnescape(path.Base(u.EscapedPath()))
		}
		// A PEP 503 entry names one distribution file. Anchor text is free-form, so a
		// page can yield "/" or "a/b" here; such a name is not a filename and would
		// become a nonsense cache key and a broken link, so drop the entry.
		if !validDistributionFilename(filename) {
			continue
		}
		hashes := map[string]string{}
		addFragmentHash(hashes, fragment)
		requires := ""
		if a, ok := attrs["data-requires-python"]; ok {
			requires = html.UnescapeString(a.value)
		}
		yanked := any(false)
		if a, ok := attrs["data-yanked"]; ok {
			if a.value == "" {
				yanked = true
			} else {
				yanked = html.UnescapeString(a.value)
			}
		}
		core := any(false)
		if a, ok := attrs["data-core-metadata"]; ok {
			core = normalizeCoreString(html.UnescapeString(a.value), true)
		} else if a, ok := attrs["data-dist-info-metadata"]; ok {
			core = normalizeCoreString(html.UnescapeString(a.value), true)
		}
		files = append(files, simpleFile{
			Filename: filename, URL: resolved, Hashes: hashes,
			RequiresPython: requires, Yanked: yanked, CoreMetadata: core,
		})
	}
	return files, nil
}

type htmlAttribute struct {
	value string
}

func parseAttributes(raw string) map[string]htmlAttribute {
	out := map[string]htmlAttribute{}
	for i := 0; i < len(raw); {
		for i < len(raw) && unicode.IsSpace(rune(raw[i])) {
			i++
		}
		start := i
		for i < len(raw) && !unicode.IsSpace(rune(raw[i])) && raw[i] != '=' {
			i++
		}
		if start == i {
			i++
			continue
		}
		name := strings.ToLower(raw[start:i])
		for i < len(raw) && unicode.IsSpace(rune(raw[i])) {
			i++
		}
		value := ""
		if i < len(raw) && raw[i] == '=' {
			i++
			for i < len(raw) && unicode.IsSpace(rune(raw[i])) {
				i++
			}
			if i < len(raw) && (raw[i] == '"' || raw[i] == '\'') {
				quote := raw[i]
				i++
				start = i
				for i < len(raw) && raw[i] != quote {
					i++
				}
				value = raw[start:i]
				if i < len(raw) {
					i++
				}
			} else {
				start = i
				for i < len(raw) && !unicode.IsSpace(rune(raw[i])) {
					i++
				}
				value = raw[start:i]
			}
		}
		out[name] = htmlAttribute{value: value}
	}
	return out
}

func findTagEnd(s string, start int) int {
	var quote byte
	for i := start; i < len(s); i++ {
		switch {
		case quote != 0 && s[i] == quote:
			quote = 0
		case quote == 0 && (s[i] == '"' || s[i] == '\''):
			quote = s[i]
		case quote == 0 && s[i] == '>':
			return i
		}
	}
	return -1
}

func stripTags(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch r {
		case '<':
			inTag = true
		case '>':
			inTag = false
		default:
			if !inTag {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func resolveFileURL(pageURL, reference string) (resolved, fragment string, err error) {
	base, err := url.Parse(pageURL)
	if err != nil {
		return "", "", err
	}
	ref, err := url.Parse(reference)
	if err != nil {
		return "", "", err
	}
	full := base.ResolveReference(ref)
	// An index is remote input, including from a private mirror nobody here operates.
	// A reference carrying its own non-HTTP scheme — "javascript:", "file:", "data:" —
	// survives reference resolution intact, so reject it at the parse boundary instead
	// of carrying a URL nothing downstream can safely act on.
	if full.Scheme != "http" && full.Scheme != "https" {
		return "", "", fmt.Errorf("unsupported URL scheme %q", full.Scheme)
	}
	if full.Host == "" {
		return "", "", fmt.Errorf("URL %q has no host", full.String())
	}
	fragment = full.Fragment
	full.Fragment = ""
	full.RawFragment = ""
	return full.String(), fragment, nil
}

func addFragmentHash(hashes map[string]string, fragment string) {
	algorithm, digest, ok := strings.Cut(fragment, "=")
	if ok && algorithm != "" && digest != "" {
		hashes[strings.ToLower(algorithm)] = digest
	}
}

func normalizeYanked(raw json.RawMessage) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	var value any
	if json.Unmarshal(trimmed, &value) != nil || value == nil {
		return false
	}
	switch value.(type) {
	case bool, string:
		return value
	default:
		return false
	}
}

func normalizeCoreMetadata(raw json.RawMessage) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	var value any
	if json.Unmarshal(trimmed, &value) != nil {
		return false
	}
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return normalizeCoreString(v, true)
	case map[string]any:
		out := map[string]string{}
		for algorithm, digest := range v {
			if s, ok := digest.(string); ok && s != "" {
				out[algorithm] = s
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return false
}

func normalizeCoreString(value string, present bool) any {
	if !present {
		return false
	}
	if value == "" || strings.EqualFold(value, "true") {
		return true
	}
	if strings.EqualFold(value, "false") {
		return false
	}
	algorithm, digest, ok := strings.Cut(value, "=")
	if ok && algorithm != "" && digest != "" {
		return map[string]string{algorithm: digest}
	}
	return true
}

func renderJSON(project string, files []simpleFile, prefix string) ([]byte, error) {
	type outputFile struct {
		Filename       string            `json:"filename"`
		URL            string            `json:"url"`
		Hashes         map[string]string `json:"hashes"`
		RequiresPython any               `json:"requires-python"`
		Yanked         any               `json:"yanked"`
		CoreMetadata   any               `json:"core-metadata"`
	}
	out := struct {
		Meta  map[string]string `json:"meta"`
		Name  string            `json:"name"`
		Files []outputFile      `json:"files"`
	}{
		Meta: map[string]string{"api-version": "1.1"},
		Name: project,
	}
	for _, f := range files {
		hashes := f.Hashes
		if hashes == nil {
			hashes = map[string]string{}
		}
		var requiresPython any
		if f.RequiresPython != "" {
			requiresPython = f.RequiresPython
		}
		out.Files = append(out.Files, outputFile{
			Filename: f.Filename,
			URL:      strings.TrimRight(prefix, "/") + "/" + url.PathEscape(f.Filename),
			Hashes:   hashes, RequiresPython: requiresPython,
			Yanked: f.Yanked, CoreMetadata: f.CoreMetadata,
		})
	}
	return json.Marshal(out)
}

func renderHTML(project string, files []simpleFile, prefix string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "<!DOCTYPE html><html><head>"+
		"<meta name=\"pypi:repository-version\" content=\"1.1\">"+
		"<title>Links for %s</title></head><body><h1>Links for %s</h1>\n",
		html.EscapeString(project), html.EscapeString(project))
	for _, f := range files {
		href := strings.TrimRight(prefix, "/") + "/" + url.PathEscape(f.Filename)
		if digest := f.Hashes["sha256"]; digest != "" {
			href += "#sha256=" + digest
		}
		fmt.Fprintf(&b, "<a href=\"%s\"", html.EscapeString(href))
		if f.RequiresPython != "" {
			fmt.Fprintf(&b, " data-requires-python=\"%s\"", html.EscapeString(f.RequiresPython))
		}
		switch y := f.Yanked.(type) {
		case bool:
			if y {
				b.WriteString(" data-yanked=\"\"")
			}
		case string:
			if y != "" {
				// Preserve the retired Python contract: it exposed the PEP 592
				// reason in JSON, but HTML used the boolean-compatible empty
				// attribute. Installers only depend on attribute presence.
				b.WriteString(" data-yanked=\"\"")
			}
		}
		if value, ok := coreMetadataHTML(f.CoreMetadata); ok {
			fmt.Fprintf(&b, " data-core-metadata=\"%s\"", html.EscapeString(value))
		}
		fmt.Fprintf(&b, ">%s</a><br/>\n", html.EscapeString(f.Filename))
	}
	b.WriteString("</body></html>\n")
	return []byte(b.String())
}

func coreMetadataHTML(value any) (string, bool) {
	switch v := value.(type) {
	case bool:
		return "true", v
	case map[string]string:
		if digest := v["sha256"]; digest != "" {
			return "sha256=" + digest, true
		}
		keys := make([]string, 0, len(v))
		for algorithm := range v {
			keys = append(keys, algorithm)
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			return keys[0] + "=" + v[keys[0]], true
		}
	}
	return "", false
}

func wantsJSON(accept string) bool {
	return strings.Contains(strings.ToLower(accept), jsonMediaType)
}

func addMetadataSuffix(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL + ".metadata"
	}
	// The original encoding is kept, not rebuilt.
	//
	// Clearing RawPath told url.String to re-encode from Path, and Path is decoded — so a
	// filename containing %2B came back out with a literal '+'. Every PyTorch CUDA wheel
	// is named that way, because the local version is part of the filename:
	// torchcodec-0.16.0%2Bcu130-cp312-...whl. download.pytorch.org does not treat the two
	// spellings as the same file, so the metadata request 404'd upstream and this cache
	// answered 502 — while the wheel beside it, whose URL was never rebuilt, served fine.
	//
	// RawPath is only honoured when it is a valid encoding of Path, so both get the
	// suffix or the whole thing is ignored and we are back where we started.
	u.Path += ".metadata"
	if u.RawPath != "" {
		u.RawPath += ".metadata"
	}
	return u.String()
}

func escapeIndex(index string) string {
	parts := strings.Split(index, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

var normalizedNameRE = regexp.MustCompile(`[-_.]+`)

// NormalizeName applies PEP 503 project-name normalization.
func NormalizeName(name string) string {
	return strings.ToLower(normalizedNameRE.ReplaceAllString(name, "-"))
}

var distributionSuffixes = []string{
	".tar.gz", ".tar.bz2", ".tar.xz", ".tgz", ".zip",
}

// ParseDistributionFilename returns normalized name, version, and wheel tag.
func ParseDistributionFilename(filename string) (name, version, arch string, ok bool) {
	if strings.HasSuffix(filename, ".whl") {
		stem := strings.TrimSuffix(filename, ".whl")
		parts := strings.Split(stem, "-")
		if len(parts) < 5 || parts[0] == "" || parts[1] == "" {
			return "", "", "", false
		}
		return NormalizeName(parts[0]), parts[1], strings.Join(parts[2:], "-"), true
	}
	for _, suffix := range distributionSuffixes {
		if !strings.HasSuffix(filename, suffix) {
			continue
		}
		stem := strings.TrimSuffix(filename, suffix)
		at := strings.LastIndex(stem, "-")
		if at <= 0 || at+1 == len(stem) {
			return "", "", "", false
		}
		return NormalizeName(stem[:at]), stem[at+1:], "", true
	}
	return "", "", "", false
}

func parseArtifactKey(key string) (name, version, arch string, ok bool) {
	_, filename, found := strings.Cut(key, "/+f/")
	if !found {
		return "", "", "", false
	}
	if at := strings.LastIndex(filename, "/"); at >= 0 {
		filename = filename[at+1:]
	}
	if strings.HasSuffix(filename, ".metadata") {
		return "", "", "", false
	}
	return ParseDistributionFilename(filename)
}

func setupSteps(_ eco.SetupContext) []eco.SetupStep {
	return []eco.SetupStep{
		{Comment: "The pkgreg shell already points pip and uv at this project. Use normal commands:"},
		{Command: "python -m pip install <package>"},
		{Command: "uv pip install <package>"},
	}
}

func cloneMap[M ~map[string]string](in M) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// asciiLower folds A-Z and leaves every other byte exactly as it was, so the result is
// always the same length as the input. HTML tag and attribute names are ASCII, so this
// is all the folding the scanner needs — and unlike strings.ToLower it cannot change
// byte offsets on input that is not valid UTF-8.
func asciiLower(s string) string {
	var b []byte
	for i := range len(s) {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c + ('a' - 'A')
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

// validDistributionFilename reports whether a name can be a single distribution file.
// Rejecting separators and dot segments here keeps malformed index entries out of the
// catalog; the safety boundary itself is elsewhere (exact-match keys and validated
// digests), so this is about not publishing links that cannot work.
func validDistributionFilename(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, "/\\") && !strings.Contains(name, "\x00")
}
