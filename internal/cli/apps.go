package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/client"
	"github.com/egoist/saba/internal/shellwords"
)

func first(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func (c *CLI) appsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "apps",
		Aliases: []string{"ls", "list"},
		Short:   "List apps on the server",
		Args:    maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := c.api()
			if err != nil {
				return err
			}
			apps, err := cl.Apps(cmd.Context())
			if err != nil {
				return err
			}
			if apps == nil {
				apps = []*api.App{}
			}
			return c.result(apps, func() { c.printApps(apps, cl.Target) })
		},
	}
}

func (c *CLI) printApps(apps []*api.App, t client.Target) {
	ui := c.ui
	if len(apps) == 0 {
		ui.printf("No apps on %s yet. Deploy one with `saba deploy`.\n", t)
		return
	}
	rows := make([][]string, 0, len(apps))
	for _, a := range apps {
		version, deployed := "-", "-"
		if a.CurrentVersion > 0 {
			version = "v" + strconv.Itoa(a.CurrentVersion)
		}
		replicas := "-"
		if len(a.Deployments) > 0 {
			d := a.Deployments[0]
			replicas = strconv.Itoa(len(d.Replicas))
			if d.FinishedAt != nil {
				deployed = ago(*d.FinishedAt)
			}
		}
		url := "-"
		if len(a.URLs) > 0 {
			url = a.URLs[0]
		} else if a.Kind == api.KindDatabase && a.Database != nil {
			url = a.Database.Engine + " " + a.Database.Version
		}
		rows = append(rows, []string{a.Name, ui.status(a.Status), version, replicas, deployed, url})
	}
	ui.table([]string{"NAME", "STATUS", "VERSION", "REPLICAS", "DEPLOYED", "URL"}, rows)
}

func (c *CLI) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [app]",
		Short: "Show the state of an app (or of all apps)",
		Long: `Show what is running: version, replicas, URLs, containers and recent deployments.

Without an app (and outside a project directory) it lists all apps.`,
		Example: `  saba status              # app from ./saba.toml, or all apps
  saba status api
  saba status api --json   # full machine-readable state`,
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := c.api()
			if err != nil {
				return err
			}
			name, err := c.app(first(args))
			if err != nil && err != error(errNoApp) {
				return err // e.g. a malformed saba.toml
			}
			if err != nil {
				apps, lerr := cl.Apps(cmd.Context())
				if lerr != nil {
					return lerr
				}
				if apps == nil {
					apps = []*api.App{}
				}
				return c.result(apps, func() { c.printApps(apps, cl.Target) })
			}
			app, err := cl.App(cmd.Context(), name)
			if err != nil {
				return err
			}
			return c.result(app, func() { c.printStatus(app, cl.Target) })
		},
	}
}

func currentDeployment(a *api.App) *api.Deployment {
	for _, d := range a.Deployments {
		if d.Version == a.CurrentVersion {
			return d
		}
	}
	return nil
}

