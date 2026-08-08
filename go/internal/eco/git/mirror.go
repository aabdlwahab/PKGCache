// Package git implements a read-only smart-HTTP Git mirror.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const stderrTailBytes = 4096

var (
	// ErrNotCached is the expected air-gap miss for a repository that has never
	// been mirrored.
	ErrNotCached = errors.New("git: repository not cached")
)

// MirrorError is a failed Git subprocess with a bounded stderr tail. Keeping the
// tail makes an upstream refusal actionable without retaining unbounded progress
// output from a large fetch.
type MirrorError struct {
	Operation string
	Code      int
	Tail      string
	Err       error
}

func (e *MirrorError) Error() string {
	code := ""
	if e.Code >= 0 {
		code = " (exit " + strconv.Itoa(e.Code) + ")"
	}
	tail := strings.TrimSpace(e.Tail)
	if tail != "" {
		return fmt.Sprintf("git %s%s: %s", e.Operation, code, tail)
	}
	return fmt.Sprintf("git %s%s: %v", e.Operation, code, e.Err)
}

func (e *MirrorError) Unwrap() error { return e.Err }

// MirrorOutcome says why Ensure did or did not contact an origin.
type MirrorOutcome string

// The reasons Ensure can return: the mirror was fresh enough to reuse, it had to be
// cloned, it was refreshed from the origin, or the instance is offline and served
// whatever it already had.
const (
	MirrorHit     MirrorOutcome = "hit"
	MirrorClone   MirrorOutcome = "clone"
	MirrorFetch   MirrorOutcome = "fetch"
	MirrorOffline MirrorOutcome = "offline"
)

type mirrorManager struct {
	gitPath string
	refsTTL time.Duration
	sem     chan struct{}

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	fresh map[string]time.Time

	uploadActive atomic.Int64
	uploadPeak   atomic.Int64
}

func newMirrorManager(gitPath string, refsTTL time.Duration, maxUploadPacks int) *mirrorManager {
	if gitPath == "" {
		gitPath = "git"
	}
	if maxUploadPacks <= 0 {
		maxUploadPacks = 8
	}
	return &mirrorManager{
		gitPath: gitPath,
		refsTTL: refsTTL,
		sem:     make(chan struct{}, maxUploadPacks),
		locks:   make(map[string]*sync.Mutex),
		fresh:   make(map[string]time.Time),
	}
}

func (m *mirrorManager) repoLock(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lock := m.locks[key]; lock != nil {
		return lock
	}
	lock := &sync.Mutex{}
	m.locks[key] = lock
	return lock
}

func (m *mirrorManager) isFresh(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.fresh[key]
	return ok && m.refsTTL > 0 && time.Since(at) < m.refsTTL
}

func (m *mirrorManager) markFresh(key string) {
	m.mu.Lock()
	m.fresh[key] = time.Now()
	m.mu.Unlock()
}

// ensure creates or refreshes a mirror. Writers for one repository serialize on
// its lock; a waiter re-checks freshness after acquiring the lock, so a burst of
// callers produces one fetch.
func (m *mirrorManager) ensure(
	ctx context.Context,
	key, mirrorDir, upstreamURL string,
	offline bool,
	syncCatalog func() error,
) (MirrorOutcome, error) {
	lock := m.repoLock(key)
	lock.Lock()
	defer lock.Unlock()

	exists := mirrorExists(mirrorDir)
	if offline {
		if !exists {
			return "", fmt.Errorf("%w: %s", ErrNotCached, key)
		}
		if syncCatalog != nil {
			if err := syncCatalog(); err != nil {
				return "", err
			}
		}
		return MirrorOffline, nil
	}
	if exists && m.isFresh(key) {
		return MirrorHit, nil
	}

	outcome := MirrorFetch
	var err error
	if exists {
		err = m.fetch(ctx, mirrorDir)
	} else {
		outcome = MirrorClone
		err = m.clone(ctx, mirrorDir, upstreamURL)
	}
	if err != nil {
		return "", err
	}
	if syncCatalog != nil {
		if err := syncCatalog(); err != nil {
			return "", err
		}
	}
	m.markFresh(key)
	return outcome, nil
}

