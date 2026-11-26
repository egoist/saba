package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/runtime"
	"github.com/egoist/saba/internal/shellwords"
	"github.com/egoist/saba/internal/store"
)

// Apps returns the public view of every app.
func (e *Engine) Apps(ctx context.Context) ([]*api.App, error) {
	st := e.Store.Snapshot()
	containers := e.containersByApp(ctx)
	apps := make([]*api.App, 0, len(st.Apps))
	for _, name := range st.AppNames() {
		apps = append(apps, e.view(st, st.Apps[name], containers[name], false))
	}
	return apps, nil
}

// App returns the detailed public view of one app.
func (e *Engine) App(ctx context.Context, name string) (*api.App, error) {
	st := e.Store.Snapshot()
	a := st.Apps[name]
	if a == nil {
		return nil, store.NotFound(name)
	}
	cs, _ := e.RT.List(ctx, map[string]string{LabelApp: name, LabelRole: RoleApp})
	return e.view(st, a, cs, true), nil
}

func (e *Engine) containersByApp(ctx context.Context) map[string][]runtime.Container {
	out := map[string][]runtime.Container{}
	cs, err := e.RT.List(ctx, map[string]string{LabelManaged: "true", LabelRole: RoleApp})
	if err != nil {
		e.Log.Warn("cannot list containers", "err", err)
		return out
	}
	for _, c := range cs {
		out[c.Labels[LabelApp]] = append(out[c.Labels[LabelApp]], c)
	}
	return out
}

func (e *Engine) view(st *store.State, a *store.AppRecord, cs []runtime.Container, detail bool) *api.App {
	v := a.App
	v.URLs = e.URLs(st.Server, a)
	cur := a.Current()

	hostPorts := map[string]int{}
	if cur != nil {
		for _, r := range cur.Replicas {
			hostPorts[r.Container] = r.HostPort
		}
	}
	running, unstable := 0, false
	for _, c := range cs {
		ver, _ := strconv.Atoi(c.Labels[LabelVersion])
		rep, _ := strconv.Atoi(c.Labels[LabelReplica])
		if ver == a.CurrentVersion {
			if c.Running() {
				running++
			}
			if c.State == "restarting" || c.Restarts > 0 {
				unstable = true
			}
		}
		v.Containers = append(v.Containers, api.ContainerStatus{
			Name: c.Name, Replica: rep, Version: ver, State: c.State, Health: c.Health,
			Restarts: c.Restarts, ExitCode: c.ExitCode, OOMKilled: c.OOMKilled,
			StartedAt: c.StartedAt, HostPort: hostPorts[c.Name],
		})
	}
	sort.Slice(v.Containers, func(i, j int) bool {
		if v.Containers[i].Version != v.Containers[j].Version {
			return v.Containers[i].Version > v.Containers[j].Version
		}
		return v.Containers[i].Replica < v.Containers[j].Replica
	})

	switch {
	case e.Busy(a.Name):
		v.Status = api.AppDeploying
	case a.Stopped:
		v.Status = api.AppStopped
	case cur == nil:
		v.Status = api.AppCreated
		if n := len(a.Deployments); n > 0 && a.Deployments[n-1].Status == api.StatusFailed {
			v.Status = api.AppFailed
		}
	case running == 0:
		v.Status = api.AppCrashed
	case running < len(cur.Replicas) || unstable:
		v.Status = api.AppDegraded
	default:
		v.Status = api.AppRunning
	}

	if s, ok := e.Proxy.Stats(a.Name); ok {
		v.Traffic = &api.Traffic{Requests: s.Requests, Errors: s.Errors, InFlight: s.InFlight}
	}
	if detail {
		if n := len(v.Deployments); n > 10 {
			v.Deployments = v.Deployments[n-10:]
		}
	} else {
		v.Deployments = nil
		v.Containers = nil
		v.Cron = nil
		if cur != nil {
			c := *cur
			v.Deployments = []*api.Deployment{&c}
		}
	}
	return &v
}

// Deployments returns an app's deployment history, newest first.
func (e *Engine) Deployments(name string) ([]*api.Deployment, error) {
	a, err := e.Store.App(name)
	if err != nil {
		return nil, err
	}
	out := a.Deployments
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out, nil
}

