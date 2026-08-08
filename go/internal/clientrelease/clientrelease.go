// Package clientrelease owns what a published client release looks like on disk.
//
// Client binaries are served from disk rather than embedded in the server. Embedding
// was the obvious idea and the wrong one: the client is ~7 MB per platform and there
// are five, which would nearly double a 27 MB server whose entire selling point is
// being one lean static binary — and would do it for every operator, whether or not
// anyone ever downloads one.
//
// So publishing is an explicit operator step, and this package is the single place
// that knows what that step produces: the filename grammar, the checksum file format,
// and which platforms a complete release covers. Before it existed the grammar was a
// regexp in the control API, a hand-written list in `pkgreg doctor`, and a third list
// in the Makefile — three copies that had to be edited together and silently would
// not be.
package clientrelease

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// DirName is the subdirectory of the data dir that published binaries live in.
const DirName = "downloads"

// name is the only shape a published filename may take. An allowlist rather than a
// traversal check: these files are produced by our own release build, so anything not
// matching this was not put there by the process we describe.
var name = regexp.MustCompile(
	`^pkgreg-(client|bridge)-(linux|darwin|windows)-(amd64|arm64)(\.exe)?$`)

// Binary is one publishable or published file.
type Binary struct {
	Name   string `json:"name"`
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Tool   string `json:"tool"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256,omitempty"`
}

// Platform is the label a human recognises: "Linux amd64", "macOS arm64".
func (b Binary) Platform() string {
	pretty := map[string]string{"linux": "Linux", "darwin": "macOS", "windows": "Windows"}
	label := pretty[b.OS]
	if label == "" {
		label = b.OS
	}
	return label + " " + b.Arch
}

// Dir is where a data directory keeps published binaries.
func Dir(dataDir string) string { return filepath.Join(dataDir, DirName) }

// Parse reports whether a filename is publishable, and decomposes it if so. The
// caller gets no size or checksum: those describe a file, and this describes a name.
func Parse(filename string) (Binary, bool) {
	if !name.MatchString(filename) {
		return Binary{}, false
	}
	// Safe by construction: the pattern above fixed the field count.
	parts := strings.Split(strings.TrimSuffix(filename, ".exe"), "-")
	return Binary{Name: filename, Tool: parts[1], OS: parts[2], Arch: parts[3]}, true
}

// ClientPlatforms is what a complete client release covers, in the order a human
// reads them. It matches what `make client-release` builds: Windows is amd64 only,
// which ARM Windows runs under emulation.
func ClientPlatforms() []string {
	return []string{
		"pkgreg-client-linux-amd64",
		"pkgreg-client-linux-arm64",
		"pkgreg-client-darwin-amd64",
		"pkgreg-client-darwin-arm64",
		"pkgreg-client-windows-amd64.exe",
	}
}

// ChecksumsFile is the sums file for one tool: pkgreg-client-SHA256SUMS.
func ChecksumsFile(tool string) string { return "pkgreg-" + tool + "-SHA256SUMS" }

// List reports what a directory currently offers, newest checksums included. A
// missing directory is the normal state of a fresh instance, so it lists as empty
// rather than failing.
func List(dir string) ([]Binary, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sums := Checksums(dir)
	out := make([]Binary, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		binary, ok := Parse(entry.Name())
		if !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			// Vanished between ReadDir and Info. It is not published; that is all a
			// listing has to say about it.
			continue
		}
		binary.Bytes = info.Size()
		binary.SHA256 = sums[binary.Name]
		out = append(out, binary)
	}
	Sort(out)
	return out, nil
}

// Sort puts binaries in a stable, human order so a page does not reshuffle between
// loads.
func Sort(list []Binary) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Tool != list[j].Tool {
			return list[i].Tool < list[j].Tool
		}
		if list[i].OS != list[j].OS {
			return list[i].OS < list[j].OS
		}
		return list[i].Arch < list[j].Arch
	})
}

