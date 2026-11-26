# saba reference

Full documentation: https://github.com/egoist/saba/tree/main/docs

saba deploys apps to Linux servers you own. One binary plays two roles:

- **daemon** (on the server): built-in reverse proxy with automatic HTTPS, zero-downtime
  deploy engine, rollbacks, managed databases, cron. It drives Docker and exposes no network
  port of its own.
- **CLI** (anywhere): talks to the daemon over SSH (`ssh <host> saba dial-stdio`). If
  `ssh <host>` works, saba works.

## Concepts

- **App**: a named service (`web`, `api`). Name rule: lowercase letters, digits, dashes; starts
  with a letter; max 40 chars. Apps reach each other at `http://<app>:<port>` on a private
  network.
- **Version / deployment**: every change (deploy, restart, scale, env change, rollback)
  creates a numbered version `v1`, `v2`, ... Exactly one version is live.
- **Deploys are atomic**: a new version only replaces the live one after it passes its health
  check. If anything fails (build, release command, health check), the live version keeps
  serving and saved config/env are unchanged.
- **Config is remembered**: settings passed once (flags or `saba.toml`) are stored on the
  server. Later deploys only need what changes. To clear a setting, set it to an empty value
  (`--release-command ""`).

## Selecting the server and the app

Server, first match wins:
1. `--host user@host` / `SABA_HOST` (an SSH target, `local`, or `unix:///path/to.sock`)
2. `--context name` / `SABA_CONTEXT`
3. `context = "name"` in `./saba.toml`
4. current context (`saba context use <name>`)
5. a daemon on this machine

App, first match wins: positional `[app]` where shown, `-a/--app`, `SABA_APP`, `name` in
`./saba.toml`. `saba deploy` additionally falls back to the directory name. `saba destroy`
never infers the app.

## Output contract (for scripts and agents)

- Add `--json` (or `SABA_JSON=1`) to any command. On success **stdout is exactly one JSON
  document**; nothing else is printed there.
- On failure stdout is empty and **stderr is** `{"error": {"code", "message", "hint", "details"}}`.
  `hint` says how to fix it. `details.logs` holds the relevant build/container output.
- Streaming exceptions (one JSON object per line, NDJSON): `saba logs --json`, and
  `saba deploy --json --events` (progress events; the last one has `"type":"result"` or
  `"type":"error"`).
- saba never prompts. Destructive commands fail with `confirmation_required` unless `--yes`.
- Commands do not follow/hang by default: `logs` prints and exits unless `-f`; `deploy` returns
  when the version is live or failed (use `-d` to return immediately).

Exit codes: `0` ok · `1` error · `2` usage/invalid input · `3` not found · `4` conflict or
deploy in progress · `5` deployment failed (build, release, health) · `6` cannot reach server.
`exec`/`run` exit with the remote command's code.

Error codes: `invalid_request` `confirmation_required` `not_found` `conflict`
`deploy_in_progress` `build_failed` `release_failed` `health_check_failed` `deploy_failed`
`command_failed` `runtime_error` `server_unreachable` `no_server_configured` `internal_error`.

## Commands

