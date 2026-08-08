// Package diskusage reports free and total space for the filesystem holding a path.
//
// It is its own package so the eviction policy and the console's disk gauge read the
// same number from the same place. A gauge computed differently from the policy that
// acts on it eventually disagrees with it, and the disagreement surfaces at exactly
// the wrong moment — when someone is watching a disk fill up.
package diskusage

// Usage reports the bytes available to this process and the filesystem's total size.
//
// Available, not free: the blocks a filesystem reserves for root are not space this
// process will ever be given, and counting them would let eviction believe it has
// headroom that does not exist.
func Usage(path string) (free, total int64, err error) {
	return usage(path)
}
