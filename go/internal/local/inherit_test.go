//go:build !windows

package local

import (
	"context"
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control"
)

// The bug this closes: a CUDA channel or a private wheelhouse added to the global project
// existed only there, so a project created afterwards rewrote nothing for it and its
// builds pulled gigabytes from the internet — while the same Dockerfile on global hit the
// cache every time, which is what made it look like the feature worked.
func TestANewProjectInheritsTheGlobalProjectsIndexes(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	sources, instance := newSources(t)

	wheelhouse := control.Upstream{
		Eco: "pypi", Name: "pytorch-cu130", Kind: "origin", Priority: 20, Enabled: true,
		URL: "https://download.pytorch.org/whl/cu130",
	}
	if _, err := instance.Projects.AddUpstream(config.GlobalProject, wheelhouse); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.Projects.Create("gpu", ""); err != nil {
		t.Fatal(err)
	}
	if err := sources.Adopt(context.Background(), "gpu"); err != nil {
		t.Fatal(err)
	}

	rows, err := instance.Projects.Upstreams("gpu")
	if err != nil {
		t.Fatal(err)
	}
	var found *control.Upstream
	for index, row := range rows {
		if row.Eco == "pypi" && row.Name == "pytorch-cu130" {
			found = &rows[index]
		}
	}
	if found == nil {
		t.Fatalf("the new project did not inherit the index: %+v", rows)
	}
	if found.URL != wheelhouse.URL {
		t.Fatalf("inherited row points at %s", found.URL)
	}
	if found.Project != "gpu" {
		t.Fatalf("inherited row belongs to %q", found.Project)
	}
}

// Filling gaps, not overwriting. A project configured before it was adopted keeps what it
// was given.
func TestInheritingDoesNotOverwriteWhatIsAlreadyThere(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	sources, instance := newSources(t)

	if _, err := instance.Projects.AddUpstream(config.GlobalProject, control.Upstream{
		Eco: "pypi", Name: "house", Kind: "origin", Priority: 20, Enabled: true,
		URL: "https://wheels.example.com/simple",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.Projects.Create("gpu", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.Projects.AddUpstream("gpu", control.Upstream{
		Eco: "pypi", Name: "house", Kind: "origin", Priority: 20, Enabled: true,
		URL: "https://mirror.internal/simple",
	}); err != nil {
		t.Fatal(err)
	}
	if err := sources.Adopt(context.Background(), "gpu"); err != nil {
		t.Fatal(err)
	}

	rows, err := instance.Projects.Upstreams("gpu")
	if err != nil {
		t.Fatal(err)
	}
	houses := 0
	for _, row := range rows {
		if row.Name != "house" {
			continue
		}
		houses++
		if row.URL != "https://mirror.internal/simple" {
			t.Fatalf("the project's own row was overwritten with %s", row.URL)
		}
	}
	if houses != 1 {
		t.Fatalf("adopting produced %d rows named house, want 1", houses)
	}
}

// The global project is the template, so it has nothing to inherit from and adopting it
// must not copy its own rows onto itself.
func TestGlobalInheritsNothing(t *testing.T) {
	t.Setenv(ProjectEnvVar, "")
	sources, instance := newSources(t)

	if _, err := instance.Projects.AddUpstream(config.GlobalProject, control.Upstream{
		Eco: "pypi", Name: "house", Kind: "origin", Priority: 20, Enabled: true,
		URL: "https://wheels.example.com/simple",
	}); err != nil {
		t.Fatal(err)
	}
	if err := sources.Adopt(context.Background(), config.GlobalProject); err != nil {
		t.Fatal(err)
	}
	rows, err := instance.Projects.Upstreams(config.GlobalProject)
	if err != nil {
		t.Fatal(err)
	}
	houses := 0
	for _, row := range rows {
		if row.Name == "house" {
			houses++
		}
	}
	if houses != 1 {
		t.Fatalf("global holds %d copies of its own row", houses)
	}
}
