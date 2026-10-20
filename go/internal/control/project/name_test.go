package project

import (
	"testing"

	"github.com/aabdlwahab/PKGCache/internal/eco"
	"github.com/aabdlwahab/PKGCache/internal/eco/oci"
)

// An image path reads its first segment as a project before anything else. A project called
// local would take every pushed image on the instance, so the name is refused the way a
// registry alias is.
func TestPushedImagesNamespaceIsNotAProjectName(t *testing.T) {
	ecos := eco.NewRegistry()
	ecos.Register(oci.New())
	s := &Service{ecos: ecos}
	for _, name := range []string{oci.HostedNamespace, "dockerhub"} {
		if err := s.validateName(name); err == nil {
			t.Errorf("project %q was allowed", name)
		}
	}
	if err := s.validateName("work"); err != nil {
		t.Fatalf("an ordinary name was refused: %v", err)
	}
}
