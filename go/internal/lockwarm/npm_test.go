package lockwarm

import (
	"strings"
	"testing"
)

// sampleNPMLock exercises the shapes that actually turn up in a lock: a plain name, a
// scoped name, a private registry served under a path prefix, a git dependency, and a
// workspace link. Only the first three are things a registry cache can hold.
const sampleNPMLock = `{
  "name": "demo",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {
      "name": "demo",
      "version": "1.0.0"
    },
    "node_modules/is-odd": {
      "version": "0.1.0",
      "resolved": "https://registry.npmjs.org/is-odd/-/is-odd-0.1.0.tgz",
      "integrity": "sha512-AAAA=="
    },
    "node_modules/@babel/core": {
      "version": "7.0.0",
      "resolved": "https://registry.npmjs.org/@babel/core/-/core-7.0.0.tgz",
      "integrity": "sha512-BBBB=="
    },
    "node_modules/private-thing": {
      "version": "2.0.0",
      "resolved": "https://npm.example.internal/artifactory/api/npm/virtual/private-thing/-/private-thing-2.0.0.tgz",
      "integrity": "sha512-CCCC=="
    },
    "node_modules/from-git": {
      "version": "1.0.0",
      "resolved": "git+ssh://git@github.com/owner/repo.git#0123456789abcdef"
    },
    "packages/workspace-pkg": {
      "resolved": "packages/workspace-pkg",
      "link": true
    }
  }
}
`

func TestParseNPMTakesRegistryTarballsOnly(t *testing.T) {
	packages, err := ParseNPM(sampleNPMLock)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]NPMPackage, len(packages))
	for _, pkg := range packages {
		byName[pkg.Name] = pkg
	}
	if len(packages) != 3 {
		t.Fatalf("want 3 registry tarballs, got %d: %+v", len(packages), packages)
	}
	for _, absent := range []string{"from-git", "workspace-pkg", "packages/workspace-pkg"} {
		if _, found := byName[absent]; found {
			t.Errorf("%s is not a registry tarball but was collected", absent)
		}
	}

	// A scoped package keeps its scope in the name and loses it in the filename, which
	// is the reason the filename is read from the URL rather than rebuilt from the name.
	scoped := byName["@babel/core"]
	if scoped.Filename != "core-7.0.0.tgz" {
		t.Errorf("scoped filename = %q, want core-7.0.0.tgz", scoped.Filename)
	}
	if scoped.Registry != "https://registry.npmjs.org" {
		t.Errorf("scoped registry = %q", scoped.Registry)
	}

	// A registry under a path prefix must not have that prefix mistaken for a scope.
	private := byName["private-thing"]
	if want := "https://npm.example.internal/artifactory/api/npm/virtual"; private.Registry != want {
		t.Errorf("private registry = %q, want %q", private.Registry, want)
	}
	if private.Filename != "private-thing-2.0.0.tgz" {
		t.Errorf("private filename = %q", private.Filename)
	}
}

func TestParseNPMRejectsOtherJSON(t *testing.T) {
	// A package.json is JSON, sits next door, and is the likeliest thing to be pointed
	// at by mistake. Warming nothing and reporting success would be the bad outcome.
	if _, err := ParseNPM(`{"name":"demo","version":"1.0.0"}`); err == nil {
		t.Fatal("a package.json must not parse as a lock file")
	}
	if _, err := ParseNPM("not json at all"); err == nil {
		t.Fatal("non-JSON must not parse")
	}
}

func TestParseNPMDedupesAcrossV2Containers(t *testing.T) {
	// A v2 lock lists every tarball twice: once under "packages" and once under the
	// legacy "dependencies". Warming it twice would be waste, not breakage, but the
	// count is what the operator is told, so it has to be the truth.
	const v2 = `{
  "lockfileVersion": 2,
  "packages": {
    "node_modules/is-odd": {
      "resolved": "https://registry.npmjs.org/is-odd/-/is-odd-0.1.0.tgz",
      "integrity": "sha512-AAAA=="
    }
  },
  "dependencies": {
    "is-odd": {
      "resolved": "https://registry.npmjs.org/is-odd/-/is-odd-0.1.0.tgz",
      "integrity": "sha512-AAAA=="
    }
  }
}`
	packages, err := ParseNPM(v2)
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 1 {
		t.Fatalf("want 1 deduped tarball, got %d: %+v", len(packages), packages)
	}
}

func TestRewriteNPMChangesURLsAndNothingElse(t *testing.T) {
	packages, err := ParseNPM(sampleNPMLock)
	if err != nil {
		t.Fatal(err)
	}
	const base = "http://127.0.0.1:8080/global/npm"
	rewritten := RewriteNPM(sampleNPMLock, packages, base)

	for _, want := range []string{
		`"resolved": "http://127.0.0.1:8080/global/npm/is-odd/-/is-odd-0.1.0.tgz"`,
		`"resolved": "http://127.0.0.1:8080/global/npm/@babel/core/-/core-7.0.0.tgz"`,
		`"resolved": "http://127.0.0.1:8080/global/npm/private-thing/-/private-thing-2.0.0.tgz"`,
	} {
		if !strings.Contains(rewritten, want) {
			t.Errorf("rewritten lock is missing:\n  %s", want)
		}
	}

	// Integrity is what makes the rewrite safe to do at all: npm still verifies every
	// byte, so these must survive untouched.
	for _, hash := range []string{"sha512-AAAA==", "sha512-BBBB==", "sha512-CCCC=="} {
		if !strings.Contains(rewritten, hash) {
			t.Errorf("integrity %s did not survive the rewrite", hash)
		}
	}
	// Sources the cache cannot serve are left pointing where they pointed.
	if !strings.Contains(rewritten, "git+ssh://git@github.com/owner/repo.git#0123456789abcdef") {
		t.Error("a git dependency was rewritten and should not have been")
	}
	if strings.Count(rewritten, "\n") != strings.Count(sampleNPMLock, "\n") {
		t.Error("the rewrite changed the line count, so it reformatted something")
	}

	// Rewriting an already-rewritten lock is a no-op, which is what makes the command
	// safe to run twice.
	again, err := ParseNPM(rewritten)
	if err != nil {
		t.Fatal(err)
	}
	if second := RewriteNPM(rewritten, again, base); second != rewritten {
		t.Error("rewriting twice did not settle")
	}
}

func TestNPMRegistriesReportsEachRootOnce(t *testing.T) {
	packages, err := ParseNPM(sampleNPMLock)
	if err != nil {
		t.Fatal(err)
	}
	roots := NPMRegistries(packages)
	if len(roots) != 2 {
		t.Fatalf("want 2 distinct registries, got %v", roots)
	}
}
