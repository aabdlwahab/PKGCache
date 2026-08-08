package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brightskies/pkgreg/internal/catalog"
	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/control"
	"github.com/brightskies/pkgreg/internal/control/auth"
	"github.com/brightskies/pkgreg/internal/onboarding"
	"github.com/brightskies/pkgreg/internal/pki"
)

// runInit prepares a host to serve.
//
// It replaces scripts/bootstrap.sh and scripts/gen-certs.sh. Most of what
// bootstrap.sh did was work around containers — pre-creating bind-mount sources so
// the docker daemon would not create them root-owned, and writing a uid/gid mapping
// into .env. None of that exists any more; what remains is genuinely needed setup.
//
// Idempotent: re-running never overwrites a CA or a config file that already exists.
func runInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	collect := bindConfigFlags(fs)
	var (
		hostnames = fs.String("hostnames", "",
			"extra names or IPs clients will reach this cache by (comma-separated)")
		writeConfig = fs.Bool("write-config", true, "write a starter config file into the data dir")
		force       = fs.Bool("force", false, "re-issue the server certificate even if one exists")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	snap, err := config.Load(collect())
	if err != nil {
		return err
	}
	if err := snap.EnsureDirs(); err != nil {
		return err
	}
	fmt.Printf("data directory  %s\n", snap.DataDir)

	// The CA is reused if present: it is distributed to every build host, so minting
	// a new one would silently invalidate trust everywhere.
	ca, created, err := pki.LoadOrCreateCA(snap.CertsDir())
	if err != nil {
		return err
	}
	caPath := filepath.Join(snap.CertsDir(), pki.CACertFile)
	if created {
		fmt.Printf("certificate authority  minted %s\n", caPath)
	} else {
		fmt.Printf("certificate authority  reusing %s (already-distributed trust preserved)\n", caPath)
	}

	// Print the fingerprint here, at the one moment the operator is certainly looking.
	// pkgreg-client and pkgreg-bridge both refuse to run without it, and telling people
	// to "ask your administrator" is no help when they are the administrator.
	fingerprint := caFingerprint(caPath)
	if fingerprint != "" {
		fmt.Printf("CA fingerprint         %s\n", fingerprint)
	}

	serverCert := filepath.Join(snap.CertsDir(), pki.ServerCertFile)
	if _, err := os.Stat(serverCert); err == nil && !*force && *hostnames == "" {
		fmt.Printf("server certificate     keeping %s (pass -force or -hostnames to re-issue)\n", serverCert)
	} else {
		var extra []string
		for _, h := range strings.Split(*hostnames, ",") {
			if h = strings.TrimSpace(h); h != "" {
				extra = append(extra, h)
			}
		}
		sans, err := ca.IssueServer(pki.DiscoverSANs(ctx, extra...))
		if err != nil {
			return err
		}
		fmt.Printf("server certificate     issued for %s\n", strings.Join(sans, ", "))
	}

	if *writeConfig {
		path := filepath.Join(snap.DataDir, "pkgreg.yaml")
		if _, err := os.Stat(path); err == nil {
			fmt.Printf("config                 keeping %s\n", path)
		} else if err := writeStarterConfig(path, snap); err != nil {
			return err
		} else {
			fmt.Printf("config                 wrote %s\n", path)
		}
	}

	// Provisioning the superuser is what makes the control plane closed by default.
	// It happens after the certificate work so that a failure here still leaves a host
	// with usable TLS material, and before the "Next" text so that text can describe
	// the instance that now exists.
	bootstrap, err := bootstrapAuth(snap)
	if err != nil {
		return err
	}

	printBootstrap(bootstrap)

	fmt.Printf(`
Next:
  # 1. Offer the client for download. Copy the pkgreg-client-* release files onto
  #    this host first; with no path, the directory holding pkgreg is scanned.
  pkgreg publish-client -data-dir %s [path...]

  # 2. Start the server:
  pkgreg serve -config %s

Developers then open https://<this-host>%s/tutorial, download pkgreg-client, and
copy the command shown there. The normal client needs no sudo and is undone by
typing exit. --persist is only for managed CI or Docker hosts that intentionally
need machine-wide trust.

Step 1 is not optional in practice: until it is done, the tutorial has nothing to
hand out and a new developer's first page is a dead end. `+"`pkgreg doctor`"+` reports
whether it has been done.

Give the fingerprint above to client users over a channel separate from the
certificate itself — that is what makes the first download safe. It is not a
secret; `+"`pkgreg doctor`"+` prints it again at any time.

The CA private key (%s) must never leave this host — it could mint
certificates trusted by every one of them.
`,
		snap.DataDir,
		filepath.Join(snap.DataDir, "pkgreg.yaml"),
		portSuffix(snap.Server.UnifiedAddr),
		filepath.Join(snap.CertsDir(), pki.CAKeyFile))
	return nil
}

