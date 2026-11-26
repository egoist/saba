package cluster

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRegistry plays the daemon's role in authorizing agents.
type fakeRegistry struct {
	mu     sync.Mutex
	tokens map[string]string // node → token
	pinned map[string]string // node → key fingerprint
}

func (r *fakeRegistry) authorize(h Hello, fp string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.pinned[h.Node]; ok {
		if p != fp {
			return errors.New("key mismatch")
		}
		return nil
	}
	if t, ok := r.tokens[h.Node]; ok && t == h.Token && t != "" {
		r.pinned[h.Node] = fp
		delete(r.tokens, h.Node)
		return nil
	}
	return errors.New("unknown node or invalid token")
}

type testCluster struct {
	t        testing.TB
	mgr      *Manager
	addr     string
	reg      *fakeRegistry
	fp       string
	listener net.Listener
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func shortDir(t testing.TB) string {
	t.Helper()
	d, err := os.MkdirTemp("", "sc") // unix socket paths must stay short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func newPrimary(t testing.TB) *testCluster {
	t.Helper()
	dir := shortDir(t)
	id, err := LoadIdentity(filepath.Join(dir, "id"), "primary")
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := NewManager(id, filepath.Join(dir, "n"), "v9.9.9", quiet())
	if err != nil {
		t.Fatal(err)
	}
	reg := &fakeRegistry{tokens: map[string]string{}, pinned: map[string]string{}}
	mgr.Authorize = reg.authorize
	l, err := tls.Listen("tcp", "127.0.0.1:0", mgr.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go mgr.Accept(c.(*tls.Conn))
		}
	}()
	t.Cleanup(func() { l.Close(); mgr.Close() })
	return &testCluster{t: t, mgr: mgr, addr: l.Addr().String(), reg: reg, fp: id.Fingerprint, listener: l}
}

// fakeDocker answers on a unix socket like the Docker API would.
func fakeDocker(t testing.TB, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "docker.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return path
}

func (c *testCluster) startAgent(ctx context.Context, node, token, primary, fp string) (*Agent, chan error) {
	c.t.Helper()
	dir := shortDir(c.t)
	cfg := &AgentConfig{Node: node, Primary: primary, Fingerprint: fp, Token: token}
	if err := cfg.Save(filepath.Join(dir, "agent.json")); err != nil {
		c.t.Fatal(err)
	}
	a := &Agent{
		ConfigPath: filepath.Join(dir, "agent.json"), DataDir: filepath.Join(dir, "data"),
		DockerSocket: fakeDocker(c.t, dir), Version: "v9.9.9", Log: quiet(),
		DockerVersion:     func(context.Context) string { return "27.0.0" },
		HeartbeatInterval: 50 * time.Millisecond,
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	return a, done
}

func (c *testCluster) waitConnected(node string, want bool) Status {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := c.mgr.Status()[node]; ok && st.Connected == want {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("node %s connected=%v never happened: %+v", node, want, c.mgr.Status()[node])
	return Status{}
}

func TestJoinStreamsAndReconnect(t *testing.T) {
	c := newPrimary(t)
	c.reg.tokens["w1"] = "secret-token"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent, done := c.startAgent(ctx, "w1", "secret-token", c.addr, c.fp)

	st := c.waitConnected("w1", true)
	if st.Hello.Version != "v9.9.9" || st.Hello.Docker != "27.0.0" || !strings.Contains(st.Hello.Platform, "/") {
		t.Fatalf("hello: %+v", st.Hello)
	}
	// The token was spent: removed from the node's config, pinned on the primary.
	cfg, err := LoadAgentConfig(agent.ConfigPath)
	if err != nil || cfg.Token != "" {
		t.Fatalf("token should be removed after joining: %+v %v", cfg, err)
	}

	// Docker API through the node's socket on the primary.
	docker := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", strings.TrimPrefix(c.mgr.DockerHost("w1"), "unix://"))
	}}}
	resp, err := docker.Get("http://docker/v1.45/version")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != `{"path":"/v1.45/version"}` {
		t.Fatalf("docker through the agent: %q", body)
	}

	// A replica port on the node's loopback, through Dial.
	replica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(w, r.Body) // echo
	}))
	defer replica.Close()
	port, _ := strconv.Atoi(replica.URL[strings.LastIndex(replica.URL, ":")+1:])
	web := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return c.mgr.Dial(ctx, "w1", port)
	}}}
	big := strings.Repeat("0123456789", 300_000) // 3 MB both ways
	resp, err = web.Post("http://replica/", "text/plain", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	echo, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(echo) != len(big) || sha256.Sum256(echo) != sha256.Sum256([]byte(big)) {
		t.Fatalf("echo through the tunnel: %d bytes", len(echo))
	}

	// A closed port is a dial error, so the proxy can fail over.
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	var op *net.OpError
	if _, err := c.mgr.Dial(ctx, "w1", closedPort); !errors.As(err, &op) || op.Op != "dial" {
		t.Fatalf("dialing a closed port: %v", err)
	}
	if _, err := c.mgr.Dial(ctx, "nope", port); !errors.As(err, &op) {
		t.Fatalf("dialing an unknown node: %v", err)
	}

	// Heartbeats deliver host stats.
	deadline := time.Now().Add(3 * time.Second)
	for c.mgr.Status()["w1"].Stats.CPUs == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s := c.mgr.Status()["w1"].Stats; s.CPUs == 0 {
		t.Fatalf("no stats: %+v", s)
	}

	// The primary restarts (its listener goes away and comes back on the
	// same address): the agent reconnects by itself with its pinned key.
	c.listener.Close()
	c.mgr.Close()
	c.waitConnected("w1", false)
	l, err := tls.Listen("tcp", c.addr, c.mgr.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go c.mgr.Accept(conn.(*tls.Conn))
		}
	}()
	c.waitConnected("w1", true)

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("agent exit: %v", err)
	}
}

