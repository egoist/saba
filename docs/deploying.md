# Deploying

`saba deploy` creates a new version of an app and switches traffic to it without downtime. This
page covers where the code comes from, how it is built, how saba decides a version is ready, and
how to undo a deploy.

## Sources

The source is the first of these that applies:

| Source | How | What happens |
|---|---|---|
| A registry image | `--image ghcr.io/me/api:1.4` | Pulled on the server |
| A git repository | `--git https://github.com/me/api --ref main` | Cloned and built on the server |
| `image` in `saba.toml` | `image = "nginx:alpine"` | Pulled on the server |
| A directory | `saba deploy [path]` (default `.`) | Uploaded and built on the server |

### Uploading a directory

Inside a git checkout, saba uploads every tracked file plus untracked files that are **not**
ignored, so `.gitignore` decides what is sent; uncommitted changes are included. Outside a git
checkout it uploads everything except `.git`, `node_modules`, `.venv`, `__pycache__` and
`.DS_Store`. Symlinks are preserved. The unpacked upload may be up to 4 GiB.

When the directory is a git checkout, the short commit hash is recorded with the deployment, and
the last commit's subject becomes the deployment note unless you pass `-m`.

### Git repositories

The server fetches only the requested ref (`--ref`, default: the default branch) with depth 1.
Private repositories need credentials in the URL (`https://<token>@github.com/me/api`) or an SSH
key on the server that can read the repository. Credentials in URLs are hidden in logs.

### Registry images

The image is pulled on the server and pinned under saba's own tag (`saba/<app>:v<N>`), so a
rollback brings back exactly that image even if the registry tag moved. If the pull fails but
the image is already on the server, saba uses that copy and says so. For private registries, run
`docker login` on the server once.

## Builds

A directory or repository is built with **its `Dockerfile`** when it has one at the top level.
Point at another one, a build stage or build arguments in `saba.toml`:

```toml
[build]
dockerfile = "docker/Dockerfile.prod"   # relative to the source directory
target = "runtime"
args = { NODE_ENV = "production" }
```

Without a Dockerfile, saba detects the stack and generates one:

| Detected by | Stack | Build | Listens on |
|---|---|---|---|
| `package.json` + `bun.lock` / `bun.lockb` | Bun | `bun install --frozen-lockfile`, then the `build` script if any | 3000 |
| `package.json` | Node.js | `npm ci` / `pnpm` / `yarn` according to the lockfile, then the `build` script if any | 3000 |
| `go.mod` | Go | `go build` of the main package at the top level, or the first one under `cmd/` | 8080 |
| `requirements.txt` or `pyproject.toml` | Python | `uv sync` with a `uv.lock`, else `pip install` | 8000 |
| `Cargo.toml` | Rust | `cargo build --release` | 8080 |
| `index.html`, `public/index.html` or `dist/index.html` | Static site | Served by nginx | 80 |

For Node.js, Bun and Python, the start command is the `web:` line of a `Procfile` when there is
one. Otherwise:

- **Node.js / Bun**: the `start` script, else `main` from `package.json`, else `server.js` or
  `index.js`.
- **Python**: `python manage.py runserver 0.0.0.0:$PORT` for Django, else `main.py` or `app.py`.
  For production servers, use a `Procfile`, e.g.
  `web: gunicorn app:app --bind 0.0.0.0:$PORT`.

Generated builds use a default `.dockerignore` (version control, dependencies, `.env` files,
logs) unless the project has its own. When detection fails, the error says what is missing; you
can always add a Dockerfile to take full control. `saba init` shows what was detected.

## Ports

saba needs to know which port the app listens on inside its container. In order:

1. `port` in the configuration (`--port`, or `port =` in `saba.toml`),
2. the first TCP port the image `EXPOSE`s,
3. none: the app is deployed as a worker without HTTP routing, and the deploy log says so.

The port is passed to the app as `$PORT` (unless you set `PORT` yourself). **The app must listen
on `0.0.0.0`, not `127.0.0.1`**, or nothing outside the container can reach it. Apps that do not
serve HTTP at all should set `worker = true`.

## Health checks

A new version only receives traffic after **every** replica passes its health check. Pick the
kind that matches the app:

| Configuration | Healthy when |
|---|---|
| nothing (HTTP app) | `GET /` gets any HTTP response below 500 |
| `[health] path = "/healthz"` | `GET /healthz` returns 2xx or 3xx |
| `[health] command = "pg_isready"` | the command, run inside the container, exits 0 |
| a worker (`worker = true`, or no port) | the container stays up for 3 seconds, or its image `HEALTHCHECK` reports healthy |

```toml
[health]
path = "/healthz"
timeout = "90s"      # total time allowed to become healthy (default 60s)
interval = "1s"      # pause between attempts (default 500ms)
```

