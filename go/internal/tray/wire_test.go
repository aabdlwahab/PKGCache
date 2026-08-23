package tray

import (
	"encoding/json"
	"strings"
	"testing"
)

// The state and the menu cross a pipe into a helper written in another language, so their
// field names are a wire format. Go's default is the Go field name — capitalised — and the
// helper reads lowercase keys, which would have decoded to nothing at all: an icon
// permanently "asleep" that never turned red, on the one platform where nothing could be
// tested here.
func TestStateWireNamesAreLowercase(t *testing.T) {
	encoded, err := json.Marshal(State{
		Running: true, Full: true, Project: "work", Used: 1 << 20, Limit: 1 << 30, Served: 0.64,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"running", "full", "project", "used", "limit", "served"} {
		if !strings.Contains(string(encoded), `"`+key+`"`) {
			t.Errorf("state carries no %q key: %s", key, encoded)
		}
	}
	if strings.Contains(string(encoded), `"Running"`) {
		t.Errorf("state uses Go field names on the wire: %s", encoded)
	}
}
