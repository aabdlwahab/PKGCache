package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/diskusage"
)

// Moving a cache to another disk.
//
// This is a command rather than a paragraph of documentation because each line that
// paragraph would contain has a way to go wrong that a person finds out about much
// later:
//
//   - a copy made while the daemon is running catches SQLite between two writes, and
//     the cache that arrives is subtly not the one that left;
//   - a copy that does not preserve hardlinks silently inflates a deduplicated store,
//     which on a cache holding the same wheel for four projects is not a rounding error;
//   - a destination on exFAT or a network filesystem accepts every byte and then cannot
//     hardlink a blob into place, so the cache breaks on the first fetch after the move,
//     when nobody is thinking about the move any more;
//   - and a cache that arrives intact is still lost if nothing else on the machine knows
//     where it went.
//
// The copy is the easy half. Refusing the first three and answering the fourth is the
// feature.

// stagingDir is the one part of a cache that is deliberately left behind: in-flight
// downloads, which by definition nothing references yet. It mirrors the layout owned by
// internal/blob, which recreates it on the first open.
var stagingDir = filepath.Join("blobs", "staging")

// transient names the files that belong to a running daemon rather than to a cache.
// Copying them would carry a state file naming a pid on the machine we are leaving, and
// a lock this process is holding.
var transient = map[string]bool{
	filepath.Base(StatePath("")):     true,
	filepath.Base(LockPath("")):      true,
	filepath.Base(StartLockPath("")): true,
}

// MigrateOptions describes one move.
type MigrateOptions struct {
	// From is the cache as it is now.
	From string
	// To is where it should be. It must not exist, or must be empty.
	To string
	// Keep leaves the old copy in place. The default removes it, because the reason
	// people move a cache is that the disk holding it is full.
	Keep bool
	// DryRun reports what would happen and changes nothing.
	DryRun bool
	// Signpost is the directory to leave the file that sends every later pkgcache to
	// the new location. It is the location this user's cache is found at by default —
	// which is where From was on a first move, and stays there on a second, since a
	// signpost anywhere else is one nothing reads. Empty writes none.
	Signpost string
	// Out receives progress. A multi-gigabyte copy that prints nothing is
	// indistinguishable from one that has hung.
	Out io.Writer
}

// MigrateResult is what the move did, for a caller that has to report it.
type MigrateResult struct {
	From, To string
	// Bytes and Files are what the cache holds, not what crossed the wire: a rename
	// moves the same tree without copying a byte of it.
	Bytes int64
	Files int
	// Renamed records that source and destination shared a filesystem, so the move was
	// a rename. Worth saying out loud — somebody who asked for a move onto another disk
	// and got an instant one has not got what they asked for.
	Renamed bool
	// Kept records that the old copy is still there, taking up the space the move was
	// meant to reclaim.
	Kept bool
	// Signpost is where the file naming the new location was written, if any.
	Signpost string
}

