<img src="assets/icon.svg" width="64" height="64" alt="">

# saba

Deploy and run apps on your own servers, from the command line.

saba is a CLI-first alternative to Dokploy, Coolify and Railway: one Go binary that turns a
Linux VPS with Docker into an app platform with zero-downtime deploys, automatic HTTPS,
rollbacks, managed databases and cron — and that an AI agent can operate as easily as you can.

```console
$ saba server setup root@203.0.113.10
✓ saba is running on root@203.0.113.10
  Apps will be served at https://<app>.203-0-113-10.sslip.io

$ cd my-app && saba deploy
▶ Deploying my-app → root@203.0.113.10
  upload   Uploading source (42 files, 310.4 KB)
  build    Building image (node)
  start    Starting 1 replica(s) of v1
  health   Waiting for v1 to become healthy (HTTP on port 3000, timeout 1m0s)
  switch   Routing traffic to v1
✓ my-app v1 is live (38.2s)
  https://my-app.203-0-113-10.sslip.io
```

## Why

- **No dashboard to host, no agent to expose.** The server runs one daemon with no network
  API; the CLI reaches it through SSH. If `ssh host` works, saba works.
- **Zero-downtime by construction.** saba ships its own reverse proxy, so it knows exactly
  when a version is healthy, switches traffic atomically and drains in-flight requests
  (WebSockets included) before stopping the old version.
- **Failed deploys change nothing.** A new version replaces the live one only after it passes
  its health check. Otherwise the old version keeps serving, saved config is untouched, and the
  error tells you why — with the container's last log lines attached.
- **Built to be driven by agents and scripts.** `--json` everywhere with one document per
  command, stable error codes and exit codes, hints that say what to do next, no prompts, a
  built-in reference (`saba guide`) and an MCP server (`saba mcp`).

## Features

| | |
|---|---|
| Deploy sources | A directory (uploaded and built on the server), a git repository, or a registry image |
| Builds | Your `Dockerfile`, or a generated build for Node.js, Bun, Go, Python, Rust and static sites |
| Rollouts | Rolling (default) or recreate; health-gated; release commands for migrations |
| Rollbacks | `saba rollback` restores image, config and environment of a previous version |
| HTTPS | Automatic Let's Encrypt certificates for every routed domain; instant `sslip.io` URLs |
| Wildcards | Wildcard certificates through DNS-01 (Cloudflare built in, a hook for any other DNS host); bring your own certificates |
| Scaling | `saba scale web 3` — replicas are load-balanced by the built-in proxy, on one server or spread across several |
| Config | Declarative `saba.toml`, or flags that the server remembers |
| Secrets | `saba env set`, with `${{db.URL}}`-style references between apps |
| Databases | Postgres, MySQL, MariaDB, Redis, MongoDB with volumes and generated credentials |
| Backups | On-demand and scheduled database backups with retention, download and restore |
| Cron | Scheduled commands in the app's image |
| Operations | Logs, exec, one-off runs, stats, `saba doctor` diagnostics |
| Upgrades | The daemon restarts without refusing or dropping requests (systemd socket activation) |
| Multi-server | Add worker nodes over SSH; named contexts for separate environments |

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/egoist/saba/main/install.sh | sh
```

This installs the CLI for macOS or Linux from the latest
[GitHub release](https://github.com/egoist/saba/releases). With Go installed,
`go install github.com/egoist/saba/cmd/saba@latest` works too.

## Quick start

### 1. Set up a server

Any Linux server with systemd, ports 80/443 free, and SSH access as root (or a user with
passwordless sudo):

```sh
saba server setup root@203.0.113.10 --install-docker --email you@example.com
```

This checks for Docker (installing it if asked), downloads saba from GitHub releases onto the
server, installs the daemon as a systemd service and saves the server as a context on your
machine. The server fetches the build for its own architecture, in the same version as your
CLI, so it works the same from a Mac or a Linux box. Run it again any time to upgrade.

### 2. Deploy

```sh
saba deploy                                   # the current directory
saba deploy -a blog --image ghost:5 --port 2368
saba deploy -a api --git https://github.com/me/api --ref main
```

Optionally pin settings in a `saba.toml` (`saba init` writes a commented one):

```toml
name = "api"
port = 8080
domains = ["api.example.com"]
replicas = 2
release_command = "npm run migrate"

[health]
path = "/healthz"

