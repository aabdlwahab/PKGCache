package local

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// A bare .tar name is the server's own contract — the job writes it into shuttle/out and
// leaves it there — and it has to keep meaning that, because every existing instruction
// says it.
func TestPlanExportLeavesABareNameWhereTheServerWouldWrite(t *testing.T) {
	dir := t.TempDir()
	plan, err := PlanExport(dir, "work.tar")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Name != "work.tar" || plan.Final != "" {
		t.Fatalf("a bare name planned as %+v", plan)
	}
	if got := plan.StagedPath(dir); got != filepath.Join(ShuttleOut(dir), "work.tar") {
		t.Fatalf("staged at %s", got)
	}
}

// A path is the case the client exists for: the pack is going onto a stick, not into
// ~/.local/share/pkgcache.
func TestPlanExportSendsAPathSomewhereElse(t *testing.T) {
	dir, stick := t.TempDir(), t.TempDir()
	destination := filepath.Join(stick, "work.tar")
	plan, err := PlanExport(dir, destination)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Name != "work.tar" || plan.Final != destination {
		t.Fatalf("planned as %+v", plan)
	}

	// FinishExport moves what the job wrote.
	if err := os.MkdirAll(ShuttleOut(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plan.StagedPath(dir), []byte("pack"), 0o600); err != nil {
		t.Fatal(err)
	}
	final, err := plan.FinishExport(dir)
	if err != nil {
		t.Fatal(err)
	}
	if final != destination {
		t.Fatalf("the pack ended up at %s", final)
	}
	if _, err := os.Stat(plan.StagedPath(dir)); !os.IsNotExist(err) {
		t.Error("the staged copy was left behind")
	}
	if data, err := os.ReadFile(destination); err != nil || string(data) != "pack" {
		t.Fatalf("the pack did not arrive intact: %q %v", data, err)
	}
}

// Every one of these is refused before a job is submitted. An export is minutes and
// gigabytes; a destination that was never going to work should cost neither, and the
// job's own failure would land in a log nobody is watching.
func TestPlanExportRefusesBeforeAnythingIsSubmitted(t *testing.T) {
	dir, stick := t.TempDir(), t.TempDir()
	existing := filepath.Join(stick, "taken.tar")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, file, want string }{
		{"not a tar", filepath.Join(stick, "work.tgz"), "does not end in .tar"},
		{"bare name that is not a tar", "work", "does not end in .tar"},
		{"destination exists", existing, "already exists"},
		{"no such directory", filepath.Join(stick, "nope", "work.tar"), "no such file"},
	} {
		_, err := PlanExport(dir, c.file)
		if err == nil {
			t.Errorf("%s: accepted %q", c.name, c.file)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want something about %q", c.name, err, c.want)
		}
	}
}

// A directory that cannot be written to is caught by writing to it. Mode bits do not
// answer the question — a read-only mount and a full filesystem both look writable.
func TestPlanExportRefusesAnUnwritableDestination(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission this asserts")
	}
	stick := t.TempDir()
	locked := filepath.Join(stick, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	_, err := PlanExport(t.TempDir(), filepath.Join(locked, "work.tar"))
	if err == nil || !strings.Contains(err.Error(), "cannot write to") {
		t.Fatalf("an unwritable destination gave %v", err)
	}
}

// The staged name is unique, so importing two packs at once — or one whose name is
// already sitting in shuttle/in — cannot collide.
func TestStageImportIsUniqueAndReversible(t *testing.T) {
	dir, stick := t.TempDir(), t.TempDir()
	pack := filepath.Join(stick, "work.tar")
	if err := os.WriteFile(pack, []byte("pack"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A pack of the same name already waiting, which must not be touched.
	if err := os.MkdirAll(ShuttleIn(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	bystander := filepath.Join(ShuttleIn(dir), "work.tar")
	if err := os.WriteFile(bystander, []byte("someone else's"), 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := StageImport(dir, pack)
	if err != nil {
		t.Fatal(err)
	}
	second, err := StageImport(dir, pack)
	if err != nil {
		t.Fatal(err)
	}
	if first.Name == second.Name {
		t.Fatalf("two stagings share the name %q", first.Name)
	}
	for _, source := range []ImportSource{first, second} {
		staged := filepath.Join(ShuttleIn(dir), source.Name)
		if data, err := os.ReadFile(staged); err != nil || string(data) != "pack" {
			t.Fatalf("staged copy %s is %q (%v)", source.Name, data, err)
		}
	}
	first.Cleanup(dir)
	second.Cleanup(dir)
	if data, err := os.ReadFile(bystander); err != nil || string(data) != "someone else's" {
		t.Fatalf("staging disturbed a pack already there: %q %v", data, err)
	}
	if _, err := os.Stat(pack); err != nil {
		t.Fatalf("staging disturbed the source pack: %v", err)
	}
}

// A bare name means "already in shuttle/in", so nothing is staged and nothing is cleaned
// up — cleanup must not remove a pack the client did not put there.
func TestStageImportLeavesABareNameAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(ShuttleIn(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	waiting := filepath.Join(ShuttleIn(dir), "work.tar")
	if err := os.WriteFile(waiting, []byte("pack"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := StageImport(dir, "work.tar")
	if err != nil {
		t.Fatal(err)
	}
	if source.Name != "work.tar" {
		t.Fatalf("staged as %q", source.Name)
	}
	source.Cleanup(dir)
	if _, err := os.Stat(waiting); err != nil {
		t.Fatalf("cleanup removed a pack it did not stage: %v", err)
	}
}

func TestStageImportRefusesWhatIsNotAPack(t *testing.T) {
	dir, stick := t.TempDir(), t.TempDir()
	for _, file := range []string{
		filepath.Join(stick, "missing.tar"),
		stick, // a directory
		filepath.Join(stick, "notatar"),
	} {
		if _, err := StageImport(dir, file); err == nil {
			t.Errorf("accepted %q", file)
		}
	}
}

// The follower streams the log as it grows and fails when the job did, carrying the
// job's own error rather than a status word.
func TestFollowJobStreamsAndReportsFailure(t *testing.T) {
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := polls.Add(1)
		job := Job{ID: 7, Project: "work", Action: "export", Status: "running"}
		switch n {
		case 1:
			job.Log = "building\n"
		case 2:
			job.Log = "building\nwrote the pack\n"
		default:
			job.Log = "building\nwrote the pack\n"
			job.Status = "failed"
			job.Error = "snapshot: project has no checkpoint to export"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(job)
	}))
	defer server.Close()

	var out strings.Builder
	state := State{Addr: strings.TrimPrefix(server.URL, "http://")}
	_, err := FollowJob(context.Background(), state, 7, &out)
	if err == nil {
		t.Fatal("a failed job was reported as success")
	}
	if !strings.Contains(err.Error(), "no checkpoint to export") {
		t.Fatalf("the job's own error was lost: %v", err)
	}
	// Each line once: the endpoint returns the whole log every time, so a follower that
	// did not track its offset would print the first line three times.
	if got := strings.Count(out.String(), "building\n"); got != 1 {
		t.Fatalf("the log was printed %d times:\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "wrote the pack") {
		t.Fatalf("the log stopped early:\n%s", out.String())
	}
}

func TestFollowJobReturnsOnSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Job{ID: 1, Action: "import", Status: "done", Log: "ok\n"})
	}))
	defer server.Close()
	var out strings.Builder
	state := State{Addr: strings.TrimPrefix(server.URL, "http://")}
	job, err := FollowJob(context.Background(), state, 1, &out)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "done" || out.String() != "ok\n" {
		t.Fatalf("job=%+v out=%q", job, out.String())
	}
}