// DeployEvents streams the event log of a deployment. version 0 means the latest.
func (e *Engine) DeployEvents(ctx context.Context, name string, version int, follow bool, fn func(api.Event) error) error {
	a, err := e.Store.App(name)
	if err != nil {
		return err
	}
	if version == 0 {
		version = a.LastVersion
	}
	if a.Deployment(version) == nil {
		return api.Errorf(api.CodeNotFound, "%q has no deployment v%d", name, version).
			WithHint("Run `saba deployments -a %s` to list versions.", name)
	}
	e.mu.Lock()
	l := e.logs[eventKey(name, version)]
	e.mu.Unlock()
	if l != nil {
		return l.Stream(ctx, follow, fn)
	}
	return readEventFile(e.eventPath(name, version), fn)
}

// Env returns an app's environment, raw and with references resolved.
func (e *Engine) Env(name string) (*api.EnvResponse, error) {
	st := e.Store.Snapshot()
	a := st.Apps[name]
	if a == nil {
		return nil, store.NotFound(name)
	}
	env := a.Env
	if env == nil {
		env = map[string]string{}
	}
	resp := &api.EnvResponse{App: name, Env: env}
	if resolved, err := ResolveEnv(st, a, env); err == nil {
		resp.Resolved = resolved
	}
	return resp, nil
}

// Outputs returns the values an app exposes to others (database URLs etc).
func (e *Engine) Outputs(name string) (map[string]string, error) {
	a, err := e.Store.App(name)
	if err != nil {
		return nil, err
	}
	return a.Outputs, nil
}

// current returns the app and its live deployment, or a helpful error.
func (e *Engine) current(name string) (*store.State, *store.AppRecord, *api.Deployment, error) {
	st := e.Store.Snapshot()
	a := st.Apps[name]
	if a == nil {
		return nil, nil, nil, store.NotFound(name)
	}
	cur := a.Current()
	if cur == nil {
		err := api.Errorf(api.CodeConflict, "%q has no live version", name)
		if a.LastVersion > 0 {
			return nil, nil, nil, err.WithHint("Its last deployment did not succeed. See why with `saba logs -a %s --deployment latest`.", name)
		}
		return nil, nil, nil, err.WithHint("Deploy it first with `saba deploy -a %s`.", name)
	}
	return st, a, cur, nil
}

// Container returns the container name of a replica of the live version.
func (e *Engine) Container(name string, replica int) (string, error) {
	_, _, cur, err := e.current(name)
	if err != nil {
		return "", err
	}
	if replica <= 0 {
		replica = 1
	}
	for _, r := range cur.Replicas {
		if r.Index == replica {
			return r.Container, nil
		}
	}
	return "", api.Errorf(api.CodeNotFound, "%q has no replica %d (it runs %d)", name, replica, len(cur.Replicas))
}

