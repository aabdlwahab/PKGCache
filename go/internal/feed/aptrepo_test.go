package feed

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildDeb assembles a minimal but real .deb: an ar archive of three members, the middle
// one a gzipped tar holding ./control. The same shape packaging/deb/build.sh writes, so a
// test failing here means the reader would fail on the real thing.
func buildDeb(t *testing.T, control string) []byte {
	t.Helper()

	var tarball bytes.Buffer
	zip := gzip.NewWriter(&tarball)
	archive := tar.NewWriter(zip)
	body := []byte(control)
	if err := archive.WriteHeader(&tar.Header{
		Name: "./control", Mode: 0o644, Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}

	var deb bytes.Buffer
	deb.WriteString("!<arch>\n")
	member := func(name string, content []byte) {
		fmt.Fprintf(&deb, "%-16s%-12d%-6d%-6d%-8o%-10d`\n",
			name, 0, 0, 0, 0o644, len(content))
		deb.Write(content)
		if len(content)%2 == 1 {
			deb.WriteByte('\n')
		}
	}
	member("debian-binary", []byte("2.0\n"))
	member("control.tar.gz", tarball.Bytes())
	member("data.tar.gz", []byte("not a real payload"))
	return deb.Bytes()
}

const sampleControl = `Package: pkgcache
Version: 1.2.0
Architecture: amd64
Maintainer: pkgreg <root@localhost>
Installed-Size: 27344
Depends: pkgcache (= 1.2.0), libgtk-4-1
Description: Package cache for one machine
 pkgcache keeps this machine's package downloads on local disk.
 .
 It is a single static binary.
`

func writeDeb(t *testing.T, control string) string {
	t.Helper()
	dir := t.TempDir()
	name := filepath.Join(dir, "pkgcache_1.2.0_amd64.deb")
	if err := os.WriteFile(name, buildDeb(t, control), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestReadDebExtractsTheControlStanza(t *testing.T) {
	pkg, err := ReadDeb(writeDeb(t, sampleControl))
	if err != nil {
		t.Fatalf("ReadDeb: %v", err)
	}
	for field, want := range map[string]string{
		"Package": "pkgcache", "Version": "1.2.0", "Architecture": "amd64",
	} {
		if got := pkg.Get(field); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
	if pkg.SHA256 == "" || pkg.MD5 == "" || pkg.Size == 0 {
		t.Error("a package with no size or hashes cannot be put in an index")
	}
	// Case-insensitivity is in the format, and a repository that missed a field over
	// capitalisation would produce an index apt rejects for an invisible reason.
	if pkg.Get("package") != "pkgcache" {
		t.Error("field lookup must be case-insensitive")
	}
}

func TestReadDebKeepsFoldedDescriptions(t *testing.T) {
	pkg, err := ReadDeb(writeDeb(t, sampleControl))
	if err != nil {
		t.Fatalf("ReadDeb: %v", err)
	}
	description := pkg.Get("Description")
	if !strings.Contains(description, "single static binary") {
		t.Errorf("continuation lines were dropped:\n%q", description)
	}
	// Folded into the field above, not promoted into fields of their own.
	for _, field := range pkg.Control {
		if strings.HasPrefix(field.Name, " ") || field.Name == "." {
			t.Errorf("a continuation line became a field: %q", field.Name)
		}
	}
}

func TestReadDebRejectsThingsThatAreNotDebs(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "notadeb.deb")
	if err := os.WriteFile(name, []byte("this is just a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDeb(name); err == nil {
		t.Fatal("a file that is not an ar archive must be refused")
	}
}

func TestReadDebRejectsATruncatedArchive(t *testing.T) {
	full := buildDeb(t, sampleControl)
	dir := t.TempDir()
	name := filepath.Join(dir, "truncated.deb")
	// Half a package is the shape an interrupted upload leaves behind, and publishing it
	// would put a hash in the index for bytes nobody can fetch.
	if err := os.WriteFile(name, full[:len(full)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDeb(name); err == nil {
		t.Fatal("a truncated .deb must be refused")
	}
}

func TestPackagesIndexIsAStanzaPerPackage(t *testing.T) {
	pkg, err := ReadDeb(writeDeb(t, sampleControl))
	if err != nil {
		t.Fatal(err)
	}
	pkg.Filename = PoolPath("main", "pkgcache", "pkgcache_1.2.0_amd64.deb")

	index := string(PackagesIndex([]DebPackage{pkg, pkg}))

	// apt splits on blank lines; a missing trailing newline silently drops the last entry.
	if !strings.HasSuffix(index, "\n\n") {
		t.Error("every stanza must be terminated by a blank line")
	}
	if got := strings.Count(index, "Package: pkgcache"); got != 2 {
		t.Errorf("want 2 stanzas, got %d", got)
	}
	for _, want := range []string{
		"Filename: pool/main/p/pkgcache/pkgcache_1.2.0_amd64.deb",
		"SHA256: " + pkg.SHA256,
		"MD5sum: " + pkg.MD5,
		fmt.Sprintf("Size: %d", pkg.Size),
	} {
		if !strings.Contains(index, want) {
			t.Errorf("the index is missing %q\n%s", want, index)
		}
	}
}

func TestPackagesIndexOverridesRepositoryOwnedFields(t *testing.T) {
	// A control file claiming its own Filename is describing a location it cannot know.
	// The repository's answer has to win, or apt fetches from the wrong place.
	pkg, err := ReadDeb(writeDeb(t, sampleControl+"Filename: lies/about/where/it/is.deb\n"))
	if err != nil {
		t.Fatal(err)
	}
	pkg.Filename = "pool/main/p/pkgcache/real.deb"
	index := string(PackagesIndex([]DebPackage{pkg}))

	if strings.Contains(index, "lies/about") {
		t.Errorf("a control file's own Filename must not survive into the index\n%s", index)
	}
	if strings.Count(index, "Filename:") != 1 {
		t.Errorf("exactly one Filename per stanza\n%s", index)
	}
}

func TestPoolPathFansOutTheWayDebianDoes(t *testing.T) {
	for _, testCase := range []struct{ source, want string }{
		{"pkgcache", "pool/main/p/pkgcache/x.deb"},
		{"libfoo", "pool/main/libf/libfoo/x.deb"},
	} {
		if got := PoolPath("main", testCase.source, "x.deb"); got != testCase.want {
			t.Errorf("PoolPath(%q) = %q, want %q", testCase.source, got, testCase.want)
		}
	}
}

func TestReleaseFileVouchesForEveryIndex(t *testing.T) {
	files := []IndexFile{
		{Path: "main/binary-arm64/Packages", Body: []byte("arm64 index")},
		{Path: "main/binary-amd64/Packages", Body: []byte("amd64 index")},
	}
	release := string(ReleaseFile(ReleaseOptions{
		Origin: "pkgreg", Label: "pkgcache", Suite: "stable", Codename: "stable",
		Components: []string{"main"}, Architectures: []string{"amd64", "arm64"},
		Date: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
	}, files))

	for _, want := range []string{
		"Origin: pkgreg", "Suite: stable", "Architectures: amd64 arm64",
		"Components: main", "MD5Sum:", "SHA256:",
		"Date: Sun, 23 Aug 2026 12:00:00 +0000",
	} {
		if !strings.Contains(release, want) {
			t.Errorf("Release is missing %q\n%s", want, release)
		}
	}
	// The signature over Release is a signature over the whole repository only if every
	// index is named in it. One omitted index is an unsigned index.
	for _, file := range files {
		if strings.Count(release, file.Path) != 2 {
			t.Errorf("%s should appear under both hash sections\n%s", file.Path, release)
		}
	}
	// Sorted, so two runs over the same content produce the same file.
	if strings.Index(release, "binary-amd64") > strings.Index(release, "binary-arm64") {
		t.Error("index files must be listed in a stable order")
	}
}

func TestReleaseValidUntilIsOptional(t *testing.T) {
	// An air-gapped machine that has not reached its server in a month still needs apt
	// to work, so this is off unless somebody asks for it.
	plain := string(ReleaseFile(ReleaseOptions{Suite: "stable"}, nil))
	if strings.Contains(plain, "Valid-Until") {
		t.Error("Valid-Until must not appear unless it was asked for")
	}
	dated := string(ReleaseFile(
		ReleaseOptions{Suite: "stable", ValidUntil: 7 * 24 * time.Hour}, nil))
	if !strings.Contains(dated, "Valid-Until") {
		t.Error("Valid-Until was asked for and is missing")
	}
}

func TestGzipIsReproducible(t *testing.T) {
	// Without this the repository loses the byte-identical rebuild property the .deb
	// already has, and every republish looks like a change nobody made.
	payload := []byte(strings.Repeat("Package: pkgcache\n", 100))
	first, err := Gzip(payload)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	second, err := Gzip(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("two compressions of one input differ; the header is carrying a clock")
	}

	reader, err := gzip.NewReader(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := out.ReadFrom(reader); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Error("the compressed index does not decompress to what went in")
	}
}

func TestParseControlStopsAtTheBlankLine(t *testing.T) {
	// A control file holds one stanza. Reading past the blank line would fold a second
	// package's fields into the first.
	fields := ParseControl("Package: a\nVersion: 1\n\nPackage: b\n")
	if len(fields) != 2 {
		t.Fatalf("want 2 fields from the first stanza, got %d: %+v", len(fields), fields)
	}
}
