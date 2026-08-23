// Package snapshot implements the durable, streamed air-gap formats.
package snapshot

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
)

const manifestMagic = "#pkgreg-manifest v1"

// ManagedKeyPrefix reserves manifest keys for tarred descriptor-owned directory
// trees. Ordinary adapter keys sort before this prefix.
const ManagedKeyPrefix = "~managed/"

// ManagedKey identifies one managed directory tar entry.
func ManagedKey(key string) (string, bool) {
	if !strings.HasPrefix(key, ManagedKeyPrefix) {
		return "", false
	}
	relative := strings.TrimPrefix(key, ManagedKeyPrefix)
	return relative, relative != ""
}

// Entry is one immutable cache mapping in a snapshot.
type Entry struct {
	Eco    string
	Key    string
	Digest blob.Digest
	Size   int64
}

// Header identifies the manifest before its entry stream.
type Header struct {
	Project string
	Created time.Time
}

// EntrySource emits sorted entries without collecting them in memory.
type EntrySource func(yield func(Entry) error) error

// WriteManifest writes the stable v1 gzip format and returns its inventory totals.
func WriteManifest(dst io.Writer, header Header, source EntrySource) (count, bytes int64, err error) {
	if header.Project == "" || header.Created.IsZero() {
		return 0, 0, errors.New("snapshot: project and creation time are required")
	}
	gz, err := gzip.NewWriterLevel(dst, gzip.BestSpeed)
	if err != nil {
		return 0, 0, fmt.Errorf("snapshot: create gzip writer: %w", err)
	}
	gz.ModTime = time.Unix(0, 0).UTC()
	gz.OS = 255
	bw := bufio.NewWriterSize(gz, 64<<10)
	if _, err := fmt.Fprintf(bw, "%s project=%s created=%s\n", manifestMagic,
		url.QueryEscape(header.Project), header.Created.UTC().Format(time.RFC3339Nano)); err != nil {
		_ = gz.Close()
		return 0, 0, fmt.Errorf("snapshot: write header: %w", err)
	}
	var previous string
	err = source(func(entry Entry) error {
		if err := validateEntry(entry); err != nil {
			return err
		}
		order := entry.Eco + "\x00" + entry.Key
		if previous != "" && order <= previous {
			return fmt.Errorf("snapshot: entries are not strictly sorted at %s/%s",
				entry.Eco, entry.Key)
		}
		previous = order
		if _, err := fmt.Fprintf(bw, "%s\t%s\t%s\t%d\n",
			entry.Eco, entry.Key, entry.Digest, entry.Size); err != nil {
			return fmt.Errorf("snapshot: write entry: %w", err)
		}
		count++
		bytes += entry.Size
		return nil
	})
	if err == nil {
		err = bw.Flush()
	}
	closeErr := gz.Close()
	if err != nil {
		return 0, 0, err
	}
	if closeErr != nil {
		return 0, 0, fmt.Errorf("snapshot: close manifest: %w", closeErr)
	}
	return count, bytes, nil
}

// Iterator reads a manifest one row at a time.
type Iterator struct {
	gz       *gzip.Reader
	reader   *bufio.Reader
	Header   Header
	previous string
	done     bool
}

// NewIterator validates and consumes the manifest header.
func NewIterator(src io.Reader) (*Iterator, error) {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return nil, fmt.Errorf("snapshot: open manifest gzip: %w", err)
	}
	it := &Iterator{gz: gz, reader: bufio.NewReaderSize(gz, 64<<10)}
	line, err := readLine(it.reader)
	if err != nil {
		_ = gz.Close()
		return nil, fmt.Errorf("snapshot: read manifest header: %w", err)
	}
	header, err := parseHeader(line)
	if err != nil {
		_ = gz.Close()
		return nil, err
	}
	it.Header = header
	return it, nil
}

// Next returns the next entry. ok=false is clean end of stream.
func (it *Iterator) Next() (entry Entry, ok bool, err error) {
	if it.done {
		return Entry{}, false, nil
	}
	line, err := readLine(it.reader)
	if errors.Is(err, io.EOF) {
		it.done = true
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("snapshot: read entry: %w", err)
	}
	parts := strings.Split(line, "\t")
	if len(parts) != 4 {
		return Entry{}, false, errors.New("snapshot: malformed manifest entry")
	}
	size, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return Entry{}, false, fmt.Errorf("snapshot: invalid entry size: %w", err)
	}
	entry = Entry{Eco: parts[0], Key: parts[1], Digest: blob.Digest(parts[2]), Size: size}
	if err := validateEntry(entry); err != nil {
		return Entry{}, false, err
	}
	order := entry.Eco + "\x00" + entry.Key
	if it.previous != "" && order <= it.previous {
		return Entry{}, false, fmt.Errorf("snapshot: manifest is not strictly sorted at %s/%s",
			entry.Eco, entry.Key)
	}
	it.previous = order
	return entry, true, nil
}

// Walk drains the manifest through fn and returns its totals.
func (it *Iterator) Walk(fn func(Entry) error) (count, bytes int64, err error) {
	for {
		entry, ok, err := it.Next()
		if err != nil {
			return 0, 0, err
		}
		if !ok {
			return count, bytes, nil
		}
		if err := fn(entry); err != nil {
			return 0, 0, err
		}
		count++
		bytes += entry.Size
	}
}

// Close validates the gzip trailer when the iterator was fully consumed.
func (it *Iterator) Close() error {
	if it.gz == nil {
		return nil
	}
	err := it.gz.Close()
	it.gz = nil
	return err
}

func validateEntry(entry Entry) error {
	if entry.Eco == "" || entry.Key == "" || strings.ContainsAny(entry.Eco, "\t\r\n") ||
		strings.ContainsAny(entry.Key, "\t\r\n") || !entry.Digest.Valid() || entry.Size < 0 {
		return fmt.Errorf("snapshot: invalid entry %q/%q", entry.Eco, entry.Key)
	}
	return nil
}

func parseHeader(line string) (Header, error) {
	fields := strings.Fields(line)
	if len(fields) != 4 || fields[0]+" "+fields[1] != manifestMagic ||
		!strings.HasPrefix(fields[2], "project=") || !strings.HasPrefix(fields[3], "created=") {
		return Header{}, errors.New("snapshot: unsupported manifest header")
	}
	project, err := url.QueryUnescape(strings.TrimPrefix(fields[2], "project="))
	if err != nil || project == "" {
		return Header{}, errors.New("snapshot: invalid manifest project")
	}
	created, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(fields[3], "created="))
	if err != nil {
		return Header{}, fmt.Errorf("snapshot: invalid manifest creation time: %w", err)
	}
	return Header{Project: project, Created: created}, nil
}

func readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line != "" {
			return "", errors.New("snapshot: unterminated manifest line")
		}
		return "", err
	}
	return strings.TrimSuffix(line, "\n"), nil
}