func (c *CLI) printStatus(a *api.App, t client.Target) {
	ui := c.ui
	version := ""
	if a.CurrentVersion > 0 {
		version = " v" + strconv.Itoa(a.CurrentVersion)
	}
	ui.printf("%s%s  %s  %s\n", ui.bold(a.Name), version, ui.status(a.Status), ui.dim("on "+t.String()))
	field := func(k, v string) { ui.printf("  %s %s\n", ui.dim(pad(k, 10)), v) }

	for _, u := range a.URLs {
		field("URL", ui.cyan(u))
	}
	cur := currentDeployment(a)
	if a.Database != nil {
		field("Database", a.Database.Engine+" "+a.Database.Version)
	}
	if cur != nil {
		src := cur.Source.Type
		switch cur.Source.Type {
		case "image":
			src = cur.Source.Image
		case "git":
			src = cur.Source.Git
			if cur.Source.Ref != "" {
				src += "@" + cur.Source.Ref
			}
		case "upload":
			src = "uploaded source"
			if cur.Source.Builder != "" {
				src += " (" + cur.Source.Builder + ")"
			}
		case "current":
			src = "image of the previous version (" + cur.Trigger + ")"
		case "rollback":
			src = "rollback"
		}
		if cur.Source.Commit != "" {
			src += " @ " + truncate(cur.Source.Commit, 12)
		}
		field("Source", src)
		when := "-"
		if cur.FinishedAt != nil {
			when = ago(*cur.FinishedAt)
		}
		if cur.Message != "" {
			when += " — " + cur.Message
		}
		field("Deployed", when)

		running := 0
		for _, ct := range a.Containers {
			if ct.Version == a.CurrentVersion && ct.State == "running" {
				running++
			}
		}
		field("Replicas", fmt.Sprintf("%d/%d running", running, len(cur.Replicas)))
		var bits []string
		if cur.Port > 0 {
			bits = append(bits, "port "+strconv.Itoa(cur.Port))
		} else if a.Kind == api.KindApp {
			bits = append(bits, "worker (no HTTP)")
		}
		bits = append(bits, cur.Config.EffectiveStrategy()+" deploys")
		if cur.Config.Internal {
			bits = append(bits, "internal")
		}
		if len(cur.Config.Volumes) > 0 {
			bits = append(bits, "volumes "+strings.Join(cur.Config.Volumes, ", "))
		}
		field("Config", strings.Join(bits, " · "))
	}
	if a.Traffic != nil && a.Traffic.Requests > 0 {
		field("Traffic", fmt.Sprintf("%d requests · %d errors · %d in flight (since the daemon started)",
			a.Traffic.Requests, a.Traffic.Errors, a.Traffic.InFlight))
	}
	if a.PendingChanges {
		field("Pending", ui.yellow("config or env changes are saved but not deployed")+ui.dim(" — apply with `saba restart -a "+a.Name+"`"))
	}

	if len(a.Containers) > 0 {
		ui.printf("\n")
		rows := make([][]string, 0, len(a.Containers))
		for _, ct := range a.Containers {
			state := ui.status(ct.State)
			if ct.OOMKilled {
				state += ui.red(" (out of memory)")
			} else if ct.State == "exited" {
				state += ui.dim(fmt.Sprintf(" (%d)", ct.ExitCode))
			}
			rows = append(rows, []string{
				strconv.Itoa(ct.Replica), "v" + strconv.Itoa(ct.Version), ct.Name, state,
				strconv.Itoa(ct.Restarts), ago(ct.StartedAt),
			})
		}
		ui.table([]string{"REPLICA", "VERSION", "CONTAINER", "STATE", "RESTARTS", "STARTED"}, rows)
	}

	if cur != nil && len(cur.Config.Cron) > 0 {
		ui.printf("\n")
		var rows [][]string
		for _, j := range cur.Config.Cron {
			last, st := "never", "-"
			if s := a.Cron[j.Name]; s != nil {
				last, st = ago(s.LastRun), ui.status(s.LastStatus)
			}
			rows = append(rows, []string{j.Name, j.Schedule, truncate(j.Command, 40), last, st})
		}
		ui.table([]string{"CRON", "SCHEDULE", "COMMAND", "LAST RUN", "RESULT"}, rows)
	}

	if len(a.Deployments) > 0 {
		ui.printf("\n")
		c.printDeployments(a.Deployments, a.CurrentVersion, 5)
	}

	switch a.Status {
	case api.AppCrashed, api.AppDegraded:
		ui.printf("\n%s Containers are not healthy. Inspect with `saba logs -a %s`; recover with `saba restart -a %s` or `saba rollback -a %s`.\n",
			ui.red("!"), a.Name, a.Name, a.Name)
	case api.AppFailed:
		ui.printf("\n%s The last deployment failed. See why: `saba logs -a %s --deployment latest`.\n", ui.red("!"), a.Name)
	}
}

func (c *CLI) printDeployments(deps []*api.Deployment, current, limit int) {
	ui := c.ui
	sorted := append([]*api.Deployment(nil), deps...)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].Version > sorted[i].Version {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	if limit > 0 && len(sorted) > limit {
		sorted = sorted[:limit]
	}
	rows := make([][]string, 0, len(sorted))
	for _, d := range sorted {
		took := "-"
		if d.DurationMS > 0 {
			took = millis(d.DurationMS)
		}
		note := d.Message
		if d.Error != nil {
			note = d.Error.Message
		}
		restorable := ""
		if d.Status == api.StatusSuperseded && d.Image != "" {
			restorable = "yes"
		}
		rows = append(rows, []string{
			d.Ref(), ui.status(d.Status), d.Trigger, ago(d.CreatedAt), took, orDash(restorable), truncate(orDash(note), 60),
		})
	}
	ui.table([]string{"VERSION", "STATUS", "TRIGGER", "WHEN", "TOOK", "ROLLBACK", "NOTE"}, rows)
}

