package local

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseSizeReadsWhatPeopleWrite(t *testing.T) {
	cases := map[string]int64{
		"25G":       25 << 30,
		"25g":       25 << 30,
		"25GiB":     25 << 30,
		"25GB":      25 << 30,
		"500M":      500 << 20,
		"2T":        2 << 40,
		"512K":      512 << 10,
		"1024":      1024,
		"1.5G":      1536 << 20,
		"none":      NoLimit,
		"unlimited": NoLimit,
		" 25G ":     25 << 30,
	}
	for input, want := range cases {
		got, err := ParseSize(input)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", input, got, want)
		}
	}
	for _, input := range []string{"", "nonsense", "0", "-5G", "G"} {
		if _, err := ParseSize(input); err == nil {
			t.Errorf("ParseSize(%q) accepted a value that is not a size", input)
		}
	}
}

// The limit is mandatory, and "not chosen" has to be distinguishable from "chosen to
// be unlimited". Collapsing them would make an explicit `pkgcache limit none` look
// like a cache nobody had configured.
func TestBudgetDistinguishesUnsetFromUnlimited(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PKGCACHE_LIMIT", "")

	if _, err := ReadBudget(dir); !errors.Is(err, ErrNoLimit) {
		t.Fatalf("a fresh cache reported err = %v, want ErrNoLimit", err)
	}
	if err := WriteBudget(dir, Budget{LimitBytes: NoLimit}); err != nil {
		t.Fatal(err)
	}
	budget, err := ReadBudget(dir)
	if err != nil {
		t.Fatalf("an explicit `none` was read as unset: %v", err)
	}
	if budget.LimitBytes != NoLimit {
		t.Fatalf("limit = %d, want NoLimit", budget.LimitBytes)
	}
}

