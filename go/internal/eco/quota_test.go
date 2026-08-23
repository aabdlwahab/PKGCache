package eco

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/catalog"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/router"
)

func TestWriteErrorQuotaIs507WithUsage(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/global/files/a", nil)
	snapshot := config.Defaults()
	ctx := NewCtx(recorder, request, "global", "/global/files", router.Params{},
		nil, &snapshot, Descriptor{ID: "files"})
	ctx.WriteError(&catalog.QuotaError{
		Kind: "bytes", Usage: 9, Limit: 10, Attempt: 12,
	})
	if recorder.Code != http.StatusInsufficientStorage {
		t.Fatalf("status=%d", recorder.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["usage"] != float64(9) || body["limit"] != float64(10) {
		t.Fatalf("body=%v", body)
	}
}