// bootstrapAuth creates this host's databases and guarantees it has a superuser.
//
// init is the one command allowed to create state, so both databases are created here
// rather than lazily on the first serve. That is what lets `pkgreg doctor` be a
// read-only diagnostic: "the catalog exists" is only a usable signal for whether a
// directory has been initialized if something reliably creates it at initialization
// time. Opening a catalog runs its migrations, so this also surfaces a schema problem
// while the operator is still watching, rather than at first traffic.
func bootstrapAuth(snap *config.Snapshot) (auth.BootstrapResult, error) {
	cat, err := catalog.Open(catalog.Options{
		Path:          snap.CatalogPath(),
		ReadPoolSize:  snap.Catalog.ReadPoolSize,
		BatchInterval: snap.Catalog.BatchInterval,
		BatchSize:     snap.Catalog.BatchSize,
		CacheSize:     snap.Catalog.CacheSize,
	})
	if err != nil {
		return auth.BootstrapResult{}, err
	}
	if err := cat.Close(); err != nil {
		return auth.BootstrapResult{}, err
	}

	db, err := control.Open(snap.ControlPath())
	if err != nil {
		return auth.BootstrapResult{}, err
	}
	defer func() { _ = db.Close() }()
	return auth.Bootstrap(db, snap.Auth.RootUser)
}

// printBootstrap shows a generated password exactly once.
//
// It is deliberately the loudest thing init prints. A credential that scrolls past in
// the same voice as a file path is a credential the operator will not save, and there
// is no second chance: only its scrypt digest is stored.
func printBootstrap(result auth.BootstrapResult) {
	switch {
	case result.Created:
		fmt.Printf(`
────────────────────────────────────────────────────────────────────────────
  CONTROL-PLANE LOGIN — shown once, not recoverable

      username  %s
      password  %s

  Save this now. Only its scrypt digest is stored, so a lost password means
  deleting the account from control.db and re-running init.

  Authentication is ON because this account exists. Without it the console
  and the whole /api/v1 surface — projects, tokens, users, credentials,
  maintenance, audit — would answer any caller on the network.
────────────────────────────────────────────────────────────────────────────
`, result.Username, result.Password)
	case result.ConfiguredRoot != "":
		fmt.Printf("authentication         enforced by auth.root_user=%q in the configuration\n",
			result.ConfiguredRoot)
	default:
		fmt.Printf("authentication         enforced; %d account(s) already exist, none created\n",
			result.Existing)
	}
}

// caFingerprint returns the CA's SHA-256, or "" if it cannot be read. A missing
// fingerprint is never fatal here: init's job is to mint certificates, and failing
// the whole command because a convenience line could not be printed would be absurd.
func caFingerprint(caPath string) string {
	pem, err := os.ReadFile(caPath)
	if err != nil {
		return ""
	}
	fingerprint, err := onboarding.FingerprintSHA256(pem)
	if err != nil {
		return ""
	}
	return fingerprint
}

