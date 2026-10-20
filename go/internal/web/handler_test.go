package web

import (
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"
)

func get(t *testing.T, h *Handler, path string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	for key, values := range header {
		request.Header[key] = values
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	return response
}

// The console is compiled in with no build tag, so a plain `go build` — which is what
// `go test` uses — must already be able to serve it. This is the test that fails if
// anyone reintroduces a toolchain step.
func TestConsoleIsEmbeddedWithoutAnyBuildTag(t *testing.T) {
	t.Parallel()
	handler := New(true, MarkServer)
	files, bytes := handler.Assets()
	if files == 0 || bytes == 0 {
		t.Fatalf("nothing embedded: %d files, %d bytes", files, bytes)
	}
	for _, path := range []string{
		"/", "/console", "/tutorial",
		"/tokens.css", "/console/console.css", "/console/boot.js", "/console/views/overview.js",
	} {
		if response := get(t, handler, path, nil); response.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, response.Code)
		}
	}
}

// The console imports its own modules by absolute path. If the embedded tree and the
// paths in the HTML ever disagree the page dies with a blank screen and a console
// error nobody sees, so the graph is checked here instead.
func TestEveryConsoleImportResolves(t *testing.T) {
	t.Parallel()
	handler := New(true, MarkServer)
	pattern := regexp.MustCompile(`(?:from\s+|src=|<link[^>]*href=)["']([^"']+)["']`)

	for name, asset := range handler.assets {
		if !strings.HasSuffix(name, ".js") && !strings.HasSuffix(name, ".html") {
			continue
		}
		for _, match := range pattern.FindAllStringSubmatch(string(asset.body), -1) {
			target := match[1]
			if !strings.HasPrefix(target, "/") && !strings.HasPrefix(target, ".") {
				continue // a bare specifier would be a bug, but it is not a path
			}
			resolved := target
			if strings.HasPrefix(target, ".") {
				resolved = path.Join("/", path.Dir(name), target)
			}
			if _, ok := handler.assets[assetName(resolved)]; !ok {
				t.Errorf("%s imports %s, which is not in the embedded tree", name, target)
			}
		}
	}
}

func TestPublicSetupGuidanceKeepsTLSVerificationEnabled(t *testing.T) {
	t.Parallel()
	handler := New(true, MarkServer)
	tutorial := string(handler.assets["tutorial.html"].body)
	landing := string(handler.assets["landing.html"].body)

	for _, required := range []string{
		"Download <code>pkgcache</code>",
		`data-role="downloads"`,
		"-ca-sha256",
		`id="start"`,
		`id="commands"`,
		`id="docker"`,
		`id="persist"`,
		`id="help"`,
		// Anchors other pages link to, which a rewrite of this page has to keep.
		`id="tldr"`,
		`id="download"`,
		`id="fingerprint"`,
		`id="pkgcache"`,
		"PKGREG_CA_FILE",
		"Type <code>exit</code>",
		// Docker cannot read the shell pkgcache configured, so the page must say what
		// to pass instead. These are the two things every reader gets wrong.
		"--network=host",
		"--build-arg",
		// Telling the Docker daemon about the cache belongs to pkgcache, and trusting
		// the server's CA machine-wide to the script that embeds it. This page used to
		// spell the second out as a curl into ~/.docker/certs.d, which trusted whatever
		// answered.
		"pkgcache docker-setup",
		"setup.sh",
	} {
		if !strings.Contains(tutorial, required) {
			t.Errorf("tutorial is missing %q", required)
		}
	}
	// Every one of these turns a verified connection into an unverified one, and each
	// is a plausible-looking "fix" for a fingerprint or address mistake. The whole
	// point of the client is that none of them is ever needed.
	for _, insecure := range []string{
		"PIP_TRUSTED_HOST",
		"UV_INSECURE_HOST",
		"NPM_CONFIG_STRICT_SSL=false",
		"--no-check-certificate",
		"sslVerify false",
		"insecure-registries",
	} {
		if strings.Contains(tutorial, insecure) {
			t.Errorf("tutorial reintroduced insecure setup %q", insecure)
		}
	}
	if !strings.Contains(landing, `href="/tutorial">Start the tutorial`) ||
		!strings.Contains(landing, "<code>pkgcache shell</code>") {
		t.Error("landing page does not send new users through the pkgcache setup")
	}
	for _, registry := range []string{"$PKGREG_DOCKER_REGISTRY", "$PKGCACHE_DOCKER_REGISTRY"} {
		if strings.Contains(landing, registry) {
			t.Error("landing page presents the daemon-side Docker path as a shell command")
		}
	}
}

