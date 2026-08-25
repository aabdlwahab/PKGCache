package local

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
)

const (
	caOne = "-----BEGIN CERTIFICATE-----\nONE\n-----END CERTIFICATE-----"
	caTwo = "-----BEGIN CERTIFICATE-----\nTWO\n-----END CERTIFICATE-----"
)

// The document written before configuration was per project is a single team object.
// Reading it as the global project's is what keeps a cache configured yesterday working,
// and it is the kind of compatibility that is only ever verified by a test.
func TestLegacyTeamFileReadsAsTheGlobalProject(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"server":"https://cache.internal:8443","fingerprint":"AB","project":"global","direct":true}`
	if err := os.WriteFile(filepath.Join(dir, "team.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := ReadTeams(dir)
	if err != nil {
		t.Fatal(err)
	}
	team, has := set.For(config.GlobalProject)
	if !has || team.Server != "https://cache.internal:8443" {
		t.Fatalf("the legacy document was not read: %+v", set)
	}
	// And it is the fallback for a project that did not exist when it was written.
	if _, has := set.For("work"); !has {
		t.Fatal("a new project did not inherit the legacy configuration")
	}
}

// A CA kept in the file the pool reads, from before it was kept in the record, is
// adopted — otherwise the first write would rebuild the bundle without it and the
// machine would stop trusting the cache it was already using.
func TestLegacyTeamCAIsAdopted(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"server":"https://cache.internal:8443","direct":true}`
	if err := os.WriteFile(filepath.Join(dir, "team.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(TeamCAPath(dir), []byte(caOne), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := ReadTeams(dir)
	if err != nil {
		t.Fatal(err)
	}
	team, _ := set.For(config.GlobalProject)
	if !strings.Contains(team.CAPEM, "ONE") {
		t.Fatalf("the existing CA was not adopted: %q", team.CAPEM)
	}
	if err := WriteTeams(dir, set); err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile(TeamCAPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bundle), "ONE") {
		t.Fatal("rewriting the configuration dropped the CA it was already using")
	}
}

// The fallback rule, stated as a test: a project's own configuration wins, the global
// project's is inherited, and Own is what tells the two apart for anything that prints
// or removes them.
func TestTeamResolutionFallsBackToGlobal(t *testing.T) {
	var set TeamSet
	set.Set(config.GlobalProject, Team{Server: "https://team", Project: "global", Direct: true})
	set.Set("work", Team{Server: "https://work-team", Project: "work"})

	if team, has := set.For("work"); !has || team.Server != "https://work-team" {
		t.Fatalf("work resolved to %+v", team)
	}
	if team, has := set.For("side"); !has || team.Server != "https://team" {
		t.Fatalf("side did not inherit the global chain: %+v", team)
	}
	if _, own := set.Own("side"); own {
		t.Fatal("an inherited chain was reported as the project's own")
	}
	if _, own := set.Own("work"); !own {
		t.Fatal("a project's own chain was reported as inherited")
	}

	set.Remove("work")
	if team, has := set.For("work"); !has || team.Server != "https://team" {
		t.Fatalf("removing work's own chain did not fall back to global: %+v", team)
	}
}

// Two team caches mean two self-minted CAs and one outbound pool, so the file the pool
// reads is a bundle. The failure this guards is the quiet one: a bundle that only ever
// grows keeps a laptop trusting a cache it was moved off months ago.
func TestTrustBundleHoldsEveryCAAndForgetsRemovedOnes(t *testing.T) {
	dir := t.TempDir()
	var set TeamSet
	set.Set(config.GlobalProject, Team{Server: "https://one", CAPEM: caOne})
	set.Set("work", Team{Server: "https://two", CAPEM: caTwo})
	if err := WriteTeams(dir, set); err != nil {
		t.Fatal(err)
	}
	bundle := read(t, TeamCAPath(dir))
	if !strings.Contains(bundle, "ONE") || !strings.Contains(bundle, "TWO") {
		t.Fatalf("the bundle is missing a CA:\n%s", bundle)
	}

	set.Remove("work")
	if err := WriteTeams(dir, set); err != nil {
		t.Fatal(err)
	}
	bundle = read(t, TeamCAPath(dir))
	if strings.Contains(bundle, "TWO") {
		t.Fatalf("a removed team cache is still trusted:\n%s", bundle)
	}
	if !strings.Contains(bundle, "ONE") {
		t.Fatalf("removing one project's team cache removed another's CA:\n%s", bundle)
	}

	// The last one goes, and so does the file: an empty ca_file is a configuration error
	// to the pool, not an empty trust store.
	set.Remove(config.GlobalProject)
	if err := WriteTeams(dir, set); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(TeamCAPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("the bundle outlived the last team cache: %v", err)
	}
}

// Two projects reaching the same cache is the ordinary case, and it must not write the
// certificate twice: a pool that parses a duplicate is fine, a file that grows on every
// setup is not.
func TestTrustBundleWritesEachCAOnce(t *testing.T) {
	dir := t.TempDir()
	var set TeamSet
	set.Set(config.GlobalProject, Team{Server: "https://one", CAPEM: caOne})
	set.Set("work", Team{Server: "https://one", CAPEM: caOne})
	if err := WriteTeams(dir, set); err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(read(t, TeamCAPath(dir)), "BEGIN CERTIFICATE"); count != 1 {
		t.Fatalf("the bundle holds the same CA %d times", count)
	}
}

