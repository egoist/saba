package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/buildpack"
	"github.com/egoist/saba/internal/proxy"
	"github.com/egoist/saba/internal/runtime"
	"github.com/egoist/saba/internal/shellwords"
	"github.com/egoist/saba/internal/store"
)

var errDryRun = errors.New("dry run")

// Deploy validates and applies a deploy request.
//
// For dry-run and no-deploy requests it returns a result immediately. Otherwise
// it records a new deployment, starts it in the background and returns a Run
// whose event log can be followed. The deployment keeps going if the caller
// goes away. contextDir, if set, is an extracted source upload that the engine
// takes ownership of.
func (e *Engine) Deploy(name string, req api.DeployRequest, contextDir string) (*Run, *api.DeployResult, error) {
	cleanup := func() {
		if contextDir != "" {
			os.RemoveAll(contextDir)
		}
	}
	if err := api.ValidateName(name); err != nil {
		cleanup()
		return nil, nil, err
	}
	if req.Source.Type == "upload" && contextDir == "" && !req.DryRun {
		return nil, nil, api.Errorf(api.CodeInvalid, "source type \"upload\" requires a build context")
	}

	// IfLive requests may turn into no-deploy ones; decide that up front.
	if req.IfLive && !req.NoDeploy {
		if a, err := e.Store.App(name); err == nil && (a.Current() == nil || a.Stopped) {
			req.NoDeploy = true
		}
	}
	immediate := req.DryRun || req.NoDeploy
	run := &Run{App: name}
	if req.NoDeploy && !req.DryRun && e.Busy(name) {
		cleanup()
		return nil, nil, api.Errorf(api.CodeDeployInProgress, "app %q is being deployed; its config cannot be changed right now", name).
			WithHint("Wait for the deployment to finish, then retry.")
	}
	if !immediate {
		if err := e.lock(name, run); err != nil {
			cleanup()
			return nil, nil, err
		}
	}

	var (
		plan []string
		dep  *api.Deployment
		urls []string
	)
	err := e.Store.Update(func(st *store.State) error {
		var err error
		dep, plan, urls, err = e.prepare(st, name, &req)
		if err != nil {
			return err
		}
		if req.DryRun {
			return errDryRun
		}
		return nil
	})
	if err != nil && !errors.Is(err, errDryRun) {
		if !immediate {
			e.unlock(name, run)
		}
		cleanup()
		return nil, nil, err
	}
	if immediate {
		cleanup()
		return nil, &api.DeployResult{App: name, Plan: plan, URLs: urls, DryRun: req.DryRun, Saved: !req.DryRun}, nil
	}

	run.Version = dep.Version
	run.Events = newEventLog(e.eventPath(name, dep.Version))
	e.mu.Lock()
	e.logs[eventKey(name, dep.Version)] = run.Events
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), deployTimeout)
	run.cancel = cancel
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer cancel()
		e.runDeploy(ctx, run, contextDir)
	}()
	return run, nil, nil
}

// Cancel aborts the deployment in progress for an app.
func (e *Engine) Cancel(app string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.busy[app]
	if r == nil || r.cancel == nil {
		return api.Errorf(api.CodeConflict, "no deployment is in progress for %q", app)
	}
	r.cancel()
	return nil
}