// PKGCACHE_LIMIT is what makes a mandatory limit one setup line in CI rather than an
// interactive prompt nobody can answer there.
func TestBudgetEnvironmentOverride(t *testing.T) {
	dir := t.TempDir()
	if err := WriteBudget(dir, Budget{LimitBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PKGCACHE_LIMIT", "25G")
	budget, err := ReadBudget(dir)
	if err != nil {
		t.Fatal(err)
	}
	if budget.LimitBytes != 25<<30 {
		t.Fatalf("limit = %d, want the environment's 25G", budget.LimitBytes)
	}

	t.Setenv("PKGCACHE_LIMIT", "nonsense")
	if _, err := ReadBudget(dir); err == nil {
		t.Fatal("an unparseable PKGCACHE_LIMIT was accepted")
	}
}

func TestBudgetFileIsPrivate(t *testing.T) {
	dir := t.TempDir()
	if err := WriteBudget(dir, Budget{LimitBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "budget.json"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("budget file mode = %o, want 600", mode)
	}
}

// The guard is the whole disk policy in one decision, so its boundaries are tested
// directly: under the limit stores, over it does not, and the free-space floor refuses
// even when the limit says there is room.
func TestGuardRefusesOverTheLimitAndUnderTheFloor(t *testing.T) {
	dir := t.TempDir()
	guard := NewGuard(nil, dir, Budget{LimitBytes: 1000, MinFreeBytes: 1}, nil)
	// Pretend the store already holds 900 bytes on a disk with plenty free.
	guard.mu.Lock()
	guard.bytes, guard.free, guard.sampled = 900, 1<<40, time.Now()
	guard.mu.Unlock()

	if ok, _ := guard.MayStore(50); !ok {
		t.Error("an artifact that fits was refused")
	}
	ok, reason := guard.MayStore(200)
	if ok {
		t.Fatal("an artifact that does not fit was accepted")
	}
	if reason == "" {
		t.Error("a refusal must say why; that message is what the user acts on")
	}
	if full, _ := guard.Full(); !full {
		t.Error("the guard did not record that the cache is full")
	}
	// And it recovers: a later artifact that fits clears the condition, so a cache
	// somebody pruned does not keep reporting itself full.
	if ok, _ := guard.MayStore(10); !ok {
		t.Fatal("a small artifact was refused after a large one did not fit")
	}
	if full, _ := guard.Full(); full {
		t.Error("the full condition survived an artifact that fit")
	}
}

func TestGuardKeepsDiskFreeEvenUnderTheLimit(t *testing.T) {
	dir := t.TempDir()
	guard := NewGuard(nil, dir, Budget{LimitBytes: 1 << 40, MinFreeBytes: 1000}, nil)
	guard.mu.Lock()
	guard.bytes, guard.free, guard.sampled = 0, 1200, time.Now()
	guard.mu.Unlock()

	if ok, _ := guard.MayStore(100); !ok {
		t.Error("an artifact leaving room above the floor was refused")
	}
	ok, reason := guard.MayStore(500)
	if ok {
		t.Fatal("an artifact that would breach the free-space floor was accepted")
	}
	if reason == "" {
		t.Fatal("the floor refusal said nothing")
	}
}

// A limit of "none" still keeps the disk floor: no cap is a decision about the cache,
// not permission to fill the machine.
func TestGuardWithNoLimitStillKeepsTheFloor(t *testing.T) {
	dir := t.TempDir()
	guard := NewGuard(nil, dir, Budget{LimitBytes: NoLimit, MinFreeBytes: 1000}, nil)
	guard.mu.Lock()
	guard.bytes, guard.free, guard.sampled = 1<<40, 1200, time.Now()
	guard.mu.Unlock()

	if ok, _ := guard.MayStore(10); !ok {
		t.Error("`none` should not be capped by a limit")
	}
	if ok, _ := guard.MayStore(900); ok {
		t.Error("`none` must still respect the free-space floor")
	}
}

// The desktop notification is throttled because a full cache stays full until somebody
// acts, and a toast every few seconds trains people to dismiss them unread.
func TestGuardNotifiesAtMostHourly(t *testing.T) {
	dir := t.TempDir()
	done := make(chan struct{}, 8)
	guard := NewGuard(nil, dir, Budget{LimitBytes: 10, MinFreeBytes: 1}, func(string) {
		done <- struct{}{}
	})
	guard.mu.Lock()
	guard.bytes, guard.free, guard.sampled = 100, 1<<40, time.Now()
	guard.mu.Unlock()

	for range 5 {
		guard.MayStore(50)
	}
	// Notification is raised on the first refusal, not on every one.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no notification was raised for a full cache")
	}
	// Any further notification would arrive on the channel; give it a moment to fail.
	select {
	case <-done:
		t.Fatal("a second notification was raised within the hour")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestUsageRoundTripAndClear(t *testing.T) {
	dir := t.TempDir()
	if _, _, found := ReadUsage(dir); found {
		t.Fatal("a fresh cache published usage")
	}
	PublishUsage(dir, Usage{
		Bytes: 1234, FreeBytes: 5678, Full: true, Reason: "the cache is full",
		Budget: Budget{LimitBytes: 1 << 30},
	})
	usage, sampled, found := ReadUsage(dir)
	if !found {
		t.Fatal("published usage could not be read back")
	}
	if !usage.Full || usage.Reason != "the cache is full" || usage.Bytes != 1234 {
		t.Fatalf("usage = %+v", usage)
	}
	if time.Since(sampled) > time.Minute {
		t.Errorf("sampled time = %v", sampled)
	}
	// prune and limit clear it, so a cache that has just been given room does not keep
	// reporting itself full until the next download happens to re-measure it.
	ClearUsage(dir)
	if _, _, found := ReadUsage(dir); found {
		t.Fatal("ClearUsage left the measurement behind")
	}
}

// Notify must never fail a command. There is no desktop in CI, over SSH or in a
// container, and the other three channels have already reported the same thing.
func TestNotifyIsSilentWithoutADesktop(t *testing.T) {
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	Notify("pkgcache", "the cache is full") // must not panic, block or exit
}
