package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The air-gap shuttle, from the client.
//
// A transfer is a pack: a tar carrying a manifest and the blobs it names. Nothing in the
// engine had to change for a laptop to make one — the job runner, the staging directories
// and the control API are all in every instance, local mode included — so this file is
// the two things a command line needs that a browser does not: following a job it
// submitted, and putting a pack where a person keeps it rather than inside the cache
// directory.
//
// The jobs go through the daemon rather than opening the store, for the reason every
// other client verb does: the store has one writer, and exporting a project should not
// require stopping the cache that is serving the build somebody is running.
//
// Two rules from the engine shape everything here, and both were measured rather than
// assumed. An import applies the pack itself — there is no second step. And an import is
// refused unless the pack's base matches the project's snapshot head, changing nothing on
// refusal, which is what makes this safe to point at a project that already holds work.

// Job is what the control plane reports about one background operation.
type Job struct {
	ID      int64  `json:"id"`
	Project string `json:"project"`
	Action  string `json:"action"`
	Status  string `json:"status"`
	Error   string `json:"error"`
	Log     string `json:"log"`
}

// Done reports whether this job has stopped, either way.
func (j Job) Done() bool {
	return j.Status == "done" || j.Status == "failed" || j.Status == "cancelled"
}

// SubmitJob queues one operation on the running daemon.
func SubmitJob(
	ctx context.Context, state State, project, action string, params map[string]any,
) (Job, error) {
	var job Job
	path := "/api/v1/projects/" + project + "/" + action
	err := newProjectAPI(state).do(ctx, http.MethodPost, path, params, &job)
	return job, err
}

// jobPollInterval matches the server's own CLI. These are local database round trips, and
// the log is what somebody is watching to know a 20 GB export is still moving.
const jobPollInterval = 100 * time.Millisecond

// FollowJob streams a job's log until it finishes, and fails if the job did.
//
// The log comes from the single-job endpoint: the list omits it, so a client polling the
// list would report progress as a status that sits at "running" for ten minutes.
func FollowJob(ctx context.Context, state State, id int64, out io.Writer) (Job, error) {
	api := newProjectAPI(state)
	path := fmt.Sprintf("/api/v1/jobs/%d", id)
	offset := 0
	for {
		var job Job
		if err := api.do(ctx, http.MethodGet, path, nil, &job); err != nil {
			return job, err
		}
		if len(job.Log) > offset {
			if out != nil {
				_, _ = io.WriteString(out, job.Log[offset:])
			}
			offset = len(job.Log)
		}
		if job.Done() {
			if job.Status != "done" {
				return job, fmt.Errorf("%s: %s", job.Action, jobFailure(job))
			}
			return job, nil
		}
		select {
		case <-ctx.Done():
			return job, ctx.Err()
		case <-time.After(jobPollInterval):
		}
	}
}

func jobFailure(job Job) string {
	if strings.TrimSpace(job.Error) != "" {
		return job.Error
	}
	return job.Status
}

