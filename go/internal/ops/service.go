// Package ops implements the durable operational jobs.
package ops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control"
	"github.com/aabdlwahab/PKGCache/internal/control/job"
	controlproject "github.com/aabdlwahab/PKGCache/internal/control/project"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/lockwarm"
	"github.com/aabdlwahab/PKGCache/internal/obs"
	"github.com/aabdlwahab/PKGCache/internal/snapshot"
)

var hostRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

// Service owns the collaborators shared by every air-gap job runner.
type Service struct {
	Catalog  *catalog.DB
	Blobs    *blob.Store
	Config   *config.Store
	Projects *controlproject.Service
	Ecos     *eco.Registry
	Data     http.Handler
	DataDir  string
	Now      func() time.Time
	// Events carries pack progress to anything watching. Nil is silent, which is what
	// a test wants and what a build without a bus gets.
	Events *obs.Bus
}

// packProgress reports one transfer's progress onto the bus.
//
// Frames rather than job log lines because a bar is a thing being watched now: the log
// is what the job did, and by the time somebody reads it the bar is not needed. The job
// id is the frame's id, so a window can tell one export from another export started
// beside it.
//
// Nil bus, nil function: the pack writer skips the call entirely rather than publishing
// into nothing on every twenty-fifth blob.
func (s *Service) packProgress(record control.Job) func(done, total int64) {
	if s.Events == nil {
		return nil
	}
	return func(done, total int64) {
		s.Events.Publish(obs.Event{
			Kind: obs.EventPackProgress, Project: record.Project,
			ID: fmt.Sprint(record.ID), Name: record.Action, Size: done, Total: total,
		})
	}
}

// Register installs every Phase 8 action on the durable manager.
func (s *Service) Register(manager *job.Manager) {
	manager.Register("checkpoint", s.checkpointJob)
	manager.Register("snapshot", s.checkpointJob)
	manager.Register("rollback", s.rollbackJob)
	manager.Register("export", s.exportJob)
	manager.Register("import", s.importJob)
	manager.Register("lockwarm", s.lockwarmJob)
}

