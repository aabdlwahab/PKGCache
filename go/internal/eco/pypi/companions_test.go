package pypi

import (
	"slices"
	"testing"
)

func TestADistributionTravelsWithItsSimplePageAndMetadata(t *testing.T) {
	for key, want := range map[string][]string{
		"pypi/+f/requests/requests-2.32.3-py3-none-any.whl": {
			"simple/pypi/requests", "pypi/+f/requests/requests-2.32.3-py3-none-any.whl.metadata",
		},
		"pypi/+f/requests/requests-2.32.3-py3-none-any.whl.metadata": {"simple/pypi/requests"},
		"simple/pypi/requests": nil,
	} {
		got, err := companions(key, nil)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("companions(%q) = %q, %v; want %q", key, got, err, want)
		}
	}
}
