package daemon

import (
	"context"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/store"
)

// connectNodes re-establishes the connections to all registered nodes.
func (d *Daemon) connectNodes(ctx context.Context) {
	st := d.store.Snapshot()
	for _, name := range sortedKeys(st.Nodes) {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := d.cluster.Add(cctx, *st.Nodes[name]); err != nil {
			d.log.Warn("node is unreachable; its replicas are out of rotation until it is back", "node", name, "err", err)
		} else {
			d.log.Info("connected to node", "node", name)
		}
		cancel()
	}
}

func sortedKeys(m map[string]*api.Node) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (d *Daemon) handleNodeKey(w http.ResponseWriter, r *http.Request) error {
	key, err := d.cluster.PublicKey()
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]string{"public_key": key})
	return nil
}

func (d *Daemon) handleNodes(w http.ResponseWriter, r *http.Request) error {
	st := d.store.Snapshot()
	counts := map[string]int{}
	for _, a := range st.Apps {
		if cur := a.Current(); cur != nil {
			for _, rep := range cur.Replicas {
				n := rep.Node
				if n == "" {
					n = api.LocalNode
				}
				counts[n]++
			}
		}
	}
	docker, _ := d.rt.Version(r.Context())
	nodes := []api.Node{{Name: api.LocalNode, Address: st.Server.Address, Status: "connected", Docker: docker, Replicas: counts[api.LocalNode]}}
	status := d.cluster.Status()
	for _, name := range sortedKeys(st.Nodes) {
		n := *st.Nodes[name]
		n.HostKeys = nil
		n.Replicas = counts[name]
		n.Status = "unreachable"
		if s, ok := status[name]; ok {
			n.Error, n.Docker = s.Error, s.Docker
			if s.Up {
				n.Status = "connected"
			}
		}
		nodes = append(nodes, n)
	}
	writeJSON(w, http.StatusOK, nodes)
	return nil
}

func (d *Daemon) handleNodeAdd(w http.ResponseWriter, r *http.Request) error {
	var n api.Node
	if err := readJSON(r, &n); err != nil {
		return err
	}
	if err := api.ValidateName(n.Name); err != nil {
		return err
	}
	if n.Name == api.LocalNode {
		return api.Errorf(api.CodeInvalid, "%q is the name of the primary server itself", api.LocalNode).
			WithHint("Pick another name, e.g. worker1.")
	}
	n.SSH = strings.TrimSpace(n.SSH)
	if n.SSH == "" || strings.HasPrefix(n.SSH, "-") {
		return api.Errorf(api.CodeInvalid, "an SSH destination is required").WithHint("Example: `saba node add worker1 root@10.0.0.2`.")
	}
	n.Address = strings.TrimSpace(n.Address)
	if n.Direct {
		ip := net.ParseIP(n.Address)
		cgnat := ip != nil && ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64
		switch {
		case ip == nil:
			return api.Errorf(api.CodeInvalid, "--direct needs the node's private IP address").
				WithHint("Example: `saba node add %s %s --address 10.0.0.2 --direct`.", n.Name, n.SSH)
		case !ip.IsPrivate() && !ip.IsLoopback() && !cgnat:
			// Direct traffic is plain HTTP and the replica ports are open on that address.
			return api.Errorf(api.CodeInvalid, "%s is a public address; direct mode is only safe on a private network", n.Address).
				WithHint("Use the node's private address (10.x, 172.16-31.x, 192.168.x, or a 100.64.x VPN address), or leave --direct out to keep the encrypted SSH tunnel.")
		}
	}
	n.CreatedAt = time.Now().UTC()
	n.Status, n.Error, n.Docker, n.Replicas = "", "", "", 0

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Minute)
	defer cancel()
	_, existed := d.store.Snapshot().Nodes[n.Name]
	if err := d.cluster.Add(ctx, n); err != nil {
		if !existed {
			d.cluster.Remove(ctx, n.Name)
		}
		key, _ := d.cluster.PublicKey()
		e := api.Errorf(api.CodeUnreachable, "%v", err).WithDetail("public_key", key)
		if strings.Contains(err.Error(), "over SSH") {
			return e.WithHint("The primary must be able to reach %s over SSH with its own key. Authorize the key printed by `saba node key` in the node's ~/.ssh/authorized_keys.", n.SSH)
		}
		return e.WithHint("Fix that on the node, then run `saba node add` again.")
	}
	if err := d.cluster.Runtime(n.Name).EnsureNetwork(ctx, "saba"); err != nil {
		return api.Errorf(api.CodeRuntime, "cannot prepare Docker on node %q: %v", n.Name, err)
	}
	if err := d.store.Update(func(st *store.State) error {
		if st.Nodes == nil {
			st.Nodes = map[string]*api.Node{}
		}
		st.Nodes[n.Name] = &n
		return nil
	}); err != nil {
		return err
	}
	out := n
	out.HostKeys = nil
	out.Status = "connected"
	out.Docker = d.cluster.Status()[n.Name].Docker
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (d *Daemon) handleNodeRemove(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("node")
	st := d.store.Snapshot()
	if st.Nodes[name] == nil {
		return api.Errorf(api.CodeNotFound, "node %q does not exist", name).WithHint("List nodes with `saba node list`.")
	}
	for _, appName := range st.AppNames() {
		a := st.Apps[appName]
		uses := false
		for _, n := range a.Config.Nodes {
			uses = uses || n == name
		}
		if cur := a.Current(); cur != nil {
			for _, rep := range cur.Replicas {
				uses = uses || rep.Node == name
			}
		}
		if uses {
			return api.Errorf(api.CodeConflict, "node %q still runs app %q", name, appName).
				WithHint("Move the app off the node first, e.g. `saba deploy -a %s --node local`, or destroy it.", appName)
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Minute)
	defer cancel()
	d.cluster.Remove(ctx, name)
	if err := d.store.Update(func(st *store.State) error {
		delete(st.Nodes, name)
		return nil
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": name, "removed": true})
	return nil
}
