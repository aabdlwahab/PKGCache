package clientbuild

import "testing"

// An image is pushed under its own name with the registry it came from taken off, and one
// that already carries local/ keeps it once.
func TestPushNameIsTheImagesOwn(t *testing.T) {
	for _, tc := range []struct{ image, name, repo, tag string }{
		{"mold", "", "mold", "latest"},
		{"mold:latest", "", "mold", "latest"},
		{"myteam/app:1.2", "", "myteam/app", "1.2"},
		{"ghcr.io/org/tool:3", "", "org/tool", "3"},
		{"localhost:5000/app:dev", "", "app", "dev"},
		{"127.0.0.1:41780/local/mold:latest", "", "mold", "latest"},
		{"local/mold:latest", "", "mold", "latest"},
		{"anything:1", "renamed/thing:2", "renamed/thing", "2"},
		{"0123456789abcdef", "built:1", "built", "1"},
	} {
		repo, tag, err := PushName(tc.image, tc.name)
		if err != nil || repo != tc.repo || tag != tc.tag {
			t.Errorf("PushName(%q, %q) = %q %q %v, want %q %q", tc.image, tc.name, repo, tag, err, tc.repo, tc.tag)
		}
	}
	for _, image := range []string{"app@sha256:0123", "0123456789abcdef", "Upper/case:1"} {
		if _, _, err := PushName(image, ""); err == nil {
			t.Errorf("PushName(%q) was accepted", image)
		}
	}
}
