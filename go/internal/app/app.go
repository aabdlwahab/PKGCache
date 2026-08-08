// Package app is the composition root: it constructs every subsystem, wires them
// together, and owns their lifecycle.
//
// This is the only place construction happens. Every other package receives its
// collaborators as arguments and reaches for no globals, which is what keeps them
// testable in isolation.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/brightskies/pkgreg/internal/blob"
	"github.com/brightskies/pkgreg/internal/buildinfo"
	"github.com/brightskies/pkgreg/internal/catalog"
	"github.com/brightskies/pkgreg/internal/config"
	"github.com/brightskies/pkgreg/internal/control"
	controlapi "github.com/brightskies/pkgreg/internal/control/api"
	"github.com/brightskies/pkgreg/internal/control/auth"
	"github.com/brightskies/pkgreg/internal/control/credential"
	"github.com/brightskies/pkgreg/internal/control/job"
	controlproject "github.com/brightskies/pkgreg/internal/control/project"
	"github.com/brightskies/pkgreg/internal/eco"
	"github.com/brightskies/pkgreg/internal/eco/apt"
	"github.com/brightskies/pkgreg/internal/eco/files"
	ecogit "github.com/brightskies/pkgreg/internal/eco/git"
	"github.com/brightskies/pkgreg/internal/eco/npm"
	"github.com/brightskies/pkgreg/internal/eco/oci"
	"github.com/brightskies/pkgreg/internal/eco/pypi"
	"github.com/brightskies/pkgreg/internal/engine"
	"github.com/brightskies/pkgreg/internal/maintenance"
	"github.com/brightskies/pkgreg/internal/obs"
	"github.com/brightskies/pkgreg/internal/ops"
	"github.com/brightskies/pkgreg/internal/upstream"
	peerupstream "github.com/brightskies/pkgreg/internal/upstream/peer"
	consoleweb "github.com/brightskies/pkgreg/internal/web"
)

// App holds the running subsystems.
type App struct {
	Config      *config.Store
	Log         *slog.Logger
	Metrics     *obs.Metrics
	Events      *obs.Bus
	Blobs       *blob.Store
	Catalog     *catalog.DB
	Control     *control.DB
	Pool        *upstream.Pool
	Engine      *engine.Engine
	Ecos        *eco.Registry
	Data        *DataPlane
	API         *controlapi.API
	Accounts    *auth.Accounts
	Sessions    *auth.Sessions
	Tokens      *auth.Tokens
	Projects    *controlproject.Service
	Credentials *credential.Store
	Jobs        *job.Manager
	Ops         *ops.Service
	Maintenance *maintenance.Service
	Peer        *peerupstream.Service
	Console     *consoleweb.Handler

	// Activity, when set, is called once for every request that reaches this process,
	// before it is served.
	//
	// It exists for pkgcache, whose daemon exits after an idle period: request arrival
	// is the only honest signal that a cache is still in use, and there is no other
	// place that sees every request regardless of listener, ecosystem or outcome. A
	// server leaves it nil and pays nothing — the wrapper is not installed at all.
	//
	// Set it before StartListeners. It is read without synchronisation, which is safe
	// only because nothing writes it once requests can arrive.
	Activity func()

	// inherited is a socket the caller already bound; see WithListener.
	inherited net.Listener

	cancel context.CancelFunc

	listenersExpected atomic.Bool
	listenersReady    atomic.Bool
}

// Option customises construction. It exists for pkgcache, which needs to install a
// store guard the server has no use for; every option is inert unless passed.
type Option func(*options)

type options struct {
	guard    engine.StoreGuard
	listener net.Listener
}

// WithStoreGuard installs a policy deciding whether fills may be kept. See
// engine.StoreGuard: only pkgcache passes one, and a nil guard is the server's
// behaviour of storing everything.
func WithStoreGuard(g engine.StoreGuard) Option {
	return func(o *options) { o.guard = g }
}

// WithListener serves on a socket the caller already holds instead of binding one.
//
// This is what socket activation needs: systemd or launchd binds the port, hands the
// descriptor to the process it starts, and holds the address open between runs — which
// is the only way persistent client settings can name a fixed port that an on-demand
// daemon is not always listening on. Single-port mode only.
func WithListener(l net.Listener) Option {
	return func(o *options) { o.listener = l }
}

