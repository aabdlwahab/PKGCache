package clientbuild

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
)

// Whether the Docker daemon can see this terminal's loopback, which decides the address a
// build or a pull is given.
//
// This used to be -host-address, a flag somebody had to know about before their first pull
// could work. On macOS it is needed every single time — Docker there is always a virtual
// machine, and 127.0.0.1 inside it is the machine, not the Mac — so the flag was not
// really a choice: it was a fact about the platform, spelled as a question to the user,
// and the failure for getting it wrong is `connection refused` from a daemon that never
// mentions loopback belonging to somebody else.
//
// So it is derived instead, and the flag stays as an override for the cases derivation
// cannot see.

// DaemonInfo reports what `docker info` says about the daemon, or "" when it cannot be
// asked. Injected so the decision is testable without a Docker installation.
type DaemonInfo func(context.Context) string

// NeedsHostGateway reports whether the cache has to be reached through the host gateway
// rather than through loopback.
//
// Two rules, and the first covers most of the world:
//
//   - Not Linux. There is no native Docker daemon on macOS or Windows — Docker Desktop,
//     Colima, Rancher, OrbStack and podman machine are all virtual machines with their
//     own loopback — so the gateway is always the answer, and asking `docker info` would
//     only spend a subprocess confirming what the platform already settles.
//   - Linux with Docker Desktop, which is also a virtual machine, and is the one case on
//     Linux where the daemon does not share this loopback. Only here is `docker info`
//     worth the subprocess.
//
// Everything else on Linux is a daemon on this host, where loopback is the better address:
// it needs no name resolution inside the build and no `docker-setup` for a pull.
func NeedsHostGateway(ctx context.Context, goos string, info DaemonInfo) bool {
	if goos != "linux" {
		return true
	}
	if info == nil {
		return false
	}
	return strings.Contains(strings.ToLower(info(ctx)), "docker desktop")
}

// DockerInfo asks the daemon what it is. Empty on any failure, which is read as "an
// ordinary Linux daemon" — the assumption that changes nothing for anybody whose setup
// already worked.
func DockerInfo(docker string) DaemonInfo {
	return func(ctx context.Context) string {
		if docker == "" {
			docker = "docker"
		}
		// #nosec G204 -- the command is the container client, not caller input.
		cmd := exec.Command(docker, "info", "--format", "{{.OperatingSystem}} {{.Name}}")
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return string(out)
	}
}

// GatewayDefault is NeedsHostGateway wired to this machine: what the commands use when no
// flag has settled the question.
func GatewayDefault(ctx context.Context, docker string) bool {
	return NeedsHostGateway(ctx, runtime.GOOS, DockerInfo(docker))
}

// GatewayNote is the one line a command prints when it chose the gateway itself.
//
// Said rather than done silently: the address a pull goes through is then not the one
// `pkgcache status` prints, which is worth explaining once — and it is also the sentence
// that makes the `docker-setup` instruction beside it make sense.
func GatewayNote(address string) string {
	return "pkgcache: this Docker daemon cannot see your loopback, so the cache is\n" +
		"  reached at " + address + ". `pkgcache docker-setup` (once) is what lets\n" +
		"  the daemon pull from it; -host-address=false forces loopback."
}
