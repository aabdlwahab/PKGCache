package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/brightskies/pkgreg/internal/app"
	"github.com/brightskies/pkgreg/internal/buildinfo"
	"github.com/brightskies/pkgreg/internal/clientrelease"
	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/pki"
	consoleweb "github.com/brightskies/pkgreg/internal/web"
)

// runDoctor reports everything that would stop this host from serving, in one pass,
// instead of making the operator discover the problems one failed request at a time.
//
// Two properties this command did not have and needed:
//
// It is read-only. It used to call EnsureDirs and app.Open, so pointing it at a path
// that did not exist yet — a typo, or the wrong -data-dir — created a directory tree,
// a catalog, a control database and a host key there, and then reported the result
// "healthy". A diagnostic that changes the system cannot be run to find out what the
// system is, which is the only reason to run one.
//
// It fails on an unsafe posture rather than staying quiet about it. A fresh instance
// with no authentication and an unrestricted forward proxy is not healthy just because
// its disks are writable, and reporting it as healthy is how that configuration reached
// a network. The posture itself is computed in internal/config so that this command and
// `pkgreg serve` cannot form different opinions about the same fields.
func runDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindConfigFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: pkgreg doctor [flags]

Reports what would stop this host from serving. Never modifies anything: an
uninitialized data directory is reported, not created.

exit status:
  0  healthy (warnings may still be printed)
  1  broken — configuration, storage or TLS would stop this host serving
  3  not initialized — run `+"`pkgreg init`"+`
  4  unsafe to expose — reachable from other machines with authentication off,
     or with an unrestricted apt/apk forward proxy

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	d := &diagnosis{}
	fmt.Println(buildinfo.Get().String())
	fmt.Println()

	snap, err := config.Load(collect())
	if err != nil {
		d.fail("config", err.Error())
		d.report()
		return errUnhealthy
	}
	d.ok("config", "valid; data_dir="+snap.DataDir)

	// Doctor's output is a report for a human to read. Opening the storage layer
	// would otherwise interleave startup logs through it.
	snap.Log.Level = "error"
	opened := d.checkStorage(snap)
	d.checkTLS(ctx, snap)
	d.checkPosture(snap, opened)
	d.checkGit(ctx)
	d.checkConsole()
	d.checkClientDownloads(snap.DataDir)
	d.checkFileLimits()

	d.report()
	switch {
	case d.uninitialized:
		return errNotInitialized
	case d.unsafe:
		return errUnsafePosture
	case d.failures > 0:
		return errUnhealthy
	}
	return nil
}

// Distinct exit statuses so a provisioning script can branch on the class of problem
// rather than grepping the report. dispatch maps each to its documented code.
var (
	errUnhealthy      = fmt.Errorf("doctor found problems")
	errNotInitialized = fmt.Errorf("doctor: this data directory is not initialized")
	errUnsafePosture  = fmt.Errorf("doctor: unsafe to expose on a network")
)

type diagnosis struct {
	lines    []string
	failures int
	warnings int
	// uninitialized and unsafe select an exit status. They are separate from the
	// failure count because both are reported alongside ordinary failures and the
	// most specific status is the useful one.
	uninitialized bool
	unsafe        bool
}

func (d *diagnosis) ok(area, msg string) {
	d.lines = append(d.lines, fmt.Sprintf("  ok    %-10s %s", area, msg))
}
func (d *diagnosis) warn(area, msg string) {
	d.lines = append(d.lines, fmt.Sprintf("  warn  %-10s %s", area, msg))
	d.warnings++
}
func (d *diagnosis) fail(area, msg string) {
	d.lines = append(d.lines, fmt.Sprintf("  FAIL  %-10s %s", area, msg))
	d.failures++
}

func (d *diagnosis) report() {
	for _, l := range d.lines {
		fmt.Println(l)
	}
	fmt.Println()
	switch {
	case d.uninitialized:
		fmt.Printf("not initialized — run `pkgreg init` (%d problem(s), %d warning(s))\n",
			d.failures, d.warnings)
	case d.unsafe:
		fmt.Printf("UNSAFE TO EXPOSE — %d problem(s), %d warning(s)\n", d.failures, d.warnings)
	case d.failures > 0:
		fmt.Printf("%d problem(s), %d warning(s)\n", d.failures, d.warnings)
	case d.warnings > 0:
		fmt.Printf("healthy, with %d warning(s)\n", d.warnings)
	default:
		fmt.Println("healthy")
	}
}