func (c *CLI) deploymentsCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:     "deployments [app]",
		Aliases: []string{"history", "releases"},
		Short:   "List an app's deployment history",
		Long: `List deployments, newest first. The ROLLBACK column shows which versions can
still be restored with "saba rollback --to <version>".`,
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := c.app(first(args))
			if err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			deps, err := cl.Deployments(cmd.Context(), name)
			if err != nil {
				return err
			}
			if deps == nil {
				deps = []*api.Deployment{}
			}
			if limit > 0 && len(deps) > limit {
				deps = deps[:limit]
			}
			return c.result(deps, func() {
				if len(deps) == 0 {
					c.ui.printf("%s has no deployments yet.\n", name)
					return
				}
				c.printDeployments(deps, 0, 0)
			})
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 20, "Maximum number of deployments to show")
	return cmd
}

func (c *CLI) logsCmd() *cobra.Command {
	var (
		follow     bool
		tail       int
		since      string
		replica    int
		deployment string
		timestamps bool
	)
	cmd := &cobra.Command{
		Use:   "logs [app]",
		Short: "Show app logs, or the log of a deployment",
		Long: `Show the output of an app's running containers.

By default the last 100 lines are printed and the command exits; use --follow
to keep streaming. With --deployment, the build and rollout log of that
deployment is shown instead (use "latest" for the most recent one).

With --json, each line is one JSON event (NDJSON).`,
		Example: `  saba logs                          # last 100 lines of the app in ./saba.toml
  saba logs api -n 500 --since 1h
  saba logs api -f                   # stream until interrupted
  saba logs api --deployment latest  # why did the last deploy fail?`,
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := c.app(first(args))
			if err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			ui := c.ui
			if deployment != "" {
				r := &deployRenderer{c: c, events: true}
				return cl.DeployEvents(cmd.Context(), name, deployment, follow, func(ev api.Event) error {
					if c.jsonOut {
						c.event(ev)
						return nil
					}
					switch ev.Type {
					case "result":
						ui.success("%s", ev.Message)
					case "error":
						if ev.Error != nil {
							ui.printf("%s %s\n", ui.red("✗"), ev.Error.Message)
							if ev.Error.Hint != "" {
								ui.printf("  %s %s\n", ui.dim("hint:"), ev.Error.Hint)
							}
						}
						return client.ErrStop
					default:
						r.handle(ev)
					}
					return nil
				})
			}
			multi := false
			if app, aerr := cl.App(cmd.Context(), name); aerr == nil {
				if cur := currentDeployment(app); cur != nil && len(cur.Replicas) > 1 && replica == 0 {
					multi = true
				}
			}
			return cl.Logs(cmd.Context(), name, api.LogsQuery{Tail: tail, Since: since, Follow: follow, Replica: replica}, func(ev api.Event) error {
				if c.jsonOut {
					c.event(ev)
					return nil
				}
				if ev.Type != "log" {
					return nil
				}
				prefix := ""
				if timestamps && !ev.Time.IsZero() {
					prefix = ui.dim(ev.Time.Local().Format("2006-01-02 15:04:05") + " ")
				}
				if multi {
					prefix += ui.cyan(fmt.Sprintf("[%d] ", ev.Replica))
				}
				ui.printf("%s%s\n", prefix, ev.Message)
				return nil
			})
		},
	}
	fl := cmd.Flags()
	fl.BoolVarP(&follow, "follow", "f", false, "Keep streaming new output")
	fl.IntVarP(&tail, "tail", "n", 100, "Number of most recent lines to show")
	fl.StringVar(&since, "since", "", "Only show output newer than this (e.g. 10m, 2h, or an RFC 3339 time)")
	fl.IntVar(&replica, "replica", 0, "Only show this replica (default: all)")
	fl.StringVar(&deployment, "deployment", "", "Show the log of a deployment instead (e.g. v3 or latest)")
	fl.BoolVarP(&timestamps, "timestamps", "t", false, "Prefix each line with its timestamp")
	return cmd
}

func (c *CLI) statsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats [app]",
		Short: "Show CPU and memory usage of running containers",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := first(args)
			if name == "" {
				name = c.appFlag
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			stats, err := cl.Stats(cmd.Context(), name)
			if err != nil {
				return err
			}
			if stats == nil {
				stats = []api.Stats{}
			}
			return c.result(stats, func() {
				if len(stats) == 0 {
					c.ui.printf("No running containers.\n")
					return
				}
				rows := make([][]string, 0, len(stats))
				for _, s := range stats {
					rows = append(rows, []string{s.App, s.Container, s.CPU, s.Memory, s.MemoryPct, s.NetIO, s.PIDs})
				}
				c.ui.table([]string{"APP", "CONTAINER", "CPU", "MEMORY", "MEM %", "NET I/O", "PIDS"}, rows)
			})
		},
	}
}

