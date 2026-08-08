// Package differential replays one ordered request/command corpus against the
// Python and Go deployments and compares the externally observable contract.
package differential

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Corpus is deliberately data-driven so production-specific artifact digests and
// filenames do not get compiled into the harness.
type Corpus struct {
	Required      []string        `json:"required"`
	Normalize     []Normalization `json:"normalize,omitempty"`
	Requests      []RequestCase   `json:"requests"`
	Commands      []CommandCase   `json:"commands,omitempty"`
	IgnoreHeaders []string        `json:"ignore_headers,omitempty"`
}

// Normalization rewrites a deployment-specific substring — a base URL, a port — to a
// fixed placeholder on each side, so bodies that differ only in where they are hosted
// still compare byte for byte.
type Normalization struct {
	Left        string `json:"left"`
	Right       string `json:"right"`
	Replacement string `json:"replacement"`
}

// RequestCase is one request replayed against both deployments. The comparison is
// byte-exact by default; JSON is compared semantically because key order carries no
// meaning and the two implementations serialize independently.
type RequestCase struct {
	Name         string            `json:"name"`
	Method       string            `json:"method"`
	Path         string            `json:"path,omitempty"`
	Target       string            `json:"target,omitempty"`
	Mode         string            `json:"mode,omitempty"` // origin (default) | proxy
	Headers      map[string]string `json:"headers,omitempty"`
	LeftHeaders  map[string]string `json:"left_headers,omitempty"`
	RightHeaders map[string]string `json:"right_headers,omitempty"`
	Body         string            `json:"body,omitempty"`
	BodyBase64   string            `json:"body_base64,omitempty"`
	Compare      string            `json:"compare,omitempty"` // bytes (default) | json
	// IgnoreJSONKeys excludes deployment-specific fields from semantic JSON
	// comparison. Keys are removed recursively and must be declared per case.
	IgnoreJSONKeys []string `json:"ignore_json_keys,omitempty"`
	WantStatus     int      `json:"want_status,omitempty"`
}

// CommandCase is an external client invocation — a git clone — run against both sides,
// optionally comparing the resulting working tree.
type CommandCase struct {
	Name        string   `json:"name"`
	Args        []string `json:"args"`
	CompareTree string   `json:"compare_tree,omitempty"`
}

// Endpoints locates the two deployments under comparison. Left is conventionally the
// retired Python stack and Right the Go one.
type Endpoints struct {
	Left, Right string
	LeftSide    SideEndpoints
	RightSide   SideEndpoints
	Vars        map[string]string
}

// SideEndpoints models the retired stack's real topology. Origin carries the
// TLS package protocols and Git, Proxy carries apt/apk absolute-form requests,
// and Admin carries /api. Proxy and Admin default to Origin so a single-port Go
// deployment and older callers only need to set Origin (or Endpoints.Left/Right).
type SideEndpoints struct {
	Origin string
	Proxy  string
	Admin  string
	CAFile string
}

// Result reports how many corpus cases were compared. The run stops at the first
// difference, so a full count means the corpus passed.
type Result struct {
	Cases int
}

type response struct {
	status int
	header http.Header
	body   []byte
}

// LoadCorpus decodes a checked-in or operator-supplied corpus.
func LoadCorpus(path string) (Corpus, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Corpus{}, err
	}
	var corpus Corpus
	if err := json.Unmarshal(body, &corpus); err != nil {
		return Corpus{}, fmt.Errorf("differential: decode corpus: %w", err)
	}
	return corpus, nil
}

