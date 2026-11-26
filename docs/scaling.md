# Scaling

saba scales an app in two directions: more replicas on one server, and replicas spread across
several servers. Both use the same rolling deploys, health checks and load balancing.

## Replicas on one server

```sh
saba scale 3                   # or: replicas = 3 in saba.toml, or --replicas 3
```

Each replica is its own container. The proxy sends requests to them in turn. If a replica
refuses a connection, the request is retried on the next one and the failing replica is skipped
for two seconds, so a crashed replica costs no failed requests while Docker restarts it.

Scaling is a rollout like any other: a full new set of replicas starts, passes its health checks
and takes over. Every replica gets `SABA_REPLICA` (1, 2, 3, ...) in its environment, which helps
when exactly one of them should run a background job.

Replicas on one server share its CPU and memory. Set `[resources]` limits so one app cannot
starve the others.

## Adding servers

A saba cluster has one **primary**, the server running the saba daemon, and any number of
**worker nodes**. The primary terminates all traffic, keeps all state and decides what runs
where. Each node runs the **saba agent**, which connects out to the primary and carries
everything over that one connection: Docker control and the requests to the node's replicas.

A node needs:

- Linux with Docker,
- to be able to reach the primary on its HTTPS port (443), or on the cluster port (7443) when
  the primary runs with `--tls off`.

The node needs no open ports of its own and may sit behind NAT. Only the agent's outbound
connection is required.

### With SSH access to the node

```sh
saba node add worker1 root@203.0.113.20
```

```text
▶ Connecting to root@203.0.113.20
▶ Downloading saba v0.2.0 (linux/amd64) from GitHub
▶ Installing the agent
▶ Waiting for the agent to connect
✓ Node worker1 joined (saba v0.2.0, Docker 29.1.0)
```

saba connects over SSH as `server setup` does: it checks the machine (root or passwordless
`sudo`), installs Docker if missing and you passed `--install-docker`, installs the same saba
version as the primary, and installs the agent as the `saba-agent` systemd service. SSH is only
used for the install; afterwards the agent's own connection is all the cluster uses.

### Without SSH access

For a machine you cannot reach from here, such as a home server behind NAT, leave out the
destination:

```sh
saba node add homelab
```

```text
✓ Node homelab is waiting to join

On the node, run:

  curl -fsSL https://raw.githubusercontent.com/egoist/saba/main/install.sh | sudo SABA_VERSION=v0.2.0 sh
  sudo saba agent install --join saba-join-eyJu...
```

The join code works once and expires after 24 hours. Running `saba node add homelab` again
while the node has not joined yet replaces the code.

### How a node joins

- The join code contains the primary's address, the fingerprint of the primary's key, the node's
  name and a one-time token. The primary stores only a hash of the token.
- On its first connection the agent presents the token together with a key pair it generated
  for itself. The primary pins that key, and from then on accepts the node only with it. The
  agent likewise only talks to a primary with the pinned key.
