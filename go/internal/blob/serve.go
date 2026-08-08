package blob

import (
	"net/http"
	"time"
)

// Serve writes a blob to an HTTP response.
//
// This delegates to http.ServeContent, which handles Range, If-Range, If-None-Match,
// If-Modified-Since and multipart range requests, and lets the kernel sendfile the
// body. Getting Range right is not incidental here: the defect that motivated
// replacing devpi was that it had no Range support, so a 2.5 GB CUDA wheel was
// re-downloaded in full on every install.
//
// Blobs are immutable, so the digest is a perfect strong ETag and the content may be
// cached by anything downstream forever.
func Serve(w http.ResponseWriter, r *http.Request, s *Store, d Digest, mediaType string) error {
	f, st, err := s.Open(d)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	h := w.Header()
	if mediaType != "" {
		h.Set("Content-Type", mediaType)
	}
	h.Set("ETag", `"`+string(d)+`"`)
	h.Set("Accept-Ranges", "bytes")
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	}

	// The name is only used for content-type sniffing, which we have already set,
	// and the modtime drives If-Modified-Since. Both are deliberately explicit.
	modTime := st.ModTime
	if modTime.IsZero() {
		modTime = time.Time{}
	}
	http.ServeContent(w, r, "", modTime, f)
	return nil
}
