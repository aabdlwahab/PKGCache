// Package feed renders a published client release into the update feeds each platform's
// own updater expects.
//
// One source of truth, three renderings. `pkgreg publish-client` already knows what
// versions exist and what their checksums are; nothing here discovers anything, it only
// writes what that knowledge looks like to apt on Linux and to the Wails updater on macOS
// and Windows.
//
// The signatures are the point. A feed is a list of things to download and run, served
// over a network, so an unsigned one is a remote code execution waiting for a bad DNS
// answer. Every artefact here carries a signature the updater checks before it installs
// anything, and the keys that make them are the operator's to look after.
package feed

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Appcast operating systems, as the updater spells them.
const (
	OSmacOS   = "macos"
	OSWindows = "windows"
)

// Artifact is one downloadable file in one release.
type Artifact struct {
	// OS is OSmacOS or OSWindows. Linux is absent on purpose: it updates through apt,
	// because an app that replaces its own files behind dpkg's back is fighting the
	// package manager rather than using it.
	OS string
	// URL is where the updater fetches it. Absolute, because the feed may be read from
	// a cached copy whose own address says nothing about where the payload lives.
	URL string
	// Size in bytes, which the updater uses for its progress bar and as a first cheap
	// check that it fetched what the feed described.
	Size int64
	// Signature is the base64 Ed25519 signature over the file's bytes, from Sign.
	Signature string
	// MinimumSystemVersion is optional, and only macOS reads it.
	MinimumSystemVersion string
}

// Release is one version, and everything published under it.
type Release struct {
	// Version is what the updater compares against what is installed. Ordering is the
	// caller's: this writes the feed in the order it is given, newest first by convention.
	Version string
	// Date is the publication time. Zero means "now", resolved once so every item in one
	// feed agrees rather than drifting across a slow write.
	Date time.Time
	// Notes is optional HTML shown in the updater's window.
	Notes     string
	Artifacts []Artifact
}

// Sign returns the base64 Ed25519 signature the feed carries for one artefact.
//
// Over the file's bytes rather than over a hash of them: Ed25519 hashes internally, and a
// sign-the-hash construction invites somebody to later "optimise" it into signing a hash
// chosen by the attacker.
func Sign(key ed25519.PrivateKey, payload []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))
}

// Verify checks a signature produced by Sign. The updater does this itself; this is here
// so a test, and `pkgreg doctor`, can ask the same question.
func Verify(key ed25519.PublicKey, payload []byte, signature string) bool {
	raw, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	return ed25519.Verify(key, payload, raw)
}

// The appcast is RSS with one namespaced extension, which is what Sparkle defined and
// what every updater that speaks this format — including the one Wails ships — reads.
// Written through encoding/xml rather than a template because an unescaped release note
// containing an ampersand would otherwise produce a feed no parser accepts.

type rss struct {
	XMLName xml.Name `xml:"rss"`
	Version string   `xml:"version,attr"`
	Sparkle string   `xml:"xmlns:sparkle,attr"`
	Channel channel  `xml:"channel"`
}

type channel struct {
	Title       string `xml:"title"`
	Description string `xml:"description"`
	Items       []item `xml:"item"`
}

type item struct {
	Title       string      `xml:"title"`
	PubDate     string      `xml:"pubDate"`
	Version     string      `xml:"sparkle:version"`
	ShortVer    string      `xml:"sparkle:shortVersionString"`
	MinSystem   string      `xml:"sparkle:minimumSystemVersion,omitempty"`
	Description *cdata      `xml:"description,omitempty"`
	Enclosures  []enclosure `xml:"enclosure"`
}

type cdata struct {
	Value string `xml:",cdata"`
}

type enclosure struct {
	URL       string `xml:"url,attr"`
	OS        string `xml:"sparkle:os,attr"`
	Length    int64  `xml:"length,attr"`
	Type      string `xml:"type,attr"`
	Signature string `xml:"sparkle:edSignature,attr"`
}

// sparkleNS is the namespace every reader of this format keys off. It is a URL that has
// never served anything and is not meant to; changing it makes the feed unreadable.
const sparkleNS = "http://www.andymatuschak.org/xml-namespaces/sparkle"

