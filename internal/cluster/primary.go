package cluster

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/runtime"
)

// Manager is the primary's side of the cluster.
type Manager struct {
	Identity *Identity
	Version  string
	Log      *slog.Logger
	// Authorize admits an agent. fingerprint identifies the key it connected
	// with. On a first join (valid token) it pins that key.
	Authorize func(h Hello, fingerprint string) error
	// OnConnect runs after an agent connected, e.g. to reconcile the node.
	OnConnect func(node string)

	sockDir string
	mu      sync.Mutex
	links   map[string]*link
}

type link struct {
	node      string
	sess      *yamux.Session
	hello     Hello
	remote    string
	connected bool
	since     time.Time
	lastSeen  time.Time
	lastErr   string
	hb        Heartbeat
	sock      net.Listener
}

func NewManager(id *Identity, sockDir, version string, log *slog.Logger) (*Manager, error) {
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		return nil, err
	}
	return &Manager{Identity: id, Version: version, Log: log, sockDir: sockDir, links: map[string]*link{}}, nil
}

// TLSConfig is the server configuration for agent connections.
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{m.Identity.Cert},
		NextProtos:   []string{ALPN},
		// Any certificate: the key is checked against the node's pinned one.
		ClientAuth: tls.RequireAnyClientCert,
		MinVersion: tls.VersionTLS13,
	}
}

func (m *Manager) link(node string) *link {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.links[node]
	if l == nil {
		l = &link{node: node}
		m.links[node] = l
	}
	return l
}

// Prepare makes a node known: its Docker socket on the primary exists from
// now on, failing fast until the agent connects.
func (m *Manager) Prepare(node string) {
	l := m.link(node)
	m.mu.Lock()
	defer m.mu.Unlock()
	if l.sock != nil {
		return
	}
	path := m.dockerSocket(node)
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		m.Log.Error("cannot create the node's docker socket", "node", node, "err", err)
		return
	}
	os.Chmod(path, 0o600)
	l.sock = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go m.serveDocker(node, c)
		}
	}()
}

func (m *Manager) dockerSocket(node string) string { return filepath.Join(m.sockDir, node+".sock") }

// DockerHost is the DOCKER_HOST value that reaches a node's engine.
func (m *Manager) DockerHost(node string) string { return "unix://" + m.dockerSocket(node) }

// Runtime returns a container runtime that drives the node's Docker engine.
func (m *Manager) Runtime(node string) runtime.Runtime {
	m.Prepare(node)
	return &runtime.Docker{Bin: "docker", Host: m.DockerHost(node)}
}

func (m *Manager) serveDocker(node string, c net.Conn) {
	defer c.Close()
	s, err := m.open(context.Background(), node, streamRequest{Op: "docker"})
	if err != nil {
		return // the docker CLI reports the closed connection
	}
	defer s.Close()
	splice(c, s)
}

// Dial opens a connection to a port on the node's loopback interface, where
// replicas publish their ports. Failures are *net.OpError dial errors, so the
// proxy fails over to other replicas as it does for local ones.
func (m *Manager) Dial(ctx context.Context, node string, port int) (net.Conn, error) {
	s, err := m.open(ctx, node, streamRequest{Op: "tcp", Port: port})
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Addr: nodeAddr(fmt.Sprintf("%s:%d", node, port)), Err: err}
	}
	return s, nil
}

type nodeAddr string

func (a nodeAddr) Network() string { return "tcp" }
func (a nodeAddr) String() string  { return string(a) }

func (m *Manager) open(ctx context.Context, node string, req streamRequest) (net.Conn, error) {
	m.mu.Lock()
	l := m.links[node]
	var sess *yamux.Session
	if l != nil && l.connected {
		sess = l.sess
	}
	m.mu.Unlock()
	if sess == nil {
		return nil, fmt.Errorf("node %s is not connected", node)
	}
	s, err := sess.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", node, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	s.SetDeadline(deadline)
	var reply streamReply
	if err := writeFrame(s, req); err == nil {
		err = readFrame(s, &reply)
	}
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("node %s: %w", node, err)
	}
	if reply.Error != "" {
		s.Close()
		return nil, fmt.Errorf("node %s: %s", node, reply.Error)
	}
	s.SetDeadline(time.Time{})
	return s, nil
}

