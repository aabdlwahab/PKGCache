package git

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/brightskies/pkgreg/internal/catalog"
	"github.com/brightskies/pkgreg/internal/eco"
	"github.com/brightskies/pkgreg/internal/eco/ecotest"
	testupstream "github.com/brightskies/pkgreg/internal/testutil/upstream"
)

func TestManagedMirrorLifecycleAndCatalog(t *testing.T) {
	fixture := newGitFixture(t, false)
	h := newGitHarness(t, fixture.upstream, nil)

	repoPath := "/global/git/example.test/org/demo.git"
	response := h.Get(repoPath + "/info/refs?service=git-upload-pack")
	if response.Status != http.StatusOK {
		t.Fatalf("info/refs status = %d, body = %q", response.Status, response.Text())
	}
	if !bytes.HasPrefix(response.Body, serviceAdvertisement) {
		t.Fatalf("advertisement does not have smart HTTP preamble: %q", response.Body)
	}

	mirror := filepath.Join(h.Blobs.Root(), "managed", ID, "global",
		"example.test", "org", "demo.git")
	if !mirrorExists(mirror) {
		t.Fatalf("mirror was not created at %s", mirror)
	}
	for key, want := range map[string]string{
		"gc.auto":                "0",
		"maintenance.auto":       "false",
		"uploadpack.allowFilter": "true",
	} {
		got := strings.TrimSpace(runGitCommand(t, "", nil,
			"--git-dir", mirror, "config", "--get", key))
		if got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	refspecs := strings.Fields(runGitCommand(t, "", nil,
		"--git-dir", mirror, "config", "--get-all", "remote.origin.fetch"))
	if len(refspecs) != 2 {
		t.Fatalf("remote.origin.fetch = %v, want heads and tags", refspecs)
	}

	refs, err := h.Catalog.ListRefs("global", ID, "example.test/org/demo/")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) < 2 {
		t.Fatalf("catalog refs = %v, want branch and tag", refs)
	}
	artifacts, _, err := h.Catalog.QueryArtifacts(catalog.ArtifactQuery{
		Project: "global", Eco: ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	var nonZero int
	for _, artifact := range artifacts {
		if artifact.Name == "example.test/org/demo" && artifact.Size > 0 {
			nonZero++
		}
	}
	if nonZero != 1 {
		t.Fatalf("non-zero mirror-size rows = %d, want exactly one; artifacts=%+v",
			nonZero, artifacts)
	}

	for _, path := range []string{
		"/global/git/example.test/%2e%2e/escape.git/info/refs?service=git-upload-pack",
		"/global/git/not-a-host/repo.git/info/refs?service=git-upload-pack",
		"/global/git/example.test/org/%2e/demo.git/info/refs?service=git-upload-pack",
	} {
		if got := h.Get(path); got.Status != http.StatusNotFound {
			t.Errorf("unsafe path %q status = %d, want 404", path, got.Status)
		}
	}
}

func TestRealGitClientsFullShallowPartialPinnedOfflineAndPushRefusal(t *testing.T) {
	fixture := newGitFixture(t, false)
	h := newGitHarness(t, fixture.upstream, nil)
	cacheURL := h.URL("/global/git/example.test/org/demo.git")

	full := filepath.Join(t.TempDir(), "full")
	runGitCommand(t, "", nil, "clone", cacheURL, full)
	assertFile(t, filepath.Join(full, "README.md"), "second\n")

	shallow := filepath.Join(t.TempDir(), "shallow")
	runGitCommand(t, "", nil, "clone", "--depth=1", cacheURL, shallow)
	if got := strings.TrimSpace(runGitCommand(t, shallow, nil,
		"rev-list", "--count", "HEAD")); got != "1" {
		t.Fatalf("shallow clone commit count = %s, want 1", got)
	}

	partial := filepath.Join(t.TempDir(), "partial")
	runGitCommand(t, "", nil, "-c", "protocol.version=2",
		"clone", "--filter=blob:none", "--no-checkout", cacheURL, partial)
	if got := strings.TrimSpace(runGitCommand(t, partial, nil,
		"rev-parse", "HEAD")); got != fixture.head {
		t.Fatalf("partial clone HEAD = %s, want %s", got, fixture.head)
	}

	pinned := filepath.Join(t.TempDir(), "pinned")
	runGitCommand(t, "", nil, "init", "-q", pinned)
	runGitCommand(t, pinned, nil, "fetch", cacheURL, fixture.first)
	if got := strings.TrimSpace(runGitCommand(t, pinned, nil,
		"rev-parse", "FETCH_HEAD")); got != fixture.first {
		t.Fatalf("pinned fetch = %s, want %s", got, fixture.first)
	}

	push := exec.Command("git", "-C", full, "push", cacheURL, "HEAD:refs/heads/refused")
	push.Env = gitEnv("")
	output, err := push.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "403") {
		t.Fatalf("push err=%v output=%q, want HTTP 403", err, output)
	}
	refusal := h.Get("/global/git/example.test/org/demo.git/info/refs?service=git-receive-pack")
	if refusal.Status != http.StatusForbidden ||
		!strings.Contains(refusal.Text(), "read-only mirror") {
		t.Fatalf("receive-pack refusal = %d %q", refusal.Status, refusal.Text())
	}

	offlineOrigin := fixture.upstream + ".offline"
	if err := os.Rename(fixture.upstream, offlineOrigin); err != nil {
		t.Fatal(err)
	}
	h.Offline(true)
	offline := filepath.Join(t.TempDir(), "offline")
	runGitCommand(t, "", nil, "clone", cacheURL, offline)
	assertFile(t, filepath.Join(offline, "README.md"), "second\n")

	miss := h.Get("/global/git/example.test/org/missing.git/info/refs?service=git-upload-pack")
	if miss.Status != http.StatusNotFound || !strings.Contains(miss.Text(), "offline") {
		t.Fatalf("offline miss = %d %q", miss.Status, miss.Text())
	}
}

func TestRefreshPrunesRefsSynchronizesHEADAndMaintenance(t *testing.T) {
	fixture := newGitFixture(t, false)
	h := ecotest.New(t, func(*testupstream.Server) eco.Ecosystem {
		return NewWithOptions(Options{
			RefsTTL: -1,
			ResolveUpstream: func(string) (string, error) {
				return fixture.upstream, nil
			},
		})
	})
	path := "/global/git/example.test/org/demo.git/info/refs?service=git-upload-pack"
	if got := h.Get(path); got.Status != http.StatusOK {
		t.Fatalf("initial mirror: %d %q", got.Status, got.Text())
	}

	runGitCommand(t, fixture.source, nil, "checkout", "-q", "-b", "next")
	if err := os.WriteFile(filepath.Join(fixture.source, "NEXT"), []byte("next\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, fixture.source, nil, "add", "NEXT")
	runGitCommand(t, fixture.source, nil, "commit", "-q", "-m", "next")
	runGitCommand(t, fixture.source, nil, "push", fixture.upstream, "next")
	runGitCommand(t, "", nil, "--git-dir", fixture.upstream,
		"symbolic-ref", "HEAD", "refs/heads/next")
	runGitCommand(t, fixture.source, nil, "push", fixture.upstream, ":refs/tags/v1")

	if got := h.Get(path); got.Status != http.StatusOK {
		t.Fatalf("refresh: %d %q", got.Status, got.Text())
	}
	mirror := filepath.Join(h.Blobs.Root(), "managed", ID, "global",
		"example.test", "org", "demo.git")
	head := strings.TrimSpace(runGitCommand(t, "", nil,
		"--git-dir", mirror, "symbolic-ref", "HEAD"))
	if head != "refs/heads/next" {
		t.Fatalf("mirror HEAD = %q, want refs/heads/next", head)
	}
	if commandSucceeds("", "--git-dir", mirror, "show-ref", "--verify", "refs/tags/v1") {
		t.Fatal("pruned upstream tag still exists in mirror")
	}
	refs, err := h.Catalog.ListRefs("global", ID, "example.test/org/demo/")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if strings.HasSuffix(ref.Name, "/refs/tags/v1") {
			t.Fatalf("pruned tag remains in catalog: %+v", ref)
		}
	}

	request, _ := http.NewRequest(http.MethodPost, h.URL("/global/git/+maintain"), nil)
	maintained := h.Do(request)
	if maintained.Status != http.StatusOK {
		t.Fatalf("maintain = %d %q", maintained.Status, maintained.Text())
	}
	var result struct {
		Maintained int      `json:"maintained"`
		Errors     []string `json:"errors"`
	}
	if err := json.Unmarshal(maintained.Body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Maintained != 1 || len(result.Errors) != 0 {
		t.Fatalf("maintain result = %+v", result)
	}
	packs, err := filepath.Glob(filepath.Join(mirror, "objects", "pack", "*.pack"))
	if err != nil || len(packs) == 0 {
		t.Fatalf("geometric repack produced no packs: %v, %v", packs, err)
	}
}

func TestConcurrentRefreshCollapsesAndSerializes(t *testing.T) {
	fixture := newGitFixture(t, false)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	seed := newMirrorManager(realGit, time.Hour, 8)
	mirror := filepath.Join(t.TempDir(), "demo.git")
	if _, err := seed.ensure(context.Background(), "repo", mirror,
		fixture.upstream, false, nil); err != nil {
		t.Fatal(err)
	}

	control := t.TempDir()
	countFile := filepath.Join(control, "fetches")
	lockDir := filepath.Join(control, "active")
	overlapFile := filepath.Join(control, "overlap")
	wrapper := filepath.Join(control, "git-wrapper")
	script := fmt.Sprintf(`#!/bin/sh
is_fetch=0
for arg in "$@"; do
  if [ "$arg" = "fetch" ]; then is_fetch=1; fi
done
if [ "$is_fetch" = "1" ]; then
  printf 'fetch\n' >> %s
  if ! mkdir %s 2>/dev/null; then printf 'overlap\n' >> %s; fi
  sleep 0.15
  %s "$@"
  result=$?
  rmdir %s 2>/dev/null || true
  exit "$result"
fi
exec %s "$@"
`, shellQuote(countFile), shellQuote(lockDir), shellQuote(overlapFile),
		shellQuote(realGit), shellQuote(lockDir), shellQuote(realGit))
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	manager := newMirrorManager(wrapper, time.Hour, 8)
	const callers = 12
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			_, err := manager.ensure(context.Background(), "repo", mirror,
				fixture.upstream, false, nil)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	count, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(count), "fetch"); got != 1 {
		t.Fatalf("fetch subprocesses = %d, want 1", got)
	}
	if overlap, _ := os.ReadFile(overlapFile); len(overlap) != 0 {
		t.Fatalf("fetches overlapped: %q", overlap)
	}
}

func TestUploadPackSemaphoreAndCancellationReap(t *testing.T) {
	dir := t.TempDir()
	slow := filepath.Join(dir, "slow-git")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 0.12\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := newMirrorManager(slow, time.Hour, defaultMaxUploadPacks)
	const calls = 16
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	wg.Add(calls)
	for range calls {
		go func() {
			defer wg.Done()
			errs <- manager.uploadPack(context.Background(), "ignored", "", []byte("x"), io.Discard)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if peak := manager.uploadPeak.Load(); peak != defaultMaxUploadPacks {
		t.Fatalf("peak upload-pack processes = %d, want %d",
			peak, defaultMaxUploadPacks)
	}

	pidFile := filepath.Join(dir, "pid")
	blocked := filepath.Join(dir, "blocked-git")
	script := "#!/bin/sh\nprintf '%s' \"$$\" > " + shellQuote(pidFile) + "\nexec sleep 30\n"
	if err := os.WriteFile(blocked, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cancelManager := newMirrorManager(blocked, time.Hour, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- cancelManager.uploadPack(ctx, "ignored", "", []byte("x"), io.Discard)
	}()
	var pid int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(pidFile)
		if err == nil {
			pid, _ = strconv.Atoi(string(raw))
			if pid > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("upload-pack child did not record its pid")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled upload-pack err = %v", err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(syscall.Signal(0)); err == nil {
		t.Fatalf("upload-pack pid %d still exists after cancellation", pid)
	}
}

func TestRealGitLFSPullAndOfflineCASReuse(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs is not installed")
	}
	payload := []byte("large object supplied by git lfs\n")
	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])
	fixture := newGitFixtureWithLFS(t, oid, int64(len(payload)))

	h := ecotest.New(t, func(origin *testupstream.Server) eco.Ecosystem {
		origin.Serve("/lfs/object", payload)
		batch, err := json.Marshal(map[string]any{
			"transfer": "basic",
			"objects": []map[string]any{{
				"oid": oid, "size": len(payload),
				"actions": map[string]any{
					"download": map[string]any{"href": origin.URLFor("/lfs/object")},
				},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		origin.Serve("/lfs/batch", batch)
		return NewWithOptions(Options{
			RefsTTL: time.Hour,
			ResolveUpstream: func(string) (string, error) {
				return fixture.upstream, nil
			},
			LFSBatchURL: func(string, string) string {
				return origin.URLFor("/lfs/batch")
			},
		})
	})
	cacheURL := h.URL("/global/git/example.test/org/lfs-demo.git")

	first := filepath.Join(t.TempDir(), "first")
	runGitCommand(t, "", []string{"GIT_LFS_SKIP_SMUDGE=1"}, "clone", cacheURL, first)
	runGitCommand(t, first, nil, "lfs", "install", "--local")
	runGitCommand(t, first, nil, "lfs", "pull")
	assertBytes(t, filepath.Join(first, "asset.bin"), payload)
	if hits := h.Origin.Hits("/lfs/object"); hits != 1 {
		t.Fatalf("origin LFS object hits = %d, want 1", hits)
	}

	h.Offline(true)
	second := filepath.Join(t.TempDir(), "second")
	runGitCommand(t, "", []string{"GIT_LFS_SKIP_SMUDGE=1"}, "clone", cacheURL, second)
	runGitCommand(t, second, nil, "lfs", "install", "--local")
	runGitCommand(t, second, nil, "lfs", "pull")
	assertBytes(t, filepath.Join(second, "asset.bin"), payload)
	if hits := h.Origin.Hits("/lfs/object"); hits != 1 {
		t.Fatalf("offline LFS pull contacted origin; hits = %d", hits)
	}
}

func TestLFSDeduplicatesFromAnotherEcosystem(t *testing.T) {
	payload := []byte("already present in the shared CAS")
	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])
	fixture := newGitFixture(t, false)
	h := newGitHarness(t, fixture.upstream, nil)

	digest, err := h.Engine.PutBytes("another-project", "files", "shared.bin",
		payload, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	if digest.String() != oid {
		t.Fatalf("seed digest = %s, want %s", digest, oid)
	}
	body, _ := json.Marshal(map[string]any{
		"operation": "download",
		"objects":   []map[string]any{{"oid": oid, "size": len(payload)}},
	})
	request, _ := http.NewRequest(http.MethodPost,
		h.URL("/global/git/example.test/org/demo.git/info/lfs/objects/batch"),
		bytes.NewReader(body))
	request.Header.Set("Content-Type", lfsMediaType)
	response := h.Do(request)
	if response.Status != http.StatusOK {
		t.Fatalf("LFS batch = %d %q", response.Status, response.Text())
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, lfsMediaType) {
		t.Fatalf("LFS content type = %q, want %q", got, lfsMediaType)
	}
	if hits := h.Origin.Hits("/lfs/batch"); hits != 0 {
		t.Fatalf("CAS hit forwarded an LFS batch upstream %d times", hits)
	}
	object := h.Get("/global/git/+lfs/" + oid)
	if object.Status != http.StatusOK || !bytes.Equal(object.Body, payload) {
		t.Fatalf("deduplicated LFS GET = %d %q", object.Status, object.Body)
	}
	count, size, err := h.Blobs.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || size != int64(len(payload)) {
		t.Fatalf("CAS usage = (%d, %d), want one %d-byte blob", count, size, len(payload))
	}
}

func TestNegotiationBodyBoundsAndGzip(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write([]byte("negotiation")); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/git-upload-pack",
		bytes.NewReader(compressed.Bytes()))
	request.Header.Set("Content-Encoding", "gzip")
	body, status, err := readNegotiation(httptest.NewRecorder(), request)
	if err != nil || status != 0 || string(body) != "negotiation" {
		t.Fatalf("gzip body = %q status=%d err=%v", body, status, err)
	}

	tooLarge := httptest.NewRequest(http.MethodPost, "/git-upload-pack",
		bytes.NewReader([]byte("small")))
	tooLarge.ContentLength = maxNegotiationBytes + 1
	if _, status, err := readNegotiation(httptest.NewRecorder(), tooLarge); status != http.StatusRequestEntityTooLarge || err == nil {
		t.Fatalf("oversize status=%d err=%v", status, err)
	}

	unsupported := httptest.NewRequest(http.MethodPost, "/git-upload-pack",
		bytes.NewReader(nil))
	unsupported.Header.Set("Content-Encoding", "br")
	if _, status, err := readNegotiation(httptest.NewRecorder(), unsupported); status != http.StatusUnsupportedMediaType || err == nil {
		t.Fatalf("unsupported encoding status=%d err=%v", status, err)
	}
}

type gitFixture struct {
	source   string
	upstream string
	first    string
	head     string
}

func newGitFixture(t *testing.T, lfs bool) gitFixture {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	runGitCommand(t, "", nil, "init", "-q", "-b", "main", source)
	runGitCommand(t, source, nil, "config", "user.name", "Phase Five")
	runGitCommand(t, source, nil, "config", "user.email", "phase5@example.test")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, source, nil, "add", "README.md")
	runGitCommand(t, source, nil, "commit", "-q", "-m", "first")
	first := strings.TrimSpace(runGitCommand(t, source, nil, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, source, nil, "commit", "-q", "-am", "second")
	runGitCommand(t, source, nil, "tag", "v1")
	head := strings.TrimSpace(runGitCommand(t, source, nil, "rev-parse", "HEAD"))
	upstreamPath := filepath.Join(root, "upstream.git")
	runGitCommand(t, "", nil, "clone", "-q", "--bare", "--no-local", source, upstreamPath)
	_ = lfs
	return gitFixture{source: source, upstream: upstreamPath, first: first, head: head}
}

func newGitFixtureWithLFS(t *testing.T, oid string, size int64) gitFixture {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	runGitCommand(t, "", nil, "init", "-q", "-b", "main", source)
	runGitCommand(t, source, nil, "config", "user.name", "Phase Five")
	runGitCommand(t, source, nil, "config", "user.email", "phase5@example.test")
	attributes := "*.bin filter=lfs diff=lfs merge=lfs -text\n"
	pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n",
		oid, size)
	if err := os.WriteFile(filepath.Join(source, ".gitattributes"), []byte(attributes), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "asset.bin"), []byte(pointer), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, source, nil, "add", ".gitattributes", "asset.bin")
	runGitCommand(t, source, nil, "commit", "-q", "-m", "lfs pointer")
	head := strings.TrimSpace(runGitCommand(t, source, nil, "rev-parse", "HEAD"))
	upstreamPath := filepath.Join(root, "upstream.git")
	runGitCommand(t, "", nil, "clone", "-q", "--bare", "--no-local", source, upstreamPath)
	return gitFixture{source: source, upstream: upstreamPath, first: head, head: head}
}

func newGitHarness(
	t *testing.T, upstreamPath string,
	configure func(*testupstream.Server, *Options),
) *ecotest.Harness {
	t.Helper()
	return ecotest.New(t, func(origin *testupstream.Server) eco.Ecosystem {
		options := Options{
			RefsTTL: time.Hour,
			ResolveUpstream: func(string) (string, error) {
				return upstreamPath, nil
			},
		}
		if configure != nil {
			configure(origin, &options)
		}
		return NewWithOptions(options)
	})
}

func runGitCommand(t *testing.T, dir string, extraEnv []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(""), extraEnv...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func commandSucceeds(dir string, args ...string) bool {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv("")
	return cmd.Run() == nil
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	assertBytes(t, path, []byte(want))
}

func assertBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
