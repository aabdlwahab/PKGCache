package clientinstaller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Docker trust is its own mode because Docker is the one tool a temporary session cannot
// reach and machine-wide setup is too big a hammer for.
//
// The session works by setting environment variables in a child shell. The Docker daemon
// is a separate long-running process that never sees them, and on macOS and Windows it
// runs in a virtual machine whose loopback interface is not the developer's — so the
// bridge address the session exports is unreachable and a pull fails with
// "connection refused". The fix is to pull from the cache's own address, which needs the
// daemon to trust the cache's CA.
//
// --persist does that, along with the OS trust store, system-wide tool settings and a
// hosts entry, all of which need root. On a laptop that is the wrong trade: the developer
// wants one registry to work, not a managed host. Docker reads a per-registry CA from a
// directory it owns under the user's home on Docker Desktop, so this mode writes exactly
// one file, needs no administrator access, and is removed by deleting it.
//
// The alternative was leaving this as documentation — three shell commands in the
// tutorial. That is what it was, and it re-implemented by hand the one thing this client
// exists to do: fetch the CA and check it against the operator's fingerprint before
// trusting it. A copy-pasted curl skips the check.

// dockerConfigEnv is Docker's own override for the directory holding its client
// configuration, which under Docker Desktop is where certs.d lives. Honouring it keeps
// this consistent with what the docker CLI itself would use, and gives tests a seam that
// needs no flag. It applies only where certs.d really is client-side — see
// dockerCertsDir.
const dockerConfigEnv = "DOCKER_CONFIG"

