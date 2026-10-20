package local

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/control/credential"
)

// A team project that serves downloads only to a token.
//
// pkgreg can require a read token for a project's data plane (data_plane_auth: token), and
// a machine pointed at such a project without one gets a 401 for every package. The token
// is part of the team record rather than a setting beside it, because it belongs to exactly
// one project on exactly one cache: it is written with the record, removed with it, and
// never carried to a record that names anything else.
//
// From the record it reaches every request this machine makes to that cache over TLS: the
// -no-cache bridge's header, a sealed credential on each team chain row, and the data-plane
// hop the relays take. It never rides the team's apt proxy, which is cleartext by protocol;
// see Relays.

// TokenFileEnvVar names a token file for `pkgcache setup` when -token-file is not given,
// for CI jobs that already hold the path in a secret variable.
const TokenFileEnvVar = "PKGCACHE_TOKEN_FILE"

// maxTokenFile bounds what is read from a token file. A token is a line; anything near this
// size is the wrong file, and reading all of it into a record that is rewritten on every
// setup would be the wrong thing to do with it.
const maxTokenFile = 16 << 10

// ReadTokenFile reads a project token from a file, the way pkgreg-client always did.
//
// A file rather than a flag value, so the secret never appears in a process listing or a
// shell history. Surrounding whitespace is dropped — an editor's trailing newline is not
// part of the token — and anything that is not exactly one token is refused rather than
// stored, because a wrong token is a 401 on every request long after the command that
// accepted it has been forgotten.
func ReadTokenFile(path string) (string, error) {
	file, err := os.Open(path) // #nosec G304 -- the path is the one the user named.
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, maxTokenFile+1))
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	if len(body) > maxTokenFile {
		return "", fmt.Errorf("token file %s is too large to be a token", path)
	}
	token := strings.TrimSpace(string(body))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	if strings.ContainsAny(token, "\r\n") {
		return "", errors.New("the token file must contain exactly one token")
	}
	return token, nil
}

// KeepToken returns next carrying the token the project's own record already holds, when
// next brings none and names the same cache and the same project on it.
//
// Re-running setup to change -no-direct, or pressing Update in the console, must not cost a
// working token: neither asks for it again. A record naming another server or another far
// project starts without one, because a token presented anywhere else is a credential
// handed to somebody it was not issued to.
func (s TeamSet) KeepToken(project string, next Team) Team {
	if next.Token != "" {
		return next
	}
	current, own := s.Own(project)
	if !own || current.Token == "" {
		return next
	}
	if strings.TrimRight(current.Server, "/") != strings.TrimRight(next.Server, "/") ||
		farProjectOf(current) != farProjectOf(next) {
		return next
	}
	next.Token = current.Token
	return next
}

// farProjectOf is the team project a record names, with the empty name spelled as the
// global project it means.
func farProjectOf(team Team) string {
	if team.Project == "" {
		return config.GlobalProject
	}
	return team.Project
}

// credential is the token as a chain row presents it: a bearer header, which is the form
// pkgreg's data plane reads. Nil for a team whose project takes anonymous pulls.
func (t Team) credential() *credential.Plain {
	if t.Token == "" {
		return nil
	}
	return &credential.Plain{
		Label: "team " + strings.TrimRight(t.Server, "/"), Kind: "bearer", Token: t.Token,
	}
}
