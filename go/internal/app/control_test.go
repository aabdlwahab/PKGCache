package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/obs"
)

func controlApp(t *testing.T) *App {
	t.Helper()
	return configuredApp(t, func(snapshot *config.Snapshot) {
		snapshot.Auth.RootUser = "root"
		snapshot.Auth.RootPassword = "rootpass12"
	})
}

func controlLogin(t *testing.T, server *httptest.Server) *http.Cookie {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/login",
		strings.NewReader(`{"username":"root","password":"rootpass12"}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || len(response.Cookies()) != 1 {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("login = %d %q cookies=%v", response.StatusCode, body, response.Cookies())
	}
	return response.Cookies()[0]
}

func controlRequest(
	t *testing.T,
	client *http.Client,
	method, target string,
	cookie *http.Cookie,
	body string,
) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return response, payload
}

func TestPhase7ControlAPIIsLiveScopedAndLegacyCompatible(t *testing.T) {
	privateAuthenticated := make(chan bool, 1)
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		privateAuthenticated <- ok && username == "mirror" && password == "secret-pass"
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<a href="demo-1.0-py3-none-any.whl">demo</a>`)
	}))
	defer private.Close()
	a := controlApp(t)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()
	cookie := controlLogin(t, server)

	response, body := controlRequest(t, server.Client(), http.MethodPost,
		server.URL+"/api/v1/projects", cookie, `{"name":"team-a"}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create project = %d %s", response.StatusCode, body)
	}

	// The data-plane handler was built before this project existed. The next request
	// reaches files (an empty listing 404), rather than the unknown-project router.
	recorder := httptest.NewRecorder()
	a.UnifiedHandler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodGet, "/team-a/files/", nil))
	if strings.Contains(recorder.Body.String(), "unknown project") {
		t.Fatalf("new project was not live on the next request: %s", recorder.Body.String())
	}

	response, body = controlRequest(t, server.Client(), http.MethodPost,
		server.URL+"/api/v1/projects/team-a/upstreams", cookie,
		fmt.Sprintf(`{"eco":"pypi","name":"private","url":%q,`, private.URL)+
			`"kind":"origin","credential":{"label":"private","kind":"basic",`+
			`"username":"mirror","password":"secret-pass"}}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create upstream = %d %s", response.StatusCode, body)
	}
	recorder = httptest.NewRecorder()
	a.UnifiedHandler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodGet, "/team-a/pypi/+indexes", nil))
	if recorder.Code != http.StatusOK ||
		!strings.Contains(recorder.Body.String(), `"private":"`+private.URL+`"`) {
		t.Fatalf("live PyPI override = %d %s", recorder.Code, recorder.Body.String())
	}
	var createdUpstream struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &createdUpstream); err != nil || createdUpstream.ID == 0 {
		t.Fatalf("upstream response = %s, %v", body, err)
	}
	response, body = controlRequest(t, server.Client(), http.MethodPatch,
		fmt.Sprintf("%s/api/v1/projects/team-a/upstreams/%d", server.URL, createdUpstream.ID),
		cookie, `{"priority":5}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("partial upstream edit = %d %s", response.StatusCode, body)
	}
	credential := a.Config.Current().ProjectCredentials["team-a"]["pypi"]["private"]
	if credential.Username != "mirror" || credential.Password != "secret-pass" {
		t.Fatalf("live upstream credential = %+v", credential)
	}
	recorder = httptest.NewRecorder()
	a.UnifiedHandler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodGet, "/team-a/pypi/private/+simple/demo/", nil))
	authenticated := <-privateAuthenticated
	if recorder.Code != http.StatusOK || !authenticated {
		t.Fatalf("private PyPI request = %d authenticated=%v body=%s",
			recorder.Code, authenticated, recorder.Body.String())
	}

	response, body = controlRequest(t, server.Client(), http.MethodPost,
		server.URL+"/api/v1/tokens", cookie,
		`{"project":"team-a","eco":"files","scope":"write","label":"ci"}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create token = %d %s", response.StatusCode, body)
	}
	var issued struct {
		Token struct {
			ID string `json:"id"`
		} `json:"token"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(body, &issued); err != nil || issued.Secret == "" {
		t.Fatalf("token response = %s, %v", body, err)
	}

	response, body = controlRequest(t, server.Client(), http.MethodPatch,
		server.URL+"/api/v1/projects/team-a", cookie,
		`{"data_plane_auth":"token"}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("enable project auth = %d %s", response.StatusCode, body)
	}

	put := httptest.NewRequest(http.MethodPut, "/team-a/files/build/app.bin",
		strings.NewReader("artifact"))
	put.Header.Set("Authorization", "Bearer "+issued.Secret)
	recorder = httptest.NewRecorder()
	a.UnifiedHandler().ServeHTTP(recorder, put)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("scoped files PUT = %d %s", recorder.Code, recorder.Body.String())
	}

	// A write token can also read: the data plane demands the read scope for every GET,
	// so a CI credential that cannot install what it just published would force every
	// pipeline onto an admin token.
	get := httptest.NewRequest(http.MethodGet, "/team-a/files/build/app.bin", nil)
	get.Header.Set("Authorization", "Bearer "+issued.Secret)
	recorder = httptest.NewRecorder()
	a.UnifiedHandler().ServeHTTP(recorder, get)
	if recorder.Code != http.StatusOK {
		t.Fatalf("write token read = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}

	// The ladder only runs upward. A read token must not be able to publish.
	response, body = controlRequest(t, server.Client(), http.MethodPost,
		server.URL+"/api/v1/tokens", cookie,
		`{"project":"team-a","eco":"files","scope":"read","label":"pull-only"}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("issue read token = %d %s", response.StatusCode, body)
	}
	var readOnly struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(body, &readOnly); err != nil || readOnly.Secret == "" {
		t.Fatalf("read token response = %s, %v", body, err)
	}
	put = httptest.NewRequest(http.MethodPut, "/team-a/files/build/other.bin",
		strings.NewReader("nope"))
	put.Header.Set("Authorization", "Bearer "+readOnly.Secret)
	recorder = httptest.NewRecorder()
	a.UnifiedHandler().ServeHTTP(recorder, put)
	if recorder.Code < 400 {
		t.Fatalf("read token published: %d %s", recorder.Code, recorder.Body.String())
	}

	// A project-scoped token cannot write another project.
	put = httptest.NewRequest(http.MethodPut, "/global/files/app.bin",
		strings.NewReader("escape"))
	put.Header.Set("Authorization", "Bearer "+issued.Secret)
	recorder = httptest.NewRecorder()
	a.UnifiedHandler().ServeHTTP(recorder, put)
	if recorder.Code < 400 {
		t.Fatalf("project-scoped token wrote global: %d", recorder.Code)
	}

	response, body = controlRequest(t, server.Client(), http.MethodGet,
		server.URL+"/api/projects", cookie, "")
	if response.StatusCode != http.StatusOK ||
		!bytes.Contains(body, []byte(`"name":"team-a"`)) ||
		!bytes.Contains(body, []byte(`"ports"`)) {
		t.Fatalf("legacy project API = %d %s", response.StatusCode, body)
	}
	for _, endpoint := range []string{
		"/api/proxies?project=team-a",
		"/api/downloads?project=team-a",
		"/api/recent?project=team-a",
		"/api/manifests?project=team-a",
		"/api/stats?project=team-a",
		"/api/history?project=team-a",
		"/api/endpoints?project=team-a",
		"/api/shuttle?project=team-a",
		"/api/packages?project=team-a",
		"/api/token?project=team-a",
		"/api/jobs",
		"/api/me",
		"/api/users",
	} {
		response, payload := controlRequest(t, server.Client(), http.MethodGet,
			server.URL+endpoint, cookie, "")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("legacy %s = %d %s", endpoint, response.StatusCode, payload)
		}
		if strings.HasPrefix(endpoint, "/api/endpoints") &&
			(!bytes.Contains(payload, []byte(`"url"`)) ||
				!bytes.Contains(payload, []byte(`"setup"`))) {
			t.Fatalf("legacy endpoint shape = %s", payload)
		}
	}

	response, body = controlRequest(t, server.Client(), http.MethodGet,
		server.URL+"/api/v1/projects/ghost", cookie, "")
	var apiError map[string]string
	_ = json.Unmarshal(body, &apiError)
	if response.StatusCode != http.StatusNotFound ||
		apiError["error"] == "" || apiError["code"] == "" {
		t.Fatalf("uniform error = %d %s", response.StatusCode, body)
	}

	request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/projects",
		strings.NewReader(`{"name":"csrf-project"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://evil.example")
	request.AddCookie(cookie)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin mutation = %d, want 403", response.StatusCode)
	}
	if a.Config.Current().HasProject("csrf-project") {
		t.Fatal("cross-origin mutation changed control state")
	}

	audit, err := a.Control.ListAudit(100)
	if err != nil || len(audit) < 4 {
		t.Fatalf("audit records = %d, %v", len(audit), err)
	}
}

func TestSSEDeliversProgressWithin100Milliseconds(t *testing.T) {
	a := controlApp(t)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()
	cookie := controlLogin(t, server)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		server.URL+"/api/v1/events", nil)
	request.AddCookie(cookie)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	started := time.Now()
	a.Events.Publish(obs.Event{
		Kind: obs.EventFetchProgress, Project: "global", Eco: "pypi",
		ID: "demo.whl", Size: 64,
	})
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			if elapsed := time.Since(started); elapsed >= 100*time.Millisecond {
				t.Fatalf("SSE progress took %s, want <100ms", elapsed)
			}
			if !strings.Contains(scanner.Text(), `"project":"global"`) {
				t.Fatalf("SSE payload = %s", scanner.Text())
			}
			return
		}
	}
	t.Fatalf("SSE stream ended before event: %v", scanner.Err())
}
