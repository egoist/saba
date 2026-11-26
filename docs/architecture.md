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
| `internal/cluster` | The agent, and the primary's side of agent connections |
| `internal/certs` | DNS providers for DNS-01 (libdns) and the store of uploaded certificates |
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
6. **Start replicas.** Each gets a free port in 20000–29999, published on its server's loopback
   interface. Replicas start with restart policy
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

Certificates come from two places: uploaded certificates in saba's own store, and
[CertMagic](https://github.com/caddyserver/certmagic), which obtains, stores and renews ACME
certificates (in `certs/acme`). The TLS handshake consults the store first for routed hostnames,
then CertMagic's cache; ACME TLS-ALPN-01 validation handshakes always go to CertMagic, and the
HTTP listener hands `/.well-known/acme-challenge/` to it before redirecting to HTTPS.

CertMagic runs with two configurations sharing one cache:

- **On demand**: a certificate for a hostname is obtained during its first handshake, through
  TLS-ALPN-01 or HTTP-01, answered by the proxy's own listeners. A decision function allows only
  hostnames routed to an app, and limits subdomains matched only by a wildcard route to 10 new
  certificates per hour per route.
- **Wildcards**: DNS-01 only, with the configured [libdns](https://github.com/libdns) provider.
  saba asks CertMagic to manage the wanted wildcard names (the base domain and wildcard routes),
  recomputed whenever the routing table's hostnames change and once a minute. CertMagic obtains
  them in the background, retries failures with backoff and renews them.

A cached wildcard certificate covers its subdomains without asking the decision function, so
on-demand issuance under a wildcard route only happens while no wildcard certificate exists.
Changing the DNS provider builds a new wildcard configuration and cancels the old one's retries.

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
- makes sure the wanted wildcard certificates are managed (renewal itself runs inside
  CertMagic),
- once an hour, uploads backups that exist only on the server to S3.

## The cluster

Every node runs `saba agent`, the same binary. The agent dials the primary: TLS 1.3 with the
ALPN protocol `saba-cluster/1` on the primary's HTTPS port, so it shares port 443 with regular
traffic. The primary's TLS listener hands those handshakes to the cluster manager instead of the
HTTP server. With `--tls off` it uses a separate listener, `--cluster-addr` (default `:7443`).

- **Identity.** Both sides have an ed25519 key with a self-signed certificate and recognize each
  other by the key's fingerprint, not by a certificate authority. The join code carries the
  primary's fingerprint and a one-time token; the primary pins the agent's key on first contact.
- **Multiplexing.** After the handshake the connection runs yamux. The primary opens one stream
  per Docker API connection and one per proxied request or health check. The agent connects
  each stream to the node's Docker socket or to `127.0.0.1:<port>`, and refuses anything else.
- **Docker.** For each node the primary exposes `/run/saba/nodes/<node>.sock`. The engine drives
  the node with the regular Docker CLI pointed there, so every runtime operation (run, logs,
  exec, stats, `save | load` for images) works the same locally and remotely.
- **Replica traffic.** Remote replicas are addressed as `<node>.saba-node:<port>`. The proxy's
  dialer recognizes that suffix and opens a stream through the node's agent. A failure is a
  regular dial error, so the proxy fails over as it does for local replicas.
- **Liveness.** yamux keep-alives detect a dead connection within seconds. The agent reconnects
  with backoff; on every connect the primary reconciles the node's containers, skipping apps
  that have an operation in progress.
- **Heartbeats.** The agent sends its Docker version, load, memory and disk usage every ten
  seconds on a stream of its own.
- **Upgrades.** The primary tells the agent its version in the handshake. An agent on another
  release downloads it, verifies the checksum, replaces itself and re-executes.

The primary never needs to reach the node. That is what lets nodes sit behind NAT, and it means
the primary holds no SSH key or other credential for its nodes.

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
