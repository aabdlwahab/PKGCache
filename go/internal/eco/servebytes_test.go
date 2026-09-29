package eco

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A document served twice is recognised the second time: the first answer carries a
// validator over exactly its bytes, and asking with it gets a 304 with no body. An
// adapter's own validator is kept, and a changed document gets a new one.
func TestServeBytesAnswersARevalidationWithNotModified(t *testing.T) {
	serve := func(method, ifNoneMatch, etag string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, "/left-pad", nil)
		if ifNoneMatch != "" {
			request.Header.Set("If-None-Match", ifNoneMatch)
		}
		recorder := httptest.NewRecorder()
		if etag != "" {
			recorder.Header().Set("ETag", etag)
		}
		c := &Ctx{W: recorder, R: request}
		if err := c.ServeBytes(http.StatusOK, "application/json", body); err != nil {
			t.Fatal(err)
		}
		return recorder
	}
	body := []byte(`{"name":"left-pad"}`)
	first := serve(http.MethodGet, "", "", body)
	validator := first.Header().Get("ETag")
	if first.Code != http.StatusOK || validator == "" || first.Body.String() != string(body) {
		t.Fatalf("first answer = %d, ETag %q, %q", first.Code, validator, first.Body.String())
	}
	for _, header := range []string{validator, "W/" + validator, `"other", ` + validator, "*"} {
		again := serve(http.MethodGet, header, "", body)
		if again.Code != http.StatusNotModified || again.Body.Len() != 0 || again.Header().Get("ETag") != validator {
			t.Errorf("If-None-Match %s: %d with %d bytes, ETag %q", header, again.Code, again.Body.Len(), again.Header().Get("ETag"))
		}
	}
	if changed := serve(http.MethodGet, validator, "", []byte(`{"name":"left-pad","v":2}`)); changed.Code != http.StatusOK ||
		changed.Header().Get("ETag") == validator {
		t.Errorf("a changed document: %d, ETag %q", changed.Code, changed.Header().Get("ETag"))
	}
	own := `"sha256:0123"`
	if manifest := serve(http.MethodGet, own, own, body); manifest.Code != http.StatusNotModified ||
		manifest.Header().Get("ETag") != own {
		t.Errorf("an adapter's own validator: %d, ETag %q", manifest.Code, manifest.Header().Get("ETag"))
	}
	if head := serve(http.MethodHead, "", "", body); head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("ETag") != validator {
		t.Errorf("HEAD: %d, %d bytes, ETag %q", head.Code, head.Body.Len(), head.Header().Get("ETag"))
	}
}
