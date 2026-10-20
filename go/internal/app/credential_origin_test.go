package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A credential goes only to the origin it was configured against. An edit that moved an
// upstream to another host used to keep it, so the next request handed the secret — a
// team token among them — to whatever host the new URL named; and a new row could borrow
// an existing row's credential by its id.
func TestUpstreamCredentialStaysWithItsOrigin(t *testing.T) {
	a := controlApp(t)
	server := httptest.NewServer(a.AdminHandler())
	defer server.Close()
	cookie := controlLogin(t, server)
	client := server.Client()

	response, body := controlRequest(t, client, http.MethodPost, server.URL+"/api/v1/projects", cookie, `{"name":"team-a"}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create project = %d %s", response.StatusCode, body)
	}
	response, body = controlRequest(t, client, http.MethodPost, server.URL+"/api/v1/projects/team-a/upstreams", cookie,
		`{"eco":"pypi","name":"private","url":"https://mirror.internal/simple","kind":"origin",`+
			`"credential":{"label":"private","kind":"basic","username":"mirror","password":"secret-pass"}}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create upstream = %d %s", response.StatusCode, body)
	}
	var row struct {
		ID           int64  `json:"id"`
		CredentialID *int64 `json:"credential_id"`
	}
	if err := json.Unmarshal(body, &row); err != nil || row.CredentialID == nil {
		t.Fatalf("upstream = %s, %v", body, err)
	}
	edit := func(url string) string {
		t.Helper()
		response, body := controlRequest(t, client, http.MethodPatch,
			fmt.Sprintf("%s/api/v1/projects/team-a/upstreams/%d", server.URL, row.ID), cookie,
			fmt.Sprintf(`{"url":%q}`, url))
		if response.StatusCode != http.StatusOK {
			t.Fatalf("edit to %s = %d %s", url, response.StatusCode, body)
		}
		chain := a.Config.Current().ProjectUpstreams["team-a"]["pypi"]["private"]
		if len(chain) != 1 {
			t.Fatalf("chain after edit to %s = %+v", url, chain)
		}
		return chain[0].Credential.Password
	}

	if got := edit("https://mirror.internal/other/simple"); got != "secret-pass" {
		t.Fatalf("a path edit on the same origin lost the credential (%q)", got)
	}
	if got := edit("https://elsewhere.example/simple"); got != "" {
		t.Fatalf("the credential followed the row to another host: %q", got)
	}
	if _, err := a.Credentials.Get(*row.CredentialID); err == nil {
		t.Fatal("the dropped credential is still stored")
	}

	// A second row on the original host, and one that names the first row's credential
	// by id on a host of its own: neither gets a secret it was not created with.
	response, body = controlRequest(t, client, http.MethodPost, server.URL+"/api/v1/projects/team-a/upstreams", cookie,
		`{"eco":"pypi","name":"second","url":"https://mirror.internal/simple","kind":"origin",`+
			`"credential":{"label":"second","kind":"basic","username":"mirror","password":"other-pass"}}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create second upstream = %d %s", response.StatusCode, body)
	}
	var second struct {
		CredentialID *int64 `json:"credential_id"`
	}
	_ = json.Unmarshal(body, &second)
	response, body = controlRequest(t, client, http.MethodPost, server.URL+"/api/v1/projects/team-a/upstreams", cookie,
		fmt.Sprintf(`{"eco":"pypi","name":"borrowed","url":"https://attacker.example/simple","kind":"origin","credential_id":%d}`,
			*second.CredentialID))
	if response.StatusCode == http.StatusCreated {
		chain := a.Config.Current().ProjectUpstreams["team-a"]["pypi"]["borrowed"]
		if len(chain) == 1 && chain[0].Credential.Password != "" {
			t.Fatal("a new row borrowed another row's credential by its id")
		}
	}
	if got := edit("https://mirror.internal:8443/simple"); got != "" {
		t.Fatalf("another port on the same host kept the credential: %q", got)
	}
}
