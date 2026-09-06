package ops

import (
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/snapshot"
)

// The name is the only thing about a pack a person reads before opening it, and these
// files are made to be carried somewhere and read back weeks later. So the shape is
// pinned: change it deliberately, not by editing a format string.
func TestDefaultPackName(t *testing.T) {
	made := time.Date(2026, 9, 6, 15, 30, 45, 0, time.UTC)
	target := "241f47c5ec0f3700731ba148aaaaaaaabbbbbbbbccccccccdddddddd00000000"
	base := "7b69cf291a04ffffffffeeeeeeeeddddddddccccccccbbbbbbbbaaaaaaaa1111"

	for _, test := range []struct {
		name string
		pack snapshot.Pack
		want string
	}{
		{
			name: "a full pack names its checkpoint",
			pack: snapshot.Pack{Target: target},
			want: "pkgreg-work-full-20260906T153045Z-241f47c5ec0f.tar",
		},
		{
			// The case the old name could not express: same target, different content.
			name: "a delta names both ends",
			pack: snapshot.Pack{Base: base, Target: target},
			want: "pkgreg-work-delta-20260906T153045Z-7b69cf291a04-241f47c5ec0f.tar",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := defaultPackName("work", test.pack, made); got != test.want {
				t.Fatalf("named it\n  %s\nwant\n  %s", got, test.want)
			}
		})
	}
}

// A local clock is stamped as UTC, so two machines in two time zones exporting the same
// checkpoint at the same moment produce the same name rather than two that sort apart.
func TestDefaultPackNameIsUTC(t *testing.T) {
	zone := time.FixedZone("UTC+5", 5*60*60)
	made := time.Date(2026, 9, 6, 20, 30, 45, 0, zone)
	got := defaultPackName("work", snapshot.Pack{Target: "241f47c5ec0f3700"}, made)
	want := "pkgreg-work-full-20260906T153045Z-241f47c5ec0f.tar"
	if got != want {
		t.Fatalf("named it\n  %s\nwant\n  %s", got, want)
	}
}
