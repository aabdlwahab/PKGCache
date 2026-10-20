package local

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/ociname"
	"github.com/aabdlwahab/PKGCache/internal/router"
	"github.com/aabdlwahab/PKGCache/internal/trust"
)

// Sending an image pushed to this machine's cache on to the team cache it is pointed at.
//
// Docker pushes to this cache over loopback, which needs no certificate and no login. This
// then moves the image the rest of the way itself, with the machine's own team token and
// the CA setup pinned. Docker pushing to the team directly would mean installing that CA in
// Docker's trust store and logging Docker in on every machine — and a registry that answers
// anonymous pulls cannot ask for a login without breaking them (see eco/oci/push.go).
//
// It is a registry-to-registry copy through each side's ordinary API: a layer the team
// already holds is not sent, and an index travels after every manifest it names, the order
// a registry that checks references needs.

// manifestAccept is every manifest form a push may have produced.
const manifestAccept = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

// ErrNoTeamToken means the team record holds no token to push with.
var ErrNoTeamToken = errors.New("no team token")

// TeamPush is one image, already pushed to this machine's cache, to send on to the team.
type TeamPush struct {
	// Source is this cache's address, host:port, which the image is read back from.
	Source string
	// Project is the project on this cache the image was pushed to.
	Project string
	// Repository and Tag name it under local/.
	Repository, Tag string
	// Team is the team record for Project, token included.
	Team Team
	// Client overrides the client the team is reached with; nil builds one from the team's
	// pinned CA. Tests use it.
	Client *http.Client
	// Notes receives a line per layer sent. Nil discards them.
	Notes io.Writer
}

// TeamPushResult says what was sent.
type TeamPushResult struct {
	// Target is the image as the team names it.
	Target string
	// Digest is the manifest's, as the team now serves it.
	Digest string
	// Layers counts the blobs the image names; Sent, the ones the team did not have yet.
	Layers, Sent int
	// Bytes is what was sent.
	Bytes int64
}

// CheckTeamPush reports why a team record cannot take a push, or nil when it can be tried.
// Whether the token's scope is write only the team can say, and does when the push starts.
func CheckTeamPush(team Team) error {
	if strings.TrimSpace(team.Server) == "" {
		return errors.New("this cache is not pointed at a team cache; run pkgcache setup -server first")
	}
	if team.NoCache {
		return errors.New("this machine is set up with -no-cache, so it holds no image to send on; " +
			"push to the team directly instead")
	}
	if team.Token == "" {
		return fmt.Errorf("%w: pushing to %s needs a write token for project %s.\n"+
			"  Create one in its console (Connect, Access tokens, scope write) and run:\n"+
			"  pkgcache setup -server %s -ca-sha256 %s -team-project %s -token-file -",
			ErrNoTeamToken, team.Server, farProjectOf(team),
			team.Server, team.Fingerprint, farProjectOf(team))
	}
	return nil
}

// PushToTeam copies an image from this machine's cache to the team cache.
func PushToTeam(ctx context.Context, p TeamPush) (TeamPushResult, error) {
	if err := CheckTeamPush(p.Team); err != nil {
		return TeamPushResult{}, err
	}
	client := p.Client
	if client == nil {
		var err error
		if client, err = uploadClient([]byte(p.Team.CAPEM)); err != nil {
			return TeamPushResult{}, err
		}
	}
	repo := ociname.Hosted + "/" + p.Repository
	src := registry{
		base:   "http://" + p.Source + "/v2/" + router.OCIProjectPrefix(p.Project) + repo,
		client: &http.Client{Timeout: 0},
	}
	host := strings.TrimRight(p.Team.Server, "/")
	dst := registry{
		base:   host + "/v2/" + router.OCIProjectPrefix(farProjectOf(p.Team)) + repo,
		client: client, token: p.Team.Token,
	}
	authority := strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	result := TeamPushResult{
		Target: authority + "/" + router.OCIProjectPrefix(farProjectOf(p.Team)) + repo + ":" + p.Tag,
	}
	copier := &imageCopier{ctx: ctx, src: src, dst: dst, team: p.Team, notes: p.Notes, result: &result}

	body, mediaType, err := src.manifest(ctx, p.Tag)
	if err != nil {
		return result, fmt.Errorf("read %s:%s back from this cache: %w", repo, p.Tag, err)
	}
	children, err := indexChildren(body)
	if err != nil {
		return result, err
	}
	for _, child := range children {
		childBody, childType, err := src.manifest(ctx, child)
		if err != nil {
			return result, fmt.Errorf("read %s from this cache: %w", child, err)
		}
		if err := copier.image(childBody); err != nil {
			return result, err
		}
		if _, err := dst.putManifest(ctx, child, childBody, childType); err != nil {
			return result, teamRefusal(p.Team, err)
		}
	}
	if len(children) == 0 {
		if err := copier.image(body); err != nil {
			return result, err
		}
	}
	digest, err := dst.putManifest(ctx, p.Tag, body, mediaType)
	if err != nil {
		return result, teamRefusal(p.Team, err)
	}
	result.Digest = digest
	return result, nil
}