// Accept serves one agent connection until it ends. The TLS handshake must
// have used TLSConfig.
func (m *Manager) Accept(conn *tls.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	if err := conn.Handshake(); err != nil {
		return
	}
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return
	}
	var h Hello
	if err := readFrame(conn, &h); err != nil {
		return
	}
	if err := m.Authorize(h, Fingerprint(certs[0])); err != nil {
		m.Log.Warn("refused an agent", "node", h.Node, "remote", conn.RemoteAddr().String(), "err", err)
		writeFrame(conn, Welcome{Error: err.Error()})
		return
	}
	if err := writeFrame(conn, Welcome{OK: true, Version: m.Version}); err != nil {
		return
	}
	conn.SetDeadline(time.Time{})
	sess, err := yamux.Client(conn, yamuxConfig())
	if err != nil {
		return
	}
	m.Prepare(h.Node)
	l := m.link(h.Node)
	m.mu.Lock()
	if l.sess != nil {
		l.sess.Close() // a reconnect replaces the old connection
	}
	l.sess, l.hello, l.connected = sess, h, true
	l.remote = conn.RemoteAddr().String()
	l.since, l.lastSeen, l.lastErr = time.Now(), time.Now(), ""
	l.hb.Docker = h.Docker
	m.mu.Unlock()
	m.Log.Info("node connected", "node", h.Node, "remote", l.remote, "version", h.Version)
	if m.OnConnect != nil {
		go m.OnConnect(h.Node)
	}

	// Streams the agent opens carry heartbeats.
	go func() {
		for {
			s, err := sess.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				defer s.Close()
				for {
					var hb Heartbeat
					if readFrame(s, &hb) != nil {
						return
					}
					m.mu.Lock()
					if l.sess == sess {
						l.hb, l.lastSeen = hb, time.Now()
					}
					m.mu.Unlock()
				}
			}()
		}
	}()

	<-sess.CloseChan()
	m.mu.Lock()
	if l.sess == sess {
		l.connected = false
		l.lastErr = "disconnected " + time.Now().UTC().Format(time.RFC3339)
	}
	m.mu.Unlock()
	m.Log.Warn("node disconnected", "node", h.Node)
}

// Remove disconnects and forgets a node.
func (m *Manager) Remove(node string) {
	m.mu.Lock()
	l := m.links[node]
	delete(m.links, node)
	m.mu.Unlock()
	if l == nil {
		return
	}
	if l.sess != nil {
		l.sess.Close()
	}
	if l.sock != nil {
		l.sock.Close()
	}
	os.Remove(m.dockerSocket(node))
}

// Status describes a node's connection.
type Status struct {
	Connected bool
	Error     string
	Remote    string
	Hello     Hello
	Since     time.Time
	LastSeen  time.Time
	Docker    string
	Stats     api.HostStats
}

func (m *Manager) Status() map[string]Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]Status{}
	for name, l := range m.links {
		docker := l.hb.Docker
		if docker == "" {
			docker = l.hello.Docker
		}
		out[name] = Status{Connected: l.connected, Error: l.lastErr, Remote: l.remote, Hello: l.hello,
			Since: l.since, LastSeen: l.lastSeen, Docker: docker, Stats: l.hb.Stats}
	}
	return out
}

// Names returns the known node names, sorted.
func (m *Manager) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.links))
	for n := range m.links {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Close disconnects every node.
func (m *Manager) Close() {
	for _, n := range m.Names() {
		m.mu.Lock()
		l := m.links[n]
		m.mu.Unlock()
		if l != nil && l.sess != nil {
			l.sess.Close()
		}
	}
}

// splice copies both ways until either side is done.
func splice(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	cp := func(dst io.WriteCloser, src io.Reader) {
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}
