package engine

import (
	"context"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/runtime"
	"github.com/egoist/saba/internal/store"
)

// LabelPort records the loopback port a replica is reachable on from the primary.
const LabelPort = "saba.port"

// Nodes gives the engine access to the other servers of a cluster. The
// primary reaches every remote replica on a local port that is forwarded to
// the node, so the proxy and health checks treat all replicas alike.
type Nodes interface {
	// Runtime returns the container runtime of a remote node.
	Runtime(node string) runtime.Runtime
	// Forward makes 127.0.0.1:port on the primary reach 127.0.0.1:port on the node.
	Forward(ctx context.Context, node string, port int) error
	// Unforward undoes Forward.
	Unforward(ctx context.Context, node string, port int)
}

func isLocal(node string) bool { return node == "" || node == api.LocalNode }

// rt returns the runtime of the node a container lives on.
func (e *Engine) rt(node string) runtime.Runtime {
	if isLocal(node) || e.Nodes == nil {
		return e.RT
	}
	return e.Nodes.Runtime(node)
}

// remoteNodes returns the names of all registered remote nodes, sorted.
func (e *Engine) remoteNodes() []string {
	if e.Nodes == nil {
		return nil
	}
	st := e.Store.Snapshot()
	names := make([]string, 0, len(st.Nodes))
	for n := range st.Nodes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// list returns matching containers from the primary and every reachable node.
// An unreachable node is skipped: its containers are dealt with once it is back.
func (e *Engine) list(ctx context.Context, labels map[string]string) ([]runtime.Container, error) {
	out, err := e.RT.List(ctx, labels)
	if err != nil {
		return nil, err
	}
	remotes := e.remoteNodes()
	if len(remotes) == 0 {
		return out, nil
	}
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, node := range remotes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cs, err := e.Nodes.Runtime(node).List(ctx, labels)
			if err != nil {
				e.Log.Warn("cannot list containers on node", "node", node, "err", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, c := range cs {
				c.Node = node
				out = append(out, c)
			}
		}()
	}
	wg.Wait()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// placement resolves an app's `nodes` setting to concrete node names.
func placement(st *store.State, cfg *api.AppConfig) ([]string, error) {
	if len(cfg.Nodes) == 0 {
		return []string{api.LocalNode}, nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, n := range cfg.Nodes {
		switch {
		case n == "*":
			add(api.LocalNode)
			names := make([]string, 0, len(st.Nodes))
			for name := range st.Nodes {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				add(name)
			}
		case n == api.LocalNode:
			add(n)
		default:
			if st.Nodes[n] == nil {
				known := append([]string{api.LocalNode}, sortedNodeNames(st)...)
				return nil, api.Errorf(api.CodeNotFound, "node %q does not exist", n).
					WithHint("Known nodes: %s. Add one with `saba node add <name> <user@host>`.", strings.Join(known, ", "))
			}
			add(n)
		}
	}
	return out, nil
}

func sortedNodeNames(st *store.State) []string {
	names := make([]string, 0, len(st.Nodes))
	for n := range st.Nodes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ship makes an image available on a remote node, streaming it straight from
// the primary's engine. Nothing is sent if the node already has it.
func (e *Engine) ship(ctx context.Context, r *Run, image, node string) error {
	remote := e.rt(node)
	if err := remote.EnsureNetwork(ctx, Network); err != nil {
		return api.Errorf(api.CodeRuntime, "node %q is not reachable: %v", node, err).
			WithHint("Check it with `saba node list`. The node needs Docker running and SSH access from the primary.")
	}
	local, err := e.RT.InspectImage(ctx, image)
	if err != nil {
		return api.Errorf(api.CodeRuntime, "image %s is missing on the primary: %v", image, err)
	}
	if have, err := remote.InspectImage(ctx, image); err == nil && have.ID == local.ID {
		return nil
	}
	// The same image under another tag (a new version of unchanged code, or a
	// rollback) only needs a tag.
	if _, err := remote.InspectImage(ctx, local.ID); err == nil {
		if err := remote.TagImage(ctx, local.ID, image); err == nil {
			return nil
		}
	}
	r.phase("ship", "Sending the image to node %s", node)
	pr, pw := io.Pipe()
	saved := make(chan error, 1)
	go func() {
		err := e.RT.SaveImage(ctx, image, pw)
		pw.CloseWithError(err)
		saved <- err
	}()
	loadErr := remote.LoadImage(ctx, pr)
	pr.CloseWithError(loadErr)
	if saveErr := <-saved; saveErr != nil && loadErr == nil {
		loadErr = saveErr
	}
	if loadErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return api.Errorf(api.CodeRuntime, "could not send the image to node %q: %v", node, loadErr)
	}
	return nil
}

// extraHosts returns /etc/hosts entries that let a container on `node` reach,
// by name, apps that publish ports on a different node. Names of apps on the
// same node resolve through Docker's own DNS.
func extraHosts(st *store.State, self, node string) []string {
	addr := func(n string) string {
		if isLocal(n) {
			return st.Server.Address
		}
		if nd := st.Nodes[n]; nd != nil {
			return nd.Address
		}
		return ""
	}
	var out []string
	for _, name := range st.AppNames() {
		a := st.Apps[name]
		cur := a.Current()
		if name == self || cur == nil || len(cur.Config.Publish) == 0 || len(cur.Replicas) == 0 {
			continue
		}
		home := cur.Replicas[0].Node
		if (isLocal(home) && isLocal(node)) || home == node {
			continue // same server: Docker's own DNS resolves the name
		}
		if ip := addr(home); ip != "" {
			out = append(out, name+":"+ip)
		}
	}
	return out
}

// portConflict reports whether starting a container failed because its host
// port is taken on the node.
func portConflict(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "port is already allocated") || strings.Contains(m, "address already in use")
}

// unforward releases the tunnel of a remote container, if it has one.
func (e *Engine) unforward(ctx context.Context, c *runtime.Container) {
	if isLocal(c.Node) || e.Nodes == nil {
		return
	}
	if port, err := strconv.Atoi(c.Labels[LabelPort]); err == nil && port > 0 {
		e.Nodes.Unforward(ctx, c.Node, port)
	}
}

// removeImage removes an image tag from the primary and from every node.
func (e *Engine) removeImage(ctx context.Context, ref string) {
	_ = e.RT.RemoveImage(ctx, ref)
	for _, node := range e.remoteNodes() {
		_ = e.Nodes.Runtime(node).RemoveImage(ctx, ref)
	}
}

// replicaNodes maps container names of a deployment to their nodes.
func replicaNodes(d *api.Deployment) map[string]string {
	out := map[string]string{}
	if d != nil {
		for _, r := range d.Replicas {
			out[r.Container] = r.Node
		}
	}
	return out
}