// pkgcache is the client now, and pkgreg-client a shim kept for old instructions. Every
// command these pages print has to be one that runs: the tutorial once told readers to
// type `pkgreg-client -docker-trust`, `--persist` and `-cache-address`, none of which
// pkgcache has, and to read $PKGREG_ variables inside a shell that clears them.
func TestSetupPagesTeachPkgcacheCommands(t *testing.T) {
	t.Parallel()
	handler := New(true, MarkServer)
	tutorial := string(handler.assets["tutorial.html"].body)
	landing := string(handler.assets["landing.html"].body)
	script := string(handler.assets["tutorial.js"].body)

	for name, page := range map[string]string{"tutorial.html": tutorial, "landing.html": landing} {
		for _, gone := range []string{
			"./pkgreg-client", `.\pkgreg-client.exe`, "pkgreg-client build",
			"-docker-trust", "--persist", "-cache-address",
			// A pkgcache session exports PKGCACHE_ and clears these.
			"$PKGREG_APT_PROXY", "$PKGREG_GIT_URL", "${PKGREG_FILES_URL}",
			"$PKGREG_DOCKER_REGISTRY", "${PKGREG_DOCKER_REGISTRY}",
			// The session sets UV_INDEX_URL, which yields to an explicit --index-url.
			"-e UV_DEFAULT_INDEX", "ARG UV_DEFAULT_INDEX",
		} {
			if strings.Contains(page, gone) {
				t.Errorf("%s still prints %q", name, gone)
			}
		}
		// setup.sh spells it --cache-ip; the single-dash form was pkgreg-client's.
		if strings.Contains(strings.ReplaceAll(page, "--cache-ip", ""), "-cache-ip") {
			t.Errorf("%s still prints -cache-ip, which setup.sh calls --cache-ip", name)
		}
	}
	for _, required := range []string{
		"pkgcache setup -server https://cache.example.com:8443",
		"-team-project global",
		"setup -no-cache",
		"./pkgcache shell",
		"pkgcache: session ready",
		"pkgcache pull",
		"pkgcache build",
		"pkgcache docker-build-setup",
		"pkgcache persist",
		"-token-file ./pkgreg.token",
		"$PKGCACHE_APT_PROXY",
		"GOPROXY",
	} {
		if !strings.Contains(tutorial, required) {
			t.Errorf("tutorial is missing %q", required)
		}
	}
	// Only pkgcache is offered for download, and ?project= names the server's project,
	// which setup spells -team-project; pkgcache's own -project is the local one.
	if !strings.Contains(script, `item.tool === "pkgcache"`) {
		t.Error("tutorial.js offers something other than pkgcache for download")
	}
	if !strings.Contains(script, `"-team-project global"`) ||
		strings.Contains(script, `split("-project global")`) {
		t.Error("tutorial.js rewrites ?project= into pkgcache's local -project")
	}
	// Seven ecosystems, and the landing page says so and shows each of them.
	if strings.Contains(landing, "Six ecosystems") || strings.Contains(landing, "six ecosystems") ||
		!strings.Contains(landing, "Seven ecosystems") || !strings.Contains(landing, "eco-go") {
		t.Error("landing page does not count or show the Go module ecosystem")
	}
}

// Both binaries serve the tutorial. On pkgcache the example address is the team's
// server, not this cache, so the page must not rewrite `setup -server` to point at the
// cache being set up, must not tell its reader to fix a server it does not have, and
// must name the product it is.
func TestTutorialKnowsWhichBinaryServesIt(t *testing.T) {
	t.Parallel()
	handler := New(true, MarkLocal)
	page := string(handler.assets["tutorial.html"].body)
	script := string(handler.assets["tutorial.js"].body)
	coords := string(handler.assets["coords.js"].body)

	if !strings.Contains(coords, "/api/v1/me") || !strings.Contains(coords, "data.product") {
		t.Error("coords.js does not learn which binary is serving the page")
	}
	if !strings.Contains(page, `data-example="team-server"`) ||
		!strings.Contains(coords, `"team-server"`) {
		t.Error("the tutorial's server example is rewritten to a pkgcache's own address")
	}
	if !strings.Contains(page, `data-product="pkgcache"`) ||
		!strings.Contains(script, `[data-product]`) {
		t.Error("the tutorial has no wording for a page served by pkgcache")
	}
	if !strings.Contains(script, `both[1] === "pkgcache"`) {
		t.Error("tutorial.js can show the server-side TLS warning on pkgcache")
	}
}

