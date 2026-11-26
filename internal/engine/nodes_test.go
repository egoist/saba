package engine

import (
	"context"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/runtime"
	"github.com/egoist/saba/internal/runtime/runtimetest"
	"github.com/egoist/saba/internal/store"
)

// fakeNodes is a cluster of in-process runtimes. Their containers listen on
// the local loopback already, so a "tunnel" only needs to be recorded.
type fakeNodes struct {
	mu    sync.Mutex
	rts   map[string]*runtimetest.Fake
	dials map[string]int  // node → connections made through its "agent"
	down  map[string]bool // nodes whose agent is disconnected
}

func (f *fakeNodes) Runtime(node string) runtime.Runtime {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rts[node] == nil {
		f.rts[node] = runtimetest.New()
	}
	return f.rts[node]
}

// Dial stands in for the agent: the fake runtime's containers listen on this
// machine's loopback, whichever "node" they belong to.
func (f *fakeNodes) Dial(ctx context.Context, node string, port int) (net.Conn, error) {
	f.mu.Lock()
	down := f.down[node]
	f.dials[node]++
	f.mu.Unlock()
	if down {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("node " + node + " is not connected")}
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", "127.0.0.1:"+strconv.Itoa(port))
}

func (f *fakeNodes) dialed(node string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials[node]
}

func (f *fakeNodes) setDown(node string, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down[node] = down
}

func itoa(n int) string { return strconv.Itoa(n) }

