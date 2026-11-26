# Architecture

How saba works inside: useful for understanding its guarantees, debugging something unusual,
or contributing.

## Overview

```text
 your machine                                  the primary server
┌──────────────┐                              ┌───────────────────────────────────────────────────┐
│  saba CLI    │── ssh host saba dial-stdio ─►│ saba daemon                                       │
│  (or MCP)    │   HTTP over SSH's stdio      │  ├─ control API     HTTP on /run/saba/saba.sock   │
└──────────────┘                              │  ├─ deploy engine   builds, rollouts, rollbacks   │
                                              │  ├─ proxy           :80/:443, TLS, routing        │
  internet ───────── :80 / :443 ─────────────►│  ├─ scheduler       cron, backups, certificates   │
                                              │  ├─ cluster         one SSH connection per node   │
                                              │  └─ state.json                                    │
                                              │          │ docker CLI                             │
                                              │   ┌──────▼─────┐ ┌────────────┐ ┌────────────┐    │
                                              │   │ web-v7-1   │ │ web-v7-2   │ │ main-v1-1  │    │
                                              │   └────────────┘ └────────────┘ └────────────┘    │
                                              └──────────────────────────┬────────────────────────┘
                                                                         │ SSH: Docker API + replica ports
                                                                ┌────────▼──────────┐
                                                                │ worker node       │
                                                                │  web-v7-3, ...    │
                                                                └───────────────────┘
```

The daemon is a single Go process. The code is organized as:

| Package | Role |
|---|---|
| `cmd/saba` | Entry point |
| `internal/cli` | Commands, output, `saba.toml`, the generated reference |
| `internal/client` | Talks to a daemon over SSH or a unix socket; contexts |
| `internal/daemon` | Wires everything together; control API handlers; doctor |
| `internal/engine` | Deployments, health checks, rollbacks, operations, backups, placement |
| `internal/proxy` | Reverse proxy, routing, draining, TLS listeners |
| `internal/runtime` | The container runtime interface and its Docker implementation |
| `internal/store` | The state file |
| `internal/buildpack` | Stack detection and generated Dockerfiles |
| `internal/cluster` | SSH connections to nodes |
| `internal/certs` | Wildcard (DNS-01) issuance and the certificate store |
| `internal/s3` | A small S3 client for backup mirroring |
| `internal/cron` | Cron expression parsing |
| `internal/mcp` | The MCP server |

## The control API

The daemon serves a JSON HTTP API (`/v1/...`) on its unix socket. Long operations (deploys,
logs, exec) answer with a stream of NDJSON events. The socket is the only way in: there is no
TCP listener for the API and no authentication beyond the socket's file permissions (root and
the `docker` group).

Remote access is SSH running `saba dial-stdio` on the server, which pipes stdin and stdout to
the socket, the same trick Docker uses for `ssh://` hosts. The CLI is the supported interface;
the HTTP API is internal and may change between versions.

## State

Everything the daemon knows is one JSON document, `/var/lib/saba/state.json`: server settings,
apps with their configuration, environment and deployment history, nodes. It is small, so every
change rewrites it whole: the new state is built on a copy, written to a temporary file, synced
and atomically renamed. An update either fully commits or leaves the file and the in-memory state
untouched; a crash mid-write cannot corrupt it.

Deployment logs are separate append-only NDJSON files, one per deployment.

## A deployment, step by step

1. **Validate and record.** The request is merged into the app's configuration on a copy, then
   validated: config keys, domains, conflicts with other apps, references in the environment,
   placement. A deployment record with a new version number is stored together with the exact
   configuration and environment it will use. The app's saved configuration is **not** changed
   yet. An app runs one operation at a time; a second one is rejected.
2. **Image.** Pull and pin (`saba/<app>:v<N>`), clone and build, or build the upload. Builds use
   the project's Dockerfile, or a generated one written outside the build context.
3. **Port.** Configured, or the image's first exposed TCP port, or none (worker).
4. **Release command**, in a one-off container with the new image.
5. **Ship** the image to nodes that will run replicas and do not have it.
6. **Start replicas.** Each gets a free port in 20000–29999, published on the server's loopback
   interface (or a node's private address in direct mode). Replicas start with restart policy
   `no`, so a container that crashes on boot stays down with its real exit code.
7. **Health checks**, in parallel for all replicas. On failure: capture the container's last log
   lines, remove the new containers and the image, record the error. Done; nothing else changed.
8. **Switch.** Healthy replicas get the restart policy `unless-stopped`. The proxy's routing
   entry for the app is replaced atomically.
9. **Drain** the old version's in-flight requests (up to `drain_timeout`) and close its idle
   upstream connections.
10. **Commit.** The deployment is marked live, and its configuration and environment become the
    app's saved ones. The old version's containers are stopped and removed.
11. **Prune** images and history beyond the retention window.

With the `recreate` strategy, steps 6–9 become: pause the app's routing entry (incoming requests
wait), drain, stop the old containers, start and check the new ones, switch, resume. If the new
version fails, the old containers are started again and checked before requests are released.