// Snapshot is one checkpoint of a project.
type Snapshot struct {
	ID      string `json:"id"`
	Parent  string `json:"parent"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
	// CreatedAt is a string rather than a time: it is only ever printed, and a checkpoint
	// list that fails to parse because of a timestamp is a worse outcome than a timestamp
	// shown as it arrived.
	CreatedAt string `json:"created_at"`
	Entries   int64  `json:"entry_count"`
	Bytes     int64  `json:"total_bytes"`
}

// Snapshots lists a project's checkpoints, newest first, and the one it is on.
//
// The head is not the tip of the parent chain and cannot be derived from this list: a
// rollback moves the head backwards without removing what came after it. It is also the
// only fact that decides whether a pack will import, so it is reported separately.
func Snapshots(ctx context.Context, state State, project string) (string, []Snapshot, error) {
	var body struct {
		Snapshots []Snapshot `json:"snapshots"`
		Head      string     `json:"head"`
	}
	err := newProjectAPI(state).do(
		ctx, http.MethodGet, "/api/v1/projects/"+project+"/snapshots", nil, &body)
	return body.Head, body.Snapshots, err
}

// Rollback makes a checkpoint the project's live content again.
func Rollback(ctx context.Context, state State, project, snapshot string) (Job, error) {
	var job Job
	path := "/api/v1/projects/" + project + "/snapshots/" + snapshot + "/rollback"
	err := newProjectAPI(state).do(ctx, http.MethodPost, path, map[string]any{}, &job)
	return job, err
}

// ---- packs, and where people keep them -----------------------------------

// ShuttleIn and ShuttleOut are where the job runner stages packs.
func ShuttleIn(dataDir string) string  { return filepath.Join(dataDir, "shuttle", "in") }
func ShuttleOut(dataDir string) string { return filepath.Join(dataDir, "shuttle", "out") }

// ErrNotAPack reports a filename the job runner will not accept.
//
// The errors in this file carry no "local:" prefix, unlike the rest of the package. They
// are answers to something a person typed, printed straight back at them by the command
// that read it, and "pkgcache: local: a pack is a .tar file" spends a word on saying
// which Go package was disappointed.
var ErrNotAPack = errors.New("a pack is a .tar file")

// ExportTarget is the name the job writes and where the client leaves it afterwards.
//
// The job only ever writes a basename into shuttle/out — that is the server's rule and
// this does not change it. What the client adds is the last move, so `-file
// /media/usb/work.tar` means what somebody carrying a USB stick expects.
type ExportTarget struct {
	// Name is the basename the export job is given.
	Name string
	// Final is where the pack is moved to, or empty to leave it in shuttle/out.
	Final string
}

// PlanExport validates a -file value before anything is submitted.
//
// Before, deliberately: a job that fails on its last line has already spent the minutes
// it took to build the pack, and it reports the failure into a log nobody is reading.
func PlanExport(dataDir, file string) (ExportTarget, error) {
	if file == "" {
		// The job names it after the checkpoint it exported, which is the better name
		// and one the client cannot know yet.
		return ExportTarget{}, nil
	}
	name := filepath.Base(file)
	if !strings.HasSuffix(name, ".tar") {
		return ExportTarget{}, fmt.Errorf("%w: %s does not end in .tar", ErrNotAPack, file)
	}
	if name == file && !strings.ContainsRune(file, filepath.Separator) {
		// A bare name means the server's own behaviour: it stays in shuttle/out.
		return ExportTarget{Name: name}, nil
	}
	final, err := filepath.Abs(file)
	if err != nil {
		return ExportTarget{}, err
	}
	if _, err := os.Stat(final); err == nil {
		return ExportTarget{}, fmt.Errorf(
			"%s already exists; remove it or choose another name", final)
	}
	if err := writable(filepath.Dir(final)); err != nil {
		return ExportTarget{}, err
	}
	// Staged under the destination's own name, so a failed move leaves a pack somebody
	// can find and finish carrying by hand.
	return ExportTarget{Name: name, Final: final}, nil
}

// StagedPath is where the export job will write this pack.
func (t ExportTarget) StagedPath(dataDir string) string {
	if t.Name == "" {
		return ""
	}
	return filepath.Join(ShuttleOut(dataDir), t.Name)
}

// FinishExport moves the pack to where it was asked for, and reports where it is.
func (t ExportTarget) FinishExport(dataDir string) (string, error) {
	staged := t.StagedPath(dataDir)
	if t.Final == "" {
		return staged, nil
	}
	if err := move(staged, t.Final); err != nil {
		return staged, fmt.Errorf(
			"the pack was built but could not be moved to %s: %w\n"+
				"  it is still at %s", t.Final, err, staged)
	}
	return t.Final, nil
}

// ImportSource is a pack made visible to the job runner.
type ImportSource struct {
	// Name is the basename the import job is given, empty to let it pick the only pack
	// in shuttle/in.
	Name string
	// staged records that this file is the client's copy and must be removed afterwards.
	staged bool
}

// StageImport makes a pack readable by the job runner, wherever it came from.
func StageImport(dataDir, file string) (ImportSource, error) {
	if file == "" {
		return ImportSource{}, nil
	}
	name := filepath.Base(file)
	if !strings.HasSuffix(name, ".tar") {
		return ImportSource{}, fmt.Errorf("%w: %s does not end in .tar", ErrNotAPack, file)
	}
	if name == file && !strings.ContainsRune(file, filepath.Separator) {
		return ImportSource{Name: name}, nil
	}
	source, err := filepath.Abs(file)
	if err != nil {
		return ImportSource{}, err
	}
	info, err := os.Stat(source)
	if err != nil {
		return ImportSource{}, fmt.Errorf("read the pack: %w", err)
	}
	if !info.Mode().IsRegular() {
		return ImportSource{}, fmt.Errorf("%s is not a file", source)
	}
	if err := os.MkdirAll(ShuttleIn(dataDir), 0o755); err != nil {
		return ImportSource{}, err
	}
	// A unique staged name, so importing two packs at once — or importing one while a
	// pack of the same name is already waiting in shuttle/in — cannot collide. Reserved
	// by CreateTemp rather than composed, because composing one is a race.
	reserved, err := os.CreateTemp(ShuttleIn(dataDir), ".pkgcache-import-*.tar")
	if err != nil {
		return ImportSource{}, err
	}
	staged := reserved.Name()
	_ = reserved.Close()
	// CreateTemp reserved the name by creating the file, and both ways of filling it
	// refuse to touch one that exists — os.Link cannot, and the copy uses O_EXCL so a
	// half-written pack can never be mistaken for a whole one. So the reservation is
	// released here, having served its only purpose: making the name unique.
	if err := os.Remove(staged); err != nil {
		return ImportSource{}, err
	}
	if err := link(source, staged); err != nil {
		_ = os.Remove(staged)
		return ImportSource{}, err
	}
	return ImportSource{Name: filepath.Base(staged), staged: true}, nil
}

// Cleanup removes the client's staged copy. The pack it came from is untouched.
func (s ImportSource) Cleanup(dataDir string) {
	if !s.staged || s.Name == "" {
		return
	}
	_ = os.Remove(filepath.Join(ShuttleIn(dataDir), s.Name))
}

// writable reports whether a directory can be written to, by writing to it. Nothing else
// is reliable: the mode bits on a directory do not account for a read-only mount, a full
// filesystem, or somebody else's ACL.
func writable(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	probe, err := os.CreateTemp(dir, ".pkgcache-probe-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// link hardlinks where it can and copies where it cannot, which is the usual case here:
// a USB stick is a different filesystem.
func link(source, destination string) error {
	if err := os.Link(source, destination); err == nil {
		return nil
	}
	return copyFile(source, destination)
}

// move is rename where it can be, copy-then-remove where it cannot.
func move(source, destination string) error {
	if err := os.Rename(source, destination); err == nil {
		return nil
	}
	if err := copyFile(source, destination); err != nil {
		return err
	}
	return os.Remove(source)
}

// copyFile writes the bytes and waits for them to land. A pack that is still in the page
// cache when somebody pulls the stick out is not a pack.
func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(destination)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(destination)
		return err
	}
	return out.Close()
}