// The tutorial's start command is only useful if the address, the project and the
// fingerprint are this instance's own. The first two come from coords.js and the
// project query parameter; the fingerprint used to come from an authenticated route,
// which meant that on any instance with a login the page served a command containing
// the literal placeholder. Guard all three substitution paths.
func TestTutorialFillsInEveryPlaceholderFromPublicData(t *testing.T) {
	t.Parallel()
	handler := New(true, MarkServer)
	page := string(handler.assets["tutorial.html"].body)
	script := string(handler.assets["tutorial.js"].body)
	coords := string(handler.assets["coords.js"].body)

	if !strings.Contains(page, "PASTE_FINGERPRINT") {
		t.Fatal("tutorial has no fingerprint placeholder to fill in")
	}
	if !strings.Contains(page, "https://cache.example.com:8443") {
		t.Fatal("tutorial has no example address for coords.js to rewrite")
	}
	if !strings.Contains(coords, "window.pkgregCoordinates") {
		t.Error("coords.js no longer publishes the coordinates it fetched")
	}
	if !strings.Contains(script, "window.pkgregCoordinates") ||
		!strings.Contains(script, "ca_sha256") {
		t.Error("tutorial.js does not take the fingerprint from the public coordinates route")
	}
	if strings.Contains(script, "/endpoints") {
		t.Error("tutorial.js reads an authenticated route again; a logged-out reader " +
			"would be shown PASTE_FINGERPRINT as if it were the command")
	}
	// Assigning textContent detaches the text nodes coords.js kept references to, so
	// whichever fetch resolved second would write into a node no longer in the page.
	if strings.Contains(script, ".textContent = command.textContent") {
		t.Error("tutorial.js replaces command text nodes instead of rewriting them in place")
	}
}

// coords.js rewrites the example address to whatever scheme the server reports, so on an
// instance with no certificate pair every command on the page becomes `-server http://…`
// — which pkgreg-client refuses outright. Both public surfaces have to say so instead of
// letting the reader discover it as an error with no visible cause.
func TestSetupSurfacesFlagAnInstanceServingPlainHTTP(t *testing.T) {
	t.Parallel()
	handler := New(true, MarkServer)
	page := string(handler.assets["tutorial.html"].body)
	script := string(handler.assets["tutorial.js"].body)
	connect := string(handler.assets["console/views/connect.js"].body)

	if !strings.Contains(page, `data-role="no-tls"`) {
		t.Error("the tutorial has no plain-HTTP warning to show")
	}
	if !strings.Contains(script, `data-role="no-tls"`) ||
		!strings.Contains(script, `data.scheme !== "https"`) {
		t.Error("tutorial.js never reveals the plain-HTTP warning")
	}
	if !strings.Contains(connect, `coordinates.scheme !== "https"`) {
		t.Error("Connect still renders a start command for a server the client will refuse")
	}
	// Both must name the error the client actually prints, since that string is what a
	// stuck reader searches for.
	for name, body := range map[string]string{"tutorial.html": page, "connect.js": connect} {
		if !strings.Contains(body, "server must use https") {
			t.Errorf("%s does not connect its warning to the client's own error text", name)
		}
	}
}

func TestTutorialInstructionsRemainVisibleWithJavaScript(t *testing.T) {
	t.Parallel()
	handler := New(true, MarkServer)
	page := string(handler.assets["tutorial.html"].body)
	landingPosition := strings.Index(page, `href="/landing.css"`)
	tutorialPosition := strings.Index(page, `href="/tutorial.css"`)
	if landingPosition < 0 || tutorialPosition <= landingPosition {
		t.Fatal("tutorial.css must load after the shared landing reveal styles")
	}
	css := string(handler.assets["tutorial.css"].body)
	if !strings.Contains(css, ".rv, .js .rv") ||
		!strings.Contains(css, "opacity: 1") {
		t.Fatal("tutorial does not override the landing page's hidden reveal state")
	}
}

func TestHeadlessAnswers404OnEveryConsolePath(t *testing.T) {
	t.Parallel()
	handler := New(false, MarkServer)
	if handler.Enabled() {
		t.Fatal("handler reports enabled")
	}
	for _, path := range []string{"/", "/console", "/tutorial", "/landing.css"} {
		response := get(t, handler, path, nil)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", path, response.Code)
		}
		// The point of headless is that the machine-facing surface stays up, so the
		// body has to say where it went rather than being a bare 404.
		if !strings.Contains(response.Body.String(), "/api/v1") {
			t.Fatalf("%s body = %q", path, response.Body.String())
		}
		if got := response.Header().Get("Content-Security-Policy"); got != ContentSecurityPolicy {
			t.Fatalf("%s dropped the CSP: %q", path, got)
		}
	}
}

