package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/proxy"
	"github.com/egoist/saba/internal/runtime"
	"github.com/egoist/saba/internal/runtime/runtimetest"
	"github.com/egoist/saba/internal/store"
)

type runtimeOptions = runtime.RunOptions

type harness struct {
	t      *testing.T
	engine *Engine
	rt     *runtimetest.Fake
	front  *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *store.State) error { s.Server.BaseDomain = "test"; return nil }); err != nil {
		t.Fatal(err)
	}
	rt := runtimetest.New()
	px := proxy.New(nil)
	e := New(st, rt, px, dir, nil)
	e.HealthInterval = 5 * time.Millisecond
	e.StableWindow = 20 * time.Millisecond
	e.PortMin, e.PortMax = 31000, 31999
	front := httptest.NewServer(px)
	t.Cleanup(func() {
		front.Close()
		e.Wait(10 * time.Second)
		for _, n := range rt.Names() {
			_ = rt.Remove(context.Background(), n)
		}
	})
	return &harness{t: t, engine: e, rt: rt, front: front}
}

// deploy runs a deploy request to completion and returns its outcome.
func (h *harness) deploy(app string, req api.DeployRequest) (*api.DeployResult, error) {
	h.t.Helper()
	run, result, err := h.engine.Deploy(app, req, "")
	if err != nil {
		return nil, err
	}
	if result != nil {
		return result, nil
	}
	var final *api.DeployResult
	var failure error
	_ = run.Events.Stream(context.Background(), true, func(ev api.Event) error {
		switch ev.Type {
		case "result":
			final = ev.Result
		case "error":
			failure = ev.Error
		}
		return nil
	})
	// The app must be free for the next operation as soon as the outcome is known.
	if h.engine.Busy(app) {
		h.t.Fatalf("%s is still locked after its deployment finished", app)
	}
	return final, failure
}

func (h *harness) mustDeploy(app string, req api.DeployRequest) *api.DeployResult {
	h.t.Helper()
	res, err := h.deploy(app, req)
	if err != nil {
		h.t.Fatalf("deploy %s failed: %v", app, err)
	}
	return res
}

func image(ref string) api.DeployRequest {
	return api.DeployRequest{Source: api.Source{Type: "image", Image: ref}}
}

// get requests the app through the proxy and returns status and body.
func (h *harness) get(host, path string) (int, string) {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.front.URL+path, nil)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("GET %s%s: %v", host, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func codeOf(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func TestDeployRoutesTraffic(t *testing.T) {
	h := newHarness(t)
	res := h.mustDeploy("web", image("ok:1"))
	if !res.Deployed || res.Deployment.Version != 1 || res.Deployment.Port != 8080 {
		t.Fatalf("unexpected result: %+v", res.Deployment)
	}
	if len(res.URLs) != 1 || res.URLs[0] != "http://web.test" {
		t.Fatalf("urls = %v", res.URLs)
	}
	if code, body := h.get("web.test", "/"); code != 200 || body != "ok v1" {
		t.Fatalf("got %d %q", code, body)
	}
	if code, _ := h.get("other.test", "/"); code != 404 {
		t.Fatalf("unknown host returned %d, want 404", code)
	}
}

// The core promise: requests never fail while new versions roll out.
func TestRollingDeployHasNoDowntime(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy("web", api.DeployRequest{Source: api.Source{Type: "image", Image: "slow:1"}, Config: map[string]any{"replicas": 2}})

	var ok, bad atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := "/"
			if i%2 == 0 {
				path = "/slow" // in flight across the switch, so draining matters
			}
			for {
				select {
				case <-stop:
					return
				default:
				}
				req, _ := http.NewRequest("GET", h.front.URL+path, nil)
				req.Host = "web.test"
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					bad.Add(1)
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 {
					ok.Add(1)
				} else {
					bad.Add(1)
				}
			}
		}(i)
	}
	for v := 2; v <= 5; v++ {
		h.mustDeploy("web", image("slow:1"))
	}
	close(stop)
	wg.Wait()

	if bad.Load() != 0 {
		t.Fatalf("%d of %d requests failed during rolling deploys", bad.Load(), ok.Load()+bad.Load())
	}
	if ok.Load() < 50 {
		t.Fatalf("only %d requests were made; the test did not exercise the rollout", ok.Load())
	}
	if _, body := h.get("web.test", "/"); body != "slow v5" {
		t.Fatalf("live version serves %q, want v5", body)
	}
	if names := h.rt.Names(); len(names) != 2 || names[0] != "saba-web-v5-1" {
		t.Fatalf("old containers were not cleaned up: %v", names)
	}
}

