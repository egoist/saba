package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/client"
	"github.com/egoist/saba/internal/daemon"
	"github.com/egoist/saba/internal/runtime/runtimetest"
)

// startDaemon runs a real daemon (API, engine, proxy) over a fake container
// runtime and points the CLI at it.
func startDaemon(t *testing.T) {
	t.Helper()
	// Unix socket paths are length-limited, so keep the directory short.
	dir, err := os.MkdirTemp("", "saba")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s.sock")
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, daemon.Options{
			Version: "test", DataDir: filepath.Join(dir, "data"), Socket: sock,
			HTTPAddr: "127.0.0.1:0", TLS: "off", BaseDomain: "apps.test",
			Runtime: runtimetest.New(), Ready: ready,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon failed to start: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not start")
	}
	t.Cleanup(func() {
		cancel()
		<-done
		os.RemoveAll(dir)
	})
	t.Setenv("SABA_HOST", "unix://"+sock)
	t.Setenv("SABA_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("SABA_APP", "")
	t.Setenv("SABA_JSON", "")
	t.Chdir(t.TempDir())
}

// saba runs the CLI and returns exit code, stdout and stderr.
func saba(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func mustJSON(t *testing.T, out string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, out)
	}
}

func errorCode(t *testing.T, stderr string) string {
	t.Helper()
	var er api.ErrorResponse
	mustJSON(t, stderr, &er)
	if er.Error == nil {
		t.Fatalf("no error in %s", stderr)
	}
	return er.Error.Code
}

func TestLifecycleThroughTheCLI(t *testing.T) {
	startDaemon(t)

	// Deploy: one JSON document on stdout, nothing on stderr.
	code, out, errOut := saba("deploy", "-a", "web", "--image", "ok:1", "-e", "MODE=a", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("deploy: exit %d, stderr %q", code, errOut)
	}
	var res api.DeployResult
	mustJSON(t, out, &res)
	if !res.Deployed || res.Deployment.Version != 1 || res.Deployment.Status != api.StatusLive || res.URLs[0] != "http://web.apps.test" {
		t.Fatalf("deploy result: %s", out)
	}

	// Events mode streams NDJSON ending in a result event.
	code, out, _ = saba("deploy", "-a", "web", "--image", "ok:2", "--json", "--events")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var last api.Event
	mustJSON(t, lines[len(lines)-1], &last)
	if code != 0 || len(lines) < 3 || last.Type != "result" || last.Result.Deployment.Version != 2 {
		t.Fatalf("events: exit %d\n%s", code, out)
	}

	code, out, _ = saba("apps", "--json")
	var apps []api.App
	mustJSON(t, out, &apps)
	if code != 0 || len(apps) != 1 || apps[0].Status != api.AppRunning || apps[0].CurrentVersion != 2 {
		t.Fatalf("apps: %s", out)
	}

	// Positional app, -a and SABA_APP are interchangeable.
	for _, args := range [][]string{{"status", "web", "--json"}, {"status", "-a", "web", "--json"}} {
		code, out, _ = saba(args...)
		var app api.App
		mustJSON(t, out, &app)
		if code != 0 || app.Name != "web" || len(app.Containers) != 1 || app.Containers[0].State != "running" {
			t.Fatalf("%v: %s", args, out)
		}
	}

	// Env changes roll out a new version; secrets never appear in status output.
	if code, _, errOut = saba("env", "set", "-a", "web", "TOKEN=s3cret", "URL=a=b", "--json"); code != 0 {
		t.Fatalf("env set: %s", errOut)
	}
	_, out, _ = saba("status", "web", "--json")
	if strings.Contains(out, "s3cret") {
		t.Fatal("status output leaks environment values")
	}
	_, out, _ = saba("env", "list", "-a", "web", "--json")
	var env api.EnvResponse
	mustJSON(t, out, &env)
	if !reflect.DeepEqual(env.Env, map[string]string{"MODE": "a", "TOKEN": "s3cret", "URL": "a=b"}) {
		t.Fatalf("env: %v", env.Env)
	}
	if _, out, _ = saba("env", "get", "-a", "web", "TOKEN"); out != "s3cret\n" {
		t.Fatalf("env get printed %q", out)
	}
	if _, out, _ = saba("env", "-a", "web"); !strings.Contains(out, "MODE=a\n") {
		t.Fatalf("env list should be dotenv formatted: %q", out)
	}

	// Rollback goes to the previous live version and is itself a deployment.
	code, out, _ = saba("rollback", "web", "--json")
	mustJSON(t, out, &res)
	if code != 0 || res.Deployment.Version != 4 || res.Deployment.Source.Version != 2 {
		t.Fatalf("rollback: %s", out)
	}
	_, out, _ = saba("env", "list", "-a", "web", "--json")
	env = api.EnvResponse{}
	mustJSON(t, out, &env)
	if _, ok := env.Env["TOKEN"]; ok {
		t.Fatalf("rollback should restore the env of v2: %v", env.Env)
	}

	code, out, _ = saba("deployments", "web", "--json")
	var deps []api.Deployment
	mustJSON(t, out, &deps)
	if code != 0 || len(deps) != 4 || deps[0].Version != 4 || deps[0].Status != api.StatusLive || deps[1].Status != api.StatusSuperseded {
		t.Fatalf("deployments: %s", out)
	}

	code, out, _ = saba("exec", "-a", "web", "--json", "--", "ls", "-la")
	var ex execResult
	mustJSON(t, out, &ex)
	if code != 0 || ex.Stdout != "exec ls -la\n" {
		t.Fatalf("exec: %d %s", code, out)
	}
	if code, _, _ = saba("exec", "-a", "web", "--", "false"); code != 1 {
		t.Fatalf("exec should pass the command's exit code through, got %d", code)
	}

	if code, out, _ = saba("scale", "web", "3", "--json"); code != 0 {
		t.Fatalf("scale: %s", out)
	}
	_, out, _ = saba("status", "web", "--json")
	var app api.App
	mustJSON(t, out, &app)
	if len(app.Containers) != 3 || app.Config.Replicas != 3 {
		t.Fatalf("scale did not take effect: %s", out)
	}

	// The deployment log is retrievable after the fact.
	code, out, _ = saba("logs", "web", "--deployment", "v1")
	if code != 0 || !strings.Contains(out, "Routing traffic to v1") {
		t.Fatalf("deployment log: %d %q", code, out)
	}

	if code, out, _ = saba("doctor", "--json"); code != 0 || !strings.Contains(out, `"ok": true`) {
		t.Fatalf("doctor: %d %s", code, out)
	}

	if code, _, _ = saba("destroy", "web", "--yes", "--json"); code != 0 {
		t.Fatal("destroy failed")
	}
	_, out, _ = saba("apps", "--json")
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("apps after destroy should be an empty array, got %s", out)
	}
}