// prepare validates a deploy request against the state and records its
// outcome: saved config for no-deploy requests, or a new deployment record.
//
// Deploys are atomic: the requested config and env are stored on the
// deployment and only become the app's saved config once it goes live, so a
// failed deploy changes nothing.
func (e *Engine) prepare(st *store.State, name string, req *api.DeployRequest) (*api.Deployment, []string, []string, error) {
	var plan []string
	now := time.Now().UTC()
	src := req.Source
	a := st.Apps[name]

	if a == nil {
		switch src.Type {
		case "upload", "git", "image":
		default:
			return nil, nil, nil, store.NotFound(name)
		}
		kind := req.Kind
		if kind == "" {
			kind = api.KindApp
		}
		a = &store.AppRecord{App: api.App{Name: name, Kind: kind, Database: req.Database, CreatedAt: now, UpdatedAt: now}}
		a.Outputs = req.Outputs
		if !req.DryRun {
			st.Apps[name] = a
		}
		plan = append(plan, fmt.Sprintf("create %s %q", kind, name))
	} else if req.CreateOnly {
		return nil, nil, nil, api.Errorf(api.CodeConflict, "%q already exists", name).
			WithHint("Pick another name, or remove it first with `saba destroy %s --yes`.", name)
	}
	if req.IfLive && (a.Current() == nil || a.Stopped) {
		req.NoDeploy = true
	}

	// Work on copies; nothing is written to the app until the end.
	cfg := cloneConfig(a.Config)
	env := copyMap(a.Env)

	var image string
	switch src.Type {
	case "rollback":
		target, err := rollbackTarget(a, src.Version)
		if err != nil {
			return nil, nil, nil, err
		}
		src.Version = target.Version
		cfg = cloneConfig(target.Config)
		if snap, ok := a.EnvSnapshots[target.Version]; ok {
			env = copyMap(snap)
		}
		image = target.Image
		plan = append(plan, fmt.Sprintf("roll back to v%d (image, config and env)", target.Version))
	case "upload", "git", "image", "current", "":
	default:
		return nil, nil, nil, api.Errorf(api.CodeInvalid, "unknown source type %q", src.Type).
			WithHint("Use one of: upload, git, image, current, rollback.")
	}

	if len(req.Config) > 0 {
		merged, err := mergeConfig(cfg, req.Config)
		if err != nil {
			return nil, nil, nil, err
		}
		cfg = merged
	}
	for _, k := range req.EnvUnset {
		delete(env, k)
	}
	for k, v := range req.EnvSet {
		if !api.ValidEnvKey(k) {
			return nil, nil, nil, api.Errorf(api.CodeInvalid, "invalid environment variable name %q", k).
				WithHint("Names use letters, digits and underscores and cannot start with a digit.")
		}
		env[k] = v
	}
	if len(req.AddDomains)+len(req.RemoveDomains) > 0 {
		remove := map[string]bool{}
		for _, d := range req.RemoveDomains {
			remove[api.NormalizeDomain(d)] = true
		}
		var domains []string
		for _, d := range cfg.Domains {
			if !remove[d] {
				domains = append(domains, d)
			}
		}
		cfg.Domains = append(domains, req.AddDomains...)
	}

	cur := a.Current()
	switch src.Type {
	case "image":
		if src.Image != "" {
			cfg.Image = src.Image
		}
		if cfg.Image == "" {
			return nil, nil, nil, api.Errorf(api.CodeInvalid, "no image specified").
				WithHint("Pass `--image <ref>` or set `image` in saba.toml.")
		}
		src.Image = cfg.Image
		plan = append(plan, "pull image "+src.Image)
	case "upload":
		cfg.Image = ""
		plan = append(plan, "build image from the uploaded source")
	case "git":
		if src.Git == "" {
			return nil, nil, nil, api.Errorf(api.CodeInvalid, "source type \"git\" requires a repository URL")
		}
		cfg.Image = ""
		plan = append(plan, "clone "+redactURL(src.Git)+" and build image")
	case "current", "":
		src.Type = "current"
		switch {
		case cur != nil && cur.Image != "" && cfg.Image != "" && cfg.Image != cur.Config.Image:
			// A changed `image` in config means "deploy that image", not "restart".
			src = api.Source{Type: "image", Image: cfg.Image}
			plan = append(plan, "pull image "+src.Image)
		case cur != nil && cur.Image != "":
			image = cur.Image
			plan = append(plan, fmt.Sprintf("reuse the image of v%d", cur.Version))
		case cfg.Image != "":
			src = api.Source{Type: "image", Image: cfg.Image}
			plan = append(plan, "pull image "+src.Image)
		case !req.NoDeploy:
			return nil, nil, nil, api.Errorf(api.CodeConflict, "%q has no running version to redeploy", name).
				WithHint("Deploy it from source or an image first: `saba deploy -a %s`.", name)
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, nil, nil, err
	}
	if a.Kind == api.KindDatabase && cfg.EffectiveReplicas() > 1 {
		return nil, nil, nil, api.Errorf(api.CodeInvalid, "databases run a single replica")
	}
	// Validate against a candidate record without touching the saved one.
	candidate := *a
	candidate.Config, candidate.Env = cfg, env
	if err := checkDomainConflicts(st, &candidate); err != nil {
		return nil, nil, nil, err
	}
	if _, err := ResolveEnv(st, &candidate, env); err != nil {
		return nil, nil, nil, err
	}
	planCfg := cfg
	if planCfg.Port == 0 && cur != nil {
		planCfg.Port = cur.Port
	}
	var urls []string
	for _, h := range Hosts(st.Server, name, a.Kind, &planCfg) {
		if !strings.HasPrefix(h, "*.") {
			urls = append(urls, e.scheme()+"://"+h+e.URLSuffix)
		}
	}

	if cfg.ReleaseCommand != "" {
		plan = append(plan, "run release command: "+cfg.ReleaseCommand)
	}
	plan = append(plan, fmt.Sprintf("start %d replica(s) with the %s strategy", cfg.EffectiveReplicas(), cfg.EffectiveStrategy()))
	if cfg.EffectiveStrategy() == api.StrategyRolling || cur == nil {
		plan = append(plan, "wait for health checks, switch traffic, then drain and stop the old version")
	} else {
		plan = append(plan, "hold incoming requests, stop the old version, start the new one, resume traffic")
	}

	if req.DryRun {
		return nil, plan, urls, nil
	}
	a.UpdatedAt = now
	if req.NoDeploy {
		a.Config, a.Env = cfg, env
		a.PendingChanges = pending(a)
		msg := "saved; takes effect on the next deploy"
		if a.PendingChanges {
			msg = fmt.Sprintf("saved; apply it with `saba restart -a %s`", name)
		}
		return nil, []string{msg}, urls, nil
	}

	a.LastVersion++
	status := api.StatusDeploying
	if src.Type == "upload" || src.Type == "git" || src.Type == "image" {
		status = api.StatusBuilding
	}
	trigger := req.Trigger
	if trigger == "" {
		trigger = "deploy"
		if src.Type == "rollback" {
			trigger = "rollback"
		}
	}
	dep := &api.Deployment{
		Version:   a.LastVersion,
		Status:    status,
		Trigger:   trigger,
		Message:   req.Message,
		Source:    src,
		Image:     image,
		Config:    cfg,
		CreatedAt: now,
	}
	a.Deployments = append(a.Deployments, dep)
	if a.EnvSnapshots == nil {
		a.EnvSnapshots = map[int]map[string]string{}
	}
	a.EnvSnapshots[dep.Version] = env
	return dep, plan, urls, nil
}

func cloneConfig(c api.AppConfig) api.AppConfig {
	b, _ := json.Marshal(c)
	var out api.AppConfig
	_ = json.Unmarshal(b, &out)
	return out
}

func rollbackTarget(a *store.AppRecord, version int) (*api.Deployment, error) {
	if version > 0 {
		d := a.Deployment(version)
		switch {
		case d == nil:
			return nil, api.Errorf(api.CodeNotFound, "%q has no version v%d", a.Name, version).
				WithHint("Run `saba deployments -a %s` to list versions.", a.Name)
		case d.Version == a.CurrentVersion:
			return nil, api.Errorf(api.CodeConflict, "v%d is already the live version", version)
		case d.Status != api.StatusSuperseded:
			return nil, api.Errorf(api.CodeConflict, "v%d never went live (status: %s) and cannot be rolled back to", version, d.Status)
		case d.Image == "":
			return nil, api.Errorf(api.CodeConflict, "the image for v%d has been pruned", version).
				WithHint("Only recent versions are kept. Run `saba deployments -a %s` to see which can be restored.", a.Name)
		}
		return d, nil
	}
	for i := len(a.Deployments) - 1; i >= 0; i-- {
		d := a.Deployments[i]
		if d.Status == api.StatusSuperseded && d.Image != "" && d.Version != a.CurrentVersion {
			return d, nil
		}
	}
	return nil, api.Errorf(api.CodeConflict, "%q has no previous version to roll back to", a.Name)
}

// mergeConfig applies a JSON merge patch to cfg, rejecting unknown keys.
func mergeConfig(cfg api.AppConfig, patch map[string]any) (api.AppConfig, error) {
	b, _ := json.Marshal(cfg)
	var cur map[string]any
	_ = json.Unmarshal(b, &cur)
	merged := mergePatch(cur, patch)
	b, _ = json.Marshal(merged)
	var out api.AppConfig
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "unknown field") {
			field := strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`)
			return out, api.Errorf(api.CodeInvalid, "unknown config key %q", field).
				WithHint("Valid keys: image, build, port, domains, internal, worker, replicas, command, release_command, strategy, health, resources, volumes, publish, cron, drain_timeout, stop_timeout. See `saba guide`.")
		}
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			return out, api.Errorf(api.CodeInvalid, "config key %q has the wrong type: expected %s, got %s", te.Field, te.Type, te.Value)
		}
		return out, api.Errorf(api.CodeInvalid, "invalid config: %v", err)
	}
	return out, nil
}

func mergePatch(dst map[string]any, patch map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for k, v := range patch {
		if v == nil {
			delete(dst, k)
			continue
		}
		if pm, ok := v.(map[string]any); ok {
			dm, _ := dst[k].(map[string]any)
			dst[k] = mergePatch(dm, pm)
			continue
		}
		dst[k] = v
	}
	return dst
}

func checkDomainConflicts(st *store.State, a *store.AppRecord) error {
	mine := map[string]bool{}
	for _, d := range a.Config.Domains {
		mine[d] = true
	}
	if len(mine) == 0 {
		return nil
	}
	for _, other := range st.Apps {
		if other.Name == a.Name {
			continue
		}
		domains := other.Config.Domains
		if len(domains) == 0 && st.Server.BaseDomain != "" {
			domains = []string{other.Name + "." + st.Server.BaseDomain}
		}
		for _, d := range domains {
			if mine[d] {
				return api.Errorf(api.CodeConflict, "domain %s is already used by app %q", d, other.Name).
					WithHint("Remove it there first: `saba domains remove -a %s %s`.", other.Name, d)
			}
		}
	}
	return nil
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// runDeploy drives one deployment to a terminal state.
func (e *Engine) runDeploy(ctx context.Context, r *Run, contextDir string) {
	start := time.Now()
	// Deferred as a safety net; the normal paths below release explicitly.
	defer e.unlock(r.App, r)
	defer r.Events.Close()
	if contextDir != "" {
		defer os.RemoveAll(contextDir)
	}

	err := e.execute(ctx, r, contextDir)
	finished := time.Now().UTC()
	elapsed := finished.Sub(start).Milliseconds()

	if err != nil {
		ae := api.AsError(err)
		if ctx.Err() != nil {
			ae = api.Errorf(api.CodeDeployFailed, "deployment v%d was canceled or timed out", r.Version)
		}
		if ae.Code == api.CodeInternal {
			ae.Code = api.CodeDeployFailed
		}
		cctx, cancel := bg(ctx, 2*time.Minute)
		defer cancel()
		e.removeContainers(cctx, r.App, func(c *runtime.Container) bool {
			return c.Labels[LabelVersion] == strconv.Itoa(r.Version)
		}, time.Second)
		_ = e.RT.RemoveImage(cctx, imageTag(r.App, r.Version))
		_ = e.Store.UpdateApp(r.App, func(a *store.AppRecord) error {
			if d := a.Deployment(r.Version); d != nil {
				d.Status = api.StatusFailed
				d.Error = ae
				d.FinishedAt = &finished
				d.DurationMS = elapsed
				d.Image = ""
				d.Replicas = nil
			}
			delete(a.EnvSnapshots, r.Version)
			return nil
		})
		e.Log.Warn("deployment failed", "app", r.App, "version", r.Version, "code", ae.Code, "err", ae.Message)
		// Release the app before announcing the outcome: a client that reacts
		// to the final event must be able to start its next operation at once.
		e.unlock(r.App, r)
		r.Events.Append(api.Event{Type: "error", Error: ae})
		r.Events.Close()
		return
	}

	var result api.DeployResult
	_ = e.Store.UpdateApp(r.App, func(a *store.AppRecord) error {
		if d := a.Deployment(r.Version); d != nil {
			d.FinishedAt = &finished
			d.DurationMS = elapsed
			dc := *d
			result.Deployment = &dc
		}
		return nil
	})
	st := e.Store.Snapshot()
	if a := st.Apps[r.App]; a != nil {
		result.URLs = e.URLs(st.Server, a)
	}
	result.App = r.App
	result.Deployed = true
	e.Log.Info("deployment live", "app", r.App, "version", r.Version, "ms", elapsed)
	e.unlock(r.App, r)
	r.Events.Append(api.Event{Type: "result", Message: fmt.Sprintf("v%d is live", r.Version), Result: &result})
	r.Events.Close()

	pctx, cancel := bg(ctx, 2*time.Minute)
	defer cancel()
	e.pruneVersions(pctx, r.App)
}

func (e *Engine) execute(ctx context.Context, r *Run, contextDir string) error {
	st := e.Store.Snapshot()
	a := st.Apps[r.App]
	if a == nil {
		return store.NotFound(r.App)
	}
	dep := a.Deployment(r.Version)
	if dep == nil {
		return api.Errorf(api.CodeInternal, "deployment record v%d is missing", r.Version)
	}
	cfg := dep.Config

	image, builder, commit, err := e.resolveImage(ctx, r, a, dep, contextDir)
	if err != nil {
		return err
	}

	// Effective HTTP port: explicit config wins, otherwise the image's EXPOSE.
	port := cfg.Port
	if cfg.Worker {
		port = 0
	} else if port == 0 && a.Kind == api.KindApp {
		if img, err := e.RT.InspectImage(ctx, image); err == nil && len(img.ExposedPorts) > 0 {
			port = img.ExposedPorts[0]
			r.logf("deploy", "Detected port %d from the image (set `port` to override, or `worker = true` if this app does not serve HTTP).", port)
		}
	}
	if a.Kind == api.KindApp && port == 0 && !cfg.Worker {
		r.logf("deploy", "No port configured or exposed by the image: deploying as a worker without HTTP routing. Set `port` to route traffic to it.")
	}

	env, err := ResolveEnv(st, a, a.EnvSnapshots[r.Version])
	if err != nil {
		return err
	}
	if port > 0 {
		if _, ok := env["PORT"]; !ok {
			env["PORT"] = strconv.Itoa(port)
		}
	}
	env["SABA_APP"] = a.Name
	env["SABA_VERSION"] = strconv.Itoa(r.Version)

	if err := e.Store.UpdateApp(r.App, func(a *store.AppRecord) error {
		d := a.Deployment(r.Version)
		d.Status = api.StatusDeploying
		d.Image = image
		d.Port = port
		if builder != "" {
			d.Source.Builder = builder
		}
		if commit != "" {
			d.Source.Commit = commit
		}
		return nil
	}); err != nil {
		return err
	}
	dep.Image, dep.Port = image, port

	if err := e.RT.EnsureNetwork(ctx, Network); err != nil {
		return api.Errorf(api.CodeRuntime, "cannot create the docker network: %v", err)
	}

	if cfg.ReleaseCommand != "" {
		if err := e.release(ctx, r, a, dep, env); err != nil {
			return err
		}
	}

	var replicas []api.Replica
	if cfg.EffectiveStrategy() == api.StrategyRecreate && a.CurrentVersion > 0 {
		replicas, err = e.recreate(ctx, r, st.Server, a, dep, env)
	} else {
		replicas, err = e.rolling(ctx, r, st.Server, a, dep, env)
	}
	if err != nil {
		return err
	}

	// The new version is serving. Record it, then retire the old one.
	if err := e.Store.UpdateApp(r.App, func(a *store.AppRecord) error {
		for _, d := range a.Deployments {
			if d.Status == api.StatusLive {
				d.Status = api.StatusSuperseded
				d.Replicas = nil
			}
		}
		d := a.Deployment(r.Version)
		d.Status = api.StatusLive
		d.Replicas = replicas
		a.CurrentVersion = r.Version
		a.Stopped = false
		// The deployed config and env become the app's saved state.
		a.Config = cloneConfig(d.Config)
		a.Env = copyMap(a.EnvSnapshots[r.Version])
		a.PendingChanges = false
		return nil
	}); err != nil {
		return err
	}

	cctx, cancel := bg(ctx, 5*time.Minute)
	defer cancel()
	removed := e.removeContainers(cctx, r.App, func(c *runtime.Container) bool {
		return c.Labels[LabelRole] == RoleApp && c.Labels[LabelVersion] != strconv.Itoa(r.Version)
	}, dur(cfg.StopTimeout, defaultStopTimeout))
	if removed > 0 {
		r.phase("cleanup", "Stopped %d old container(s)", removed)
	}
	return nil
}

// rolling starts the new version next to the old one, waits for it to become
// healthy, switches traffic atomically and drains the old version.
func (e *Engine) rolling(ctx context.Context, r *Run, server api.ServerConfig, a *store.AppRecord, dep *api.Deployment, env map[string]string) ([]api.Replica, error) {
	replicas, err := e.startReplicas(ctx, r, a, dep, env)
	if err != nil {
		return nil, err
	}
	if err := e.waitHealthy(ctx, r, a, dep, replicas); err != nil {
		return nil, err
	}
	e.keepAlive(ctx, replicas)
	old := e.switchTraffic(r, server, a, dep, replicas)
	e.retire(ctx, r, old, dep.Config.DrainTimeout)
	return replicas, nil
}

// recreate replaces the old version in place. Incoming requests are held at
// the proxy while the swap happens, so clients see a pause instead of errors.
// If the new version fails, the old containers are started again.
func (e *Engine) recreate(ctx context.Context, r *Run, server api.ServerConfig, a *store.AppRecord, dep *api.Deployment, env map[string]string) ([]api.Replica, error) {
	cfg := dep.Config
	e.Proxy.Pause(a.Name)
	defer e.Proxy.Resume(a.Name)
	// Requests are held from here on, so the pool only empties.
	e.retire(ctx, r, e.Proxy.Pool(a.Name), cfg.DrainTimeout)

	sctx, cancel := bg(ctx, 5*time.Minute)
	defer cancel()
	old, _ := e.RT.List(sctx, map[string]string{LabelApp: a.Name, LabelRole: RoleApp})
	var stopped []string
	if len(old) > 0 {
		r.phase("stop", "Stopping v%d (requests are held until the new version is ready)", a.CurrentVersion)
	}
	for _, c := range old {
		if c.Labels[LabelVersion] == strconv.Itoa(dep.Version) {
			continue
		}
		wasRunning := c.Running()
		if err := e.RT.Stop(sctx, c.Name, dur(cfg.StopTimeout, defaultStopTimeout)); err != nil {
			e.Log.Warn("stop failed", "container", c.Name, "err", err)
		}
		if wasRunning {
			stopped = append(stopped, c.Name)
		}
	}

	replicas, err := e.startReplicas(ctx, r, a, dep, env)
	if err == nil {
		err = e.waitHealthy(ctx, r, a, dep, replicas)
	}
	if err != nil {
		if len(stopped) > 0 {
			r.phase("restore", "Restoring v%d", a.CurrentVersion)
			// The failed containers must release volumes and ports first.
			e.removeContainers(sctx, a.Name, func(c *runtime.Container) bool {
				return c.Labels[LabelVersion] == strconv.Itoa(dep.Version)
			}, time.Second)
			for _, name := range stopped {
				if serr := e.RT.Start(sctx, name); serr != nil {
					e.Log.Error("could not restore old container", "container", name, "err", serr)
					r.logf("deploy", "Could not restart %s: %v", name, serr)
				}
			}
			// Give the restored version a chance to boot before held requests reach it.
			if cur := a.Current(); cur != nil {
				quiet := &Run{App: a.Name, Events: newEventLog("")}
				hctx, hcancel := context.WithTimeout(sctx, 30*time.Second)
				_ = e.waitHealthy(hctx, quiet, a, cur, cur.Replicas)
				hcancel()
			}
		}
		return nil, err
	}
	e.keepAlive(ctx, replicas)
	if old := e.switchTraffic(r, server, a, dep, replicas); old != nil {
		old.Close()
	}
	return replicas, nil
}

// keepAlive makes healthy replicas restart on crashes and host reboots.
func (e *Engine) keepAlive(ctx context.Context, replicas []api.Replica) {
	for _, rep := range replicas {
		if err := e.RT.SetRestart(ctx, rep.Container, "unless-stopped"); err != nil {
			e.Log.Warn("cannot set restart policy", "container", rep.Container, "err", err)
		}
	}
}

// pending reports whether the app's saved config or env differs from what
// its live version runs with.
func pending(a *store.AppRecord) bool {
	cur := a.Current()
	if cur == nil {
		return false
	}
	if !reflect.DeepEqual(normalize(a.Config), normalize(cur.Config)) {
		return true
	}
	snap, ok := a.EnvSnapshots[cur.Version]
	return ok && !(len(snap) == 0 && len(a.Env) == 0) && !reflect.DeepEqual(snap, a.Env)
}

// normalize round-trips a config through JSON so that nil and empty
// collections compare equal.
func normalize(c api.AppConfig) map[string]any {
	b, _ := json.Marshal(c)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func (e *Engine) switchTraffic(r *Run, server api.ServerConfig, a *store.AppRecord, dep *api.Deployment, replicas []api.Replica) *proxy.Pool {
	cfg := dep.Config
	cfg.Port = dep.Port
	hosts := Hosts(server, a.Name, a.Kind, &cfg)
	if len(hosts) == 0 {
		return e.Proxy.RemoveApp(a.Name)
	}
	r.phase("switch", "Routing traffic to v%d", dep.Version)
	return e.Proxy.SetApp(a.Name, dep.Version, hosts, addrs(replicas))
}

func (e *Engine) drain(ctx context.Context, r *Run, pool *proxy.Pool, timeout string) {
	n := pool.InFlight()
	if n == 0 {
		return
	}
	r.phase("drain", "Waiting for %d in-flight request(s) on v%d to finish", n, pool.Version)
	dctx, cancel := bg(ctx, dur(timeout, defaultDrainTimeout))
	defer cancel()
	if left := pool.Drain(dctx); left > 0 {
		r.logf("deploy", "Drain timeout reached; %d request(s) will be interrupted.", left)
	}
}

// retire drains a replaced pool and closes its keep-alive connections so the
// old containers can exit promptly.
func (e *Engine) retire(ctx context.Context, r *Run, pool *proxy.Pool, timeout string) {
	if pool == nil {
		return
	}
	e.drain(ctx, r, pool, timeout)
	pool.Close()
}

func (e *Engine) resolveImage(ctx context.Context, r *Run, a *store.AppRecord, dep *api.Deployment, contextDir string) (image, builder, commit string, err error) {
	tag := imageTag(a.Name, dep.Version)
	switch dep.Source.Type {
	case "image":
		ref := dep.Source.Image
		r.phase("pull", "Pulling %s", ref)
		lw := runtime.NewLineWriter(func(line string) {
			// Per-layer progress is noise; keep the digest and status lines.
			if strings.Contains(line, ": ") && !strings.HasPrefix(line, "Digest:") && !strings.HasPrefix(line, "Status:") {
				return
			}
			r.logf("pull", "%s", line)
		})
		perr := e.RT.Pull(ctx, ref, lw)
		lw.Flush()
		if perr != nil {
			if ctx.Err() != nil {
				return "", "", "", ctx.Err()
			}
			if _, ierr := e.RT.InspectImage(ctx, ref); ierr != nil {
				return "", "", "", api.Errorf(api.CodeBuildFailed, "could not pull image %s: %v", ref, perr).
					WithHint("Check the image name and tag. For private registries, run `docker login` on the server.")
			}
			r.logf("pull", "Pull failed (%v); using the image already present on the server.", perr)
		}
		// Pin what was pulled so that rollbacks do not depend on a mutable tag.
		if err := e.RT.TagImage(ctx, ref, tag); err != nil {
			return "", "", "", api.Errorf(api.CodeRuntime, "cannot tag image: %v", err)
		}
		return tag, "", "", nil

	case "upload", "git":
		dir := contextDir
		work, err := os.MkdirTemp(filepath.Join(e.DataDir, "tmp"), "build-")
		if err != nil {
			return "", "", "", api.Errorf(api.CodeInternal, "cannot create build directory: %v", err)
		}
		defer os.RemoveAll(work)
		if dep.Source.Type == "git" {
			dir = filepath.Join(work, "src")
			r.phase("clone", "Cloning %s", redactURL(dep.Source.Git))
			commit, err = gitClone(ctx, dep.Source.Git, dep.Source.Ref, dir, r.lineWriter("clone"))
			if err != nil {
				return "", "", "", api.Errorf(api.CodeBuildFailed, "git clone failed: %v", err).
					WithHint("Check the URL and ref. Private repositories need credentials in the URL or an SSH key on the server.")
			}
		}
		plan, err := buildpack.Resolve(dir, work, dep.Config.Build)
		if err != nil {
			return "", "", "", err
		}
		r.phase("build", "Building image (%s)", plan.Builder)
		for _, n := range plan.Notes {
			r.logf("build", "%s", n)
		}
		opts := runtime.BuildOptions{
			ContextDir: dir,
			Dockerfile: plan.Dockerfile,
			Tag:        tag,
			Labels:     map[string]string{LabelManaged: "true", LabelApp: a.Name},
		}
		if b := dep.Config.Build; b != nil {
			opts.Target = b.Target
			opts.Args = b.Args
		}
		lw := r.lineWriter("build")
		berr := e.RT.Build(ctx, opts, lw)
		lw.Flush()
		if berr != nil {
			if ctx.Err() != nil {
				return "", "", "", ctx.Err()
			}
			return "", "", "", api.Errorf(api.CodeBuildFailed, "image build failed: %v", berr).
				WithHint("The build output above shows the failing step. Reproduce locally with `docker build .`.")
		}
		return tag, plan.Builder, commit, nil

	default: // current, rollback
		if dep.Image == "" {
			return "", "", "", api.Errorf(api.CodeDeployFailed, "no image is available to redeploy")
		}
		if _, err := e.RT.InspectImage(ctx, dep.Image); err != nil {
			return "", "", "", api.Errorf(api.CodeDeployFailed, "image %s is missing on the server: %v", dep.Image, err).
				WithHint("Deploy again from source or a registry image: `saba deploy -a %s`.", a.Name)
		}
		return dep.Image, "", "", nil
	}
}

func gitClone(ctx context.Context, url, ref, dir string, out *runtime.LineWriter) (string, error) {
	defer out.Flush()
	if ref == "" {
		ref = "HEAD"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	steps := [][]string{
		{"init", "-q"},
		{"fetch", "--depth", "1", url, ref},
		{"-c", "advice.detachedHead=false", "checkout", "-q", "FETCH_HEAD"},
	}
	for _, args := range steps {
		c := exec.CommandContext(ctx, "git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		c.Stdout, c.Stderr = out, out
		if err := c.Run(); err != nil {
			return "", fmt.Errorf("git %s: %w", args[0], err)
		}
	}
	c := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	c.Dir = dir
	b, _ := c.Output()
	return strings.TrimSpace(string(b)), nil
}

// redactURL hides credentials embedded in a URL.
func redactURL(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		if j := strings.LastIndex(u, "@"); j > i {
			return u[:i+3] + "***@" + u[j+1:]
		}
	}
	return u
}

func (e *Engine) release(ctx context.Context, r *Run, a *store.AppRecord, dep *api.Deployment, env map[string]string) error {
	cmd, err := shellwords.Command(dep.Config.ReleaseCommand)
	if err != nil {
		return api.Errorf(api.CodeInvalid, "invalid release_command: %v", err)
	}
	r.phase("release", "Running release command: %s", dep.Config.ReleaseCommand)
	opts := e.runOptions(a, dep, env)
	opts.Name = fmt.Sprintf("saba-%s-release-v%d", a.Name, dep.Version)
	opts.Labels = labels(a.Name, dep.Version, 0, RoleRelease)
	opts.Command = cmd
	opts.Aliases = nil
	opts.Restart = ""
	_ = e.RT.Remove(ctx, opts.Name)
	lw := r.lineWriter("release")
	code, err := e.RT.RunOnce(ctx, opts, lw, lw)
	lw.Flush()
	if err != nil {
		return api.Errorf(api.CodeReleaseFailed, "release command could not run: %v", err)
	}
	if code != 0 {
		return api.Errorf(api.CodeReleaseFailed, "release command exited with code %d", code).
			WithDetail("exit_code", code).
			WithHint("The new version was not started and the current version keeps serving. Fix the command or its output above and deploy again.")
	}
	return nil
}

// runOptions builds the container options shared by replicas and one-off runs.
func (e *Engine) runOptions(a *store.AppRecord, dep *api.Deployment, env map[string]string) runtime.RunOptions {
	cfg := dep.Config
	opts := runtime.RunOptions{
		Image:   dep.Image,
		Env:     copyMap(env),
		Network: Network,
		Aliases: []string{a.Name},
	}
	for _, v := range cfg.Volumes {
		vol, err := api.ParseVolume(v)
		if err != nil {
			continue
		}
		src := vol.Source
		if !vol.Bind {
			src = volumeName(a.Name, vol.Source)
		}
		opts.Volumes = append(opts.Volumes, src+":"+vol.Target)
	}
	if cfg.Resources != nil {
		opts.Memory = cfg.Resources.Memory
		opts.CPUs = cfg.Resources.CPUs
	}
	return opts
}

func (e *Engine) startReplicas(ctx context.Context, r *Run, a *store.AppRecord, dep *api.Deployment, env map[string]string) ([]api.Replica, error) {
	cfg := dep.Config
	n := cfg.EffectiveReplicas()
	cmd, err := shellwords.Command(cfg.Command)
	if err != nil {
		return nil, api.Errorf(api.CodeInvalid, "invalid command: %v", err)
	}
	used := usedPorts(e.Store.Snapshot())
	r.phase("start", "Starting %d replica(s) of v%d", n, dep.Version)

	var replicas []api.Replica
	fail := func(err error) ([]api.Replica, error) {
		cctx, cancel := bg(ctx, time.Minute)
		defer cancel()
		for _, rep := range replicas {
			_ = e.RT.Remove(cctx, rep.Container)
		}
		return nil, err
	}
	for i := 1; i <= n; i++ {
		rep := api.Replica{Index: i, Container: containerName(a.Name, dep.Version, i)}
		opts := e.runOptions(a, dep, env)
		opts.Name = rep.Container
		opts.Labels = labels(a.Name, dep.Version, i, RoleApp)
		opts.Command = cmd
		// No restart policy until the replica has proven healthy: a container
		// that crashes on boot then stays down with its real exit code.
		opts.Restart = "no"
		opts.StopTimeout = dur(cfg.StopTimeout, defaultStopTimeout)
		opts.Env["SABA_REPLICA"] = strconv.Itoa(i)
		if dep.Port > 0 && a.Kind == api.KindApp {
			hp, err := e.allocPort(used)
			if err != nil {
				return fail(err)
			}
			rep.HostPort = hp
			opts.Ports = append(opts.Ports, fmt.Sprintf("127.0.0.1:%d:%d", hp, dep.Port))
		}
		opts.Ports = append(opts.Ports, cfg.Publish...)
		_ = e.RT.Remove(ctx, rep.Container) // leftover from a crashed attempt
		id, err := e.RT.Run(ctx, opts)
		if err != nil {
			if ctx.Err() != nil {
				return fail(ctx.Err())
			}
			return fail(api.Errorf(api.CodeRuntime, "could not start container %s: %v", rep.Container, err).
				WithHint("Check `command`, `volumes`, `publish` and `resources` in the app config."))
		}
		if len(id) > 12 {
			id = id[:12]
		}
		rep.ContainerID = id
		replicas = append(replicas, rep)
	}
	// Persist immediately so port reservations and crash recovery see them.
	_ = e.Store.UpdateApp(a.Name, func(a *store.AppRecord) error {
		if d := a.Deployment(dep.Version); d != nil {
			d.Replicas = replicas
		}
		return nil
	})
	return replicas, nil
}

// removeContainers stops and removes the app's containers selected by match.
func (e *Engine) removeContainers(ctx context.Context, app string, match func(*runtime.Container) bool, stopTimeout time.Duration) int {
	cs, err := e.RT.List(ctx, map[string]string{LabelApp: app})
	if err != nil {
		e.Log.Warn("cannot list containers", "app", app, "err", err)
		return 0
	}
	done := make(chan struct{})
	n := 0
	for i := range cs {
		c := &cs[i]
		if !match(c) {
			continue
		}
		n++
		go func() {
			defer func() { done <- struct{}{} }()
			if c.Running() {
				_ = e.RT.Stop(ctx, c.Name, stopTimeout)
			}
			if err := e.RT.Remove(ctx, c.Name); err != nil {
				e.Log.Warn("cannot remove container", "container", c.Name, "err", err)
			}
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
	return n
}

// pruneVersions drops images, env snapshots and history beyond the retention window.
func (e *Engine) pruneVersions(ctx context.Context, app string) {
	var remove []string
	var dropLogs []int
	_ = e.Store.Update(func(st *store.State) error {
		a := st.Apps[app]
		if a == nil {
			return nil
		}
		keep := st.Server.KeepVersions
		if keep <= 0 {
			keep = defaultKeepVersions
		}
		kept := map[string]bool{}
		var stale []*api.Deployment
		sort.Slice(a.Deployments, func(i, j int) bool { return a.Deployments[i].Version < a.Deployments[j].Version })
		n := 0
		for i := len(a.Deployments) - 1; i >= 0; i-- {
			d := a.Deployments[i]
			switch {
			case !d.Terminal(), d.Version == a.CurrentVersion:
				kept[d.Image] = true
			case d.Status == api.StatusSuperseded && d.Image != "" && n < keep:
				n++
				kept[d.Image] = true
			default:
				stale = append(stale, d)
			}
		}
		for _, d := range stale {
			if d.Image != "" && !kept[d.Image] && strings.HasPrefix(d.Image, "saba/"+app+":") {
				remove = append(remove, d.Image)
			}
			d.Image = ""
			delete(a.EnvSnapshots, d.Version)
		}
		if extra := len(a.Deployments) - maxDeployHistory; extra > 0 {
			for _, d := range a.Deployments[:extra] {
				dropLogs = append(dropLogs, d.Version)
			}
			a.Deployments = a.Deployments[extra:]
		}
		return nil
	})
	seen := map[string]bool{}
	for _, img := range remove {
		if !seen[img] {
			seen[img] = true
			_ = e.RT.RemoveImage(ctx, img)
		}
	}
	e.mu.Lock()
	for _, v := range dropLogs {
		delete(e.logs, eventKey(app, v))
		os.Remove(e.eventPath(app, v))
	}
	// Finished logs are served from disk; keep memory bounded.
	for k, l := range e.logs {
		l.mu.Lock()
		done := l.done
		l.mu.Unlock()
		if done && strings.HasPrefix(k, app+"/") {
			delete(e.logs, k)
		}
	}
	e.mu.Unlock()
}
