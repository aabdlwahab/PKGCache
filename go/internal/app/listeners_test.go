package app

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/pki"
)

func TestSinglePortServesTLSPlainProxyAndReloadsCertificate(t *testing.T) {
	a, ca := appWithTLS(t, true)
	runtime, err := a.StartListeners()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = runtime.Shutdown(ctx)
	})
	address := runtime.Addresses()["single"]

	roots := x509.NewCertPool()
	caPEM, err := os.ReadFile(filepath.Join(a.Config.Current().CertsDir(), pki.CACertFile))
	if err != nil || !roots.AppendCertsFromPEM(caPEM) {
		t.Fatalf("load test CA: %v", err)
	}
	clientTLS := &tls.Config{
		RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12,
	}
	transport := &http.Transport{TLSClientConfig: clientTLS.Clone()}
	defer transport.CloseIdleConnections()
	httpsClient := &http.Client{Transport: transport}
	for _, path := range []string{"/", "/healthz", "/readyz", "/v2/"} {
		response := getResponse(t, httpsClient, "https://"+address+path)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("HTTPS %s = %d %q", path, response.StatusCode, body)
		}
	}

	// Origin-form plain HTTP no longer reaches the admin namespace; liveness is the
	// one carve-out, because probes routinely cannot follow a redirect. See
	// TestSinglePortPlainBranchDoesNotServeTheAdminNamespace for the rest.
	response := getResponse(t, http.DefaultClient, "http://"+address+"/healthz")
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("plain health = %d", response.StatusCode)
	}

	// Absolute-form plain HTTP on that exact socket is the apt/apk proxy.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("single-port deb"))
	}))
	defer origin.Close()
	proxyTransport := &http.Transport{Proxy: http.ProxyURL(proxyURL(t, address, "team-a"))}
	defer proxyTransport.CloseIdleConnections()
	proxyClient := &http.Client{Transport: proxyTransport}
	response = getResponse(t, proxyClient, origin.URL+"/pool/pkg_1.0_amd64.deb")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "single-port deb" {
		t.Fatalf("single-port proxy = %d %q", response.StatusCode, body)
	}

	// Keep one TLS connection active across the reload: it must remain usable and
	// retain its negotiated certificate, while a new handshake gets the replacement.
	conn, err := tls.Dial("tcp", address, clientTLS.Clone())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	oldSerial := conn.ConnectionState().PeerCertificates[0].SerialNumber.String()
	httpOnTLSConn(t, conn, reader)

	if _, err := ca.IssueServer([]string{"localhost", "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.ReloadTLS(); err != nil {
		t.Fatal(err)
	}
	httpOnTLSConn(t, conn, reader)
	if got := conn.ConnectionState().PeerCertificates[0].SerialNumber.String(); got != oldSerial {
		t.Fatalf("established connection certificate changed from %s to %s", oldSerial, got)
	}

	replacement, err := tls.Dial("tcp", address, clientTLS.Clone())
	if err != nil {
		t.Fatal(err)
	}
	newSerial := replacement.ConnectionState().PeerCertificates[0].SerialNumber.String()
	replacement.Close()
	if newSerial == oldSerial {
		t.Fatalf("new handshake still received old certificate serial %s", oldSerial)
	}

	// A broken SIGHUP replacement must not evict the last known-good pair.
	if err := os.WriteFile(a.Config.Current().Server.TLS.CertFile,
		[]byte("not a certificate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runtime.ReloadTLS(); err == nil {
		t.Fatal("malformed replacement certificate was accepted")
	}
	stillGood, err := tls.Dial("tcp", address, clientTLS.Clone())
	if err != nil {
		t.Fatalf("last known-good certificate was lost: %v", err)
	}
	retainedSerial := stillGood.ConnectionState().PeerCertificates[0].SerialNumber.String()
	stillGood.Close()
	if retainedSerial != newSerial {
		t.Fatalf("failed reload changed certificate from %s to %s", newSerial, retainedSerial)
	}
}

func TestExplicitListenersBindSeparateNamespaces(t *testing.T) {
	a := configuredApp(t, func(snapshot *config.Snapshot) {
		snapshot.Server.SinglePort = false
		snapshot.Server.UnifiedAddr = "127.0.0.1:0"
		snapshot.Server.ProxyAddr = "127.0.0.1:0"
		snapshot.Server.AdminAddr = "127.0.0.1:0"
	})
	runtime, err := a.StartListeners()
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownRuntime(t, runtime)
	addresses := runtime.Addresses()
	if addresses["unified"] == addresses["proxy"] ||
		addresses["unified"] == addresses["admin"] ||
		addresses["proxy"] == addresses["admin"] {
		t.Fatalf("explicit listeners did not bind distinct sockets: %+v", addresses)
	}

	for name, path := range map[string]string{
		"unified": "/",
		"admin":   "/healthz",
	} {
		response := getResponse(t, http.DefaultClient, "http://"+addresses[name]+path)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s listener = %d", name, response.StatusCode)
		}
	}

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("explicit proxy"))
	}))
	defer origin.Close()
	proxyTransport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL(t, addresses["proxy"], "")),
	}
	response := getResponse(t, &http.Client{Transport: proxyTransport}, origin.URL+"/x.deb")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	proxyTransport.CloseIdleConnections()
	if response.StatusCode != http.StatusOK || string(body) != "explicit proxy" {
		t.Fatalf("explicit proxy = %d %q", response.StatusCode, body)
	}
}

