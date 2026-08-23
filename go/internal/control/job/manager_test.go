package job

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/control"
	"github.com/aabdlwahab/PKGCache/internal/obs"
)

func TestProjectsRunConcurrentlyButOneProjectStaysOrdered(t *testing.T) {
	db, err := control.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager, err := New(db, obs.NewBus(), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	started := make(chan string, 3)
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	manager.Register("block", func(ctx context.Context, record control.Job, logf func(string)) error {
		started <- record.Project
		if record.Project == "a" {
			select {
			case <-releaseA:
			case <-ctx.Done():
				return ctx.Err()
			}
		} else {
			select {
			case <-releaseB:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		logf("done")
		return nil
	})
	a1, _ := manager.Submit("a", "block", "root", nil)
	a2, _ := manager.Submit("a", "block", "root", nil)
	b1, _ := manager.Submit("b", "block", "root", nil)

	seen := map[string]int{}
	for range 2 {
		select {
		case project := <-started:
			seen[project]++
		case <-time.After(time.Second):
			t.Fatal("different projects did not occupy the pool concurrently")
		}
	}
	if seen["a"] != 1 || seen["b"] != 1 {
		t.Fatalf("started before release = %+v; project a must remain ordered", seen)
	}
	close(releaseA)
	select {
	case project := <-started:
		if project != "a" {
			t.Fatalf("third start = %q", project)
		}
	case <-time.After(time.Second):
		t.Fatal("second project-a job did not start after first completed")
	}
	close(releaseB)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		one, _ := manager.Get(a1.ID)
		two, _ := manager.Get(a2.ID)
		three, _ := manager.Get(b1.ID)
		if one.Status == "done" && two.Status == "done" && three.Status == "done" {
			if one.Log != "done\n" || two.Log != "done\n" {
				t.Fatalf("logs were not persisted: %q %q", one.Log, two.Log)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("jobs did not finish")
}

func TestCancelRunningJob(t *testing.T) {
	db, err := control.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager, err := New(db, obs.NewBus(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	started := make(chan struct{})
	manager.Register("wait", func(ctx context.Context, _ control.Job, _ func(string)) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	record, err := manager.Submit("a", "wait", "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := manager.Cancel(record.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got, _ := manager.Get(record.ID)
		if got.Status == "cancelled" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("running job did not become cancelled")
}