// checkStorage inspects the store without creating any part of it, and reports whether
// it managed to open the databases — the posture check needs the control database to
// know whether any account exists.
func (d *diagnosis) checkStorage(snap *config.Snapshot) *app.App {
	if missing := uninitializedReason(snap); missing != "" {
		d.fail("storage", missing+
			"; run `pkgreg init -data-dir "+snap.DataDir+"` (doctor never creates it)")
		d.uninitialized = true
		return nil
	}
	a, err := app.Open(snap)
	if err != nil {
		d.fail("storage", err.Error())
		return nil
	}

	if err := a.Catalog.Ping(); err != nil {
		d.fail("catalog", err.Error())
	} else {
		d.ok("catalog", snap.CatalogPath())
	}

	// Writability is proved by writing, because a read-only filesystem is the failure
	// that otherwise surfaces as every download mysteriously failing. The staging file
	// is aborted immediately and never becomes a blob, so this stays read-only in
	// every sense that matters: nothing observable is added or changed.
	w, err := a.Blobs.Create()
	if err != nil {
		d.fail("blobs", "store is not writable: "+err.Error())
		return a
	}
	_ = w.Abort()
	d.checkHardlinks(snap.DataDir)

	count, bytes, err := a.Blobs.Usage()
	if err != nil {
		d.warn("blobs", "could not measure the store: "+err.Error())
		return a
	}
	d.ok("blobs", fmt.Sprintf("%s, %d blob(s), %s", snap.BlobRoot(), count, humanBytes(bytes)))
	return a
}

// uninitializedReason names the first thing `pkgreg init` would have created that is
// not there, or "" when the directory has been initialized.
//
// Checking specific artifacts rather than merely whether the directory exists: an
// operator who points doctor at an existing but unrelated directory — a home directory,
// a mount point — should be told it is not a pkgreg data directory, not have one
// quietly built inside it.
func uninitializedReason(snap *config.Snapshot) string {
	if info, err := os.Stat(snap.DataDir); err != nil || !info.IsDir() {
		return "no data directory at " + snap.DataDir
	}
	if _, err := os.Stat(snap.CatalogPath()); err != nil {
		return "no catalog database at " + snap.CatalogPath()
	}
	if _, err := os.Stat(snap.ControlPath()); err != nil {
		return "no control database at " + snap.ControlPath()
	}
	return ""
}

// checkPosture is the check that decides whether this instance may face a network.
//
// It asks internal/config the same question `pkgreg serve` asks, so the two can never
// disagree — they did, and the disagreement is precisely how an unauthenticated control
// plane came to be reported healthy.
func (d *diagnosis) checkPosture(snap *config.Snapshot, opened *app.App) {
	if opened == nil {
		// Without the control database there is no way to know whether an account
		// exists, and guessing "authentication is on" would turn this check into
		// false reassurance. The uninitialized failure already dominates the exit
		// status, so say why the check did not run and stop.
		d.warn("posture", "not evaluated: the control database could not be opened")
		return
	}
	defer func() { _ = opened.Close() }()

	issues := snap.Posture(opened.Accounts.Enabled())
	if len(issues) == 0 {
		reach := "loopback only"
		if snap.AdminReachableOffHost() {
			reach = "reachable from other machines"
		}
		d.ok("posture", "authentication on, proxy restricted ("+reach+")")
		return
	}
	for _, issue := range issues {
		switch issue.Severity {
		case config.SeverityCritical:
			d.fail("posture", issue.Summary+"\n"+
				strings.Repeat(" ", 16)+"fix: "+issue.Remedy)
			d.unsafe = true
		case config.SeverityWarn:
			d.warn("posture", issue.Summary+"\n"+
				strings.Repeat(" ", 16)+"fix: "+issue.Remedy)
		default:
			d.ok("posture", issue.Summary)
		}
	}
}

func (d *diagnosis) checkHardlinks(dataDir string) {
	source, err := os.CreateTemp(dataDir, ".doctor-link-source-*")
	if err != nil {
		d.fail("hardlinks", err.Error())
		return
	}
	sourceName := source.Name()
	targetName := sourceName + ".link"
	defer func() { _ = os.Remove(sourceName) }()
	defer func() { _ = os.Remove(targetName) }()
	if err := source.Close(); err != nil {
		d.fail("hardlinks", err.Error())
		return
	}
	if err := os.Link(sourceName, targetName); err != nil {
		d.fail("hardlinks", "data filesystem cannot create hardlinks: "+err.Error())
		return
	}
	d.ok("hardlinks", "supported (migration and cross-project dedup enabled)")
}

