package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/egoist/saba/internal/api"
)

// ---- domains ---------------------------------------------------------------

func (c *CLI) domainsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "domains",
		Short: "Manage the domains routed to an app",
		Long: `Manage custom domains. HTTPS certificates are issued automatically once a
domain's DNS A record points at the server.

An app without custom domains is served at <app>.<server base domain>.`,
		Example: `  saba domains -a web
  saba domains add -a web example.com www.example.com
  saba domains remove -a web www.example.com`,
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error { return c.domainsList(cmd) },
	}
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List an app's domains", Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error { return c.domainsList(cmd) },
	}
	change := func(add bool) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			name, err := c.app()
			if err != nil {
				return err
			}
			req := api.DeployRequest{Source: api.Source{Type: "current"}, IfLive: true, Trigger: "domains"}
			if add {
				req.AddDomains = args
				req.Message = "add " + strings.Join(args, ", ")
			} else {
				req.RemoveDomains = args
				req.Message = "remove " + strings.Join(args, ", ")
			}
			return c.redeploy(cmd, name, req, true)
		}
	}
	add := &cobra.Command{Use: "add <domain>...", Short: "Route domains to the app", Args: minArgs(1), RunE: change(true)}
	remove := &cobra.Command{Use: "remove <domain>...", Aliases: []string{"rm"}, Short: "Stop routing domains to the app", Args: minArgs(1), RunE: change(false)}
	cmd.AddCommand(list, add, remove)
	return cmd
}

func (c *CLI) domainsList(cmd *cobra.Command) error {
	name, err := c.app()
	if err != nil {
		return err
	}
	cl, err := c.api()
	if err != nil {
		return err
	}
	app, err := cl.App(cmd.Context(), name)
	if err != nil {
		return err
	}
	out := map[string]any{"app": name, "domains": app.Config.Domains, "urls": app.URLs}
	if app.Config.Domains == nil {
		out["domains"] = []string{}
	}
	return c.result(out, func() {
		if len(app.URLs) == 0 {
			c.ui.printf("%s has no public URL.\n", name)
			c.ui.note("Add a domain with `saba domains add -a %s <domain>`.", name)
			return
		}
		for _, u := range app.URLs {
			fmt.Fprintln(c.stdout, u)
		}
		if len(app.Config.Domains) == 0 {
			c.ui.note("This is the automatic domain. Add your own with `saba domains add -a %s <domain>`.", name)
		}
	})
}

// ---- databases -------------------------------------------------------------

type dbEngine struct {
	image   string
	version string
	envName string
	build   func(name, db, pw string) (cfg map[string]any, env, outputs map[string]string)
}

