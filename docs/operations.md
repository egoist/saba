# Operating apps

Day-to-day commands for looking at, debugging and managing running apps. Inside a project
directory with a `saba.toml` they pick up the app from it; otherwise pass it as an argument
(`saba status api`), with `-a api`, or through `SABA_APP`. Only `saba deploy` falls back to the
directory's name.

## What is running

```sh
saba apps                # every app: status, version, replicas, last deploy, URL
saba status              # one app in detail
saba status api --json   # the full machine-readable state
```

`saba status` shows the URL, where the code came from (image, commit, builder), the live
version, each container with its node, state and restart count, cron jobs with their last
result, recent deployments, traffic counters since the daemon started, and whether saved changes
are waiting for a deploy. When something is wrong it ends with what to do about it.

The status values are described in [Concepts](concepts.md#app-status).

### What happens after a deploy

Health checks gate deploys; they do not run continuously afterwards. Once a version is live:

- a replica that crashes is restarted by Docker, and the proxy routes around it in the meantime,
- a replica whose image defines a `HEALTHCHECK` shows its health in `saba status`,
- the app shows `degraded` while some replicas are down, and `crashed` when none run.

`saba doctor` reports crashed and degraded apps, with the commands to investigate.

## Logs

```sh
saba logs                    # the last 100 lines of every replica, then exit
saba logs -f                 # keep streaming
saba logs -n 500 --since 2h  # more history
saba logs --replica 2        # one replica
saba logs -t                 # with timestamps
```

With several replicas, each line is prefixed with its replica number. Lines from all replicas are
merged in time order. `--since` takes a duration (`10m`, `2h`) or an RFC 3339 time.

Logs come from the live version's containers. Docker keeps up to 60 MB per container (three 20 MB
files) when its logging driver is `json-file` or `local`, the default on most installations.

### Deployment logs

```sh
saba logs --deployment latest       # the most recent deployment's build and rollout
saba logs --deployment v12
saba logs --deployment latest -f    # follow a deployment in progress
```

Every deployment's log is kept on the server, for the last 40 deployments. It is the first
place to look when a deploy failed.

With `--json`, both kinds of logs are printed as one JSON event per line. See
[Automation](automation.md#streams).

## Running commands

### In a running replica

```sh
saba exec -- ls -la /app
saba exec --replica 2 -- cat /tmp/debug.log
saba exec -t -- sh                       # an interactive shell
saba exec -t -- rails console
```

Everything after `--` is the command. Output is streamed, and the command's exit code becomes
`saba`'s exit code, so `saba exec -- test -f /app/ready` works in scripts. `-t` gives an
interactive terminal by handing your terminal to `docker exec -it` over SSH. With `--json`, the
result is one document with `stdout`, `stderr` and `exit_code`.

### In a fresh container

```sh
saba run -- npm run migrate
saba run -- python manage.py createsuperuser --noinput
```

`saba run` starts a new container from the live version's image, with the app's environment and
volumes, runs the command to completion and removes the container. Use it for one-off tasks that
should not run inside a serving replica. It runs on the primary server.

## Resource usage

```sh
saba stats          # every running container, on every server
saba stats api
```

Shows CPU, memory (and its share of the limit or of the server), network and disk I/O, and the
number of processes per container.

## Cron jobs

Cron jobs are declared in [`saba.toml`](configuration.md#cron) and deployed with the app:

```toml
[[cron]]
name = "cleanup"
schedule = "0 3 * * *"
command = "node scripts/cleanup.js"
```

```sh
saba cron                # jobs, schedules (UTC) and the result of their last run
saba cron run cleanup    # run one now and print its output
```

Each run starts a fresh container from the app's live image, with its environment and volumes, on
the primary server. saba records when it ran, how long it took, its exit code and the last 4 KB of
its output, shown in `saba cron` and `saba status`.

Things to know:

- If a run is still going when the next one is due, the next one is skipped.
- Runs missed while the daemon was down are not made up later.
- A failed run is not retried; it shows as `failed` until the next run.
- Jobs of a stopped app do not run.
- `saba cron run` exits non-zero when the job fails, so it can be used in scripts.

## Stopping and starting

```sh
saba stop
saba start
```

`stop` takes the app out of rotation, lets in-flight requests finish and stops its containers.
Configuration, environment, data and versions are kept. While stopped, requests get `503` and
cron jobs do not run. `start` starts the same containers, waits for them to pass their health
check and routes traffic to them again.

## Deleting an app

```sh
saba destroy old-api --yes
saba destroy old-db --yes --volumes
```

This removes the app's containers, images, deployment history and logs. Named volumes, and a
database's backups on the server, are kept unless `--volumes` is given; backups in S3 are never
deleted. `destroy` never guesses the app from the current directory, always needs `--yes`, and
refuses while other apps reference the app's variables.

## Diagnosing

```sh
saba doctor
```

```text
✓ docker         Docker 29.1.0 is running
✓ base_domain    Apps get <name>.apps.example.com by default
✓ tls            Automatic HTTPS (Let's Encrypt) is enabled
✓ disk           Disk is 41% full (23.5 GiB free)
✓ backup-storage Backups are mirrored to s3://my-backups/prod
✓ node:worker1   Connected (Docker 29.1.0)
✗ app:api        Some replicas are down or restarting
                 → Inspect with `saba status -a api` and `saba logs -a api`.
✓ dns:api.example.com Resolves to this server
```

| Check | Fails when |
|---|---|
| `docker` | Docker is not reachable on the server |
| `base_domain` | No base domain is set, so apps without custom domains have no URL |
| `disk` | The disk holding saba's data is 90% full or more |
| `backup-storage` | The last operation against the S3 backup bucket failed |
| `node:<name>` | A node is unreachable |
| `cert:<name>` | A certificate could not be issued (with Let's Encrypt's error), or an uploaded one expires within two weeks |
| `app:<name>` | An app is crashed, degraded, or has never deployed successfully |
| `dns:<host>` | A routed domain does not resolve (informational when it resolves elsewhere, e.g. through a CDN) |

Every failed check comes with a suggested fix. `saba doctor` exits with code 1 when any check
fails, which makes it usable as a monitoring probe. See also [Troubleshooting](troubleshooting.md).

## Disk space

- Superseded versions keep their images for rollbacks: the last 5 per app, or
  `saba server config set keep_versions N`.
- `saba server prune` removes dangling images and build cache older than three days.
- Database backups on the server follow their retention (`--keep`); manual ones stay until
  deleted.
