package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/client"
	"github.com/egoist/saba/internal/cluster"
	"github.com/egoist/saba/internal/shellwords"
)

func (c *CLI) nodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "node",
		Aliases: []string{"nodes"},
		Short:   "Add servers to scale apps across machines",
		Long: `Spread an app's replicas across several servers.

The server running the saba daemon is the primary (node "local"). Every other
node runs the saba agent, which connects out to the primary over TLS. That one
encrypted connection carries everything: Docker control and the traffic to the
node's replicas. Nodes need Docker and no open ports, so they can sit behind
NAT; the primary only needs its HTTPS port reachable from them.

Place an app with "nodes" in saba.toml or --node on deploy:

  nodes = ["local", "worker1"]     # or ["*"] for every node
  replicas = 4                     # spread round-robin: 2 per server

Good to know:
  - Apps with volumes and databases run on a single node.
  - Containers reach each other by name only on the same server. An app that
    publishes a port (publish = ["10.0.0.1:5432:5432"]) is also reachable by
    name from other nodes, at its server's address (--address).`,
		Example: `  saba node add worker1 root@203.0.113.20     # install the agent over SSH
  saba node add homelab                       # print a join command to run on the node
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

	var f nodeAddFlags
	add := &cobra.Command{
		Use:   "add <name> [user@host]",
		Short: "Add a server to the cluster",
		Long: `Add a server as a node. With an SSH destination, saba installs everything:
it connects to the server, checks Docker (installing it with
--install-docker), installs the saba binary and the agent service, and waits
for the agent to connect.

Without a destination, saba prints a join command to run on the server
yourself. Use that for machines you cannot reach over SSH from here, such as a
home server behind NAT. The join code works once and expires after 24 hours.

The agent connects to the primary at --primary-address, by default the
primary's "address" setting or its public IP, on its HTTPS port (443).

Running it again for a joined node updates its --address. --rejoin issues a new
join code for a reinstalled machine.`,
		Example: `  saba node add worker1 root@203.0.113.20
  saba node add worker1 deploy@10.0.0.2 --address 10.0.0.2 --primary-address 10.0.0.1
  saba node add homelab
  saba node add worker1 root@203.0.113.20 --rejoin`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) == 2 {
				target = args[1]
			}
			return c.nodeAdd(cmd.Context(), args[0], target, &f)
		},
	}
	fl := add.Flags()
	fl.StringVar(&f.address, "address", "", "The node's IP on the network shared with the other servers (for cross-node access to published ports)")
	fl.StringVar(&f.primary, "primary-address", "", "Address the node uses to reach the primary (default: the primary's address setting, or its public IP)")
	fl.BoolVar(&f.rejoin, "rejoin", false, "Issue a new join code and forget the node's previous key")
	fl.BoolVar(&f.installDocker, "install-docker", false, "Install Docker on the node if it is missing")
	fl.StringVar(&f.binary, "binary", "", "Upload this local Linux binary instead of downloading a release (for unreleased builds)")

	var (
		yes       bool
		keepAgent bool
	)
	remove := &cobra.Command{
		Use: "remove <name>", Aliases: []string{"rm"}, Short: "Remove a server from the cluster", Args: exactArgs(1),
		Long: `Remove a node. It refuses while apps are placed on the node. When the agent
was installed over SSH, it is uninstalled the same way unless --keep-agent is
given. Containers, images and volumes on the node are not touched.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.confirm(yes, "Removing node "+args[0], "saba node remove "+args[0]); err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			var node *api.Node
			if nodes, err := cl.Nodes(cmd.Context()); err == nil {
				for i := range nodes {
					if nodes[i].Name == args[0] {
						node = &nodes[i]
					}
				}
			}
			if err := cl.NodeRemove(cmd.Context(), args[0]); err != nil {
				return err
			}
			uninstalled := false
			if node != nil && node.SSH != "" && !keepAgent {
				host := c.newRemote(cmd.Context(), node.SSH, "node")
				if err := host.probe(); err == nil {
					_, err = host.run(host.sudo+"/usr/local/bin/saba agent uninstall", nil, false)
					uninstalled = err == nil
				}
			}
			return c.result(map[string]any{"node": args[0], "removed": true, "agent_uninstalled": uninstalled}, func() {
				c.ui.success("Node %s removed", args[0])
				if !uninstalled {
					c.ui.note("The agent on the node now gets refused. Uninstall it there with `sudo saba agent uninstall`.")
				}
			})
		},
	}
	remove.Flags().BoolVarP(&yes, "yes", "y", false, "Confirm the removal")
	remove.Flags().BoolVar(&keepAgent, "keep-agent", false, "Do not uninstall the agent from the node")
	cmd.AddCommand(list, add, remove)
	return cmd
}

type nodeAddFlags struct {
	address, primary, binary string
	rejoin, installDocker    bool
}

type nodeAddResult struct {
	api.NodeAddResponse
	// Instructions is set when the agent must be installed by hand.
	Instructions []string `json:"instructions,omitempty"`
}