// Checksums parses whichever sums files a directory holds, so a caller can verify a
// download without a second request. Absent files are simply absent: a published
// binary with no recorded digest is a partial publish, not an error to parse.
func Checksums(dir string) map[string]string {
	sums := map[string]string{}
	for _, tool := range []string{"client", "bridge"} {
		body, err := os.ReadFile(filepath.Join(dir, ChecksumsFile(tool)))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			// sha256sum writes "<digest>  <name>", with the name possibly "*name" when
			// it was read in binary mode. Accept both: operators do run sha256sum.
			sums[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
		}
	}
	return sums
}

// Digest is the lowercase hex SHA-256 of a file.
func Digest(path string) (string, error) {
	file, err := os.Open(path) // #nosec G304 -- callers pass paths they were given by an operator.
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// archiveSuffixes are the wrappers a platform release travels in. macOS notarization
// in particular submits and returns a .zip, because notarytool takes an archive rather
// than a bare executable.
var archiveSuffixes = []string{".zip", ".tar.gz", ".tgz", ".gz"}

// NearMiss reports a file that names a publishable binary but arrived inside an
// archive, and returns the name it would have once extracted.
//
// This exists because the answer "nothing here is publishable" was actively
// misleading for the one input an operator is most likely to have: the official
// release. The signed macOS artifacts were published as pkgreg-client-darwin-arm64.zip,
// which Parse rejects, so `publish-client` silently skipped them and reported three of
// five platforms — leaving macOS developers with no download and no clue why. The
// release now ships bare binaries, but an operator can still be holding an older
// archive, and telling them exactly what to do with it costs four lines.
func NearMiss(filename string) (inner string, ok bool) {
	for _, suffix := range archiveSuffixes {
		if !strings.HasSuffix(strings.ToLower(filename), suffix) {
			continue
		}
		candidate := filename[:len(filename)-len(suffix)]
		if _, parsed := Parse(candidate); parsed {
			return candidate, true
		}
	}
	return "", false
}

// CollectNearMisses lists archived binaries among the given paths, as
// "archive -> extracted name" pairs, sorted for a stable report.
func CollectNearMisses(paths []string) []string {
	var out []string
	note := func(name string) {
		if inner, ok := NearMiss(name); ok {
			out = append(out, name+" -> "+inner)
		}
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			note(filepath.Base(path))
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				note(entry.Name())
			}
		}
	}
	sort.Strings(out)
	return out
}

// Collect finds publishable files among the given paths. A path may be a file, which
// must be publishably named, or a directory, which is scanned one level deep. Later
// paths win, so `publish-client old/ new/pkgreg-client-linux-amd64` does what it
// looks like.
//
// The returned map is keyed by published filename and valued by source path.
func Collect(paths []string) (map[string]string, error) {
	found := map[string]string{}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			base := filepath.Base(path)
			if _, ok := Parse(base); !ok {
				if inner, archived := NearMiss(base); archived {
					return nil, fmt.Errorf(
						"%s is an archive, not a binary: extract it to %s first",
						base, inner)
				}
				return nil, fmt.Errorf("%s is not a publishable name: expected one of %s",
					base, strings.Join(ClientPlatforms(), ", "))
			}
			found[base] = path
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if _, ok := Parse(entry.Name()); ok {
				found[entry.Name()] = filepath.Join(path, entry.Name())
			}
		}
	}
	return found, nil
}

// Publish copies the given source files into dir and rewrites the checksum files to
// match what dir then holds.
//
// Two properties matter more than speed here. Every file is written to a temporary
// name and renamed into place, so a running server never serves a half-copied binary
// — a client that downloads one gets an executable that fails to run, with nothing to
// suggest why. And the sums files are regenerated from the whole directory rather than
// appended to, so publishing one platform cannot leave stale digests behind for the
// others.
//
// When a source directory carries its own sums file, every file taken from it is
// verified against that file first. That is the guarantee the old
// `sha256sum -c` step in the Makefile provided, and a corrupted copy onto an air-gapped
// host is exactly the failure it catches.
func Publish(dir string, sources map[string]string) ([]Binary, error) {
	if len(sources) == 0 {
		return nil, errors.New("no publishable client binaries found")
	}
	if err := verifySources(sources); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}

	names := make([]string, 0, len(sources))
	for filename := range sources {
		names = append(names, filename)
	}
	sort.Strings(names)
	for _, filename := range names {
		if err := install(sources[filename], filepath.Join(dir, filename)); err != nil {
			return nil, err
		}
	}
	if err := writeChecksums(dir); err != nil {
		return nil, err
	}
	return List(dir)
}