func writeStarterConfig(path string, snap *config.Snapshot) error {
	body := fmt.Sprintf(`# pkgreg configuration. Every value here may also be set by a PKGREG_* environment
# variable or a command-line flag; flags win, then the environment, then this file.

data_dir: %s

server:
  # Serve TLS, plain HTTP and the apt forward proxy on one address by sniffing the
  # first byte of each connection. One firewall rule instead of three. Set false to
  # bind unified_addr, proxy_addr and admin_addr separately.
  single_port: true
  unified_addr: "%s"
  proxy_addr: "%s"
  admin_addr: "%s"
  tls:
    cert_file: %s
    key_file: %s
    ca_file: %s
  # Only enable behind a reverse proxy you control: otherwise a client can spoof its
  # own login-throttle key through X-Forwarded-For.
  trust_proxy: false
  # Which upstream hosts the apt/apk forward proxy will fetch on a client's behalf.
  #
  # LEAVING THIS EMPTY MAKES THIS HOST AN OPEN HTTP RELAY. Anyone who can reach the
  # proxy port can have pkgreg fetch any http:// URL for them — including addresses
  # only this host can route to, such as cloud instance metadata or an internal admin
  # page. The request goes out with this host's address, not theirs.
  #
  # List the repositories you actually mirror. A leading "*." admits subdomains but
  # not the parent. "*" on its own restores relay-anywhere as a deliberate choice.
  # `+"`pkgreg doctor`"+` fails when this is empty and the listener is not loopback-only.
  proxy_allowlist: []
  #  - archive.ubuntu.com
  #  - security.ubuntu.com
  #  - deb.debian.org
  #  - "*.alpinelinux.org"

log:
  level: info
  format: json
  access: true

upstream:
  # Generous on purpose: the largest artifact seen in production is a 2.5 GB CUDA
  # wheel, which over a slow uplink takes many minutes.
  request_timeout: 20m

git:
  refs_ttl: 1m
  max_upload_packs: 8

maintenance:
  gc_interval: 6h
  # Must comfortably exceed the longest plausible fetch. A blob is written before its
  # catalog entry, so too short a grace period could collect content that an
  # in-flight request is about to serve.
  gc_grace: 1h
  # Set either target to make the scheduled LRU pass active. TTL is optional.
  evict_interval: 30m
  evict_target_bytes: 0
  evict_min_free_bytes: 0
  evict_ttl: 0s

auth:
  # Authentication is already ON: `+"`pkgreg init`"+` provisioned a superuser in control.db
  # and printed its password once. Manage accounts in Console → Admin, not here.
  #
  # root_user/root_password below are the separate break-glass superuser, verified
  # straight from configuration. They are commented out on purpose: the password
  # would sit in this file in plaintext, so use them only to recover an instance
  # whose stored accounts are unusable, and remove them again afterwards. Prefer
  # PKGREG_ROOT_PASSWORD over writing it here.
  # root_user: breakglass
  # root_password: <set via PKGREG_ROOT_PASSWORD>
  session_ttl: 12h

  # Sign-in-free, read-only browsing of the global project. The console's login page
  # offers "Browse as guest", which shows what is cached and how to point tools at
  # this cache — the Overview, Cache and Connect views.
  #
  # A guest can reach an explicit allowlist of read-only routes and nothing else:
  # no accounts, audit log, tokens, upstreams, snapshots or maintenance, and no
  # project but "global". Set false to require an account for any view at all.
  guest_read: true

  # The broader, unscoped form: any caller with no session at all may read every
  # project. Off by default, and there is rarely a reason to prefer it over
  # guest_read, which is bounded.
  # anon_read: false
`,
		snap.DataDir,
		snap.Server.UnifiedAddr, snap.Server.ProxyAddr, snap.Server.AdminAddr,
		filepath.Join(snap.CertsDir(), pki.ServerCertFile),
		filepath.Join(snap.CertsDir(), pki.ServerKeyFile),
		filepath.Join(snap.CertsDir(), pki.CACertFile),
	)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write starter config: %w", err)
	}
	return nil
}