HTTP checks are sent to each replica directly, with the `Host` header set to the app's first
domain. A check fails immediately, without waiting for the timeout, when the container exits,
restarts, is killed for exceeding its memory limit, or its image's own `HEALTHCHECK` reports
unhealthy. The error then includes the container's last 40 lines of output, which usually say
why: a missing environment variable, a wrong command, a crash on boot.

A good `/healthz` checks that the process is up and can serve requests. Make it cheap; it is
called often during a deploy.

## Strategies

### Rolling (default)

New replicas start next to the old ones. Once all are healthy, traffic switches in one step.
The old version then **drains**: requests already in flight, including open WebSocket
connections, get up to `drain_timeout` (default 30s) to finish. Then the old containers get
`SIGTERM`, and `SIGKILL` after `stop_timeout` (default 10s).

During a rolling deploy both versions run at once for a short while. Your app must tolerate
that: two versions against the same database, two copies of a background worker.

### Recreate

```toml
strategy = "recreate"
```

The old version stops before the new one starts. Use it when two versions must never run at
once: an app holding a lock on a volume, a single-instance worker, an embedded database. saba
still avoids errors for HTTP clients: incoming requests are **held** at the proxy, for up to 60
seconds, while the swap happens, and are released to the new version once it is healthy. Clients
see a pause, not a failure.

If the new version fails its health check, saba starts the old containers again.

`recreate` is used automatically for databases and for apps that `publish` host ports, because a
host port can only be bound by one container.

## Release commands

```toml
release_command = "npm run migrate"
```

The release command runs after the image is ready and **before** any new replica starts, in a
one-off container from the new image, with the app's environment and volumes. Its output appears
in the deploy log. If it exits non-zero, the deploy stops there and the current version keeps
serving.

Typical uses are database migrations and cache warm-ups. Because the old version keeps serving
while the release command runs and until the switch, migrations should be backward compatible
with the old code (add a column first, use it in the next deploy).

Release commands, like cron jobs and `saba run`, always run on the primary server, even when the
app's replicas are placed on other nodes.

## Watching a deploy

```sh
saba deploy                 # progress and build output
saba deploy -q              # phases only; build output is printed only if something fails
saba deploy -d              # return as soon as the deployment has started
saba logs --deployment latest -f     # follow a running deployment from anywhere
saba logs --deployment v12           # read the log of a past deployment
```

A deployment runs on the server. If your connection drops or you press Ctrl-C, only the watching
stops; the deployment keeps going. Follow it again with `saba logs --deployment latest -f`.

### Canceling

```sh
saba cancel            # or: saba cancel api
```

This stops the deployment in progress, wherever it is: the build or pull is killed, a running
release command is stopped, new containers are removed. The current version keeps serving and
nothing else changes; the deployment is recorded as failed with `deployment v14 was canceled`.
The command returns once the cleanup is done.

A deployment that has already switched traffic finishes anyway, because the new version is
serving at that point. `saba cancel` says so; undo it with `saba rollback`.

Each deployment's log is kept on the server, so `saba logs --deployment v12` works long after
the fact.

### Validating without deploying

```sh
saba deploy --dry-run
```

This validates the configuration and environment against the server (unknown keys, bad domains,
missing references, domain conflicts) and prints the plan. Nothing is uploaded or changed.

## What a deploy cannot do at the same time

Only one operation runs per app at a time. A second deploy, restart, scale or configuration
change of the same app while one is in progress fails with `deploy_in_progress` (exit code 4):
wait for it, or [cancel](#canceling) it. Different apps deploy in parallel.

A deployment is aborted after 45 minutes. A deployment interrupted by a daemon restart is marked
failed when the daemon comes back; deploy again.

## Restart, scale and redeploy

```sh
saba restart              # fresh containers from the current image
saba scale 3              # change the number of replicas
saba deploy               # rebuild from source, even if nothing changed
```

`restart` and `scale` reuse the live version's image and create a new version with the same
zero-downtime switch. `restart` also applies configuration and environment changes saved with
`--no-deploy` (see [pending changes](environment.md#saving-without-deploying)).

## Rollbacks

```sh
saba rollback                 # the most recent previously-live version
saba rollback --to v12        # a specific one
saba deployments              # the ROLLBACK column says which versions are restorable
```

A rollback is a new deployment (`v15` restoring `v12`) with the same health check and switch.
It restores the target's **image, configuration and environment**. It does not touch volumes or
databases: if `v13` ran a migration, rolling back to `v12` leaves the migrated schema in place.

What can be restored:

- the last 5 superseded versions keep their images (change with
  `saba server config set keep_versions 10`); older ones show an empty ROLLBACK column,
- failed deployments never went live and cannot be restored,
- the last 40 deployments are kept in the history.

## Cleaning up

After a successful switch, the old version's containers are removed. Images beyond the
retention window are deleted. `saba server prune` frees more space by removing dangling images
and build cache older than three days.