// Run stops at the first difference and identifies the exact corpus case.
func Run(ctx context.Context, endpoints Endpoints, corpus Corpus) (Result, error) {
	left := resolvedSide(endpoints.LeftSide, endpoints.Left)
	right := resolvedSide(endpoints.RightSide, endpoints.Right)
	if left.Origin == "" || right.Origin == "" {
		return Result{}, errors.New("differential: both endpoints are required")
	}
	for _, name := range corpus.Required {
		if endpoints.Vars[name] == "" {
			return Result{}, fmt.Errorf("differential: required variable %s is unset", name)
		}
	}
	normalizations := endpointNormalizations(left, right)
	normalizations = append(normalizations, corpus.Normalize...)
	// Framing is transport-specific (the Go server streams and therefore often
	// omits Content-Length). The Go binary also applies the retired console's
	// security policy to every listener, whereas Python applied it only at nginx.
	// Body bytes and protocol headers remain strict.
	ignored := map[string]bool{
		"Date": true, "Server": true, "Content-Length": true, "Connection": true,
		// Validators are opaque by HTTP definition: Python used an MD5-shaped
		// validator while Go uses the content SHA-256. Conditional behaviour is
		// covered explicitly by the corpus. Go's immutable cache directive is an
		// additive client optimization.
		"Etag": true, "Last-Modified": true, "Cache-Control": true, "Accept-Ranges": true,
		"Docker-Distribution-Api-Version": true,
		"Content-Security-Policy":         true, "Permissions-Policy": true,
		"Referrer-Policy": true, "X-Content-Type-Options": true,
		"X-Frame-Options": true,
	}
	for _, name := range corpus.IgnoreHeaders {
		ignored[http.CanonicalHeaderKey(name)] = true
	}

	result := Result{}
	for _, test := range corpus.Requests {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		leftResponse, err := executeRequest(ctx, left, test, endpoints.Vars, true)
		if err != nil {
			return result, fmt.Errorf("%s (Python): %w", test.Name, err)
		}
		rightResponse, err := executeRequest(ctx, right, test, endpoints.Vars, false)
		if err != nil {
			return result, fmt.Errorf("%s (Go): %w", test.Name, err)
		}
		if err := compareResponse(test, leftResponse, rightResponse, ignored, normalizations); err != nil {
			return result, fmt.Errorf("%s: %w", test.Name, err)
		}
		result.Cases++
	}
	for _, test := range corpus.Commands {
		if err := compareCommand(ctx, test, left, right, endpoints.Vars, normalizations); err != nil {
			return result, fmt.Errorf("%s: %w", test.Name, err)
		}
		result.Cases++
	}
	return result, nil
}

func resolvedSide(side SideEndpoints, fallback string) SideEndpoints {
	if side.Origin == "" {
		side.Origin = fallback
	}
	side.Origin = strings.TrimRight(side.Origin, "/")
	if side.Proxy == "" {
		side.Proxy = side.Origin
	}
	if side.Admin == "" {
		side.Admin = side.Origin
	}
	side.Proxy = strings.TrimRight(side.Proxy, "/")
	side.Admin = strings.TrimRight(side.Admin, "/")
	return side
}

func endpointNormalizations(left, right SideEndpoints) []Normalization {
	pairs := []struct {
		left, right, replacement string
	}{
		{left.Origin, right.Origin, "<BASE>"},
		{left.Proxy, right.Proxy, "<PROXY>"},
		{left.Admin, right.Admin, "<ADMIN>"},
	}
	out := make([]Normalization, 0, len(pairs))
	seen := make(map[string]bool)
	for _, pair := range pairs {
		key := pair.left + "\x00" + pair.right
		if pair.left == "" || pair.right == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Normalization{
			Left: pair.left, Right: pair.right, Replacement: pair.replacement,
		})
	}
	return out
}

func executeRequest(
	ctx context.Context,
	side SideEndpoints,
	test RequestCase,
	vars map[string]string,
	isLeft bool,
) (response, error) {
	method := test.Method
	if method == "" {
		method = http.MethodGet
	}
	var body []byte
	var err error
	if test.BodyBase64 != "" {
		body, err = base64.StdEncoding.DecodeString(expand(test.BodyBase64, vars))
		if err != nil {
			return response{}, err
		}
	} else {
		body = []byte(expand(test.Body, vars))
	}
	base := side.Origin
	if strings.HasPrefix(test.Path, "/api/") {
		base = side.Admin
	}
	requestURL := base + expand(test.Path, vars)
	transport, err := transportFor(side.CAFile)
	if err != nil {
		return response{}, err
	}
	if test.Mode == "proxy" {
		requestURL = expand(test.Target, vars)
		proxyURL, err := url.Parse(side.Proxy)
		if err != nil {
			return response{}, err
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL, bytes.NewReader(body))
	if err != nil {
		return response{}, err
	}
	for name, value := range test.Headers {
		request.Header.Set(name, expand(value, vars))
	}
	sideHeaders := test.RightHeaders
	if isLeft {
		sideHeaders = test.LeftHeaders
	}
	for name, value := range sideHeaders {
		request.Header.Set(name, expand(value, vars))
	}
	client := &http.Client{Transport: transport}
	reply, err := client.Do(request)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = reply.Body.Close() }()
	content, err := io.ReadAll(reply.Body)
	if err != nil {
		return response{}, err
	}
	return response{status: reply.StatusCode, header: reply.Header.Clone(), body: content}, nil
}

