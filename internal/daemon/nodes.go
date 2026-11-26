package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/cluster"
	"github.com/egoist/saba/internal/store"
)

const joinTokenTTL = 24 * time.Hour

func sortedKeys(m map[string]*api.Node) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// authorizeAgent admits an agent: by its pinned key once joined, by its
// one-time token the first time (which pins its key).
func (d *Daemon) authorizeAgent(h cluster.Hello, fingerprint string) error {
	return d.store.Update(func(st *store.State) error {
		n := st.Nodes[h.Node]
		switch {
		case n == nil:
			return fmt.Errorf("%q is not a node of this cluster (it may have been removed); add it again with `saba node add %s`", h.Node, h.Node)
		case n.KeyFingerprint != "":
			if n.KeyFingerprint != fingerprint {
				return fmt.Errorf("this agent's key does not match the one %q joined with; re-add the node with `saba node add %s --rejoin`", h.Node, h.Node)
			}
			return nil
		case h.Token == "" || n.TokenHash == "" || hashToken(h.Token) != n.TokenHash:
			return fmt.Errorf("invalid join token for %q; get a new one with `saba node add %s --rejoin`", h.Node, h.Node)
		case time.Now().After(n.TokenExpires):
			return fmt.Errorf("the join token for %q expired; get a new one with `saba node add %s --rejoin`", h.Node, h.Node)
		}
		n.KeyFingerprint, n.TokenHash, n.TokenExpires = fingerprint, "", time.Time{}
		n.JoinedAt = time.Now().UTC()
		return nil
	})
}

// joinAddress is where agents reach this server.
func (d *Daemon) joinAddress(ctx context.Context, override string) (string, error) {
	host := strings.TrimSpace(override)
	if host == "" {
		host = d.store.Snapshot().Server.Address
	}
	if host == "" {
		host = PublicIP(ctx)
	}
	if host == "" {
		return "", api.Errorf(api.CodeInvalid, "cannot work out an address nodes can reach this server at").
			WithHint("Pass --primary-address <ip-or-host>, or set it once with `saba server config set address <ip>`.")
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host, nil
	}
	addr := d.opts.ClusterAddr
	if d.opts.TLS == "auto" {
		addr = d.opts.HTTPSAddr
	}
	port := "443"
	if _, p, err := net.SplitHostPort(addr); err == nil && p != "" && p != "0" {
		port = p
	}
	return net.JoinHostPort(host, port), nil
}

// view adds connection details to a stored node, without secrets.
func (d *Daemon) nodeView(n api.Node, status map[string]cluster.Status, replicas map[string]int) api.Node {
	n.TokenHash, n.TokenExpires = "", time.Time{}
	n.Replicas = replicas[n.Name]
	s, ok := status[n.Name]
	switch {
	case n.KeyFingerprint == "":
		n.Status = "pending"
	case ok && s.Connected:
		n.Status = "connected"
	default:
		n.Status = "unreachable"
		n.Error = s.Error
		if n.Error == "" {
			n.Error = "the agent has not connected since the primary started"
		}
	}
	if ok {
		n.Version, n.Hostname, n.Platform, n.LastSeen = s.Hello.Version, s.Hello.Hostname, s.Hello.Platform, s.LastSeen
		n.Docker = s.Docker
		if s.Stats.CPUs > 0 {
			stats := s.Stats
			n.Stats = &stats
		}
	}
	return n
}

func (d *Daemon) handleNodes(w http.ResponseWriter, r *http.Request) error {
	st := d.store.Snapshot()
	replicas := map[string]int{}
	for _, a := range st.Apps {
		if cur := a.Current(); cur != nil {
			for _, rep := range cur.Replicas {
				n := rep.Node
				if n == "" {
					n = api.LocalNode
				}
				replicas[n]++
			}
		}
	}
	docker, _ := d.rt.Version(r.Context())
	host, _ := os.Hostname()
	localStats := cluster.HostStats(d.opts.DataDir)
	nodes := []api.Node{{
		Name: api.LocalNode, Address: st.Server.Address, Status: "connected", Docker: docker,
		Version: d.opts.Version, Hostname: host, Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Stats: &localStats, Replicas: replicas[api.LocalNode],
	}}
	status := d.cluster.Status()
	for _, name := range sortedKeys(st.Nodes) {
		nodes = append(nodes, d.nodeView(*st.Nodes[name], status, replicas))
	}
	writeJSON(w, http.StatusOK, nodes)
	return nil
}

func (d *Daemon) handleNodeAdd(w http.ResponseWriter, r *http.Request) error {
	var req api.NodeAddRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := api.ValidateName(req.Name); err != nil {
		return err
	}
	if req.Name == api.LocalNode {
		return api.Errorf(api.CodeInvalid, "%q is the name of the primary server itself", api.LocalNode).
			WithHint("Pick another name, e.g. worker1.")
	}
	req.Address = strings.TrimSpace(req.Address)
	if req.Address != "" && net.ParseIP(req.Address) == nil {
		return api.Errorf(api.CodeInvalid, "--address must be an IP address, got %q", req.Address)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	var (
		resp     api.NodeAddResponse
		newToken string
		expires  time.Time
	)
	existing := d.store.Snapshot().Nodes[req.Name]
	if existing == nil || existing.KeyFingerprint == "" || req.Rejoin {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		newToken, expires = hex.EncodeToString(b), time.Now().Add(joinTokenTTL).UTC()
	}
	var primary string
	if newToken != "" {
		var err error
		if primary, err = d.joinAddress(ctx, req.PrimaryAddress); err != nil {
			return err
		}
	}
	if err := d.store.Update(func(st *store.State) error {
		if st.Nodes == nil {
			st.Nodes = map[string]*api.Node{}
		}
		n := st.Nodes[req.Name]
		if n == nil {
			n = &api.Node{Name: req.Name, CreatedAt: time.Now().UTC()}
			st.Nodes[req.Name] = n
		}
		if req.SSH != "" {
			n.SSH = req.SSH
		}
		if req.Address != "" {
			n.Address = req.Address
		}
		if newToken != "" {
			n.TokenHash, n.TokenExpires = hashToken(newToken), expires
			if req.Rejoin {
				n.KeyFingerprint = ""
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if req.Rejoin {
		d.cluster.Remove(req.Name) // drop the old agent's connection
	}
	d.cluster.Prepare(req.Name)
	st := d.store.Snapshot()
	resp.Node = d.nodeView(*st.Nodes[req.Name], d.cluster.Status(), map[string]int{})
	if newToken != "" {
		resp.Join = cluster.JoinInfo{Node: req.Name, Primary: primary, Fingerprint: d.cluster.Identity.Fingerprint, Token: newToken}.String()
		resp.JoinExpires = expires
	}
	writeJSON(w, http.StatusOK, resp)
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
	d.cluster.Remove(name)
	if err := d.store.Update(func(st *store.State) error {
		delete(st.Nodes, name)
		return nil
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": name, "removed": true})
	return nil
}