var dbEngines = map[string]dbEngine{
	"postgres": {image: "postgres", version: "17", envName: "DATABASE_URL",
		build: func(name, db, pw string) (map[string]any, map[string]string, map[string]string) {
			return map[string]any{
					"port":    5432,
					"volumes": []string{"data:/var/lib/postgresql/data"},
					// TCP check: the init-time server only listens on a unix socket.
					"health": map[string]any{"command": "pg_isready -h 127.0.0.1 -U postgres", "timeout": "120s"},
				},
				map[string]string{"POSTGRES_PASSWORD": pw, "POSTGRES_DB": db, "PGDATA": "/var/lib/postgresql/data/pgdata"},
				map[string]string{"URL": fmt.Sprintf("postgres://postgres:%s@%s:5432/%s", pw, name, db),
					"HOST": name, "PORT": "5432", "USER": "postgres", "PASSWORD": pw, "DATABASE": db}
		}},
	"mysql": {image: "mysql", version: "8.4", envName: "DATABASE_URL",
		build: func(name, db, pw string) (map[string]any, map[string]string, map[string]string) {
			return map[string]any{
					"port":    3306,
					"volumes": []string{"data:/var/lib/mysql"},
					"health":  map[string]any{"command": `mysqladmin ping -h 127.0.0.1 -uroot -p"$MYSQL_ROOT_PASSWORD" --silent`, "timeout": "180s"},
				},
				map[string]string{"MYSQL_ROOT_PASSWORD": pw, "MYSQL_DATABASE": db},
				map[string]string{"URL": fmt.Sprintf("mysql://root:%s@%s:3306/%s", pw, name, db),
					"HOST": name, "PORT": "3306", "USER": "root", "PASSWORD": pw, "DATABASE": db}
		}},
	"mariadb": {image: "mariadb", version: "11", envName: "DATABASE_URL",
		build: func(name, db, pw string) (map[string]any, map[string]string, map[string]string) {
			return map[string]any{
					"port":    3306,
					"volumes": []string{"data:/var/lib/mysql"},
					"health":  map[string]any{"command": "healthcheck.sh --connect --innodb_initialized", "timeout": "180s"},
				},
				map[string]string{"MARIADB_ROOT_PASSWORD": pw, "MARIADB_DATABASE": db},
				map[string]string{"URL": fmt.Sprintf("mysql://root:%s@%s:3306/%s", pw, name, db),
					"HOST": name, "PORT": "3306", "USER": "root", "PASSWORD": pw, "DATABASE": db}
		}},
	"redis": {image: "redis", version: "7", envName: "REDIS_URL",
		build: func(name, db, pw string) (map[string]any, map[string]string, map[string]string) {
			return map[string]any{
					"port":    6379,
					"volumes": []string{"data:/data"},
					"command": "redis-server --requirepass " + pw + " --appendonly yes",
					"health":  map[string]any{"command": "redis-cli -a " + pw + " --no-auth-warning ping", "timeout": "60s"},
				},
				map[string]string{},
				map[string]string{"URL": fmt.Sprintf("redis://default:%s@%s:6379", pw, name),
					"HOST": name, "PORT": "6379", "USER": "default", "PASSWORD": pw}
		}},
	"mongo": {image: "mongo", version: "8", envName: "MONGODB_URL",
		build: func(name, db, pw string) (map[string]any, map[string]string, map[string]string) {
			return map[string]any{
					"port":    27017,
					"volumes": []string{"data:/data/db"},
					// While initializing, mongod runs without auth on 127.0.0.1 only and
					// then restarts. Ready means: reachable on the container's own
					// address, with the root credentials.
					"health": map[string]any{"command": `mongosh --quiet --host "$(hostname)" -u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin --eval "db.adminCommand('ping').ok"`, "timeout": "120s"},
				},
				map[string]string{"MONGO_INITDB_ROOT_USERNAME": "root", "MONGO_INITDB_ROOT_PASSWORD": pw},
				map[string]string{"URL": fmt.Sprintf("mongodb://root:%s@%s:27017/%s?authSource=admin", pw, name, db),
					"HOST": name, "PORT": "27017", "USER": "root", "PASSWORD": pw, "DATABASE": db}
		}},
}