// Errors are part of the contract: a stable code, a hint, and an exit code.
func TestErrorsAreStructured(t *testing.T) {
	startDaemon(t)
	saba("deploy", "-a", "web", "--image", "ok:1", "--json")

	cases := []struct {
		args []string
		code string
		exit int
	}{
		{[]string{"status", "ghost"}, api.CodeNotFound, api.ExitNotFound},
		{[]string{"destroy", "web"}, api.CodeConfirmation, api.ExitUsage},
		{[]string{"destroy"}, api.CodeInvalid, api.ExitUsage},
		{[]string{"deploy", "-a", "web", "--image", "ok:1", "--strategy", "yolo"}, api.CodeInvalid, api.ExitUsage},
		{[]string{"deploy", "-a", "Bad_Name", "--image", "ok:1"}, api.CodeInvalid, api.ExitUsage},
		{[]string{"deploy", "-a", "web", "--image", "crash:1"}, api.CodeUnhealthy, api.ExitDeployFail},
		{[]string{"deploy", "-a", "web", "--image", "missing:1"}, api.CodeBuildFailed, api.ExitDeployFail},
		{[]string{"rollback", "web"}, api.CodeConflict, api.ExitConflict},
		{[]string{"env", "set", "-a", "web", "not-a-pair"}, api.CodeInvalid, api.ExitUsage},
		{[]string{"env", "get", "-a", "web", "NOPE"}, api.CodeNotFound, api.ExitNotFound},
		{[]string{"scale", "web", "many"}, api.CodeInvalid, api.ExitUsage},
		{[]string{"logs"}, api.CodeInvalid, api.ExitUsage},
		{[]string{"nonsense"}, api.CodeInvalid, api.ExitUsage},
		{[]string{"apps", "--nope"}, api.CodeInvalid, api.ExitUsage},
		{[]string{"-c", "missing", "-H", "", "apps"}, api.CodeNoContext, api.ExitUnreachable},
	}
	for _, tc := range cases {
		if tc.code == api.CodeNoContext {
			t.Setenv("SABA_HOST", "")
		}
		exit, out, errOut := saba(append(tc.args, "--json")...)
		if exit != tc.exit {
			t.Errorf("%v: exit %d, want %d (%s)", tc.args, exit, tc.exit, errOut)
			continue
		}
		if got := errorCode(t, errOut); got != tc.code {
			t.Errorf("%v: code %s, want %s", tc.args, got, tc.code)
		}
		if strings.TrimSpace(out) != "" {
			t.Errorf("%v: stdout must stay empty on failure, got %q", tc.args, out)
		}
		var er api.ErrorResponse
		mustJSON(t, errOut, &er)
		if er.Error.Hint == "" && tc.code != api.CodeConflict {
			t.Errorf("%v: error has no hint: %s", tc.args, errOut)
		}
	}

	// Human mode prints the same information without JSON.
	t.Setenv("SABA_HOST", os.Getenv("SABA_HOST"))
	_, _, errOut := saba("destroy", "web")
	if !strings.Contains(errOut, "--yes") || !strings.Contains(errOut, api.CodeConfirmation) {
		t.Fatalf("human error should name the fix and the code: %q", errOut)
	}
}