func TestAgentsAreAuthenticated(t *testing.T) {
	c := newPrimary(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A wrong token is refused.
	c.reg.tokens["w2"] = "right"
	c.startAgent(ctx, "w2", "wrong", c.addr, c.fp)
	time.Sleep(300 * time.Millisecond)
	if c.mgr.Status()["w2"].Connected {
		t.Fatal("an agent with a wrong token connected")
	}

	// An agent that does not pin the primary's key refuses to talk to it.
	other, _ := LoadIdentity(shortDir(t), "impostor")
	c.reg.tokens["w3"] = "t3"
	c.startAgent(ctx, "w3", "t3", c.addr, other.Fingerprint)
	time.Sleep(300 * time.Millisecond)
	if c.mgr.Status()["w3"].Connected {
		t.Fatal("an agent connected to a primary whose key it did not pin")
	}
	if _, stillThere := c.reg.tokens["w3"]; !stillThere {
		t.Fatal("the token must not be spent when the agent rejected the primary")
	}

	// A second agent with another key cannot take over a joined node.
	c.reg.tokens["w4"] = "t4"
	c.startAgent(ctx, "w4", "t4", c.addr, c.fp)
	c.waitConnected("w4", true)
	c.startAgent(ctx, "w4", "t4", c.addr, c.fp)
	time.Sleep(300 * time.Millisecond)
	st := c.mgr.Status()["w4"]
	if !st.Connected {
		t.Fatal("the legitimate agent was displaced")
	}
}

func TestJoinCode(t *testing.T) {
	j := JoinInfo{Node: "w1", Primary: "203.0.113.10:443", Fingerprint: "sha256:abc", Token: "tok"}
	got, err := ParseJoin(j.String())
	if err != nil || got != j {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for _, bad := range []string{"", "hello", "saba-join-!!!", "saba-join-e30"} {
		if _, err := ParseJoin(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestSelfUpgrade(t *testing.T) {
	good := "#!/bin/sh\necho saba v2.0.0\n"
	sum := sha256.Sum256([]byte(good))
	asset := "saba-" + osArch()
	files := map[string]string{
		"/download/v2.0.0/" + asset:      good,
		"/download/v2.0.0/checksums.txt": hex.EncodeToString(sum[:]) + "  " + asset + "\n",
		"/download/v3.0.0/" + asset:      good,
		"/download/v3.0.0/checksums.txt": strings.Repeat("0", 64) + "  " + asset + "\n",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := files[r.URL.Path]; ok {
			io.WriteString(w, b)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	if err := SelfUpgrade(context.Background(), srv.URL, "dev"); err == nil {
		t.Fatal("a development build must not be installed automatically")
	}
	if err := SelfUpgrade(context.Background(), srv.URL, "v3.0.0"); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("a checksum mismatch must refuse: %v", err)
	}
	if err := SelfUpgrade(context.Background(), srv.URL, "v4.0.0"); err == nil {
		t.Fatal("a missing release must refuse")
	}
	// The success path would replace the test binary itself, so it is
	// exercised end to end with a real release instead.
}

// BenchmarkRequest compares HTTP requests to a replica made directly with
// requests made through an agent connection, both over loopback.
func BenchmarkRequest(b *testing.B) {
	replica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer replica.Close()
	port, _ := strconv.Atoi(replica.URL[strings.LastIndex(replica.URL, ":")+1:])
	c := newPrimary(b)
	c.reg.tokens["w1"] = "tok"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.startAgent(ctx, "w1", "tok", c.addr, c.fp)
	for i := 0; i < 300 && !c.mgr.Status()["w1"].Connected; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	run := func(b *testing.B, dial func(ctx context.Context, network, addr string) (net.Conn, error)) {
		client := &http.Client{Transport: &http.Transport{DialContext: dial, MaxIdleConnsPerHost: 64}}
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				resp, err := client.Get("http://replica/")
				if err != nil {
					b.Error(err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		})
	}
	b.Run("direct", func(b *testing.B) {
		run(b, func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, replica.Listener.Addr().String())
		})
	})
	b.Run("agent", func(b *testing.B) {
		run(b, func(ctx context.Context, _, _ string) (net.Conn, error) { return c.mgr.Dial(ctx, "w1", port) })
	})
}