func (m *mirrorManager) clone(ctx context.Context, mirrorDir, upstreamURL string) error {
	parent := filepath.Dir(mirrorDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("git: create mirror parent: %w", err)
	}
	if _, err := os.Stat(mirrorDir); err == nil {
		return fmt.Errorf("git: refusing to replace incomplete mirror %s", mirrorDir)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("git: inspect mirror: %w", err)
	}

	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(mirrorDir)+".clone-")
	if err != nil {
		return fmt.Errorf("git: create mirror staging directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(tmp)
		}
	}()

	if _, err := m.run(ctx, "init", "", "init", "--bare", "-q", tmp); err != nil {
		return err
	}
	config := [][2]string{
		{"remote.origin.url", upstreamURL},
		{"gc.auto", "0"},
		{"maintenance.auto", "false"},
		{"uploadpack.allowFilter", "true"},
		{"uploadpack.allowAnySHA1InWant", "true"},
	}
	for _, item := range config {
		if _, err := m.run(ctx, "config "+item[0], "",
			"--git-dir", tmp, "config", item[0], item[1]); err != nil {
			return err
		}
	}
	if _, err := m.run(ctx, "configure heads refspec", "",
		"--git-dir", tmp, "config", "remote.origin.fetch",
		"+refs/heads/*:refs/heads/*"); err != nil {
		return err
	}
	if _, err := m.run(ctx, "configure tags refspec", "",
		"--git-dir", tmp, "config", "--add", "remote.origin.fetch",
		"+refs/tags/*:refs/tags/*"); err != nil {
		return err
	}
	if err := m.fetch(ctx, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, mirrorDir); err != nil {
		return fmt.Errorf("git: publish mirror: %w", err)
	}
	committed = true
	return nil
}

func (m *mirrorManager) fetch(ctx context.Context, mirrorDir string) error {
	if _, err := m.run(ctx, "fetch", "",
		"-c", "credential.helper=",
		"--git-dir", mirrorDir,
		"fetch", "--progress", "--prune", "--no-write-fetch-head",
		"--no-auto-maintenance", "--atomic", "origin"); err != nil {
		return err
	}
	// Fetch does not update the bare repository's HEAD. A stale HEAD still serves,
	// so synchronization is deliberately best-effort.
	out, _ := m.run(ctx, "resolve upstream HEAD", "",
		"--git-dir", mirrorDir, "ls-remote", "--symref", "origin", "HEAD")
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "ref:" && fields[2] == "HEAD" {
			_, _ = m.run(ctx, "synchronize HEAD", "",
				"--git-dir", mirrorDir, "symbolic-ref", "HEAD", fields[1])
			break
		}
	}
	return nil
}

type gitRef struct {
	Name   string
	Object string
}

func (m *mirrorManager) inspect(ctx context.Context, mirrorDir string) ([]gitRef, string, error) {
	out, err := m.run(ctx, "list refs", "",
		"--git-dir", mirrorDir, "for-each-ref",
		"--format=%(refname) %(objectname)", "refs/heads", "refs/tags")
	if err != nil {
		return nil, "", err
	}
	refs := make([]gitRef, 0)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			refs = append(refs, gitRef{Name: fields[0], Object: fields[1]})
		}
	}
	head, _ := m.run(ctx, "read HEAD", "",
		"--git-dir", mirrorDir, "symbolic-ref", "-q", "HEAD")
	return refs, strings.TrimSpace(head), nil
}

func (m *mirrorManager) maintain(
	ctx context.Context, key, mirrorDir string, syncCatalog func() error,
) error {
	lock := m.repoLock(key)
	lock.Lock()
	defer lock.Unlock()
	if !mirrorExists(mirrorDir) {
		return fmt.Errorf("%w: %s", ErrNotCached, key)
	}
	if _, err := m.run(ctx, "geometric repack", "",
		"--git-dir", mirrorDir, "repack", "-d", "--geometric=2", "--write-midx"); err != nil {
		return err
	}
	if _, err := m.run(ctx, "pack refs", "",
		"--git-dir", mirrorDir, "pack-refs", "--all"); err != nil {
		return err
	}
	if syncCatalog != nil {
		return syncCatalog()
	}
	return nil
}

func (m *mirrorManager) advertise(
	ctx context.Context, mirrorDir, protocol string, dst io.Writer,
) error {
	return m.pump(ctx, mirrorDir, protocol, nil, true, dst)
}