// Logs streams runtime logs of the live version.
func (e *Engine) Logs(ctx context.Context, name string, q api.LogsQuery, fn func(api.Event)) error {
	_, _, cur, err := e.current(name)
	if err != nil {
		return err
	}
	var reps []api.Replica
	for _, r := range cur.Replicas {
		if q.Replica == 0 || q.Replica == r.Index {
			reps = append(reps, r)
		}
	}
	if len(reps) == 0 {
		return api.Errorf(api.CodeNotFound, "%q has no replica %d (it runs %d)", name, q.Replica, len(cur.Replicas))
	}
	opts := runtime.LogOptions{Tail: q.Tail, Since: q.Since, Follow: q.Follow}

	type entry struct {
		t   time.Time
		ev  api.Event
		seq int
	}
	var (
		mu       sync.Mutex
		buffered []entry
		wg       sync.WaitGroup
		firstErr error
	)
	for _, rep := range reps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := e.RT.Logs(ctx, rep.Container, opts, func(l runtime.LogLine) {
				ev := api.Event{Type: "log", Stream: "container", Replica: rep.Index, Time: l.Time, Message: l.Text}
				mu.Lock()
				defer mu.Unlock()
				if q.Follow {
					fn(ev)
				} else {
					buffered = append(buffered, entry{l.Time, ev, len(buffered)})
				}
			})
			if err != nil && !errors.Is(err, runtime.ErrNotFound) {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if !q.Follow {
		sort.SliceStable(buffered, func(i, j int) bool {
			if buffered[i].t.Equal(buffered[j].t) {
				return buffered[i].seq < buffered[j].seq
			}
			return buffered[i].t.Before(buffered[j].t)
		})
		if q.Tail > 0 && len(buffered) > q.Tail {
			buffered = buffered[len(buffered)-q.Tail:]
		}
		for _, b := range buffered {
			fn(b.ev)
		}
	}
	if firstErr != nil && ctx.Err() == nil {
		return api.Errorf(api.CodeRuntime, "cannot read logs: %v", firstErr)
	}
	return nil
}

// Exec runs a command in a running replica, or in a fresh one-off container
// created from the live version's image, env and volumes.
func (e *Engine) Exec(ctx context.Context, name string, req api.ExecRequest, stdout, stderr io.Writer) (int, error) {
	if len(req.Command) == 0 {
		return -1, api.Errorf(api.CodeInvalid, "no command given").WithHint("Example: `saba exec -a %s -- ls -la`.", name)
	}
	st, a, cur, err := e.current(name)
	if err != nil {
		return -1, err
	}
	if !req.OneOff {
		container, err := e.Container(name, req.Replica)
		if err != nil {
			return -1, err
		}
		code, err := e.RT.Exec(ctx, container, req.Command, stdout, stderr)
		if err != nil {
			return -1, api.Errorf(api.CodeRuntime, "exec failed: %v", err).
				WithHint("Check the app is running with `saba status -a %s`. Use `saba run` to start a fresh one-off container instead.", name)
		}
		return code, nil
	}
	env, err := e.runtimeEnv(st, a, cur)
	if err != nil {
		return -1, err
	}
	opts := e.runOptions(a, cur, env)
	opts.Name = fmt.Sprintf("saba-%s-run-%s", name, randomID())
	opts.Labels = labels(name, cur.Version, 0, RoleRun)
	opts.Aliases = nil
	opts.Command = req.Command
	code, err := e.RT.RunOnce(ctx, opts, stdout, stderr)
	if err != nil {
		return -1, api.Errorf(api.CodeRuntime, "run failed: %v", err)
	}
	return code, nil
}

func (e *Engine) runtimeEnv(st *store.State, a *store.AppRecord, dep *api.Deployment) (map[string]string, error) {
	src := a.EnvSnapshots[dep.Version]
	if src == nil {
		src = a.Env
	}
	env, err := ResolveEnv(st, a, src)
	if err != nil {
		return nil, err
	}
	if dep.Port > 0 {
		if _, ok := env["PORT"]; !ok {
			env["PORT"] = strconv.Itoa(dep.Port)
		}
	}
	env["SABA_APP"] = a.Name
	env["SABA_VERSION"] = strconv.Itoa(dep.Version)
	return env, nil
}

func randomID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Stop stops the live version's containers without removing them.
func (e *Engine) Stop(ctx context.Context, name string) error {
	run := &Run{App: name}
	if err := e.lock(name, run); err != nil {
		return err
	}
	defer e.unlock(name, run)
	st, a, cur, err := e.current(name)
	if err != nil {
		return err
	}
	if err := e.Store.UpdateApp(name, func(a *store.AppRecord) error { a.Stopped = true; return nil }); err != nil {
		return err
	}
	a.Stopped = true
	old := e.applyRoute(st.Server, a)
	if old != nil {
		dctx, cancel := context.WithTimeout(ctx, dur(cur.Config.DrainTimeout, defaultDrainTimeout))
		old.Drain(dctx)
		cancel()
		old.Close()
	}
	for _, r := range cur.Replicas {
		if err := e.RT.Stop(ctx, r.Container, dur(cur.Config.StopTimeout, defaultStopTimeout)); err != nil {
			return api.Errorf(api.CodeRuntime, "cannot stop %s: %v", r.Container, err)
		}
	}
	return nil
}

// Start starts a stopped app's containers and restores its route.
func (e *Engine) Start(ctx context.Context, name string) error {
	run := &Run{App: name, Events: newEventLog("")}
	if err := e.lock(name, run); err != nil {
		return err
	}
	defer e.unlock(name, run)
	st, a, cur, err := e.current(name)
	if err != nil {
		return err
	}
	for _, r := range cur.Replicas {
		if err := e.RT.Start(ctx, r.Container); err != nil {
			return api.Errorf(api.CodeRuntime, "cannot start %s: %v", r.Container, err).
				WithHint("Recreate the containers with `saba restart -a %s`.", name)
		}
	}
	if err := e.waitHealthy(ctx, run, a, cur, cur.Replicas); err != nil {
		return err
	}
	e.keepAlive(ctx, cur.Replicas)
	if err := e.Store.UpdateApp(name, func(a *store.AppRecord) error { a.Stopped = false; return nil }); err != nil {
		return err
	}
	a.Stopped = false
	e.applyRoute(st.Server, a)
	return nil
}

// Destroy removes an app: containers, images, route, history and optionally volumes.
func (e *Engine) Destroy(ctx context.Context, name string, removeVolumes bool) ([]string, error) {
	run := &Run{App: name}
	if err := e.lock(name, run); err != nil {
		return nil, err
	}
	defer e.unlock(name, run)
	a, err := e.Store.App(name)
	if err != nil {
		return nil, err
	}
	// Refuse while other apps still depend on this one's variables.
	st := e.Store.Snapshot()
	for _, other := range st.Apps {
		if other.Name == name {
			continue
		}
		for k, v := range other.Env {
			for _, m := range refRe.FindAllStringSubmatch(v, -1) {
				if m[1] == name {
					return nil, api.Errorf(api.CodeConflict, "%q is referenced by %s of app %q", name, k, other.Name).
						WithHint("Remove the reference first: `saba env unset -a %s %s`.", other.Name, k)
				}
			}
		}
	}

	if old := e.Proxy.RemoveApp(name); old != nil {
		old.Close()
	}
	e.removeContainers(ctx, name, func(*runtime.Container) bool { return true }, 5*time.Second)
	seen := map[string]bool{}
	for _, d := range a.Deployments {
		if d.Image != "" && !seen[d.Image] && strings.HasPrefix(d.Image, "saba/"+name+":") {
			seen[d.Image] = true
			_ = e.RT.RemoveImage(ctx, d.Image)
		}
	}
	// Volumes may have been created by any version, including failed ones.
	specs := append([]string(nil), a.Config.Volumes...)
	for _, d := range a.Deployments {
		specs = append(specs, d.Config.Volumes...)
	}
	var kept []string
	seenVol := map[string]bool{}
	for _, v := range specs {
		vol, err := api.ParseVolume(v)
		if err != nil || vol.Bind || seenVol[vol.Source] {
			continue
		}
		seenVol[vol.Source] = true
		vn := volumeName(name, vol.Source)
		if !removeVolumes {
			kept = append(kept, vn)
			continue
		}
		if err := e.RT.RemoveVolume(ctx, vn); err != nil {
			e.Log.Warn("cannot remove volume", "volume", vn, "err", err)
			kept = append(kept, vn)
		}
	}
	if err := e.Store.Update(func(st *store.State) error {
		delete(st.Apps, name)
		return nil
	}); err != nil {
		return nil, err
	}
	e.mu.Lock()
	for k := range e.logs {
		if strings.HasPrefix(k, name+"/") {
			delete(e.logs, k)
		}
	}
	e.mu.Unlock()
	os.RemoveAll(filepath.Join(e.DataDir, "logs", name))
	return kept, nil
}

// RunCron executes one cron job of an app and records the outcome.
func (e *Engine) RunCron(ctx context.Context, name, job string) (*api.CronState, error) {
	st, a, cur, err := e.current(name)
	if err != nil {
		return nil, err
	}
	var spec *api.CronJob
	for i := range cur.Config.Cron {
		if cur.Config.Cron[i].Name == job {
			spec = &cur.Config.Cron[i]
		}
	}
	if spec == nil {
		return nil, api.Errorf(api.CodeNotFound, "%q has no cron job %q in its live version", name, job).
			WithHint("Cron jobs are defined under [[cron]] in saba.toml and take effect on deploy.")
	}
	cmd, err := shellwords.Command(spec.Command)
	if err != nil {
		return nil, api.Errorf(api.CodeInvalid, "invalid cron command: %v", err)
	}
	env, err := e.runtimeEnv(st, a, cur)
	if err != nil {
		return nil, err
	}
	opts := e.runOptions(a, cur, env)
	opts.Name = fmt.Sprintf("saba-%s-cron-%s-%s", name, job, randomID())
	opts.Labels = labels(name, cur.Version, 0, RoleCron)
	opts.Aliases = nil
	opts.Command = cmd

	var out tail
	start := time.Now()
	code, runErr := e.RT.RunOnce(ctx, opts, &out, &out)
	state := &api.CronState{
		LastRun:    start.UTC(),
		ExitCode:   code,
		Duration:   time.Since(start).Round(time.Millisecond).String(),
		Output:     out.String(),
		LastStatus: "succeeded",
	}
	if runErr != nil {
		state.LastStatus = "error"
		state.Output = strings.TrimSpace(state.Output + "\n" + runErr.Error())
	} else if code != 0 {
		state.LastStatus = "failed"
	}
	_ = e.Store.UpdateApp(name, func(a *store.AppRecord) error {
		if a.Cron == nil {
			a.Cron = map[string]*api.CronState{}
		}
		a.Cron[job] = state
		return nil
	})
	return state, nil
}

// tail keeps the last 4 KB written to it.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(strings.ToValidUTF8(string(t.buf), ""))
}

