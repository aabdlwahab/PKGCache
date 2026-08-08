// Package ops implements the durable operational jobs.
package ops

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brightskies/pkgreg/internal/blob"
	"github.com/brightskies/pkgreg/internal/catalog"
	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/control"
	"github.com/brightskies/pkgreg/internal/control/job"
	controlproject "github.com/brightskies/pkgreg/internal/control/project"
	"github.com/brightskies/pkgreg/internal/eco"
	"github.com/brightskies/pkgreg/internal/lockwarm"
	"github.com/brightskies/pkgreg/internal/snapshot"
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
		Project: record.Project,
		Base:    stringParam(record.Params, "base"),
		Target:  stringParam(record.Params, "target"),
		CertDir: filepath.Join(s.DataDir, "certs"),
		Logf:    logf,
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
	if name == "" {
		name = fmt.Sprintf("pkgreg-%s-%s.tar", record.Project, shortID(pack.Target))
	}
	if filepath.Base(name) != name || !strings.HasSuffix(name, ".tar") {
		return errors.New("export: file must be a .tar basename")
	}
	finalPath := filepath.Join(outDir, name)
	if err := os.Link(tempPath, finalPath); err != nil {
		if errors.Is(err, os.ErrExist) {
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
	if name != "" {
		if filepath.Base(name) != name || !strings.HasSuffix(name, ".tar") {
			return "", errors.New("import: file must be a .tar basename")
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