func (c *CLI) nodeAdd(ctx context.Context, name, target string, f *nodeAddFlags) error {
	if err := api.ValidateName(name); err != nil {
		return err
	}
	tag, err := releaseTag("", Version)
	if err != nil {
		return err
	}
	if f.binary != "" {
		if _, err := os.Stat(f.binary); err != nil {
			return api.Errorf(api.CodeInvalid, "cannot read binary %s: %v", f.binary, err)
		}
	}
	cl, err := c.api()
	if err != nil {
		return err
	}
	resp, err := cl.NodeAdd(ctx, api.NodeAddRequest{Name: name, SSH: target, Address: f.address, PrimaryAddress: f.primary, Rejoin: f.rejoin})
	if err != nil {
		return err
	}
	res := nodeAddResult{NodeAddResponse: *resp}
	if resp.Join == "" {
		return c.result(res, func() {
			c.ui.success("Node %s updated", c.ui.bold(name))
			c.ui.note("It already joined. To join a reinstalled machine, run this again with --rejoin.")
		})
	}

	if target == "" {
		// As root, install.sh puts the binary in /usr/local/bin.
		install := "curl -fsSL https://raw.githubusercontent.com/egoist/saba/main/install.sh | sudo sh"
		if tag != "" {
			install = "curl -fsSL https://raw.githubusercontent.com/egoist/saba/main/install.sh | sudo SABA_VERSION=" + tag + " sh"
		}
		res.Instructions = []string{install, "sudo saba agent install --join " + resp.Join}
		return c.result(res, func() {
			c.ui.success("Node %s is waiting to join", c.ui.bold(name))
			c.ui.printf("\nOn the node, run:\n\n")
			for _, line := range res.Instructions {
				c.ui.printf("  %s\n", line)
			}
			c.ui.printf("\n")
			c.ui.note("The join code works once and expires %s. Check progress with `saba node list`.", resp.JoinExpires.Local().Format("Jan 2 15:04"))
		})
	}

	host := c.newRemote(ctx, target, "node")
	if err := host.probe(); err != nil {
		return err
	}
	if err := host.ensureDocker(f.installDocker); err != nil {
		return err
	}
	if err := host.installBinary(f.binary, tag); err != nil {
		return err
	}
	host.step("Installing the agent")
	if _, err := host.run(host.sudo+shellwords.Join([]string{"/usr/local/bin/saba", "agent", "install", "--join", resp.Join}), nil, true); err != nil {
		return api.Errorf(api.CodeRuntime, "installing the agent failed: %v", err).
			WithHint("Inspect it on the node: `journalctl -u saba-agent -n 50`.")
	}
	host.step("Waiting for the agent to connect")
	node, err := waitForNode(ctx, cl, name, 45*time.Second)
	if err != nil {
		return api.Errorf(api.CodeUnreachable, "the agent on %s did not connect: %v", target, err).
			WithHint("On the node, `journalctl -u saba-agent -n 50` says why. It must reach the primary at %s.", primaryOf(resp.Join))
	}
	res.Node = *node
	res.Join = "" // spent
	return c.result(res, func() {
		c.ui.success("Node %s joined (saba %s, Docker %s)", c.ui.bold(name), node.Version, node.Docker)
		c.ui.note("Run replicas on it with `saba deploy -a <app> --node local --node %s --replicas 2`.", name)
	})
}

func primaryOf(join string) string {
	if j, err := cluster.ParseJoin(join); err == nil {
		return j.Primary
	}
	return "its configured address"
}

func waitForNode(ctx context.Context, cl *client.Client, name string, timeout time.Duration) (*api.Node, error) {
	deadline := time.Now().Add(timeout)
	last := "not connected yet"
	for time.Now().Before(deadline) {
		nodes, err := cl.Nodes(ctx)
		if err != nil {
			return nil, err
		}
		for i := range nodes {
			if nodes[i].Name == name {
				if nodes[i].Status == "connected" {
					return &nodes[i], nil
				}
				if nodes[i].Error != "" {
					last = nodes[i].Error
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("%s", last)
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
			status := c.ui.red(n.Status)
			switch n.Status {
			case "connected":
				status = c.ui.green(n.Status)
			case "pending":
				status = c.ui.yellow(n.Status)
			}
			load, mem, disk := "-", "-", "-"
			if s := n.Stats; s != nil {
				load = strconv.FormatFloat(s.Load1, 'f', 2, 64) + "/" + strconv.Itoa(s.CPUs)
				if s.MemTotal > 0 {
					mem = fmt.Sprintf("%d%% of %s", s.MemUsed*100/s.MemTotal, humanBytes(int64(s.MemTotal)))
				}
				if s.DiskTotal > 0 {
					disk = fmt.Sprintf("%d%% of %s", s.DiskUsed*100/s.DiskTotal, humanBytes(int64(s.DiskTotal)))
				}
			}
			rows = append(rows, []string{n.Name, status, orDash(n.Address), orDash(n.Version), orDash(n.Docker), load, mem, disk, strconv.Itoa(n.Replicas)})
		}
		c.ui.table([]string{"NAME", "STATUS", "ADDRESS", "SABA", "DOCKER", "LOAD", "MEMORY", "DISK", "REPLICAS"}, rows)
		for _, n := range nodes {
			switch {
			case n.Status == "pending":
				c.ui.printf("%s %s has not joined yet; `saba node add %s --rejoin` prints a new join command.\n", c.ui.yellow("!"), n.Name, n.Name)
			case n.Error != "":
				c.ui.printf("%s %s: %s\n", c.ui.red("!"), n.Name, n.Error)
			}
		}
	})
}
