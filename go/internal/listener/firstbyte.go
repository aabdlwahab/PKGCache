package listener

import (
	"bufio"
	"errors"
	"net"
	"sync"
	"time"
)

// FirstByteMux separates TLS records (first byte 0x16) from plain HTTP on one
// socket. The consumed byte remains visible through bufferedConn.
type FirstByteMux struct {
	base    net.Listener
	timeout time.Duration
	done    chan struct{}
	once    sync.Once
	wg      sync.WaitGroup

	tlsConns   chan net.Conn
	plainConns chan net.Conn
	tls        *subListener
	plain      *subListener
}

// Split starts classifying connections accepted from base.
func Split(base net.Listener, timeout time.Duration) *FirstByteMux {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	m := &FirstByteMux{
		base: base, timeout: timeout, done: make(chan struct{}),
		tlsConns: make(chan net.Conn, 64), plainConns: make(chan net.Conn, 64),
	}
	m.tls = &subListener{parent: m, conns: m.tlsConns}
	m.plain = &subListener{parent: m, conns: m.plainConns}
	m.wg.Add(1)
	go m.accept()
	return m
}

// TLS returns the TLS-handshake side.
func (m *FirstByteMux) TLS() net.Listener { return m.tls }

// Plain returns the plain-HTTP side.
func (m *FirstByteMux) Plain() net.Listener { return m.plain }

func (m *FirstByteMux) accept() {
	defer m.wg.Done()
	for {
		conn, err := m.base.Accept()
		if err != nil {
			select {
			case <-m.done:
				return
			default:
			}
			// Only a timeout is worth retrying. net.Error.Temporary() is deprecated
			// precisely because "temporary" was never well defined; every other accept
			// failure means this listener is no longer usable.
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			_ = m.Close()
			return
		}
		m.wg.Add(1)
		go m.classify(conn)
	}
}

func (m *FirstByteMux) classify(conn net.Conn) {
	defer m.wg.Done()
	_ = conn.SetReadDeadline(time.Now().Add(m.timeout))
	reader := bufio.NewReaderSize(conn, 4096)
	first, err := reader.Peek(1)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		return
	}
	wrapped := &bufferedConn{Conn: conn, reader: reader}
	target := m.plainConns
	if first[0] == 0x16 {
		target = m.tlsConns
	}
	select {
	case target <- wrapped:
	case <-m.done:
		_ = conn.Close()
	}
}

// Close stops both child listeners and the underlying socket.
func (m *FirstByteMux) Close() error {
	var err error
	m.once.Do(func() {
		close(m.done)
		err = m.base.Close()
	})
	return err
}

type subListener struct {
	parent *FirstByteMux
	conns  chan net.Conn
}

// Accept hands over the next connection the mux classified as belonging to this half.
func (l *subListener) Accept() (net.Conn, error) {
	select {
	case <-l.parent.done:
		return nil, net.ErrClosed
	default:
	}
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.parent.done:
		return nil, net.ErrClosed
	}
}

// Close shuts down the whole mux: both halves share one underlying socket, so closing
// either has to stop accepting for both.
func (l *subListener) Close() error { return l.parent.Close() }

// Addr reports the shared underlying address.
func (l *subListener) Addr() net.Addr { return l.parent.base.Addr() }

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
