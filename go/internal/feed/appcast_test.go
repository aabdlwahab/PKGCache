package feed

import (
	"crypto/ed25519"
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return public, private
}

func TestSignRoundTrips(t *testing.T) {
	public, private := testKey(t)
	payload := []byte("the bytes of a release artefact")

	signature := Sign(private, payload)
	if !Verify(public, payload, signature) {
		t.Fatal("a signature this package produced should verify against its own key")
	}
	if Verify(public, []byte("different bytes"), signature) {
		t.Error("a signature over one payload must not verify another")
	}
	if Verify(public, payload, "not base64 at all") {
		t.Error("unparseable signatures must be refused, not panic")
	}
}

func TestAppcastIsWellFormedAndCarriesTheNamespace(t *testing.T) {
	_, private := testKey(t)
	body, err := Appcast("pkgcache", []Release{{
		Version: "1.2.0",
		Date:    time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
		Artifacts: []Artifact{
			{OS: OSmacOS, URL: "https://cache/pkgcache-1.2.0.dmg", Size: 42,
				Signature: Sign(private, []byte("dmg")), MinimumSystemVersion: "11.0"},
			{OS: OSWindows, URL: "https://cache/pkgcache-1.2.0.exe", Size: 43,
				Signature: Sign(private, []byte("exe"))},
		},
	}})
	if err != nil {
		t.Fatalf("Appcast: %v", err)
	}

	// Parseable at all is the first bar, and the one a hand-written template fails.
	var parsed struct {
		XMLName xml.Name
		Channel struct {
			Items []struct {
				Version    string `xml:"version"`
				Enclosures []struct {
					URL       string `xml:"url,attr"`
					OS        string `xml:"os,attr"`
					Length    int64  `xml:"length,attr"`
					Signature string `xml:"edSignature,attr"`
				} `xml:"enclosure"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("the feed does not parse: %v\n%s", err, body)
	}
	if len(parsed.Channel.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(parsed.Channel.Items))
	}
	if got := parsed.Channel.Items[0].Version; got != "1.2.0" {
		t.Errorf("version = %q, want 1.2.0", got)
	}
	if got := len(parsed.Channel.Items[0].Enclosures); got != 2 {
		t.Fatalf("want both platforms in one item, got %d enclosures", got)
	}

	// The namespace declaration is what makes every sparkle: attribute meaningful. An
	// updater reading a feed without it sees an item with no version and no signature.
	if !strings.Contains(string(body), sparkleNS) {
		t.Errorf("the sparkle namespace is missing; the feed is unreadable without it\n%s", body)
	}
	for _, want := range []string{"sparkle:version", "sparkle:edSignature", "sparkle:os"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("%s is missing from the feed\n%s", want, body)
		}
	}
}

func TestAppcastRefusesUnsignedArtifacts(t *testing.T) {
	_, err := Appcast("pkgcache", []Release{{
		Version:   "1.0.0",
		Artifacts: []Artifact{{OS: OSmacOS, URL: "https://cache/x.dmg", Size: 1}},
	}})
	if err == nil {
		t.Fatal("an unsigned artefact must not reach a feed")
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("the refusal should say what is missing: %v", err)
	}
}

func TestAppcastRefusesLinux(t *testing.T) {
	// Linux updates through apt. Letting it into the appcast would mean an app quietly
	// replacing files dpkg believes it owns.
	_, private := testKey(t)
	_, err := Appcast("pkgcache", []Release{{
		Version: "1.0.0",
		Artifacts: []Artifact{{OS: "linux", URL: "https://cache/x.deb", Size: 1,
			Signature: Sign(private, []byte("deb"))}},
	}})
	if err == nil {
		t.Fatal("linux must not be offered through the appcast")
	}
}

func TestAppcastRefusesAVersionlessRelease(t *testing.T) {
	if _, err := Appcast("pkgcache", []Release{{}}); err == nil {
		t.Fatal("a release with no version has nothing for an updater to compare against")
	}
}

func TestEmptyAppcastIsValidNotAnError(t *testing.T) {
	// A fresh instance has published nothing. "You are up to date" is the right answer;
	// a 404 would report a problem the operator does not have.
	body, err := Appcast("pkgcache", nil)
	if err != nil {
		t.Fatalf("an empty feed is a normal state: %v", err)
	}
	if err := xml.Unmarshal(body, new(struct {
		XMLName xml.Name
	})); err != nil {
		t.Fatalf("an empty feed must still parse: %v\n%s", err, body)
	}
}

func TestAppcastEscapesReleaseNotes(t *testing.T) {
	// The reason this goes through encoding/xml rather than a template.
	_, private := testKey(t)
	body, err := Appcast("pkgcache", []Release{{
		Version: "1.0.0",
		Notes:   "fixed apt & npm <badly>",
		Artifacts: []Artifact{{OS: OSmacOS, URL: "https://cache/x.dmg", Size: 1,
			Signature: Sign(private, []byte("dmg"))}},
	}})
	if err != nil {
		t.Fatalf("Appcast: %v", err)
	}
	if err := xml.Unmarshal(body, new(struct {
		XMLName xml.Name
	})); err != nil {
		t.Fatalf("release notes broke the feed: %v\n%s", err, body)
	}
}

func TestSortReleasesIsNewestFirst(t *testing.T) {
	releases := []Release{
		{Version: "1.9.0"}, {Version: "1.10.0"}, {Version: "1.2.0"}, {Version: "2.0.0"},
	}
	SortReleases(releases)
	want := []string{"2.0.0", "1.10.0", "1.9.0", "1.2.0"}
	for index, expected := range want {
		if releases[index].Version != expected {
			t.Fatalf("order = %v, want %v",
				[]Release{releases[0], releases[1], releases[2], releases[3]}, want)
		}
	}
}

func TestSortReleasesHandlesUnnumberedVersions(t *testing.T) {
	// A build named for a branch should land somewhere predictable rather than wherever
	// the sort happened to leave it.
	releases := []Release{{Version: "main"}, {Version: "1.0.0"}, {Version: "v2.0.0"}}
	SortReleases(releases)
	if releases[0].Version != "v2.0.0" {
		t.Errorf("a numbered version should still win: got %q", releases[0].Version)
	}
}

func TestAppcastPlatformNamesTheTwoThatUpdateThisWay(t *testing.T) {
	for goos, want := range map[string]string{"darwin": OSmacOS, "windows": OSWindows} {
		got, ok := AppcastPlatform(goos)
		if !ok || got != want {
			t.Errorf("AppcastPlatform(%q) = %q, %v; want %q, true", goos, got, ok, want)
		}
	}
	if _, ok := AppcastPlatform("linux"); ok {
		t.Error("linux does not update through the appcast")
	}
}