func withNodes(t *testing.T, h *harness, names ...string) *fakeNodes {
	t.Helper()
	fn := &fakeNodes{rts: map[string]*runtimetest.Fake{}, dials: map[string]int{}, down: map[string]bool{}}
	h.engine.Nodes = fn
	if err := h.engine.Store.Update(func(st *store.State) error {
		st.Nodes = map[string]*api.Node{}
		for _, n := range names {
			st.Nodes[n] = &api.Node{Name: n, SSH: "root@" + n, Address: "10.0.0." + itoa(len(st.Nodes)+2)}
			fn.Runtime(n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, rt := range fn.rts {
			for _, n := range rt.Names() {
				_ = rt.Remove(context.Background(), n)
			}
		}
	})
	return fn
}

func TestReplicasAreSpreadAcrossNodes(t *testing.T) {
	h := newHarness(t)
	nodes := withNodes(t, h, "w1")
	ctx := context.Background()

	req := image("ok:1")
	req.Config = map[string]any{"replicas": 4, "nodes": []any{"local", "w1"}}
	res := h.mustDeploy("web", req)

	where := map[string]int{}
	for _, r := range res.Deployment.Replicas {
		where[r.Node]++
	}
	if where[""] != 2 || where["w1"] != 2 {
		t.Fatalf("4 replicas over 2 nodes should be 2+2, got %v", where)
	}
	w1 := nodes.rts["w1"]
	if got := w1.Names(); len(got) != 2 || len(h.rt.Names()) != 2 {
		t.Fatalf("containers: local %v, w1 %v", h.rt.Names(), got)
	}
	if w1.Loads != 1 || !w1.HasImage(imageTag("web", 1)) {
		t.Fatalf("the image should have been shipped to w1 exactly once (loads: %d)", w1.Loads)
	}
	before := nodes.dialed("w1")
	for i := 0; i < 8; i++ {
		if code, body := h.get("web.test", "/"); code != 200 || body != "ok v1" {
			t.Fatalf("request %d: %d %q", i, code, body)
		}
	}
	if nodes.dialed("w1") == before {
		t.Fatal("requests to replicas on w1 must go through its agent")
	}

	// Status shows which server each container runs on.
	app, _ := h.engine.App(ctx, "web")
	byNode := map[string]int{}
	for _, c := range app.Containers {
		byNode[c.Node]++
	}
	if app.Status != api.AppRunning || byNode["local"] != 2 || byNode["w1"] != 2 {
		t.Fatalf("status %s, containers by node %v", app.Status, byNode)
	}

	// A restart reuses the image already on the node and replaces all replicas.
	h.mustDeploy("web", api.DeployRequest{Source: api.Source{Type: "current"}})
	if w1.Loads != 1 {
		t.Fatalf("restart re-shipped the image (loads: %d)", w1.Loads)
	}
	names := append(h.rt.Names(), w1.Names()...)
	sort.Strings(names)
	if strings.Join(names, ",") != "saba-web-v2-1,saba-web-v2-2,saba-web-v2-3,saba-web-v2-4" {
		t.Fatalf("after the rollout: %v", names)
	}

	// Moving the app entirely to the worker.
	move := api.DeployRequest{Source: api.Source{Type: "current"}, Config: map[string]any{"nodes": []any{"w1"}, "replicas": 2}}
	h.mustDeploy("web", move)
	if len(h.rt.Names()) != 0 || len(w1.Names()) != 2 {
		t.Fatalf("after moving: local %v, w1 %v", h.rt.Names(), w1.Names())
	}
	if code, _ := h.get("web.test", "/"); code != 200 {
		t.Fatalf("app unreachable after moving to w1: %d", code)
	}

	// exec and logs follow the replica to its node.
	var out strings.Builder
	if code, err := h.engine.Exec(ctx, "web", api.ExecRequest{Command: []string{"hostname"}}, nil, &out, &out); err != nil || code != 0 || out.String() != "exec hostname\n" {
		t.Fatalf("exec on a remote replica: %d %v %q", code, err, out.String())
	}

	if _, err := h.engine.Destroy(ctx, "web", true); err != nil {
		t.Fatal(err)
	}
	if len(w1.Names()) != 0 || w1.HasImage(imageTag("web", 1)) {
		t.Fatalf("destroy left things on w1: containers %v", w1.Names())
	}
}

func TestPlacementRules(t *testing.T) {
	h := newHarness(t)
	nodes := withNodes(t, h, "w1", "w2")

	// "*" means every server; fewer replicas than servers uses the first ones.
	all := image("ok:1")
	all.Config = map[string]any{"replicas": 3, "nodes": []any{"*"}}
	res := h.mustDeploy("web", all)
	var got []string
	for _, r := range res.Deployment.Replicas {
		got = append(got, r.Node)
	}
	if strings.Join(got, ",") != ",w1,w2" {
		t.Fatalf("placement over * = %q", got)
	}
	one := image("ok:1")
	one.Config = map[string]any{"replicas": 1, "nodes": []any{"w2", "w1"}}
	h.mustDeploy("solo", one)
	if len(nodes.rts["w2"].Names()) != 2 || len(nodes.rts["w1"].Names()) != 1 {
		t.Fatalf("a single replica should go to the first listed node: w1 %v, w2 %v", nodes.rts["w1"].Names(), nodes.rts["w2"].Names())
	}

	for name, cfg := range map[string]map[string]any{
		"unknown node":    {"nodes": []any{"nope"}},
		"volumes spread":  {"nodes": []any{"local", "w1"}, "volumes": []any{"data:/data"}},
		"volumes on star": {"nodes": []any{"*"}, "volumes": []any{"data:/data"}},
		"bad node name":   {"nodes": []any{"Not Valid"}},
	} {
		req := image("ok:1")
		req.Config = cfg
		_, err := h.deploy("bad", req)
		if code := codeOf(err); code != api.CodeInvalid && code != api.CodeNotFound {
			t.Errorf("%s: expected a validation error, got %v", name, err)
		}
	}
	db := image("ok:1")
	db.Kind = api.KindDatabase
	db.Config = map[string]any{"nodes": []any{"local", "w1"}, "health": map[string]any{"command": "true"}}
	if _, err := h.deploy("db", db); codeOf(err) != api.CodeInvalid {
		t.Errorf("a database on two nodes should be rejected, got %v", err)
	}

	// A pinned app with a volume may live on a worker.
	pinned := image("ok:1")
	pinned.Config = map[string]any{"nodes": []any{"w1"}, "volumes": []any{"data:/data"}}
	h.mustDeploy("files", pinned)
}

func TestCrossNodeNameResolution(t *testing.T) {
	h := newHarness(t)
	withNodes(t, h, "w1")
	if err := h.engine.Store.Update(func(st *store.State) error { st.Server.Address = "10.0.0.1"; return nil }); err != nil {
		t.Fatal(err)
	}
	// A database on the primary that publishes its port, and one that does not.
	for name, publish := range map[string][]any{"main": {"10.0.0.1:5432:5432"}, "private": nil} {
		db := image("ok:1")
		db.Kind = api.KindDatabase
		db.Config = map[string]any{"health": map[string]any{"command": "true"}}
		if publish != nil {
			db.Config["publish"] = publish
		}
		h.mustDeploy(name, db)
	}
	st := h.engine.Store.Snapshot()
	if got := extraHosts(st, "web", "w1"); len(got) != 1 || got[0] != "main:10.0.0.1" {
		t.Fatalf("a container on w1 should resolve the published database by the primary's address, got %v", got)
	}
	if got := extraHosts(st, "web", "local"); len(got) != 0 {
		t.Fatalf("on the same server Docker's DNS is used, got %v", got)
	}
}

// When a node's agent is disconnected, requests go to the replicas elsewhere.
func TestDisconnectedNodeFailsOver(t *testing.T) {
	h := newHarness(t)
	nodes := withNodes(t, h, "w1")
	req := image("ok:1")
	req.Config = map[string]any{"replicas": 2, "nodes": []any{"local", "w1"}}
	h.mustDeploy("web", req)
	nodes.setDown("w1", true)
	for i := 0; i < 10; i++ {
		if code, body := h.get("web.test", "/"); code != 200 || body != "ok v1" {
			t.Fatalf("request %d with w1 down: %d %q", i, code, body)
		}
	}
	nodes.setDown("w1", false)
	before := nodes.dialed("w1")
	time.Sleep(2100 * time.Millisecond) // the proxy skips a failed replica for 2s
	for i := 0; i < 6; i++ {
		h.get("web.test", "/")
	}
	if nodes.dialed("w1") == before {
		t.Fatal("w1 should be back in rotation once its agent reconnects")
	}
}