// Migrate moves a cache directory to another location.
func Migrate(ctx context.Context, o MigrateOptions) (MigrateResult, error) {
	from, to, err := migratePaths(o.From, o.To)
	if err != nil {
		return MigrateResult{}, err
	}
	result := MigrateResult{From: from, To: to, Kept: o.Keep}

	if info, err := os.Stat(from); err != nil || !info.IsDir() {
		return result, fmt.Errorf("local: there is no cache at %s", from)
	}

	// The lock, not a look at the state file, is what makes this safe. A daemon that
	// exited without cleaning up leaves a state file behind, and a daemon started by
	// socket activation two milliseconds ago has not written one yet; the lock is
	// wrong in neither direction, and holding it for the whole move is also what stops
	// one starting halfway through.
	//
	// A dry run does not take it. It changes nothing, so nothing needs excluding, and
	// refusing to describe a move because the cache is currently serving would make the
	// one command that is safe to run at any time the one that needs a stopped daemon.
	if !o.DryRun {
		lock, err := acquireIdle(ctx, from, 15*time.Second)
		if err != nil {
			return result, err
		}
		defer func() { _ = lock.Close() }()
	}

	held, err := measure(from)
	if err != nil {
		return result, err
	}
	result.Bytes, result.Files = held.bytes, held.files

	if err := prepareDestination(to); err != nil {
		return result, err
	}

	if o.DryRun {
		note(o.Out, "pkgcache: would move %s (%d files) from %s to %s\n",
			FormatSize(held.bytes), held.files, from, to)
		// The destination is asked the real questions where it already exists, because
		// "will this work" is the whole reason to run this first, and a probe on a
		// directory that is already there costs one file.
		if _, err := os.Stat(to); err == nil {
			if err := checkDestination(to, held.bytes); err != nil {
				return result, err
			}
			note(o.Out, "pkgcache: %s can hold it and can hardlink\n", to)
		}
		if !o.Keep {
			note(o.Out, "pkgcache: would then remove %s\n", from)
		}
		if o.Signpost != "" {
			note(o.Out, "pkgcache: would leave %s naming the new location\n",
				config.MovedToPath(o.Signpost))
		}
		if _, err := ReadState(from); err == nil {
			note(o.Out, "pkgcache: the cache is running; the move would stop it first\n")
		}
		return result, nil
	}

	// A rename first, because a move within one filesystem is one, and a person moving
	// a cache between two directories on the same disk should not wait for five
	// gigabytes to be read and written back.
	renamed, err := renameInto(from, to)
	if err != nil {
		return result, err
	}
	result.Renamed = renamed
	if renamed {
		result.Kept = false
		note(o.Out, "pkgcache: moved %s to %s (same filesystem, nothing was copied)\n",
			FormatSize(held.bytes), to)
	} else {
		if err := copyInto(ctx, from, to, held, o.Out); err != nil {
			return result, err
		}
		if !o.Keep {
			if err := os.RemoveAll(from); err != nil {
				return result, fmt.Errorf(
					"local: the copy at %s is complete, but %s could not be removed: %w",
					to, from, err)
			}
			result.Kept = false
		}
	}

	// The signpost goes in last, once there is somewhere for it to point.
	if o.Signpost != "" {
		if err := config.WriteMovedTo(o.Signpost, to); err != nil {
			return result, err
		}
		result.Signpost = config.MovedToPath(o.Signpost)
	} else if _, err := config.ClearMovedTo(to); err != nil {
		// Moving back to the default location: whatever sent us away from it is now
		// wrong, and leaving it would send the next command straight back out again.
		return result, err
	}
	return result, nil
}

