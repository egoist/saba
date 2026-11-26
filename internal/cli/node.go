package cli

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/client"
	"github.com/egoist/saba/internal/shellwords"
)

func (c *CLI) nodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "node",
		Aliases: []string{"nodes"},
		Short:   "Add servers to scale apps across machines",
		Long: `Spread an app's replicas across several servers.

The server running the saba daemon is the primary (node "local"). Additional
nodes need only Docker and SSH access from the primary: no saba install and no
open ports. The primary drives each node's Docker engine and reaches its
replicas through one persistent SSH connection, so traffic between servers is
encrypted. The primary keeps terminating HTTPS and load-balances all replicas.

Place an app with "nodes" in saba.toml or --node on deploy:

  nodes = ["local", "worker1"]     # or ["*"] for every node
  replicas = 4                     # spread round-robin: 2 per server

Good to know:
  - Apps with volumes and databases run on a single node.
  - Containers reach each other by name only on the same server. An app that
    publishes a port (publish = ["10.0.0.1:5432:5432"]) is also reachable by
    name from other nodes, at its server's address (--address).`,
		Example: `  saba node add worker1 root@10.0.0.2
  saba node list
  saba deploy -a web --node local --node worker1 --replicas 4
  saba node remove worker1 --yes`,
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error { return c.nodeList(cmd) },
	}
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List the cluster's servers", Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error { return c.nodeList(cmd) },
	}

	var (
		address     string
		noAuthorize bool
		direct      bool
	)
	add := &cobra.Command{
		Use:   "add <name> <user@host>",
		Short: "Add a server to the cluster",
		Long: `Register a server as a node. Two things happen:

  1. From this machine, saba logs in to the node over SSH, authorizes the
     primary's public key there and reads the node's host keys (so the primary
     can verify it is talking to the same machine).
  2. The primary connects to the node and checks its Docker engine.

<user@host> must be reachable from the primary. If this machine cannot reach
the node (e.g. it only has a private address), use --no-authorize and add the
key printed by "saba node key" to the node's ~/.ssh/authorized_keys yourself.

Traffic to the node's replicas goes through the SSH connection by default:
encrypted, and nothing to configure. When the servers share a private network,
--direct --address <private-ip> makes the primary connect to replicas straight
over that network instead, which is faster. Replica ports are then open on
that address, unencrypted, so only use it on a network you trust. Run the
command again to change a node's settings; apps pick them up on their next
deploy or restart.`,
		Example: "  saba node add worker1 root@203.0.113.20\n  saba node add worker1 root@10.0.0.2 --address 10.0.0.2 --direct",
		Args:    exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, target := args[0], args[1]
			if err := api.ValidateName(name); err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			node := api.Node{Name: name, SSH: target, Address: address, Direct: direct}
			if !noAuthorize {
				key, err := cl.NodeKey(cmd.Context())
				if err != nil {
					return err
				}
				hostKeys, err := authorizeNode(cmd, target, key)
				if err != nil {
					return err
				}
				node.HostKeys = hostKeys
			}
			added, err := cl.NodeAdd(cmd.Context(), node)
			if err != nil {
				return err
			}
			return c.result(added, func() {
				mode := "traffic goes through the SSH tunnel"
				if added.Direct {
					mode = "traffic goes directly to " + added.Address
				}
				c.ui.success("Node %s added (Docker %s, %s)", c.ui.bold(name), added.Docker, mode)
				c.ui.note("Run replicas on it with `saba deploy -a <app> --node local --node %s --replicas 2`.", name)
			})
		},
	}
	add.Flags().StringVar(&address, "address", "", "The node's address on the network shared with the other servers (for cross-node access to published ports)")
	add.Flags().BoolVar(&direct, "direct", false, "Send replica traffic straight to --address over a private network instead of through the SSH tunnel (faster, unencrypted)")
	add.Flags().BoolVar(&noAuthorize, "no-authorize", false, "Do not log in to the node from this machine; the primary's key is already authorized there")

	var yes bool
	remove := &cobra.Command{
		Use: "remove <name>", Aliases: []string{"rm"}, Short: "Remove a server from the cluster", Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.confirm(yes, "Removing node "+args[0], "saba node remove "+args[0]); err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			if err := cl.NodeRemove(cmd.Context(), args[0]); err != nil {
				return err
			}
			return c.result(map[string]any{"node": args[0], "removed": true}, func() { c.ui.success("Node %s removed", args[0]) })
		},
	}
	remove.Flags().BoolVarP(&yes, "yes", "y", false, "Confirm the removal")

	key := &cobra.Command{
		Use: "key", Short: "Print the primary's public key (to authorize on nodes by hand)", Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := c.api()
			if err != nil {
				return err
			}
			k, err := cl.NodeKey(cmd.Context())
			if err != nil {
				return err
			}
			return c.result(map[string]string{"public_key": k}, func() { fmt.Fprintln(c.stdout, k) })
		},
	}
	cmd.AddCommand(list, add, remove, key)
	return cmd
}