Every step emits events into the deployment's event log, which streams to any client following
it and is persisted for later. The deployment runs in the daemon, detached from the request that
started it, so clients can disconnect.

## The proxy

The proxy keeps a routing table from hostname to app. Each app points at a **pool**: the
addresses of one version's replicas, plus a counter of in-flight requests.

- **Atomic switch.** A deploy builds a new pool and swaps it in with one pointer exchange.
  Requests that started before the swap finish on the old pool; new ones use the new pool.
- **No lost requests at the switch.** A request increments its pool's counter, then checks that
  the pool is still current, retrying if not. So a deploy that sees the old pool's counter at
  zero can stop the old containers knowing no request is about to use them.
- **Draining** waits for the old pool's counter to reach zero, or the drain timeout.
  Upgraded connections (WebSockets) count as in flight until closed.
- **Holding** (recreate deploys): a per-app gate makes new requests wait, up to 60 seconds, and
  releases them all once the new version is routed.
- **Load balancing** is round-robin. A replica that refuses a connection is marked down for two
  seconds and the request is retried on the next replica; requests that already reached a
  replica are not retried, since they may not be idempotent.
- **Connections to replicas** are kept alive per pool; retiring a pool closes them, so old
  containers can exit promptly instead of waiting for idle keep-alive connections.
- **Headers.** `X-Forwarded-For`, `-Host` and `-Proto` are set by the proxy; client-supplied
  values are discarded. Responses are streamed without buffering.

### TLS

HTTPS uses three sources, in order: certificates in saba's store (uploaded, and wildcards issued
through DNS-01), then certificates obtained on demand from Let's Encrypt through `autocert`. The
on-demand path only accepts hostnames currently routed to an app; under a wildcard route that is
every subdomain, which is why wildcard routes should have a DNS provider. ACME TLS-ALPN-01 validation handshakes always reach `autocert`.

### Listeners and restarts

Under systemd, ports 80 and 443 belong to `saba.socket` and are passed to the daemon (socket
activation). When the daemon stops, it stops accepting, lets in-flight requests finish (up to 30
seconds) and exits; the kernel keeps queueing new connections on the sockets, and the next daemon
process picks them up. Clients see a short delay, not an error.

## Startup and recovery

On start the daemon:

1. opens the state file and checks that Docker answers,
2. reconnects to every node and restores the SSH port forwards of remote replicas,
3. marks deployments that were in progress as failed (they were interrupted),
4. removes containers of versions that are not live (leftovers of interrupted deployments,
   one-off containers), but never containers of apps it does not know,
5. starts live containers that should be running but are not,
6. rebuilds the routing table,
7. starts the scheduler.

App containers do not depend on the daemon: Docker's restart policy keeps them running through
daemon restarts and server reboots.

## The scheduler

Once a minute, at the top of the minute (UTC), the daemon:

- starts due cron jobs (skipping a job whose previous run is still going),
- starts due database backups,
- checks whether wildcard certificates need issuing or renewing,
- once an hour, uploads backups that exist only on the server to S3.

## The cluster

For each node, the primary keeps one OpenSSH master connection (`ssh -M`), authenticated with
the primary's own ed25519 key and pinned host keys. Through it:

- the node's Docker socket is forwarded to `/run/saba/nodes/<node>.sock`, and the engine drives
  the node with the regular Docker CLI pointed there,
- each remote replica's port is forwarded to the same port on the primary's loopback, so the
  proxy reaches it like a local replica (unless the node is in direct mode),
- images are streamed with `docker save | docker load`, skipped when the node has the image.

A watcher checks each connection every 10 seconds and reconnects, restoring forwards, when one
dropped. Keep-alives detect a dead node within about six seconds.

## The Docker driver

saba drives Docker through the `docker` CLI rather than the API library. That keeps it
compatible with any Docker version and gives BuildKit's build output for free. Notable details:

- environment values are passed through the CLI's process environment (`-e NAME`), or a private
  temporary env file for names the docker CLI itself reads (`PATH`, `HTTPS_PROXY`, ...), never
  as arguments, so secrets do not appear in the process list,
- containers run with `--init`, so signals are delivered and zombies reaped,
- log files are capped at three 20 MB files per container when the logging driver allows it,
- containers carry `saba.*` labels; the daemon finds its containers by label, never by name
  pattern.