// Stats returns resource usage for one app, or all apps if name is empty.
func (e *Engine) Stats(ctx context.Context, name string) ([]api.Stats, error) {
	filter := map[string]string{LabelManaged: "true", LabelRole: RoleApp}
	if name != "" {
		if _, err := e.Store.App(name); err != nil {
			return nil, err
		}
		filter[LabelApp] = name
	}
	cs, err := e.RT.List(ctx, filter)
	if err != nil {
		return nil, api.Errorf(api.CodeRuntime, "cannot list containers: %v", err)
	}
	apps := map[string]string{}
	var names []string
	for _, c := range cs {
		if c.Running() {
			names = append(names, c.Name)
			apps[c.Name] = c.Labels[LabelApp]
		}
	}
	raw, err := e.RT.Stats(ctx, names)
	if err != nil {
		return nil, api.Errorf(api.CodeRuntime, "cannot read stats: %v", err)
	}
	out := make([]api.Stats, 0, len(raw))
	for _, s := range raw {
		out = append(out, api.Stats{
			App: apps[s.Container], Container: s.Container, CPU: s.CPU, Memory: s.Memory,
			MemoryPct: s.MemoryPct, NetIO: s.NetIO, BlockIO: s.BlockIO, PIDs: s.PIDs,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Container < out[j].Container })
	return out, nil
}

// Reconcile brings the world in line with the stored state after a daemon
// start: interrupted deployments are failed, orphaned containers removed,
// stopped-but-wanted containers started and routes restored.
func (e *Engine) Reconcile(ctx context.Context) {
	if err := e.RT.EnsureNetwork(ctx, Network); err != nil {
		e.Log.Error("cannot ensure docker network", "err", err)
	}
	now := time.Now().UTC()
	_ = e.Store.Update(func(st *store.State) error {
		for _, a := range st.Apps {
			for _, d := range a.Deployments {
				if !d.Terminal() {
					d.Status = api.StatusFailed
					d.Error = api.Errorf(api.CodeDeployFailed, "the saba daemon restarted while this deployment was in progress").
						WithHint("Deploy again.")
					d.FinishedAt = &now
					d.Replicas = nil
					d.Image = ""
					delete(a.EnvSnapshots, d.Version)
				}
			}
		}
		return nil
	})
	st := e.Store.Snapshot()
	all, err := e.RT.List(ctx, map[string]string{LabelManaged: "true"})
	if err != nil {
		e.Log.Error("cannot list containers", "err", err)
	}
	for _, c := range all {
		a := st.Apps[c.Labels[LabelApp]]
		if a == nil {
			// Never delete what the state does not know about: a lost or
			// replaced state file must not take running apps down with it.
			e.Log.Warn("container belongs to an app unknown to this daemon; leaving it alone", "container", c.Name, "app", c.Labels[LabelApp])
			continue
		}
		ver, _ := strconv.Atoi(c.Labels[LabelVersion])
		orphan := c.Labels[LabelRole] != RoleApp || ver != a.CurrentVersion
		switch {
		case orphan:
			e.Log.Info("removing orphaned container", "container", c.Name)
			_ = e.RT.Remove(ctx, c.Name)
		case !a.Stopped && !c.Running() && c.State != "restarting":
			e.Log.Info("starting container", "container", c.Name)
			if err := e.RT.Start(ctx, c.Name); err != nil {
				e.Log.Warn("cannot start container", "container", c.Name, "err", err)
			}
		}
	}
	e.RefreshRoutes()
}