func (c *CLI) nodeList(cmd *cobra.Command) error {
	cl, err := c.api()
	if err != nil {
		return err
	}
	nodes, err := cl.Nodes(cmd.Context())
	if err != nil {
		return err
	}
	return c.result(nodes, func() {
		rows := make([][]string, 0, len(nodes))
		for _, n := range nodes {
			target := n.SSH
			if n.Name == api.LocalNode {
				target = "(primary)"
			}
			status := c.ui.status(n.Status)
			if n.Status == "connected" {
				status = c.ui.green(n.Status)
			} else {
				status = c.ui.red(n.Status)
			}
			traffic := "ssh tunnel"
			switch {
			case n.Name == api.LocalNode:
				traffic = "-"
			case n.Direct:
				traffic = "direct"
			}
			rows = append(rows, []string{n.Name, status, target, orDash(n.Address), traffic, orDash(n.Docker), strconv.Itoa(n.Replicas)})
		}
		c.ui.table([]string{"NAME", "STATUS", "SSH", "ADDRESS", "TRAFFIC", "DOCKER", "REPLICAS"}, rows)
		for _, n := range nodes {
			if n.Error != "" {
				c.ui.printf("%s %s: %s\n", c.ui.red("!"), n.Name, n.Error)
			}
		}
	})
}

// authorizeNode logs in to a node from this machine, appends the primary's
// public key to authorized_keys and returns the node's SSH host keys.
func authorizeNode(cmd *cobra.Command, target, key string) ([]string, error) {
	k := shellwords.Quote(key)
	script := strings.Join([]string{
		"set -e",
		"mkdir -p ~/.ssh && chmod 700 ~/.ssh",
		"touch ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys",
		"grep -qxF " + k + " ~/.ssh/authorized_keys || echo " + k + " >> ~/.ssh/authorized_keys",
		"command -v docker >/dev/null 2>&1 || echo SABA_NO_DOCKER",
		"cat /etc/ssh/ssh_host_*_key.pub 2>/dev/null || true",
	}, "; ")
	argv := client.SSHCommand(target, false, script)
	proc := exec.CommandContext(cmd.Context(), argv[0], argv[1:]...)
	var out, errb bytes.Buffer
	proc.Stdout, proc.Stderr = &out, &errb
	proc.WaitDelay = 2 * time.Second
	if err := proc.Run(); err != nil && err != exec.ErrWaitDelay {
		return nil, api.Errorf(api.CodeUnreachable, "cannot log in to %s from this machine: %s", target, lastLine(orDash(strings.TrimSpace(errb.String())))).
			WithHint("If only the primary can reach the node, authorize the key from `saba node key` in the node's ~/.ssh/authorized_keys and re-run with --no-authorize.")
	}
	var hostKeys []string
	for _, line := range strings.Split(out.String(), "\n") {
		f := strings.Fields(line)
		switch {
		case strings.TrimSpace(line) == "SABA_NO_DOCKER":
			return nil, api.Errorf(api.CodeInvalid, "Docker is not installed on %s", target).
				WithHint("Install it on the node first: `ssh %s 'curl -fsSL https://get.docker.com | sh'`.", target)
		case len(f) >= 2 && (strings.HasPrefix(f[0], "ssh-") || strings.HasPrefix(f[0], "ecdsa-")):
			hostKeys = append(hostKeys, f[0]+" "+f[1])
		}
	}
	return hostKeys, nil
}
