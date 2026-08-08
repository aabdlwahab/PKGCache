//go:build !unix

package diskusage

import "errors"

// ErrUnsupported is returned where there is no statfs. The server targets Unix; this
// exists so a cross-compile of a package that happens to link this one still builds,
// and fails loudly rather than reporting a plausible zero.
var ErrUnsupported = errors.New("diskusage: not supported on this platform")

func usage(string) (free, total int64, err error) { return 0, 0, ErrUnsupported }