type imageCopier struct {
	ctx    context.Context
	src    registry
	dst    registry
	team   Team
	notes  io.Writer
	result *TeamPushResult
}

// image sends every blob one image manifest names that the team does not have yet.
func (c *imageCopier) image(manifest []byte) error {
	digests, err := imageBlobs(manifest)
	if err != nil {
		return err
	}
	for _, digest := range digests {
		c.result.Layers++
		held, err := c.dst.hasBlob(c.ctx, digest)
		if err != nil {
			return teamRefusal(c.team, err)
		}
		if held {
			continue
		}
		size, err := c.dst.sendBlob(c.ctx, c.src, digest)
		if err != nil {
			return teamRefusal(c.team, err)
		}
		c.result.Sent++
		c.result.Bytes += size
		if c.notes != nil {
			_, _ = fmt.Fprintf(c.notes, "pkgcache: sent %s (%s) to the team\n", shortDigest(digest), humanBytes(size))
		}
	}
	return nil
}

// registry is one side of the copy: a repository's API root and how to reach it.
type registry struct {
	base   string
	client *http.Client
	token  string
}

func (r registry) do(ctx context.Context, method, target string, body io.Reader, size int64, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	for name, values := range header {
		req.Header[name] = values
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	return r.client.Do(req)
}

func (r registry) manifest(ctx context.Context, ref string) (body []byte, mediaType string, err error) {
	resp, err := r.do(ctx, http.MethodGet, r.base+"/manifests/"+url.PathEscape(ref), nil, 0,
		http.Header{"Accept": {manifestAccept}})
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", registryError(resp)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return body, resp.Header.Get("Content-Type"), err
}

func (r registry) hasBlob(ctx context.Context, digest string) (bool, error) {
	resp, err := r.do(ctx, http.MethodHead, r.base+"/blobs/"+digest, nil, 0, nil)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, registryError(resp)
}

// sendBlob streams one blob from src into this registry: an upload opened with a POST and
// closed with a single PUT that carries the whole layer and names its digest.
func (r registry) sendBlob(ctx context.Context, src registry, digest string) (int64, error) {
	layer, err := src.do(ctx, http.MethodGet, src.base+"/blobs/"+digest, nil, 0, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = layer.Body.Close() }()
	if layer.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("read %s from this cache: %w", shortDigest(digest), registryError(layer))
	}
	opened, err := r.do(ctx, http.MethodPost, r.base+"/blobs/uploads/", nil, 0, nil)
	if err != nil {
		return 0, err
	}
	_ = opened.Body.Close()
	if opened.StatusCode != http.StatusAccepted {
		return 0, registryError(opened)
	}
	target, err := uploadTarget(r.base, opened.Header.Get("Location"), digest)
	if err != nil {
		return 0, err
	}
	closed, err := r.do(ctx, http.MethodPut, target, layer.Body, layer.ContentLength,
		http.Header{"Content-Type": {"application/octet-stream"}})
	if err != nil {
		return 0, err
	}
	_ = closed.Body.Close()
	if closed.StatusCode != http.StatusCreated {
		return 0, registryError(closed)
	}
	return layer.ContentLength, nil
}

