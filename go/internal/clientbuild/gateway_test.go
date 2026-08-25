package clientbuild

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// -host-address used to be a flag somebody had to know about before their first pull on a
// Mac could work. These pin the derivation that replaced it.

func info(text string) DaemonInfo {
	return func(context.Context) string { return text }
}

func TestTheGatewayIsDerivedFromWhatTheDaemonIs(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, goos string
		info       DaemonInfo
		want       bool
	}{
		// No native daemon exists on either, so the platform settles it and no subprocess
		// is spent asking. The nil DaemonInfo is the assertion: it would panic if called.
		{"macOS", "darwin", nil, true},
		{"Windows", "windows", nil, true},

		// Linux with a daemon on this host, which is the case loopback was right for all
		// along: no name resolution inside the build, no docker-setup for a pull.
		{"Linux, native daemon", "linux", info("Ubuntu 24.04 laptop"), false},
		{"Linux, no daemon to ask", "linux", info(""), false},
		{"Linux, docker not installed", "linux", nil, false},

		// Docker Desktop for Linux is a virtual machine too, and is the one case on Linux
		// that loopback cannot reach.
		{"Linux, Docker Desktop", "linux", info("Docker Desktop docker-desktop"), true},
		{"case is not part of the answer", "linux", info("DOCKER DESKTOP"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeedsHostGateway(ctx, tc.goos, tc.info); got != tc.want {
				t.Fatalf("NeedsHostGateway(%q) = %v, want %v", tc.goos, got, tc.want)
			}
		})
	}
}

// The note is the difference between an address that looks wrong and one that explains
// itself, so it has to carry both the address chosen and the command that makes it work.
func TestTheGatewayNoteNamesTheAddressAndTheFix(t *testing.T) {
	note := GatewayNote(DefaultHostGateway + ":41780")
	for _, want := range []string{DefaultHostGateway + ":41780", "docker-setup", "-host-address=false"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not mention %q:\n%s", want, note)
		}
	}
}

// A pull that fails because the daemon never reached the cache must not read as a broken
// cache. This is the message somebody gets when derivation was wrong about their daemon.
func TestAnUnreachableCacheSaysWhichAddressToTryInstead(t *testing.T) {
	err := pullFailure("alpine:3.20", "127.0.0.1:41780/dockerhub/library/alpine:3.20",
		`Error response from daemon: Get "http://127.0.0.1:41780/v2/": `+
			"dial tcp 127.0.0.1:41780: connect: connection refused", errors.New("exit status 1"))
	if !strings.Contains(err.Error(), "-host-address") {
		t.Fatalf("no way out offered:\n%s", err)
	}
}

// And a daemon that reached the cache but demanded TLS is a different fix entirely: one
// file, not a different address.
func TestAPlainHTTPRefusalPointsAtDockerSetup(t *testing.T) {
	err := pullFailure("alpine:3.20", "host.docker.internal:41780/dockerhub/library/alpine:3.20",
		"Error response from daemon: Get \"https://host.docker.internal:41780/v2/\": "+
			"http: server gave HTTP response to HTTPS client", errors.New("exit status 1"))
	if !strings.Contains(err.Error(), "docker-setup") {
		t.Fatalf("no way out offered:\n%s", err)
	}
	if strings.Contains(err.Error(), "-host-address") {
		t.Errorf("offered an address change for a trust problem:\n%s", err)
	}
}

// A client that cannot reach its own daemon is not a cache-address problem, and saying so
// would send somebody to change an address that was never dialled.
func TestADeadDaemonIsNotAnAddressProblem(t *testing.T) {
	err := pullFailure("alpine:3.20", "127.0.0.1:41780/dockerhub/library/alpine:3.20",
		"Cannot connect to the Docker daemon at tcp://0.0.0.0:2375. "+
			"Is the docker daemon running? connection refused", errors.New("exit status 1"))
	if strings.Contains(err.Error(), "-host-address") {
		t.Fatalf("a dead daemon was explained as a cache address:\n%s", err)
	}
}
