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
| Scaling | `saba scale web 3` — replicas are load-balanced by the built-in proxy |
| Config | Declarative `saba.toml`, or flags that the server remembers |
| Secrets | `saba env set`, with `${{db.URL}}`-style references between apps |
| Databases | Postgres, MySQL, MariaDB, Redis, MongoDB with volumes and generated credentials |
| Cron | Scheduled commands in the app's image |
| Operations | Logs, exec, one-off runs, stats, `saba doctor` diagnostics |
| Multi-server | Named contexts; pin a project to a server in `saba.toml` |

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
saba scale 3
saba rollback                     # something went wrong
saba doctor                       # why is something wrong
```

Run `saba --help`, `saba <command> --help` or `saba guide` for everything else.

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
- State is one JSON file (`/var/lib/saba/state.json`); deployment logs are kept next to it.
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

- One server per app; there is no clustering or multi-node scheduling.
- Restarting the daemon (for example when upgrading saba) interrupts the proxy for a moment.
  App containers keep running.
- Certificates use the HTTP-01/TLS-ALPN-01 challenges, so wildcard certificates are not issued;
  each hostname under a wildcard domain gets its own certificate on first use.
- Anyone who can SSH in as root or as a member of the `docker` group can control saba. There
  are no per-user permissions.
- Database backups are manual (`saba exec -a db -- pg_dump ...`).
