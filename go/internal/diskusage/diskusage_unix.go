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
	// Bsize is int64 on Linux and int32 on the BSDs, so the conversion is explicit
	// rather than relying on whichever the build platform happens to use.
	size := int64(stat.Bsize)
	return int64(stat.Bavail) * size, int64(stat.Blocks) * size, nil
}
