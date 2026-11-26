package cluster

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/hashicorp/yamux"
)

// AgentConfig is stored on the node, by default in /etc/saba/agent.json.
type AgentConfig struct {
	Node        string `json:"node"`
	Primary     string `json:"primary"`
	Fingerprint string `json:"primary_fingerprint"`
	// Token is the one-time join token; removed after the first connection.
	Token string `json:"token,omitempty"`
}

func LoadAgentConfig(path string) (*AgentConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c AgentConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s is not valid: %w", path, err)
	}
	if c.Node == "" || c.Primary == "" || c.Fingerprint == "" {
		return nil, fmt.Errorf("%s is incomplete", path)
	}
	return &c, nil
}

func (c *AgentConfig) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ConfigFromJoin turns a join code into an agent configuration.
func ConfigFromJoin(j JoinInfo) *AgentConfig {
	return &AgentConfig{Node: j.Node, Primary: j.Primary, Fingerprint: j.Fingerprint, Token: j.Token}
}

// Agent is the node's side of the cluster.
type Agent struct {
	ConfigPath   string
	DataDir      string // holds the agent's key
	DockerSocket string // default /var/run/docker.sock
	Version      string
	Log          *slog.Logger
	// DockerVersion reports the local engine's version.
	DockerVersion func(ctx context.Context) string
	// Upgrade replaces this binary with the given release; Run returns
	// ErrUpgraded afterwards so a supervisor restarts the new binary.
	Upgrade func(ctx context.Context, version string) error
	// HeartbeatInterval defaults to 10s.
	HeartbeatInterval time.Duration
}

// ErrUpgraded is returned by Run after the binary was replaced.
var ErrUpgraded = errors.New("upgraded; restart to run the new version")

// errRefused is a definitive rejection by the primary.
type errRefused struct{ msg string }

func (e errRefused) Error() string { return "the primary refused this node: " + e.msg }

// Run keeps the agent connected until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	if a.DockerSocket == "" {
		a.DockerSocket = "/var/run/docker.sock"
	}
	if a.HeartbeatInterval == 0 {
		a.HeartbeatInterval = 10 * time.Second
	}
	id, err := LoadIdentity(a.DataDir, "agent")
	if err != nil {
		return fmt.Errorf("cannot load the agent's key: %w", err)
	}
	backoff := time.Second
	for {
		start := time.Now()
		err := a.session(ctx, id)
		if errors.Is(err, ErrUpgraded) || ctx.Err() != nil {
			return err
		}
		var refused errRefused
		switch {
		case errors.As(err, &refused):
			a.Log.Error(err.Error())
			backoff = time.Minute
		case err != nil:
			if time.Since(start) > 30*time.Second {
				backoff = time.Second // it was up for a while: reconnect fast
			}
			a.Log.Warn("connection to the primary lost", "err", err, "retry_in", backoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if !errors.As(err, &refused) {
			backoff = min(backoff*2, 30*time.Second)
		}
	}
}

func (a *Agent) session(ctx context.Context, id *Identity) error {
	cfg, err := LoadAgentConfig(a.ConfigPath)
	if err != nil {
		return err
	}
	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second},
		Config: &tls.Config{
			Certificates: []tls.Certificate{id.Cert},
			NextProtos:   []string{ALPN},
			MinVersion:   tls.VersionTLS13,
			// The primary is recognized by its pinned key, not by a CA.
			InsecureSkipVerify:    true,
			VerifyPeerCertificate: pinned(cfg.Fingerprint),
		},
	}
	raw, err := d.DialContext(ctx, "tcp", cfg.Primary)
	if err != nil {
		return err
	}
	conn := raw.(*tls.Conn)
	defer conn.Close()
	if conn.ConnectionState().NegotiatedProtocol != ALPN {
		return fmt.Errorf("%s is not a saba primary (or runs an older version)", cfg.Primary)
	}
	host, _ := os.Hostname()
	hello := Hello{Node: cfg.Node, Version: a.Version, Token: cfg.Token, Hostname: host, Platform: runtime.GOOS + "/" + runtime.GOARCH}
	if a.DockerVersion != nil {
		hello.Docker = a.DockerVersion(ctx)
	}
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	var w Welcome
	if err := writeFrame(conn, hello); err != nil {
		return err
	}
	if err := readFrame(conn, &w); err != nil {
		return err
	}
	if !w.OK {
		return errRefused{w.Error}
	}
	conn.SetDeadline(time.Time{})
	if cfg.Token != "" { // joined: the token is spent
		cfg.Token = ""
		if err := cfg.Save(a.ConfigPath); err != nil {
			a.Log.Warn("cannot update the agent config", "err", err)
		}
	}
	a.Log.Info("connected to the primary", "primary", cfg.Primary, "node", cfg.Node)

	// Keep the same version as the primary.
	if w.Version != "" && w.Version != a.Version && a.Upgrade != nil {
		a.Log.Info("upgrading to the primary's version", "from", a.Version, "to", w.Version)
		if err := a.Upgrade(ctx, w.Version); err != nil {
			a.Log.Error("upgrade failed; staying on this version", "err", err)
		} else {
			return ErrUpgraded
		}
	}

	sess, err := yamux.Server(conn, yamuxConfig())
	if err != nil {
		return err
	}
	defer sess.Close()
	go a.heartbeat(ctx, sess)
	go func() {
		<-ctx.Done()
		sess.Close()
	}()
	for {
		s, err := sess.AcceptStream()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("connection closed")
		}
		go a.serve(s)
	}
}

func (a *Agent) heartbeat(ctx context.Context, sess *yamux.Session) {
	s, err := sess.OpenStream()
	if err != nil {
		return
	}
	defer s.Close()
	t := time.NewTicker(a.HeartbeatInterval)
	defer t.Stop()
	for {
		hb := Heartbeat{Stats: HostStats("/var/lib/docker")}
		if a.DockerVersion != nil {
			hb.Docker = a.DockerVersion(ctx)
		}
		if writeFrame(s, hb) != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-sess.CloseChan():
			return
		case <-t.C:
		}
	}
}

// serve handles one stream opened by the primary.
func (a *Agent) serve(s *yamux.Stream) {
	defer s.Close()
	s.SetDeadline(time.Now().Add(15 * time.Second))
	var req streamRequest
	if readFrame(s, &req) != nil {
		return
	}
	var (
		target net.Conn
		err    error
	)
	switch req.Op {
	case "docker":
		target, err = net.DialTimeout("unix", a.DockerSocket, 5*time.Second)
	case "tcp":
		// Only the loopback interface, where replicas publish their ports.
		if req.Port <= 0 || req.Port > 65535 {
			err = fmt.Errorf("invalid port %d", req.Port)
			break
		}
		target, err = net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(req.Port)), 5*time.Second)
	default:
		err = fmt.Errorf("unknown operation %q", req.Op)
	}
	if err != nil {
		writeFrame(s, streamReply{Error: err.Error()})
		return
	}
	defer target.Close()
	if writeFrame(s, streamReply{}) != nil {
		return
	}
	s.SetDeadline(time.Time{})
	splice(target, s)
}
