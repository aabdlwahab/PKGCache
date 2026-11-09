package dockerfront

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// Listen opens the socket file the front answers on.
//
// A file, never a port: this is the daemon's API, which is root on the machine to anyone
// who can reach it, and a file has an owner and a mode. It is made 0660, as the daemon's
// own socket is, so the group is whoever runs the front — docker, for a service. A file
// left by a front that did not shut down is replaced; one somebody still answers on is not.
func Listen(ctx context.Context, path string) (net.Listener, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		probe, cancel := context.WithTimeout(ctx, time.Second)
		var dialer net.Dialer
		conn, err := dialer.DialContext(probe, "unix", path)
		cancel()
		if err == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("something is already answering on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove the stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

// Dialer returns how to reach the daemon named by host, which is written the way
// DOCKER_HOST is: unix:///var/run/docker.sock, a bare socket path, or tcp://host:port.
// A daemon behind TLS or ssh:// is not one the front can reach.
func Dialer(host string) (dial func(ctx context.Context) (net.Conn, error), err error) {
	network, address := "unix", host
	switch {
	case strings.HasPrefix(host, "unix://"):
		address = strings.TrimPrefix(host, "unix://")
	case strings.HasPrefix(host, "tcp://"):
		network, address = "tcp", strings.TrimPrefix(host, "tcp://")
	case strings.HasPrefix(host, "/"):
	default:
		return nil, fmt.Errorf("docker host %q: only unix:// and tcp:// daemons can be reached", host)
	}
	if address == "" {
		return nil, fmt.Errorf("docker host %q names no address", host)
	}
	var dialer net.Dialer
	return func(ctx context.Context) (net.Conn, error) {
		return dialer.DialContext(ctx, network, address)
	}, nil
}

// SocketPath is the file a unix:// or bare-path docker host names, or "" for any other.
func SocketPath(host string) string {
	switch {
	case strings.HasPrefix(host, "unix://"):
		return strings.TrimPrefix(host, "unix://")
	case strings.HasPrefix(host, "/"):
		return host
	}
	return ""
}
