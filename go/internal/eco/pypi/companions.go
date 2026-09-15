package pypi

import "strings"

// companions keeps a distribution file servable in the project it is copied to. pip
// finds a file through its project's simple page, which this adapter also reads to serve
// the file itself, and uv asks for the PEP 658 metadata beside it before it downloads.
func companions(key string, _ func() ([]byte, error)) ([]string, error) {
	index, rest, found := strings.Cut(key, "/+f/")
	if !found || index == "" {
		return nil, nil
	}
	project, filename, found := strings.Cut(rest, "/")
	if !found || project == "" || filename == "" {
		return nil, nil
	}
	keys := []string{"simple/" + index + "/" + project}
	if !strings.HasSuffix(filename, ".metadata") {
		keys = append(keys, key+".metadata")
	}
	return keys, nil
}