// dockerTrust applies or removes per-registry trust for one cache.
func dockerTrust(ctx context.Context, options Options) error {
	if options.Print {
		return errors.New("-print shows the persistent setup script; use it with -persist")
	}
	if options.Host != "" || options.CacheIP != "" {
		return errors.New(
			"-host and -cache-ip change persistent machine setup; use them with -persist")
	}
	if options.TokenFile != "" {
		return errors.New(
			"-token-file is for temporary bridge sessions; Docker trust stores no token")
	}

	base, err := parseServer(options.Server)
	if err != nil {
		return err
	}
	authority := base.Host
	// The authority becomes a directory name. parseServer already rejects paths,
	// credentials and whitespace, so this cannot currently fire — it is here because a
	// future change to that parser must not silently turn into a path traversal.
	if authority == "" || strings.ContainsAny(authority, `/\`) ||
		authority == "." || authority == ".." {
		return fmt.Errorf("server authority %q cannot name a directory", authority)
	}

	goos := options.OperatingSystem
	if goos == "" {
		goos = runtime.GOOS
	}
	directory, root, err := dockerCertsDir(goos)
	if err != nil {
		return err
	}
	target := filepath.Join(directory, authority, "ca.crt")

	out := options.Stdout
	if out == nil {
		out = os.Stdout
	}

	if options.Uninstall {
		return removeDockerTrust(out, target, options.DryRun)
	}

	// Fetched and pinned before anything is written: the point of this mode over a
	// hand-copied curl is that the bytes are checked against the operator's fingerprint.
	trust, err := fetchTrust(ctx, options)
	if err != nil {
		return err
	}
	if options.DryRun {
		_, _ = fmt.Fprintf(out, `+ write %s (mode 0644, %d bytes)
+ CA SHA-256 %s

Nothing was changed. Re-run without -dry-run to apply.
`, target, len(trust.caPEM), trust.fingerprint)
		return nil
	}
	if err := writeDockerCA(target, trust.caPEM); err != nil {
		return err
	}

	prefix := "dockerhub/library/alpine:3.20"
	if trust.project != "global" {
		prefix = trust.project + "/" + prefix
	}
	_, _ = fmt.Fprintf(out, `pkgreg-client: Docker now trusts %s

  CA SHA-256  %s
  installed   %s
%s
Restart Docker so the daemon rereads that directory, then pull from the cache
directly — this address is stable, so it can go in a Dockerfile or a Compose file:

  docker pull %s/%s

Remove it again with the same command plus -uninstall.
`, authority, trust.fingerprint, target, rootNote(root), authority, prefix)
	return nil
}

// rootNote warns when the target is a system path. Only Linux daemons read one, and a
// developer who has to reach for sudo should be told why before they wonder.
func rootNote(root bool) string {
	if !root {
		return ""
	}
	return "\nThis is a system path because a Linux daemon reads only /etc/docker/certs.d.\n" +
		"If it failed with a permission error, re-run it with sudo, or use --persist to\n" +
		"configure the whole host at once.\n"
}

// dockerCertsDir reports where this platform's Docker daemon reads per-registry
// certificates, and whether that location is a system path.
//
// The split is real and got the shipped setup script wrong: it wrote
// /etc/docker/certs.d on every platform, including macOS, where Docker Desktop does not
// read it — verified against Docker Desktop 29.6.2 on darwin/arm64, where a CA placed
// under ~/.docker/certs.d made a pull succeed.
//
// Windows has no such directory to write. A per-registry path there would have to be
// named "<host>:<port>", and a colon cannot appear in a Windows path — so Docker Desktop
// for Windows takes registry trust from the Windows certificate store instead, which is
// what --persist populates.
func dockerCertsDir(goos string) (directory string, root bool, err error) {
	switch goos {
	case "darwin":
		if override := strings.TrimSpace(os.Getenv(dockerConfigEnv)); override != "" {
			return filepath.Join(override, "certs.d"), false, nil
		}
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", false, fmt.Errorf("locate the Docker configuration directory: %w", homeErr)
		}
		return filepath.Join(home, ".docker", "certs.d"), false, nil
	case "windows":
		return "", false, errors.New(
			"on Windows, Docker Desktop reads registry trust from the certificate store " +
				"rather than a per-registry directory — and a path named \"<host>:<port>\" is " +
				"not expressible there.\nUse --persist, which imports the CA into " +
				"LocalMachine\\Root from an Administrator PowerShell")
	case "linux":
		// DOCKER_CONFIG is deliberately ignored here. It names the *client's* config
		// directory, which is where certs.d lives under Docker Desktop but not on Linux:
		// there the daemon reads /etc/docker/certs.d and nothing else. Honouring the
		// variable would write a file dockerd never opens, report success, and leave the
		// pull failing — the same silent-no-op this whole change exists to remove.
		return "/etc/docker/certs.d", true, nil
	default:
		return "", false, fmt.Errorf("unsupported operating system %q", goos)
	}
}

// writeDockerCA installs the certificate through a temporary file and a rename, so a
// daemon reading the directory never sees a partial certificate.
func writeDockerCA(target string, caPEM []byte) error {
	directory := filepath.Dir(target)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", directory, err)
	}
	temporary, err := os.CreateTemp(directory, ".ca-*.crt")
	if err != nil {
		return fmt.Errorf("stage %s: %w", target, err)
	}
	staged := temporary.Name()
	defer func() { _ = os.Remove(staged) }()
	if _, err := temporary.Write(caPEM); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write %s: %w", target, err)
	}
	// A public certificate: world-readable on purpose, because the daemon may run as
	// another user than the developer who installed it.
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(staged, target)
}

// removeDockerTrust deletes the certificate and the directory that held it, and nothing
// else: sibling directories belong to other registries.
func removeDockerTrust(out io.Writer, target string, dryRun bool) error {
	directory := filepath.Dir(target)
	if dryRun {
		_, _ = fmt.Fprintf(out, "+ remove %s\n+ remove %s if empty\n\nNothing was changed.\n",
			target, directory)
		return nil
	}
	switch err := os.Remove(target); {
	case errors.Is(err, os.ErrNotExist):
		_, _ = fmt.Fprintf(out, "pkgreg-client: nothing to remove at %s\n", target)
		return nil
	case err != nil:
		return fmt.Errorf("remove %s: %w", target, err)
	}
	// Best-effort: fails harmlessly when the operator keeps other files beside it.
	_ = os.Remove(directory)
	_, _ = fmt.Fprintf(out,
		"pkgreg-client: removed %s\nRestart Docker so the daemon rereads that directory.\n",
		target)
	return nil
}
