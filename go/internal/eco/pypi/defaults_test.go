package pypi

import (
	"strings"
	"testing"
)

// A missing or misspelled default index fails silently in the worst way available: the
// build succeeds, having pulled a multi-hundred-megabyte wheel from the internet past a
// cache that would have served it. Nothing errors, nothing logs, and the only symptom is
// a slow build somebody blames on their network.
//
// So the shape is asserted rather than trusted. The name carries the channel and the URL
// has to end in that same channel; a transposed digit in either is the whole bug.
func TestDefaultIndexNamesMatchTheirChannel(t *testing.T) {
	for name, origin := range defaultIndexes {
		if name == "root/pypi" {
			if origin != "https://pypi.org/simple" {
				t.Errorf("root/pypi points at %s", origin)
			}
			continue
		}
		vendor, channel, ok := strings.Cut(strings.TrimPrefix(name, "root/"), "-")
		if !ok {
			t.Errorf("%q is not <vendor>-<channel>", name)
			continue
		}
		if !strings.HasSuffix(origin, "/"+channel) {
			t.Errorf("%s points at %s, which does not end in /%s", name, origin, channel)
		}
		switch vendor {
		case "pytorch":
			if !strings.HasPrefix(origin, "https://download.pytorch.org/whl/") {
				t.Errorf("%s points at %s", name, origin)
			}
		case "flashinfer":
			if !strings.HasPrefix(origin, "https://flashinfer.ai/whl/") {
				t.Errorf("%s points at %s", name, origin)
			}
		default:
			t.Errorf("%s names a vendor this test does not know about", name)
		}
	}
}

// The channels a current CUDA build is likely to name. cu130 is the one that started
// this: a Hopper image pinned to it missed the cache entirely while a CPU image beside it
// hit, because the list had gone stale at cu128.
func TestCurrentCUDAChannelsAreServed(t *testing.T) {
	for _, want := range []string{
		"https://download.pytorch.org/whl/cpu",
		"https://download.pytorch.org/whl/cu126",
		"https://download.pytorch.org/whl/cu128",
		"https://download.pytorch.org/whl/cu129",
		"https://download.pytorch.org/whl/cu130",
		// Not on PyPI, and a vLLM stack pulls a 1.4 GiB wheel from it.
		"https://flashinfer.ai/whl/cu128",
		"https://flashinfer.ai/whl/cu130",
	} {
		found := false
		for _, origin := range defaultIndexes {
			if origin == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no default index serves %s", want)
		}
	}
}

// Two names for one origin would make the rewrite ambiguous — the client keys its map by
// URL, so the second would silently win.
func TestNoOriginIsServedTwice(t *testing.T) {
	seen := map[string]string{}
	for name, origin := range defaultIndexes {
		if first, clash := seen[origin]; clash {
			t.Errorf("%s and %s both serve %s", first, name, origin)
		}
		seen[origin] = name
	}
}
