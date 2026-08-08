package dockerfile

import (
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The rendered form `docker compose config` produces: absolute context, resolved
// interpolation, canonical keys.
const rendered = `
name: demo
services:
  db:
    image: postgres:16-alpine
  cache:
    image: redis:7
  app:
    build:
      context: /work/app
      dockerfile: Dockerfile
      args:
        APP_VERSION: "1.0"
    image: myteam/app:dev
`

func readStub(t *testing.T, body string) func(string) ([]byte, error) {
	t.Helper()
	return func(path string) ([]byte, error) {
		if !strings.HasPrefix(path, "/work/app") {
			return nil, fmt.Errorf("unexpected path %q", path)
		}
		return []byte(body), nil
	}
}

func decode(t *testing.T, out []byte) map[string]any {
	t.Helper()
	var document map[string]any
	if err := yaml.Unmarshal(out, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func service(t *testing.T, document map[string]any, name string) map[string]any {
	t.Helper()
	services, _ := document["services"].(map[string]any)
	entry, ok := services[name].(map[string]any)
	if !ok {
		t.Fatalf("no service %q", name)
	}
	return entry
}

func TestPullOnlyServiceImagesAreRedirected(t *testing.T) {
	result, err := RewriteCompose([]byte(rendered), bridge(), readStub(t, "FROM alpine\n"))
	if err != nil {
		t.Fatal(err)
	}
	document := decode(t, result.Content)
	for name, want := range map[string]string{
		"db":    "127.0.0.1:41999/dockerhub/library/postgres:16-alpine",
		"cache": "127.0.0.1:41999/dockerhub/library/redis:7",
	} {
		if got := service(t, document, name)["image"]; got != want {
			t.Errorf("%s image = %v, want %v", name, got, want)
		}
	}
}

// A service with both build and image uses image as the *output* tag. Rewriting it
// renames what the developer just built, and they find out at `docker run`.
func TestBuiltServiceKeepsItsOutputTag(t *testing.T) {
	result, err := RewriteCompose([]byte(rendered), bridge(), readStub(t, "FROM alpine\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := service(t, decode(t, result.Content), "app")["image"]; got != "myteam/app:dev" {
		t.Fatalf("build output tag was rewritten to %v", got)
	}
}

func TestBuildGetsHostNetworkAndARewrittenDockerfile(t *testing.T) {
	result, err := RewriteCompose([]byte(rendered), bridge(),
		readStub(t, "FROM python:3.12-alpine\nRUN pip install six\n"))
	if err != nil {
		t.Fatal(err)
	}
	build, _ := service(t, decode(t, result.Content), "app")["build"].(map[string]any)
	if build["network"] != "host" {
		t.Fatalf("build.network = %v, want host — RUN cannot reach the bridge without it", build["network"])
	}
	// The author's own build args must survive.
	args, _ := build["args"].(map[string]any)
	if args["APP_VERSION"] != "1.0" {
		t.Fatalf("author's build args lost: %v", args)
	}
	generated, ok := result.Dockerfiles["app"]
	if !ok {
		t.Fatal("no rewritten Dockerfile produced for app")
	}
	if !strings.Contains(string(generated.Content), "ARG PIP_INDEX_URL=") {
		t.Fatalf("Dockerfile not rewritten:\n%s", generated.Content)
	}
	if !strings.Contains(string(generated.Content), "127.0.0.1:41999/dockerhub/library/python:3.12-alpine") {
		t.Fatalf("FROM not rewritten:\n%s", generated.Content)
	}
}

func TestDockerfilePathIsResolvedAgainstTheContext(t *testing.T) {
	document := `
services:
  app:
    build:
      context: /work/app
      dockerfile: docker/Dockerfile.prod
`
	var seen string
	_, err := RewriteCompose([]byte(document), bridge(), func(path string) ([]byte, error) {
		seen = path
		return []byte("FROM alpine\n"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != "/work/app/docker/Dockerfile.prod" {
		t.Fatalf("resolved %q", seen)
	}
}

func TestCacheAddressModeLeavesTheBuildNetworkAlone(t *testing.T) {
	options := bridge()
	options.Mode = CacheAddress
	options.Registry = "cache:8443"
	options.Base = "https://cache:8443"

	result, err := RewriteCompose([]byte(rendered), options,
		readStub(t, "FROM alpine\nRUN pip install six\n"))
	if err != nil {
		t.Fatal(err)
	}
	build, _ := service(t, decode(t, result.Content), "app")["build"].(map[string]any)
	if _, set := build["network"]; set {
		t.Fatal("host networking forced in cache-address mode, where it buys nothing")
	}
	if !result.Dockerfiles["app"].NeedsSecret {
		t.Fatal("cache-address build did not report needing the CA secret")
	}
}

func TestSetComposeDockerfilePointsTheBuildAtTheGeneratedFile(t *testing.T) {
	out, err := SetComposeDockerfile([]byte(rendered), "app", "/tmp/pkgreg-x.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	build, _ := service(t, decode(t, out), "app")["build"].(map[string]any)
	if build["dockerfile"] != "/tmp/pkgreg-x.Dockerfile" {
		t.Fatalf("dockerfile = %v", build["dockerfile"])
	}
}

func TestUnreadableDockerfileIsReportedWithItsService(t *testing.T) {
	_, err := RewriteCompose([]byte(rendered), bridge(), func(string) ([]byte, error) {
		return nil, fmt.Errorf("no such file")
	})
	if err == nil || !strings.Contains(err.Error(), "app") {
		t.Fatalf("error should name the service: %v", err)
	}
}