// redeploy issues a deploy request that reuses the current image, renders
// progress and prints the result. It backs restart, scale, env and domains.
func (c *CLI) redeploy(cmd *cobra.Command, name string, req api.DeployRequest, quiet bool) error {
	cl, err := c.api()
	if err != nil {
		return err
	}
	r := &deployRenderer{c: c, quiet: quiet}
	start := time.Now()
	result, err := cl.Deploy(cmd.Context(), name, req, nil, false, r.handle)
	if err != nil {
		return r.fail(err)
	}
	return c.printDeployResult(name, result, time.Since(start))
}

func (c *CLI) restartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart [app]",
		Short: "Restart an app with zero downtime (applies pending config)",
		Long: `Roll out fresh containers of the current image, using the app's deploy
strategy. Saved-but-undeployed config and env changes take effect.`,
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := c.app(first(args))
			if err != nil {
				return err
			}
			return c.redeploy(cmd, name, api.DeployRequest{Source: api.Source{Type: "current"}, Trigger: "restart"}, true)
		},
	}
}

func (c *CLI) scaleCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "scale [app] <replicas>",
		Short:   "Change the number of replicas",
		Example: "  saba scale api 3\n  saba scale 2            # app from ./saba.toml",
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			count := args[len(args)-1]
			n, err := strconv.Atoi(count)
			if err != nil || n < 1 {
				return api.Errorf(api.CodeInvalid, "invalid replica count %q", count).
					WithHint("Usage: saba scale [app] <replicas>, e.g. `saba scale api 3`. Use `saba stop` to stop an app.")
			}
			positional := ""
			if len(args) == 2 {
				positional = args[0]
			}
			name, err := c.app(positional)
			if err != nil {
				return err
			}
			return c.redeploy(cmd, name, api.DeployRequest{
				Source:  api.Source{Type: "current"},
				Config:  map[string]any{"replicas": n},
				Trigger: "scale",
				Message: fmt.Sprintf("scale to %d", n),
			}, true)
		},
	}
}

func (c *CLI) rollbackCmd() *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "rollback [app]",
		Short: "Roll back to the previous (or a specific) version",
		Long: `Redeploy an earlier version: its image, config and environment. The rollback
is itself a zero-downtime deployment and gets a new version number.

Without --to, the most recent previously-live version is used. Run
"saba deployments" to see which versions can be restored.`,
		Example: "  saba rollback api\n  saba rollback api --to v12",
		Args:    maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := c.app(first(args))
			if err != nil {
				return err
			}
			version := 0
			if to != "" {
				version, err = strconv.Atoi(strings.TrimPrefix(strings.ToLower(to), "v"))
				if err != nil || version < 1 {
					return api.Errorf(api.CodeInvalid, "invalid version %q", to).WithHint("Use a version like v12.")
				}
			}
			return c.redeploy(cmd, name, api.DeployRequest{Source: api.Source{Type: "rollback", Version: version}}, true)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "Version to restore (default: the previous live version)")
	return cmd
}

func (c *CLI) stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop [app]",
		Short: "Stop an app's containers (keeps config, data and versions)",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := c.app(first(args))
			if err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			app, err := cl.Stop(cmd.Context(), name)
			if err != nil {
				return err
			}
			return c.result(app, func() {
				c.ui.success("%s stopped", c.ui.bold(name))
				c.ui.note("Start it again with `saba start -a %s`.", name)
			})
		},
	}
}

func (c *CLI) startCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start [app]",
		Short: "Start a stopped app",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := c.app(first(args))
			if err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			app, err := cl.Start(cmd.Context(), name)
			if err != nil {
				return err
			}
			return c.result(app, func() {
				c.ui.success("%s started", c.ui.bold(name))
				for _, u := range app.URLs {
					c.ui.printf("  %s\n", c.ui.cyan(u))
				}
			})
		},
	}
}

