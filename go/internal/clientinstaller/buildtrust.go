package clientinstaller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Docker's build-time proxy settings, installed once into the Docker client's own
// configuration.
//
// This is the only channel Docker offers for getting anything into a build that the
// author did not write into their Dockerfile: HTTP_PROXY, HTTPS_PROXY, NO_PROXY and
// their lowercase twins are predefined build arguments, injected into every RUN with
// no ARG line to declare them. Put the cache's apt/apk proxy there and `apt-get
// install` inside any build on this machine is cached, with no flags on the build and
// nothing added to the Dockerfile. It applies to `docker compose build` and to a
// colleague's Makefile too, which is exactly what a wrapper command cannot do.
//
// It covers apt and apk, and nothing else. pip, uv and npm speak HTTPS to their
// upstreams, which through a proxy means a CONNECT tunnel — see refuseHTTPSProxy.

const (
	proxyConfigFile = "config.json"
	// managedKey marks the entry as ours so uninstall removes what it installed and
	// leaves a hand-written proxy alone.
	managedKey = "pkgregManaged"
)

// buildTrust installs or removes the proxy entry in the Docker client configuration.
func buildTrust(options Options) error {
	out := options.Stdout
	if out == nil {
		out = os.Stdout
	}
	directory, err := dockerConfigDir()
	if err != nil {
		return err
	}
	path := filepath.Join(directory, proxyConfigFile)

	if options.Uninstall {
		return removeBuildTrust(out, path, options.DryRun)
	}
	proxy, err := aptProxyOrigin(options)
	if err != nil {
		return err
	}
	document, err := readDockerConfig(path)
	if err != nil {
		return err
	}
	proxies, _ := document["proxies"].(map[string]any)
	if proxies == nil {
		proxies = map[string]any{}
	}
	previous, _ := proxies["default"].(map[string]any)
	if previous != nil && previous[managedKey] != true && len(previous) > 0 {
		// Overwriting somebody's corporate proxy would break every build on the
		// machine, and they would have no reason to suspect this command.
		return fmt.Errorf(
			"%s already sets a build proxy that pkgreg did not install.\n"+
				"Merge it by hand rather than have this overwrite it:\n"+
				"  proxies.default.httpProxy = %s\n"+
				"  proxies.default.noProxy    = %s", path, proxy, noProxyList)
	}

	entry := map[string]any{
		"httpProxy": proxy,
		"noProxy":   noProxyList,
		managedKey:  true,
	}
	proxies["default"] = entry
	document["proxies"] = proxies

	if options.DryRun {
		encoded, _ := json.MarshalIndent(document, "", "  ")
		fmt.Fprintf(out, "+ write %s\n%s\n\nNothing was changed. Re-run without -dry-run to apply.\n",
			path, encoded)
		return nil
	}
	if err := writeDockerConfig(path, document); err != nil {
		return err
	}
	fmt.Fprintf(out, `pkgreg-client: docker builds on this machine now use the cache for OS packages

  proxy    %s
  written  %s

apt-get and apk inside any `+"`docker build`"+` are now served from the cache, with no
flags and nothing added to your Dockerfile — including builds started by Compose, a
Makefile or an IDE.

This does not cover pip, uv or npm: those speak HTTPS to their upstreams, which a
proxy can only tunnel, not cache. Use `+"`pkgreg-client build`"+` for those.

Remove it again with the same command plus -uninstall.
`, proxy, path)
	return nil
}

// noProxyList keeps loopback traffic away from the proxy. Without it the tools that
// are meant to reach the bridge on 127.0.0.1 would be sent through the cache's apt
// proxy instead, which cannot serve them.
const noProxyList = "127.0.0.1,localhost,::1"

func removeBuildTrust(out io.Writer, path string, dryRun bool) error {
	document, err := readDockerConfig(path)
	if err != nil {
		return err
	}
	proxies, _ := document["proxies"].(map[string]any)
	entry, _ := proxies["default"].(map[string]any)
	if entry == nil || entry[managedKey] != true {
		fmt.Fprintf(out, "pkgreg-client: nothing installed by pkgreg in %s\n", path)
		return nil
	}
	if dryRun {
		fmt.Fprintf(out, "+ remove proxies.default from %s\n\nNothing was changed.\n", path)
		return nil
	}
	delete(proxies, "default")
	if len(proxies) == 0 {
		delete(document, "proxies")
	} else {
		document["proxies"] = proxies
	}
	if err := writeDockerConfig(path, document); err != nil {
		return err
	}
	fmt.Fprintf(out, "pkgreg-client: removed the build proxy from %s\n", path)
	return nil
}

// aptProxyOrigin resolves the plain-HTTP forward proxy to advertise.
func aptProxyOrigin(options Options) (string, error) {
	if raw := strings.TrimSpace(os.Getenv("PKGREG_APT_PROXY")); raw != "" {
		return raw, nil
	}
	if options.Server == "" {
		return "", errors.New(
			"no apt proxy known: run this inside a pkgreg shell, or pass -server so the " +
				"cache can be asked for its proxy address")
	}
	base, err := url.Parse(options.Server)
	if err != nil {
		return "", fmt.Errorf("parse -server: %w", err)
	}
	// The forward proxy is plain HTTP by protocol: apt and apk cannot speak to a TLS
	// proxy at all.
	return "http://" + base.Host, nil
}

// refuseHTTPSProxy is not a function but a rule, recorded here because it is the one
// thing that must never be added to this file.
//
// Setting httpsProxy would send every HTTPS request in every build on this machine to
// the cache's forward proxy, which answers CONNECT with 405 by design (see
// internal/eco/apt). The result is not "uncached" — it is every https fetch in every
// build failing, including builds that have nothing to do with pkgreg. Only httpProxy
// is ever written.
const refuseHTTPSProxy = "httpsProxy is deliberately never set; see the comment above"

func readDockerConfig(path string) (map[string]any, error) {
	payload, err := os.ReadFile(path) // #nosec G304 -- a path this program derived itself
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(payload))) == 0 {
		return map[string]any{}, nil
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if document == nil {
		document = map[string]any{}
	}
	return document, nil
}

// writeDockerConfig replaces the file atomically. A half-written config.json would
// break every docker command on the machine, including the one that would fix it.
func writeDockerConfig(path string, document map[string]any) error {
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	temporary := path + ".pkgreg.tmp"
	if err := os.WriteFile(temporary, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", temporary, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// dockerConfigDir is where the Docker CLI keeps its own configuration, honouring
// DOCKER_CONFIG so a CI job or a test can point this somewhere harmless.
func dockerConfigDir() (string, error) {
	if override := strings.TrimSpace(os.Getenv("DOCKER_CONFIG")); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate the Docker configuration directory: %w", err)
	}
	return filepath.Join(home, ".docker"), nil
}