func (d *diagnosis) checkTLS(ctx context.Context, snap *config.Snapshot) {
	certDir := snap.CertsDir()
	caPath := filepath.Join(certDir, pki.CACertFile)
	if _, err := os.Stat(caPath); err != nil {
		d.warn("tls", "no CA yet — run `pkgreg init`")
		return
	}
	// The fingerprint is what pkgreg-client and pkgreg-bridge pin against, so it has to
	// be obtainable without reaching for openssl or the console.
	if fingerprint := caFingerprint(caPath); fingerprint != "" {
		d.ok("ca", fingerprint)
	}
	// Exactly what the server will use, with no fallback of its own.
	//
	// Doctor used to substitute the conventional paths when the configuration named
	// none, so on a host started with -data-dir and no config it reported a valid
	// certificate — for a server that was serving cleartext, because serve read the
	// configuration and found no pair. config.Load now adopts those paths for both, and
	// reading the same field here is what keeps the two answers the same one.
	certFile, keyFile := snap.Server.TLS.CertFile, snap.Server.TLS.KeyFile
	if certFile == "" || keyFile == "" {
		d.fail("tls", "no certificate pair, so this server answers in cleartext and "+
			"pkgreg-client will refuse it (\"server must use https\"); run `pkgreg init -data-dir "+
			snap.DataDir+"` to mint one, or set server.tls.cert_file and key_file")
		return
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		d.fail("tls", "certificate and key do not load: "+err.Error())
		return
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		d.fail("tls", "unparseable certificate: "+err.Error())
		return
	}

	left := time.Until(leaf.NotAfter)
	switch {
	case left <= 0:
		d.fail("tls", "certificate expired on "+leaf.NotAfter.Format(time.DateOnly))
	case left < 30*24*time.Hour:
		d.warn("tls", fmt.Sprintf("certificate expires in %d days — re-run `pkgreg init -force`",
			int(left.Hours()/24)))
	default:
		names := append([]string{}, leaf.DNSNames...)
		for _, ip := range leaf.IPAddresses {
			names = append(names, ip.String())
		}
		d.ok("tls", fmt.Sprintf("valid until %s for %s",
			leaf.NotAfter.Format(time.DateOnly), strings.Join(names, ", ")))
	}

	// The most common bring-up failure is a certificate that does not cover the name
	// clients actually use, which surfaces as an opaque x509 error on every host.
	discovered := pki.DiscoverSANs(ctx)
	var missing []string
	for _, n := range discovered {
		if leaf.VerifyHostname(n) != nil {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		d.warn("tls", "certificate does not cover "+strings.Join(missing, ", ")+
			" — clients reaching this host by those names will fail; re-run `pkgreg init -force`")
	}
}

// checkGit reports the one external runtime dependency. Its absence disables only the
// git mirror role; everything else still serves, so this is a warning, not a failure.
func (d *diagnosis) checkGit(ctx context.Context) {
	path, err := exec.LookPath("git")
	if err != nil {
		d.warn("git", "`git` is not on PATH — the git mirror ecosystem will be unavailable")
		return
	}
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		d.warn("git", "`git` is present but not runnable: "+err.Error())
		return
	}
	d.ok("git", strings.TrimSpace(string(out)))
}

// checkConsole reports what is compiled in. There is no longer a way to build
// without the console, so this cannot fail — it exists to show that the binary is
// self-contained, and to catch an embed that silently picked up nothing.
func (d *diagnosis) checkConsole() {
	files, bytes := consoleweb.New(true).Assets()
	if files == 0 {
		d.warn("console", "no assets embedded — the dist tree is empty")
		return
	}
	d.ok("console", fmt.Sprintf("%d files embedded (%d KiB)", files, bytes/1024))
}

// checkClientDownloads reports whether developers can actually get the client from
// this instance. A cache with nothing published still serves packages perfectly, so
// this is a warning — but it is the warning that explains why the tutorial's download
// panel is empty, which is otherwise a mystery from the reader's side.
func (d *diagnosis) checkClientDownloads(dataDir string) {
	dir := clientrelease.Dir(dataDir)
	total := len(clientrelease.ClientPlatforms())
	missing, corrupt, unrecorded, err := clientrelease.Verify(dir)
	if err != nil {
		d.warn("clients", "cannot read "+dir+": "+err.Error())
		return
	}
	switch {
	case len(corrupt) > 0:
		d.fail("clients", strings.Join(corrupt, ", ")+
			" does not match its published SHA-256 — re-run `pkgreg publish-client`")
	case len(unrecorded) > 0:
		d.warn("clients", "no digest is recorded for "+strings.Join(unrecorded, ", ")+
			" — re-run `pkgreg publish-client`")
	case len(missing) == total:
		d.warn("clients", fmt.Sprintf(
			"nothing published, so /tutorial offers no download; run `pkgreg publish-client` (see %s)",
			dir))
	case len(missing) > 0:
		d.warn("clients", fmt.Sprintf("%d of %d platforms are published; missing %s",
			total-len(missing), total, strings.Join(missing, ", ")))
	default:
		d.ok("clients", fmt.Sprintf(
			"%d platform binaries published with matching checksums", total))
	}
}

func (d *diagnosis) checkFileLimits() {
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		d.warn("nofile", "could not read process limit: "+err.Error())
		return
	}
	if limit.Cur < 8192 {
		d.warn("nofile", fmt.Sprintf("soft limit is %d; systemd install configures 1048576", limit.Cur))
		return
	}
	d.ok("nofile", fmt.Sprintf("soft limit %d", limit.Cur))
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