func TestFailedDeployKeepsCurrentVersionAndConfig(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy("web", image("ok:1"))

	for _, tc := range []struct {
		img, wantCode string
	}{
		{"crash:1", api.CodeUnhealthy},
		{"broken:1", api.CodeUnhealthy},
		{"missing:1", api.CodeBuildFailed},
	} {
		req := image(tc.img)
		req.Config = map[string]any{"replicas": 3, "health": map[string]any{"timeout": "300ms"}}
		req.EnvSet = map[string]string{"NEW": "1"}
		_, err := h.deploy("web", req)
		if codeOf(err) != tc.wantCode {
			t.Fatalf("%s: got error %v, want code %s", tc.img, err, tc.wantCode)
		}
		if code, body := h.get("web.test", "/"); code != 200 || body != "ok v1" {
			t.Fatalf("%s: old version no longer serving: %d %q", tc.img, code, body)
		}
	}

	var ae *api.Error
	_, err := h.deploy("web", image("crash:1"))
	if !errors.As(err, &ae) || ae.Details["logs"] == nil || ae.Hint == "" {
		t.Fatalf("crash error should carry logs and a hint: %+v", ae)
	}

	a, _ := h.engine.Store.App("web")
	if a.CurrentVersion != 1 || a.Config.Replicas != 0 || a.Env["NEW"] != "" || a.Config.Image != "ok:1" {
		t.Fatalf("a failed deploy must not change saved state: current=%d config=%+v env=%v", a.CurrentVersion, a.Config, a.Env)
	}
	if names := h.rt.Names(); len(names) != 1 || names[0] != "saba-web-v1-1" {
		t.Fatalf("failed containers were not cleaned up: %v", names)
	}
	if h.rt.HasImage(imageTag("web", 2)) {
		t.Fatal("image of the failed version was not removed")
	}
}

func TestRollbackRestoresImageConfigAndEnv(t *testing.T) {
	h := newHarness(t)
	r1 := image("ok:1")
	r1.EnvSet = map[string]string{"MODE": "one"}
	h.mustDeploy("web", r1)
	r2 := image("slow:2")
	r2.EnvSet = map[string]string{"MODE": "two"}
	r2.Config = map[string]any{"replicas": 2}
	h.mustDeploy("web", r2)
	if _, body := h.get("web.test", "/"); body != "slow v2" {
		t.Fatalf("got %q", body)
	}

	res := h.mustDeploy("web", api.DeployRequest{Source: api.Source{Type: "rollback"}})
	if res.Deployment.Version != 3 || res.Deployment.Trigger != "rollback" {
		t.Fatalf("unexpected rollback deployment: %+v", res.Deployment)
	}
	if _, body := h.get("web.test", "/"); body != "ok v3" {
		t.Fatalf("after rollback got %q, want the v1 image running as v3", body)
	}
	a, _ := h.engine.Store.App("web")
	if a.Env["MODE"] != "one" || a.Config.EffectiveReplicas() != 1 {
		t.Fatalf("rollback did not restore env/config: env=%v replicas=%d", a.Env, a.Config.Replicas)
	}

	_, err := h.deploy("web", api.DeployRequest{Source: api.Source{Type: "rollback", Version: 3}})
	if codeOf(err) != api.CodeConflict {
		t.Fatalf("rolling back to the live version should conflict, got %v", err)
	}
}

func TestRecreateHoldsRequestsInsteadOfFailing(t *testing.T) {
	h := newHarness(t)
	req := image("ok:1")
	req.Config = map[string]any{"strategy": "recreate"}
	h.mustDeploy("db", req)

	var bad atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if code, _ := h.get("db.test", "/"); code != 200 {
					bad.Add(1)
				}
			}
		}()
	}
	for i := 0; i < 3; i++ {
		h.mustDeploy("db", image("ok:1"))
	}
	close(stop)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d requests failed during recreate deploys", bad.Load())
	}

	// A failing recreate must bring the old version back.
	fail := image("crash:1")
	if _, err := h.deploy("db", fail); codeOf(err) != api.CodeUnhealthy {
		t.Fatalf("expected a health failure, got %v", err)
	}
	if code, body := h.get("db.test", "/"); code != 200 || body != "ok v4" {
		t.Fatalf("old version was not restored: %d %q", code, body)
	}
}

