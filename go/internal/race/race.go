// Package race reports whether this binary was built with the race detector.
//
// The Go runtime knows, but does not export it, so the standard workaround is a
// constant selected by build tag. Tests use it to skip performance assertions with a
// visible message rather than excluding the whole file from a -race build, where the
// spike would vanish without a trace.
//
// Why performance assertions cannot run under -race: ThreadSanitizer instruments
// every memory read and write to track happens-before relationships, costing 5-20x
// CPU. The cost is not uniform — the catalog's transpiled-from-C SQLite is
// memory-access-dense and lands at the top of that range (measured: 15,396
// inserts/s without, 1,223 with), while hand-written Go packages barely notice.
// Thresholds calibrated on an uninstrumented binary are therefore meaningless under
// the detector.
//
// Correctness tests must always run under -race. Only numbers belong behind this.
package race

// SkipReason explains a skip to whoever reads the test output.
const SkipReason = "performance thresholds are not meaningful under -race " +
	"(ThreadSanitizer costs 5-20x); run without -race to exercise this"
