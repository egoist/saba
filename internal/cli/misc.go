package cli

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/buildpack"
	"github.com/egoist/saba/internal/mcp"
)

//go:embed guide.md
var guideText string

func (c *CLI) initCmd() *cobra.Command {
	var (
		port  int
		image string
		force bool
	)
	cmd := &cobra.Command{
		Use:   "init [path]",
		Short: "Create a saba.toml for a project",
		Long: `Write a commented saba.toml that pins the app's name and settings. It is
optional: "saba deploy" works without it, taking the app name from the
directory and everything else from flags and what the server remembers.`,
		Example: "  saba init\n  saba init -a api --port 8080\n  saba init -a cache --image redis:7",
		Args:    maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			path := filepath.Join(dir, ProjectFile)
			if _, err := os.Stat(path); err == nil && !force {
				return api.Errorf(api.CodeConflict, "%s already exists", path).WithHint("Edit it, or overwrite it with --force.")
			}
			name := c.appFlag
			if name == "" {
				abs, _ := filepath.Abs(dir)
				name = api.SanitizeName(filepath.Base(abs))
			}
			if err := api.ValidateName(name); err != nil {
				return err
			}
			builder := "image"
			if image == "" {
				det, err := buildpack.Detect(dir)
				switch {
				case err == nil:
					builder = det.Builder
					if port == 0 {
						port = det.Port
					}
				default:
					var ae *api.Error
					if errors.As(err, &ae) {
						builder = "unknown"
					}
				}
			}
			if err := os.WriteFile(path, []byte(projectTemplate(name, port, image)), 0o644); err != nil {
				return err
			}
			out := map[string]any{"path": path, "name": name, "builder": builder, "port": port}
			return c.result(out, func() {
				c.ui.success("Wrote %s for app %s", path, c.ui.bold(name))
				switch builder {
				case "unknown":
					c.ui.note("No Dockerfile or supported stack detected. Add a Dockerfile, or set `image` in saba.toml.")
				case "image":
				case "dockerfile":
					c.ui.note("Builds with your Dockerfile.")
				default:
					c.ui.note("Detected a %s app; saba will generate the build. Add a Dockerfile to take control.", builder)
				}
				c.ui.printf("Next: `saba deploy`\n")
			})
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "Port the app listens on")
	cmd.Flags().StringVar(&image, "image", "", "Deploy this image instead of building the directory")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite an existing saba.toml")
	return cmd
}

func (c *CLI) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose problems with the server and its apps",
		Long: `Check the connection, Docker, disk space, DNS of routed domains and the
health of every app. Each failed check comes with a suggested fix. Exits with
code 1 if any check fails.`,
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := c.api()
			if err != nil {
				return err
			}
			rep, err := cl.Doctor(cmd.Context())
			if err != nil {
				return err
			}
			if err := c.result(rep, func() {
				for _, ch := range rep.Checks {
					mark := c.ui.green("✓")
					if !ch.OK {
						mark = c.ui.red("✗")
					}
					c.ui.printf("%s %s %s\n", mark, c.ui.dim(pad(ch.Name, 14)), ch.Message)
					if ch.Hint != "" {
						c.ui.printf("  %s %s\n", pad("", 14), c.ui.dim("→ "+ch.Hint))
					}
				}
			}); err != nil {
				return err
			}
			if !rep.OK {
				return exitError{api.ExitError}
			}
			return nil
		},
	}
}

func (c *CLI) guideCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "guide",
		Short: "Print the complete reference (written for AI agents and scripts)",
		Long: `Print a compact, complete reference of saba: concepts, every command, the
saba.toml schema, JSON output shapes, error codes and exit codes.

It is the fastest way for an AI agent to learn how to drive saba.`,
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			return c.result(map[string]string{"guide": guideText}, func() { fmt.Fprint(c.stdout, guideText) })
		},
	}
}

func (c *CLI) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version (and the server's, if reachable)",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := map[string]string{"client": Version, "os": runtime.GOOS, "arch": runtime.GOARCH}
			if cl, err := c.api(); err == nil {
				if info, err := cl.Info(cmd.Context()); err == nil {
					out["server"] = info.Version
					out["server_target"] = cl.Target.String()
				}
			}
			return c.result(out, func() {
				c.ui.printf("saba %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
				if v, ok := out["server"]; ok {
					c.ui.printf("server %s (%s)\n", v, out["server_target"])
				}
			})
		},
	}
}

func (c *CLI) mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run a Model Context Protocol server on stdio",
		Long: `Expose saba as MCP tools so AI agents can deploy and operate apps.

Each tool maps to a CLI command and returns its --json output. The server
uses the same server selection as the CLI (--host, --context, saba.toml,
current context), fixed at startup.

Example client configuration:
  { "mcpServers": { "saba": { "command": "saba", "args": ["mcp"] } } }
  { "mcpServers": { "saba": { "command": "saba", "args": ["mcp", "--context", "prod"] } } }`,
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			var global []string
			if c.hostFlag != "" {
				global = append(global, "--host", c.hostFlag)
			}
			if c.ctxFlag != "" {
				global = append(global, "--context", c.ctxFlag)
			}
			srv := &mcp.Server{Exe: exe, GlobalArgs: global, Version: Version, Guide: guideText}
			return srv.Serve(cmd.Context(), os.Stdin, os.Stdout)
		},
	}
}
