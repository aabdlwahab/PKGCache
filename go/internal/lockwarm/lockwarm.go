// Package lockwarm parses uv.lock v1 files, warms every pinned registry file
// through the local PyPI adapter, and rewrites only registry and file URLs.
package lockwarm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// SupportedVersion is the uv.lock schema major understood by this parser.
const SupportedVersion = 1

var (
	versionRE  = regexp.MustCompile(`(?m)^\s*version\s*=\s*(\d+)\s*(?:#.*)?$`)
	packageRE  = regexp.MustCompile(`(?m)^\s*\[\[package\]\]\s*(?:#.*)?$`)
	nameRE     = regexp.MustCompile(`(?m)^\s*name\s*=\s*("(?:\\.|[^"])*")`)
	registryRE = regexp.MustCompile(`\bregistry\s*=\s*("(?:\\.|[^"])*")`)
	urlRE      = regexp.MustCompile(`\burl\s*=\s*("(?:\\.|[^"])*")`)
	normalize  = regexp.MustCompile(`[-_.]+`)
)

// File is one pinned distribution file.
type File struct {
	Filename string
	URL      string
}

// Package is one registry-sourced locked package and its complete file set.
type Package struct {
	Name     string
	Registry string
	Files    []File
}

// Project returns the PEP 503 normalized distribution name, which is the form the
// simple index is addressed by.
func (p Package) Project() string {
	return strings.ToLower(normalize.ReplaceAllString(p.Name, "-"))
}

// Parse reads the stable registry/file subset of uv.lock schema v1. The uv writer
// emits these fields as TOML strings; preserving the original document lets Rewrite
// remain byte-for-byte except for the URL tokens.
func Parse(text string) ([]Package, error) {
	versionMatch := versionRE.FindStringSubmatch(text)
	if len(versionMatch) != 2 {
		return nil, errors.New("lockwarm: not a valid uv.lock (missing integer version)")
	}
	version, _ := strconv.Atoi(versionMatch[1])
	if version != SupportedVersion {
		return nil, fmt.Errorf("lockwarm: unsupported uv.lock version %d (expected %d)",
			version, SupportedVersion)
	}
	locs := packageRE.FindAllStringIndex(text, -1)
	packages := make([]Package, 0, len(locs))
	for i, loc := range locs {
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		block := text[loc[1]:end]
		registryToken := registryRE.FindStringSubmatch(block)
		if len(registryToken) != 2 {
			continue
		}
		nameToken := nameRE.FindStringSubmatch(block)
		if len(nameToken) != 2 {
			return nil, errors.New("lockwarm: registry package is missing a name")
		}
		name, err := strconv.Unquote(nameToken[1])
		if err != nil || name == "" {
			return nil, errors.New("lockwarm: invalid package name")
		}
		registry, err := strconv.Unquote(registryToken[1])
		if err != nil || registry == "" {
			return nil, errors.New("lockwarm: invalid registry URL")
		}
		rawURLs := urlRE.FindAllStringSubmatch(block, -1)
		files := make([]File, 0, len(rawURLs))
		for _, raw := range rawURLs {
			fileURL, err := strconv.Unquote(raw[1])
			if err != nil {
				return nil, errors.New("lockwarm: invalid file URL")
			}
			parsed, err := url.Parse(fileURL)
			if err != nil || parsed.Scheme == "" || parsed.Host == "" {
				return nil, fmt.Errorf("lockwarm: invalid file URL %q", fileURL)
			}
			filename := path.Base(parsed.Path)
			if decoded, err := url.PathUnescape(filename); err == nil {
				filename = decoded
			}
			if filename == "" || filename == "." || filename == "/" {
				return nil, fmt.Errorf("lockwarm: file URL has no filename: %s", fileURL)
			}
			files = append(files, File{Filename: filename, URL: fileURL})
		}
		if len(files) > 0 {
			packages = append(packages, Package{Name: name, Registry: registry, Files: files})
		}
	}
	return packages, nil
}