func (s *Service) checkpointJob(
	ctx context.Context, record control.Job, logf func(string),
) error {
	message := stringParam(record.Params, "message")
	if message == "" {
		return errors.New("checkpoint: a message is required")
	}
	if _, err := s.Projects.Get(record.Project); err != nil {
		return err
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	created := s.Now().UTC()
	parent, err := s.Catalog.GetHead(record.Project)
	if err != nil {
		return err
	}
	logf("streaming catalog entries into the manifest")
	writer, err := s.Blobs.Create()
	if err != nil {
		return err
	}
	defer func() { _ = writer.Abort() }()
	count, bytes, err := snapshot.WriteManifest(writer, snapshot.Header{
		Project: record.Project, Created: created,
	}, func(yield func(snapshot.Entry) error) error {
		descriptors := s.Ecos.Descriptors()
		sort.Slice(descriptors, func(i, j int) bool {
			return descriptors[i].ID < descriptors[j].ID
		})
		for _, descriptor := range descriptors {
			err := s.Catalog.WalkEntriesEco(
				record.Project, descriptor.ID, func(entry catalog.Entry) error {
					if err := ctx.Err(); err != nil {
						return err
					}
					if descriptor.Storage == eco.StorageManagedDir &&
						entry.Key >= snapshot.ManagedKeyPrefix {
						return fmt.Errorf(
							"checkpoint: %s key %q conflicts with managed snapshot namespace",
							descriptor.ID, entry.Key)
					}
					return yield(snapshot.Entry{
						Eco: entry.Eco, Key: entry.Key,
						Digest: entry.Digest, Size: entry.Size,
					})
				})
			if err != nil {
				return err
			}
			if descriptor.Storage == eco.StorageManagedDir {
				if err := s.checkpointManaged(
					ctx, record.Project, descriptor, created, yield); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	digest, manifestBytes, err := writer.Commit()
	if err != nil {
		return err
	}
	if err := s.Catalog.UpsertBlob(catalog.Blob{
		Digest: digest, Size: manifestBytes, CreatedAt: created, LastAccess: created,
	}); err != nil {
		return err
	}
	checkpoint := catalog.Snapshot{
		ID: string(digest), Project: record.Project, Parent: parent, Manifest: digest,
		EntryCount: count, TotalBytes: bytes, CreatedAt: created,
		Subject: message, Author: record.Actor,
	}
	if err := s.Catalog.CommitSnapshot(checkpoint); err != nil {
		return err
	}
	logf(fmt.Sprintf("checkpoint %s: %d entries, %d bytes", digest, count, bytes))
	return nil
}

func (s *Service) rollbackJob(
	ctx context.Context, record control.Job, logf func(string),
) error {
	id := stringParam(record.Params, "snapshot")
	if id == "" {
		id = stringParam(record.Params, "commit")
	}
	if id == "" {
		return errors.New("rollback: a snapshot id is required")
	}
	target, err := s.Catalog.GetSnapshot(id)
	if err != nil {
		return err
	}
	if target.Project != record.Project {
		return fmt.Errorf("rollback: snapshot %s belongs to project %s", id, target.Project)
	}
	logf("verifying snapshot blobs")
	meta := snapshot.Meta{
		ID: target.ID, Project: target.Project, Parent: target.Parent,
		Manifest: target.Manifest, EntryCount: target.EntryCount,
		TotalBytes: target.TotalBytes, CreatedAt: target.CreatedAt,
		Subject: target.Subject, Author: target.Author,
	}
	if err := s.applySnapshot(ctx, record.Project, nil, meta); err != nil {
		return err
	}
	logf(fmt.Sprintf("restored snapshot %s (%d entries)", id, target.EntryCount))
	return nil
}

func (s *Service) exportJob(
	ctx context.Context, record control.Job, logf func(string),
) error {
	if _, err := s.Projects.Get(record.Project); err != nil {
		return err
	}
	outDir := filepath.Join(s.DataDir, "shuttle", "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(outDir, ".pkgreg-export-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	logf("building streamed transfer pack")
	pack, err := snapshot.WritePack(ctx, temp, s.Catalog, s.Blobs, snapshot.ExportOptions{
		Project:  record.Project,
		Base:     stringParam(record.Params, "base"),
		Target:   stringParam(record.Params, "target"),
		CertDir:  filepath.Join(s.DataDir, "certs"),
		Logf:     logf,
		Progress: s.packProgress(record),
	})
	if err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	name := stringParam(record.Params, "file")
	// Whether the name was chosen here decides what an existing file at that path means,
	// which is why this is remembered rather than recomputed from the string.
	defaulted := name == ""
	if defaulted {
		name = defaultPackName(record.Project, pack, s.clock())
	}
	if filepath.Base(name) != name || !strings.HasSuffix(name, packExtension) {
		return errors.New("export: file must be a .tar basename")
	}
	// A directory somebody chose in the window. The name stays a basename either way —
	// the two halves are separate so that "which pack" and "which place" cannot be
	// confused, and so a caller that supplies neither still gets the generated name in
	// the cache's own outbox.
	//
	// Absolute only, and checked here rather than trusted: a relative directory would
	// resolve against the daemon's working directory, which is not anywhere the person
	// choosing it is standing.
	destination := outDir
	if chosen := stringParam(record.Params, "dir"); chosen != "" {
		if !filepath.IsAbs(chosen) {
			return fmt.Errorf("export: %s is not an absolute directory", chosen)
		}
		info, err := os.Stat(chosen)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("export: %s is not a directory", chosen)
		}
		destination = chosen
	}
	finalPath := filepath.Join(destination, name)
	if err := publishPack(tempPath, finalPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			// A generated name carries the kind, both checkpoints and the second it was
			// made in, so a file already at that path is this exact pack, written inside
			// the same second. Refusing would fail a scripted export loop over a file
			// identical to the one it was about to write.
			//
			// This used to be narrowed to full packs, because the old name carried the
			// target and nothing else, so a delta and a full pack of the same checkpoint
			// were the same name for different content. The name distinguishes them now,
			// so the narrowing is gone with the ambiguity that forced it.
			//
			// A name somebody typed stays a refusal: it is a place they chose, and being
			// told it is taken is the answer they want.
			if defaulted {
				logf(fmt.Sprintf("wrote %s (already exported; %d blobs, %d bytes)",
					finalPath, pack.Blobs, pack.Bytes))
				return nil
			}
			return fmt.Errorf("export: %s already exists; remove or rename it first", finalPath)
		}
		return fmt.Errorf("export: publish pack: %w", err)
	}
	logf(fmt.Sprintf("wrote %s (%d blobs, %d bytes)", finalPath, pack.Blobs, pack.Bytes))
	return nil
}

func (s *Service) importJob(
	ctx context.Context, record control.Job, logf func(string),
) error {
	path, err := s.importPath(stringParam(record.Params, "file"))
	if err != nil {
		return err
	}
	inspect, err := os.Open(path)
	if err != nil {
		return err
	}
	pack, inspectErr := snapshot.InspectPack(inspect, record.Project)
	_ = inspect.Close()
	if inspectErr != nil {
		return inspectErr
	}
	createdProject := false
	if _, err := s.Projects.Get(pack.Project); err != nil {
		var clientErr *control.Error
		if !errors.As(err, &clientErr) || clientErr.Code != "project_not_found" {
			return err
		}
		if _, err := s.Projects.Create(pack.Project, record.Actor); err != nil {
			return err
		}
		createdProject = true
		logf(fmt.Sprintf("registered project %q", pack.Project))
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	logf(fmt.Sprintf("verifying and applying %s", path))
	imported, err := snapshot.ReadPack(ctx, file, s.Catalog, s.Blobs, snapshot.ImportOptions{
		Project: pack.Project, CertDir: filepath.Join(s.DataDir, "certs"), Logf: logf,
		Progress: s.packProgress(record),
		Apply: func(ctx context.Context, pack snapshot.Pack, target snapshot.Meta) error {
			base := pack.Base
			return s.applySnapshot(ctx, pack.Project, &base, target)
		},
	})
	if err != nil {
		if createdProject {
			_ = s.Projects.Delete(pack.Project)
		}
		return err
	}
	logf(fmt.Sprintf("imported checkpoint %s (%d blobs, %d bytes)",
		imported.Target, imported.Blobs, imported.Bytes))
	return nil
}

func (s *Service) lockwarmJob(
	ctx context.Context, record control.Job, logf func(string),
) error {
	lock := stringParam(record.Params, "lock")
	if strings.TrimSpace(lock) == "" {
		return errors.New("lockwarm: a uv.lock file is required")
	}
	host := stringParam(record.Params, "host")
	if !validHost(host) {
		return errors.New("lockwarm: host must be a bare hostname or IP address")
	}
	if _, err := s.Projects.Get(record.Project); err != nil {
		return err
	}
	if s.Config.Current().OfflineFor(record.Project) {
		return errors.New("lockwarm: cache is offline")
	}
	packages, err := lockwarm.Parse(lock)
	if err != nil {
		return err
	}
	if len(packages) == 0 {
		logf("no registry-sourced packages found")
		return nil
	}
	ecosystem, ok := s.Ecos.Get("pypi")
	if !ok {
		return errors.New("lockwarm: PyPI ecosystem is unavailable")
	}
	indexes := make(map[string]string)
	for name, upstream := range ecosystem.Descriptor().DefaultUpstreams {
		indexes[name] = upstream
	}
	// The head of each chain. Lock warming resolves an index to decide what to fetch,
	// and the fetch itself goes through the engine, which walks the rest of the chain
	// if the head is unreachable.
	for name, chain := range s.Config.Current().ProjectUpstreams[record.Project]["pypi"] {
		if len(chain) > 0 {
			indexes[name] = chain[0].URL
		}
	}
	indexMap := lockwarm.NewIndexMap(indexes)
	for _, pkg := range packages {
		if _, ok := indexMap.Index(pkg.Registry); !ok {
			return fmt.Errorf("lockwarm: no configured PyPI index for %s", pkg.Registry)
		}
	}
	total := 0
	for _, pkg := range packages {
		total += len(pkg.Files)
	}
	logf(fmt.Sprintf("warming %d files from %d packages", total, len(packages)))
	if err := lockwarm.Warm(ctx, s.Data, record.Project, packages, indexMap, 8,
		func(result lockwarm.Result) {
			if result.Err != nil {
				logf(fmt.Sprintf("FAIL %s: %v", result.Filename, result.Err))
			} else if result.Status != http.StatusOK {
				logf(fmt.Sprintf("FAIL %s: HTTP %d", result.Filename, result.Status))
			} else {
				logf("cached " + result.Filename)
			}
		}); err != nil {
		return err
	}
	port := addressPort(s.Config.Current().Server.UnifiedAddr)
	if port == 0 {
		return errors.New("lockwarm: unified listener has no configured port")
	}
	publicBase := "https://" + net.JoinHostPort(host, strconv.Itoa(port)) +
		"/" + record.Project + "/pypi"
	rewritten := lockwarm.Rewrite(lock, packages, indexMap, publicBase)
	outDir := filepath.Join(s.DataDir, "lockwarm", record.Project)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(outDir, ".uv.lock-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if _, err := temp.WriteString(rewritten); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempPath, 0o644); err != nil {
		return err
	}
	out := filepath.Join(outDir, "uv.lock")
	if err := os.Rename(tempPath, out); err != nil {
		return err
	}
	logf("rewritten lock available at " + out)
	return nil
}

func (s *Service) importPath(name string) (string, error) {
	inDir := filepath.Join(s.DataDir, "shuttle", "in")
	// An absolute path is a pack somebody pointed at — on the stick it arrived on,
	// usually, rather than one they copied into the inbox first. Accepted only in that
	// form: a relative path would resolve against the daemon's working directory, which
	// is nowhere the person choosing it is standing.
	if filepath.IsAbs(name) {
		if !strings.HasSuffix(name, packExtension) {
			return "", fmt.Errorf("import: %s is not a .tar pack", name)
		}
		if _, err := os.Stat(name); err != nil {
			return "", err
		}
		return filepath.Clean(name), nil
	}
	if name != "" {
		if filepath.Base(name) != name || !strings.HasSuffix(name, packExtension) {
			return "", errors.New("import: file must be a .tar basename or an absolute path")
		}
		return filepath.Join(inDir, name), nil
	}
	entries, err := os.ReadDir(inDir)
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tar") {
			candidates = append(candidates, entry.Name())
		}
	}
	sort.Strings(candidates)
	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("import: no .tar pack found in %s", inDir)
	case 1:
		return filepath.Join(inDir, candidates[0]), nil
	default:
		return "", fmt.Errorf("import: multiple packs found in %s; specify file", inDir)
	}
}

func stringParam(params map[string]any, name string) string {
	value, _ := params[name].(string)
	return strings.TrimSpace(value)
}

// packExtension is the only thing a pack is ever called.
const packExtension = ".tar"

// publishPack puts the finished pack at its destination without overwriting anything.
//
// A hard link is the cheap path and the one that has always been taken, because the
// staging file and the outbox are the same filesystem. A chosen directory usually is
// not — that is the entire point of choosing one, it is a USB stick — and a link across
// devices fails with EXDEV. So the copy is the fallback, and it is opened O_EXCL so that
// the "already exists" answer is the same on both paths rather than depending on which
// one happened to run.
func publishPack(from, to string) error {
	if err := os.Link(from, to); err == nil || errors.Is(err, os.ErrExist) {
		return err
	}
	source, err := os.Open(from) // #nosec G304 -- a temp file this job just wrote.
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	// #nosec G304 -- the destination the operator chose.
	target, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		_ = os.Remove(to)
		return err
	}
	if err := target.Sync(); err != nil {
		_ = target.Close()
		_ = os.Remove(to)
		return err
	}
	return target.Close()
}

// packNameTime is the timestamp in a generated pack name: ISO 8601 basic, UTC.
//
// Basic rather than extended because a colon is a path separator on Windows and a
// separator in half the tools that would carry one of these files. Sorting a directory by
// name then sorts by project, then kind, then time — the order somebody hunting for "the
// last full pack of work" reads them in.
const packNameTime = "20060102T150405Z"

// defaultPackName names a pack after what is in it.
//
// The old name was pkgreg-<project>-<target>.tar, which said which checkpoint and nothing
// else. A full pack and a delta ending at the same checkpoint got the same name for
// entirely different content, and nothing anywhere recorded when either was made — so a
// drawer of these files could not be told apart without opening them. The kind is named,
// the base is named where there is one, and the time is stamped, because none of that is
// something a person should have to add by hand to a file they are about to carry
// somewhere and read back weeks later.
//
//	pkgreg-work-full-20260906T153045Z-241f47c5ec0f.tar
//	pkgreg-work-delta-20260906T153045Z-7b69cf291a04-241f47c5ec0f.tar
//
// Project names are [a-z0-9._-] and the rest is hex and digits, so the result is always a
// safe basename; the caller checks that anyway, because a name it did not generate is not
// covered by that argument.
func defaultPackName(project string, pack snapshot.Pack, now time.Time) string {
	kind, ids := "full", shortID(pack.Target)
	if pack.Base != "" {
		kind, ids = "delta", shortID(pack.Base)+"-"+shortID(pack.Target)
	}
	return fmt.Sprintf("pkgreg-%s-%s-%s-%s.tar",
		project, kind, now.UTC().Format(packNameTime), ids)
}

// clock reads the service's time source without installing one.
//
// checkpointJob sets s.Now when it finds it nil, which is fine there and would be a data
// race here: exports and checkpoints run as separate jobs.
func (s *Service) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func validHost(host string) bool {
	if host == "" || strings.ContainsAny(host, "/[]") {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	return !strings.ContainsAny(host, ":[]") && hostRE.MatchString(host)
}

func addressPort(address string) int {
	_, raw, err := net.SplitHostPort(address)
	if err != nil {
		return 0
	}
	port, _ := strconv.Atoi(raw)
	return port
}
