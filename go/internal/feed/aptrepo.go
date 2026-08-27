package feed

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5" // #nosec G501 -- apt's Release format requires an MD5Sum section.
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// An apt repository, which is a directory of files with very particular names.
//
// Nothing here is clever, and it must not become clever: apt is thirty years old, its
// clients are conservative, and a repository that is subtly wrong fails on somebody's
// laptop with a message about a hash mismatch that tells them nothing. The layout:
//
//	dists/<suite>/Release              what the suite contains, and every index's hash
//	dists/<suite>/InRelease            the same bytes, clear-signed
//	dists/<suite>/<component>/binary-<arch>/Packages[.gz]
//	pool/<component>/<initial>/<source>/<file>.deb
//
// The Release file is the trust root of the whole thing. It names every index and its
// hash, each index names every package and its hash, and the signature over Release is
// therefore a signature over the entire repository. That chain is the reason none of the
// hashes below are optional.

// DebPackage is one .deb, as a repository needs to describe it.
type DebPackage struct {
	// Control is the package's own control stanza, field order preserved. It is copied
	// into the index rather than reconstructed, because apt reads fields this code has
	// no reason to know about.
	Control []ControlField
	// Filename is the path within the repository, which is what apt fetches.
	Filename string
	Size     int64
	MD5      string
	SHA256   string
	// source is where the file was read from, so the pool copy does not need a second
	// lookup. Unexported: it describes this run, not the package.
	source string
}

// ControlField is one line of a Debian control file, in the order it appeared.
type ControlField struct {
	Name  string
	Value string
}

// Get returns a field's value, or "" — field names are case-insensitive in the format,
// and a repository that missed "package:" because the file said "Package:" would produce
// an index that apt rejects for a reason nobody could see.
func (p DebPackage) Get(name string) string {
	for _, field := range p.Control {
		if strings.EqualFold(field.Name, name) {
			return field.Value
		}
	}
	return ""
}

// ReadDeb reads one .deb and returns what a repository index needs to say about it.
func ReadDeb(filePath string) (DebPackage, error) {
	// #nosec G304 -- the caller names a file it is publishing, not user input.
	body, err := os.ReadFile(filePath)
	if err != nil {
		return DebPackage{}, fmt.Errorf("feed: read %s: %w", filePath, err)
	}
	control, err := controlFromDeb(body)
	if err != nil {
		return DebPackage{}, fmt.Errorf("feed: %s: %w", path.Base(filePath), err)
	}
	sum256 := sha256.Sum256(body)
	sum128 := md5.Sum(body) // #nosec G401 -- format compatibility, not a security claim.
	return DebPackage{
		Control: control,
		Size:    int64(len(body)),
		SHA256:  hex.EncodeToString(sum256[:]),
		MD5:     hex.EncodeToString(sum128[:]),
	}, nil
}

// controlFromDeb digs the control stanza out of a .deb.
//
// A .deb is an ar archive of exactly three members in a fixed order: debian-binary,
// then control.tar.<compression>, then data.tar.<compression>. Only the middle one is
// wanted, and only one file inside it.
func controlFromDeb(body []byte) ([]ControlField, error) {
	members, err := readAr(body)
	if err != nil {
		return nil, err
	}
	for name, content := range members {
		if !strings.HasPrefix(name, "control.tar") {
			continue
		}
		// gzip is what this project's own build script writes. xz and zstd are legal in
		// the format and are named rather than silently mishandled, because a package
		// built by dpkg-deb on a modern Debian will use one of them.
		switch {
		case strings.HasSuffix(name, ".gz"):
			reader, gzErr := gzip.NewReader(bytes.NewReader(content))
			if gzErr != nil {
				return nil, fmt.Errorf("control.tar.gz is not gzip: %w", gzErr)
			}
			fields, tarErr := controlFromTar(reader)
			_ = reader.Close()
			return fields, tarErr
		case strings.HasSuffix(name, ".tar"):
			return controlFromTar(bytes.NewReader(content))
		default:
			return nil, fmt.Errorf(
				"%s uses a compression this does not read.\n"+
					"  Repack with gzip: this project's own build.sh writes control.tar.gz", name)
		}
	}
	return nil, fmt.Errorf("no control.tar member; is this a .deb?")
}

