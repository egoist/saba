// Package cli implements the saba command line interface.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/client"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// CLI carries state shared by all commands.
type CLI struct {
	stdout io.Writer
	stderr io.Writer

	jsonOut  bool
	noColor  bool
	hostFlag string
	ctxFlag  string
	appFlag  string

	ui      *UI
	project *Project
	client  *client.Client
}

// exitError carries the exit code of a remote command through cobra.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// Main runs the CLI and returns the process exit code.
func Main(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return Run(ctx, args, os.Stdout, os.Stderr)
}

// Run executes one CLI invocation with the given output streams.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c := &CLI{stdout: stdout, stderr: stderr}
	// Errors raised before flags are parsed (unknown command, bad flag) must
	// honor --json too, so detect it up front.
	if v := os.Getenv("SABA_JSON"); v != "" && v != "0" {
		c.jsonOut = true
	}
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "--json" || a == "--json=true" {
			c.jsonOut = true
		}
	}
	root := c.rootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.ExecuteContext(ctx)
	if c.client != nil {
		c.client.Close()
	}
	if err == nil {
		return api.ExitOK
	}
	var ee exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		return 130
	}
	ae := toAPIError(err)
	c.printError(ae)
	return ae.ExitCode()
}

// toAPIError classifies errors that did not originate from saba itself
// (cobra's argument and flag errors) as usage errors.
func toAPIError(err error) *api.Error {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae
	}
	msg := err.Error()
	for _, p := range []string{"unknown command", "unknown flag", "unknown shorthand", "flag needs an argument", "invalid argument", "accepts ", "requires ", "bad flag syntax", "required flag"} {
		if strings.Contains(msg, p) {
			return api.Errorf(api.CodeInvalid, "%s", msg).WithHint("Run the command with --help to see its usage, or `saba guide` for an overview.")
		}
	}
	return api.Errorf(api.CodeInternal, "%s", msg)
}

func (c *CLI) printError(e *api.Error) {
	if c.jsonOut {
		enc := json.NewEncoder(c.stderr)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(api.ErrorResponse{Error: e})
		return
	}
	ui := c.ui
	if ui == nil {
		ui = newUI(c.stderr, c.noColor)
	}
	fmt.Fprintf(c.stderr, "%s %s\n", ui.red("✗"), e.Message)
	if e.Hint != "" {
		fmt.Fprintf(c.stderr, "  %s %s\n", ui.dim("hint:"), e.Hint)
	}
	fmt.Fprintf(c.stderr, "  %s %s\n", ui.dim("code:"), e.Code)
}

func (c *CLI) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "saba",
		Short: "Deploy and run apps on your own servers",
		Long: `saba deploys apps to your own servers with zero downtime.

One binary: a daemon on the server (reverse proxy with automatic HTTPS,
rolling deploys, rollbacks, managed databases, cron) and this CLI, which
talks to it over plain SSH.

Get started:
  saba server setup root@203.0.113.10   Install saba on a server
  saba deploy                           Deploy the current directory
  saba status                           See what is running
  saba logs -f                          Tail logs

Every command accepts --json for machine-readable output.
AI agents: run "saba guide" for a complete, compact reference.`,
		SilenceUsage:      true,
		SilenceErrors:     true,
		Version:           Version,
		CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: true},
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			c.ui = newUI(c.stdout, c.noColor || c.jsonOut)
		},
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&c.jsonOut, "json", c.jsonOut, "Print machine-readable JSON, errors included (env SABA_JSON=1)")
	pf.BoolVar(&c.noColor, "no-color", false, "Disable colors")
	pf.StringVarP(&c.hostFlag, "host", "H", "", "Server to talk to: user@host, or \"local\" (env SABA_HOST)")
	pf.StringVarP(&c.ctxFlag, "context", "c", "", "Named server context to use (env SABA_CONTEXT)")
	pf.StringVarP(&c.appFlag, "app", "a", "", "App name (default: name in ./saba.toml, env SABA_APP)")
	root.SetVersionTemplate("saba {{.Version}}\n")
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return api.Errorf(api.CodeInvalid, "%v", err).WithHint("Run `%s --help` to see the available flags.", cmd.CommandPath())
	})

	groups := []*cobra.Group{
		{ID: "deploy", Title: "Deploy:"},
		{ID: "observe", Title: "Observe:"},
		{ID: "manage", Title: "Manage apps:"},
		{ID: "server", Title: "Servers:"},
		{ID: "more", Title: "More:"},
	}
	root.AddGroup(groups...)
	add := func(group string, cmds ...*cobra.Command) {
		for _, cmd := range cmds {
			cmd.GroupID = group
			root.AddCommand(cmd)
		}
	}
	add("deploy", c.initCmd(), c.deployCmd(), c.cancelCmd(), c.rollbackCmd(), c.deploymentsCmd())
	add("observe", c.appsCmd(), c.statusCmd(), c.logsCmd(), c.statsCmd(), c.doctorCmd())
	add("manage", c.envCmd(), c.domainsCmd(), c.scaleCmd(), c.restartCmd(), c.stopCmd(), c.startCmd(),
		c.execCmd(), c.runCmd(), c.dbCmd(), c.cronCmd(), c.destroyCmd())
	add("server", c.serverCmd(), c.nodeCmd(), c.agentCmd(), c.certsCmd(), c.contextCmd())
	add("more", c.guideCmd(), c.mcpCmd(), c.versionCmd())
	root.AddCommand(c.daemonCmd(), c.dialStdioCmd())
	root.SetHelpCommandGroupID("more")
	return root
}

