// Package cluster connects the primary to the other servers of a saba
// cluster. Each node is reached through one persistent, multiplexed SSH
// connection that carries both the node's Docker API (forwarded to a local
// unix socket) and the traffic to its replicas (per-port forwards). Nodes
// therefore need nothing but SSH and Docker, and nothing travels unencrypted.
package cluster

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/runtime"
)

type Manager struct {
	dir     string // key and known_hosts
	sockDir string // control and docker sockets; must be a short path
	log     *slog.Logger

	mu    sync.Mutex
	links map[string]*link
}

type link struct {
	node api.Node
	rt   *runtime.Docker

	mu       sync.Mutex
	forwards map[int]bool
	up       bool
	lastErr  string
	docker   string
}

// New creates a manager. dir holds the cluster key; sockDir holds sockets.
func New(dir, sockDir string, log *slog.Logger) (*Manager, error) {
	for _, d := range []string{dir, sockDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	return &Manager{dir: dir, sockDir: sockDir, log: log, links: map[string]*link{}}, nil
}

func (m *Manager) keyPath() string     { return filepath.Join(m.dir, "id_ed25519") }
func (m *Manager) knownHosts() string  { return filepath.Join(m.dir, "known_hosts") }
func (m *Manager) ctl(n string) string { return filepath.Join(m.sockDir, n+".ctl") }
func (m *Manager) dockerSock(n string) string {
	return filepath.Join(m.sockDir, n+".sock")
}
func alias(n string) string { return "saba-node-" + n }

// PublicKey returns the primary's cluster key in authorized_keys format,
// generating the key pair on first use.
func (m *Manager) PublicKey() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, err := os.ReadFile(m.keyPath() + ".pub"); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	host, _ := os.Hostname()
	block, err := ssh.MarshalPrivateKey(priv, "saba@"+host)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(m.keyPath(), pem.EncodeToMemory(block), 0o600); err != nil {
		return "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " saba@" + host
	if err := os.WriteFile(m.keyPath()+".pub", []byte(line+"\n"), 0o644); err != nil {
		return "", err
	}
	return line, nil
}

// destination splits an SSH target into the destination and extra arguments.
func destination(target string) (dest string, args []string) {
	dest = strings.TrimPrefix(target, "ssh://")
	if strings.HasPrefix(target, "ssh://") {
		if i := strings.LastIndex(dest, ":"); i > strings.LastIndex(dest, "]") && i > strings.LastIndex(dest, "@") {
			args = append(args, "-p", dest[i+1:])
			dest = dest[:i]
		}
	}
	return dest, args
}

// sshArgs returns the options shared by every ssh invocation for a node.
func (m *Manager) sshArgs(n api.Node) []string {
	strict := "yes"
	if len(n.HostKeys) == 0 {
		strict = "accept-new" // not pinned by the client: trust on first use
	}
	_, extra := destination(n.SSH)
	return append([]string{
		"-S", m.ctl(n.Name),
		"-i", m.keyPath(),
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "UserKnownHostsFile=" + m.knownHosts(),
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "HostKeyAlias=" + alias(n.Name),
		"-o", "StrictHostKeyChecking=" + strict,
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=3",
		"-o", "ServerAliveCountMax=2",
	}, extra...)
}

// ssh runs ssh with extra options for a node (control commands, the master).
func (m *Manager) ssh(ctx context.Context, n api.Node, args ...string) (string, error) {
	return m.exec(ctx, n, args, "")
}

// run executes a command on the node through the existing connection.
func (m *Manager) run(ctx context.Context, n api.Node, command string) (string, error) {
	return m.exec(ctx, n, nil, command)
}

func (m *Manager) exec(ctx context.Context, n api.Node, args []string, command string) (string, error) {
	dest, _ := destination(n.SSH)
	full := append(m.sshArgs(n), args...)
	full = append(full, dest)
	if command != "" {
		full = append(full, command)
	}
	cmd := exec.CommandContext(ctx, "ssh", full...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// The backgrounded master must not keep us waiting on inherited pipes.
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		msg := strings.TrimSpace(out.String())
		if msg == "" {
			msg = err.Error()
		}
		lines := strings.Split(msg, "\n")
		return out.String(), errors.New(strings.TrimSpace(lines[len(lines)-1]))
	}
	return out.String(), nil
}

// pin rewrites the node's entries in known_hosts.
func (m *Manager) pin(n api.Node) error {
	b, _ := os.ReadFile(m.knownHosts())
	var keep []string
	for _, line := range strings.Split(string(b), "\n") {
		if line != "" && !strings.HasPrefix(line, alias(n.Name)+" ") {
			keep = append(keep, line)
		}
	}
	for _, k := range n.HostKeys {
		if f := strings.Fields(k); len(f) >= 2 {
			keep = append(keep, alias(n.Name)+" "+f[0]+" "+f[1])
		}
	}
	content := ""
	if len(keep) > 0 {
		content = strings.Join(keep, "\n") + "\n"
	}
	return os.WriteFile(m.knownHosts(), []byte(content), 0o600)
}

// Add registers a node and connects to it. It fails if the node cannot be
// reached or does not run Docker.
func (m *Manager) Add(ctx context.Context, n api.Node) error {
	if _, err := m.PublicKey(); err != nil {
		return err
	}
	if err := m.pin(n); err != nil {
		return err
	}
	l := &link{node: n, forwards: map[int]bool{}, rt: &runtime.Docker{Bin: "docker", Host: "unix://" + m.dockerSock(n.Name)}}
	m.mu.Lock()
	old := m.links[n.Name]
	m.links[n.Name] = l
	m.mu.Unlock()
	if old != nil {
		_, _ = m.ssh(ctx, old.node, "-O", "exit")
	}
	return m.connect(ctx, l, true)
}

// Remove disconnects and forgets a node.
func (m *Manager) Remove(ctx context.Context, name string) {
	m.mu.Lock()
	l := m.links[name]
	delete(m.links, name)
	m.mu.Unlock()
	if l == nil {
		return
	}
	_, _ = m.ssh(ctx, l.node, "-O", "exit")
	os.Remove(m.dockerSock(name))
	n := l.node
	n.HostKeys = nil
	_ = m.pin(n)
}

// connect brings the node's master connection up. With fresh set, an existing
// master (from a previous daemon process) is replaced.
func (m *Manager) connect(ctx context.Context, l *link, fresh bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.node
	if fresh {
		_, _ = m.ssh(ctx, n, "-O", "exit")
	} else if _, err := m.ssh(ctx, n, "-O", "check"); err == nil {
		l.up = true
		return nil
	}
	os.Remove(m.dockerSock(n.Name))
	os.Remove(m.ctl(n.Name))
	_, err := m.ssh(ctx, n,
		"-M", "-f", "-N",
		"-o", "ControlPersist=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "StreamLocalBindUnlink=yes",
		"-L", m.dockerSock(n.Name)+":/var/run/docker.sock",
	)
	if err != nil {
		l.up, l.lastErr = false, "ssh: "+err.Error()
		return fmt.Errorf("cannot connect to node %q over SSH: %v", n.Name, err)
	}
	version, err := l.rt.Version(ctx)
	if err != nil {
		// Tell a node without usable Docker apart from one whose sshd refuses
		// forwarding: run docker there directly, without the forwarded socket.
		if _, derr := m.run(ctx, n, "docker version --format '{{.Server.Version}}'"); derr == nil {
			l.up, l.lastErr = false, "ssh forwarding is disabled on the node"
			return fmt.Errorf("node %q refuses SSH forwarding; set `AllowTcpForwarding yes` and `AllowStreamLocalForwarding yes` in its /etc/ssh/sshd_config and restart sshd", n.Name)
		} else {
			l.up, l.lastErr = false, "docker: "+derr.Error()
			return fmt.Errorf("connected to node %q, but Docker is not usable there: %v", n.Name, derr)
		}
	}
	// Replica tunnels live in the master; a new master needs them again.
	for port := range l.forwards {
		if _, err := m.ssh(ctx, n, "-O", "forward", "-L", forwardSpec(port)); err != nil {
			m.log.Warn("cannot restore tunnel", "node", n.Name, "port", port, "err", err)
		}
	}
	l.up, l.lastErr, l.docker = true, "", version
	return nil
}

func forwardSpec(port int) string {
	p := strconv.Itoa(port)
	return "127.0.0.1:" + p + ":127.0.0.1:" + p
}

func (m *Manager) link(name string) *link {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.links[name]
}

// Runtime implements engine.Nodes.
func (m *Manager) Runtime(name string) runtime.Runtime {
	if l := m.link(name); l != nil {
		return l.rt
	}
	// An unknown node: every call fails with a clear connection error.
	return &runtime.Docker{Bin: "docker", Host: "unix://" + filepath.Join(m.sockDir, "unknown-node-"+name)}
}

// DockerHost returns the DOCKER_HOST value that reaches a node from the primary.
func (m *Manager) DockerHost(name string) string { return "unix://" + m.dockerSock(name) }

// Forward implements engine.Nodes.
func (m *Manager) Forward(ctx context.Context, name string, port int) error {
	l := m.link(name)
	if l == nil {
		return fmt.Errorf("unknown node %q", name)
	}
	if err := m.connect(ctx, l, false); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.forwards[port] {
		if _, err := m.ssh(ctx, l.node, "-O", "forward", "-L", forwardSpec(port)); err != nil {
			// The forward may already exist in a master we adopted.
			if c, derr := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second); derr != nil {
				return fmt.Errorf("port forward %d: %v", port, err)
			} else {
				c.Close()
			}
		}
		l.forwards[port] = true
	}
	return nil
}

