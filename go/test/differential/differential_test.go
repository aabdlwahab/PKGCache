package differential

import (
	"context"
	"encoding/pem"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var (
	pythonBase  = flag.String("python", "", "Python package/Git origin URL")
	pythonProxy = flag.String("python-proxy", "", "Python apt/apk proxy URL (default -python)")
	pythonAdmin = flag.String("python-admin", "", "Python control API URL (default -python)")
	pythonCA    = flag.String("python-ca", "", "PEM CA trusted by Python-side HTTPS and Git")
	goBase      = flag.String("go", "", "Go package/Git origin URL")
	goProxy     = flag.String("go-proxy", "", "Go apt/apk proxy URL (default -go)")
	goAdmin     = flag.String("go-admin", "", "Go control API URL (default -go)")
	goCA        = flag.String("go-ca", "", "PEM CA trusted by Go-side HTTPS and Git")
	corpusPath  = flag.String("corpus", "corpus.json", "request corpus JSON")
	corpusVars  multiFlag
)

func init() {
	flag.Var(&corpusVars, "var", "corpus variable KEY=VALUE (repeatable)")
}

type multiFlag []string

func (f *multiFlag) String() string { return strings.Join(*f, ",") }
func (f *multiFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func TestCheckedInCorpusLoads(t *testing.T) {
	corpus, err := LoadCorpus("corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(corpus.Requests) < 40 || len(corpus.Commands) != 3 {
		t.Fatalf("corpus is unexpectedly incomplete: %d requests, %d commands",
			len(corpus.Requests), len(corpus.Commands))
	}
}

func TestProductionCorpus(t *testing.T) {
	if *pythonBase == "" || *goBase == "" {
		t.Skip("opt in with -python and -go; see test/differential/README.md")
	}
	path := *corpusPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(".", path)
	}
	corpus, err := LoadCorpus(path)
	if err != nil {
		t.Fatal(err)
	}
	vars, err := ParseVars(corpusVars)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), Endpoints{
		LeftSide: SideEndpoints{
			Origin: *pythonBase, Proxy: *pythonProxy, Admin: *pythonAdmin, CAFile: *pythonCA,
		},
		RightSide: SideEndpoints{
			Origin: *goBase, Proxy: *goProxy, Admin: *goAdmin, CAFile: *goCA,
		},
		Vars: vars,
	}, corpus)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(DiffSummary(result))
}

func TestRunnerNormalizesBaseAndJSON(t *testing.T) {
	handler := func(label string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Contract", "same")
			w.Header().Set("Server", label)
			_, _ = fmt.Fprintf(w, `{"url":"%s/item","nested":{"b":2,"a":1}}`,
				"http://"+r.Host)
		})
	}
	left := httptest.NewServer(handler("python"))
	defer left.Close()
	right := httptest.NewServer(handler("go"))
	defer right.Close()
	result, err := Run(context.Background(), Endpoints{
		Left: left.URL, Right: right.URL, Vars: map[string]string{},
	}, Corpus{Requests: []RequestCase{{
		Name: "semantic JSON", Method: "GET", Path: "/", Compare: "json",
		WantStatus: http.StatusOK,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Cases != 1 {
		t.Fatalf("cases = %d", result.Cases)
	}
}

func TestRunnerIgnoresDeclaredJSONKeysRecursively(t *testing.T) {
	left := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"projects":[{"name":"global","repo":"/python","ports":{"apt":3142}}]}`))
	}))
	defer left.Close()
	right := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"projects":[{"ports":{"apt":8080},"repo":"/go","name":"global"}]}`))
	}))
	defer right.Close()

	result, err := Run(context.Background(), Endpoints{
		Left: left.URL, Right: right.URL, Vars: map[string]string{},
	}, Corpus{Requests: []RequestCase{{
		Name: "deployment fields", Method: "GET", Path: "/", Compare: "json",
		IgnoreJSONKeys: []string{"repo", "ports"}, WantStatus: http.StatusOK,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Cases != 1 {
		t.Fatalf("cases = %d", result.Cases)
	}
}

func TestRunnerRoutesSplitTopologyAndTrustsCA(t *testing.T) {
	side := func() (SideEndpoints, func()) {
		origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("origin"))
		}))
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("proxy"))
		}))
		admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("admin"))
		}))
		caFile := filepath.Join(t.TempDir(), "ca.pem")
		certificate := pem.EncodeToMemory(&pem.Block{
			Type: "CERTIFICATE", Bytes: origin.Certificate().Raw,
		})
		if err := os.WriteFile(caFile, certificate, 0o600); err != nil {
			t.Fatal(err)
		}
		return SideEndpoints{
				Origin: origin.URL, Proxy: proxy.URL, Admin: admin.URL, CAFile: caFile,
			}, func() {
				origin.Close()
				proxy.Close()
				admin.Close()
			}
	}
	left, closeLeft := side()
	defer closeLeft()
	right, closeRight := side()
	defer closeRight()

	result, err := Run(context.Background(), Endpoints{
		LeftSide: left, RightSide: right, Vars: map[string]string{},
	}, Corpus{Requests: []RequestCase{
		{Name: "origin", Method: "GET", Path: "/", WantStatus: http.StatusOK},
		{Name: "proxy", Method: "GET", Mode: "proxy",
			Target: "http://fixture.invalid/archive", WantStatus: http.StatusOK},
		{Name: "admin", Method: "GET", Path: "/api/projects", WantStatus: http.StatusOK},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Cases != 3 {
		t.Fatalf("cases = %d", result.Cases)
	}
}

func TestTreeDigestComparesCheckoutAndIgnoresGitMetadata(t *testing.T) {
	left := filepath.Join(t.TempDir(), "checkout")
	right := filepath.Join(t.TempDir(), "checkout")
	for _, root := range []string{left, right} {
		if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "README"), []byte("same\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(left, ".git", "config"), []byte("python"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(right, ".git", "config"), []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	leftSum, err := treeDigest(left)
	if err != nil {
		t.Fatal(err)
	}
	rightSum, err := treeDigest(right)
	if err != nil {
		t.Fatal(err)
	}
	if leftSum != rightSum {
		t.Fatal(".git implementation metadata changed the checkout digest")
	}
	if err := os.WriteFile(filepath.Join(right, "README"), []byte("different\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rightSum, err = treeDigest(right)
	if err != nil {
		t.Fatal(err)
	}
	if leftSum == rightSum {
		t.Fatal("working-tree content change did not change the checkout digest")
	}
}