func (m *mirrorManager) uploadPack(
	ctx context.Context, mirrorDir, protocol string, body []byte, dst io.Writer,
) error {
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	return m.pump(ctx, mirrorDir, protocol, body, false, dst)
}

func (m *mirrorManager) pump(
	parent context.Context,
	mirrorDir, protocol string,
	body []byte,
	advertise bool,
	dst io.Writer,
) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	args := []string{
		"-c", "uploadpack.allowFilter=true",
		"-c", "uploadpack.allowAnySHA1InWant=true",
		"upload-pack", "--stateless-rpc",
	}
	if advertise {
		args = append(args, "--advertise-refs")
	}
	args = append(args, mirrorDir)

	cmd := exec.CommandContext(ctx, m.gitPath, args...)
	cmd.Env = gitEnv(protocol)
	if body != nil {
		cmd.Stdin = bytes.NewReader(body)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return &MirrorError{Operation: "upload-pack", Code: -1, Err: err}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return &MirrorError{Operation: "upload-pack", Code: -1, Err: err}
	}
	if err := cmd.Start(); err != nil {
		return &MirrorError{Operation: "upload-pack", Code: -1, Err: err}
	}

	if !advertise {
		active := m.uploadActive.Add(1)
		for {
			peak := m.uploadPeak.Load()
			if active <= peak || m.uploadPeak.CompareAndSwap(peak, active) {
				break
			}
		}
		defer m.uploadActive.Add(-1)
	}

	tail := &tailWriter{max: stderrTailBytes}
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(tail, stderr)
		close(drained)
	}()

	buf := make([]byte, 64<<10)
	_, copyErr := io.CopyBuffer(dst, stdout, buf)
	if copyErr != nil {
		cancel()
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	<-drained

	if copyErr != nil {
		if parent.Err() != nil {
			return parent.Err()
		}
		return fmt.Errorf("git: stream upload-pack: %w", copyErr)
	}
	if waitErr != nil {
		if parent.Err() != nil {
			return parent.Err()
		}
		return processError("upload-pack", waitErr, tail.String())
	}
	return nil
}

func (m *mirrorManager) run(
	ctx context.Context, operation, protocol string, args ...string,
) (string, error) {
	cmd := exec.CommandContext(ctx, m.gitPath, args...)
	cmd.Env = gitEnv(protocol)
	var stdout bytes.Buffer
	tail := &tailWriter{max: stderrTailBytes}
	cmd.Stdout = &stdout
	cmd.Stderr = tail
	if err := cmd.Run(); err != nil {
		return "", processError(operation, err, tail.String())
	}
	return stdout.String(), nil
}

func processError(operation string, err error, tail string) error {
	code := -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	}
	return &MirrorError{Operation: operation, Code: code, Tail: tail, Err: err}
}

func mirrorExists(path string) bool {
	info, err := os.Stat(filepath.Join(path, "HEAD"))
	return err == nil && !info.IsDir()
}

func dirSize(path string) int64 {
	var total int64
	// Best-effort by design: this figure is reported in the console, so a file that
	// vanished mid-walk should shrink the number, not fail the request.
	_ = filepath.WalkDir(path, func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil //nolint:nilerr // a size report must not fail on a racing delete
		}
		if info, infoErr := entry.Info(); infoErr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func gitEnv(protocol string) []string {
	overrides := map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "/bin/true",
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"GIT_CONFIG_NOSYSTEM": "1",
		"LC_ALL":              "C",
	}
	if protocol != "" {
		overrides["GIT_PROTOCOL"] = protocol
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if _, replaced := overrides[key]; !replaced && key != "GIT_PROTOCOL" {
			env = append(env, item)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

type tailWriter struct {
	max int
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.max <= 0 {
		return n, nil
	}
	if len(p) >= w.max {
		w.buf = append(w.buf[:0], p[len(p)-w.max:]...)
		return n, nil
	}
	excess := len(w.buf) + len(p) - w.max
	if excess > 0 {
		copy(w.buf, w.buf[excess:])
		w.buf = w.buf[:len(w.buf)-excess]
	}
	w.buf = append(w.buf, p...)
	return n, nil
}

func (w *tailWriter) String() string { return string(w.buf) }
