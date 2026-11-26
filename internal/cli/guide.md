# saba reference

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
     -m "note" -d/--detach --dry-run -q/--quiet --events
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
saba destroy <app> --yes [--volumes]

saba context add <name> <user@host> | use <name> | list | remove <name>
saba server setup <user@host> [--name N] [--email E] [--base-domain D] [--install-docker]
                              [--version vX.Y.Z]   Downloads saba from GitHub releases onto the
                              server (same version as this CLI by default). Re-run to upgrade.
saba server info | config | config set <key> <value> | prune
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
Back up with e.g. `saba exec -a main -- pg_dump -U postgres main > backup.sql`.

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
Event        { type: "phase"|"log"|"result"|"error", time, phase, stream, message, result, error }
```

## Good to know

- No domain yet? Every app gets `https://<app>.<base_domain>`; `saba server setup` sets
  `base_domain` to `<public-ip>.sslip.io`, which needs no DNS. For your own domain, point an A
  record (or a wildcard) at the server; certificates are issued on the first request.
- `saba deploy` with no changes still builds and rolls out; `saba restart` reuses the image.
- A stopped app is not started by env/domain changes; they are saved and apply on next deploy.
- Concurrent deploys of the same app are rejected with `deploy_in_progress` (exit 4).
- A deployment keeps running on the server if the CLI disconnects; follow it with
  `saba logs --deployment latest -f`.
- Restarting the daemon (e.g. upgrading saba) interrupts the proxy for a moment; app containers
  keep running.
