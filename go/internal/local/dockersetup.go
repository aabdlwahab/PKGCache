package local

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// The two commands that touch something outside the cache directory, and the only ones
// in the whole product that do.
//
// Docker is the exception to "nothing is installed". Its daemon is a separate process
// that never sees a shell's environment, and under Docker Desktop it runs in a virtual
// machine whose loopback is not the developer's — so the session's address is
// unreachable from it and no amount of environment will fix that. One file has to say
// that this machine's cache is an acceptable plain-HTTP registry.
//
// Both are reversible, both take -dry-run, and both refuse to remove anything they did
// not add. That last rule is why the daemon entries are matched by value: daemon.json
// rejects keys it does not know, so there is nowhere to record a marker, and removing
// "everything that looks like ours" would take an operator's own entry with it.

// DockerSetup describes a change to the Docker daemon's configuration.
type DockerSetup struct {
	// Address is the authority a build reaches this cache on, host:port.
	Address string
	// Mirror also registers the cache as a registry mirror, so an unmodified
	// `docker pull python:3.12` is served from it. Off by default: rerouting every pull
	// on a machine is not a side effect a setup command should have.
	Mirror bool
	// DryRun prints what would change and changes nothing.
	DryRun bool
	// Uninstall reverses a previous run.
	Uninstall bool
	// ConfigPath overrides where the daemon configuration lives. Tests set it.
	ConfigPath string
	Out        io.Writer
}

// DaemonConfigPath is where the Docker daemon reads its configuration.
//
// The split matters more than it looks. On Docker Desktop the file is under the user's
// home and needs no administrator access, which is what makes this a one-line change a
// developer can make themselves. On native Linux it is /etc/docker and needs root — and
// there it is also usually unnecessary, because the daemon already accepts loopback.
func DaemonConfigPath() (string, error) {
	if override := strings.TrimSpace(os.Getenv("PKGCACHE_DOCKER_DAEMON_CONFIG")); override != "" {
		return override, nil
	}
	switch runtime.GOOS {
	case "darwin", "windows":
		if override := strings.TrimSpace(os.Getenv("DOCKER_CONFIG")); override != "" {
			return filepath.Join(override, "daemon.json"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("local: locate the Docker configuration: %w", err)
		}
		return filepath.Join(home, ".docker", "daemon.json"), nil
	default:
		return "/etc/docker/daemon.json", nil
	}
}

// ApplyDockerSetup adds or removes this cache's entries in the daemon configuration.
func ApplyDockerSetup(setup DockerSetup) error {
	out := setup.Out
	if out == nil {
		out = os.Stdout
	}
	path := setup.ConfigPath
	if path == "" {
		resolved, err := DaemonConfigPath()
		if err != nil {
			return err
		}
		path = resolved
	}
	document, err := readJSONFile(path)
	if err != nil {
		return err
	}

	mirrorURL := "http://" + setup.Address
	insecure := stringList(document["insecure-registries"])
	mirrors := stringList(document["registry-mirrors"])

	var changes []string
	if setup.Uninstall {
		if next, removed := without(insecure, setup.Address); removed {
			insecure = next
			changes = append(changes, "remove "+setup.Address+" from insecure-registries")
		}
		if next, removed := without(mirrors, mirrorURL); removed {
			mirrors = next
			changes = append(changes, "remove "+mirrorURL+" from registry-mirrors")
		}
	} else {
		if !slices.Contains(insecure, setup.Address) {
			insecure = append(insecure, setup.Address)
			changes = append(changes, "add "+setup.Address+" to insecure-registries")
		}
		if setup.Mirror && !slices.Contains(mirrors, mirrorURL) {
			mirrors = append(mirrors, mirrorURL)
			changes = append(changes, "add "+mirrorURL+" to registry-mirrors")
		}
	}

	if len(changes) == 0 {
		fmt.Fprintf(out, "pkgcache: %s is already as requested\n", path)
		return nil
	}
	for _, change := range changes {
		fmt.Fprintf(out, "+ %s\n", change)
	}
	if setup.DryRun {
		fmt.Fprintln(out, "\nNothing was changed.")
		return nil
	}

	setList(document, "insecure-registries", insecure)
	setList(document, "registry-mirrors", mirrors)
	if err := writeJSONFile(path, document); err != nil {
		return err
	}
	fmt.Fprintf(out, "\npkgcache: wrote %s\n", path)
	fmt.Fprintln(out, "Restart the Docker daemon for this to take effect.")
	if runtime.GOOS == "linux" {
		fmt.Fprintln(out, "  sudo systemctl restart docker")
	} else {
		fmt.Fprintln(out, "  Docker Desktop → Restart")
	}
	return nil
}

// DockerBuildProxy points every `docker build` on this machine at the cache's apt/apk
// proxy.
//
// This is the only channel Docker offers for getting anything into a build the author
// did not write into their Dockerfile: HTTP_PROXY and its relatives are predefined
// build arguments, injected into every RUN with no ARG line to declare them. It covers
// apt and apk and nothing else — pip, uv and npm speak HTTPS to their upstreams, which
// through a proxy means a CONNECT tunnel this proxy does not offer.
type DockerBuildProxy struct {
	Address    string
	DryRun     bool
	Uninstall  bool
	ConfigPath string
	Out        io.Writer
}

// managedKey marks the proxy entry as ours, so uninstall removes what it installed and
// leaves a hand-written proxy alone. Unlike daemon.json, the client's config.json
// tolerates keys it does not know.
const managedKey = "pkgcacheManaged"

// ApplyDockerBuildProxy installs or removes the build proxy entry.
func ApplyDockerBuildProxy(setup DockerBuildProxy) error {
	out := setup.Out
	if out == nil {
		out = os.Stdout
	}
	path := setup.ConfigPath
	if path == "" {
		directory, err := dockerClientConfigDir()
		if err != nil {
			return err
		}
		path = filepath.Join(directory, "config.json")
	}
	document, err := readJSONFile(path)
	if err != nil {
		return err
	}
	proxies, _ := document["proxies"].(map[string]any)
	if proxies == nil {
		proxies = map[string]any{}
	}
	entry, _ := proxies["default"].(map[string]any)

	if setup.Uninstall {
		if entry == nil || entry[managedKey] != true {
			fmt.Fprintf(out, "pkgcache: nothing installed by pkgcache in %s\n", path)
			return nil
		}
		fmt.Fprintf(out, "+ remove proxies.default from %s\n", path)
		if setup.DryRun {
			fmt.Fprintln(out, "\nNothing was changed.")
			return nil
		}
		delete(proxies, "default")
		if len(proxies) == 0 {
			delete(document, "proxies")
		} else {
			document["proxies"] = proxies
		}
		if err := writeJSONFile(path, document); err != nil {
			return err
		}
		fmt.Fprintf(out, "\npkgcache: removed the build proxy from %s\n", path)
		return nil
	}

	if entry != nil && entry[managedKey] != true {
		return fmt.Errorf(
			"local: %s already has a build proxy this did not install; leave it alone or "+
				"remove it yourself", path)
	}
	proxyURL, proxyHost, err := proxyAddress(setup.Address)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "+ set proxies.default.httpProxy = %s in %s\n", proxyURL, path)
	if setup.DryRun {
		fmt.Fprintln(out, "\nNothing was changed.")
		return nil
	}
	proxies["default"] = map[string]any{
		"httpProxy": proxyURL,
		// noProxy keeps pip, uv and npm out of the proxy on their way to the cache.
		"noProxy":  "127.0.0.1,localhost," + proxyHost,
		managedKey: true,
	}
	document["proxies"] = proxies
	if err := writeJSONFile(path, document); err != nil {
		return err
	}
	fmt.Fprintf(out, "\npkgcache: wrote %s\n", path)
	fmt.Fprintln(out,
		"Every `docker build` on this machine now sends apt and apk through the cache.")
	fmt.Fprintln(out,
		"The cache must be running for those builds to work: `pkgcache limit` and "+
			"`pkgcache persist` keep it up.")
	return nil
}

