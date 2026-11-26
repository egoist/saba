# Automation and AI agents

saba is built to be driven by scripts, CI pipelines and AI agents as easily as by people. Every
command has machine-readable output, failures carry stable codes and a suggested fix, nothing
prompts, and an MCP server exposes the same operations as tools.

## The output contract

Add `--json` to any command, or set `SABA_JSON=1` for all of them.

- **On success, stdout is exactly one JSON document**, and nothing else is printed there.
- **On failure, stdout is empty** and stderr holds one JSON document:

  ```json
  {
    "error": {
      "code": "health_check_failed",
      "message": "v15 did not become healthy: the container exited with code 3 shortly after starting",
      "hint": "The container output above usually says why (missing env var, bad command, crash on boot). The previous version keeps serving traffic. Fix the problem and deploy again.",
      "details": {
        "logs": ["starting up", "fatal: missing DATABASE_URL"],
        "replica": 2,
        "version": 15
      }
    }
  }
  ```

  `code` is stable and meant to be branched on. `hint` says how to fix it. `details` holds what is
  relevant to that error: the container's last log lines, an exit code, a command's output.

- **The exit code tells the category** without parsing anything.

This holds for errors raised before a command even runs: an unknown command or flag still gives
JSON on stderr and exit code 2.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Success |
| `1` | Error (anything not listed below), or `saba doctor` found a problem |
| `2` | Invalid input or usage; also a destructive command run without `--yes` |
| `3` | Not found: app, version, backup, node, certificate |
| `4` | Conflict, or another operation is in progress for the app |
| `5` | Deployment failed: build, release command or health check |
| `6` | The server cannot be reached, no server is configured, or the requested context does not exist |

`saba exec` and `saba run` exit with the remote command's own exit code.

### Error codes

| Code | Exit | Typical cause |
|---|---|---|
| `invalid_request` | 2 | Bad flag, config key, name, domain, schedule |
| `confirmation_required` | 2 | A destructive command needs `--yes` |
| `not_found` | 3 | The app, version or resource does not exist |
| `conflict` | 4 | Domain already used, nothing to roll back to, app still referenced |
| `deploy_in_progress` | 4 | Another operation on the same app is running |
| `build_failed` | 5 | Image build, pull or clone failed |
| `release_failed` | 5 | The release command exited non-zero |
| `health_check_failed` | 5 | A new replica did not become healthy |
| `deploy_failed` | 5 | Anything else that stopped a deployment; `details.cause` holds the underlying code, e.g. `runtime_error` |
| `command_failed` | 1 | A cron job, backup or restore command failed |
| `runtime_error` | 1 | Docker refused an operation outside a deployment |
| `server_unreachable` | 6 | SSH failed, saba is not installed, or the daemon is down |
| `no_server_configured` | 6 | No `--host`, context or local daemon |
| `internal_error` | 1 | A bug; please report it |

### Streams

A few commands produce a stream rather than one document. With `--json` they print one JSON
object per line (NDJSON):

| Command | Stream |
|---|---|
| `saba logs --json` | One event per log line |
| `saba logs --deployment <v> --json` | The deployment's events |
| `saba deploy --json --events` | Progress events while deploying, ending in a `result` or `error` event |

Plain `saba deploy --json` prints only the final result.

Events look like this (the `result` is abridged):

```json
{"type":"phase","time":"2026-11-26T09:54:59.989Z","phase":"health","message":"Waiting for v4 to become healthy (HTTP on port 80, timeout 1m0s)"}
{"type":"log","time":"2026-11-26T09:54:59.990Z","stream":"container","replica":1,"message":"listening on 3000"}
{"type":"result","time":"2026-11-26T09:55:00.517Z","message":"v4 is live","result":{"app":"web","deployed":true,"deployment":{"version":4,"status":"live"},"urls":["https://web.example.com"]}}
```