// Unforward implements engine.Nodes.
func (m *Manager) Unforward(ctx context.Context, name string, port int) {
	l := m.link(name)
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.forwards[port] {
		_, _ = m.ssh(ctx, l.node, "-O", "cancel", "-L", forwardSpec(port))
		delete(l.forwards, port)
	}
}

// Watch keeps the connections alive until ctx ends: a node that dropped off
// is reconnected and its tunnels restored.
func (m *Manager) Watch(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.mu.Lock()
		links := make([]*link, 0, len(m.links))
		for _, l := range m.links {
			links = append(links, l)
		}
		m.mu.Unlock()
		for _, l := range links {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if _, err := m.ssh(cctx, l.node, "-O", "check"); err != nil {
				l.mu.Lock()
				wasUp := l.up
				l.up = false
				l.mu.Unlock()
				if wasUp {
					m.log.Warn("lost connection to node", "node", l.node.Name, "err", err)
				}
				if err := m.connect(cctx, l, true); err == nil {
					m.log.Info("reconnected to node", "node", l.node.Name)
				}
			}
			cancel()
		}
	}
}

// Status describes the connection to each node.
type Status struct {
	Up     bool
	Error  string
	Docker string
}

func (m *Manager) Status() map[string]Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]Status{}
	for name, l := range m.links {
		l.mu.Lock()
		out[name] = Status{Up: l.up, Error: l.lastErr, Docker: l.docker}
		l.mu.Unlock()
	}
	return out
}

// Names returns the connected or known node names, sorted.
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

// Close disconnects from every node.
func (m *Manager) Close(ctx context.Context) {
	m.mu.Lock()
	links := make([]*link, 0, len(m.links))
	for _, l := range m.links {
		links = append(links, l)
	}
	m.mu.Unlock()
	for _, l := range links {
		_, _ = m.ssh(ctx, l.node, "-O", "exit")
		os.Remove(m.dockerSock(l.node.Name))
	}
}
