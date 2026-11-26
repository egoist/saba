// Package engine implements saba's deployment logic: turning a deploy request
// into running containers and switching traffic to them without downtime.
package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/proxy"
	"github.com/egoist/saba/internal/runtime"
	"github.com/egoist/saba/internal/store"
)

const (
	Network = "saba"

	LabelManaged = "saba.managed"
	LabelApp     = "saba.app"
	LabelVersion = "saba.version"
	LabelReplica = "saba.replica"
	LabelRole    = "saba.role"

	RoleApp     = "app"
	RoleRelease = "release"
	RoleRun     = "run"
	RoleCron    = "cron"

	defaultHealthTimeout = 60 * time.Second
	defaultDrainTimeout  = 30 * time.Second
	defaultStopTimeout   = 10 * time.Second
	defaultKeepVersions  = 5
	maxDeployHistory     = 40
	deployTimeout        = 45 * time.Minute
)

type Engine struct {
	Store *store.Store
	RT    runtime.Runtime
	Proxy *proxy.Proxy
	// Nodes connects to the cluster's other servers; nil on a single server.
	Nodes   Nodes
	DataDir string
	Log     *slog.Logger
	// PortMin/PortMax bound the loopback ports replicas are published on.
	PortMin, PortMax int
	// StableWindow is how long a worker must stay up to count as healthy.
	StableWindow time.Duration
	// HealthInterval is the default pause between health check attempts.
	HealthInterval time.Duration
	// TLS and URLSuffix shape the URLs reported to users (https://host[:port]).
	TLS       bool
	URLSuffix string

	backupLocks map[string]*sync.Mutex // one backup or restore per database at a time
	wg          sync.WaitGroup         // in-flight deployments, including their cleanup
	mu          sync.Mutex
	busy        map[string]*Run
	logs        map[string]*EventLog
	portCursor  int
}

func New(st *store.Store, rt runtime.Runtime, px *proxy.Proxy, dataDir string, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Engine{
		Store: st, RT: rt, Proxy: px, DataDir: dataDir, Log: log,
		PortMin: 20000, PortMax: 29999,
		StableWindow:   3 * time.Second,
		HealthInterval: 500 * time.Millisecond,
		busy:           map[string]*Run{},
		logs:           map[string]*EventLog{},
		portCursor:     rand.IntN(10000),
	}
}

// Run is a deployment in progress.
type Run struct {
	App     string
	Version int
	Events  *EventLog
	cancel  context.CancelFunc
}

func (r *Run) phase(phase, format string, args ...any) {
	r.Events.Append(api.Event{Type: "phase", Phase: phase, Message: fmt.Sprintf(format, args...)})
}

func (r *Run) logf(stream, format string, args ...any) {
	r.Events.Append(api.Event{Type: "log", Stream: stream, Message: fmt.Sprintf(format, args...)})
}

func (r *Run) lineWriter(stream string) *runtime.LineWriter {
	return runtime.NewLineWriter(func(line string) {
		r.Events.Append(api.Event{Type: "log", Stream: stream, Message: line})
	})
}

func imageTag(app string, version int) string { return fmt.Sprintf("saba/%s:v%d", app, version) }

func containerName(app string, version, replica int) string {
	return fmt.Sprintf("saba-%s-v%d-%d", app, version, replica)
}

func volumeName(app, name string) string { return fmt.Sprintf("saba-%s-%s", app, name) }

func eventKey(app string, version int) string { return fmt.Sprintf("%s/v%d", app, version) }

func (e *Engine) eventPath(app string, version int) string {
	return filepath.Join(e.DataDir, "logs", app, fmt.Sprintf("v%d.ndjson", version))
}

// Wait blocks until background deployment work has finished or the timeout
// passes. It reports whether everything finished.
func (e *Engine) Wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Busy reports whether a deployment is in progress for the app.
func (e *Engine) Busy(app string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.busy[app] != nil
}

func (e *Engine) lock(app string, r *Run) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cur := e.busy[app]; cur != nil {
		err := api.Errorf(api.CodeDeployInProgress, "app %q is busy with another operation", app)
		if cur.Version > 0 {
			err = api.Errorf(api.CodeDeployInProgress, "a deployment (v%d) is already in progress for %q", cur.Version, app).
				WithDetail("version", cur.Version)
		}
		return err.WithHint("Wait for it to finish (`saba logs -a %s --deployment latest --follow`), then retry.", app)
	}
	e.busy[app] = r
	return nil
}

func (e *Engine) unlock(app string, r *Run) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.busy[app] == r {
		delete(e.busy, app)
	}
}

// routed reports whether the app (with the effective config cfg) receives HTTP traffic.
func routed(kind string, cfg *api.AppConfig) bool {
	return kind != api.KindDatabase && !cfg.Worker && cfg.Port > 0
}

// Hosts returns the hostnames routed to an app.
func Hosts(server api.ServerConfig, name, kind string, cfg *api.AppConfig) []string {
	if !routed(kind, cfg) || cfg.Internal {
		return nil
	}
	if len(cfg.Domains) > 0 {
		return cfg.Domains
	}
	if server.BaseDomain != "" {
		return []string{name + "." + server.BaseDomain}
	}
	return nil
}

