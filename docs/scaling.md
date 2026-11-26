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
**worker nodes**. The primary terminates all traffic, keeps all state and drives every node's
Docker engine. Nodes run replicas.

A node needs:

- Linux with Docker,
- SSH access from the primary as a user that can use Docker (root, or a member of the `docker`
  group),
- SSH forwarding enabled in its `sshd` (`AllowTcpForwarding` and `AllowStreamLocalForwarding`,
  on by default in most distributions).

Nothing else: no saba install on the node and no open ports besides SSH.

```sh
saba node add worker1 root@10.0.0.2
```

```text
✓ Node worker1 added (Docker 29.1.0, traffic goes through the SSH tunnel)
```

What this does:

1. **From your machine**, saba logs in to the node over SSH, adds the primary's public key to the
   node's `~/.ssh/authorized_keys` and reads the node's SSH host keys.
2. **From the primary**, saba connects to the node with that key, verifying the host keys it
   read, and checks that Docker works there.

If your machine cannot reach the node, for example because it only has a private address the
primary can reach, add the primary's key to the node's `~/.ssh/authorized_keys` by another route
(the cloud console, or through the primary as a jump host) and skip step 1:

```sh
saba node key | ssh -J root@203.0.113.10 root@10.0.0.2 'cat >> ~/.ssh/authorized_keys'
saba node add worker1 root@10.0.0.2 --no-authorize
```

Without step 1 the primary trusts the node's host key the first time it connects.

```sh
saba node list
```

```text
NAME     STATUS     SSH            ADDRESS   TRAFFIC     DOCKER  REPLICAS
local    connected  (primary)      -         -           29.1.0  3
worker1  connected  root@10.0.0.2  10.0.0.2  ssh tunnel  29.1.0  2
```

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
from Docker to Docker over the SSH connection. Nothing is sent when the node already has the
image, which also makes restarts and rollbacks to a version the node has seen cheap. No registry
is involved.

## Traffic between servers

### Through the SSH tunnel (default)

Each replica on a node publishes its port on the node's loopback interface, and the primary's
SSH connection forwards a matching local port to it. The proxy treats remote replicas exactly
like local ones. Traffic between servers is encrypted, nothing is exposed on the node, and no
private network or firewall rule is needed.

The cost is throughput and latency: every request to a remote replica passes through SSH on
both ends.

### Directly over a private network

When the servers share a private network (a cloud VPC, WireGuard, Tailscale), let the primary
connect to the node's replicas directly:

```sh
saba node add worker1 root@10.0.0.2 --address 10.0.0.2 --direct
```

Replicas on `worker1` then publish their ports on `10.0.0.2`, and the proxy connects there.
Docker is still driven over SSH. This is faster, and it is **unencrypted**: replica ports are
open on that address to anything on the network. saba therefore only accepts private addresses
(10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10) for direct mode.

Run `node add` again with or without `--direct` to change a node. Apps pick up the change on their
next deploy or `saba restart`. If replicas fail their health check right after enabling direct
mode, the error says so: the primary cannot reach the node's address on the replica ports, which
usually means a firewall.

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

- The primary notices within seconds. Connections to that node's replicas fail, and the proxy
  retries those requests on replicas elsewhere. Requests that were in flight on the node at that
  moment fail.
- The app shows `degraded`; `saba node list` and `saba doctor` show the node as `unreachable`
  with the error.
- The primary keeps trying to reconnect. When the node is back, its tunnels are restored and its
  replicas, restarted by Docker, return to rotation without a deploy.
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
node and forgets its keys; containers and images on the node are not touched.

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
