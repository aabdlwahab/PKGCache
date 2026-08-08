package peer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/brightskies/pkgreg/internal/blob"
	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/obs"
	"github.com/brightskies/pkgreg/internal/upstream"
)

type staticTokens struct{}

func (staticTokens) VerifyToken(project, eco, scope, token string) bool {
	return project == config.GlobalProject && eco == "" && scope == "peer" && token == "peer-secret"
}

func TestTwoInstancesTransferByDigest(t *testing.T) {
	source, err := blob.Open(filepath.Join(t.TempDir(), "source"))
	if err != nil {
		t.Fatal(err)
	}
	writer, _ := source.Create()
	_, _ = writer.Write([]byte("served by sibling without origin"))
	digest, size, err := writer.Commit()
	if err != nil {
		t.Fatal(err)
	}

	sourceCfg := config.Defaults()
	sourceStore := config.NewStore(&sourceCfg)
	sourcePeer := New(source, sourceStore, nil, staticTokens{})
	server := httptest.NewServer(sourcePeer)
	defer server.Close()

	destination, err := blob.Open(filepath.Join(t.TempDir(), "destination"))
	if err != nil {
		t.Fatal(err)
	}
	destinationCfg := config.Defaults()
	destinationCfg.Upstream.RequestTimeout = 5 * time.Second
	destinationCfg.ProjectPeers = map[string]map[string][]config.Peer{
		config.GlobalProject: {
			"pypi": {{
				URL: server.URL,
				Credential: config.UpstreamCredential{
					Kind: "bearer", Token: "peer-secret",
				},
			}},
		},
	}
	cfg := config.NewStore(&destinationCfg)
	pool := upstream.New(destinationCfg.Upstream, obs.NewMetrics())
	defer pool.CloseIdleConnections()
	destinationPeer := New(destination, cfg, pool, nil)

	found, gotSize, err := destinationPeer.Fetch(
		context.Background(), config.GlobalProject, "pypi", digest,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !found || gotSize != size {
		t.Fatalf("found=%v size=%d want %d", found, gotSize, size)
	}
	file, _, err := destination.Open(digest)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	body, _ := io.ReadAll(file)
	if string(body) != "served by sibling without origin" {
		t.Fatalf("body=%q", body)
	}
}

func TestPeerRequiresTokenAndSupportsRangeAndHave(t *testing.T) {
	store, err := blob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writer, _ := store.Create()
	_, _ = writer.Write([]byte("0123456789"))
	digest, _, _ := writer.Commit()
	service := New(store, config.NewStore(func() *config.Snapshot {
		value := config.Defaults()
		return &value
	}()), nil, staticTokens{})

	unauthorized := httptest.NewRecorder()
	service.ServeHTTP(unauthorized, httptest.NewRequest(
		http.MethodHead, "/peer/v1/blob/"+digest.String(), nil,
	))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/peer/v1/blob/"+digest.String(), nil)
	request.Header.Set("Authorization", "Bearer peer-secret")
	request.Header.Set("Range", "bytes=2-5")
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent || response.Body.String() != "2345" {
		t.Fatalf("range status=%d body=%q", response.Code, response.Body.String())
	}

	haveRequest := httptest.NewRequest(
		http.MethodGet, "/peer/v1/have?d="+digest.String(), nil,
	)
	haveRequest.Header.Set("Authorization", "Bearer peer-secret")
	have := httptest.NewRecorder()
	service.ServeHTTP(have, haveRequest)
	if have.Code != http.StatusOK || !contains(have.Body.String(), digest.String()) {
		t.Fatalf("have status=%d body=%q", have.Code, have.Body.String())
	}
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
