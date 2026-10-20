package local

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/eco/ecotest"
	"github.com/aabdlwahab/PKGCache/internal/eco/oci"
	testupstream "github.com/aabdlwahab/PKGCache/internal/testutil/upstream"
)

type teamTokens struct{ token string }

func (t teamTokens) HasToken(_, _, scope string) bool { return scope == "write" }
func (t teamTokens) VerifyToken(_, _, scope, presented string) bool {
	return scope == "write" && presented == t.token
}

func registryHarness(t *testing.T, policy oci.PushPolicy) *ecotest.Harness {
	t.Helper()
	return ecotest.New(t, func(*testupstream.Server) eco.Ecosystem {
		return oci.NewWithRegistries(map[string]string{}).WithPush(policy)
	})
}

func request(t *testing.T, h *ecotest.Harness, method, path string, body []byte, header http.Header) *ecotest.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, h.URL(path), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range header {
		req.Header[name] = values
	}
	return h.Do(req)
}

// pushImage puts a two-layer image on a machine's cache the way docker push leaves it.
func pushImage(t *testing.T, h *ecotest.Harness, repo, tag string) []byte {
	t.Helper()
	var digests []blob.Digest
	for _, content := range []string{"{}", "layer bytes"} {
		digest := blob.DigestBytes([]byte(content))
		got := request(t, h, http.MethodPost, "/v2/local/"+repo+"/blobs/uploads/?digest="+digest.Prefixed(), []byte(content), nil)
		if got.Status != http.StatusCreated {
			t.Fatalf("push %q: %d %s", content, got.Status, got.Text())
		}
		digests = append(digests, digest)
	}
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",`+
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":2},`+
		`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"%s","size":11}]}`,
		digests[0].Prefixed(), digests[1].Prefixed()))
	got := request(t, h, http.MethodPut, "/v2/local/"+repo+"/manifests/"+tag, manifest,
		http.Header{"Content-Type": {"application/vnd.oci.image.manifest.v1+json"}})
	if got.Status != http.StatusCreated {
		t.Fatalf("push manifest: %d %s", got.Status, got.Text())
	}
	return manifest
}

// An image pushed to a machine's cache arrives on the team whole, with the machine's token,
// and a second push of it sends no layer again.
func TestImagePushedHereIsSentOnToTheTeam(t *testing.T) {
	machine := registryHarness(t, oci.PushPolicy{Open: true})
	team := registryHarness(t, oci.PushPolicy{Tokens: teamTokens{token: "id.secret"}})
	manifest := pushImage(t, machine, "tools/mold", "1.0")

	push := TeamPush{
		Source:     strings.TrimPrefix(machine.Server.URL, "http://"),
		Repository: "tools/mold", Tag: "1.0",
		Team:   Team{Server: team.Server.URL, Token: "id.secret"},
		Client: team.Server.Client(),
	}
	result, err := PushToTeam(t.Context(), push)
	if err != nil {
		t.Fatal(err)
	}
	if result.Layers != 2 || result.Sent != 2 || result.Digest != blob.DigestBytes(manifest).Prefixed() {
		t.Fatalf("result = %+v", result)
	}
	if want := strings.TrimPrefix(team.Server.URL, "http://") + "/local/tools/mold:1.0"; result.Target != want {
		t.Fatalf("target %q, want %q", result.Target, want)
	}
	pulled := team.Get("/v2/local/tools/mold/manifests/1.0")
	if pulled.Status != http.StatusOK || !bytes.Equal(pulled.Body, manifest) {
		t.Fatalf("the team does not serve the image: %d %s", pulled.Status, pulled.Text())
	}

	again, err := PushToTeam(t.Context(), push)
	if err != nil {
		t.Fatal(err)
	}
	if again.Sent != 0 {
		t.Fatalf("a second push sent %d layers the team already had", again.Sent)
	}
}

// Without a token there is nothing to push with, and the answer says how to get one rather
// than leaving a 401 to be puzzled over.
func TestPushToTheTeamWithoutATokenSaysHowToGetOne(t *testing.T) {
	_, err := PushToTeam(t.Context(), TeamPush{
		Source: "127.0.0.1:1", Repository: "app", Tag: "1",
		Team: Team{Server: "https://team.internal:8443", Fingerprint: "AB:CD"},
	})
	if !errors.Is(err, ErrNoTeamToken) || !strings.Contains(err.Error(), "-token-file -") {
		t.Fatalf("err = %v", err)
	}
}

// A token the team refuses — a read token, or one for another project — is named as the
// problem, since that is the one thing the person pushing can change.
func TestTeamRefusingTheTokenIsExplained(t *testing.T) {
	machine := registryHarness(t, oci.PushPolicy{Open: true})
	team := registryHarness(t, oci.PushPolicy{Tokens: teamTokens{token: "id.secret"}})
	pushImage(t, machine, "app", "1")
	_, err := PushToTeam(t.Context(), TeamPush{
		Source: strings.TrimPrefix(machine.Server.URL, "http://"), Repository: "app", Tag: "1",
		Team: Team{Server: team.Server.URL, Token: "id.read-only"}, Client: team.Server.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "scope write") {
		t.Fatalf("err = %v", err)
	}
}