func TestReleaseCommandGatesTheRollout(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy("web", image("ok:1"))
	h.rt.SetRunOnce(func(runtimeOptions) int { return 1 })
	req := image("ok:2")
	req.Config = map[string]any{"release_command": "migrate now"}
	_, err := h.deploy("web", req)
	if codeOf(err) != api.CodeReleaseFailed {
		t.Fatalf("got %v, want release_failed", err)
	}
	if _, body := h.get("web.test", "/"); body != "ok v1" {
		t.Fatalf("got %q; a failed release must not replace the live version", body)
	}
	h.rt.SetRunOnce(nil)
	h.mustDeploy("web", req)
	if _, body := h.get("web.test", "/"); body != "ok v3" {
		t.Fatalf("got %q", body)
	}
}

func TestConfigValidationAndStaging(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy("web", image("ok:1"))

	bad := image("ok:1")
	bad.Config = map[string]any{"prot": 3000}
	_, err := h.deploy("web", bad)
	if codeOf(err) != api.CodeInvalid || !strings.Contains(err.Error(), `"prot"`) {
		t.Fatalf("unknown key should be rejected by name, got %v", err)
	}

	// Dry run changes nothing.
	dry := image("ok:2")
	dry.DryRun = true
	dry.Config = map[string]any{"replicas": 4}
	res := h.mustDeploy("web", dry)
	if !res.DryRun || len(res.Plan) == 0 {
		t.Fatalf("dry run result: %+v", res)
	}
	if a, _ := h.engine.Store.App("web"); a.LastVersion != 1 || a.Config.Replicas != 0 {
		t.Fatal("dry run changed state")
	}

	// no-deploy stages a change; restart applies it.
	stage := api.DeployRequest{Source: api.Source{Type: "current"}, EnvSet: map[string]string{"A": "1"}, NoDeploy: true}
	res = h.mustDeploy("web", stage)
	if !res.Saved {
		t.Fatalf("expected a saved result: %+v", res)
	}
	a, _ := h.engine.Store.App("web")
	if !a.PendingChanges || a.CurrentVersion != 1 {
		t.Fatalf("staged change should be pending: %+v", a.App)
	}
	h.mustDeploy("web", api.DeployRequest{Source: api.Source{Type: "current"}, Trigger: "restart"})
	a, _ = h.engine.Store.App("web")
	if a.PendingChanges || a.CurrentVersion != 2 || a.EnvSnapshots[2]["A"] != "1" {
		t.Fatalf("restart should apply the staged change: %+v", a.App)
	}
}

func TestDomainsAndConflicts(t *testing.T) {
	h := newHarness(t)
	req := image("ok:1")
	req.Config = map[string]any{"domains": []any{"Example.com", "www.example.com"}}
	h.mustDeploy("web", req)
	if code, _ := h.get("example.com", "/"); code != 200 {
		t.Fatalf("custom domain not routed: %d", code)
	}
	if code, _ := h.get("web.test", "/"); code != 404 {
		t.Fatalf("automatic domain should be replaced by custom domains, got %d", code)
	}

	other := image("ok:1")
	other.Config = map[string]any{"domains": []any{"example.com"}}
	if _, err := h.deploy("api", other); codeOf(err) != api.CodeConflict {
		t.Fatalf("expected a domain conflict, got %v", err)
	}

	h.mustDeploy("web", api.DeployRequest{Source: api.Source{Type: "current"}, RemoveDomains: []string{"www.example.com"}})
	if code, _ := h.get("www.example.com", "/"); code != 404 {
		t.Fatalf("removed domain still routed: %d", code)
	}
}

