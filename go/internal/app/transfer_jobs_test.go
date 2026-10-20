package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/control"
)

// An export packs a checkpoint, so a project nobody has checkpointed has nothing to
// export. The console answered that with "Export started" and a job that then failed; it
// now asks the job to checkpoint first, which is what `pkgcache export` has always done.
// Asked for, not assumed: without the flag an export stays a read.
func TestExportCheckpointsFirstOnlyWhenAsked(t *testing.T) {
	instance := newApp(t)
	store(t, instance, "global", "files", "notes.txt", []byte("first"))

	plain, err := instance.Jobs.Submit("global", "export", "tester", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if record := waitJobSettled(t, instance, plain.ID); record.Status != "failed" ||
		!strings.Contains(record.Error, "no checkpoint") {
		t.Fatalf("an export of a never-checkpointed project = %s %q; want the refusal",
			record.Status, record.Error)
	}

	for round, want := range []int64{1, 2} {
		if round == 1 {
			// Something cached since the first pack, which only a new checkpoint carries.
			store(t, instance, "global", "files", "more.txt", []byte("second"))
		}
		job, err := instance.Jobs.Submit("global", "export", "tester",
			map[string]any{"checkpoint": true})
		if err != nil {
			t.Fatal(err)
		}
		record := waitJob(t, instance, job.ID)
		head, err := instance.Catalog.GetHead("global")
		if err != nil {
			t.Fatal(err)
		}
		// The log names the checkpoint it packed, and it is the one just taken.
		if head == "" || !strings.Contains(record.Log, "packed checkpoint "+head) {
			t.Fatalf("round %d: head %q is not what the export says it packed:\n%s",
				round, head, record.Log)
		}
		taken, err := instance.Catalog.GetSnapshot(head)
		if err != nil {
			t.Fatal(err)
		}
		if taken.EntryCount != want {
			t.Fatalf("round %d: the checkpoint holds %d entries, want %d",
				round, taken.EntryCount, want)
		}
	}
	if rows, _ := instance.Catalog.ListSnapshots("global", 100); len(rows) != 2 {
		t.Fatalf("two checkpoint-first exports left %d checkpoints, want 2", len(rows))
	}
}

// A name the export was always going to refuse must not cost a checkpoint first.
func TestExportRefusesABadNameBeforeCheckpointing(t *testing.T) {
	instance := newApp(t)
	store(t, instance, "global", "files", "notes.txt", []byte("first"))
	job, err := instance.Jobs.Submit("global", "export", "tester",
		map[string]any{"checkpoint": true, "file": "../escape.tar"})
	if err != nil {
		t.Fatal(err)
	}
	if record := waitJobSettled(t, instance, job.ID); record.Status != "failed" {
		t.Fatalf("export named ../escape.tar = %s", record.Status)
	}
	if rows, _ := instance.Catalog.ListSnapshots("global", 100); len(rows) != 0 {
		t.Fatalf("a refused export left %d checkpoint(s) behind", len(rows))
	}
}

// The rewritten lock names this instance in the scheme it serves — plain HTTP here, as on
// every pkgcache — and the versioned API hands it out, which is what the console's
// download link reads.
func TestLockwarmRewritesToThisSchemeAndServesTheLock(t *testing.T) {
	a := controlApp(t)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()
	cookie := controlLogin(t, server)
	download := server.URL + "/api/v1/projects/global/lockwarm/uv.lock"

	if response, _ := controlRequest(t, server.Client(), http.MethodGet, download,
		cookie, ""); response.StatusCode != http.StatusNotFound {
		t.Fatalf("before any warm, the lock = %d; want 404", response.StatusCode)
	}

	// An index with one wheel on it, so the warm succeeds without the network: a lock
	// is rewritten only once everything it pins is cached.
	wheel := "idna-3.10-py3-none-any.whl"
	mux := http.NewServeMux()
	mux.HandleFunc("/simple/idna/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<a href="/files/` + wheel + `">` + wheel + `</a>`))
	})
	mux.HandleFunc("/files/"+wheel, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("a wheel"))
	})
	origin := httptest.NewServer(mux)
	defer origin.Close()
	if _, err := a.Projects.AddUpstream("global", control.Upstream{
		Eco: "pypi", Name: "root/pypi", URL: origin.URL + "/simple",
		Kind: "origin", Priority: 10, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	lock := "version = 1\nrevision = 3\n\n[[package]]\nname = \"idna\"\n" +
		"version = \"3.10\"\nsource = { registry = \"" + origin.URL + "/simple\" }\n" +
		"wheels = [\n    { url = \"" + origin.URL + "/files/" + wheel + "\", " +
		"hash = \"sha256:bbb\", size = 20 },\n]\n"
	job, err := a.Jobs.Submit("global", "lockwarm", "root",
		map[string]any{"lock": lock, "host": "cache.internal"})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, a, job.ID)

	response, body := controlRequest(t, server.Client(), http.MethodGet, download, cookie, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("rewritten lock = %d %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), `registry = "http://cache.internal:`) ||
		!strings.Contains(string(body), `/global/pypi/root/pypi/+simple"`) ||
		!strings.Contains(string(body), `/global/pypi/root/pypi/+f/idna/`) {
		t.Fatalf("the lock does not name this plain-HTTP instance:\n%s", body)
	}
}
