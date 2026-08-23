package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/aabdlwahab/PKGCache/internal/config"
	"github.com/aabdlwahab/PKGCache/internal/listener"
)

// ErrTLSDisabled reports that an operation needing a certificate was attempted on a
// process running without TLS — reloading certificates, most often.
var ErrTLSDisabled = errors.New("listeners: TLS is not configured")

type boundServer struct {
	name     string
	server   *http.Server
	listener net.Listener
}

// Runtime is the bound listener set owned by one App.
type Runtime struct {
	app    *App
	bound  []boundServer
	certs  *listener.Certificates
	split  *listener.FirstByteMux
	errors chan error
	addrs  map[string]string
	wg     sync.WaitGroup
	stop   sync.Once
}

// StartListeners binds every configured socket before returning. Readiness becomes
// true only after all binds succeed, so a process never advertises ready while one
// of its required ports is missing.
func (a *App) StartListeners() (*Runtime, error) {
	serverCfg := a.Config.Current().Server
	runtime := &Runtime{
		app: a, errors: make(chan error, 4), addrs: make(map[string]string),
	}
	a.listenersExpected.Store(true)
	a.listenersReady.Store(false)

	if serverCfg.TLS.Enabled() {
		certs, err := listener.LoadCertificates(serverCfg.TLS.CertFile, serverCfg.TLS.KeyFile)
		if err != nil {
			return nil, err
		}
		runtime.certs = certs
	}

	var err error
	if serverCfg.SinglePort {
		err = runtime.bindSingle(serverCfg)
	} else {
		err = runtime.bindExplicit(serverCfg)
	}
	if err != nil {
		runtime.closeBound()
		return nil, err
	}
	for _, bound := range runtime.bound {
		runtime.serve(bound)
	}
	a.listenersReady.Store(true)
	return runtime, nil
}

func (r *Runtime) bindSingle(cfg config.Server) error {
	base := r.app.inherited
	if base == nil {
		bound, err := listenTCP(cfg.UnifiedAddr)
		if err != nil {
			return fmt.Errorf("listeners: bind single port %s: %w", cfg.UnifiedAddr, err)
		}
		base = bound
	}
	r.addrs["single"] = base.Addr().String()
	if r.certs == nil {
		r.bound = append(r.bound, boundServer{
			name: "single", server: NewServer(base.Addr().String(),
				r.app.SinglePortHandler(), cfg.ReadHeaderTimeout),
			listener: base,
		})
		return nil
	}

	r.split = listener.Split(base, cfg.ReadHeaderTimeout)
	tlsConfig := r.certs.TLSConfig()
	tlsServer := NewServer(base.Addr().String(), r.app.UnifiedHandler(), cfg.ReadHeaderTimeout)
	tlsServer.TLSConfig = tlsConfig
	r.bound = append(r.bound,
		boundServer{
			name: "single-tls", server: tlsServer,
			listener: tls.NewListener(r.split.TLS(), tlsConfig),
		},
		boundServer{
			name: "single-plain",
			// Not SinglePortHandler: this half is reached without TLS on a port that
			// has a certificate, so it serves the forward proxy and redirects
			// everything else rather than exposing the console and control API in
			// the clear. See SinglePortPlainHandler.
			server: NewServer(base.Addr().String(), r.app.SinglePortPlainHandler(),
				cfg.ReadHeaderTimeout),
			listener: r.split.Plain(),
		},
	)
	return nil
}

