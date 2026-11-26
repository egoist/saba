package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/cluster"
	"github.com/egoist/saba/internal/runtime"
	"github.com/egoist/saba/internal/shellwords"
)

const (
	agentConfigPath = "/etc/saba/agent.json"
	agentDataDir    = "/var/lib/saba-agent"
	agentUnitPath   = "/etc/systemd/system/saba-agent.service"
)

func (c *CLI) agentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run on worker nodes: connect this machine to a saba primary",
		Long: `The agent runs on every node except the primary. It connects out to the
primary over TLS and carries Docker control and replica traffic over that one
connection, so the node needs no open ports.

"saba node add" installs it for you over SSH. To install it by hand, run the
two commands "saba node add <name>" prints, on the node.`,
	}

	var (
		join      string
		printUnit bool
	)
	install := &cobra.Command{
		Use:   "install --join <code>",
		Short: "Join a cluster and install the agent as a systemd service (as root)",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			j, err := cluster.ParseJoin(join)
			if err != nil {
				return api.Errorf(api.CodeInvalid, "%v", err).WithHint("Get a join code with `saba node add <name>` on your machine.")
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if exe, err = filepath.EvalSymlinks(exe); err != nil {
				return err
			}
			unit := fmt.Sprintf(`[Unit]
Description=saba agent: connects this node to its saba primary
Documentation=https://github.com/egoist/saba
After=network-online.target docker.service
Wants=network-online.target docker.service

[Service]
ExecStart=%s agent run
Restart=always
RestartSec=2
# Containers are managed by Docker and must survive agent restarts.
KillMode=process

[Install]
WantedBy=multi-user.target
`, shellwords.Quote(exe))
			if printUnit {
				fmt.Fprint(c.stdout, unit)
				return nil
			}
			if os.Geteuid() != 0 {
				return api.Errorf(api.CodeInvalid, "installing the agent requires root").WithHint("Run it with sudo.")
			}
			if err := cluster.ConfigFromJoin(j).Save(agentConfigPath); err != nil {
				return err
			}
			if _, err := cluster.LoadIdentity(agentDataDir, "agent"); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(agentUnitPath), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(agentUnitPath, []byte(unit), 0o644); err != nil {
				return err
			}
			for _, a := range [][]string{{"daemon-reload"}, {"enable", "saba-agent.service"}, {"restart", "saba-agent.service"}} {
				if out, err := exec.CommandContext(cmd.Context(), "systemctl", a...).CombinedOutput(); err != nil {
					return api.Errorf(api.CodeRuntime, "systemctl %s failed: %s", strings.Join(a, " "), strings.TrimSpace(string(out)))
				}
			}
			return c.result(map[string]any{"node": j.Node, "primary": j.Primary, "installed": true}, func() {
				c.ui.success("Agent installed; node %s connects to %s", c.ui.bold(j.Node), j.Primary)
			})
		},
	}
	install.Flags().StringVar(&join, "join", "", "Join code from `saba node add`")
	install.Flags().BoolVar(&printUnit, "print-unit", false, "Print the systemd unit instead of installing it")

	var configPath, dataDir, dockerSocket, runJoin string
	run := &cobra.Command{
		Use:   "run",
		Short: "Run the agent in the foreground (what the service runs)",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if runJoin != "" {
				j, err := cluster.ParseJoin(runJoin)
				if err != nil {
					return api.Errorf(api.CodeInvalid, "%v", err)
				}
				if err := cluster.ConfigFromJoin(j).Save(configPath); err != nil {
					return err
				}
			}
			if _, err := cluster.LoadAgentConfig(configPath); err != nil {
				return api.Errorf(api.CodeInvalid, "no agent configuration: %v", err).
					WithHint("Join a cluster first: `saba agent install --join <code>` (or pass --join here).")
			}
			docker := runtime.NewDocker()
			a := &cluster.Agent{
				ConfigPath: configPath, DataDir: dataDir, DockerSocket: dockerSocket, Version: Version,
				Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
				DockerVersion: func(ctx context.Context) string {
					v, _ := docker.Version(ctx)
					return v
				},
				Upgrade: func(ctx context.Context, version string) error {
					return cluster.SelfUpgrade(ctx, releasesURL, version)
				},
			}
			err := a.Run(cmd.Context())
			if errors.Is(err, cluster.ErrUpgraded) {
				// Continue as the new binary, whether or not a supervisor runs us.
				exe, _ := os.Executable()
				return syscall.Exec(exe, os.Args, os.Environ())
			}
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}
	run.Flags().StringVar(&configPath, "config", agentConfigPath, "Agent configuration file")
	run.Flags().StringVar(&dataDir, "data-dir", agentDataDir, "Directory for the agent's key")
	run.Flags().StringVar(&dockerSocket, "docker-socket", "/var/run/docker.sock", "Docker socket to expose to the primary")
	run.Flags().StringVar(&runJoin, "join", "", "Join code; writes the configuration before running")

	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the agent and remove it (containers and data on the node are kept)",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Geteuid() != 0 {
				return api.Errorf(api.CodeInvalid, "removing the agent requires root").WithHint("Run it with sudo.")
			}
			_ = exec.CommandContext(cmd.Context(), "systemctl", "disable", "--now", "saba-agent.service").Run()
			os.Remove(agentUnitPath)
			_ = exec.CommandContext(cmd.Context(), "systemctl", "daemon-reload").Run()
			os.Remove(agentConfigPath)
			os.RemoveAll(agentDataDir)
			return c.result(map[string]any{"uninstalled": true}, func() { c.ui.success("Agent removed") })
		},
	}
	cmd.AddCommand(install, run, uninstall)
	return cmd
}