// Open constructs everything from a validated snapshot. The caller closes it.
//
// Ordering matters: the data directory must exist before the blob store touches it,
// and the blob store's crash recovery must run before anything can serve, so a
// staging file from a previous kill is never mistaken for live content.
func Open(snap *config.Snapshot, opts ...Option) (*App, error) {
	var settings options
	for _, apply := range opts {
		apply(&settings)
	}
	if err := snap.EnsureDirs(); err != nil {
		return nil, err
	}

	logger := obs.NewLogger(obs.LogOptions{
		Level:  snap.Log.SlogLevel(),
		Format: snap.Log.Format,
		Output: os.Stderr,
	})
	slog.SetDefault(logger)
	logger.Info("starting", "build", buildinfo.Get().String(), "data_dir", snap.DataDir)

	blobs, err := blob.Open(snap.BlobRoot())
	if err != nil {
		return nil, err
	}
	// A previous process may have died mid-download. Its staging files are garbage;
	// nothing else is running yet, so this is the one safe moment to sweep them.
	if n, err := blobs.CleanStaging(); err != nil {
		logger.Warn("could not sweep staging files", "error", err)
	} else if n > 0 {
		logger.Info("removed interrupted downloads", "count", n)
	}

	cat, err := catalog.Open(catalog.Options{
		Path:          snap.CatalogPath(),
		ReadPoolSize:  snap.Catalog.ReadPoolSize,
		BatchInterval: snap.Catalog.BatchInterval,
		BatchSize:     snap.Catalog.BatchSize,
		CacheSize:     snap.Catalog.CacheSize,
	})
	if err != nil {
		return nil, err
	}
	controlDB, err := control.Open(snap.ControlPath())
	if err != nil {
		_ = cat.Close()
		return nil, err
	}

	cfg := config.NewStore(snap)
	metrics := obs.NewMetrics()
	events := obs.NewBus()
	accounts := auth.NewAccounts(controlDB, snap.Auth.RootUser, snap.Auth.RootPassword)
	if err := accounts.ImportLegacy(filepath.Join(snap.DataDir, "config", "users.json")); err != nil {
		_ = controlDB.Close()
		_ = cat.Close()
		return nil, fmt.Errorf("import legacy users: %w", err)
	}
	sessions := auth.NewSessions(snap.Auth.SessionTTL)
	tokens := auth.NewTokens(controlDB)
	credentials, err := credential.Open(controlDB, snap.ControlKeyPath())
	if err != nil {
		_ = controlDB.Close()
		_ = cat.Close()
		return nil, err
	}
	ecosystems := eco.NewRegistry()
	ecosystems.Register(apt.New())
	ecosystems.Register(files.New(tokens, 0))
	ecosystems.Register(ecogit.NewWithConfig(snap.Git))
	ecosystems.Register(npm.New())
	ecosystems.Register(oci.New())
	ecosystems.Register(pypi.New())
	projects, err := controlproject.New(controlDB, cfg, ecosystems, credentials, metrics)
	if err != nil {
		_ = controlDB.Close()
		_ = cat.Close()
		return nil, err
	}
	pool, err := upstream.NewWithError(snap.Upstream, metrics)
	if err != nil {
		_ = controlDB.Close()
		_ = cat.Close()
		return nil, err
	}
	baseCtx, cancel := context.WithCancel(context.Background())
	peerService := peerupstream.New(blobs, cfg, pool, tokens)
	cacheEngine := engine.New(engine.Options{
		Blobs: blobs, Catalog: cat, Pool: pool, Config: cfg,
		Metrics: metrics, Events: events, Context: baseCtx, Peer: peerService,
		Guard: settings.guard,
	})
	jobs, err := job.New(controlDB, events, 4)
	if err != nil {
		cancel()
		_ = controlDB.Close()
		_ = cat.Close()
		return nil, err
	}
	jobs.SetMetrics(metrics)

	a := &App{
		Config: cfg, Log: logger, Metrics: metrics, Events: events,
		Blobs: blobs, Catalog: cat, Control: controlDB, Pool: pool, Engine: cacheEngine,
		Ecos: ecosystems, Accounts: accounts, Sessions: sessions, Tokens: tokens,
		Projects: projects, Credentials: credentials, Jobs: jobs, cancel: cancel,
		Peer:    peerService,
		Console: consoleweb.New(!snap.Server.Headless),
	}
	a.Data = NewDataPlane(cfg, cacheEngine, ecosystems, tokens)
	a.Ops = &ops.Service{
		Catalog: cat, Blobs: blobs, Config: cfg, Projects: projects,
		Ecos: ecosystems, Data: http.HandlerFunc(a.Data.ServeInternal), DataDir: snap.DataDir,
	}
	a.Ops.Register(jobs)
	a.Maintenance = &maintenance.Service{
		Catalog: cat, Blobs: blobs, Config: cfg, Metrics: metrics,
	}
	a.Maintenance.Register(jobs)
	a.Maintenance.Start(baseCtx)
	go flushStats(baseCtx, cacheEngine, logger, snap.Maintenance.StatsFlushInterval)
	go publishHealth(baseCtx, events)
	// config.Load adopts the data directory's own certs/ca.crt when nothing names one,
	// so this is simply whatever the resolved configuration says. It used to re-derive
	// the fallback here, which meant two places decided where the CA lives.
	caFile := snap.Server.TLS.CAFile
	a.API = controlapi.New(controlapi.Options{
		DB: controlDB, Config: cfg, Accounts: accounts, Sessions: sessions,
		Tokens: tokens, Credentials: credentials, Projects: projects, Jobs: jobs, Catalog: cat,
		Engine: cacheEngine, Ecos: ecosystems, Events: events,
		DataDir: snap.DataDir, CAFile: caFile,
	})
	// Zero-value the series for every known project so dashboards read 0 rather
	// than "no data" before the first request lands. See obs.InitProjectSeries.
	a.Metrics.InitProjectSeries(config.GlobalProject)
	for name := range cfg.Current().Projects {
		a.Metrics.InitProjectSeries(name)
	}
	a.refreshStorageMetrics()
	a.inherited = settings.listener
	return a, nil
}