// api returns the daemon client, resolving the target on first use.
func (c *CLI) api() (*client.Client, error) {
	if c.client != nil {
		return c.client, nil
	}
	projectCtx := ""
	if p := c.loadProject(); p != nil {
		projectCtx = p.Context
	}
	t, err := client.Resolve(c.hostFlag, c.ctxFlag, projectCtx)
	if err != nil {
		return nil, err
	}
	c.client = client.New(t)
	return c.client, nil
}

// loadProject reads ./saba.toml once. A missing file is not an error.
func (c *CLI) loadProject() *Project {
	if c.project == nil {
		p, err := LoadProject(".")
		if err != nil {
			return nil
		}
		c.project = p
	}
	return c.project
}

var errNoApp = api.Errorf(api.CodeInvalid, "no app specified").
	WithHint("Pass `-a <app>`, set SABA_APP, or run the command in a directory with a saba.toml. List apps with `saba apps`.")

// app resolves the app a command operates on.
func (c *CLI) app(positional ...string) (string, error) {
	name := ""
	for _, p := range positional {
		if p != "" {
			name = p
			break
		}
	}
	if name == "" {
		name = c.appFlag
	}
	if name == "" {
		name = os.Getenv("SABA_APP")
	}
	if name == "" {
		if p, err := LoadProject("."); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		} else if p != nil {
			name = p.Name
		}
	}
	if name == "" {
		return "", errNoApp
	}
	return name, nil
}

// result prints v as JSON, or calls human for the terminal rendering.
func (c *CLI) result(v any, human func()) error {
	if c.jsonOut {
		enc := json.NewEncoder(c.stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(v)
	}
	human()
	return nil
}

// event prints one NDJSON event (JSON mode streaming).
func (c *CLI) event(ev any) {
	enc := json.NewEncoder(c.stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(ev)
}

// confirm guards destructive commands. saba never prompts: the caller must
// pass --yes, which keeps the CLI safe to drive from scripts and agents.
func (c *CLI) confirm(yes bool, action, retry string) error {
	if yes {
		return nil
	}
	return api.Errorf(api.CodeConfirmation, "%s is irreversible and needs confirmation", action).
		WithHint("Re-run with --yes to proceed: `%s --yes`.", retry)
}

func stdoutIsTerminal() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

func maxArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) > n {
			return api.Errorf(api.CodeInvalid, "`%s` takes at most %d argument(s), got %d", cmd.CommandPath(), n, len(args)).
				WithHint("Usage: %s", cmd.UseLine())
		}
		return nil
	}
}

func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return api.Errorf(api.CodeInvalid, "`%s` takes exactly %d argument(s), got %d", cmd.CommandPath(), n, len(args)).
				WithHint("Usage: %s", cmd.UseLine())
		}
		return nil
	}
}

func minArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < n {
			return api.Errorf(api.CodeInvalid, "`%s` needs at least %d argument(s), got %d", cmd.CommandPath(), n, len(args)).
				WithHint("Usage: %s", cmd.UseLine())
		}
		return nil
	}
}