// IndexMap translates lock registry roots to configured cache index names.
// IndexMap maps normalized upstream registry roots to local index names.
type IndexMap map[string]string

// NewIndexMap builds the lookup from configured index name to upstream root,
// normalizing trailing slashes on both sides so a lock's registry URL matches
// regardless of how the origin was written in configuration.
func NewIndexMap(indexes map[string]string) IndexMap {
	out := make(IndexMap, len(indexes))
	for index, upstream := range indexes {
		out[strings.TrimRight(upstream, "/")] = strings.Trim(index, "/")
	}
	return out
}

// Index returns the local index name serving a lock file's registry URL.
func (m IndexMap) Index(registry string) (string, bool) {
	index, ok := m[strings.TrimRight(registry, "/")]
	return index, ok
}

// Rewrite changes only quoted URL tokens and therefore preserves formatting,
// hashes, comments, ordering, and every non-registry source byte-for-byte.
func Rewrite(text string, packages []Package, indexes IndexMap, publicBase string) string {
	publicBase = strings.TrimRight(publicBase, "/")
	for _, pkg := range packages {
		index, ok := indexes.Index(pkg.Registry)
		if !ok {
			continue
		}
		text = strings.ReplaceAll(text, strconv.Quote(pkg.Registry),
			strconv.Quote(publicBase+"/"+index+"/+simple"))
		for _, file := range pkg.Files {
			target := publicBase + "/" + index + "/+f/" + pkg.Project() + "/" +
				url.PathEscape(file.Filename)
			text = strings.ReplaceAll(text, strconv.Quote(file.URL), strconv.Quote(target))
		}
	}
	return text
}

// Result reports one completed cache-warm request.
type Result struct {
	Filename string
	Status   int
	Err      error
}

// Warm requests every pinned file through the in-process data plane with bounded
// concurrency. Bodies are discarded as they stream; the engine commits each blob.
func Warm(
	ctx context.Context,
	handler http.Handler,
	project string,
	packages []Package,
	indexes IndexMap,
	workers int,
	yield func(Result),
) error {
	if handler == nil {
		return errors.New("lockwarm: data-plane handler is unavailable")
	}
	type item struct {
		index, packageName, filename string
	}
	var items []item
	for _, pkg := range packages {
		index, ok := indexes.Index(pkg.Registry)
		if !ok {
			return fmt.Errorf("lockwarm: no configured PyPI index for %s", pkg.Registry)
		}
		for _, file := range pkg.Files {
			items = append(items, item{index, pkg.Project(), file.Filename})
		}
	}
	if workers <= 0 {
		workers = 8
	}
	if workers > len(items) {
		workers = len(items)
	}
	if workers == 0 {
		return nil
	}
	queue := make(chan item)
	results := make(chan Result)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for current := range queue {
				requestPath := "/" + project + "/pypi/" + current.index + "/+f/" +
					url.PathEscape(current.packageName) + "/" + url.PathEscape(current.filename)
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://pkgreg.internal"+requestPath, http.NoBody)
				if err != nil {
					results <- Result{Filename: current.filename, Err: err}
					continue
				}
				response := &discardResponse{header: make(http.Header)}
				handler.ServeHTTP(response, request)
				status := response.status
				if status == 0 {
					status = http.StatusOK
				}
				results <- Result{Filename: current.filename, Status: status}
			}
		}()
	}
	go func() {
		defer close(queue)
		for _, current := range items {
			select {
			case queue <- current:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		group.Wait()
		close(results)
	}()
	var failed int
	for result := range results {
		if yield != nil {
			yield(result)
		}
		if result.Err != nil || result.Status != http.StatusOK {
			failed++
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("lockwarm: %d of %d files failed; lock was not rewritten",
			failed, len(items))
	}
	return nil
}

type discardResponse struct {
	header http.Header
	status int
}

// Header implements http.ResponseWriter.
func (w *discardResponse) Header() http.Header { return w.header }

// WriteHeader records the first status written and discards the rest.
func (w *discardResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *discardResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return len(p), nil
}
