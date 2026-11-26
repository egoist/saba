# Configuration

An app's configuration says how it is built and run. It can come from three places, and saba
merges them on every deploy.

## How settings are merged

From lowest to highest precedence:

1. **What the server remembers**: the configuration the app was last deployed with.
2. **`saba.toml`** in the deployed directory.
3. **Flags** on the command line (`--replicas 3`, `--domain example.com`, ...).

Each deploy sends only the keys present in `saba.toml` and in the flags, and they are applied as
a patch on top of what the server remembers. Two consequences:

- You can change one thing without repeating the rest: `saba deploy --replicas 3`.
- **Removing a key from `saba.toml` does not reset it.** The server keeps the last value. To
  clear a setting, set it to its empty value and deploy once:

  ```toml
  release_command = ""
  domains = []
  publish = []
  ```

On the command line, a list flag given once with an empty value clears that list:
`saba deploy --domain=` or `--publish=`.

The same applies to `[env]`: removing a variable from the file leaves it set on the server. Use
`saba env unset NAME`.

Unknown keys are rejected by name (`unknown config key "prot"`), so typos never silently do
nothing. Validate a configuration without deploying with `saba deploy --dry-run`.

## saba.toml

`saba init` writes a commented starting point. Every key is optional.

### Top level

```toml
name = "api"          # the app's name; default: the directory name
context = "prod"      # deploy to this context unless --host/--context says otherwise
```

