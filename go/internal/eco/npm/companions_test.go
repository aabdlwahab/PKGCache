package npm

import (
	"slices"
	"testing"
)

func TestATarballTravelsWithItsPackument(t *testing.T) {
	for key, want := range map[string][]string{
		"left-pad/-/left-pad-1.3.0.tgz": {"packument/left-pad"},
		"@types/node/-/node-20.1.0.tgz": {"packument/@types/node"},
		"packument/left-pad":            nil,
		"not-a-tarball":                 nil,
	} {
		got, err := companions(key, nil)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("companions(%q) = %q, %v; want %q", key, got, err, want)
		}
	}
}
