package ops

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/snapshot"
)

func (s *Service) checkpointManaged(
	ctx context.Context,
	project string,
	descriptor eco.Descriptor,
	created time.Time,
	yield func(snapshot.Entry) error,
) error {
	root, err := s.Blobs.ManagedDir(descriptor.ID, project)
	if err != nil {
		return err
	}
	var trees []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root || !entry.IsDir() {
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".") && strings.Contains(entry.Name(), ".clone-") {
			return filepath.SkipDir
		}
		if !strings.HasSuffix(entry.Name(), ".git") {
			return nil
		}
		// No HEAD means this .git directory is not a usable mirror — a partial clone or
		// a leftover. Skip it rather than failing the whole checkpoint.
		if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
			return nil //nolint:nilerr // a directory without HEAD is not a mirror
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		trees = append(trees, filepath.ToSlash(relative))
		return filepath.SkipDir
	})
	if err != nil {
		return fmt.Errorf("checkpoint: walk managed %s tree: %w", descriptor.ID, err)
	}
	sort.Strings(trees)
	for _, relative := range trees {
		digest, size, err := s.archiveManagedTree(
			ctx, filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return fmt.Errorf("checkpoint: archive %s/%s: %w", descriptor.ID, relative, err)
		}
		if err := s.Catalog.UpsertBlob(catalog.Blob{
			Digest: digest, Size: size, CreatedAt: created, LastAccess: created,
		}); err != nil {
			return err
		}
		if err := yield(snapshot.Entry{
			Eco: descriptor.ID, Key: snapshot.ManagedKeyPrefix + relative,
			Digest: digest, Size: size,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) archiveManagedTree(
	ctx context.Context, root string,
) (blob.Digest, int64, error) {
	writer, err := s.Blobs.Create()
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = writer.Abort() }()
	tw := tar.NewWriter(writer)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".lock") || strings.HasPrefix(name, "tmp_pack_") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink %s is not supported", path)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("special file %s is not supported", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relative)
		header.ModTime = time.Unix(0, 0).UTC()
		header.AccessTime, header.ChangeTime = time.Time{}, time.Time{}
		header.Uid, header.Gid, header.Uname, header.Gname = 0, 0, "", ""
		header.Format = tar.FormatPAX
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, contextReader{ctx: ctx, reader: file})
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		_ = tw.Close()
		return "", 0, err
	}
	if err := tw.Close(); err != nil {
		return "", 0, err
	}
	return writer.Commit()
}

type managedStage struct {
	eco     string
	root    string
	staging string
	backup  string
	swapped bool
}

func (s *Service) applySnapshot(
	ctx context.Context,
	project string,
	expectedHead *string,
	target snapshot.Meta,
) error {
	stages, err := s.stageManaged(ctx, project, target)
	if err != nil {
		return err
	}
	defer func() {
		for _, stage := range stages {
			_ = os.RemoveAll(stage.staging)
			if !stage.swapped {
				_ = os.RemoveAll(stage.backup)
			}
		}
	}()
	if err := swapManaged(stages); err != nil {
		// Report both: a partial swap that also failed to roll back has left trees on
		// disk an operator needs to know about, and hiding that behind the swap error
		// would make the mirror state look merely unchanged.
		if rollbackErr := rollbackManaged(stages); rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		return err
	}
	applyErr := s.applyCatalogManifest(ctx, project, expectedHead, target)
	if applyErr != nil {
		if rollbackErr := rollbackManaged(stages); rollbackErr != nil {
			return errors.Join(applyErr, rollbackErr)
		}
		return applyErr
	}
	for _, stage := range stages {
		if err := os.RemoveAll(stage.backup); err != nil {
			return fmt.Errorf("snapshot: remove old managed %s tree: %w", stage.eco, err)
		}
		stage.swapped = false
	}
	return nil
}

