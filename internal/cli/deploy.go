package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/buildpack"
)

type deployFlags struct {
	image, git, ref     string
	port, replicas      int
	domains, env, vols  []string
	envFile             string
	command, release    string
	strategy            string
	healthPath          string
	memory, cpus        string
	dockerfile, message string
	worker, internal    bool
	detach, dryRun      bool
	quiet, events       bool
}

func (c *CLI) deployCmd() *cobra.Command {
	var f deployFlags
	cmd := &cobra.Command{
		Use:   "deploy [path]",
		Short: "Deploy a directory, a git repository or an image",
		Long: `Deploy an app with zero downtime.

The source is, in order of precedence:
  --image <ref>    a registry image
  --git <url>      a git repository, cloned and built on the server
  image = "..."    in saba.toml
  [path]           a directory (default "."), uploaded and built on the server

A directory is built with its Dockerfile. Without one, saba generates a build
for Node.js, Bun, Go, Python, Rust or static sites.

The new version is started next to the current one. Traffic switches only
after it passes its health check; the old version then drains and stops. If
anything fails, the current version keeps serving and the exit code is 5.

Settings come from saba.toml (see "saba init") and can be overridden by flags.
They are remembered by the server, so later deploys do not need to repeat them.`,
		Example: `  saba deploy                                  # deploy the current directory
  saba deploy ./api -a api --port 8080
  saba deploy -a web --image nginx:alpine --domain example.com
  saba deploy -a api --git https://github.com/me/api --ref main
  saba deploy -e DATABASE_URL=postgres://... --replicas 2
  saba deploy --dry-run                        # validate and show the plan
  saba deploy --json                           # final result as one JSON document`,
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			return c.deploy(cmd, dir, &f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.image, "image", "", "Deploy this registry image instead of building source")
	fl.StringVar(&f.git, "git", "", "Deploy this git repository (cloned and built on the server)")
	fl.StringVar(&f.ref, "ref", "", "Branch, tag or commit to deploy with --git (default: the default branch)")
	fl.IntVar(&f.port, "port", 0, "Port the app listens on (default: the image's EXPOSE)")
	fl.StringArrayVar(&f.domains, "domain", nil, "Domain to route to the app (repeatable; replaces the list)")
	fl.IntVar(&f.replicas, "replicas", 0, "Number of containers to run")
	fl.StringArrayVarP(&f.env, "env", "e", nil, "Set an environment variable KEY=VALUE (repeatable)")
	fl.StringVar(&f.envFile, "env-file", "", "Set environment variables from a dotenv file")
	fl.StringVar(&f.command, "command", "", "Override the image's default command")
	fl.StringVar(&f.release, "release-command", "", "Command to run before the new version gets traffic (e.g. migrations)")
	fl.StringVar(&f.strategy, "strategy", "", "Deploy strategy: rolling (default) or recreate")
	fl.StringVar(&f.healthPath, "health-path", "", "HTTP path that must return 2xx/3xx before traffic is switched")
	fl.StringVar(&f.memory, "memory", "", "Memory limit per container (e.g. 512m)")
	fl.StringVar(&f.cpus, "cpus", "", "CPU limit per container (e.g. 0.5)")
	fl.StringArrayVar(&f.vols, "volume", nil, "Persistent volume name:/path (repeatable; replaces the list)")
	fl.StringVar(&f.dockerfile, "dockerfile", "", "Path of the Dockerfile, relative to the source directory")
	fl.BoolVar(&f.worker, "worker", false, "The app does not serve HTTP (no routing or HTTP health check)")
	fl.BoolVar(&f.internal, "internal", false, "Reachable from other apps only, not from the internet")
	fl.StringVarP(&f.message, "message", "m", "", "Note to attach to this deployment")
	fl.BoolVarP(&f.detach, "detach", "d", false, "Return as soon as the deployment is started")
	fl.BoolVar(&f.dryRun, "dry-run", false, "Validate everything and print the plan without deploying")
	fl.BoolVarP(&f.quiet, "quiet", "q", false, "Hide build and container output (shown only on failure)")
	fl.BoolVar(&f.events, "events", false, "With --json: stream progress as NDJSON events instead of one final document")
	return cmd
}

func (c *CLI) deploy(cmd *cobra.Command, dir string, f *deployFlags) error {
	ctx := cmd.Context()
	fl := cmd.Flags()
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return api.Errorf(api.CodeInvalid, "%q is not a directory", dir).
			WithHint("Pass the project directory, or use --image / --git to deploy without local source.")
	}

	project, err := LoadProject(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if project != nil && c.project == nil {
		c.project = project
	}

	// App name: flag/env, then saba.toml, then the directory name.
	name := c.appFlag
	if name == "" {
		name = os.Getenv("SABA_APP")
	}
	inferred := false
	if name == "" && project != nil {
		name = project.Name
	}
	if name == "" {
		abs, _ := filepath.Abs(dir)
		name = api.SanitizeName(filepath.Base(abs))
		inferred = true
	}
	if err := api.ValidateName(name); err != nil {
		return err
	}

	req := api.DeployRequest{Config: map[string]any{}, EnvSet: map[string]string{}, Message: f.message, DryRun: f.dryRun}
	if project != nil {
		for k, v := range project.Config {
			req.Config[k] = v
		}
		for k, v := range project.Env {
			req.EnvSet[k] = v
		}
	}
	set := func(flag, key string, v any) {
		if fl.Changed(flag) {
			req.Config[key] = v
		}
	}
	sub := func(flag, table, key string, v any) {
		if !fl.Changed(flag) {
			return
		}
		m, _ := req.Config[table].(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		m[key] = v
		req.Config[table] = m
	}
	set("port", "port", f.port)
	set("replicas", "replicas", f.replicas)
	set("domain", "domains", f.domains)
	set("command", "command", f.command)
	set("release-command", "release_command", f.release)
	set("strategy", "strategy", f.strategy)
	set("volume", "volumes", f.vols)
	set("worker", "worker", f.worker)
	set("internal", "internal", f.internal)
	sub("health-path", "health", "path", f.healthPath)
	sub("memory", "resources", "memory", f.memory)
	sub("cpus", "resources", "cpus", f.cpus)
	sub("dockerfile", "build", "dockerfile", f.dockerfile)

	if f.envFile != "" {
		vars, err := readEnvFile(f.envFile)
		if err != nil {
			return err
		}
		for k, v := range vars {
			req.EnvSet[k] = v
		}
	}
	for _, kv := range f.env {
		k, v, err := splitEnv(kv)
		if err != nil {
			return err
		}
		req.EnvSet[k] = v
	}

	// Source.
	var arc *archive
	configImage, _ := req.Config["image"].(string)
	switch {
	case f.image != "" && f.git != "":
		return api.Errorf(api.CodeInvalid, "--image and --git cannot be combined")
	case f.image != "":
		req.Source = api.Source{Type: "image", Image: f.image}
		delete(req.Config, "image")
	case f.git != "":
		req.Source = api.Source{Type: "git", Git: f.git, Ref: f.ref}
		delete(req.Config, "image")
	case configImage != "":
		req.Source = api.Source{Type: "image", Image: configImage}
		delete(req.Config, "image")
	default:
		req.Source = api.Source{Type: "upload"}
		customDockerfile := false
		if b, ok := req.Config["build"].(map[string]any); ok {
			df, _ := b["dockerfile"].(string)
			customDockerfile = df != ""
		}
		if !customDockerfile {
			// Fail before uploading if the server could not build this anyway.
			if _, derr := buildpack.Detect(dir); derr != nil {
				return derr
			}
		}
		if commit, err := gitOutput(dir, "rev-parse", "--short", "HEAD"); err == nil {
			req.Source.Commit = strings.TrimSpace(commit)
			if req.Message == "" {
				if subject, err := gitOutput(dir, "log", "-1", "--format=%s"); err == nil {
					req.Message = truncate(strings.TrimSpace(subject), 100)
				}
			}
		}
		if !f.dryRun {
			if arc, err = buildArchive(dir); err != nil {
				return err
			}
			defer arc.Close()
		}
	}

	cl, err := c.api()
	if err != nil {
		return err
	}

	human := !c.jsonOut
	ui := c.ui
	if human {
		verb := "Deploying"
		if f.dryRun {
			verb = "Planning deployment of"
		}
		ui.printf("%s %s %s %s\n", ui.cyan("▶"), verb, ui.bold(name), ui.dim("→ "+cl.Target.String()))
		if inferred {
			ui.note("App name taken from the directory. Pin it with `saba init` or pass -a <name>.")
		}
		if arc != nil {
			ui.printf("  %s %s\n", ui.cyan(pad("upload", 8)), fmt.Sprintf("Uploading source (%s, %s)", plural(arc.Files, "file"), humanBytes(arc.Bytes)))
		}
	}

	var body io.Reader
	if arc != nil {
		body = arc.file
	}

	r := &deployRenderer{c: c, quiet: f.quiet, events: f.events}
	start := time.Now()
	result, err := cl.Deploy(ctx, name, req, body, f.detach, r.handle)
	if err != nil {
		return r.fail(err)
	}
	if c.jsonOut && f.events {
		return nil // the result was already emitted as the last event
	}
	return c.printDeployResult(name, result, time.Since(start))
}

func (c *CLI) printDeployResult(name string, result *api.DeployResult, took time.Duration) error {
	return c.result(result, func() {
		ui := c.ui
		switch {
		case result.Deployed && result.Deployment != nil:
			ui.success("%s %s is live %s", ui.bold(name), result.Deployment.Ref(), ui.dim("("+took.Round(100*time.Millisecond).String()+")"))
			for _, u := range result.URLs {
				ui.printf("  %s\n", ui.cyan(u))
			}
			if len(result.URLs) == 0 && result.Deployment.Port > 0 && !result.Deployment.Config.Internal {
				ui.note("No public URL: add a domain with `saba domains add -a %s <domain>` or set a server base domain.", name)
			}
		case result.Deployment != nil:
			ui.success("%s %s started in the background", ui.bold(name), result.Deployment.Ref())
			ui.note("Follow it with `saba logs -a %s --deployment %s --follow`.", name, result.Deployment.Ref())
		case result.Saved:
			ui.success("%s: %s", ui.bold(name), strings.Join(result.Plan, "; "))
		default:
			for _, step := range result.Plan {
				ui.printf("  %s %s\n", ui.dim("•"), step)
			}
			for _, u := range result.URLs {
				ui.printf("  %s %s\n", ui.dim("→"), ui.cyan(u))
			}
			ui.note("Dry run: nothing was changed.")
		}
	})
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

// deployRenderer prints deploy progress events.
type deployRenderer struct {
	c      *CLI
	quiet  bool
	events bool
	tail   []string // most recent log lines, for failure reports
}

func (r *deployRenderer) handle(ev api.Event) {
	c := r.c
	if ev.Type == "log" {
		r.tail = append(r.tail, ev.Message)
		if len(r.tail) > 60 {
			r.tail = r.tail[len(r.tail)-60:]
		}
	}
	if c.jsonOut {
		if r.events {
			c.event(ev)
		}
		return
	}
	ui := c.ui
	switch ev.Type {
	case "phase":
		if ev.Phase == "queued" {
			return
		}
		ui.printf("  %s %s\n", ui.cyan(pad(ev.Phase, 8)), ev.Message)
	case "log":
		if r.quiet || strings.TrimSpace(ev.Message) == "" {
			return
		}
		if ev.Stream == "deploy" {
			ui.printf("  %s %s\n", pad("", 8), ui.yellow(ev.Message))
			return
		}
		ui.printf("  %s %s\n", pad("", 8), ui.dim("│ "+ev.Message))
	}
}

// fail enriches a deploy error with the output that preceded it.
func (r *deployRenderer) fail(err error) error {
	ae := api.AsError(err)
	if len(r.tail) > 0 {
		if _, ok := ae.Details["logs"]; !ok {
			n := len(r.tail)
			if n > 40 {
				n = 40
			}
			ae.WithDetail("logs", r.tail[len(r.tail)-n:])
		}
		if r.quiet && !r.c.jsonOut {
			for _, l := range r.tail {
				r.c.ui.printf("  %s %s\n", pad("", 8), r.c.ui.dim("│ "+l))
			}
		}
	}
	return ae
}

func splitEnv(kv string) (string, string, error) {
	k, v, ok := strings.Cut(kv, "=")
	k = strings.TrimSpace(k)
	if !ok || !api.ValidEnvKey(k) {
		return "", "", api.Errorf(api.CodeInvalid, "invalid environment variable %q", kv).
			WithHint("Use KEY=VALUE, e.g. `-e NODE_ENV=production`.")
	}
	return k, v, nil
}

// readEnvFile parses a dotenv file: KEY=VALUE lines, optional `export`,
// comments, and single- or double-quoted values.
func readEnvFile(path string) (map[string]string, error) {
	var r io.Reader
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, api.Errorf(api.CodeInvalid, "cannot read env file: %v", err)
		}
		defer f.Close()
		r = f
	}
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !api.ValidEnvKey(k) {
			return nil, api.Errorf(api.CodeInvalid, "%s:%d: expected KEY=VALUE", path, n)
		}
		v = strings.TrimSpace(v)
		switch {
		case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
			v = strings.NewReplacer(`\n`, "\n", `\"`, `"`, `\\`, `\`).Replace(v[1 : len(v)-1])
		case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
			v = v[1 : len(v)-1]
		default:
			if i := strings.Index(v, " #"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
		}
		out[k] = v
	}
	return out, sc.Err()
}