func TestEnvReferences(t *testing.T) {
	h := newHarness(t)
	db := image("ok:1")
	db.Kind = api.KindDatabase
	db.Outputs = map[string]string{"URL": "postgres://db:5432/app"}
	db.Config = map[string]any{"health": map[string]any{"command": "true"}}
	h.mustDeploy("db", db)

	app := image("ok:1")
	app.EnvSet = map[string]string{"DATABASE_URL": "${{db.URL}}?sslmode=disable", "PLAIN": "x"}
	h.mustDeploy("web", app)
	env, err := h.engine.Env("web")
	if err != nil {
		t.Fatal(err)
	}
	if env.Env["DATABASE_URL"] != "${{db.URL}}?sslmode=disable" || env.Resolved["DATABASE_URL"] != "postgres://db:5432/app?sslmode=disable" {
		t.Fatalf("unexpected env: %+v", env)
	}

	bad := api.DeployRequest{Source: api.Source{Type: "current"}, EnvSet: map[string]string{"X": "${{db.NOPE}}"}}
	if _, err := h.deploy("web", bad); codeOf(err) != api.CodeInvalid {
		t.Fatalf("dangling reference should be rejected, got %v", err)
	}
	if _, err := h.engine.Destroy(context.Background(), "db", true); codeOf(err) != api.CodeConflict {
		t.Fatalf("destroying a referenced app should conflict, got %v", err)
	}
	// Databases are never routed, even though they have a port.
	if code, _ := h.get("db.test", "/"); code != 404 {
		t.Fatalf("database is publicly routed: %d", code)
	}
}

func TestStopStartDestroy(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy("web", image("ok:1"))
	ctx := context.Background()
	if err := h.engine.Stop(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.get("web.test", "/"); code != 503 {
		t.Fatalf("stopped app returned %d, want 503", code)
	}
	if a, _ := h.engine.App(ctx, "web"); a.Status != api.AppStopped {
		t.Fatalf("status = %s", a.Status)
	}
	if err := h.engine.Start(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.get("web.test", "/"); code != 200 {
		t.Fatalf("started app returned %d", code)
	}
	if _, err := h.engine.Destroy(ctx, "web", false); err != nil {
		t.Fatal(err)
	}
	if len(h.rt.Names()) != 0 {
		t.Fatalf("containers left behind: %v", h.rt.Names())
	}
	if _, err := h.engine.App(ctx, "web"); codeOf(err) != api.CodeNotFound {
		t.Fatalf("destroyed app still exists: %v", err)
	}
}

func TestConcurrentDeploysAreRejected(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy("web", image("ok:1"))
	slow := image("silent:1")
	slow.Config = map[string]any{"health": map[string]any{"timeout": "400ms"}}
	run, _, err := h.engine.Deploy("web", slow, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.engine.Deploy("web", image("ok:2"), ""); codeOf(err) != api.CodeDeployInProgress {
		t.Fatalf("second deploy should be rejected, got %v", err)
	}
	_ = run.Events.Stream(context.Background(), true, func(api.Event) error { return nil })
}

func TestReconcileRestoresRoutesAndCleansUp(t *testing.T) {
	h := newHarness(t)
	h.mustDeploy("web", image("ok:1"))
	// Simulate a daemon restart: a new proxy and engine over the same state.
	px := proxy.New(nil)
	e := New(h.engine.Store, h.rt, px, h.engine.DataDir, nil)
	// An orphan from an interrupted deployment, and a container of an unknown app.
	h.rt.AddImage("x", "ok")
	_, _ = h.rt.Run(context.Background(), runtimeOptions{Name: "saba-web-v9-1", Image: "x", Labels: labels("web", 9, 1, RoleApp)})
	_, _ = h.rt.Run(context.Background(), runtimeOptions{Name: "saba-ghost-v1-1", Image: "x", Labels: labels("ghost", 1, 1, RoleApp)})
	e.Reconcile(context.Background())

	front := httptest.NewServer(px)
	defer front.Close()
	req, _ := http.NewRequest("GET", front.URL, nil)
	req.Host = "web.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("route not restored: %v %v", resp, err)
	}
	resp.Body.Close()
	names := strings.Join(h.rt.Names(), ",")
	if strings.Contains(names, "saba-web-v9-1") {
		t.Fatalf("orphan was not removed: %s", names)
	}
	if !strings.Contains(names, "saba-ghost-v1-1") {
		t.Fatalf("container of an unknown app must be left alone: %s", names)
	}
}