// acquireIdle takes the cache's lock, waiting a little for a daemon on its way out.
//
// The wait is not politeness, it is a race this command would otherwise lose every
// time it did its job. Stop returns once the cache has stopped answering, and a process
// that has closed its listener still holds its flock until the kernel closes the
// descriptor — so a move that stops the daemon and takes the lock in the next breath
// finds it held by the daemon it just stopped.
func acquireIdle(ctx context.Context, dataDir string, wait time.Duration) (*Lock, error) {
	deadline := time.Now().Add(wait)
	for {
		lock, err := Acquire(LockPath(dataDir), false)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, ErrLocked) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf(
				"local: the cache at %s is in use.\n"+
					"  stop it first:  pkgcache stop", dataDir)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// migratePaths resolves both ends and rejects the pairs that cannot mean anything.
func migratePaths(from, to string) (string, string, error) {
	if strings.TrimSpace(to) == "" {
		return "", "", fmt.Errorf("local: name the directory to move the cache to")
	}
	absFrom, err := filepath.Abs(from)
	if err != nil {
		return "", "", err
	}
	absTo, err := filepath.Abs(to)
	if err != nil {
		return "", "", err
	}
	absFrom, absTo = filepath.Clean(absFrom), filepath.Clean(absTo)
	if absFrom == absTo {
		return "", "", fmt.Errorf("local: %s is already where the cache is", absTo)
	}
	if within(absTo, absFrom) {
		return "", "", fmt.Errorf("local: %s is inside the cache being moved", absTo)
	}
	if within(absFrom, absTo) {
		return "", "", fmt.Errorf("local: %s contains the cache being moved", absTo)
	}
	return absFrom, absTo, nil
}

// within reports whether path is under root.
func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// prepareDestination establishes that the destination is empty, treating a signpost as
// the nothing it is: moving a cache back to the location it left is a normal thing to
// do, and the file naming where it went is not data that would be overwritten.
func prepareDestination(to string) error {
	entries, err := os.ReadDir(to)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	if len(entries) == 1 && entries[0].Name() == config.MovedToName {
		return nil
	}
	if len(entries) > 0 {
		return fmt.Errorf(
			"local: %s is not empty.\n"+
				"  a cache is moved into a directory of its own, so that removing it later\n"+
				"  removes a cache and not somebody's files", to)
	}
	return nil
}

// checkDestination asks the destination filesystem the two questions that decide
// whether a cache can live on it, before gigabytes are written to find out.
func checkDestination(to string, need int64) error {
	if err := checkLinks(to); err != nil {
		return err
	}
	free, _, err := diskusage.Usage(to)
	if err != nil {
		// A filesystem that will not answer is not a reason to refuse to move onto it;
		// the copy will find out, and it will say so with the write that fails.
		return nil //nolint:nilerr // see above
	}
	if free < need {
		return fmt.Errorf(
			"local: %s has %s free and the cache is %s.\n"+
				"  reclaim space first (pkgcache prune), or move it somewhere larger",
			to, FormatSize(free), FormatSize(need))
	}
	return nil
}

// checkLinks refuses a destination that cannot hardlink.
//
// Not a proxy for a filesystem name, which is what a check on the mount type would be:
// the blob store publishes every blob with link(2) and deduplicates across projects the
// same way, so a filesystem without hardlinks is one where this cache does not work.
// exFAT, FAT32 and most SMB mounts fail here, which is exactly the set of disks people
// reach for when a laptop is full.
func checkLinks(to string) error {
	probe, err := os.CreateTemp(to, ".pkgcache-probe-*")
	if err != nil {
		return fmt.Errorf("local: %s cannot be written to: %w", to, err)
	}
	name := probe.Name()
	_ = probe.Close()
	defer func() { _ = os.Remove(name) }()

	link := name + ".link"
	if err := os.Link(name, link); err != nil {
		return fmt.Errorf(
			"local: %s cannot hardlink, so a cache cannot live on it: %w\n"+
				"  exFAT, FAT32 and most network shares behave this way. Move the cache to\n"+
				"  an ext4, xfs or btrfs filesystem instead", to, err)
	}
	_ = os.Remove(link)
	return nil
}

// renameMove is os.Rename, named so that the copy path can be exercised in a test —
// where both ends of a move are always the same filesystem and rename would otherwise
// always win.
var renameMove = os.Rename

// renameInto moves the tree with rename(2) where the two ends share a filesystem, and
// reports whether it did. Every failure is a fall-through to the copy: the reason this
// can fail — EXDEV, a destination that is a mount point, Windows — is never a reason
// the move as a whole cannot happen.
func renameInto(from, to string) (bool, error) {
	// Rename onto an existing directory is portable only when it is empty, and not even
	// then on Windows. prepareDestination has established there is nothing here worth
	// keeping, so the directory goes and the rename creates it again.
	if _, err := config.ClearMovedTo(to); err != nil {
		return false, err
	}
	if err := os.Remove(to); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err := renameMove(from, to); err != nil {
		return false, nil
	}
	return true, nil
}

// tally is what a cache holds, counted the way a copy of it can be checked.
type tally struct {
	files int
	bytes int64
	// links counts the names beyond the first that share a file with another name. It
	// is the number that says whether hardlinks survived the move.
	links int
}

// measure walks a cache, skipping what a move deliberately leaves behind, and counts
// bytes once per file rather than once per name — which is both what the destination
// needs to have free and what makes the count comparable across the move.
func measure(root string) (tally, error) {
	var (
		out  tally
		seen = map[string]bool{}
	)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A cache being read while nothing writes it should not lose files, but a
			// vanished one is not worth failing a move over either.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if skip := skipped(rel, entry); skip != nil {
			if errors.Is(skip, errSkipEntry) {
				return nil
			}
			return skip
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		out.files++
		if key, shared := fileIdentity(info); shared {
			if seen[key] {
				out.links++
				return nil
			}
			seen[key] = true
		}
		out.bytes += info.Size()
		return nil
	})
	if err != nil {
		return tally{}, fmt.Errorf("local: read %s: %w", root, err)
	}
	return out, nil
}

