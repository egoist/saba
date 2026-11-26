# Troubleshooting

Start with the tools that diagnose for you, then find your symptom below.

```sh
saba doctor                          # server and app checks, each failure with a fix
saba status <app>                    # containers, restarts, exit codes, pending changes
saba logs <app>                      # what the app printed
saba logs <app> --deployment latest  # what happened during the last deploy
```

On the server itself:

```sh
journalctl -u saba -n 100            # the daemon's log
docker ps -a --filter label=saba.managed=true
```

Every saba error names a `code` and a `hint`. The hint is usually the fix.

## Connecting

### `cannot reach the saba server`

The hint names the cause:

| Hint | Fix |
|---|---|
| SSH authentication failed | `ssh <destination>` must work with a key, without a password prompt. Check the user and key, or set `SABA_SSH_OPTS="-i ~/.ssh/key"` |
| The server's SSH host key is not trusted yet | Run `ssh <destination> true` once and accept the key, or pin it in `~/.ssh/known_hosts` |
| saba is not installed on the server | `saba server setup <destination>` |
| The saba daemon is not running | `ssh <destination> systemctl status saba` and `journalctl -u saba`; see [the daemon does not start](#the-daemon-does-not-start) |
| The SSH user may not use the saba daemon | Connect as root, or add the user to the `docker` group: `ssh <destination> 'sudo usermod -aG docker $USER'` |

Running the command with `--json` shows SSH's full output under `error.details.output`.

### `no saba server is configured`

The command found no `--host`, no context and no local daemon. Set up a server
(`saba server setup`), add an existing one (`saba context add prod root@203.0.113.10`), or pass
`--host`. See [contexts](servers.md#contexts).

### Commands go to the wrong server

The order is `--host`, `--context`, `context` in `saba.toml`, then the current context. A
`saba.toml` with `context = "..."` in the directory you are in takes precedence over
`saba context use`. Check with `saba server info`.

### The daemon does not start

`journalctl -u saba -n 50` shows why. The usual causes:

- **Ports 80 or 443 are in use**, typically by a web server that came with the OS image. Find it
  with `ss -ltnp | grep -E ':(80|443) '` and stop or remove it.
- **Docker is not running**: `systemctl status docker`.
- **`socket path ... is too long`** (local daemons): unix socket paths are limited to about 107
  characters; use a shorter `--socket` path.

## Deploying

### `health_check_failed`

The new version never became healthy. The current version is still serving and nothing else
changed. The error says which of these happened:

- **`the container exited with code N shortly after starting`**: the app crashed on boot. The
  error includes its last output; a missing environment variable or a wrong `command` is the
  usual reason. Check with `saba env` and fix with `saba env set`.
- **`the app is not answering HTTP requests`**: nothing answered on the port. Either the app
  listens on `127.0.0.1` instead of `0.0.0.0` (it must bind to all interfaces, on `$PORT`), it
  listens on another port (set `port`; the hint names the port the image exposes when it
  differs), it needs longer to start (raise `[health] timeout`), or it does not serve HTTP at all
  (set `worker = true`).
- **`GET /healthz returned 404`** (or another status): with `[health] path` set, that path must
  return 2xx or 3xx.
- **`killed for exceeding its memory limit`**: raise `[resources] memory`.
- **`health command exited with code N`**: the `[health] command` failed inside the container.

### `build_failed`

The output above the error shows the failing build step. Reproduce it locally with
`docker build .`. Common causes:

- a file the build needs is ignored by `.gitignore`, so it was not uploaded (uncommitted but
  not ignored files are uploaded),
- the generated build could not find how to start the app: add a `start` script, a `Procfile`
  with a `web:` line, or a Dockerfile,
- a private base image or registry image: run `docker login` on the server.

### `release_failed`

The release command exited non-zero; its output is in the deploy log
(`saba logs --deployment latest`). The current version kept serving. Fix the command, or clear
it with `release_command = ""`.

### `deploy_in_progress`

Another operation on the same app is running. Follow it with
`saba logs --deployment latest -f` and retry when it finishes, or stop it with `saba cancel`. A
deployment interrupted by a daemon restart is marked failed, not left in progress.

### A setting I removed from saba.toml is still active

Settings are remembered by the server; removing a key does not reset it. Set it to an empty value
(`release_command = ""`, `domains = []`) and deploy once. See
[how settings are merged](configuration.md#how-settings-are-merged).

### `has no previous version to roll back to`

Only superseded versions whose image is still kept can be restored: the last 5 per app by
default. `saba deployments` shows what is restorable. Keep more with
`saba server config set keep_versions 10`.

## Serving

### `404 No app is configured for this domain`

The hostname is not routed to any app. Check `saba domains -a <app>`. Remember that custom
domains replace the automatic `<app>.<base domain>`, and that `*.example.com` covers one level of
subdomains only.

### `502 The app did not respond`

The proxy reached no replica, or a replica closed the connection without answering. Look at
`saba status` (are the containers running, are they restarting?) and `saba logs`. For apps on
other nodes, check `saba node list`.

### `503 The app is not running`

No replica is running: the app was stopped (`saba start`), or every replica crashed (see below).
`503 The app is restarting` means a `recreate` deploy took longer than 60 seconds to bring the new
version up.

### The app ran fine, then crashed or degraded

`saba status` shows restart counts, exit codes and out-of-memory kills; `saba logs` shows the
output. To restore service while you investigate, `saba rollback` or `saba restart`.

## HTTPS

### The browser shows a certificate error, or HTTPS hangs

Certificates are requested on the first HTTPS request for a hostname. For that to work:

- the hostname's DNS must point at this server (`saba doctor` checks it),
- ports 80 and 443 must be reachable from the internet; check the cloud provider's firewall,
- the hostname must be routed to an app.

`saba certs` lists every routed hostname with its certificate's status and, for a failed one,
the error from Let's Encrypt; `saba doctor` reports failures too.

Under a wildcard domain (`*.example.com`) without a
[DNS provider](domains-and-https.md#wildcard-certificates), every subdomain needs its own
certificate, issued on its first request and limited to 10 new subdomains per hour; configure a
provider instead.

### A wildcard certificate fails

`saba certs` shows the cause. The usual ones:

- **Wrong or under-privileged credentials.** Check them with `saba certs dns`, and set them again
  with `saba certs dns set <provider> <key>=-`. A Cloudflare token needs Zone:Read and DNS:Edit
  on the zone.
- **`could not determine zone`.** saba finds the zone by asking DNS for its SOA record; the
  domain must be delegated to the provider's name servers.
- **Timed out waiting for the record.** saba waits until the TXT record is visible on the zone's
  authoritative name servers. If the server cannot query them directly, point the daemon at a
  resolver that can: add `--dns-resolver 1.1.1.1:53` to its command line.

Failures are retried with growing intervals, and right away after `saba certs dns set`.

Let's Encrypt has rate limits on certificates per domain per week; repeatedly destroying and
recreating apps under one domain can hit them. A wildcard certificate for the base domain avoids
per-app certificates entirely.

Every issued and failed certificate is logged by the daemon (`journalctl -u saba`); with
[verbose logs](servers.md#the-daemon-and-systemd) the ACME exchange is logged as well.

### Behind Cloudflare

Use SSL mode **Full (strict)**, and install a Cloudflare origin certificate with `saba certs add`
or configure the `cloudflare` DNS provider. See [Behind Cloudflare's proxy](domains-and-https.md#behind-cloudflares-proxy).

## Nodes

### A node stays `pending`

Its agent has not joined. Run the commands `saba node add <name>` printed on the node, or
`saba node add <name> --rejoin` for a new join code (the old one stops working after 24 hours or
once used).

### A node shows `unreachable`

`saba node list` shows when it disconnected. On the node:

```sh
systemctl status saba-agent
journalctl -u saba-agent -n 50
```

The agent's log says why it cannot connect:

- **connection refused or timed out**: the node cannot reach the primary's address on port 443
  (7443 with `--tls off`). Check the primary's firewall, or join with a `--primary-address`
  the node can reach.
- **the primary refused this node**: the node was removed, or its key changed because it was
  reinstalled. Run `saba node add <name> --rejoin` and use the new code.
- **the primary's key is ..., but this node joined ...**: the agent reached a different server
  than the one it joined, or the primary's data directory was replaced. Rejoin the node.

The agent reconnects on its own once the cause is fixed; its replicas come back without a deploy.

### `the agent runs another version than the primary`

Agents upgrade themselves to the primary's release when they connect. That fails when the node
cannot download from GitHub (the agent's log says so), and never happens for development
builds: install the same build on the node, e.g. `saba node add <name> <user@host> --binary
./dist/saba-linux-amd64 --rejoin`.

## Backups

### `the S3 settings do not work`

The hint distinguishes a missing bucket (create it, or pass `--create-bucket`), an unreachable
endpoint, and credentials or permissions. For non-AWS services, `--region` is often `auto` or
`us-east-1`, and the endpoint must include `https://`.

### `backup-storage` fails in `saba doctor`

Either S3 cannot be reached right now (the check shows the error), or some backups exist only on
the server because their upload failed (`LOCATION local` in `saba db backup list`). They are
uploaded automatically once uploads work again, within the hour, or immediately when you run
`saba db backup s3 set` again.

### `has nothing to back up: it has no named volumes`

The app keeps no data saba can back up: it has no named volumes, only bind mounts or none at
all. Data written inside a container is lost on every deploy anyway; keep it in a named volume
(`volumes = ["data:/data"]`). Back up bind-mounted host paths with your own tools.

### A volume backup fails while the app writes

`tar` can fail on files that change while they are read. Retry, or back up with `--stop`
(also for schedules: `saba backup schedule -a <app> @daily --stop`).

### A restore failed

For a volume restore that fails while unpacking, the volumes may be partly restored; run the
restore again, which empties them first. A file that is not a valid archive is refused before
anything changes.

With `--json`, `error.details.output` has the database client's output. A PostgreSQL restore runs
in one transaction, so a failure changes nothing; MySQL, MariaDB and MongoDB restores are not
atomic.

## Disk space

`saba doctor` warns at 90%. Free space with `saba server prune` (dangling images and old build
cache), lower `keep_versions`, delete old manual backups, and destroy apps you no longer need.
Docker's own `docker system df` shows what uses the space.