- The agent connects to `--primary-address` when given, otherwise to the primary's
  [`address` setting](servers.md#server-settings), otherwise to its public IP. Give a private
  address when the servers share a network, so traffic stays on it:

  ```sh
  saba node add worker1 root@10.0.0.2 --primary-address 10.0.0.1
  ```

- After a reinstall, the node has a new key and is refused. Issue a new code with
  `saba node add worker1 --rejoin` (plus the SSH destination to reinstall automatically).

```sh
saba node list
```

```text
NAME     STATUS     ADDRESS   SABA    DOCKER  LOAD    MEMORY           DISK             REPLICAS
local    connected  10.0.0.1  v0.2.0  29.1.0  0.42/4  31% of 7.8 GB    22% of 160.0 GB  3
worker1  connected  10.0.0.2  v0.2.0  29.1.0  1.10/4  58% of 7.8 GB    40% of 160.0 GB  2
homelab  pending    -         -       -       -       -                -                0
```

Agents report their load, memory and disk every ten seconds.

### Versions

Agents run the same version as the primary. When an agent connects to a primary running another
release, it downloads that release, verifies its checksum, replaces itself and restarts, so
upgrading the primary with `saba server setup` upgrades every node too. `saba doctor` reports a
node running another version. Development builds are never replaced automatically.

## Placing replicas

Choose the servers with `nodes`:

```toml
nodes = ["local", "worker1"]    # the primary and worker1
replicas = 4                    # 2 on each
```

```sh
saba deploy --node local --node worker1 --replicas 4      # the same with flags
saba deploy --node '*' --replicas 6                       # every server
saba deploy --node worker1                                # move the app to worker1 entirely
```

- Replicas are assigned round-robin in the order listed: replica 1 to the first server,
  replica 2 to the second, and so on.
- With fewer replicas than servers, the first ones are used.
- `"*"` means the primary followed by every node, by name. A node added later is used from the
  app's next deploy.
- Without `nodes`, an app runs on the primary only.

`saba status` shows each container's node, and every replica gets `SABA_NODE`.

### Images

Images are built or pulled on the primary, then streamed to each node that needs them, straight
from Docker to Docker over the agent's connection. Nothing is sent when the node already has the
image, which also makes restarts and rollbacks to a version the node has seen cheap. No registry
is involved.

## Traffic between servers

Replicas on a node publish their ports on the node's loopback interface. The primary reaches
them through the agent: each request opens a stream on the agent's connection, and the agent
connects it to the replica's port. The proxy treats remote replicas exactly like local ones,
including failover and draining.

That connection is TLS 1.3 between pinned keys. Nothing is exposed on the node, and no private
network or firewall rule is needed besides the node reaching the primary. On loopback, a request
through an agent costs about 14 µs more than a direct one. Between real servers, the network
latency between them matters far more.

## Talking between apps on different servers

Apps find each other by name (`http://api:8080`, `main:5432`) through Docker's DNS, which only
spans **one server**. Across servers:

- **Anything behind the proxy** is reachable at its public URL, from anywhere.
- **An app that publishes a port** is also reachable **by name** from other servers. Give each
  server its address on the shared network, and publish the port on that address:

  ```sh
  saba server config set address 10.0.0.1                     # the primary
  saba node add worker1 root@10.0.0.2 --address 10.0.0.2      # each node
  saba db create main --engine postgres --publish 10.0.0.1:5432:5432
  saba db publish main 10.0.0.1:5432:5432          # for a database that already exists
  ```

  Containers on `worker1` get a hosts entry mapping `main` to `10.0.0.1`, so `DATABASE_URL` works
  unchanged. Entries are written when a container starts: apps on other servers see a newly
  published app after their next deploy or restart.

## When a node fails

- The primary notices within seconds, when the agent's connection drops. New requests go to
  replicas elsewhere. Requests that were in flight on the node at that moment fail.
- The app shows `degraded`; `saba node list` and `saba doctor` show the node as `unreachable`
  with the error.
- The agent keeps trying to reconnect. When it is back, the primary checks the node's
  containers (starting live ones that should run, removing leftovers) and its replicas return to
  rotation without a deploy.
- When the primary restarts, agents reconnect within seconds by themselves.
- Deploys that place replicas on an unreachable node fail before switching traffic, so the
  current version keeps serving. Deploy with a placement that leaves the node out if it stays
  down.

## Removing a node

Move apps off it first, then remove it:

```sh
saba deploy -a web --node local
saba node remove worker1 --yes
```

Removal refuses while an app is placed on the node or has replicas there. It disconnects the
node and forgets its key, so its agent is refused from then on. When the agent was installed over
SSH, it is uninstalled the same way (`--keep-agent` skips that); otherwise run
`sudo saba agent uninstall` on the node. Containers, images and volumes on the node are not
touched.

## Limitations

- **The primary is a single point of failure.** It terminates all traffic and holds all state.
  Nodes add capacity, not redundancy for the primary. Run a second, independent server (another
  context) if you need a warm standby, and keep backups in S3.
- **Volumes belong to one server.** Apps with volumes, and databases, run on a single node.
- **One-off containers run on the primary.** Release commands, cron jobs and `saba run` start on
  the primary, with the primary's copy of any named volume. For an app pinned to another node
  with volumes, those see a different, empty volume; avoid giving such apps release commands or
  cron jobs that need the volume.
- **Name resolution across servers** works only for apps that publish ports, as described above.