// controlFromTar finds ./control inside an extracted control archive.
//
// Both spellings are accepted because both are written: tar recorded "./control" when the
// archive was built from a directory, and "control" when it was built from a file list.
// dpkg accepts either, so a repository that accepted only one would reject packages that
// install perfectly well by hand.
func controlFromTar(source io.Reader) ([]ControlField, error) {
	archive := tar.NewReader(source)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read control archive: %w", err)
		}
		if path.Clean(header.Name) != "control" {
			continue
		}
		// Bounded: a control file is a few hundred bytes, and an archive claiming
		// otherwise is not one this should try to read into memory.
		body, err := io.ReadAll(io.LimitReader(archive, 1<<20))
		if err != nil {
			return nil, fmt.Errorf("read control: %w", err)
		}
		fields := ParseControl(string(body))
		if len(fields) == 0 {
			return nil, fmt.Errorf("the control file is empty")
		}
		return fields, nil
	}
	return nil, fmt.Errorf("the control archive has no control file")
}

// ParseControl reads a Debian control stanza, preserving field order.
//
// Continuation lines — those beginning with a space or tab — belong to the field above
// them, which is how a long Description is written. Folding them into the previous value
// rather than dropping them is the difference between an index apt accepts and one whose
// second field is a fragment of English.
func ParseControl(text string) []ControlField {
	var fields []ControlField
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if trimmed == "" {
			// A blank line ends the stanza. A control file holds exactly one.
			break
		}
		if trimmed[0] == ' ' || trimmed[0] == '\t' {
			if len(fields) > 0 {
				fields[len(fields)-1].Value += "\n" + trimmed
			}
			continue
		}
		colon := strings.Index(trimmed, ":")
		if colon < 0 {
			continue
		}
		fields = append(fields, ControlField{
			Name:  strings.TrimSpace(trimmed[:colon]),
			Value: strings.TrimSpace(trimmed[colon+1:]),
		})
	}
	return fields
}

// PoolPath is where a .deb lives inside the repository.
//
// The single-letter fan-out is Debian's, and it is not decoration: a flat pool with
// thousands of files is slow to list on some filesystems and unreadable to a person. The
// "lib" special case is theirs too — libfoo would otherwise pile every library into one
// directory named l.
func PoolPath(component, source, filename string) string {
	var initial string
	switch {
	case strings.HasPrefix(source, "lib") && len(source) > 3:
		initial = source[:4]
	case source != "":
		initial = source[:1]
	default:
		initial = "_"
	}
	return path.Join("pool", component, initial, source, filename)
}

