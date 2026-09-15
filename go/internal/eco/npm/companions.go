package npm

import "strings"

// companions keeps a tarball servable in the project it is copied to. npm reaches a
// tarball through its packument — the tarball handler reads it to find the file's origin —
// so a project holding the file without the packument cannot answer the request that
// leads to it, and offline cannot answer at all.
func companions(key string, _ func() ([]byte, error)) ([]string, error) {
	name, _, found := strings.Cut(key, "/-/")
	if !found || name == "" {
		return nil, nil
	}
	return []string{"packument/" + name}, nil
}
