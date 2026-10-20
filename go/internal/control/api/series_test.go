package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
)

// The console's policy tile reads the instance-wide offline switch from here, and with
// no field to read it drew "online" for an instance started with -offline. Absent and
// false would look the same to that reader, so both states are asserted as present.
func TestCoordinatesReportInstanceOffline(t *testing.T) {
	for _, offline := range []bool{false, true} {
		snapshot := config.Defaults()
		snapshot.Upstream.Offline = offline
		api := &API{Options: Options{Config: config.NewStore(&snapshot)}}

		recorder := httptest.NewRecorder()
		if err := api.getCoordinates(recorder,
			httptest.NewRequest("GET", "http://cache.test/api/v1/coordinates", nil)); err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode coordinates: %v: %s", err, recorder.Body.String())
		}
		got, present := body["offline"]
		if !present {
			t.Fatalf("offline=%v: coordinates carry no offline field: %s", offline, recorder.Body.String())
		}
		if got != offline {
			t.Errorf("coordinates report offline=%v for an instance started with offline=%v", got, offline)
		}
	}
}
