package router

import (
	"net/url"
	"testing"
)

func TestPlainProxyFormRoundTrips(t *testing.T) {
	cases := map[string]string{
		"https://developer.download.nvidia.com/compute/cuda/repos/x/InRelease": "http://developer.download.nvidia.com:443/compute/cuda/repos/x/InRelease",
		"https://mirror.example/pool/a%2Bb.deb?x=1":                            "http://mirror.example:443/pool/a%2Bb.deb?x=1",
		// Nothing to convert: already plain, on another port, or carrying credentials.
		"http://archive.ubuntu.com/ubuntu/dists/noble/InRelease": "http://archive.ubuntu.com/ubuntu/dists/noble/InRelease",
		"https://mirror.example:8443/pool/x.deb":                 "https://mirror.example:8443/pool/x.deb",
		"https://user:secret@mirror.example/pool/x.deb":          "https://user:secret@mirror.example/pool/x.deb",
	}
	for in, want := range cases {
		got := PlainProxyForm(in)
		if got != want {
			t.Errorf("PlainProxyForm(%s) = %s, want %s", in, got, want)
			continue
		}
		parsed, err := url.Parse(got)
		if err != nil {
			t.Fatal(err)
		}
		if UpgradePlainProxyForm(parsed) != (got != in) {
			t.Errorf("UpgradePlainProxyForm(%s) disagreed about whether it was converted", got)
		}
		if parsed.String() != in {
			t.Errorf("round trip of %s came back as %s", in, parsed.String())
		}
	}
	v6, _ := url.Parse("http://[2001:db8::1]:443/pool/x.deb")
	if !UpgradePlainProxyForm(v6) || v6.String() != "https://[2001:db8::1]/pool/x.deb" {
		t.Errorf("IPv6 upgrade = %s", v6)
	}
}
