package clientrelease

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAcceptsOnlyReleaseNames(t *testing.T) {
	t.Parallel()
	for _, name := range ClientPlatforms() {
		binary, ok := Parse(name)
		if !ok {
			t.Fatalf("Parse(%q) rejected a name the release build produces", name)
		}
		if binary.Tool != "client" {
			t.Errorf("Parse(%q).Tool = %q, want client", name, binary.Tool)
		}
		if binary.OS == "" || binary.Arch == "" {
			t.Errorf("Parse(%q) left os/arch empty: %+v", name, binary)
		}
	}
	// The grammar is the traversal defence for GET /api/v1/downloads/{name}, so its
	// rejections matter more than its acceptances.
	for _, name := range []string{
		"", ".", "..", "../../etc/passwd", "pkgreg-client-linux-amd64/../ca.key",
		"pkgreg-client-SHA256SUMS", "pkgreg-client-linux-amd64.sh",
		"pkgreg-server-linux-amd64", "pkgreg-client-plan9-amd64",
		"pkgreg-client-linux-riscv64", "pkgreg-client-linux-amd64 ",
	} {
		if _, ok := Parse(name); ok {
			t.Errorf("Parse(%q) accepted an unpublishable name", name)
		}
	}
}

func TestPublishInstallsExecutablesAndRecordsDigests(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	target := filepath.Join(t.TempDir(), "downloads")
	write(t, filepath.Join(source, "pkgreg-client-linux-amd64"), "linux amd64 body")
	write(t, filepath.Join(source, "pkgreg-client-darwin-arm64"), "darwin arm64 body")
	// Not ours, and must not be copied even though it sits in the same directory.
	write(t, filepath.Join(source, "notes.txt"), "unrelated")

	found, err := Collect([]string{source})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("Collect found %d files, want 2: %v", len(found), found)
	}
	published, err := Publish(target, found)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(published) != 2 {
		t.Fatalf("Publish reported %d files, want 2", len(published))
	}
	for _, binary := range published {
		if binary.SHA256 == "" {
			t.Errorf("%s was published without a recorded digest", binary.Name)
		}
		info, err := os.Stat(filepath.Join(target, binary.Name))
		if err != nil {
			t.Fatalf("stat %s: %v", binary.Name, err)
		}
		// A client that downloads a non-executable file gets "permission denied" with
		// nothing to suggest the cause.
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable (mode %v)", binary.Name, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(target, "notes.txt")); err == nil {
		t.Error("Publish copied a file that is not part of a release")
	}

	// The sums file must stay in sha256sum's own format: operators do run
	// `sha256sum -c` on it, and Checksums parses what we write.
	sums := Checksums(target)
	if len(sums) != 2 {
		t.Fatalf("Checksums read %d entries, want 2: %v", len(sums), sums)
	}
	body, err := os.ReadFile(filepath.Join(target, ChecksumsFile("client")))
	if err != nil {
		t.Fatalf("read sums: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			t.Errorf("sums line %q is not `<64-hex>  <name>`", line)
		}
	}
}

// A second publish of one platform must not invalidate the digests of the others.
// Appending to the sums file, or writing only the new entry, both got this wrong.
func TestPublishKeepsEarlierPlatformsVerifiable(t *testing.T) {
	t.Parallel()
	first, second := t.TempDir(), t.TempDir()
	target := filepath.Join(t.TempDir(), "downloads")
	write(t, filepath.Join(first, "pkgreg-client-linux-amd64"), "linux body")
	write(t, filepath.Join(second, "pkgreg-client-windows-amd64.exe"), "windows body")

	for _, dir := range []string{first, second} {
		found, err := Collect([]string{dir})
		if err != nil {
			t.Fatalf("Collect(%s): %v", dir, err)
		}
		if _, err := Publish(target, found); err != nil {
			t.Fatalf("Publish(%s): %v", dir, err)
		}
	}

	_, corrupt, unrecorded, err := Verify(target)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(corrupt) > 0 || len(unrecorded) > 0 {
		t.Fatalf("second publish broke the first: corrupt=%v unrecorded=%v", corrupt, unrecorded)
	}
	list, err := List(target)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List = %d entries, want 2", len(list))
	}
}

// The point of carrying a sums file with the release is that a damaged copy onto an
// air-gapped host is refused instead of served.
func TestPublishRefusesSourcesThatContradictTheirChecksums(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	target := filepath.Join(t.TempDir(), "downloads")
	name := "pkgreg-client-linux-amd64"
	write(t, filepath.Join(source, name), "damaged in transit")
	write(t, filepath.Join(source, ChecksumsFile("client")),
		strings.Repeat("0", 64)+"  "+name+"\n")

	found, err := Collect([]string{source})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	_, err = Publish(target, found)
	if err == nil {
		t.Fatal("Publish accepted a file that does not match its recorded digest")
	}
	if !strings.Contains(err.Error(), "damaged") {
		t.Errorf("error does not tell the operator what happened: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(target, name)); statErr == nil {
		t.Error("a refused publish still installed the file")
	}
}

func TestVerifyReportsAnIncompleteRelease(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "downloads")
	source := t.TempDir()
	write(t, filepath.Join(source, "pkgreg-client-linux-amd64"), "body")
	found, err := Collect([]string{source})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if _, err := Publish(target, found); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	missing, corrupt, unrecorded, err := Verify(target)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(missing) != len(ClientPlatforms())-1 {
		t.Errorf("missing = %v, want every platform but linux amd64", missing)
	}
	if len(corrupt) > 0 || len(unrecorded) > 0 {
		t.Errorf("published file reported as corrupt=%v unrecorded=%v", corrupt, unrecorded)
	}

	// Rewriting the file behind the sums file is the shape of a botched manual copy.
	write(t, filepath.Join(target, "pkgreg-client-linux-amd64"), "different body")
	_, corrupt, _, err = Verify(target)
	if err != nil {
		t.Fatalf("Verify after tampering: %v", err)
	}
	if len(corrupt) != 1 {
		t.Errorf("corrupt = %v, want the rewritten file", corrupt)
	}
}

func TestListTreatsAFreshInstanceAsEmptyRatherThanBroken(t *testing.T) {
	t.Parallel()
	list, err := List(filepath.Join(t.TempDir(), "never-created"))
	if err != nil {
		t.Fatalf("List of a missing directory failed: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List = %v, want empty", list)
	}
}

func TestCollectRejectsAMisnamedFileByName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "pkgreg-client")
	write(t, path, "host build")
	// Explicitly named, so silence would publish something the API cannot serve: an
	// operator who ran `make client-build` has exactly this file and would otherwise
	// see a successful publish that offers nothing.
	if _, err := Collect([]string{path}); err == nil {
		t.Fatal("Collect accepted a file whose name the download route would reject")
	}
	found, err := Collect([]string{dir})
	if err != nil {
		t.Fatalf("Collect(dir): %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("Collect(dir) = %v, want nothing publishable", found)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
