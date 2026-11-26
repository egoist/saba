package engine

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/runtime"
	"github.com/egoist/saba/internal/shellwords"
	"github.com/egoist/saba/internal/store"
)

func (e *Engine) healthClient() *http.Client {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.health == nil {
		e.health = &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{DisableKeepAlives: true, DialContext: e.Dial},
		}
	}
	return e.health
}

type healthSpec struct {
	mode     string // command, http, stable
	command  []string
	path     string
	strict   bool // an explicit path must answer 2xx/3xx
	host     string
	interval time.Duration
	timeout  time.Duration
}

func (h healthSpec) describe(port int) string {
	switch h.mode {
	case "command":
		return "command: " + strings.Join(h.command, " ")
	case "http":
		if h.strict {
			return fmt.Sprintf("GET %s on port %d must return 2xx/3xx", h.path, port)
		}
		return fmt.Sprintf("HTTP on port %d", port)
	default:
		return "container stays up"
	}
}

func (e *Engine) healthSpec(a *store.AppRecord, dep *api.Deployment, host string) (healthSpec, error) {
	h := healthSpec{mode: "stable", path: "/", host: host, interval: e.HealthInterval, timeout: defaultHealthTimeout}
	hc := dep.Config.Health
	if hc != nil {
		h.timeout = dur(hc.Timeout, h.timeout)
		h.interval = dur(hc.Interval, h.interval)
	}
	switch {
	case hc != nil && hc.Command != "":
		cmd, err := shellwords.Command(hc.Command)
		if err != nil {
			return h, api.Errorf(api.CodeInvalid, "invalid health.command: %v", err)
		}
		h.mode, h.command = "command", cmd
	case a.Kind == api.KindApp && dep.Port > 0:
		h.mode = "http"
		if hc != nil && hc.Path != "" {
			h.path, h.strict = hc.Path, true
		}
	}
	return h, nil
}

// waitHealthy blocks until every replica passes its health check, or fails
// with a diagnosis that includes the container's last log lines.
func (e *Engine) waitHealthy(ctx context.Context, r *Run, a *store.AppRecord, dep *api.Deployment, replicas []api.Replica) error {
	cfg := dep.Config
	cfg.Port = dep.Port
	host := ""
	if hosts := Hosts(e.Store.Snapshot().Server, a.Name, a.Kind, &cfg); len(hosts) > 0 && !strings.HasPrefix(hosts[0], "*.") {
		host = hosts[0]
	}
	spec, err := e.healthSpec(a, dep, host)
	if err != nil {
		return err
	}
	r.phase("health", "Waiting for v%d to become healthy (%s, timeout %s)", dep.Version, spec.describe(dep.Port), spec.timeout)

	hctx, cancel := context.WithTimeout(ctx, spec.timeout)
	defer cancel()
	type outcome struct {
		rep api.Replica
		err *api.Error
	}
	results := make(chan outcome, len(replicas))
	for _, rep := range replicas {
		go func() { results <- outcome{rep, e.checkReplica(hctx, spec, dep, rep)} }()
	}
	for range replicas {
		out := <-results
		if out.err == nil {
			continue
		}
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Capture the evidence before the failed containers are removed.
		lctx, lcancel := bg(ctx, 15*time.Second)
		var lines []string
		_ = e.rt(out.rep.Node).Logs(lctx, out.rep.Container, runtime.LogOptions{Tail: 40}, func(l runtime.LogLine) {
			lines = append(lines, l.Text)
		})
		defer lcancel()
		if len(lines) > 0 {
			r.logf("deploy", "Last output of %s:", out.rep.Container)
			for _, l := range lines {
				r.Events.Append(api.Event{Type: "log", Stream: "container", Replica: out.rep.Index, Message: l})
			}
			out.err.WithDetail("logs", lines)
		} else {
			r.logf("deploy", "%s produced no output.", out.rep.Container)
		}
		out.err.WithDetail("replica", out.rep.Index).WithDetail("version", dep.Version)

		if spec.mode == "http" {
			if img, ierr := e.RT.InspectImage(lctx, dep.Image); ierr == nil && len(img.ExposedPorts) > 0 && !containsInt(img.ExposedPorts, dep.Port) {
				out.err.Hint = fmt.Sprintf("The image exposes port %d but the app is configured with port %d — try `port = %d`. ", img.ExposedPorts[0], dep.Port, img.ExposedPorts[0]) + out.err.Hint
			}
		}
		return out.err
	}
	return nil
}

