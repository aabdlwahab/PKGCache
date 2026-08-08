package clientbuild

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func session(t *testing.T) (Options, *[]string) {
	t.Helper()
	var invoked []string
	return Options{
		Bridge: "http://127.0.0.1:41999", Registry: "127.0.0.1:41999",
		Project: "global", AptProxy: "http://cache:3142",
		Server: "https://cache:8443", CAFile: "/tmp/ca.crt",
		GitHosts: []string{"github.com"},
		Stdout:   &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		Runner: func(_ context.Context, name string, args []string) error {
			invoked = append([]string{name}, args...)
			return nil
		},
	}, &invoked
}

func project(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBuildPassesUserFlagsThroughUntouched(t *testing.T) {
	options, invoked := session(t)
	dir := project(t, "FROM alpine\nRUN true\n")

	if err := Build(context.Background(), options,
		[]string{"-t", "app:dev", "--label", "a=b", dir}); err != nil {
		t.Fatal(err)
	}
	line := strings.Join(*invoked, " ")
	for _, want := range []string{"-t app:dev", "--label a=b", "--network=host", "-f ", dir} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in: %s", want, line)
		}
	}
}

// Bridge mode is why a Linux build needs no certificate at all.
func TestBridgeBuildAsksForNoSecret(t *testing.T) {
	options, invoked := session(t)
	if err := Build(context.Background(), options,
		[]string{project(t, "FROM alpine\nRUN pip install six\n")}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(*invoked, " "), "--secret") {
		t.Fatalf("bridge build passed a secret: %v", *invoked)
	}
}

func TestCacheAddressBuildMountsTheCA(t *testing.T) {
	options, invoked := session(t)
	options.CacheAddress = true
	if err := Build(context.Background(), options,
		[]string{project(t, "FROM alpine\nRUN pip install six\n")}); err != nil {
		t.Fatal(err)
	}
	line := strings.Join(*invoked, " ")
	if !strings.Contains(line, "--secret id=pkgreg-ca,src=/tmp/ca.crt") {
		t.Fatalf("CA not passed as a build secret: %s", line)
	}
	// Host networking buys nothing when the address is the cache's own.
	if strings.Contains(line, "--network=host") {
		t.Fatalf("cache-address build forced host networking: %s", line)
	}
}

func TestGeneratedDockerfileLivesOutsideTheBuildContext(t *testing.T) {
	options, invoked := session(t)
	dir := project(t, "FROM alpine\n")
	if err := Build(context.Background(), options, []string{dir}); err != nil {
		t.Fatal(err)
	}
	var generated string
	for i, argument := range *invoked {
		if argument == "-f" && i+1 < len(*invoked) {
			generated = (*invoked)[i+1]
		}
	}
	if generated == "" {
		t.Fatal("no -f passed to docker")
	}
	// A COPY . . would sweep it into the image if it sat in the context.
	if strings.HasPrefix(generated, dir) {
		t.Fatalf("generated file %q is inside the build context %q", generated, dir)
	}
}

func TestGeneratedFileIsRemovedAfterTheBuild(t *testing.T) {
	options, invoked := session(t)
	if err := Build(context.Background(), options,
		[]string{project(t, "FROM alpine\n")}); err != nil {
		t.Fatal(err)
	}
	for i, argument := range *invoked {
		if argument != "-f" || i+1 >= len(*invoked) {
			continue
		}
		if _, err := os.Stat((*invoked)[i+1]); !os.IsNotExist(err) {
			t.Fatalf("generated file survived the build: %v", err)
		}
	}
}

func TestUserSuppliedDockerfileFlagIsHonoured(t *testing.T) {
	options, _ := session(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "Dockerfile.prod")
	if err := os.WriteFile(path, []byte("FROM python:3.12-alpine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	buffer := &bytes.Buffer{}
	options.Stdout, options.Print = buffer, true
	if err := Build(context.Background(), options, []string{"-f", path, dir}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buffer.String(), "dockerhub/library/python:3.12-alpine") {
		t.Fatalf("named Dockerfile not used:\n%s", buffer.String())
	}
}

// A tool that rewrites your build must be able to show you the result.
func TestPrintShowsTheGeneratedFileAndBuildsNothing(t *testing.T) {
	options, invoked := session(t)
	buffer := &bytes.Buffer{}
	options.Stdout, options.Print = buffer, true
	if err := Build(context.Background(), options,
		[]string{project(t, "FROM alpine\nRUN true\n")}); err != nil {
		t.Fatal(err)
	}
	if len(*invoked) != 0 {
		t.Fatalf("print ran docker: %v", *invoked)
	}
	if !strings.Contains(buffer.String(), "ARG PIP_INDEX_URL=") {
		t.Fatalf("print produced no rewrite:\n%s", buffer.String())
	}
}

// Outside a pkgreg shell there is no bridge, and the message has to say what to do
// rather than name a variable nobody set on purpose.
func TestMissingSessionExplainsItself(t *testing.T) {
	options, _ := session(t)
	options.Bridge = ""
	err := Build(context.Background(), options, []string{project(t, "FROM alpine\n")})
	if err == nil {
		t.Fatal("built with no session")
	}
	for _, want := range []string{"pkgreg-client", "-cache-address"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message does not mention %q: %v", want, err)
		}
	}
}

func TestCacheAddressWithoutCAIsRefusedBeforeBuilding(t *testing.T) {
	options, invoked := session(t)
	options.CacheAddress, options.CAFile = true, ""
	if err := Build(context.Background(), options,
		[]string{project(t, "FROM alpine\n")}); err == nil {
		t.Fatal("built without the CA it would need")
	}
	if len(*invoked) != 0 {
		t.Fatalf("docker was invoked anyway: %v", *invoked)
	}
}

// Compose renders a different project if the selection flags do not reach `config`.
func TestComposeSelectionFlagsAreForwardedToConfig(t *testing.T) {
	got := selectionFlags([]string{
		"--profile", "dev", "-f", "compose.yaml", "--env-file=.env.ci",
		"build", "--no-cache", "app",
	})
	want := []string{"--profile", "dev", "-f", "compose.yaml", "--env-file=.env.ci"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("selection flags = %v, want %v", got, want)
	}
	// Build flags must not leak into `config`, which would reject them.
	for _, forbidden := range got {
		if forbidden == "--no-cache" || forbidden == "build" {
			t.Fatalf("build argument forwarded to config: %v", got)
		}
	}
}

func TestContextDirectorySkipsFlagValues(t *testing.T) {
	// "app:dev" is the value of -t, not the build context; reading a Dockerfile
	// beside it would fail with a baffling path.
	if got := contextDir([]string{"-t", "app:dev", "--platform", "linux/arm64", "./src"}); got != "./src" {
		t.Fatalf("context = %q, want ./src", got)
	}
	if got := contextDir([]string{"--tag=app:dev", "."}); got != "." {
		t.Fatalf("context = %q, want .", got)
	}
}

func TestEnvironmentSuppliesTheSession(t *testing.T) {
	t.Setenv("PKGREG_BRIDGE_URL", "http://127.0.0.1:9")
	t.Setenv("PKGREG_PROJECT", "team-a")
	t.Setenv("PKGREG_APT_PROXY", "http://cache:3142")
	options := FromEnvironment(Options{})
	if options.Bridge != "http://127.0.0.1:9" || options.Project != "team-a" {
		t.Fatalf("options = %+v", options)
	}
	if len(options.GitHosts) == 0 {
		t.Fatal("git rewriting silently disabled by default")
	}
}