```
saba deploy [path]            Deploy a directory (default .), or --image <ref>, or --git <url> [--ref]
     --port N --domain D... --replicas N -e K=V... --env-file F --command C
     --release-command C --strategy rolling|recreate --health-path /p
     --memory 512m --cpus 0.5 --volume name:/path... --dockerfile F --worker --internal
     --node local|<name>|"*" ...   servers to spread replicas across
     --publish [ip:]host:container ...   raw host ports
     An empty value on a repeatable flag clears the list: --domain= --publish= --volume= --node=
     -m "note" -d/--detach --dry-run -q/--quiet --events
saba cancel [app]             Stop the deployment in progress (current version keeps serving)
saba rollback [app] [--to vN] Redeploy a previous version (image + config + env)
saba deployments [app]        History; ROLLBACK column = restorable
saba init [path]              Write a commented saba.toml

saba apps                     List apps
saba status [app]             Detailed state; without app: all apps
saba logs [app] [-f] [-n 100] [--since 10m] [--replica N] [-t]
saba logs [app] --deployment vN|latest [-f]     Build + rollout log of a deployment
saba stats [app]              CPU / memory per container
saba doctor                   Server + app diagnostics with fixes (exit 1 if a check fails)

saba env [list] [--resolved]  dotenv-formatted output
saba env set K=V... | unset K... | import <file|-> | get K      (all accept --no-deploy)
saba domains [list] | add D... | remove D...
saba scale [app] <n>
saba restart [app]            Fresh containers, zero downtime; applies --no-deploy changes
saba stop [app] | start [app]
saba exec -- <cmd...>         In a running replica (--replica N, -t for an interactive TTY)
saba run -- <cmd...>          In a fresh one-off container of the live image (env + volumes)
saba cron [list] | run <job>
saba db create <name> --engine postgres|mysql|mariadb|redis|mongo [--version V]
saba db list | url <name> | link <name> -a <app> [--as VAR]
saba db publish <db> [spec...] | --clear     Expose a database on a host port (restarts it)
saba db backup create <db> [-o file|dir|-] | list <db> | download <db> [id|latest] [-o ...]
saba db backup delete <db> <id> --yes | schedule <db> [<cron>|off] [--keep N]
saba db backup s3 [show] | set --bucket B [--endpoint URL] [--region R] [--prefix P]
     [--access-key-id K] [--secret-access-key S|-] [--create-bucket] | remove
saba db restore <db> (--backup <id>|latest | <file|->) --yes
saba backup create [--stop] [-o ...] | list | download [id] | delete <id> --yes
            | schedule [<cron>|off] [--keep N] [--stop] | restore (--backup <id> | <file>) --yes
                              Any app (-a): volumes archived, databases dumped
saba destroy <app> --yes [--volumes]

saba context add <name> <user@host> | use <name> | list | remove <name>
saba server setup <user@host> [--name N] [--email E] [--base-domain D] [--install-docker]
                              [--version vX.Y.Z]   Downloads saba from GitHub releases onto the
                              server (same version as this CLI by default). Re-run to upgrade.
saba server info | config | config set <key> <value|-> | prune
     keys: base_domain acme_email keep_versions address
saba node [list] | add <name> [user@host] [--address IP] [--primary-address HOST] [--rejoin]
                  [--install-docker] [--binary FILE]
saba node remove <name> --yes [--keep-agent]
saba agent install --join <code> | run | uninstall       (on a node, as root)
saba certs [list] | add --cert <chain.pem> --key <key.pem> | remove <id> --yes
saba mcp                      MCP server on stdio (same tools as this CLI)
saba guide                    This document
```

## saba.toml

Optional, lives in the project root. Flags override it; it overrides what the server remembers.
Unknown keys are rejected.

```toml
name = "api"                    # app name
context = "prod"                # optional: pin to a server context

image = "ghcr.io/me/api:1.4"    # deploy an image; omit to build this directory
port = 8080                     # container port (also set as $PORT). Default: image EXPOSE
domains = ["api.example.com"]   # default: <name>.<server base_domain>
replicas = 2                    # default 1
nodes = ["local", "worker1"]    # servers to spread replicas across; ["*"] = all. Default: local
command = "node server.js"      # overrides the image CMD
release_command = "npm run migrate"   # one-off container, must exit 0 before the rollout
strategy = "rolling"            # rolling (default) | recreate
worker = false                  # true: no HTTP (no port, routing or HTTP health check)
internal = false                # true: reachable by other apps only
volumes = ["data:/data"]        # named volume, or "/host/path:/container/path"
publish = ["5432:5432"]         # raw host ports (forces recreate, max 1 replica)
drain_timeout = "30s"           # time for in-flight requests on the old version
stop_timeout = "10s"            # SIGTERM → SIGKILL grace period

[build]
dockerfile = "docker/Dockerfile.prod"
target = "runtime"
args = { NODE_ENV = "production" }

[health]
path = "/healthz"               # must return 2xx/3xx. Default: any HTTP response < 500 on /
command = "pg_isready"          # alternative: exec in the container, exit 0 = healthy
timeout = "60s"                 # total time allowed to become healthy
interval = "1s"

[resources]
memory = "512m"
cpus = "1"

[env]                           # non-secret values; secrets go through `saba env set`
NODE_ENV = "production"

[[cron]]
name = "cleanup"
schedule = "0 3 * * *"          # 5-field cron or @hourly/@daily/@weekly/@monthly, UTC
command = "node scripts/cleanup.js"
```

## How a deploy works

1. **Image**: `--image` is pulled; a directory or git repo is built on the server with its
   `Dockerfile`. Without a Dockerfile, a build is generated for Node.js (npm/pnpm/yarn), Bun,
   Go, Python, Rust and static sites (a `Procfile` `web:` line sets the start command).
   Uploads include tracked and untracked-but-not-ignored files (`.gitignore` applies).
2. **Release command** (optional) runs in a one-off container. Non-zero exit aborts.
3. **Start** the new replicas next to the old ones. Env gets `PORT`, `SABA_APP`,
   `SABA_VERSION`, `SABA_REPLICA`.
