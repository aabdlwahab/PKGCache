//go:build !race

package local

// startBudgetFactor scales how long a client waits for a daemon it started.
//
// One, for every build that ships: an ordinary binary opens its two SQLite databases
// and sweeps interrupted downloads in seconds. See the race-tagged file beside this one
// for why the tests need a different number.
const startBudgetFactor = 1