// verifySources checks each source against a sums file sitting beside it, when there
// is one. Sources from a directory with no sums file are published unverified: the
// operator may well have built them a moment ago.
func verifySources(sources map[string]string) error {
	byDir := map[string]map[string]string{}
	for filename, source := range sources {
		parent := filepath.Dir(source)
		if _, done := byDir[parent]; !done {
			byDir[parent] = Checksums(parent)
		}
		want := byDir[parent][filename]
		if want == "" {
			continue
		}
		got, err := Digest(source)
		if err != nil {
			return fmt.Errorf("checksum %s: %w", source, err)
		}
		if got != want {
			return fmt.Errorf("%s does not match the SHA-256 recorded beside it "+
				"(got %s, want %s): the copy is damaged, do not publish it", source, got, want)
		}
	}
	return nil
}

func install(source, target string) error {
	in, err := os.Open(source) // #nosec G304 -- an operator-supplied release path.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	temporary, err := os.CreateTemp(filepath.Dir(target), ".publish-*")
	if err != nil {
		return fmt.Errorf("stage %s: %w", filepath.Base(target), err)
	}
	staged := temporary.Name()
	defer func() { _ = os.Remove(staged) }()

	if _, err := io.Copy(temporary, in); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("copy %s: %w", filepath.Base(target), err)
	}
	// Executables, and world-readable because the server may run as a different user
	// than the operator who published them.
	if err := temporary.Chmod(0o755); err != nil {
		_ = temporary.Close()
		return err
	}
	// Durable before the rename: a host that loses power mid-publish should come back
	// with either the old binary or the new one, never a truncated file under the
	// right name.
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(staged, target)
}

// writeChecksums regenerates every sums file from the directory's current contents,
// in the same format sha256sum emits so `sha256sum -c` still works on it.
func writeChecksums(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	lines := map[string][]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		binary, ok := Parse(entry.Name())
		if !ok {
			continue
		}
		digest, err := Digest(filepath.Join(dir, entry.Name()))
		if err != nil {
			return fmt.Errorf("checksum %s: %w", entry.Name(), err)
		}
		lines[binary.Tool] = append(lines[binary.Tool], digest+"  "+entry.Name())
	}
	for tool, body := range lines {
		sort.Strings(body)
		path := filepath.Join(dir, ChecksumsFile(tool))
		if err := os.WriteFile(path, []byte(strings.Join(body, "\n")+"\n"), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}

// Verify reports which of a complete client release is missing from dir, and which
// published files no longer match their recorded digest. It is what `pkgreg doctor`
// reports and what a publish prints back.
func Verify(dir string) (missing []string, corrupt []string, unrecorded []string, err error) {
	sums := Checksums(dir)
	for _, filename := range ClientPlatforms() {
		path := filepath.Join(dir, filename)
		info, statErr := os.Stat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			missing = append(missing, filename)
			continue
		}
		if statErr != nil {
			return nil, nil, nil, statErr
		}
		if info.IsDir() {
			missing = append(missing, filename)
			continue
		}
		want := sums[filename]
		if want == "" {
			unrecorded = append(unrecorded, filename)
			continue
		}
		got, digestErr := Digest(path)
		if digestErr != nil {
			return nil, nil, nil, digestErr
		}
		if got != want {
			corrupt = append(corrupt, filename)
		}
	}
	return missing, corrupt, unrecorded, nil
}
