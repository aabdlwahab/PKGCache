//go:build unix

package diskusage

import (
	"fmt"
	"syscall"
)

func usage(path string) (free, total int64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	// Bsize is int64 on Linux and uint32 on the BSDs, so the conversion is explicit
	// rather than relying on whichever the build platform happens to use.
	//
	// unconvert is silenced rather than obeyed: it runs on Linux, where this is a no-op,
	// and taking it out breaks the darwin build outright — `int64(stat.Bavail) * size`
	// becomes "mismatched types int64 and uint32". The alternative is one file per
	// platform to hold ten identical lines.
	size := int64(stat.Bsize) //nolint:unconvert // required on darwin, no-op on linux
	return int64(stat.Bavail) * size, int64(stat.Blocks) * size, nil
}