// flushStats folds the engine's in-memory usage counters into the catalog on a timer.
//
// Without this loop the counters only reached the database at shutdown, which had
// three consequences on any long-running instance: the leaderboard and traffic totals
// read as zero, the time series had no points to draw, and — the expensive one —
// entries.last_access never advanced, so eviction ranked by write time instead of by
// use and would discard the most-requested content first.
func flushStats(ctx context.Context, e *engine.Engine, log *slog.Logger, every time.Duration) {
	if every <= 0 {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A failed window is dropped by design (see engine.Flush); log it so the
			// resulting gap in the charts has an explanation somewhere.
			if err := e.Flush(); err != nil {
				log.Warn("could not flush usage statistics", "error", err)
			}
		}
	}
}

func publishHealth(ctx context.Context, events *obs.Bus) {
	publish := func() {
		events.Publish(obs.Event{Kind: obs.EventHealth, Status: "ok", Detail: "ready"})
	}
	publish()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publish()
		}
	}
}

// Close shuts subsystems down in reverse dependency order, flushing queued writes.
func (a *App) Close() error {
	var errs []error
	if a.Jobs != nil {
		a.Jobs.Close()
	}
	if a.cancel != nil {
		a.cancel()
	}
	if a.Maintenance != nil {
		a.Maintenance.Wait()
	}
	if a.Pool != nil {
		a.Pool.CloseIdleConnections()
	}
	if a.Engine != nil {
		if err := a.Engine.Flush(); err != nil {
			errs = append(errs, fmt.Errorf("engine stats: %w", err))
		}
	}
	if a.Catalog != nil {
		if err := a.Catalog.Close(); err != nil {
			errs = append(errs, fmt.Errorf("catalog: %w", err))
		}
	}
	if a.Control != nil {
		if err := a.Control.Close(); err != nil {
			errs = append(errs, fmt.Errorf("control: %w", err))
		}
	}
	return errors.Join(errs...)
}

// refreshStorageMetrics publishes the current store size. Cheap enough at startup;
// the maintenance scheduler refreshes it periodically once it exists.
func (a *App) refreshStorageMetrics() {
	count, bytes, err := a.Blobs.Usage()
	if err != nil {
		a.Log.Warn("could not measure the blob store", "error", err)
		return
	}
	a.Metrics.BlobCount.Set(float64(count))
	a.Metrics.BlobStoreBytes.Set(float64(bytes))
}