func (r registry) putManifest(ctx context.Context, ref string, body []byte, mediaType string) (string, error) {
	if mediaType == "" {
		mediaType = "application/vnd.oci.image.manifest.v1+json"
	}
	resp, err := r.do(ctx, http.MethodPut, r.base+"/manifests/"+url.PathEscape(ref),
		strings.NewReader(string(body)), int64(len(body)), http.Header{"Content-Type": {mediaType}})
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", registryError(resp)
	}
	return resp.Header.Get("Docker-Content-Digest"), nil
}

// uploadTarget is where an upload's closing PUT goes: the Location the registry answered
// with — a path, or a whole URL — and the digest that closes it.
func uploadTarget(base, location, digest string) (string, error) {
	if location == "" {
		return "", errors.New("the team opened an upload without saying where it is")
	}
	root, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	next, err := root.Parse(location)
	if err != nil {
		return "", fmt.Errorf("the team answered with an unusable upload location %q", location)
	}
	query := next.Query()
	query.Set("digest", digest)
	next.RawQuery = query.Encode()
	return next.String(), nil
}

// registryHTTPError is a registry's refusal, with its own words when it gave any.
type registryHTTPError struct {
	Status  int
	Code    string
	Message string
}

func (e *registryHTTPError) Error() string {
	text := http.StatusText(e.Status)
	if e.Code != "" {
		text = e.Code
	}
	if e.Message != "" {
		return fmt.Sprintf("%d %s: %s", e.Status, text, e.Message)
	}
	return fmt.Sprintf("%d %s", e.Status, text)
}

func registryError(resp *http.Response) error {
	var doc struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	failure := &registryHTTPError{Status: resp.StatusCode}
	if raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10)); err == nil &&
		json.Unmarshal(raw, &doc) == nil && len(doc.Errors) > 0 {
		failure.Code, failure.Message = doc.Errors[0].Code, doc.Errors[0].Message
	}
	return failure
}

// teamRefusal explains a refusal that is about the token rather than the image, which is
// the one a person can act on without reading a registry's error codes.
func teamRefusal(team Team, err error) error {
	var refused *registryHTTPError
	if !errors.As(err, &refused) ||
		(refused.Status != http.StatusUnauthorized && refused.Status != http.StatusForbidden) {
		return fmt.Errorf("the team refused the push: %w", err)
	}
	project := "project " + farProjectOf(team) + " on " + team.Server
	return fmt.Errorf("the team refused the push: %w.\n"+
		"  The token this machine holds cannot push to %s: it needs scope write.\n"+
		"  Create one in the team console (Connect, Access tokens) and give it to pkgcache setup -token-file -",
		err, project)
}

// indexChildren is every manifest an index names. An image manifest names none.
func indexChildren(body []byte) ([]string, error) {
	var doc struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("the pushed manifest is not JSON: %w", err)
	}
	out := make([]string, 0, len(doc.Manifests))
	for _, manifest := range doc.Manifests {
		out = append(out, manifest.Digest)
	}
	return out, nil
}

// imageBlobs is every blob an image manifest names: its config and its layers.
func imageBlobs(body []byte) ([]string, error) {
	var doc struct {
		Config *struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("the pushed manifest is not JSON: %w", err)
	}
	var out []string
	if doc.Config != nil && doc.Config.Digest != "" {
		out = append(out, doc.Config.Digest)
	}
	for _, layer := range doc.Layers {
		out = append(out, layer.Digest)
	}
	return out, nil
}

// uploadClient reaches the team over the CA setup pinned, with no deadline on the whole
// request: a layer can be gigabytes, and the 30-second client trust.ClientFor builds for
// setup's small requests would cut one off partway. Only the wait for an answer once a
// request is sent is bounded.
func uploadClient(caPEM []byte) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 2 * time.Minute
	if len(caPEM) > 0 {
		roots, err := trust.Pool(caPEM)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	}
	return &http.Client{Transport: transport}, nil
}

func shortDigest(digest string) string {
	if len(digest) > 19 {
		return digest[:19]
	}
	return digest
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, suffix := float64(n), "KMGT"
	i := -1
	for value >= unit && i < len(suffix)-1 {
		value /= unit
		i++
	}
	return fmt.Sprintf("%.1f %ciB", value, suffix[i])
}