// errSkipEntry marks a single file a move leaves behind. fs.SkipDir would take a whole
// subtree with it, and the standard library has no sentinel for one entry.
var errSkipEntry = errors.New("local: skip this entry")

// skipped reports what a move leaves behind: fs.SkipDir for a subtree, errSkipEntry for
// one file, nil for "take it".
func skipped(rel string, entry fs.DirEntry) error {
	if rel == "." {
		return nil
	}
	if rel == stagingDir {
		// The directory itself is taken; its contents are half-downloaded files that
		// nothing references and that the store recreates the directory for anyway.
		return fs.SkipDir
	}
	if !entry.IsDir() && transient[rel] {
		return errSkipEntry
	}
	return nil
}

// copyInto establishes that the destination can hold a cache, copies it there, and
// establishes that the whole of it arrived — which is what earns the right to delete
// the only other copy.
func copyInto(ctx context.Context, from, to string, held tally, out io.Writer) error {
	if err := os.MkdirAll(to, 0o700); err != nil {
		return fmt.Errorf("local: create %s: %w", to, err)
	}
	if err := checkDestination(to, held.bytes); err != nil {
		return err
	}
	note(out, "pkgcache: copying %s to %s\n", FormatSize(held.bytes), to)
	if err := copyTree(ctx, from, to, held, out); err != nil {
		return err
	}
	// Counted again on the far side rather than trusted from the loop that wrote it.
	// The question this answers is not "did every copy call return nil" — it is "is the
	// whole cache there", and the second is the one worth asking here.
	arrived, err := measure(to)
	if err != nil {
		return err
	}
	if arrived.files != held.files || arrived.bytes != held.bytes {
		return fmt.Errorf(
			"local: %s holds %d files and %s, but %s holds %d files and %s.\n"+
				"  the old cache has been left alone; nothing was removed",
			from, held.files, FormatSize(held.bytes),
			to, arrived.files, FormatSize(arrived.bytes))
	}
	return nil
}

// copyTree reproduces the cache under to, preserving the hardlinks that make the blob
// store a deduplicated one.
func copyTree(ctx context.Context, from, to string, held tally, out io.Writer) error {
	var (
		copied   tally
		links    = map[string]string{}
		reported = time.Now()
	)
	return filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		if skip := skipped(rel, entry); skip != nil {
			if errors.Is(skip, errSkipEntry) {
				return nil
			}
			if errors.Is(skip, fs.SkipDir) {
				// The staging directory is taken and its contents are not, so it is
				// created here before the walk is told to skip inside it.
				if err := os.MkdirAll(filepath.Join(to, rel), 0o700); err != nil {
					return err
				}
			}
			return skip
		}
		target := filepath.Join(to, rel)
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}

		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case entry.Type()&fs.ModeSymlink != 0:
			dest, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(dest, target)
		case !entry.Type().IsRegular():
			// A socket or a device node in a cache directory is not the cache's, and
			// recreating one is not this command's business.
			return nil
		}

		key, shared := fileIdentity(info)
		if shared {
			if first, ok := links[key]; ok {
				return os.Link(first, target)
			}
			links[key] = target
		}
		if err := copyRegular(path, target, info); err != nil {
			return err
		}
		copied.files++
		copied.bytes += info.Size()
		if time.Since(reported) >= 2*time.Second {
			reported = time.Now()
			note(out, "pkgcache:   %s of %s\n",
				FormatSize(copied.bytes), FormatSize(held.bytes))
		}
		return nil
	})
}

// copyRegular writes one file and waits for it to land.
//
// Durable per file rather than once at the end, because the old copy is about to be
// deleted on the strength of this one and a power cut in between should cost a cache,
// not a corrupted one that starts and serves wrong answers.
func copyRegular(source, target string, info fs.FileInfo) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, in); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	// The modification time is content, not metadata, for a store whose reports are
	// "what has this cache held, and since when".
	return os.Chtimes(target, time.Now(), info.ModTime())
}