4. **Health check** each replica. The app must listen on `0.0.0.0:$PORT`, not `127.0.0.1`.
5. **Switch** traffic atomically, **drain** in-flight requests on the old version (WebSockets
   included, up to `drain_timeout`), then stop it.

`recreate` (default for databases; forced by `publish`): incoming requests are held at the
proxy while the old version stops and the new one starts, so clients see a pause, not errors.
If the new version fails, the old one is started again.

The last 5 superseded versions stay restorable (`saba server config set keep_versions N`).

## Environment and references

`saba env set` stores values on the server and rolls out a new version. A value may reference
another app: `${{app.KEY}}` resolves, at deploy time, to that app's output or env variable.
Databases expose `URL`, `HOST`, `PORT`, `USER`, `PASSWORD`, `DATABASE`:

```
saba db create main --engine postgres
saba db link main -a api              # sets DATABASE_URL=${{main.URL}} on api and redeploys it
```

Databases are reachable only from other apps (`main:5432`), keep data in a named volume, and are
deleted with `saba destroy main --yes --volumes` (after unlinking: `saba env unset -a api DATABASE_URL`).

## Backups

Postgres, mysql, mariadb and mongo are dumped with their own tools while running. Every other
app with named volumes (redis too) is backed up as a .tar.gz of its volumes (one top-level
directory per volume), made by a busybox helper container next to the volumes; bind mounts are
not included. `saba backup ... -a <app>` works for every app, `saba db backup <db> ...` is the
same for databases. Backups are kept on the server; ids are UTC timestamps
(`20261126T105124Z-manual`); `latest` works for download and restore (delete needs the id);
`method` in the JSON is `dump` or `volumes`.

```
saba backup create -a uploads --stop            # --stop: app stopped meanwhile, requests wait (SQLite)
saba backup schedule -a uploads @daily --stop   # scheduled, same options
saba backup restore -a uploads --backup latest --yes   # checks the whole archive first, stops the app,
                                                       # empties + unpacks the volumes in it, starts it
```

A volume restore reports `volumes` (replaced) and `skipped` (in the archive, not in the app);
volumes not in the archive are untouched. A failed scheduled backup shows in `saba doctor`
as `backup:<app>`.

```
saba db backup schedule main @daily --keep 14   # cron or @hourly/@daily/@weekly, UTC; "off" disables
saba db backup create main -o ./                # dump now and download it
saba db restore main --backup latest --yes      # or a local file, plain or .gz; "-" reads stdin
```

Scheduled backups are pruned to the newest `--keep` (default 7); manual ones are never pruned.
A Postgres restore is a single transaction: if it fails nothing changes. `saba destroy <app>
--volumes` also deletes its backups on the server; without `--volumes` they are kept.

Off-site copies: `saba db backup s3 set --bucket <b> ...` mirrors every backup to
S3-compatible storage (keys default to `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` of the CLI's
environment; `--secret-access-key -` reads stdin). The settings are checked (write, read, list,
delete) before being saved. Afterwards each backup's `location` is `local`, `s3` or
`local+s3`; list, download, restore and delete work for all of them, retention applies to the
bucket, and `destroy` never deletes from the bucket. If an upload fails the backup is still
created (`warning` in the result, a failing `backup-storage` check in `saba doctor`) and is
uploaded later automatically. Disaster recovery on a new server: configure the same bucket,
`saba db create <same-name>`, then `saba db restore <name> --backup latest --yes`.

## Multiple servers

The server running the daemon is the primary, node `local`. Every other node runs `saba agent`
(the same binary, as the `saba-agent` systemd service). The agent connects **out** to the
primary over TLS (its HTTPS port, or `--cluster-addr`, default 7443, with `--tls off`), and that
connection carries Docker control and replica traffic. Nodes need Docker and no open ports.

- `saba node add <name> <user@host>` installs everything over SSH (root or passwordless sudo;
  `--install-docker` if needed) and waits for the agent to connect.
- `saba node add <name>` without a destination prints two commands to run on the node (for
  machines behind NAT). The join code is single-use, valid 24 h; `--rejoin` issues a new one and
  forgets the node's key (needed after reinstalling a machine).
- The agent dials `--primary-address`, else the primary's `address` setting, else its public IP.
- Status: `pending` (not joined), `connected`, `unreachable` (with the last error).
  `saba node list` shows the agent's saba and Docker versions, load, memory and disk.
- Agents upgrade themselves to the primary's release on connect (checksum-verified).
- `nodes = ["local", "w1"]` (or `--node`, repeatable) spreads replicas round-robin; `["*"]`
  uses every server. Containers report their node in `saba status` and get `SABA_NODE`.
