package clientbuild

import (
	"errors"
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
