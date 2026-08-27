// Package local runs pkgcache's daemon and the client side that finds, starts and
// stops it.
//
// The shape is gpg-agent's, and it is chosen rather than inherited. A laptop should
// not run an installed service it is not using, and a command should not have to be
// preceded by "start the cache first" — so the first command that needs a cache starts
// one, it exits once nothing has used it for a while, and the next command starts it
// again.
//
// Three pieces are what make that safe rather than merely convenient:
//
//   - A file lock, which is what actually guarantees one daemon per cache directory.
//     SQLite and the blob store are single-writer; two daemons on one directory is
//     corruption, not contention.
//   - A state file, which is discovery and nothing more. A live pid does not prove the
//     process is ours, and a port that answers does not prove it is answering for us,
//     so both are checked and neither is trusted alone.
//   - A separate lock held by *clients* while they start a daemon, so twenty
//     concurrent `pkgcache run` invocations spawn one process rather than twenty that
//     then discover they cannot have the first lock.
package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// State is what a running daemon publishes about itself.
//
// Written after the listener is bound, not before: a state file naming a port nothing
// is listening on sends every client to a connection refused, which is a worse failure
// than no file at all.
type State struct {
	PID     int       `json:"pid"`
	Addr    string    `json:"addr"`
	Version string    `json:"version"`
	Started time.Time `json:"started"`
}

// BaseURL is the origin clients are pointed at.
func (s State) BaseURL() string { return "http://" + s.Addr }

// Uptime is how long this daemon has been running.
func (s State) Uptime() time.Duration { return time.Since(s.Started) }

// StatePath is where a running daemon publishes what it is doing. It and the three
// paths below it keep the on-disk layout described in one place.
func StatePath(dataDir string) string { return filepath.Join(dataDir, "daemon.json") }

// LockPath is held by the daemon for its whole life: one daemon per cache directory.
func LockPath(dataDir string) string { return filepath.Join(dataDir, "daemon.lock") }

// StartLockPath is held by a client only while it starts a daemon.
func StartLockPath(dataDir string) string { return filepath.Join(dataDir, "start.lock") }

// LogPath is where a daemon started in the background writes. A daemon nobody can see
// starting is a daemon nobody can debug when it fails to.
func LogPath(dataDir string) string { return filepath.Join(dataDir, "daemon.log") }

// ErrNoDaemon reports that no daemon is running for this cache directory.
var ErrNoDaemon = errors.New("local: no daemon is running")

// ReadState returns what the daemon published, or ErrNoDaemon.
//
// A malformed file is treated as absent rather than as an error. It means a previous
// process died between creating and writing the file, and the useful response to that
// is to start a daemon, not to make the user delete something.
func ReadState(dataDir string) (State, error) {
	data, err := os.ReadFile(StatePath(dataDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return State{}, ErrNoDaemon
		}
		return State{}, fmt.Errorf("local: read daemon state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil || s.Addr == "" {
		return State{}, ErrNoDaemon
	}
	return s, nil
}

// WriteState publishes this daemon's state atomically, so no reader ever sees half a
// document.
func WriteState(dataDir string, s State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(dataDir, ".daemon-*.json")
	if err != nil {
		return fmt.Errorf("local: write daemon state: %w", err)
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("local: write daemon state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("local: write daemon state: %w", err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("local: write daemon state: %w", err)
	}
	if err := os.Rename(name, StatePath(dataDir)); err != nil {
		return fmt.Errorf("local: publish daemon state: %w", err)
	}
	return nil
}

// RemoveState clears the published state. Errors are deliberately ignored: this runs
// on the shutdown path, where a failure to tidy up must not mask why we are shutting
// down, and a stale file is something every reader already has to survive.
func RemoveState(dataDir string) { _ = os.Remove(StatePath(dataDir)) }

// probeClient is separate from anything that carries package traffic: these requests
// must fail fast, and a caller waiting on a health check should never inherit the
// twenty-minute timeout a 2.5 GB wheel needs.
var probeClient = &http.Client{Timeout: 2 * time.Second}

// Alive reports whether this state describes a daemon that is answering right now.
//
// Both halves are necessary and neither is sufficient. The pid may have been reused by
// an unrelated program; the port may be answered by an unrelated program. Together
// they are enough for a laptop, where the alternative — a credential on a loopback
// socket — buys nothing against an attacker who can already run code as this user.
func (s State) Alive(ctx context.Context) bool {
	if s.Addr == "" {
		return false
	}
	if s.PID > 0 && !processAlive(s.PID) {
		return false
	}
	return Probe(ctx, s.Addr) == nil
}

// Probe checks that a pkgcache is answering on an address, and that what answers is
// one of ours rather than whatever else happened to take the port.
func Probe(ctx context.Context, addr string) error {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, "http://"+addr+"/healthz", http.NoBody)
	if err != nil {
		return err
	}
	response, err := probeClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("local: %s answered %s", addr, response.Status)
	}
	var health struct {
		Status string `json:"status"`
		Server string `json:"server"`
	}
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		return fmt.Errorf("local: %s is not a package cache", addr)
	}
	if health.Status != "ok" || health.Server != "unified" {
		return fmt.Errorf("local: %s is not a package cache", addr)
	}
	return nil
}