func TestShutdownDrainsInflightDownloadAndDropsReadiness(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "11")
		_, _ = w.Write([]byte("first "))
		if flush, ok := w.(http.Flusher); ok {
			flush.Flush()
		}
		close(started)
		<-release
		_, _ = w.Write([]byte("last!"))
	}))
	defer origin.Close()

	a := configuredApp(t, func(snapshot *config.Snapshot) {
		snapshot.Server.SinglePort = true
		snapshot.Server.UnifiedAddr = "127.0.0.1:0"
	})
	runtime, err := a.StartListeners()
	if err != nil {
		t.Fatal(err)
	}
	address := runtime.Addresses()["single"]
	proxyTransport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL(t, address, "")),
	}
	defer proxyTransport.CloseIdleConnections()
	clientDone := make(chan struct {
		body string
		err  error
	}, 1)
	go func() {
		response, err := (&http.Client{Transport: proxyTransport}).Get(origin.URL + "/slow.deb")
		if err != nil {
			clientDone <- struct {
				body string
				err  error
			}{err: err}
			return
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		clientDone <- struct {
			body string
			err  error
		}{body: string(body), err: readErr}
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- runtime.Shutdown(ctx) }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned before the active download finished: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	result := <-clientDone
	if result.err != nil || result.body != "first last!" {
		t.Fatalf("download during shutdown = %q, err=%v", result.body, result.err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	recorder := httptest.NewRecorder()
	a.readyz(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable ||
		!strings.Contains(recorder.Body.String(), "not accepting requests") {
		t.Fatalf("post-shutdown readiness = %d %q", recorder.Code, recorder.Body.String())
	}
}

func appWithTLS(t *testing.T, single bool) (*App, *pki.CA) {
	t.Helper()
	snapshot := config.Defaults()
	snapshot.DataDir = t.TempDir()
	snapshot.Log.Level = "error"
	snapshot.Server.SinglePort = single
	snapshot.Server.UnifiedAddr = "127.0.0.1:0"
	snapshot.Server.ProxyAddr = "127.0.0.1:0"
	snapshot.Server.AdminAddr = "127.0.0.1:0"
	snapshot.Projects = map[string]config.Project{"team-a": {Name: "team-a"}}
	ca, _, err := pki.LoadOrCreateCA(snapshot.CertsDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.IssueServer([]string{"localhost", "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	snapshot.Server.TLS.CertFile = filepath.Join(snapshot.CertsDir(), pki.ServerCertFile)
	snapshot.Server.TLS.KeyFile = filepath.Join(snapshot.CertsDir(), pki.ServerKeyFile)
	snapshot.Server.TLS.CAFile = filepath.Join(snapshot.CertsDir(), pki.CACertFile)
	a, err := Open(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, ca
}

func httpOnTLSConn(t *testing.T, conn *tls.Conn, reader *bufio.Reader) {
	t.Helper()
	if _, err := fmt.Fprint(conn,
		"GET /healthz HTTP/1.1\r\nHost: localhost\r\nConnection: keep-alive\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://localhost/healthz", nil)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("TLS keepalive health = %d", response.StatusCode)
	}
}

func shutdownRuntime(t *testing.T, runtime *Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

// The admin listener carries session logins and the control API, so wherever a
// certificate exists it must be TLS. It used to bind plain HTTP unconditionally, which
// put credentials in cleartext for every deployment that chose explicit listeners.
func TestExplicitAdminListenerUsesTLSWhenConfigured(t *testing.T) {
	a, _ := appWithTLS(t, false)
	runtime, err := a.StartListeners()
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownRuntime(t, runtime)
	addresses := runtime.Addresses()

	roots := x509.NewCertPool()
	caPEM, err := os.ReadFile(filepath.Join(a.Config.Current().CertsDir(), pki.CACertFile))
	if err != nil || !roots.AppendCertsFromPEM(caPEM) {
		t.Fatalf("load test CA: %v", err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12,
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}

	for _, name := range []string{"admin", "unified"} {
		response := getResponse(t, client, "https://"+addresses[name]+"/healthz")
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("HTTPS %s listener = %d", name, response.StatusCode)
		}
		if response.TLS == nil {
			t.Fatalf("%s listener answered without TLS", name)
		}
	}

	// The forward proxy deliberately stays plain — an http_proxy client speaks
	// cleartext to its proxy, so there is no TLS form of that protocol to offer. A TLS
	// handshake against it must therefore fail rather than succeed.
	conn, err := tls.Dial("tcp", addresses["proxy"], &tls.Config{
		RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12,
	})
	if err == nil {
		_ = conn.Close()
		t.Fatal("the apt/apk proxy listener negotiated TLS")
	}
}
