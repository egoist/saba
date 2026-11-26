package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/egoist/saba/internal/api"
)

func (c *CLI) envCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Manage environment variables and secrets",
		Long: `Manage an app's environment variables. Values are stored on the server.

Changing variables rolls out a new version with zero downtime, unless
--no-deploy is given (then the change applies on the next deploy or restart).

A value can reference another app's variables with ${{app.KEY}}, e.g.
DATABASE_URL='${{db.URL}}' — it is resolved at deploy time.`,
		Example: `  saba env                                  # list variables of the app in ./saba.toml
  saba env set -a api SECRET=abc LOG_LEVEL=debug
  saba env set -a api --no-deploy FEATURE_X=1
  saba env unset -a api FEATURE_X
  saba env import -a api .env.production
  saba env get -a api SECRET`,
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error { return c.envList(cmd, false) },
	}
	var resolved bool
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List environment variables",
		Args:    maxArgs(0),
		RunE:    func(cmd *cobra.Command, args []string) error { return c.envList(cmd, resolved) },
	}
	list.Flags().BoolVar(&resolved, "resolved", false, "Show values with ${{app.KEY}} references expanded")

	get := &cobra.Command{
		Use:   "get <KEY>",
		Short: "Print the value of one variable",
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
			env, err := cl.Env(cmd.Context(), name)
			if err != nil {
				return err
			}
			v, ok := env.Env[args[0]]
			if !ok {
				return api.Errorf(api.CodeNotFound, "%s is not set on %q", args[0], name).
					WithHint("List variables with `saba env -a %s`.", name)
			}
			return c.result(map[string]string{"key": args[0], "value": v}, func() { fmt.Fprintln(c.stdout, v) })
		},
	}

	var noDeploy bool
	set := &cobra.Command{
		Use:   "set <KEY=VALUE>...",
		Short: "Set variables and roll out the change",
		Args:  minArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vars := map[string]string{}
			for _, kv := range args {
				k, v, err := splitEnv(kv)
				if err != nil {
					return err
				}
				vars[k] = v
			}
			return c.envApply(cmd, vars, nil, noDeploy)
		},
	}
	unset := &cobra.Command{
		Use:     "unset <KEY>...",
		Aliases: []string{"rm", "remove"},
		Short:   "Remove variables and roll out the change",
		Args:    minArgs(1),
		RunE:    func(cmd *cobra.Command, args []string) error { return c.envApply(cmd, nil, args, noDeploy) },
	}
	imp := &cobra.Command{
		Use:   "import <file>",
		Short: "Set variables from a dotenv file (use - for stdin)",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vars, err := readEnvFile(args[0])
			if err != nil {
				return err
			}
			if len(vars) == 0 {
				return api.Errorf(api.CodeInvalid, "%s contains no variables", args[0])
			}
			return c.envApply(cmd, vars, nil, noDeploy)
		},
	}
	for _, sub := range []*cobra.Command{set, unset, imp} {
		sub.Flags().BoolVar(&noDeploy, "no-deploy", false, "Save the change without rolling out a new version")
	}
	cmd.AddCommand(list, get, set, unset, imp)
	return cmd
}

func (c *CLI) envList(cmd *cobra.Command, resolved bool) error {
	name, err := c.app()
	if err != nil {
		return err
	}
	cl, err := c.api()
	if err != nil {
		return err
	}
	env, err := cl.Env(cmd.Context(), name)
	if err != nil {
		return err
	}
	return c.result(env, func() {
		values := env.Env
		if resolved && env.Resolved != nil {
			values = env.Resolved
		}
		if len(values) == 0 {
			c.ui.printf("%s has no environment variables. Set one with `saba env set -a %s KEY=VALUE`.\n", name, name)
			return
		}
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		// dotenv-compatible output: can be redirected straight into a file.
		for _, k := range keys {
			fmt.Fprintf(c.stdout, "%s=%s\n", k, dotenvQuote(values[k]))
		}
	})
}

func dotenvQuote(v string) string {
	if v == "" || strings.ContainsAny(v, " \t\n\"'#$\\") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v) + `"`
	}
	return v
}

func (c *CLI) envApply(cmd *cobra.Command, set map[string]string, unset []string, noDeploy bool) error {
	name, err := c.app()
	if err != nil {
		return err
	}
	var changed []string
	for k := range set {
		changed = append(changed, k)
	}
	changed = append(changed, unset...)
	sort.Strings(changed)
	req := api.DeployRequest{
		Source:   api.Source{Type: "current"},
		EnvSet:   set,
		EnvUnset: unset,
		NoDeploy: noDeploy,
		IfLive:   true,
		Trigger:  "env",
		Message:  truncate("env: "+strings.Join(changed, ", "), 100),
	}
	return c.redeploy(cmd, name, req, true)
}