func dockerClientConfigDir() (string, error) {
	if override := strings.TrimSpace(os.Getenv("DOCKER_CONFIG")); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("local: locate the Docker configuration directory: %w", err)
	}
	return filepath.Join(home, ".docker"), nil
}

// proxyAddress turns a caller's address into the proxy URL and the bare host beside it.
//
// It exists because both halves of this were wrong, and wrong in the quietest possible
// way. The URL was "http://" + Address, so an Address that already carried a scheme
// produced "http://http://host:41780" — which Docker writes to the file without complaint
// and then ignores, leaving every build reaching the internet while the configuration
// looks correct. And the host was the text before the first colon, which for that same
// input is "http", so noProxy gained a bogus entry too.
//
// Both forms are accepted rather than one refused: a caller with a URL and a caller with
// a host:port are both being reasonable, and a function that silently mangles one of them
// is the actual defect. What is refused is an address with no host at all, because there
// is no correct file to write for that.
func proxyAddress(address string) (proxyURL, host string, err error) {
	trimmed := strings.TrimSpace(address)
	if trimmed == "" {
		return "", "", errors.New("local: the build proxy needs an address")
	}
	scheme := "http"
	if parsed, parseErr := url.Parse(trimmed); parseErr == nil && parsed.Scheme != "" &&
		parsed.Host != "" {
		// Written as a URL. Only http and https mean anything to Docker's proxy setting;
		// anything else is a caller confusion worth reporting rather than reinterpreting.
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return "", "", fmt.Errorf(
				"local: %q is not a usable build proxy address; use http or https", address)
		}
		scheme = parsed.Scheme
		trimmed = parsed.Host
	}

	// SplitHostPort so an IPv6 literal keeps its brackets in the URL and loses them in
	// noProxy, which is what each of the two wants.
	host = trimmed
	if bare, _, splitErr := net.SplitHostPort(trimmed); splitErr == nil {
		host = bare
	}
	if host == "" {
		return "", "", fmt.Errorf("local: %q has no host to send builds to", address)
	}
	return scheme + "://" + trimmed, host, nil
}

// readJSONFile reads a JSON object, treating a missing file as an empty one.
func readJSONFile(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("local: read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		// Refusing is the only safe answer: rewriting a file we could not parse would
		// discard whatever the operator put in it.
		return nil, fmt.Errorf("local: %s is not valid JSON, so it will not be edited: %w",
			path, err)
	}
	if document == nil {
		document = map[string]any{}
	}
	return document, nil
}

// writeJSONFile replaces a document atomically, preserving the file's mode.
func writeJSONFile(path string, document map[string]any) error {
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("local: create %s: %w", filepath.Dir(path), err)
	}
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".pkgcache-*.json")
	if err != nil {
		return fmt.Errorf("local: write %s: %w", path, err)
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func stringList(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func setList(document map[string]any, key string, values []string) {
	if len(values) == 0 {
		delete(document, key)
		return
	}
	items := make([]any, len(values))
	for i, value := range values {
		items[i] = value
	}
	document[key] = items
}

func without(values []string, unwanted string) ([]string, bool) {
	out := make([]string, 0, len(values))
	removed := false
	for _, value := range values {
		if value == unwanted {
			removed = true
			continue
		}
		out = append(out, value)
	}
	return out, removed
}
