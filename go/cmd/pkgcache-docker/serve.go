package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aabdlwahab/PKGCache/internal/dockerfront"
)

// runServe answers Docker's API on a socket of its own, for programs that talk to the
// daemon rather than run docker. Pulls arriving there go through the cache; everything
// else reaches the daemon untouched. See internal/dockerfront.
func runServe(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("pkgcache-docker serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	socket := fs.String("socket", "", "socket file to answer Docker's API on")
	registry := fs.String("registry", "",
		"the cache as the Docker daemon dials it, e.g. 127.0.0.1:41999 for a pkgreg-bridge")
	project := fs.String("project", "", "project every pull is made in (default: global)")
	// Not read from DOCKER_HOST. The usual way to use this is to point DOCKER_HOST at it,
	// and a front that took its daemon from there would forward every request to itself.
	daemonHost := fs.String("docker-host", "unix:///var/run/docker.sock",
		"the daemon everything is forwarded to: unix:// or tcp://")
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), `pkgcache-docker serve — Docker's API, with pulls served from a cache

usage: pkgcache-docker serve -socket PATH -registry HOST:PORT [-project NAME]

For programs that talk to the Docker daemon instead of running docker: a CI runner's
Docker executor, compose, anything built on an SDK. Point them at -socket instead of the
daemon's own. A pull of local/app:1.0 or python:3.12 is fetched through the cache and
tagged with the name that was asked for; every other request reaches the daemon as sent.

  pkgreg-bridge -server https://cache.internal:8443 -ca-sha256 <fingerprint> &
  pkgcache-docker serve -socket /run/pkgcache-docker.sock -registry 127.0.0.1:41999
  DOCKER_HOST=unix:///run/pkgcache-docker.sock docker pull local/app:1.0

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *socket == "" || *registry == "" || fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	dial, err := dockerfront.Dialer(*daemonHost)
	if err != nil {
		return fail(err)
	}
	if daemonSocket := dockerfront.SocketPath(*daemonHost); daemonSocket != "" &&
		filepath.Clean(daemonSocket) == filepath.Clean(*socket) {
		return fail(fmt.Errorf("-socket is the daemon's own socket, %s: the front would "+
			"forward every request to itself", *socket))
	}

	listener, err := dockerfront.Listen(ctx, *socket)
	if err != nil {
		return fail(err)
	}
	front := dockerfront.New(dockerfront.Options{
		Registry: *registry, Project: *project, Dial: dial, Notes: os.Stderr,
	})
	server := &http.Server{Handler: front, ReadHeaderTimeout: 30 * time.Second}
	errs := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	shown := *project
	if shown == "" {
		shown = "global"
	}
	fmt.Fprintf(os.Stderr, "pkgcache-docker: Docker's API on unix://%s -> %s, pulling "+
		"through %s (project %s)\n", *socket, *daemonHost, *registry, shown)
	warnUnreachable(ctx, *registry)

	select {
	case err := <-errs:
		return fail(err)
	case <-ctx.Done():
	}
	// ctx is done by now; the grace period needs one of its own.
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	// A follow or a wait can outlast any reasonable grace period; those are cut, and the
	// client sees its daemon go away, which is what happened.
	if err := server.Shutdown(shutdown); err != nil {
		_ = server.Close()
	}
	return 0
}

// warnUnreachable says so at startup when the cache does not answer, which is the mistake
// most likely to be made here: every pull would then fail, each with the same message, and
// none of them would say it was the -registry flag. It does not refuse to start — a bridge
// started by the same unit may simply not be up yet.
func warnUnreachable(ctx context.Context, registry string) {
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(probe, http.MethodGet, "http://"+registry+"/v2/", http.NoBody)
	if err != nil {
		return
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pkgcache-docker: warning: the cache at %s does not answer "+
			"yet (%v); pulls will fail until it does\n", registry, err)
		return
	}
	_ = response.Body.Close()
}
