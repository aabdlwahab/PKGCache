// Package peer implements the digest-first sibling cache protocol.
package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/sync/singleflight"

	"github.com/aabdlwahab/PKGCache/internal/blob"
	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/upstream"
)

type tokenVerifier interface {
	VerifyToken(project, eco, scope, presented string) bool
}

// Service is both the inbound peer handler and outbound fetcher.
type Service struct {
	blobs  *blob.Store
	config *config.Store
	pool   *upstream.Pool
	tokens tokenVerifier
	group  singleflight.Group
}

// New builds the peer federation service: the client half that fetches a digest from
// a sibling instance, and the server half that answers such fetches.
func New(
	blobs *blob.Store, cfg *config.Store, pool *upstream.Pool, tokens tokenVerifier,
) *Service {
	return &Service{blobs: blobs, config: cfg, pool: pool, tokens: tokens}
}

// ServeHTTP serves /peer/v1/blob/{sha256} and /peer/v1/have.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="pkgreg-peer"`)
		http.Error(w, "peer token required", http.StatusUnauthorized)
		return
	}
	path := r.URL.EscapedPath()
	if path == "/peer/v1/have" {
		s.have(w, r)
		return
	}
	const prefix = "/peer/v1/blob/"
	if !strings.HasPrefix(path, prefix) {
		http.NotFound(w, r)
		return
	}
	digest, err := blob.ParseDigest(strings.TrimPrefix(path, prefix))
	if err != nil {
		http.Error(w, "invalid sha256", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodHead, http.MethodGet:
		if !s.blobs.Exists(digest) {
			http.NotFound(w, r)
			return
		}
		if err := blob.Serve(w, r, s.blobs, digest, "application/octet-stream"); err != nil {
			http.Error(w, "blob unavailable", http.StatusNotFound)
		}
	default:
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Service) have(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	have := make([]blob.Digest, 0, len(r.URL.Query()["d"]))
	for _, raw := range r.URL.Query()["d"] {
		digest, err := blob.ParseDigest(raw)
		if err == nil && s.blobs.Exists(digest) {
			have = append(have, digest)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"have": have})
}

func (s *Service) authorize(r *http.Request) bool {
	if s.tokens == nil {
		return false
	}
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(value) <= 7 || !strings.EqualFold(value[:7], "bearer ") {
		return false
	}
	return s.tokens.VerifyToken(config.GlobalProject, "", "peer", strings.TrimSpace(value[7:]))
}

// Fetch asks configured peers for digest and commits verified bytes into the CAS.
func (s *Service) Fetch(
	ctx context.Context, project, eco string, digest blob.Digest,
) (found bool, size int64, err error) {
	if !digest.Valid() {
		return false, 0, nil
	}
	if stat, ok := s.blobs.Stat(digest); ok {
		return true, stat.Size, nil
	}
	value, err, _ := s.group.Do(string(digest), func() (any, error) {
		if stat, ok := s.blobs.Stat(digest); ok {
			return stat.Size, nil
		}
		detached := context.WithoutCancel(ctx)
		for _, candidate := range s.config.Current().ProjectPeers[project][eco] {
			found, size, err := s.fetchOne(detached, candidate, eco, digest)
			if err != nil {
				continue
			}
			if found {
				return size, nil
			}
		}
		return int64(-1), nil
	})
	if err != nil {
		return false, 0, err
	}
	size = value.(int64)
	return size >= 0, size, nil
}

func (s *Service) fetchOne(
	ctx context.Context, candidate config.Peer, eco string, digest blob.Digest,
) (found bool, size int64, err error) {
	base := strings.TrimRight(candidate.URL, "/")
	request := upstream.Request{
		URL: base + "/peer/v1/blob/" + digest.String(), Eco: eco,
		Credential: peerCredential(candidate.Credential),
	}
	head := request
	head.Method = http.MethodHead
	response, cancel, err := s.pool.Open(ctx, head)
	if err != nil {
		return false, 0, err
	}
	_ = response.Body.Close()
	cancel()
	if response.StatusCode == http.StatusNotFound {
		return false, 0, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, 0, fmt.Errorf("peer: HEAD returned %s", response.Status)
	}

	response, cancel, err = s.pool.Open(ctx, request)
	if err != nil {
		return false, 0, err
	}
	defer cancel()
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		return false, 0, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, 0, fmt.Errorf("peer: GET returned %s", response.Status)
	}
	writer, err := s.blobs.Create()
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = writer.Abort() }()
	if _, err := io.Copy(writer, response.Body); err != nil {
		return false, 0, fmt.Errorf("peer: copy: %w", err)
	}
	if writer.Digest() != digest {
		return false, 0, errors.New("peer: digest mismatch")
	}
	committed, size, err := writer.Commit()
	if err != nil {
		return false, 0, err
	}
	if committed != digest {
		return false, 0, errors.New("peer: committed digest mismatch")
	}
	s.pool.CountBytes(eco, candidate.URL, size)
	return true, size, nil
}

func peerCredential(value config.UpstreamCredential) *upstream.Credential {
	if value.Kind == "" && value.Token == "" && value.Username == "" {
		return nil
	}
	kind := value.Kind
	if kind == "" && value.Token != "" {
		kind = "bearer"
	}
	return &upstream.Credential{
		Kind: kind, Username: value.Username, Password: value.Password, Token: value.Token,
	}
}