// Appcast renders releases into the XML the updater fetches.
//
// Empty releases are not an error. A fresh instance has published nothing, and a feed
// with no items is the correct way to say "you are up to date" — an updater that got a
// 404 instead would report a problem the operator does not have.
func Appcast(title string, releases []Release) ([]byte, error) {
	document := rss{
		Version: "2.0",
		Sparkle: sparkleNS,
		Channel: channel{
			Title:       title,
			Description: "Updates for " + title,
		},
	}
	now := time.Now().UTC()

	for _, release := range releases {
		if release.Version == "" {
			return nil, fmt.Errorf("feed: a release with no version cannot be published")
		}
		when := release.Date
		if when.IsZero() {
			when = now
		}
		entry := item{
			Title:    release.Version,
			PubDate:  when.UTC().Format(time.RFC1123Z),
			Version:  release.Version,
			ShortVer: release.Version,
		}
		if release.Notes != "" {
			entry.Description = &cdata{Value: release.Notes}
		}
		for _, artifact := range release.Artifacts {
			switch artifact.OS {
			case OSmacOS, OSWindows:
			default:
				return nil, fmt.Errorf(
					"feed: %q is not an appcast platform; only %s and %s update this way",
					artifact.OS, OSmacOS, OSWindows)
			}
			if artifact.Signature == "" {
				return nil, fmt.Errorf(
					"feed: %s has no signature, and an unsigned artefact in an update feed is\n"+
						"  a remote code execution waiting for a bad DNS answer", artifact.URL)
			}
			// macOS is the only one that reads it, so carrying it on a Windows enclosure
			// would be noise in a file people read when something has gone wrong.
			if artifact.MinimumSystemVersion != "" && artifact.OS == OSmacOS {
				entry.MinSystem = artifact.MinimumSystemVersion
			}
			entry.Enclosures = append(entry.Enclosures, enclosure{
				URL:       artifact.URL,
				OS:        artifact.OS,
				Length:    artifact.Size,
				Type:      "application/octet-stream",
				Signature: artifact.Signature,
			})
		}
		document.Channel.Items = append(document.Channel.Items, entry)
	}

	body, err := xml.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("feed: render appcast: %w", err)
	}
	var out bytes.Buffer
	out.WriteString(xml.Header)
	out.Write(body)
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// AppcastPlatform maps a published binary's GOOS to how the appcast spells it, and
// reports whether that platform updates through the appcast at all.
func AppcastPlatform(goos string) (string, bool) {
	switch goos {
	case "darwin":
		return OSmacOS, true
	case "windows":
		return OSWindows, true
	}
	return "", false
}

// SortReleases orders releases newest first, which is the order an appcast is read in.
//
// Versions are compared as dotted numbers with a leading "v" tolerated, falling back to
// string order for anything that is not one — a release named for a branch still lands
// somewhere predictable instead of wherever the map iteration left it.
func SortReleases(releases []Release) {
	sort.SliceStable(releases, func(i, j int) bool {
		return compareVersions(releases[i].Version, releases[j].Version) > 0
	})
}

func compareVersions(a, b string) int {
	left, right := splitVersion(a), splitVersion(b)
	for index := 0; index < len(left) || index < len(right); index++ {
		var l, r int
		if index < len(left) {
			l = left[index]
		}
		if index < len(right) {
			r = right[index]
		}
		if l != r {
			if l > r {
				return 1
			}
			return -1
		}
	}
	return strings.Compare(a, b)
}

// splitVersion reads the leading dotted-number run of a version, which is all that can be
// compared arithmetically. "1.2.0-rc1" reads as {1,2,0}, and the suffix is left to the
// string comparison that breaks the tie.
func splitVersion(version string) []int {
	version = strings.TrimPrefix(version, "v")
	var out []int
	for _, field := range strings.Split(version, ".") {
		digits := 0
		for digits < len(field) && field[digits] >= '0' && field[digits] <= '9' {
			digits++
		}
		if digits == 0 {
			break
		}
		value := 0
		for _, character := range field[:digits] {
			value = value*10 + int(character-'0')
		}
		out = append(out, value)
		if digits != len(field) {
			break
		}
	}
	return out
}
