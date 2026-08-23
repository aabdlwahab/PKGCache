package snapshot

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/blob"
)

func TestManifestRoundTrip100KStreamingEntries(t *testing.T) {
	const total = 100_000
	var encoded bytes.Buffer
	created := time.Date(2026, 7, 27, 12, 0, 0, 123, time.UTC)
	count, size, err := WriteManifest(&encoded, Header{
		Project: "global", Created: created,
	}, func(yield func(Entry) error) error {
		for index := range total {
			entry := Entry{
				Eco:    "pypi",
				Key:    fmt.Sprintf("root/pypi/+f/pkg/%06d.whl", index),
				Digest: blob.Digest(fmt.Sprintf("%064x", index+1)),
				Size:   int64(index + 1),
			}
			if err := yield(entry); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != total || size != int64(total)*(total+1)/2 {
		t.Fatalf("write totals = %d/%d", count, size)
	}
	iterator, err := NewIterator(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer iterator.Close()
	if iterator.Header.Project != "global" || !iterator.Header.Created.Equal(created) {
		t.Fatalf("header = %+v", iterator.Header)
	}
	var seen int
	readCount, readSize, err := iterator.Walk(func(entry Entry) error {
		want := fmt.Sprintf("root/pypi/+f/pkg/%06d.whl", seen)
		if entry.Key != want {
			t.Fatalf("entry %d key = %q, want %q", seen, entry.Key, want)
		}
		seen++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if readCount != count || readSize != size || seen != total {
		t.Fatalf("read totals = %d/%d seen=%d", readCount, readSize, seen)
	}
}

func TestManifestRejectsUnsortedAndMalformedInput(t *testing.T) {
	var encoded bytes.Buffer
	_, _, err := WriteManifest(&encoded, Header{
		Project: "global", Created: time.Now(),
	}, func(yield func(Entry) error) error {
		for _, key := range []string{"b", "a"} {
			if err := yield(Entry{
				Eco: "npm", Key: key, Digest: blob.Digest(fmt.Sprintf("%064x", 1)), Size: 1,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		t.Fatal("unsorted manifest was accepted")
	}
}

// §8 fuzz target: manifest parsing. A manifest arrives inside an imported transfer pack
// from another host, so the reader is parsing input this instance did not produce. It
// must reject malformed input rather than panic or invent entries.
func FuzzManifestParsing(f *testing.F) {
	valid := func(entries []Entry) []byte {
		var buf bytes.Buffer
		if _, _, err := WriteManifest(&buf, Header{
			Project: "global", Created: time.Unix(0, 0).UTC(),
		}, func(yield func(Entry) error) error {
			for _, entry := range entries {
				if err := yield(entry); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			f.Fatalf("seed: %v", err)
		}
		return buf.Bytes()
	}

	digest := blob.Digest(fmt.Sprintf("%064x", 1))
	f.Add(valid(nil))
	f.Add(valid([]Entry{{Eco: "npm", Key: "a", Digest: digest, Size: 1}}))
	f.Add(valid([]Entry{
		{Eco: "npm", Key: "a", Digest: digest, Size: 1},
		{Eco: "pypi", Key: "b", Digest: digest, Size: 2},
	}))
	f.Add([]byte("not gzip at all"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, raw []byte) {
		it, err := NewIterator(bytes.NewReader(raw))
		if err != nil {
			return // rejected at the boundary, which is the point
		}
		defer func() { _ = it.Close() }()

		var seen int64
		count, bytesTotal, err := it.Walk(func(entry Entry) error {
			seen++
			// Anything the walk yields must be usable: a digest that cannot address a
			// blob, or a negative size, would corrupt an import that trusted it.
			if !entry.Digest.Valid() {
				t.Fatalf("yielded an invalid digest %q", entry.Digest)
			}
			if entry.Size < 0 {
				t.Fatalf("yielded a negative size %d", entry.Size)
			}
			if entry.Eco == "" || entry.Key == "" {
				t.Fatalf("yielded an unaddressable entry %+v", entry)
			}
			return nil
		})
		if err != nil {
			return
		}
		if count != seen {
			t.Fatalf("reported %d entries but yielded %d", count, seen)
		}
		if bytesTotal < 0 {
			t.Fatalf("negative total %d", bytesTotal)
		}
	})
}