func (e *Engine) scheme() string {
	if e.TLS {
		return "https"
	}
	return "http"
}

// URLs returns the public URLs of an app.
func (e *Engine) URLs(server api.ServerConfig, a *store.AppRecord) []string {
	cfg := a.Config
	if cur := a.Current(); cur != nil {
		cfg = cur.Config
		cfg.Port = cur.Port
	}
	var urls []string
	for _, h := range Hosts(server, a.Name, a.Kind, &cfg) {
		if strings.HasPrefix(h, "*.") {
			continue
		}
		urls = append(urls, e.scheme()+"://"+h+e.URLSuffix)
	}
	return urls
}

var refRe = regexp.MustCompile(`\$\{\{\s*([a-z][a-z0-9-]*)\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// ResolveEnv expands ${{app.KEY}} references against other apps' outputs and env.
func ResolveEnv(st *store.State, a *store.AppRecord, env map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(env))
	for k, v := range env {
		var firstErr error
		out[k] = refRe.ReplaceAllStringFunc(v, func(m string) string {
			sub := refRe.FindStringSubmatch(m)
			target, key := sub[1], sub[2]
			other := st.Apps[target]
			if target == a.Name {
				other = a
			}
			if other == nil {
				if firstErr == nil {
					firstErr = api.Errorf(api.CodeInvalid, "env var %s references %s, but app %q does not exist", k, m, target).
						WithHint("Create it first, or fix the reference with `saba env set -a %s %s=...`.", a.Name, k)
				}
				return m
			}
			if val, ok := other.Outputs[key]; ok {
				return val
			}
			if val, ok := other.Env[key]; ok {
				return val
			}
			if firstErr == nil {
				firstErr = api.Errorf(api.CodeInvalid, "env var %s references %s, but %q has no variable %s", k, m, target, key).
					WithHint("Available on %s: %s", target, strings.Join(refKeys(other), ", "))
			}
			return m
		})
		if firstErr != nil {
			return nil, firstErr
		}
	}
	return out, nil
}

func refKeys(a *store.AppRecord) []string {
	var keys []string
	for k := range a.Outputs {
		keys = append(keys, k)
	}
	for k := range a.Env {
		if _, dup := a.Outputs[k]; !dup {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return []string{"(none)"}
	}
	return keys
}

// allocPort reserves a free loopback port that no known replica uses.
func (e *Engine) allocPort(used map[int]bool) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	span := e.PortMax - e.PortMin + 1
	for i := 0; i < span; i++ {
		p := e.PortMin + (e.portCursor % span)
		e.portCursor++
		if used[p] {
			continue
		}
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			continue
		}
		l.Close()
		used[p] = true
		return p, nil
	}
	return 0, api.Errorf(api.CodeRuntime, "no free port in range %d-%d", e.PortMin, e.PortMax)
}

func usedPorts(st *store.State) map[int]bool {
	used := map[int]bool{}
	for _, a := range st.Apps {
		for _, d := range a.Deployments {
			if d.Status == api.StatusSuperseded || d.Status == api.StatusFailed {
				continue
			}
			for _, r := range d.Replicas {
				if r.HostPort > 0 {
					used[r.HostPort] = true
				}
			}
		}
	}
	return used
}

func addrs(replicas []api.Replica) []string {
	var out []string
	for _, r := range replicas {
		if r.HostPort > 0 {
			out = append(out, "127.0.0.1:"+strconv.Itoa(r.HostPort))
		}
	}
	return out
}

// applyRoute points the proxy at an app's current deployment.
func (e *Engine) applyRoute(server api.ServerConfig, a *store.AppRecord) *proxy.Pool {
	cur := a.Current()
	if cur == nil {
		return e.Proxy.RemoveApp(a.Name)
	}
	cfg := cur.Config
	cfg.Port = cur.Port
	hosts := Hosts(server, a.Name, a.Kind, &cfg)
	if len(hosts) == 0 {
		return e.Proxy.RemoveApp(a.Name)
	}
	var ups []string
	if !a.Stopped {
		ups = addrs(cur.Replicas)
	}
	return e.Proxy.SetApp(a.Name, cur.Version, hosts, ups)
}

// RefreshRoutes re-applies every app's route (after startup or a base domain change).
func (e *Engine) RefreshRoutes() {
	st := e.Store.Snapshot()
	for _, a := range st.Apps {
		if !e.Busy(a.Name) {
			e.applyRoute(st.Server, a)
		}
	}
}

func dur(s string, def time.Duration) time.Duration {
	d, err := api.ParseDuration(s, def)
	if err != nil || d == 0 {
		return def
	}
	return d
}

func labels(app string, version, replica int, role string) map[string]string {
	l := map[string]string{LabelManaged: "true", LabelApp: app, LabelRole: role}
	if version > 0 {
		l[LabelVersion] = strconv.Itoa(version)
	}
	if replica > 0 {
		l[LabelReplica] = strconv.Itoa(replica)
	}
	return l
}

func bg(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), d)
}