// PackagesIndex renders the Packages file for one architecture.
//
// The four fields appended to each stanza are the repository's own: where the file is,
// how big it is, and two hashes. Everything before them is the package's control stanza
// copied through untouched.
func PackagesIndex(packages []DebPackage) []byte {
	var out bytes.Buffer
	for _, pkg := range packages {
		for _, field := range pkg.Control {
			// Filename and the hashes are the repository's to state. A control file that
			// carried its own would be describing a location it cannot know.
			switch strings.ToLower(field.Name) {
			case "filename", "size", "md5sum", "sha256":
				continue
			}
			fmt.Fprintf(&out, "%s: %s\n", field.Name, field.Value)
		}
		fmt.Fprintf(&out, "Filename: %s\n", pkg.Filename)
		fmt.Fprintf(&out, "Size: %d\n", pkg.Size)
		fmt.Fprintf(&out, "MD5sum: %s\n", pkg.MD5)
		fmt.Fprintf(&out, "SHA256: %s\n", pkg.SHA256)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// Gzip compresses an index reproducibly.
//
// No name and no modification time in the header: with them, two runs over identical
// input produce different bytes, every rebuild looks like a change, and the byte-identical
// rebuild property this project's .deb already has would stop at the repository.
func Gzip(payload []byte) ([]byte, error) {
	var out bytes.Buffer
	writer, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(payload); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// IndexFile is one file a Release stanza vouches for.
type IndexFile struct {
	// Path is relative to the suite directory, e.g. "main/binary-amd64/Packages".
	Path string
	Body []byte
}

// ReleaseOptions describes the suite being published.
type ReleaseOptions struct {
	Origin        string
	Label         string
	Suite         string
	Codename      string
	Components    []string
	Architectures []string
	Description   string
	// Date is the publication time; zero means now. apt refuses a Release whose date is
	// far in the future, and warns on one that is very old.
	Date time.Time
	// ValidUntil, when set, is how long apt should accept this Release before treating
	// the repository as stale. Off by default: an air-gapped machine that has not been
	// able to reach its server for a month still needs apt to work.
	ValidUntil time.Duration
}

// ReleaseFile renders the Release file: what this suite is, and the hash of every index
// in it.
//
// Both hash sections are written. SHA256 is what any current apt uses; MD5Sum is still
// expected by enough tooling that omitting it produces confusing failures, and it costs
// four lines.
func ReleaseFile(options ReleaseOptions, files []IndexFile) []byte {
	when := options.Date
	if when.IsZero() {
		when = time.Now()
	}
	when = when.UTC()

	var out bytes.Buffer
	write := func(name, value string) {
		if value != "" {
			fmt.Fprintf(&out, "%s: %s\n", name, value)
		}
	}
	write("Origin", options.Origin)
	write("Label", options.Label)
	write("Suite", options.Suite)
	write("Codename", options.Codename)
	write("Architectures", strings.Join(options.Architectures, " "))
	write("Components", strings.Join(options.Components, " "))
	write("Description", options.Description)
	// RFC 1123 with a numeric zone, in UTC, which is the only form every apt accepts.
	write("Date", when.Format("Mon, 02 Jan 2006 15:04:05 -0700"))
	if options.ValidUntil > 0 {
		write("Valid-Until", when.Add(options.ValidUntil).
			Format("Mon, 02 Jan 2006 15:04:05 -0700"))
	}

	sorted := append([]IndexFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	out.WriteString("MD5Sum:\n")
	for _, file := range sorted {
		sum := md5.Sum(file.Body) // #nosec G401 -- format compatibility, not security.
		fmt.Fprintf(&out, " %s %s %s\n",
			hex.EncodeToString(sum[:]), pad(len(file.Body)), file.Path)
	}
	out.WriteString("SHA256:\n")
	for _, file := range sorted {
		sum := sha256.Sum256(file.Body)
		fmt.Fprintf(&out, " %s %s %s\n",
			hex.EncodeToString(sum[:]), pad(len(file.Body)), file.Path)
	}
	return out.Bytes()
}

// pad right-aligns a size the way apt's own tooling does. Cosmetic, and matched anyway so
// that a person diffing this against a Debian mirror sees only what actually differs.
func pad(size int) string {
	text := strconv.Itoa(size)
	if len(text) >= 16 {
		return text
	}
	return strings.Repeat(" ", 16-len(text)) + text
}

// readAr parses an ar archive into its members.
//
// Written out rather than taken from a library because the format is sixty bytes of ASCII
// header per member and nothing else, and this project already writes one by hand in
// packaging/deb/build.sh.
func readAr(body []byte) (map[string][]byte, error) {
	const magic = "!<arch>\n"
	if len(body) < len(magic) || string(body[:len(magic)]) != magic {
		return nil, fmt.Errorf("not an ar archive")
	}
	members := make(map[string][]byte)
	offset := len(magic)
	for offset+60 <= len(body) {
		header := body[offset : offset+60]
		if string(header[58:60]) != "`\n" {
			return nil, fmt.Errorf("corrupt ar header at byte %d", offset)
		}
		name := strings.TrimRight(strings.TrimSpace(string(header[0:16])), "/")
		size, err := strconv.ParseInt(strings.TrimSpace(string(header[48:58])), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("unreadable member size at byte %d: %w", offset, err)
		}
		start := offset + 60
		if size < 0 || start+int(size) > len(body) {
			return nil, fmt.Errorf("member %q claims %d bytes, past the end of the archive",
				name, size)
		}
		members[name] = body[start : start+int(size)]
		// Members are padded to an even boundary; the padding byte is not part of anything.
		offset = start + int(size)
		if size%2 == 1 {
			offset++
		}
	}
	return members, nil
}

// sha256Hex is the hex digest of a payload, which is what Release carries.
func sha256Hex(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