- Images are built on the primary and streamed to nodes over the agent connection.
- An unreachable node's replicas leave the rotation; the app shows `degraded`. When the agent
  reconnects, the primary reconciles the node and its replicas return without a deploy.
- Apps with `volumes` and databases must use exactly one node.
- Container names (`http://api:8080`, `main:5432`) resolve only between containers on the same
  server. An app with `publish` ports is additionally resolvable by name from other nodes, at
  its server's `address` (`saba node add --address` / `saba server config set address`).
- `saba node remove` refuses while apps are placed on the node, then uninstalls the agent over
  SSH when it was installed that way.

## Certificates

Regular domains: nothing to do, a certificate is issued on the first HTTPS request (status
`pending` until then). Wildcards need DNS access through a provider:
  saba certs dns providers                          # names and settings (* = secret)
  saba certs dns set cloudflare api_token=-         # "-" reads the value from stdin
  saba certs dns set hook path=/abs/path            # runs `<path> present|cleanup <record> <value>`
  saba certs dns [--json] | saba certs dns remove --yes
Settings are validated before saving; re-running set for the same provider keeps settings not
given. saba then keeps a wildcard certificate for the base domain and for each `*.domain`
routed to an app; `saba certs` shows every certificate with status and errors. Without a
provider, subdomains under a wildcard route get their own certificates on first request, at most
10 new ones per hour per route: configure a provider for wildcard routes.
`saba certs add --cert --key` installs your own certificate for the names it lists.

## Recipes

```
# First deploy of the current directory, with a custom domain
saba deploy --domain app.example.com

# A deploy failed (exit 5): read why, fix, redeploy
saba logs -a api --deployment latest

# Something is wrong in production
saba status -a api ; saba logs -a api -n 200 ; saba doctor
saba rollback -a api

# Change several variables with a single rollout
saba env set -a api --no-deploy A=1 B=2 && saba env unset -a api OLD

# Preview environment for a branch (an app is cheap; destroy it when done)
saba deploy -a api-pr-42 --git https://github.com/me/api --ref feature-x
saba destroy api-pr-42 --yes

# Validate saba.toml and flags without changing anything
saba deploy --dry-run --json
```

## JSON shapes (abridged)

```
App          { name, kind: "app"|"database", status, current_version, urls[], config{...},
               containers[{name, replica, version, state, restarts, exit_code, started_at}],
               deployments[...], traffic{requests, errors, in_flight}, pending_changes }
status       created | deploying | running | degraded | crashed | stopped | failed
Deployment   { version, status, trigger, message, source{type, image, git, ref, commit, builder},
               image, port, config{...}, replicas[], error{...}, created_at, finished_at, duration_ms }
             status: building | deploying | live | superseded | failed
             image == "" on a superseded version means it can no longer be restored
DeployResult { app, deployed, saved, dry_run, deployment, urls[], plan[] }     # deploy, restart,
                                                         # scale, rollback, env set, domains add
exec/run     { exit_code, stdout, stderr }
doctor       { ok, checks[{name, ok, message, hint}] }
Node         { name, address, status: connected|unreachable|pending, error, version, docker_version,
               hostname, platform, last_seen, stats{cpus, load1, mem_total, mem_used, disk_total, disk_used}, replicas }
Backup       { id, app, file, kind: manual|scheduled, engine, size, created_at,
               location: local|s3|local+s3, warning }      # `backup create -o` adds saved_to
Certificate  { id, names[], source: custom|dns, issuer, not_after, status: active|expired|pending|failed|skipped, error }
Event        { type: "phase"|"log"|"result"|"error", time, phase, stream, message, result, error }
```

## Good to know

- No domain yet? Every app gets `https://<app>.<base_domain>`; `saba server setup` sets
  `base_domain` to `<public-ip>.sslip.io`, which needs no DNS. For your own domain, point an A
  record (or a wildcard) at the server; certificates are issued on the first request.
- `saba deploy` with no changes still builds and rolls out; `saba restart` reuses the image.
- A stopped app is not started by env/domain changes; they are saved and apply on next deploy.
  The same goes for an app that does not exist yet: `env set`, `domains add` and `db link`
  create it without starting it, so secrets can be set before the first `saba deploy`.
- Concurrent deploys of the same app are rejected with `deploy_in_progress` (exit 4).
- A deployment keeps running on the server if the CLI disconnects; follow it with
  `saba logs --deployment latest -f`.
- Restarting the daemon (e.g. upgrading saba with `saba server setup`) does not drop traffic:
  connections wait on systemd-held sockets and in-flight requests finish first. A deployment in
  progress at that moment is marked failed and must be re-run.