[[cron]]
name = "cleanup"
schedule = "0 3 * * *"
command = "node scripts/cleanup.js"
```

### 3. Operate

```sh
saba status                       # what is running
saba logs -f                      # tail logs
saba env set STRIPE_KEY=sk_...    # set a secret and roll it out
saba db create main --engine postgres && saba db link main -a api
saba db backup schedule main @daily   # automatic backups, newest 7 kept
saba scale 3
saba rollback                     # something went wrong
saba doctor                       # why is something wrong
```

Run `saba --help`, `saba <command> --help` or `saba guide` for everything else.

## Scaling across servers

One server is the primary: it runs the saba daemon, terminates HTTPS and load-balances. Any
other Linux box with Docker becomes a worker node — nothing to install on it, no ports to open:

```sh
saba node add worker1 root@10.0.0.2
saba deploy -a web --node local --node worker1 --replicas 4    # 2 replicas per server
```

or in `saba.toml`: `nodes = ["local", "worker1"]` (`["*"]` means every server).

The primary drives each node's Docker engine and reaches its replicas through one persistent
SSH connection, so traffic between servers is encrypted and needs no private network. Rolling
deploys, rollbacks, logs, exec and stats work the same across servers. If a node goes down, its
replicas drop out of rotation and come back on their own when it returns.

Two things stay per-server: volumes (apps with volumes and databases run on one node), and
container names — `http://api:8080` resolves between containers on the same server. An app that
publishes a port (`publish = ["10.0.0.1:5432:5432"]`) is also reachable by name from other
nodes, at its server's address (`saba node add --address`, `saba server config set address`).

## Certificates

Every routed domain gets a Let's Encrypt certificate on its first request, with nothing to
configure. Wildcard certificates need the DNS-01 challenge, so saba needs access to your DNS:

```sh
saba server config set cloudflare_api_token -      # reads the token from stdin
saba server config set base_domain apps.example.com
```

saba then obtains and renews `*.apps.example.com` (new apps get HTTPS instantly, with no
per-app issuance) and a certificate for every wildcard domain routed to an app. For other DNS
hosts, point `dns_hook` at a script that creates and removes the TXT record. To use a
certificate from elsewhere: `saba certs add --cert fullchain.pem --key privkey.pem`.

## For AI agents

```sh
saba guide            # the complete reference in one compact document
saba status --json    # one JSON document on stdout
```

- Success: exactly one JSON document on stdout. Failure: empty stdout and
  `{"error": {"code", "message", "hint", "details"}}` on stderr.
- Exit codes: `0` ok · `1` error · `2` invalid input · `3` not found · `4` conflict ·
  `5` deployment failed · `6` server unreachable.
- Nothing prompts and nothing hangs by default; destructive commands require `--yes`.
- `saba deploy --dry-run` validates config and prints the plan without changing anything.

An [MCP](https://modelcontextprotocol.io) server exposes the same operations as tools:

```json
{ "mcpServers": { "saba": { "command": "saba", "args": ["mcp", "--context", "prod"] } } }
```

## How it works

```
 your machine                          your server
┌────────────┐   ssh (stdio tunnel)   ┌──────────────────────────────────────────────┐
│  saba CLI  │ ─────────────────────► │ saba daemon                                  │
└────────────┘                        │  ├─ control API   unix socket, no TCP port   │
                                      │  ├─ deploy engine build → release → start →  │
   internet ──── :80 / :443 ────────► │  │                health → switch → drain    │
                                      │  ├─ reverse proxy TLS, routing, load balance │
                                      │  └─ cron                                     │
                                      │        │ docker                              │
                                      │   ┌────▼─────┐ ┌──────────┐ ┌──────────┐     │
                                      │   │ web v7-1 │ │ web v7-2 │ │ postgres │ ... │
                                      │   └──────────┘ └──────────┘ └──────────┘     │
                                      └──────────────────────────────────────────────┘
```

- Every deployment gets a version number and its own containers
  (`saba-<app>-v<N>-<replica>`), published on a loopback port that only the proxy uses.
- State is one JSON file (`/var/lib/saba/state.json`); deployment logs and database backups
  are kept next to it.
- Ports 80 and 443 belong to systemd and are handed to the daemon (socket activation). While
  the daemon restarts, connections wait in the kernel and in-flight requests finish first, so
  upgrading saba does not drop traffic.
- Apps share a private Docker network and reach each other by name (`http://api:8080`).
- Containers are owned by Docker with a restart policy, so apps survive daemon restarts and
  host reboots.

## Development

```sh
make build      # ./dist/saba (needs Go 1.26+)
make test       # unit and integration tests (no Docker needed)
make release    # cross-compiled binaries in ./dist, as attached to releases
```

To put an unreleased build on a server, upload it instead of downloading a release:
`saba server setup root@host --binary ./dist/saba-linux-amd64`.

Try it locally without root (needs Docker):

```sh
saba daemon --data-dir ./.saba --socket ./.saba/saba.sock --http :8080 --tls off --base-domain localhost &
export SABA_HOST=unix://$PWD/.saba/saba.sock
saba deploy -a hello --image nginx:alpine
curl http://hello.localhost:8080
```

## Status and limitations

saba is new. Known limitations:

- The primary is a single point of failure: it terminates all traffic and holds the state.
  Worker nodes add capacity, not redundancy for the primary itself.
- Traffic to replicas on other servers goes through an SSH tunnel. That keeps setup trivial and
  encrypted, at some cost in throughput compared to a private network link.
- Volumes are per-server and containers find each other by name only on the same server (see
  "Scaling across servers").
- Database backups are stored on the server that runs saba. Copy them elsewhere for disaster
  recovery (`saba db backup create <db> -o <file>` from another machine). Redis is not
  supported by the backup commands.
- Anyone who can SSH in as root or as a member of the `docker` group can control saba. There
  are no per-user permissions; access control is SSH.