func (s *Service) stageManaged(
	ctx context.Context, project string, target snapshot.Meta,
) ([]*managedStage, error) {
	var stages []*managedStage
	byEco := make(map[string]*managedStage)
	for _, descriptor := range s.Ecos.Descriptors() {
		if descriptor.Storage != eco.StorageManagedDir {
			continue
		}
		root, err := s.Blobs.ManagedDir(descriptor.ID, project)
		if err != nil {
			return nil, err
		}
		staging, err := os.MkdirTemp(filepath.Dir(root), "."+project+"-restore-*")
		if err != nil {
			return nil, err
		}
		stage := &managedStage{eco: descriptor.ID, root: root, staging: staging}
		stages = append(stages, stage)
		byEco[descriptor.ID] = stage
	}
	cleanup := true
	defer func() {
		if cleanup {
			for _, stage := range stages {
				_ = os.RemoveAll(stage.staging)
			}
		}
	}()
	file, _, err := s.Blobs.Open(target.Manifest)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	iterator, err := snapshot.NewIterator(file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = iterator.Close() }()
	if iterator.Header.Project != project {
		return nil, errors.New("snapshot: manifest project does not match")
	}
	count, bytes, err := iterator.Walk(func(entry snapshot.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		stat, ok := s.Blobs.Stat(entry.Digest)
		if !ok {
			return fmt.Errorf("snapshot: missing blob %s", entry.Digest)
		}
		if stat.Size != entry.Size {
			return fmt.Errorf("snapshot: blob %s size mismatch", entry.Digest)
		}
		relative, managed := snapshot.ManagedKey(entry.Key)
		stage := byEco[entry.Eco]
		if !managed || stage == nil {
			return nil
		}
		if err := safeManagedRelative(relative); err != nil {
			return err
		}
		destination := filepath.Join(stage.staging, filepath.FromSlash(relative))
		if _, err := os.Lstat(destination); err == nil {
			return fmt.Errorf("snapshot: duplicate managed tree %s/%s", entry.Eco, relative)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(destination, 0o755); err != nil {
			return err
		}
		return s.extractManaged(ctx, entry.Digest, destination)
	})
	if err != nil {
		return nil, err
	}
	if count != target.EntryCount || bytes != target.TotalBytes {
		return nil, errors.New("snapshot: manifest totals do not match snapshot metadata")
	}
	cleanup = false
	return stages, nil
}

func (s *Service) applyCatalogManifest(
	ctx context.Context,
	project string,
	expectedHead *string,
	target snapshot.Meta,
) error {
	file, _, err := s.Blobs.Open(target.Manifest)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	iterator, err := snapshot.NewIterator(file)
	if err != nil {
		return err
	}
	defer func() { _ = iterator.Close() }()
	source := func(yield func(catalog.Entry) error) error {
		_, _, err := iterator.Walk(func(entry snapshot.Entry) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, managed := snapshot.ManagedKey(entry.Key); managed && s.managedEco(entry.Eco) {
				return nil
			}
			return yield(catalog.Entry{
				EntryKey: catalog.EntryKey{
					Project: project, Eco: entry.Eco, Key: entry.Key,
				},
				Digest: entry.Digest, Size: entry.Size,
			})
		})
		return err
	}
	if expectedHead != nil {
		return s.Catalog.ApplySnapshotFrom(project, *expectedHead, target.ID, source)
	}
	return s.Catalog.ApplySnapshot(project, target.ID, source)
}

func (s *Service) managedEco(id string) bool {
	ecosystem, ok := s.Ecos.Get(id)
	return ok && ecosystem.Descriptor().Storage == eco.StorageManagedDir
}

func swapManaged(stages []*managedStage) error {
	for _, stage := range stages {
		backup, err := os.MkdirTemp(filepath.Dir(stage.root),
			"."+filepath.Base(stage.root)+"-previous-*")
		if err != nil {
			return err
		}
		if err := os.Remove(backup); err != nil {
			return err
		}
		stage.backup = backup
		if err := os.Rename(stage.root, stage.backup); err != nil {
			return fmt.Errorf("snapshot: preserve managed %s tree: %w", stage.eco, err)
		}
		if err := os.Rename(stage.staging, stage.root); err != nil {
			_ = os.Rename(stage.backup, stage.root)
			return fmt.Errorf("snapshot: install managed %s tree: %w", stage.eco, err)
		}
		stage.swapped = true
	}
	return nil
}

func rollbackManaged(stages []*managedStage) error {
	var errs []error
	for index := len(stages) - 1; index >= 0; index-- {
		stage := stages[index]
		if !stage.swapped {
			continue
		}
		failed := stage.staging
		if err := os.Rename(stage.root, failed); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(stage.backup, stage.root); err != nil {
			errs = append(errs, err)
			_ = os.Rename(failed, stage.root)
			continue
		}
		stage.swapped = false
		_ = os.RemoveAll(failed)
	}
	return errors.Join(errs...)
}

func (s *Service) extractManaged(
	ctx context.Context, digest blob.Digest, destination string,
) error {
	file, _, err := s.Blobs.Open(digest)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	tr := tar.NewReader(file)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		clean := filepath.Clean(filepath.FromSlash(header.Name))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." ||
			strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("snapshot: unsafe managed member %q", header.Name)
		}
		path := filepath.Join(destination, clean)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, os.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY,
				os.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, contextReader{ctx: ctx, reader: tr})
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("snapshot: unsupported managed member type %d", header.Typeflag)
		}
	}
}

func safeManagedRelative(relative string) error {
	clean := filepath.Clean(filepath.FromSlash(relative))
	if filepath.IsAbs(clean) || clean == "." || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) ||
		!strings.HasSuffix(clean, ".git") {
		return fmt.Errorf("snapshot: unsafe managed tree %q", relative)
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(p)
	}
}
