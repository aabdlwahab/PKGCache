//go:build race

package local

// startBudgetFactor scales how long a client waits for a daemon it started.
//
// Larger under the race detector, because this package's own tests re-exec the test
// binary as the daemon — `Executable: os.Args[0]` — so the process being waited for is
// fully race-instrumented, opening two SQLite databases with every memory access
// checked, on a CI runner already running the rest of the suite in parallel. Three tests
// failed on exactly that: "the cache did not start within 30s", at 30 to 32 seconds,
// with nothing wrong except the budget.
//
// This changes no shipped behaviour. A release binary is never race-instrumented, so the
// 30 seconds a user waits before being told the cache did not start is unchanged.
const startBudgetFactor = 4
