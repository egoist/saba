package engine

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/runtime"
	"github.com/egoist/saba/internal/runtime/runtimetest"
	"github.com/egoist/saba/internal/store"
)

// fakeNodes is a cluster of in-process runtimes. Their containers listen on
// the local loopback already, so a "tunnel" only needs to be recorded.
type fakeNodes struct {
	mu       sync.Mutex
	rts      map[string]*runtimetest.Fake
	forwards map[string]bool
}

func (f *fakeNodes) Runtime(node string) runtime.Runtime {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rts[node] == nil {
		f.rts[node] = runtimetest.New()
	}
	return f.rts[node]
}

func (f *fakeNodes) key(node string, port int) string { return node + ":" + strconv.Itoa(port) }

func itoa(n int) string { return strconv.Itoa(n) }

func (f *fakeNodes) Forward(_ context.Context, node string, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forwards[f.key(node, port)] = true
	return nil
}

func (f *fakeNodes) Unforward(_ context.Context, node string, port int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.forwards, f.key(node, port))
}

func (f *fakeNodes) open() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.forwards)
}

func withNodes(t *testing.T, h *harness, names ...string) *fakeNodes {
	t.Helper()
	fn := &fakeNodes{rts: map[string]*runtimetest.Fake{}, forwards: map[string]bool{}}
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
	if nodes.open() != 2 {
		t.Fatalf("each remote replica needs a tunnel, have %d", nodes.open())
	}
	for i := 0; i < 8; i++ {
		if code, body := h.get("web.test", "/"); code != 200 || body != "ok v1" {
			t.Fatalf("request %d: %d %q", i, code, body)
		}
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
	if nodes.open() != 2 {
		t.Fatalf("tunnels of the old version must be closed, have %d", nodes.open())
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
	if len(w1.Names()) != 0 || nodes.open() != 0 || w1.HasImage(imageTag("web", 1)) {
		t.Fatalf("destroy left things on w1: containers %v, tunnels %d", w1.Names(), nodes.open())
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

// In direct mode the primary reaches replicas on the node's private address;
// no tunnel is opened.
func TestDirectNodeBypassesTheTunnel(t *testing.T) {
	h := newHarness(t)
	nodes := withNodes(t, h, "w1")
	if err := h.engine.Store.Update(func(st *store.State) error {
		st.Nodes["w1"].Address, st.Nodes["w1"].Direct = "127.0.0.2", true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	req := image("ok:1")
	req.Config = map[string]any{"replicas": 4, "nodes": []any{"local", "w1"}}
	res := h.mustDeploy("web", req)
	for _, r := range res.Deployment.Replicas {
		wantAddr := ""
		if r.Node == "w1" {
			wantAddr = "127.0.0.2"
		}
		if r.Addr != wantAddr {
			t.Fatalf("replica %d on %q is published on %q, want %q", r.Index, r.Node, r.Addr, wantAddr)
		}
	}
	if nodes.open() != 0 {
		t.Fatalf("direct mode must not open tunnels, have %d", nodes.open())
	}
	for i := 0; i < 8; i++ {
		if code, body := h.get("web.test", "/"); code != 200 || body != "ok v1" {
			t.Fatalf("request %d: %d %q", i, code, body)
		}
	}
	// Switching the node back to the tunnel takes effect on the next rollout.
	if err := h.engine.Store.Update(func(st *store.State) error { st.Nodes["w1"].Direct = false; return nil }); err != nil {
		t.Fatal(err)
	}
	h.mustDeploy("web", api.DeployRequest{Source: api.Source{Type: "current"}})
	if nodes.open() != 2 {
		t.Fatalf("after switching to tunnel mode each remote replica needs a tunnel, have %d", nodes.open())
	}
	if code, _ := h.get("web.test", "/"); code != 200 {
		t.Fatalf("after switching modes: %d", code)
	}
}