func transportFor(caFile string) (*http.Transport, error) {
	transport := &http.Transport{DisableCompression: true}
	if caFile == "" {
		return transport, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA %s: %w", caFile, err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("read CA %s: no certificates found", caFile)
	}
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	return transport, nil
}

func compareResponse(
	test RequestCase,
	left, right response,
	ignored map[string]bool,
	normalizations []Normalization,
) error {
	if left.status != right.status {
		return fmt.Errorf("status: Python=%d Go=%d", left.status, right.status)
	}
	if test.WantStatus != 0 && left.status != test.WantStatus {
		return fmt.Errorf("both returned %d, want %d", left.status, test.WantStatus)
	}
	leftHeaders := canonicalHeaders(left.header, ignored, normalizations, true)
	rightHeaders := canonicalHeaders(right.header, ignored, normalizations, false)
	if !reflect.DeepEqual(leftHeaders, rightHeaders) {
		return fmt.Errorf("headers:\nPython %v\nGo     %v", leftHeaders, rightHeaders)
	}
	leftBody := normalize(left.body, normalizations, true)
	rightBody := normalize(right.body, normalizations, false)
	if test.Compare == "json" {
		leftBody, _ = canonicalJSON(leftBody, test.IgnoreJSONKeys)
		rightBody, _ = canonicalJSON(rightBody, test.IgnoreJSONKeys)
	}
	if !bytes.Equal(leftBody, rightBody) {
		return fmt.Errorf("body sha256: Python=%x (%d bytes) Go=%x (%d bytes)",
			sha256.Sum256(leftBody), len(leftBody), sha256.Sum256(rightBody), len(rightBody))
	}
	return nil
}

func canonicalHeaders(
	header http.Header,
	ignored map[string]bool,
	normalizations []Normalization,
	left bool,
) map[string][]string {
	out := make(map[string][]string)
	for name, values := range header {
		name = http.CanonicalHeaderKey(name)
		if ignored[name] {
			continue
		}
		copied := make([]string, len(values))
		for i, value := range values {
			copied[i] = string(normalize([]byte(value), normalizations, left))
		}
		sort.Strings(copied)
		out[name] = copied
	}
	return out
}

func canonicalJSON(body []byte, ignoreKeys []string) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return body, err
	}
	if len(ignoreKeys) != 0 {
		ignored := make(map[string]bool, len(ignoreKeys))
		for _, key := range ignoreKeys {
			ignored[key] = true
		}
		removeJSONKeys(value, ignored)
	}
	return json.Marshal(value)
}

func removeJSONKeys(value any, ignored map[string]bool) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if ignored[key] {
				delete(value, key)
				continue
			}
			removeJSONKeys(child, ignored)
		}
	case []any:
		for _, child := range value {
			removeJSONKeys(child, ignored)
		}
	}
}

func normalize(body []byte, rules []Normalization, left bool) []byte {
	out := append([]byte(nil), body...)
	for _, rule := range rules {
		value := rule.Right
		if left {
			value = rule.Left
		}
		if value != "" {
			out = bytes.ReplaceAll(out, []byte(value), []byte(rule.Replacement))
		}
	}
	return out
}