| Key | Type | Default | Description |
|---|---|---|---|
| `name` | string | directory name | App name: 1–40 lowercase letters, digits and dashes, starting with a letter |
| `context` | string | current context | [Context](servers.md#contexts) to use for this project. `server` is accepted as an alias |
| `image` | string | — | Registry image to deploy instead of building the directory |
| `port` | integer | image `EXPOSE` | Port the app listens on inside the container. Passed as `$PORT` |
| `domains` | list of strings | `<name>.<base domain>` | Hostnames routed to the app. `*.example.com` matches one level of subdomains. Replaces the automatic domain |
| `internal` | boolean | `false` | Do not route from the internet; reachable only by other apps at `http://<name>:<port>` |
| `worker` | boolean | `false` | The app does not serve HTTP: no port detection, routing or HTTP health check |
| `replicas` | integer | `1` | Number of containers, 1–50 |
| `nodes` | list of strings | `["local"]` | Servers to spread replicas across: `"local"`, [node](scaling.md) names, or `["*"]` for all |
| `command` | string | image `CMD` | Command to run instead of the image's default |
| `release_command` | string | — | Runs to completion before the new version gets traffic. See [release commands](deploying.md#release-commands) |
| `strategy` | string | `"rolling"` | `"rolling"` or `"recreate"`. See [strategies](deploying.md#strategies) |
| `volumes` | list of strings | — | `"name:/path"` for a managed volume, `"/host/path:/path"` for a bind mount |
| `publish` | list of strings | — | Raw host ports: `"5432:5432"`, `"10.0.0.1:5432:5432"`, `"53:53/udp"`. Forces `recreate`; at most one replica |
| `drain_timeout` | duration | `"30s"` | Time in-flight requests get to finish on the old version |
| `stop_timeout` | duration | `"10s"` | Time between `SIGTERM` and `SIGKILL` when a container stops |

Durations are written like `"30s"`, `"2m"` or `"1h30m"`; a bare number means seconds.

Commands (`command`, `release_command`, `health.command`, cron `command`) are split like a shell
would, honoring quotes. A command that uses shell syntax (`&&`, `|`, `$VAR`, redirects, globs) is
run with `sh -c`, so `release_command = "npm run migrate && npm run seed"` works.

### [build]

How a directory or repository is built. Ignored for image deploys.

```toml
[build]
dockerfile = "docker/Dockerfile.prod"
target = "runtime"
args = { NODE_ENV = "production", VERSION = "1.4" }
```

| Key | Type | Description |
|---|---|---|
| `dockerfile` | string | Dockerfile path relative to the source directory. Must be inside it |
| `target` | string | Build stage to stop at (`docker build --target`) |
| `args` | table | Build arguments (`--build-arg`) |

Without a Dockerfile, a build is generated for the detected stack. See [builds](deploying.md#builds).

### [health]

When a new version counts as ready. See [health checks](deploying.md#health-checks).

```toml
[health]
path = "/healthz"
timeout = "90s"
interval = "1s"
```

| Key | Type | Default | Description |
|---|---|---|---|
| `path` | string | — | HTTP path that must return 2xx/3xx. Without it, any response below 500 on `/` counts |
| `command` | string | — | Command run inside the container; exit code 0 means healthy. Takes precedence over `path` |
| `timeout` | duration | `"60s"` | Total time a new version has to become healthy |
| `interval` | duration | `"500ms"` | Pause between attempts |

### [resources]

```toml
[resources]
memory = "512m"
cpus = "1.5"
```

Per container, with Docker's syntax (`--memory`, `--cpus`). A container that exceeds its memory
limit is killed; during a deploy this fails the health check with a message saying so.

### [env]

Non-secret environment variables. Values may be strings, numbers or booleans.

```toml
[env]
NODE_ENV = "production"
WORKERS = 4
DATABASE_URL = "${{main.URL}}"
```

They are merged into the app's stored environment on deploy. Keep secrets out of this file and
set them with `saba env set`. See [Environment variables](environment.md).

### [[cron]]

Scheduled commands, each in a fresh container from the app's live image with its environment
and volumes.

```toml
[[cron]]
name = "cleanup"
schedule = "0 3 * * *"
command = "node scripts/cleanup.js"

[[cron]]
name = "report"
schedule = "@hourly"
command = "python report.py --send"
```

| Key | Description |
|---|---|
| `name` | Unique within the app; lowercase letters, digits, `-`, `_` and `.` |
| `schedule` | Five-field cron expression, in **UTC**, or `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly` |
| `command` | Command to run |

Schedules support lists (`1,15`), ranges (`9-17`), steps (`*/15`, `5-59/20`) and both `0` and `7`
for Sunday. When both day-of-month and day-of-week are restricted, either may match, as in
classic cron. See [cron jobs](operations.md#cron-jobs).

## Flags and their keys

`saba deploy` flags set the same keys:

| Flag | Key |
|---|---|
| `--image` | `image` (the image to deploy) |
| `--port` | `port` |
| `--domain` (repeatable) | `domains` (replaces the list) |
| `--replicas` | `replicas` |
| `--node` (repeatable) | `nodes` (replaces the list) |
| `--command` | `command` |
| `--release-command` | `release_command` |
| `--strategy` | `strategy` |
| `--health-path` | `health.path` |
| `--memory`, `--cpus` | `resources.memory`, `resources.cpus` |
| `--volume` (repeatable) | `volumes` (replaces the list) |
| `--publish` (repeatable) | `publish` (replaces the list) |
| `--dockerfile` | `build.dockerfile` |
| `--worker`, `--internal` | `worker`, `internal` |
| `-e KEY=VALUE`, `--env-file` | environment variables |

`saba domains add/remove`, `saba scale`, `saba db publish` and `saba env set/unset` change one
thing at a time without a deploy from source. An empty value on a repeatable flag
(`--domain=`, `--publish=`, `--volume=`, `--node=`) clears the list.

## Validation rules

saba rejects a configuration, before anything is built, when:

- a domain is not a valid hostname, has no dot, or is already used by another app,
- a volume is not `name:/path` or `/host/path:/path`,
- `publish` is combined with more than one replica,
- an app with volumes is placed on more than one node,
- a database has more than one replica or node,
- a node name does not exist,
- a cron job has no command, a duplicate name or an invalid schedule,
- an environment variable references an app or variable that does not exist.

Domains are normalized: lowercased, with any scheme, path, port or trailing dot removed.

## CLI settings

### Environment variables

| Variable | Effect |
|---|---|
| `SABA_HOST` | Server to talk to, like `--host`: `user@host`, `ssh://user@host:2222`, `local` or `unix:///path/to.sock` |
| `SABA_CONTEXT` | Context to use, like `--context` |
| `SABA_APP` | App to operate on, like `--app` |
| `SABA_JSON` | `1` makes every command print JSON, like `--json` |
| `SABA_CONFIG` | Path of the CLI's context file |
| `SABA_SOCKET` | Daemon socket used for `local` (default `/run/saba/saba.sock`) |
| `SABA_SSH_OPTS` | Extra options for every `ssh` invocation, e.g. `-i ~/.ssh/deploy -o StrictHostKeyChecking=accept-new` |
| `SABA_SSH_MUX` | `0` disables SSH connection sharing between commands |
| `NO_COLOR` / `FORCE_COLOR` | Disable or force colored output |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | Default credentials for `saba db backup s3 set` |

The daemon reads `SABA_DEBUG=1` for verbose logs.

### The context file

Contexts live in `~/.config/saba/config.toml` on Linux and
`~/Library/Application Support/saba/config.toml` on macOS (or `$SABA_CONFIG`). `saba context`
manages it, but it is plain TOML:

```toml
current = "prod"

[contexts.prod]
ssh = "root@203.0.113.10"

[contexts.staging]
ssh = "ssh://deploy@staging.example.com:2222"
```

See [Servers](servers.md#contexts).