func (c *CLI) destroyCmd() *cobra.Command {
	var yes, volumes bool
	cmd := &cobra.Command{
		Use:     "destroy [app]",
		Aliases: []string{"rm", "delete"},
		Short:   "Delete an app, its containers, images and history",
		Long: `Permanently delete an app. Named volumes are kept unless --volumes is given.

This command never prompts; it requires --yes.`,
		Example: "  saba destroy old-api --yes\n  saba destroy old-db --yes --volumes   # also delete its data",
		Args:    maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Destroy never falls back to saba.toml: the target must be explicit.
			name := first(args)
			if name == "" {
				name = c.appFlag
			}
			if name == "" {
				return api.Errorf(api.CodeInvalid, "no app specified").
					WithHint("Name the app explicitly: `saba destroy <app> --yes`.")
			}
			retry := "saba destroy " + name
			if volumes {
				retry += " --volumes"
			}
			if err := c.confirm(yes, "Destroying "+name, retry); err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			res, err := cl.Destroy(cmd.Context(), name, volumes)
			if err != nil {
				return err
			}
			return c.result(res, func() {
				c.ui.success("%s destroyed", c.ui.bold(name))
				if len(res.VolumesKept) > 0 {
					c.ui.note("Volumes kept: %s (remove with `docker volume rm` on the server, or pass --volumes next time).", strings.Join(res.VolumesKept, ", "))
				}
			})
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Confirm the deletion")
	cmd.Flags().BoolVar(&volumes, "volumes", false, "Also delete the app's named volumes (data loss)")
	return cmd
}

type execResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

func (c *CLI) execCmd() *cobra.Command {
	var (
		replica int
		tty     bool
	)
	cmd := &cobra.Command{
		Use:   "exec [-a app] -- <command> [args...]",
		Short: "Run a command inside a running container",
		Long: `Run a command in a running replica of the app. Output is streamed and the
command's exit code becomes saba's exit code.

Use -t for an interactive terminal (a shell, a REPL). With --json, stdout,
stderr and the exit code are returned as one document.`,
		Example: `  saba exec -a api -- ls -la /app
  saba exec -a api -t -- sh
  saba exec -a db -- pg_dump -U postgres app > backup.sql`,
		Args: minArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := c.app()
			if err != nil {
				return err
			}
			if tty {
				return c.interactive(cmd, name, replica, args)
			}
			return c.runExec(cmd, name, api.ExecRequest{Command: args, Replica: replica})
		},
	}
	cmd.Flags().IntVar(&replica, "replica", 1, "Replica to run the command in")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "Allocate an interactive terminal")
	cmd.Flags().BoolP("interactive", "i", false, "Accepted for docker compatibility (implied by -t)")
	_ = cmd.Flags().MarkHidden("interactive")
	return cmd
}

func (c *CLI) runCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run [-a app] -- <command> [args...]",
		Short: "Run a one-off command in a fresh container of the app",
		Long: `Start a new container from the app's live image, with its environment and
volumes, run the command to completion and remove the container. Useful for
migrations, consoles and scripts that should not run inside a serving replica.`,
		Example: "  saba run -a api -- npm run migrate\n  saba run -a api -- node scripts/backfill.js",
		Args:    minArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := c.app()
			if err != nil {
				return err
			}
			return c.runExec(cmd, name, api.ExecRequest{Command: args, OneOff: true})
		},
	}
}

func (c *CLI) runExec(cmd *cobra.Command, name string, req api.ExecRequest) error {
	cl, err := c.api()
	if err != nil {
		return err
	}
	if c.jsonOut {
		var stdout, stderr bytes.Buffer
		code, err := cl.Exec(cmd.Context(), name, req, &stdout, &stderr)
		if err != nil {
			return err
		}
		if err := c.result(execResult{ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String()}, nil); err != nil {
			return err
		}
		if code != 0 {
			return exitError{code}
		}
		return nil
	}
	code, err := cl.Exec(cmd.Context(), name, req, c.stdout, c.stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

// interactive hands the terminal to `docker exec -it`, locally or through ssh.
func (c *CLI) interactive(cmd *cobra.Command, name string, replica int, args []string) error {
	cl, err := c.api()
	if err != nil {
		return err
	}
	container, err := cl.Container(cmd.Context(), name, replica)
	if err != nil {
		return err
	}
	dockerArgs := append([]string{"docker", "exec", "-it", container}, args...)
	var argv []string
	if cl.Target.Local() {
		argv = dockerArgs
	} else {
		argv = client.SSHCommand(cl.Target.SSH, true, shellwords.Join(dockerArgs))
	}
	proc := exec.CommandContext(cmd.Context(), argv[0], argv[1:]...)
	proc.Stdin, proc.Stdout, proc.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := proc.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return exitError{ee.ExitCode()}
		}
		return api.Errorf(api.CodeRuntime, "cannot start an interactive session: %v", err)
	}
	return nil
}
