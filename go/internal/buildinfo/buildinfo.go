// Package buildinfo carries the identity of this binary, stamped at link time.
//
// An air-gapped operator often cannot ask "which commit is this?" any other way, so
// the values are printed by `pkgreg version`, logged once at startup, and exported as
// a metric label.
package buildinfo

import "runtime"

// Stamped via -ldflags. Defaults keep an un-stamped `go build` honest rather than
// claiming a version it does not have.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Info is the full build identity.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

// Get returns this binary's build identity.
func Get() Info {
	return Info{
		Version:   version,
		Commit:    commit,
		Date:      date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// String is a one-line summary for logs and `pkgreg version`.
func (i Info) String() string {
	return "pkgreg " + i.Version + " (" + i.Commit + ", " + i.Date + ", " + i.GoVersion + ", " + i.Platform + ")"
}