// -no-cache is a promise about the machine, so only the global project's entry can carry
// it. Anything else would be a store that is open for one project and not another.
func TestBridgedIsOnlyEverTheGlobalProject(t *testing.T) {
	var set TeamSet
	set.Set("work", Team{Server: "https://team", NoCache: true})
	if _, bridged := set.Bridged(); bridged {
		t.Fatal("a project other than global put the machine in bridge-only mode")
	}
	set.Set(config.GlobalProject, Team{Server: "https://team", NoCache: true})
	if _, bridged := set.Bridged(); !bridged {
		t.Fatal("the global project's -no-cache was not honoured")
	}
}

// publicOrigins counts the indexes that have a public origin to fall back to. Every
// index has a team row; the wildcard that carries discovered registries has no second
// row, because the origin behind it is whatever the image name turns out to say.
func publicOrigins() int {
	count := 0
	for _, entry := range chainedEcosystems {
		if entry.public != "" {
			count++
		}
	}
	return count
}

// The chain is the policy: -no-direct is the absence of the public row rather than a
// flag anybody has to remember, and that is what makes a machine that must never reach
// the internet provably unable to.
func TestChainRowsAreTheWholePolicy(t *testing.T) {
	direct := ChainRows(Team{Server: "https://team/", Project: "work", Direct: true}, nil)
	if len(direct) != len(chainedEcosystems)+publicOrigins() {
		t.Fatalf("a direct chain has %d rows, want one per index plus one per public origin",
			len(direct))
	}
	for _, row := range direct {
		if row.Priority != teamPriority && row.Priority != publicPriority {
			t.Fatalf("row at priority %d is not part of a chain", row.Priority)
		}
		if !managedRow(row) {
			t.Fatalf("a row this wrote is not recognised as its own: %+v", row)
		}
	}
	if !strings.Contains(direct[0].URL, "/work/") {
		t.Fatalf("the team URL does not carry the remote project: %s", direct[0].URL)
	}
	if strings.Contains(direct[0].URL, "//pypi") {
		t.Fatalf("a trailing slash on the server leaked into the URL: %s", direct[0].URL)
	}

	walled := ChainRows(Team{Server: "https://team", Project: "work"}, nil)
	if len(walled) != len(chainedEcosystems) {
		t.Fatalf("-no-direct produced %d rows, want one per index", len(walled))
	}
	for _, row := range walled {
		if row.Priority != teamPriority {
			t.Fatalf("-no-direct left a fallback row: %+v", row)
		}
	}

	// An empty remote project is the team's global one, never an empty path segment.
	//
	// What that looks like differs by ecosystem, and getting it wrong is silent rather
	// than loud. pypi and npm name the project as a leading path segment. OCI must not:
	// the distribution spec fixes /v2 as the API root, so the server reads the project
	// from the segment after it, and a literal "global" there would be taken for the
	// name of a registry — leaving the chain pointing at a registry nobody configured.
	for _, row := range ChainRows(Team{Server: "https://team"}, nil) {
		named := strings.Contains(row.URL, "/"+config.GlobalProject+"/")
		switch {
		case row.Eco == "oci" && named:
			t.Fatalf("the global project was named inside an OCI URL: %s", row.URL)
		case row.Eco != "oci" && !named:
			t.Fatalf("an unnamed remote project produced %s", row.URL)
		}
	}
}

// The OCI chain has to land on the exact paths the server's own router parses back,
// because nothing downstream will notice if it does not: a wrong path is a 404 from a
// real registry, which looks like a missing image rather than a misrouted cache.
func TestOCIChainRowsMatchTheServersRouting(t *testing.T) {
	rows := func(project string) map[string]string {
		out := map[string]string{}
		for _, row := range ChainRows(
			Team{Server: "https://team:8443", Project: project, Direct: true}, nil) {
			if row.Eco == "oci" && row.Priority == teamPriority {
				out[row.Name] = row.URL
			}
		}
		return out
	}

	global := rows(config.GlobalProject)
	if got, want := global["dockerhub"], "https://team:8443/v2/dockerhub"; got != want {
		t.Fatalf("global project: %s, want %s", got, want)
	}
	if got, want := global["ghcr"], "https://team:8443/v2/ghcr"; got != want {
		t.Fatalf("global project: %s, want %s", got, want)
	}

	named := rows("acme")
	if got, want := named["dockerhub"], "https://team:8443/v2/acme/dockerhub"; got != want {
		t.Fatalf("named project: %s, want %s", got, want)
	}

	// The public half of every OCI chain carries its own /v2, because a fallback is the
	// head's URL with its prefix swapped — see TestOCIFallbackKeepsTheDistributionAPIRoot.
	for _, row := range ChainRows(Team{Server: "https://team", Direct: true}, nil) {
		if row.Eco != "oci" || row.Priority != publicPriority {
			continue
		}
		if !strings.HasSuffix(row.URL, "/v2") {
			t.Fatalf("public OCI origin %q does not name the distribution API root", row.URL)
		}
	}
}

// A skipped ecosystem is one the instance does not have, and it must not become a row
// the daemon will reject.
func TestChainRowsSkipUnknownEcosystems(t *testing.T) {
	rows := ChainRows(Team{Server: "https://team", Direct: true},
		func(eco string) bool { return eco == "npm" })
	if len(rows) != 2 {
		t.Fatalf("filtering left %d rows, want npm's two", len(rows))
	}
	for _, row := range rows {
		if row.Eco != "npm" {
			t.Fatalf("a filtered ecosystem produced a row: %+v", row)
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