| `type` | Fields |
|---|---|
| `phase` | `phase`: `queued` (first), then `pull`, `clone`, `build`, `ship`, `release`, `stop`, `start`, `health`, `switch`, `drain`, `restore`, `cleanup` |
| `log` | `stream`: `build`, `clone`, `pull`, `release`, `deploy` (saba's own notes) or `container`; `replica` for container output |
| `result` | `result`: the same document `saba deploy --json` prints |
| `error` | `error`: the same object as on stderr |

### No prompts, no surprises

- saba never asks a question. Destructive commands fail with `confirmation_required` until you
  add `--yes`: `destroy`, `db restore`, `db backup delete`, `node remove`, `certs remove`.
- Nothing follows or waits forever by default. `logs` prints and exits unless `-f`; `deploy`
  returns when the version is live or has failed (`-d` returns immediately). Interrupting a
  command never interrupts the deployment; `saba cancel` does, and returns
  `{"app", "version", "canceled", "status"}`.
- When there is no terminal, SSH never prompts either: an unknown host key or a missing key is
  an error, not a hang.
- `saba deploy --dry-run --json` validates everything against the server and returns the plan
  without changing anything.

## CI/CD

Deploying from CI needs the saba CLI, an SSH key the server accepts, and the server's host key.

### GitHub Actions

```yaml
name: Deploy
on:
  push:
    branches: [main]

concurrency: deploy-production

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Install saba
        run: |
          curl -fsSL https://raw.githubusercontent.com/egoist/saba/main/install.sh | SABA_INSTALL_DIR="$HOME/.local/bin" sh
          echo "$HOME/.local/bin" >> "$GITHUB_PATH"

      - name: Configure SSH
        run: |
          install -m 700 -d ~/.ssh
          echo "${{ secrets.DEPLOY_SSH_KEY }}" > ~/.ssh/id_ed25519
          chmod 600 ~/.ssh/id_ed25519
          echo "${{ secrets.DEPLOY_KNOWN_HOSTS }}" >> ~/.ssh/known_hosts

      - name: Deploy
        env:
          SABA_HOST: deploy@203.0.113.10
        run: saba deploy -q
```

- `DEPLOY_SSH_KEY`: a private key whose public half is in the server's `authorized_keys`. Use a
  dedicated key.
- `DEPLOY_KNOWN_HOSTS`: the output of `ssh-keyscan 203.0.113.10`, checked against the server
  once. Pinning it means CI cannot be tricked into deploying to another machine.
- `concurrency` keeps two pushes from deploying at once; saba would reject the second with
  `deploy_in_progress` anyway.

The job fails with saba's exit code: 5 when the new version did not become healthy, while the
old one keeps serving.

### Other CI systems

The recipe is the same everywhere: install the CLI, put the key and known hosts in `~/.ssh`
(or point `SABA_SSH_OPTS` at them: `-i /path/key -o UserKnownHostsFile=/path/known_hosts`), set
`SABA_HOST` and run `saba deploy`. Use `--json` when a later step needs the URL or version:

```sh
url=$(saba deploy --json | jq -r '.urls[0]')
```

## AI agents

### `saba guide`

```sh
saba guide
```

prints a compact, complete reference: concepts, every command, the `saba.toml` schema, output
shapes, error codes and recipes. It is written to be read by a model at the start of a task and
fits comfortably in a context window. `saba guide --json` wraps it in a JSON document.

### The MCP server

`saba mcp` runs a [Model Context Protocol](https://modelcontextprotocol.io) server on stdio,
exposing saba's operations as tools.

**Claude Code**:

```sh
claude mcp add saba -- saba mcp --context prod
```

**Claude Desktop, Cursor and other clients** that take a JSON configuration:

```json
{
  "mcpServers": {
    "saba": { "command": "saba", "args": ["mcp", "--context", "prod"] }
  }
}
```

The server is selected when the MCP server starts, with the same rules as the CLI: `--context`
or `--host` in `args`, otherwise `saba.toml` in the working directory, otherwise the current
context. Pin it explicitly, so an agent cannot drift to another server.

| Tool | What it does |
|---|---|
| `saba_guide` | The full reference (read this first) |
| `saba_apps`, `saba_status` | List apps; one app's full state |
| `saba_deploy` | Deploy a directory, image or git repository, with config and env; supports `dry_run` |
| `saba_logs` | Runtime logs, or a deployment's log (`deployment: "latest"`) |
| `saba_deployments`, `saba_rollback`, `saba_cancel` | History; roll back; stop a deployment in progress |
| `saba_restart`, `saba_scale`, `saba_stop`, `saba_start` | Lifecycle |
| `saba_env_list`, `saba_env_set` | Environment variables |
| `saba_domains` | List, add or remove domains |
| `saba_exec` | Run a command in a replica, or in a one-off container |
| `saba_db_create`, `saba_db_link` | Databases |
| `saba_db_backup`, `saba_db_backups`, `saba_db_restore` | Backups |
| `saba_nodes`, `saba_certs`, `saba_doctor` | Cluster, certificates, diagnostics |
| `saba_destroy` | Delete an app |

Each tool runs the CLI with `--json` and returns its output, so tools behave exactly like the
commands and fail with the same structured errors. `saba_logs` returns plain text, which is what
a model reads best.

Read-only tools are annotated as such. `saba_destroy` and `saba_db_restore` are marked
destructive and refuse to run unless called with `confirm: true`, so an agent has to make that
decision explicitly. Tool calls run concurrently, so an agent can check status while a deploy is
in progress.

### Guidance for agents

A loop that works well:

1. `saba_status` or `saba_apps` to see what exists.
2. `saba_deploy` with `dry_run: true` to validate a change, then without it to ship.
3. On failure, read `error.code`, `error.hint` and `error.details.logs`; for build and rollout
   problems, `saba_logs` with `deployment: "latest"`.
4. `saba_doctor` when something is wrong and the cause is not obvious.
5. `saba_rollback` to restore service first, then investigate; `saba_cancel` to stop a
   deployment that is going nowhere.

Nothing an agent does can leave an app half-deployed: a failed deploy keeps the previous version
serving and its configuration unchanged.
