package clientbuild

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A repository that does not exist arrives as "401 Unauthorized" against a loopback
// address, because Docker Hub answers 401 rather than 404 so it does not leak which
// private repositories exist. Unexplained, that reads as a broken cache — and the first
// thing anybody does is investigate the cache instead of the image name.
func TestPullFailureExplainsAnUnauthorizedAsAName(t *testing.T) {
	said := `unexpected status from HEAD request to ` +
		`http://127.0.0.1:41999/v2/dockerhub/library/mold/manifests/latest: 401 Unauthorized`
	err := pullFailure("docker.io/library/mold:latest",
		"127.0.0.1:41999/dockerhub/library/mold:latest", said, errors.New("exit status 1"))

	text := err.Error()
	for _, want := range []string{
		// Both names, because the one Docker printed is not the one that was asked for.
		"docker.io/library/mold:latest",
		"127.0.0.1:41999/dockerhub/library/mold:latest",
		"does not exist",
		"docker pull docker.io/library/mold:latest",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the failure does not mention %q:\n%s", want, text)
		}
	}
}

func TestPullFailureStaysQuietForOtherFailures(t *testing.T) {
	// A disk error or a daemon that is not running is not a naming problem, and a hint
	// about repository names would be a wrong guess stated confidently.
	err := pullFailure("alpine:3.20", "127.0.0.1:41999/dockerhub/library/alpine:3.20",
		"Cannot connect to the Docker daemon", errors.New("exit status 1"))
	if strings.Contains(err.Error(), "does not exist") {
		t.Errorf("an unrelated failure was explained as a missing repository:\n%s", err)
	}
}

func TestPullFailureLeavesAnUnmappedImageAlone(t *testing.T) {
	// Nothing was rewritten, so there is no second name to explain and Docker's own
	// message is already about the thing the person typed.
	err := pullFailure("example.com/private/x:1", "example.com/private/x:1",
		"401 Unauthorized", errors.New("exit status 1"))
	if strings.Contains(err.Error(), "through") {
		t.Errorf("an untouched reference was described as rewritten:\n%s", err)
	}
}

func TestBoundedBufferKeepsTheFirstPartAndReportsEverything(t *testing.T) {
	// A short registry error is kept whole; a progress display must not be copied twice,
	// and Write has to claim the whole length or the command sees a short write.
	var buffer boundedBuffer
	n, err := buffer.Write([]byte("401 Unauthorized"))
	if err != nil || n != len("401 Unauthorized") {
		t.Fatalf("Write = %d, %v", n, err)
	}
	big := strings.Repeat("x", 40000)
	n, _ = buffer.Write([]byte(big))
	if n != len(big) {
		t.Errorf("Write reported %d of %d; a short write stalls the command", n, len(big))
	}
	if len(buffer.String()) > 8192 {
		t.Errorf("kept %d bytes; the cap is not holding", len(buffer.String()))
	}
	if !strings.Contains(buffer.String(), "401") {
		t.Error("the first message was lost to the overflow")
	}
}

// fakeDocker is a docker that records every call and refuses what Docker refuses: a tag
// with a digest in it. FAKE_OFFLINE makes any pull that is not through the cache fail, as
// it does where the image's own registry cannot be reached.
func fakeDocker(t *testing.T) (command string, calls func() []string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$*" >> "$FAKE_LOG"
case "$1 $3" in
"tag "*@*) echo "refusing to create a tag with a digest reference" >&2; exit 1 ;;
esac
case "$1 $2" in
"pull 127.0.0.1:41780/"*) exit 0 ;;
"pull "*) [ -n "$FAKE_OFFLINE" ] && { echo "dial tcp: lookup cgr.dev: no such host" >&2; exit 1; } ;;
esac
exit 0
`
	command = filepath.Join(dir, "docker")
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_LOG", log)
	return command, func() []string {
		body, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(body)), "\n")
	}
}

const pinnedMinio = "cgr.dev/chainguard/minio@sha256:7df55d002b698e4494a131b848dd553361bbe58b7e19f62d9623c29af1ba9ccf"

// A digest pin cannot be tagged back, so the pinned name is pulled from the image's own
// registry once the cache has supplied the content, and the cache's address is dropped.
// Tagging it used to fail the whole pull, and with it any build that named the pin.
func TestPullNamesADigestPinByPullingItUnderThatName(t *testing.T) {
	docker, calls := fakeDocker(t)
	if err := Pull(t.Context(), pinnedMinio, PullOptions{Registry: "127.0.0.1:41780", Docker: docker}); err != nil {
		t.Fatal(err)
	}
	mapped := "127.0.0.1:41780/" + pinnedMinio
	want := []string{"pull " + mapped, "pull " + pinnedMinio, "rmi " + mapped}
	if got := calls(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("docker was asked:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// Where the image's own registry cannot be reached the image is still here, under the
// cache's address, and that name is kept and given rather than untagged into nothing.
func TestPinnedPullThatCannotBeNamedKeepsTheCachesName(t *testing.T) {
	docker, calls := fakeDocker(t)
	t.Setenv("FAKE_OFFLINE", "1")
	err := Pull(t.Context(), pinnedMinio, PullOptions{Registry: "127.0.0.1:41780", Docker: docker})
	mapped := "127.0.0.1:41780/" + pinnedMinio
	if err == nil || !strings.Contains(err.Error(), "the image is here as "+mapped) {
		t.Fatalf("err = %v", err)
	}
	for _, call := range calls() {
		if strings.HasPrefix(call, "rmi ") || strings.HasPrefix(call, "tag ") {
			t.Fatalf("the only name the image has was touched: %q", call)
		}
	}
}

// A tag still goes back the way it always has: one pull, through the cache.
func TestPullTagsATagBack(t *testing.T) {
	docker, calls := fakeDocker(t)
	if err := Pull(t.Context(), "cgr.dev/chainguard/minio:latest", PullOptions{Registry: "127.0.0.1:41780", Docker: docker}); err != nil {
		t.Fatal(err)
	}
	mapped := "127.0.0.1:41780/cgr.dev/chainguard/minio:latest"
	want := []string{"pull " + mapped, "tag " + mapped + " cgr.dev/chainguard/minio:latest", "rmi " + mapped}
	if got := calls(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("docker was asked:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}