func TestDeployFromProjectDirectory(t *testing.T) {
	startDaemon(t)
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "<h1>hi</h1>")
	write("saba.toml", `
name = "site"
replicas = 2
domains = ["site.example.com"]

[health]
path = "/"

[env]
STAGE = "prod"
WORKERS = 4
`)
	t.Chdir(dir)

	// No app flag: the name, config and env come from saba.toml.
	code, out, errOut := saba("deploy", "--json")
	if code != 0 {
		t.Fatalf("deploy: %s", errOut)
	}
	var res api.DeployResult
	mustJSON(t, out, &res)
	d := res.Deployment
	if res.App != "site" || d.Source.Type != "upload" || d.Source.Builder != "static" || d.Config.Replicas != 2 ||
		d.Config.Health.Path != "/" || res.URLs[0] != "http://site.example.com" {
		t.Fatalf("project deploy: %s", out)
	}
	_, out, _ = saba("env", "list", "--json")
	var env api.EnvResponse
	mustJSON(t, out, &env)
	if env.App != "site" || env.Env["STAGE"] != "prod" || env.Env["WORKERS"] != "4" {
		t.Fatalf("env from saba.toml: %s", out)
	}

	// Deploying this directory under another name (a preview environment)
	// applies the file; here its domain then conflicts with the real app.
	if code, _, errOut = saba("deploy", "-a", "site-preview", "--json"); code != api.ExitConflict || !strings.Contains(errOut, "site.example.com") {
		t.Fatalf("preview deploy should apply saba.toml: %d %s", code, errOut)
	}
	if code, out, errOut = saba("deploy", "-a", "site-preview", "--domain", "preview.example.com", "--json"); code != 0 || !strings.Contains(out, `"replicas": 2`) {
		t.Fatalf("preview deploy with its own domain: %d %s %s", code, out, errOut)
	}

	// Deploying an image as another app from this directory must not apply this file.
	code, out, errOut = saba("deploy", "-a", "other", "--image", "ok:1", "--json")
	var other api.DeployResult
	mustJSON(t, out, &other)
	if code != 0 || other.Deployment.Config.Replicas != 0 || len(other.Deployment.Config.Domains) != 0 {
		t.Fatalf("saba.toml of %q leaked into %q: %s %s", "site", "other", out, errOut)
	}
	if _, out, _ = saba("env", "list", "-a", "other", "--json"); strings.Contains(out, "STAGE") {
		t.Fatalf("[env] of another app's saba.toml was applied: %s", out)
	}

	// Flags override the file; a typo in the file is reported by name.
	code, out, _ = saba("deploy", "--replicas", "1", "--dry-run", "--json")
	if code != 0 || !strings.Contains(out, "start 1 replica(s)") || !strings.Contains(out, `"dry_run": true`) {
		t.Fatalf("dry run: %d %s", code, out)
	}
	write("saba.toml", "name = \"site\"\nreplcas = 2\n")
	code, _, errOut = saba("deploy", "--json")
	if code != api.ExitUsage || !strings.Contains(errOut, "replcas") {
		t.Fatalf("typo should be rejected by name: %d %s", code, errOut)
	}
}