func (r *Runtime) bindExplicit(cfg config.Server) error {
	type binding struct {
		name, address string
		handler       http.Handler
		tls           bool
	}
	// The admin surface carries session logins and the control API, so it gets TLS
	// wherever a certificate exists — the same treatment as the package listener. Only
	// the apt/apk forward proxy stays plain, because the protocol has no TLS form: an
	// http_proxy client speaks cleartext to its proxy by definition.
	bindings := []binding{
		{name: "unified", address: cfg.UnifiedAddr, handler: r.app.UnifiedHandler(), tls: true},
		{name: "proxy", address: cfg.ProxyAddr, handler: r.app.ProxyHandler()},
		{name: "admin", address: cfg.AdminAddr, handler: r.app.AdminHandler(), tls: true},
	}
	for _, spec := range bindings {
		base, err := listenTCP(spec.address)
		if err != nil {
			return fmt.Errorf("listeners: bind %s %s: %w", spec.name, spec.address, err)
		}
		r.addrs[spec.name] = base.Addr().String()
		server := NewServer(base.Addr().String(), spec.handler, cfg.ReadHeaderTimeout)
		var served net.Listener
		served = base
		if spec.tls && r.certs != nil {
			tlsConfig := r.certs.TLSConfig()
			server.TLSConfig = tlsConfig
			served = tls.NewListener(base, tlsConfig)
		}
		r.bound = append(r.bound, boundServer{
			name: spec.name, server: server, listener: served,
		})
	}
	return nil
}

func (r *Runtime) serve(bound boundServer) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		err := bound.server.Serve(bound.listener)
		if err == nil || errors.Is(err, http.ErrServerClosed) ||
			errors.Is(err, net.ErrClosed) {
			return
		}
		r.app.listenersReady.Store(false)
		select {
		case r.errors <- fmt.Errorf("%s listener: %w", bound.name, err):
		default:
		}
	}()
}

// Errors reports unexpected listener failures.
func (r *Runtime) Errors() <-chan error { return r.errors }

// Addresses returns actual bound addresses, including ephemeral ports in tests.
func (r *Runtime) Addresses() map[string]string {
	out := make(map[string]string, len(r.addrs))
	for name, address := range r.addrs {
		out[name] = address
	}
	return out
}

// ReloadTLS atomically installs the certificate files currently on disk.
func (r *Runtime) ReloadTLS() error {
	if r.certs == nil {
		return ErrTLSDisabled
	}
	return r.certs.Reload()
}

// Shutdown first removes readiness, then drains HTTP handlers and detached engine
// fetches within ctx. If the deadline expires, active connections are closed.
func (r *Runtime) Shutdown(ctx context.Context) error {
	var result error
	r.stop.Do(func() {
		r.app.listenersReady.Store(false)
		// Before the servers: an event stream never ends on its own, so Shutdown would
		// wait out the whole grace period for every browser window left open.
		if r.app.API != nil {
			r.app.API.Close()
		}
		var wg sync.WaitGroup
		errs := make(chan error, len(r.bound))
		for _, bound := range r.bound {
			wg.Add(1)
			go func(server *http.Server) {
				defer wg.Done()
				if err := server.Shutdown(ctx); err != nil {
					errs <- err
				}
			}(bound.server)
		}
		wg.Wait()
		close(errs)
		var joined []error
		for err := range errs {
			joined = append(joined, err)
		}
		if r.app.Jobs != nil {
			r.app.Jobs.Close()
		}
		if err := r.app.Engine.Drain(ctx); err != nil {
			joined = append(joined, err)
		}
		if ctx.Err() != nil {
			for _, bound := range r.bound {
				_ = bound.server.Close()
			}
		}
		if r.split != nil {
			_ = r.split.Close()
		}
		r.wg.Wait()
		result = errors.Join(joined...)
	})
	return result
}

func (r *Runtime) closeBound() {
	if r.split != nil {
		_ = r.split.Close()
	}
	for _, bound := range r.bound {
		_ = bound.listener.Close()
	}
}

// listenTCP binds a TCP socket through net.ListenConfig, which is the form that can
// carry socket options if this ever needs them. Binding does not block, so the process
// context is the right one to hand it.
func listenTCP(address string) (net.Listener, error) {
	var listenConfig net.ListenConfig
	return listenConfig.Listen(context.Background(), "tcp", address)
}
