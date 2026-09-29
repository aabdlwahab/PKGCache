package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/router"
)

// log.access is one line per data-plane request, read live: switching it on takes effect
// on the next request, and off means nothing at all.
func TestAccessLogIsOneLinePerDataPlaneRequest(t *testing.T) {
	a := newApp(t)
	setAccess := func(on bool) {
		t.Helper()
		if err := a.Config.Apply(func(s *config.Snapshot) error {
			s.Log.Access = on
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	setAccess(false)
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	handler := a.UnifiedHandler()

	get := func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/global/pypi/+indexes", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
		}
	}
	get()
	if strings.Contains(logged.String(), `"msg":"access"`) {
		t.Fatalf("logged with log.access off:\n%s", logged.String())
	}

	setAccess(true)
	get()
	var line struct {
		Msg, Project, Eco, Method, URL string
		Status                         int
		Bytes                          int64
	}
	for _, raw := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if strings.Contains(raw, `"msg":"access"`) {
			if err := json.Unmarshal([]byte(raw), &line); err != nil {
				t.Fatal(err)
			}
		}
	}
	if line.Project != "global" || line.Eco != "pypi" || line.Method != http.MethodGet ||
		line.URL != "/global/pypi/+indexes" || line.Status != http.StatusOK || line.Bytes == 0 {
		t.Fatalf("access line = %+v\n%s", line, logged.String())
	}
}

// A client that left before anything was sent is logged as 499, not as an empty success.
func TestAccessLogMarksAClientThatLeftFirst(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/global/pypi/root/pypi/+f/x/x.whl", nil).WithContext(ctx)
	logAccess(&accessWriter{ResponseWriter: httptest.NewRecorder()}, req,
		router.Target{Project: "global", Eco: "pypi"}, time.Now())
	if !strings.Contains(logged.String(), `"status":499`) {
		t.Fatalf("logged %s", logged.String())
	}
}