func TestInitAndContexts(t *testing.T) {
	startDaemon(t)
	dir := t.TempDir()
	t.Chdir(dir)
	os.WriteFile("package.json", []byte(`{"scripts":{"start":"node ."}}`), 0o644)

	code, out, _ := saba("init", "-a", "shop", "--json")
	if code != 0 || !strings.Contains(out, `"builder": "node"`) || !strings.Contains(out, `"port": 3000`) {
		t.Fatalf("init: %d %s", code, out)
	}
	p, err := LoadProject(".")
	if err != nil || p.Name != "shop" || p.Config["port"] != int64(3000) {
		t.Fatalf("generated saba.toml does not parse back: %+v %v", p, err)
	}
	if code, _, _ = saba("init"); code != api.ExitConflict {
		t.Fatalf("init must not overwrite an existing file, exit %d", code)
	}

	if code, _, errOut := saba("context", "add", "prod", "root@203.0.113.10", "--json"); code != 0 {
		t.Fatalf("context add: %s", errOut)
	}
	saba("context", "add", "staging", "deploy@staging.example.com")
	cfg, err := client.LoadConfig()
	if err != nil || cfg.Current != "prod" || cfg.Contexts["staging"].SSH != "deploy@staging.example.com" {
		t.Fatalf("contexts: %+v %v", cfg, err)
	}
	if code, _, _ = saba("context", "use", "nope"); code != api.ExitNotFound {
		t.Fatalf("using an unknown context should fail, exit %d", code)
	}
	saba("context", "use", "staging")
	saba("context", "remove", "staging")
	if cfg, _ = client.LoadConfig(); cfg.Current != "" || len(cfg.Contexts) != 1 {
		t.Fatalf("after remove: %+v", cfg)
	}
}

func TestEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte(`# comment
PLAIN=value
export EXPORTED=yes
QUOTED="two words \"quoted\"\nnext line"
SINGLE='raw $value'
URL=postgres://u:p@h/db?x=1
TRAILING=v # note
EMPTY=
`), 0o644)
	got, err := readEnvFile(path)
	want := map[string]string{
		"PLAIN": "value", "EXPORTED": "yes", "QUOTED": "two words \"quoted\"\nnext line", "SINGLE": "raw $value",
		"URL": "postgres://u:p@h/db?x=1", "TRAILING": "v", "EMPTY": "",
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, %v", got, err)
	}
	os.WriteFile(path, []byte("no equals sign\n"), 0o644)
	if _, err := readEnvFile(path); err == nil {
		t.Fatal("a malformed line should be an error")
	}
	// Values written by `saba env` must read back identically.
	for _, v := range []string{"simple", "two words", `quote"d`, "line1\nline2", `back\slash`, ""} {
		os.WriteFile(path, []byte("K="+dotenvQuote(v)+"\n"), 0o644)
		if got, _ := readEnvFile(path); got["K"] != v {
			t.Errorf("round trip of %q gave %q", v, got["K"])
		}
	}
}

func TestSourceFilesRespectGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	files := map[string]string{
		"main.go": "package main", ".gitignore": "secret.env\nnode_modules/\n", "secret.env": "KEY=1",
		"node_modules/x/index.js": "x", "sub/untracked.txt": "new",
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	// Outside git: everything except well-known heavy directories.
	got, viaGit, err := sourceFiles(dir)
	if err != nil || viaGit || !reflect.DeepEqual(got, []string{".gitignore", "main.go", "secret.env", "sub/untracked.txt"}) {
		t.Fatalf("plain directory: %v %v %v", got, viaGit, err)
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init failed: %s", out)
	}
	// Inside git: tracked or untracked files, minus ignored ones.
	got, viaGit, err = sourceFiles(dir)
	if err != nil || !viaGit || !reflect.DeepEqual(got, []string{".gitignore", "main.go", "sub/untracked.txt"}) {
		t.Fatalf("git checkout: %v %v %v", got, viaGit, err)
	}
	arc, err := buildArchive(dir)
	if err != nil || arc.Files != 3 || arc.Bytes == 0 {
		t.Fatalf("archive: %+v %v", arc, err)
	}
	arc.Close()
}

func TestContextName(t *testing.T) {
	cfg := &client.Config{Contexts: map[string]client.Target{"prod": {SSH: "root@1.1.1.1"}}}
	for target, want := range map[string]string{
		"root@203.0.113.10":       "prod-2",
		"root@1.1.1.1":            "prod",
		"deploy@web1.example.com": "web1",
		"ssh://me@box:2222":       "box",
		"myalias":                 "myalias",
	} {
		if got := contextName(target, cfg); got != want {
			t.Errorf("contextName(%q) = %q, want %q", target, got, want)
		}
	}
}