func TestETagRevalidationReturns304(t *testing.T) {
	t.Parallel()
	handler := New(true, MarkServer)

	first := get(t, handler, "/landing.css", nil)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	if got := first.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("cache-control = %q", got)
	}

	for name, value := range map[string]string{
		"exact":     etag,
		"weakened":  "W/" + etag,
		"in a list": `"other", ` + etag,
		"star":      "*",
	} {
		response := get(t, handler, "/landing.css", http.Header{"If-None-Match": {value}})
		if response.Code != http.StatusNotModified {
			t.Fatalf("%s: %d, want 304", name, response.Code)
		}
		if response.Body.Len() != 0 {
			t.Fatalf("%s: 304 carried %d bytes", name, response.Body.Len())
		}
	}

	stale := get(t, handler, "/landing.css", http.Header{"If-None-Match": {`"deadbeef"`}})
	if stale.Code != http.StatusOK {
		t.Fatalf("stale validator = %d, want 200", stale.Code)
	}
}

// Two different files must not share a validator, or a browser holding one in cache
// will be told the other is unchanged.
func TestETagsAreContentDistinct(t *testing.T) {
	t.Parallel()
	handler := New(true, MarkServer)
	css := get(t, handler, "/landing.css", nil).Header().Get("ETag")
	js := get(t, handler, "/landing.js", nil).Header().Get("ETag")
	if css == js {
		t.Fatalf("landing.css and landing.js share ETag %s", css)
	}
}

// mime.TypeByExtension reads /etc/mime.types, which a scratch image does not have.
// The table has to answer the same way regardless of what is installed around us.
func TestContentTypesDoNotDependOnTheHost(t *testing.T) {
	t.Parallel()
	handler := New(true, MarkServer)
	for path, want := range map[string]string{
		"/":            "text/html; charset=utf-8",
		"/landing.css": "text/css; charset=utf-8",
		"/landing.js":  "text/javascript; charset=utf-8",
	} {
		if got := get(t, handler, path, nil).Header().Get("Content-Type"); got != want {
			t.Fatalf("%s content-type = %q, want %q", path, got, want)
		}
	}
}

func TestSecurityHeadersAndMissingAssets(t *testing.T) {
	t.Parallel()
	handler := New(true, MarkServer)

	if got := get(t, handler, "/", nil).Header().Get("Content-Security-Policy"); got != ContentSecurityPolicy {
		t.Fatalf("CSP = %q", got)
	}
	// A static-looking miss must not be answered with the console shell: a mistyped
	// asset URL served back as HTML is a cache-poisoning shape.
	if got := get(t, handler, "/console/typo.js", nil).Code; got != http.StatusNotFound {
		t.Fatalf("missing asset = %d, want 404", got)
	}

	request := httptest.NewRequest(http.MethodPost, "/console", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", response.Code)
	}
}

func TestHeadCarriesLengthWithoutBody(t *testing.T) {
	t.Parallel()
	handler := New(true, MarkServer)
	request := httptest.NewRequest(http.MethodHead, "/landing.css", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if response.Body.Len() != 0 {
		t.Fatalf("HEAD returned %d bytes", response.Body.Len())
	}
	if response.Header().Get("Content-Length") == "0" {
		t.Fatal("HEAD reported zero length")
	}
}

// The pages link /favicon.svg and never name a binary, so the handler is the only place
// the two marks are told apart. A server tab showing the machine cache's mark, or no
// mark at all, is what this catches.
func TestEachBinaryServesItsOwnMark(t *testing.T) {
	t.Parallel()
	bodies := map[Mark]map[string]string{}
	for _, mark := range []Mark{MarkServer, MarkLocal} {
		handler := New(true, mark)
		bodies[mark] = map[string]string{}
		for name, contentType := range map[string]string{
			"/favicon.svg": "image/svg+xml", "/favicon.ico": "image/x-icon",
		} {
			response := get(t, handler, name, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("%s %s = %d", mark, name, response.Code)
			}
			if got := response.Header().Get("Content-Type"); got != contentType {
				t.Fatalf("%s %s Content-Type = %q, want %q", mark, name, got, contentType)
			}
			if want := handler.assets["icons/"+string(mark)+path.Ext(name)].body; response.Body.String() != string(want) {
				t.Fatalf("%s %s is not icons/%s%s", mark, name, mark, path.Ext(name))
			}
			bodies[mark][name] = response.Body.String()
		}
	}
	for _, name := range []string{"/favicon.svg", "/favicon.ico"} {
		if bodies[MarkServer][name] == bodies[MarkLocal][name] {
			t.Fatalf("%s is the same for both binaries", name)
		}
	}
}