func (e *Engine) checkReplica(ctx context.Context, spec healthSpec, dep *api.Deployment, rep api.Replica) *api.Error {
	started := time.Now()
	last := "the check did not run"
	fail := func(reason, hint string) *api.Error {
		return api.Errorf(api.CodeUnhealthy, "v%d did not become healthy: %s", dep.Version, reason).WithHint("%s", hint)
	}
	keepHint := "The previous version keeps serving traffic. Fix the problem and deploy again."
	for {
		c, err := e.rt(rep.Node).Inspect(ctx, rep.Container)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			return fail(fmt.Sprintf("container %s disappeared: %v", rep.Container, err), keepHint)
		}
		switch {
		case c.OOMKilled:
			return fail("the container was killed for exceeding its memory limit",
				"Raise `resources.memory` or reduce the app's memory usage.")
		case c.State == "exited" || c.State == "dead" || c.Restarts > 0 || c.State == "restarting":
			return fail(fmt.Sprintf("the container exited with code %d shortly after starting", c.ExitCode),
				"The container output above usually says why (missing env var, bad command, crash on boot). "+keepHint)
		case c.Health == "unhealthy":
			return fail("the image's own HEALTHCHECK reports unhealthy", keepHint)
		}
		if c.Running() {
			ok, reason := e.probe(ctx, spec, c, rep)
			if ok {
				return nil
			}
			last = reason
		} else {
			last = "container state is " + c.State
		}
		select {
		case <-ctx.Done():
		case <-time.After(spec.interval):
			continue
		}
		break
	}
	waited := time.Since(started).Round(time.Second)
	var hint string
	switch spec.mode {
	case "http":
		hint = fmt.Sprintf("Make sure the app listens on 0.0.0.0:%d (not 127.0.0.1) — the PORT env var is set to that value. "+
			"If it listens on another port, set `port`. If it needs longer to boot, raise `health.timeout`. "+
			"If it does not serve HTTP, set `worker = true`.", dep.Port)
		if spec.strict {
			hint = fmt.Sprintf("GET %s must return a 2xx or 3xx status. ", spec.path) + hint
		}
	case "command":
		hint = "The health command must exit 0 inside the container. Raise `health.timeout` if the app needs longer to boot."
	default:
		hint = keepHint
	}
	return fail(fmt.Sprintf("%s (after %s)", last, waited), hint)
}

func (e *Engine) probe(ctx context.Context, spec healthSpec, c *runtime.Container, rep api.Replica) (bool, string) {
	switch spec.mode {
	case "command":
		code, err := e.rt(rep.Node).Exec(ctx, rep.Container, spec.command, nil, discard{}, discard{})
		if err != nil {
			return false, "health command failed to run: " + err.Error()
		}
		if code != 0 {
			return false, fmt.Sprintf("health command exited with code %d", code)
		}
		return true, ""
	case "http":
		url := fmt.Sprintf("http://%s:%d%s", replicaHost(rep), rep.HostPort, spec.path)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, err.Error()
		}
		if spec.host != "" {
			req.Host = spec.host
		}
		req.Header.Set("User-Agent", "saba-health/1")
		resp, err := e.healthClient().Do(req)
		if err != nil {
			return false, "the app is not answering HTTP requests"
		}
		resp.Body.Close()
		if spec.strict && (resp.StatusCode < 200 || resp.StatusCode > 399) {
			return false, fmt.Sprintf("GET %s returned %d", spec.path, resp.StatusCode)
		}
		if resp.StatusCode >= 500 {
			return false, fmt.Sprintf("GET %s returned %d", spec.path, resp.StatusCode)
		}
		return true, ""
	default:
		if c.Health == "healthy" {
			return true, ""
		}
		if !c.StartedAt.IsZero() && time.Since(c.StartedAt) >= e.StableWindow {
			return true, ""
		}
		return false, "waiting for the container to stay up"
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