func engineNames() string {
	names := make([]string, 0, len(dbEngines))
	for n := range dbEngines {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (c *CLI) dbCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "db",
		Aliases: []string{"database"},
		Short:   "Create and link managed databases",
		Long: `Run databases next to your apps. A database is a single-replica service with
a persistent volume and generated credentials, reachable by other apps on the
server at <name>:<port>. It is not exposed to the internet.

Supported engines: ` + engineNames() + `.`,
		Example: `  saba db create main --engine postgres
  saba db link main -a api            # sets DATABASE_URL on api and redeploys it
  saba db url main                    # print the connection URL
  saba db backup schedule main @daily # automatic backups, newest 7 kept
  saba db backup create main -o main.sql.gz
  saba db restore main --backup latest --yes
  saba destroy main --yes --volumes   # delete it, including data and backups`,
	}

	var engine, version, publish, memory, node string
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a database",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := api.ValidateName(name); err != nil {
				return err
			}
			eng, ok := dbEngines[engine]
			if !ok {
				return api.Errorf(api.CodeInvalid, "unknown database engine %q", engine).
					WithHint("Supported engines: %s.", engineNames())
			}
			if version == "" {
				version = eng.version
			}
			pw, err := randomSecret()
			if err != nil {
				return err
			}
			dbName := strings.ReplaceAll(name, "-", "_")
			cfg, env, outputs := eng.build(name, dbName, pw)
			cfg["strategy"] = api.StrategyRecreate
			if publish != "" {
				cfg["publish"] = []string{publish}
			}
			if memory != "" {
				cfg["resources"] = map[string]any{"memory": memory}
			}
			if node != "" {
				cfg["nodes"] = []string{node}
			}
			req := api.DeployRequest{
				Source:     api.Source{Type: "image", Image: eng.image + ":" + version},
				Config:     cfg,
				EnvSet:     env,
				Kind:       api.KindDatabase,
				Database:   &api.DatabaseInfo{Engine: engine, Version: version, EnvName: eng.envName},
				Outputs:    outputs,
				CreateOnly: true,
				Trigger:    "create",
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			if !c.jsonOut {
				c.ui.printf("%s Creating %s database %s %s\n", c.ui.cyan("▶"), engine, c.ui.bold(name), c.ui.dim("→ "+cl.Target.String()))
			}
			r := &deployRenderer{c: c, quiet: true}
			result, err := cl.Deploy(cmd.Context(), name, req, nil, false, r.handle)
			if err != nil {
				return r.fail(err)
			}
			out := map[string]any{"name": name, "engine": engine, "version": version, "url": outputs["URL"],
				"env_name": eng.envName, "deployment": result.Deployment}
			return c.result(out, func() {
				c.ui.success("%s is ready", c.ui.bold(name))
				c.ui.printf("  %s %s\n", c.ui.dim(pad("URL", 6)), outputs["URL"])
				c.ui.note("Reachable from other apps on this server only. Connect an app with `saba db link %s -a <app>`.", name)
			})
		},
	}
	create.Flags().StringVar(&engine, "engine", "postgres", "Database engine: "+engineNames())
	create.Flags().StringVar(&version, "version", "", "Image tag of the engine (default: a current stable release)")
	create.Flags().StringVar(&publish, "publish", "", "Also expose the database on the host, e.g. 5432:5432 (not recommended)")
	create.Flags().StringVar(&memory, "memory", "", "Memory limit (e.g. 1g)")
	create.Flags().StringVar(&node, "node", "", "Server to run the database on (default: the primary)")

	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List databases", Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := c.api()
			if err != nil {
				return err
			}
			apps, err := cl.Apps(cmd.Context())
			if err != nil {
				return err
			}
			dbs := []*api.App{}
			for _, a := range apps {
				if a.Kind == api.KindDatabase {
					dbs = append(dbs, a)
				}
			}
			return c.result(dbs, func() {
				if len(dbs) == 0 {
					c.ui.printf("No databases. Create one with `saba db create <name> --engine postgres`.\n")
					return
				}
				rows := make([][]string, 0, len(dbs))
				for _, a := range dbs {
					eng := "-"
					if a.Database != nil {
						eng = a.Database.Engine + " " + a.Database.Version
					}
					rows = append(rows, []string{a.Name, eng, c.ui.status(a.Status), a.Name + ":" + fmt.Sprint(a.Config.Port)})
				}
				c.ui.table([]string{"NAME", "ENGINE", "STATUS", "INTERNAL ADDRESS"}, rows)
			})
		},
	}

	url := &cobra.Command{
		Use:   "url <name>",
		Short: "Print a database's connection URL and credentials",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := c.api()
			if err != nil {
				return err
			}
			out, err := cl.Outputs(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if out["URL"] == "" {
				return api.Errorf(api.CodeNotFound, "%q is not a managed database", args[0]).
					WithHint("List databases with `saba db list`.")
			}
			return c.result(out, func() { fmt.Fprintln(c.stdout, out["URL"]) })
		},
	}

	var as string
	var noDeploy bool
	link := &cobra.Command{
		Use:   "link <database> -a <app>",
		Short: "Give an app the database's URL as an environment variable",
		Long: `Set a variable on the app that references the database's URL, e.g.
DATABASE_URL=${{main.URL}}, and roll out the app. The reference is resolved at
deploy time, so the app never stores a copy of the credentials.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db := args[0]
			name, err := c.app()
			if err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			dbApp, err := cl.App(cmd.Context(), db)
			if err != nil {
				return err
			}
			if dbApp.Kind != api.KindDatabase || dbApp.Database == nil {
				return api.Errorf(api.CodeInvalid, "%q is not a managed database", db).
					WithHint("To reference another app's variable, set it directly: `saba env set -a %s 'KEY=${{%s.VAR}}'`.", name, db)
			}
			key := as
			if key == "" {
				key = dbApp.Database.EnvName
			}
			return c.envApply(cmd, map[string]string{key: "${{" + db + ".URL}}"}, nil, noDeploy)
		},
	}
	link.Flags().StringVar(&as, "as", "", "Variable name to set (default: DATABASE_URL, REDIS_URL or MONGODB_URL)")
	link.Flags().BoolVar(&noDeploy, "no-deploy", false, "Save the variable without rolling out the app")

	cmd.AddCommand(create, list, url, link)
	cmd.AddCommand(c.dbBackupCmds()...)
	return cmd
}

func randomSecret() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ---- cron ------------------------------------------------------------------

func (c *CLI) cronCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cron",
		Short: "List and trigger an app's scheduled jobs",
		Long: `Cron jobs are declared in saba.toml and run in a fresh container of the app's
live image, with its environment and volumes. Schedules are in UTC.

  [[cron]]
  name = "cleanup"
  schedule = "0 3 * * *"
  command = "node scripts/cleanup.js"`,
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error { return c.cronList(cmd) },
	}
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List cron jobs and their last result", Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error { return c.cronList(cmd) },
	}
	run := &cobra.Command{
		Use:   "run <job>",
		Short: "Run a cron job now and print its output",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := c.app()
			if err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			state, err := cl.CronRun(cmd.Context(), name, args[0])
			if err != nil {
				return err
			}
			if err := c.result(state, func() {
				if state.Output != "" {
					fmt.Fprintln(c.stdout, state.Output)
				}
				if state.LastStatus == "succeeded" {
					c.ui.success("%s finished in %s", args[0], state.Duration)
				}
			}); err != nil {
				return err
			}
			if state.LastStatus != "succeeded" {
				return api.Errorf(api.CodeCommandFailed, "cron job %q %s with exit code %d", args[0], state.LastStatus, state.ExitCode).
					WithDetail("exit_code", state.ExitCode)
			}
			return nil
		},
	}
	cmd.AddCommand(list, run)
	return cmd
}

type cronView struct {
	api.CronJob
	State *api.CronState `json:"last_run,omitempty"`
}

func (c *CLI) cronList(cmd *cobra.Command) error {
	name, err := c.app()
	if err != nil {
		return err
	}
	cl, err := c.api()
	if err != nil {
		return err
	}
	app, err := cl.App(cmd.Context(), name)
	if err != nil {
		return err
	}
	jobs := []cronView{}
	if cur := currentDeployment(app); cur != nil {
		for _, j := range cur.Config.Cron {
			jobs = append(jobs, cronView{CronJob: j, State: app.Cron[j.Name]})
		}
	}
	return c.result(jobs, func() {
		if len(jobs) == 0 {
			c.ui.printf("%s has no cron jobs. Declare them under [[cron]] in saba.toml and deploy.\n", name)
			return
		}
		rows := make([][]string, 0, len(jobs))
		for _, j := range jobs {
			last, st, took := "never", "-", "-"
			if j.State != nil {
				last, st, took = ago(j.State.LastRun), c.ui.status(j.State.LastStatus), j.State.Duration
			}
			rows = append(rows, []string{j.Name, j.Schedule, truncate(j.Command, 50), last, st, took})
		}
		c.ui.table([]string{"JOB", "SCHEDULE (UTC)", "COMMAND", "LAST RUN", "RESULT", "TOOK"}, rows)
	})
}