func compareCommand(
	ctx context.Context,
	test CommandCase,
	left, right SideEndpoints,
	vars map[string]string,
	normalizations []Normalization,
) error {
	if len(test.Args) == 0 {
		return errors.New("empty command")
	}
	leftWork, err := os.MkdirTemp("", "pkgreg-diff-left-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(leftWork) }()
	rightWork, err := os.MkdirTemp("", "pkgreg-diff-right-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(rightWork) }()
	leftOutput, leftErr := runCommand(ctx, test.Args, left, leftWork, vars)
	rightOutput, rightErr := runCommand(ctx, test.Args, right, rightWork, vars)
	if (leftErr == nil) != (rightErr == nil) {
		return fmt.Errorf("exit: Python=%w Go=%w", leftErr, rightErr)
	}
	if leftErr != nil {
		return fmt.Errorf("both commands failed: Python=%w Go=%w", leftErr, rightErr)
	}
	commandNormalizations := append([]Normalization(nil), normalizations...)
	commandNormalizations = append(commandNormalizations, Normalization{
		Left: leftWork, Right: rightWork, Replacement: "<WORK>",
	})
	if !bytes.Equal(
		normalize(leftOutput, commandNormalizations, true),
		normalize(rightOutput, commandNormalizations, false),
	) {
		return fmt.Errorf("command output differs: Python=%q Go=%q", leftOutput, rightOutput)
	}
	if test.CompareTree != "" {
		leftSum, err := treeDigest(filepath.Join(leftWork, test.CompareTree))
		if err != nil {
			return err
		}
		rightSum, err := treeDigest(filepath.Join(rightWork, test.CompareTree))
		if err != nil {
			return err
		}
		if leftSum != rightSum {
			return fmt.Errorf("checkout tree: Python=%s Go=%s", leftSum, rightSum)
		}
	}
	return nil
}

func runCommand(
	ctx context.Context,
	arguments []string,
	side SideEndpoints,
	work string,
	vars map[string]string,
) ([]byte, error) {
	local := make(map[string]string, len(vars)+2)
	for key, value := range vars {
		local[key] = value
	}
	local["BASE"], local["WORK"] = side.Origin, work
	expanded := make([]string, len(arguments))
	for i, argument := range arguments {
		expanded[i] = expand(argument, local)
	}
	command := exec.CommandContext(ctx, expanded[0], expanded[1:]...)
	command.Dir = work
	command.Env = os.Environ()
	if side.CAFile != "" {
		command.Env = append(command.Env, "GIT_SSL_CAINFO="+side.CAFile)
	}
	return command.CombinedOutput()
}

func treeDigest(root string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, name)
		if err != nil || relative == "." {
			return err
		}
		// A clone's .git directory contains machine-local remote URLs, reflogs,
		// indexes, and pack layout. Clone success plus the separately compared
		// info/refs response proves protocol behavior; this digest compares the
		// checked-out repository tree.
		base := filepath.Base(relative)
		if entry.IsDir() && base == ".git" {
			return fs.SkipDir
		}
		_, _ = io.WriteString(hash, filepath.ToSlash(relative))
		_, _ = io.WriteString(hash, entry.Type().String())
		if !entry.IsDir() {
			var content []byte
			if entry.Type()&os.ModeSymlink != 0 {
				target, err := os.Readlink(name)
				if err != nil {
					return err
				}
				content = []byte(target)
			} else {
				content, err = os.ReadFile(name)
			}
			if err != nil {
				return err
			}
			_, _ = hash.Write(content)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func expand(value string, vars map[string]string) string {
	for name, replacement := range vars {
		value = strings.ReplaceAll(value, "{{"+name+"}}", replacement)
	}
	return value
}

// ParseVars accepts repeated KEY=VALUE command-line arguments.
func ParseVars(values []string) (map[string]string, error) {
	out := make(map[string]string)
	for _, value := range values {
		key, replacement, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid variable %q (want KEY=VALUE)", value)
		}
		out[key] = replacement
	}
	return out, nil
}

// DiffSummary gives stable output for CI logs.
func DiffSummary(result Result) string {
	return "differential corpus clean: " + strconv.Itoa(result.Cases) + " cases"
}
