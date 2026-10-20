package main

import (
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/local"
)

// With -no-cache this machine is a bridge and nothing else, so the bridge is the only
// thing that can present a token-protected team project's token. It has to reach every
// request, or such a project is a 401 on the first package.
func TestTheNoCacheBridgePresentsTheTeamToken(t *testing.T) {
	seen := make(chan string, 1)
	team := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer team.Close()

	dir := t.TempDir()
	var set local.TeamSet
	set.Set(config.GlobalProject, local.Team{
		Server: team.URL, Project: "work", NoCache: true, Token: "s3cret",
		CAPEM: string(pem.EncodeToMemory(&pem.Block{
			Type: "CERTIFICATE", Bytes: team.Certificate().Raw,
		})),
	})
	if err := local.WriteTeams(dir, set); err != nil {
		t.Fatal(err)
	}

	state, bridgedTo, stop, err := reachCache(context.Background(), &config.Snapshot{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	// The bridge forwards paths as they are, so a session's URLs have to name the team's
	// project; the local current project would land every request in the team's global.
	if bridgedTo != "work" {
		t.Fatalf("a bridge to the team's project work scoped the session to %q", bridgedTo)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://"+state.Addr+"/work/npm/left-pad", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if got := <-seen; got != "Bearer s3cret" {
		t.Fatalf("the team cache saw Authorization %q", got)
	}
}
