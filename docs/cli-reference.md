<!-- Generated from the command definitions by `make docs`. Do not edit by hand. -->

# CLI reference

Every command saba has, with all of its flags. For a guided tour, start with the
[getting started guide](getting-started.md); for a compact overview, run `saba guide`.

## Global flags

These work with every command.

| Flag | Default | Description |
|---|---|---|
| `-a, --app string` |  | App name (default: name in ./saba.toml, env SABA_APP) |
| `-c, --context string` |  | Named server context to use (env SABA_CONTEXT) |
| `-H, --host string` |  | Server to talk to: user@host, or "local" (env SABA_HOST) |
| `--json` |  | Print machine-readable JSON, errors included (env SABA_JSON=1) |
| `--no-color` |  | Disable colors |

## Commands

- [`saba agent`](#saba-agent) — Run on worker nodes: connect this machine to a saba primary
  - [`saba agent install`](#saba-agent-install) — Join a cluster and install the agent as a systemd service (as root)
  - [`saba agent run`](#saba-agent-run) — Run the agent in the foreground (what the service runs)
  - [`saba agent uninstall`](#saba-agent-uninstall) — Stop the agent and remove it (containers and data on the node are kept)
- [`saba apps`](#saba-apps) — List apps on the server
- [`saba cancel`](#saba-cancel) — Stop a deployment in progress
- [`saba certs`](#saba-certs) — TLS certificates: status, wildcards through DNS, your own
  - [`saba certs add`](#saba-certs-add) — Upload a certificate to use for the names it lists
  - [`saba certs dns`](#saba-certs-dns) — Show the DNS provider used for wildcard certificates
    - [`saba certs dns providers`](#saba-certs-dns-providers) — List supported DNS hosts and their settings
    - [`saba certs dns remove`](#saba-certs-dns-remove) — Forget the DNS provider and its credentials
    - [`saba certs dns set`](#saba-certs-dns-set) — Configure the DNS provider (settings are checked before saving)
  - [`saba certs list`](#saba-certs-list) — List stored and pending certificates
  - [`saba certs remove`](#saba-certs-remove) — Delete a stored certificate
- [`saba context`](#saba-context) — Manage the servers this CLI can talk to
  - [`saba context add`](#saba-context-add) — Add a server (the first one becomes the current context)
  - [`saba context list`](#saba-context-list) — List contexts
  - [`saba context remove`](#saba-context-remove) — Forget a context (the server is not touched)
  - [`saba context use`](#saba-context-use) — Set the current context
- [`saba cron`](#saba-cron) — List and trigger an app's scheduled jobs
  - [`saba cron list`](#saba-cron-list) — List cron jobs and their last result
  - [`saba cron run`](#saba-cron-run) — Run a cron job now and print its output
- [`saba daemon`](#saba-daemon) — Run the saba server daemon (proxy, deploy engine, API)
  - [`saba daemon install`](#saba-daemon-install) — Install and start the daemon as a systemd service (run on the server, as root)
  - [`saba daemon uninstall`](#saba-daemon-uninstall) — Stop and remove the systemd service (apps and data are kept)
- [`saba db`](#saba-db) — Create and link managed databases
  - [`saba db backup`](#saba-db-backup) — Create, schedule, download and delete database backups
    - [`saba db backup create`](#saba-db-backup-create) — Back up a database now
    - [`saba db backup delete`](#saba-db-backup-delete) — Delete a stored backup
    - [`saba db backup download`](#saba-db-backup-download) — Download a stored backup (default: the latest)
    - [`saba db backup list`](#saba-db-backup-list) — List stored backups
    - [`saba db backup s3`](#saba-db-backup-s3) — Mirror backups to S3-compatible storage
      - [`saba db backup s3 remove`](#saba-db-backup-s3-remove) — Stop uploading backups (objects in the bucket are kept)
      - [`saba db backup s3 set`](#saba-db-backup-s3-set) — Set (and verify) the bucket backups are uploaded to
      - [`saba db backup s3 show`](#saba-db-backup-s3-show) — Show the configured bucket
    - [`saba db backup schedule`](#saba-db-backup-schedule) — Show or set the automatic backup schedule
  - [`saba db create`](#saba-db-create) — Create a database
  - [`saba db link`](#saba-db-link) — Give an app the database's URL as an environment variable
  - [`saba db list`](#saba-db-list) — List databases
  - [`saba db publish`](#saba-db-publish) — Expose a database on a host port, or stop exposing it
  - [`saba db restore`](#saba-db-restore) — Restore a database from a stored backup or a dump file
  - [`saba db url`](#saba-db-url) — Print a database's connection URL and credentials
- [`saba deploy`](#saba-deploy) — Deploy a directory, a git repository or an image
- [`saba deployments`](#saba-deployments) — List an app's deployment history
- [`saba destroy`](#saba-destroy) — Delete an app, its containers, images and history
- [`saba doctor`](#saba-doctor) — Diagnose problems with the server and its apps
- [`saba domains`](#saba-domains) — Manage the domains routed to an app
  - [`saba domains add`](#saba-domains-add) — Route domains to the app
  - [`saba domains list`](#saba-domains-list) — List an app's domains
  - [`saba domains remove`](#saba-domains-remove) — Stop routing domains to the app
- [`saba env`](#saba-env) — Manage environment variables and secrets
  - [`saba env get`](#saba-env-get) — Print the value of one variable
  - [`saba env import`](#saba-env-import) — Set variables from a dotenv file (use - for stdin)
  - [`saba env list`](#saba-env-list) — List environment variables
  - [`saba env set`](#saba-env-set) — Set variables and roll out the change
  - [`saba env unset`](#saba-env-unset) — Remove variables and roll out the change
- [`saba exec`](#saba-exec) — Run a command inside a running container
- [`saba guide`](#saba-guide) — Print the complete reference (written for AI agents and scripts)
- [`saba init`](#saba-init) — Create a saba.toml for a project
- [`saba logs`](#saba-logs) — Show app logs, or the log of a deployment
- [`saba mcp`](#saba-mcp) — Run a Model Context Protocol server on stdio
- [`saba node`](#saba-node) — Add servers to scale apps across machines
  - [`saba node add`](#saba-node-add) — Add a server to the cluster
  - [`saba node list`](#saba-node-list) — List the cluster's servers
  - [`saba node remove`](#saba-node-remove) — Remove a server from the cluster
- [`saba restart`](#saba-restart) — Restart an app with zero downtime (applies pending config)
- [`saba rollback`](#saba-rollback) — Roll back to the previous (or a specific) version
- [`saba run`](#saba-run) — Run a one-off command in a fresh container of the app
- [`saba scale`](#saba-scale) — Change the number of replicas
- [`saba server`](#saba-server) — Set up and inspect servers
  - [`saba server config`](#saba-server-config) — Show or change server settings
    - [`saba server config set`](#saba-server-config-set) — Change a server setting
  - [`saba server info`](#saba-server-info) — Show the server's version and configuration
  - [`saba server prune`](#saba-server-prune) — Free disk space (dangling images and old build cache)
  - [`saba server setup`](#saba-server-setup) — Install (or upgrade) saba on a server over SSH
- [`saba start`](#saba-start) — Start a stopped app
- [`saba stats`](#saba-stats) — Show CPU and memory usage of running containers
- [`saba status`](#saba-status) — Show the state of an app (or of all apps)
- [`saba stop`](#saba-stop) — Stop an app's containers (keeps config, data and versions)
- [`saba version`](#saba-version) — Print the CLI version (and the server's, if reachable)

## saba agent

Run on worker nodes: connect this machine to a saba primary

```text
The agent runs on every node except the primary. It connects out to the
primary over TLS and carries Docker control and replica traffic over that one
connection, so the node needs no open ports.

"saba node add" installs it for you over SSH. To install it by hand, run the
two commands "saba node add <name>" prints, on the node.
```

Subcommands: [`install`](#saba-agent-install), [`run`](#saba-agent-run), [`uninstall`](#saba-agent-uninstall)

## saba agent install

Join a cluster and install the agent as a systemd service (as root)

```sh
saba agent install --join <code> [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--join string` |  | Join code from `saba node add` |
| `--print-unit` |  | Print the systemd unit instead of installing it |

## saba agent run

Run the agent in the foreground (what the service runs)

```sh
saba agent run [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--config string` | `/etc/saba/agent.json` | Agent configuration file |
| `--data-dir string` | `/var/lib/saba-agent` | Directory for the agent's key |
| `--docker-socket string` | `/var/run/docker.sock` | Docker socket to expose to the primary |
| `--join string` |  | Join code; writes the configuration before running |

## saba agent uninstall

Stop the agent and remove it (containers and data on the node are kept)

```sh
saba agent uninstall
```

## saba apps

List apps on the server

```sh
saba apps
```

Aliases: `ls`, `list`

## saba cancel

Stop a deployment in progress

```sh
saba cancel [app]
```

```text
Abort the app's running deployment: the build, release command or health
check stops, the new containers are removed, and the current version keeps
serving. The command waits until the cleanup is done.

A deployment that has already switched traffic to the new version finishes
anyway; undo it with "saba rollback".
```

**Examples**

```sh
saba cancel
saba cancel api
```

## saba certs

TLS certificates: status, wildcards through DNS, your own

```sh
saba certs
```

```text
Every routed domain gets a Let's Encrypt certificate automatically, on its
first HTTPS request, and saba renews it. `saba certs` shows them all.

Wildcard certificates. Let's Encrypt only issues them through the DNS-01
challenge, which requires access to the domain's DNS. Give saba that access
once and it obtains and renews a wildcard certificate for the server's base
domain and for every wildcard domain routed to an app:

  saba certs dns providers                              # supported DNS hosts and their settings
  echo "$CF_TOKEN" | saba certs dns set cloudflare api_token=-

Your own certificates. A certificate from another CA (or a Cloudflare origin
certificate) is used for the names it lists instead of an issued one:

  saba certs add --cert fullchain.pem --key privkey.pem
```

Aliases: `cert`, `certificates`

**Examples**

```sh
saba certs
saba certs dns set hetzner auth_api_token=-
saba certs add --cert fullchain.pem --key privkey.pem
saba certs remove custom-example.com --yes
```

Subcommands: [`add`](#saba-certs-add), [`dns`](#saba-certs-dns), [`list`](#saba-certs-list), [`remove`](#saba-certs-remove)

## saba certs add

Upload a certificate to use for the names it lists

```sh
saba certs add --cert <file> --key <file> [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--cert string` |  | PEM file with the certificate chain, leaf first |
| `--key string` |  | PEM file with the private key |

## saba certs dns

Show the DNS provider used for wildcard certificates

```sh
saba certs dns
```

```text
saba obtains wildcard certificates through the ACME DNS-01 challenge, by
creating a TXT record at your DNS host. This configures which host and the
credentials for it. The record is created only during issuance and removed
right after.

Credentials are stored on the server and never shown again; a value of "-"
reads it from stdin to keep it out of shell history.
```

**Examples**

```sh
saba certs dns
saba certs dns providers
echo "$CF_TOKEN" | saba certs dns set cloudflare api_token=-
saba certs dns set porkbun api_key=pk1_... api_secret_key=-
saba certs dns set hook path=/usr/local/bin/dns-hook
saba certs dns remove --yes
```

Subcommands: [`providers`](#saba-certs-dns-providers), [`remove`](#saba-certs-dns-remove), [`set`](#saba-certs-dns-set)

## saba certs dns providers

List supported DNS hosts and their settings

```sh
saba certs dns providers
```

## saba certs dns remove

Forget the DNS provider and its credentials

```sh
saba certs dns remove [flags]
```

Aliases: `rm`

**Flags**

| Flag | Default | Description |
|---|---|---|
| `-y, --yes` |  | Confirm the removal |

## saba certs dns set

Configure the DNS provider (settings are checked before saving)

```sh
saba certs dns set <provider> [key=value...]
```

```text
Configure the DNS provider. Settings not given keep their current value when
the provider stays the same, so a token can be rotated alone.

The hook provider runs an executable on the server for any DNS host without
built-in support, as "<path> present|cleanup <record-name> <value>"; it must
create or remove that TXT record and exit 0.
```

## saba certs list

List stored and pending certificates

```sh
saba certs list
```

Aliases: `ls`

## saba certs remove

Delete a stored certificate

```sh
saba certs remove <id> [flags]
```

Aliases: `rm`

**Flags**

| Flag | Default | Description |
|---|---|---|
| `-y, --yes` |  | Confirm the removal |

## saba context

Manage the servers this CLI can talk to

```sh
saba context
```

```text
A context is a named server. saba reaches it over SSH, so anything that works
with "ssh <target>" works here (Host aliases from ~/.ssh/config included).

Which server a command uses, in order of precedence:
  1. --host / SABA_HOST         an SSH target, or "local"
  2. --context / SABA_CONTEXT   a named context
  3. context = "..."            in ./saba.toml
  4. the current context        (saba context use <name>)
  5. a daemon on this machine   (when running on the server itself)
```

Aliases: `ctx`

**Examples**

```sh
saba context add prod root@203.0.113.10
saba context use prod
saba context ls
saba -c staging status
```

Subcommands: [`add`](#saba-context-add), [`list`](#saba-context-list), [`remove`](#saba-context-remove), [`use`](#saba-context-use)

## saba context add

Add a server (the first one becomes the current context)

```sh
saba context add <name> <ssh-target> [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--use` |  | Make it the current context |

## saba context list

List contexts

```sh
saba context list
```

Aliases: `ls`

## saba context remove

Forget a context (the server is not touched)

```sh
saba context remove <name>
```

Aliases: `rm`

## saba context use

Set the current context

```sh
saba context use <name>
```

## saba cron

List and trigger an app's scheduled jobs

```sh
saba cron
```

```text
Cron jobs are declared in saba.toml and run in a fresh container of the app's
live image, with its environment and volumes. Schedules are in UTC.

  [[cron]]
  name = "cleanup"
  schedule = "0 3 * * *"
  command = "node scripts/cleanup.js"
```

Subcommands: [`list`](#saba-cron-list), [`run`](#saba-cron-run)

## saba cron list

List cron jobs and their last result

```sh
saba cron list
```

Aliases: `ls`

## saba cron run

Run a cron job now and print its output

```sh
saba cron run <job>
```

## saba daemon

Run the saba server daemon (proxy, deploy engine, API)

```sh
saba daemon [flags]
```

```text
Run the server side of saba in the foreground. Normally this is started by
systemd (see "saba daemon install" or "saba server setup").

For local experiments, run it unprivileged:
  saba daemon --data-dir ./.saba --socket ./.saba/saba.sock --http :8080 --tls off --base-domain localhost
  SABA_HOST=unix://$PWD/.saba/saba.sock saba status
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--acme-ca-file string` |  | Extra root certificate to trust for the ACME directory (private or test CAs) |
| `--acme-directory string` |  | Alternative ACME directory URL (e.g. Let's Encrypt staging) |
| `--acme-email string` |  | Contact email for Let's Encrypt (stored on first start) |
| `--base-domain string` |  | Base domain for app URLs; "auto" derives &lt;public-ip&gt;.sslip.io (stored on first start) |
| `--cluster-addr string` | `:7443` | Where nodes connect when --tls is off (with TLS they use the HTTPS port) |
| `--data-dir string` | `/var/lib/saba` | Directory for state, certificates and logs |
| `--dns-resolver stringSlice` |  | DNS server (host:port) to check DNS-01 records with, instead of the system's; repeatable |
| `--http string` | `:80` | HTTP listen address |
| `--https string` | `:443` | HTTPS listen address |
| `--socket string` | `/run/saba/saba.sock` | Unix socket for the control API |
| `--tls string` | `auto` | "auto" for Let's Encrypt HTTPS, "off" for plain HTTP only |

Subcommands: [`install`](#saba-daemon-install), [`uninstall`](#saba-daemon-uninstall)

## saba daemon install

Install and start the daemon as a systemd service (run on the server, as root)

```sh
saba daemon install [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--acme-ca-file string` |  | Extra root certificate to trust for the ACME directory (private or test CAs) |
| `--acme-directory string` |  | Alternative ACME directory URL (e.g. Let's Encrypt staging) |
| `--acme-email string` |  | Contact email for Let's Encrypt (stored on first start) |
| `--base-domain string` |  | Base domain for app URLs; "auto" derives &lt;public-ip&gt;.sslip.io (stored on first start) |
| `--cluster-addr string` | `:7443` | Where nodes connect when --tls is off (with TLS they use the HTTPS port) |
| `--data-dir string` | `/var/lib/saba` | Directory for state, certificates and logs |
| `--dns-resolver stringSlice` |  | DNS server (host:port) to check DNS-01 records with, instead of the system's; repeatable |
| `--http string` | `:80` | HTTP listen address |
| `--https string` | `:443` | HTTPS listen address |
| `--print-unit` |  | Print the systemd units instead of installing them |
| `--socket string` | `/run/saba/saba.sock` | Unix socket for the control API |
| `--tls string` | `auto` | "auto" for Let's Encrypt HTTPS, "off" for plain HTTP only |

## saba daemon uninstall

Stop and remove the systemd service (apps and data are kept)

```sh
saba daemon uninstall
```

## saba db

Create and link managed databases

```text
Run databases next to your apps. A database is a single-replica service with
a persistent volume and generated credentials, reachable by other apps on the
server at <name>:<port>. It is not exposed to the internet.

Supported engines: mariadb, mongo, mysql, postgres, redis.
```

Aliases: `database`

**Examples**

```sh
saba db create main --engine postgres
saba db link main -a api            # sets DATABASE_URL on api and redeploys it
saba db url main                    # print the connection URL
saba db backup schedule main @daily # automatic backups, newest 7 kept
saba db backup create main -o main.sql.gz
saba db restore main --backup latest --yes
saba destroy main --yes --volumes   # delete it, including data and backups
```

Subcommands: [`backup`](#saba-db-backup), [`create`](#saba-db-create), [`link`](#saba-db-link), [`list`](#saba-db-list), [`publish`](#saba-db-publish), [`restore`](#saba-db-restore), [`url`](#saba-db-url)

## saba db backup

Create, schedule, download and delete database backups

```text
Backups are compressed dumps (pg_dump, mysqldump, mariadb-dump, mongodump)
stored on the server in /var/lib/saba/backups/<database>/.

Scheduled backups are pruned to the newest --keep; manual ones are kept until
you delete them.

Configure S3-compatible storage once ("saba db backup s3 set") and every
backup is also uploaded there. Listing, downloading and restoring then work
from the bucket too, so the backups of a lost server are available on its
replacement.
```

**Examples**

```sh
saba db backup create main                    # dump now, keep it on the server
saba db backup create main -o main.sql.gz     # ...and download it
saba db backup schedule main @daily --keep 14 # automatic backups (UTC)
saba db backup schedule main off
saba db backup list main
saba db backup download main latest
saba db restore main --backup latest --yes
saba db restore main ./main.sql.gz --yes
saba db backup s3 set --bucket my-backups --region eu-central-1
```

Subcommands: [`create`](#saba-db-backup-create), [`delete`](#saba-db-backup-delete), [`download`](#saba-db-backup-download), [`list`](#saba-db-backup-list), [`s3`](#saba-db-backup-s3), [`schedule`](#saba-db-backup-schedule)

## saba db backup create

Back up a database now

```sh
saba db backup create <database> [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `-o, --output string` |  | Also download the backup to this file or directory (- for stdout) |

## saba db backup delete

Delete a stored backup

```sh
saba db backup delete <database> <id> [flags]
```

Aliases: `rm`

**Flags**

| Flag | Default | Description |
|---|---|---|
| `-y, --yes` |  | Confirm the deletion |

## saba db backup download

Download a stored backup (default: the latest)

```sh
saba db backup download <database> [id] [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `-o, --output string` |  | File or directory to write to (- for stdout; default: the current directory) |

## saba db backup list

List stored backups

```sh
saba db backup list <database>
```

Aliases: `ls`

## saba db backup s3

Mirror backups to S3-compatible storage

```sh
saba db backup s3
```

```text
Send every database backup to a bucket as well: AWS S3, Cloudflare R2,
Backblaze B2, MinIO, RustFS or anything else that speaks the S3 API.

Once set:
  - new backups (manual and scheduled) are uploaded right after they are made,
    and existing ones are uploaded in the background
  - "list", "download" and "restore" also see backups that exist only in the
    bucket, so a replacement server can restore the old server's backups
  - retention (--keep) and "delete" apply to the bucket too
  - "saba doctor" reports failed uploads

Objects are stored as <prefix>/<database>/<backup-id>.<format>.gz.
```

**Examples**

```sh
# AWS: endpoint is derived from the region; keys come from the environment
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... saba db backup s3 set --bucket my-backups --region eu-central-1

# Cloudflare R2
saba db backup s3 set --bucket my-backups --endpoint https://<account>.r2.cloudflarestorage.com \
    --region auto --access-key-id <id> --secret-access-key -      # secret read from stdin

saba db backup s3 show
saba db backup s3 remove
```

Subcommands: [`remove`](#saba-db-backup-s3-remove), [`set`](#saba-db-backup-s3-set), [`show`](#saba-db-backup-s3-show)

## saba db backup s3 remove

Stop uploading backups (objects in the bucket are kept)

```sh
saba db backup s3 remove
```

Aliases: `rm`

## saba db backup s3 set

Set (and verify) the bucket backups are uploaded to

```sh
saba db backup s3 set --bucket <name> [flags]
```

```text
Save the S3 destination. The server first proves the settings work by
writing, reading, listing and deleting a small object; nothing is saved if
that fails.

The access key and secret default to AWS_ACCESS_KEY_ID and
AWS_SECRET_ACCESS_KEY from this machine's environment. Pass "-" as the secret
to read it from stdin.
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--access-key-id string` |  | Access key id (default: $AWS_ACCESS_KEY_ID) |
| `--bucket string` |  | Bucket name |
| `--create-bucket` |  | Create the bucket if it does not exist |
| `--endpoint string` |  | Service URL (default: AWS S3 in --region) |
| `--path-style` |  | Force path-style (true) or virtual-hosted (false) addressing; default: automatic |
| `--prefix string` |  | Key prefix inside the bucket, e.g. saba/prod |
| `--region string` |  | Region (default us-east-1; often "auto" for non-AWS services) |
| `--secret-access-key string` |  | Secret access key, or - to read it from stdin (default: $AWS_SECRET_ACCESS_KEY) |

## saba db backup s3 show

Show the configured bucket

```sh
saba db backup s3 show
```

## saba db backup schedule

Show or set the automatic backup schedule

```sh
saba db backup schedule <database> [cron|off] [flags]
```

```text
Schedule automatic backups with a five-field cron expression or @hourly,
@daily, @weekly, @monthly (UTC). "off" disables them. Without a schedule
argument, the current policy is shown. The database is not restarted.
```

**Examples**

```sh
saba db backup schedule main @daily
saba db backup schedule main "0 */6 * * *" --keep 28
saba db backup schedule main off
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--keep int` |  | Number of scheduled backups to retain (default 7) |

## saba db create

Create a database

```sh
saba db create <name> [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--engine string` | `postgres` | Database engine: mariadb, mongo, mysql, postgres, redis |
| `--memory string` |  | Memory limit (e.g. 1g) |
| `--node string` |  | Server to run the database on (default: the primary) |
| `--publish string` |  | Also expose the database on the host, e.g. 5432:5432 (not recommended) |
| `--version string` |  | Image tag of the engine (default: a current stable release) |

## saba db link

Give an app the database's URL as an environment variable

```sh
saba db link <database> -a <app> [flags]
```

```text
Set a variable on the app that references the database's URL, e.g.
DATABASE_URL=${{main.URL}}, and roll out the app. The reference is resolved at
deploy time, so the app never stores a copy of the credentials.
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--as string` |  | Variable name to set (default: DATABASE_URL, REDIS_URL or MONGODB_URL) |
| `--no-deploy` |  | Save the variable without rolling out the app |

## saba db list

List databases

```sh
saba db list
```

Aliases: `ls`

## saba db publish

Expose a database on a host port, or stop exposing it

```sh
saba db publish <database> [host-port-spec...] [flags]
```

```text
Publish a database's port on the server, for example on the loopback
interface so you can reach it through an SSH tunnel, or on a private address
so apps on other nodes can reach it by name. The database restarts briefly.

Without a spec, the current setting is shown. --clear removes it.
```

**Examples**

```sh
saba db publish main 127.0.0.1:5432:5432     # then: ssh -N -L 5432:127.0.0.1:5432 <server>
saba db publish main 10.0.0.1:5432:5432      # reachable from other nodes as "main"
saba db publish main --clear
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--clear` |  | Stop publishing the database's port |

## saba db restore

Restore a database from a stored backup or a dump file

```sh
saba db restore <database> [file] [flags]
```

```text
Load a backup into a running database, replacing the data it contains.

The source is either a backup stored on the server (--backup <id>, or
"latest") or a local dump file, plain or gzip-compressed (- reads stdin).

For Postgres the restore runs in a single transaction: if it fails, the
database is left unchanged. This command requires --yes.
```

**Examples**

```sh
saba db restore main --backup latest --yes
saba db restore main ./main.sql.gz --yes
pg_dump ... | saba db restore main - --yes
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--backup string` |  | Restore this stored backup (an id, or "latest") |
| `-y, --yes` |  | Confirm replacing the database's data |

## saba db url

Print a database's connection URL and credentials

```sh
saba db url <name>
```

## saba deploy

Deploy a directory, a git repository or an image

```sh
saba deploy [path] [flags]
```

```text
Deploy an app with zero downtime.

The source is, in order of precedence:
  --image <ref>    a registry image
  --git <url>      a git repository, cloned and built on the server
  image = "..."    in saba.toml
  [path]           a directory (default "."), uploaded and built on the server

A directory is built with its Dockerfile. Without one, saba generates a build
for Node.js, Bun, Go, Python, Rust or static sites.

The new version is started next to the current one. Traffic switches only
after it passes its health check; the old version then drains and stops. If
anything fails, the current version keeps serving and the exit code is 5.

Settings come from saba.toml (see "saba init") and can be overridden by flags.
They are remembered by the server, so later deploys do not need to repeat them.
A list flag given once with an empty value clears the list: --domain= or --publish=.

The deployment runs on the server: Ctrl-C stops watching it, "saba cancel"
stops the deployment itself.
```

**Examples**

```sh
saba deploy                                  # deploy the current directory
saba deploy ./api -a api --port 8080
saba deploy -a web --image nginx:alpine --domain example.com
saba deploy -a api --git https://github.com/me/api --ref main
saba deploy -e DATABASE_URL=postgres://... --replicas 2
saba deploy --dry-run                        # validate and show the plan
saba deploy --json                           # final result as one JSON document
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--command string` |  | Override the image's default command |
| `--cpus string` |  | CPU limit per container (e.g. 0.5) |
| `-d, --detach` |  | Return as soon as the deployment is started |
| `--dockerfile string` |  | Path of the Dockerfile, relative to the source directory |
| `--domain string` |  | Domain to route to the app (repeatable; replaces the list) |
| `--dry-run` |  | Validate everything and print the plan without deploying |
| `-e, --env string` |  | Set an environment variable KEY=VALUE (repeatable) |
| `--env-file string` |  | Set environment variables from a dotenv file |
| `--events` |  | With --json: stream progress as NDJSON events instead of one final document |
| `--git string` |  | Deploy this git repository (cloned and built on the server) |
| `--health-path string` |  | HTTP path that must return 2xx/3xx before traffic is switched |
| `--image string` |  | Deploy this registry image instead of building source |
| `--internal` |  | Reachable from other apps only, not from the internet |
| `--memory string` |  | Memory limit per container (e.g. 512m) |
| `-m, --message string` |  | Note to attach to this deployment |
| `--node string` |  | Server to run replicas on: "local", a node name, or "*" for all (repeatable; replaces the list) |
| `--port int` |  | Port the app listens on (default: the image's EXPOSE) |
| `--publish string` |  | Expose a port on the host, e.g. 127.0.0.1:5432:5432 (repeatable; replaces the list) |
| `-q, --quiet` |  | Hide build and container output (shown only on failure) |
| `--ref string` |  | Branch, tag or commit to deploy with --git (default: the default branch) |
| `--release-command string` |  | Command to run before the new version gets traffic (e.g. migrations) |
| `--replicas int` |  | Number of containers to run |
| `--strategy string` |  | Deploy strategy: rolling (default) or recreate |
| `--volume string` |  | Persistent volume name:/path (repeatable; replaces the list) |
| `--worker` |  | The app does not serve HTTP (no routing or HTTP health check) |

## saba deployments

List an app's deployment history

```sh
saba deployments [app] [flags]
```

```text
List deployments, newest first. The ROLLBACK column shows which versions can
still be restored with "saba rollback --to <version>".
```

Aliases: `history`, `releases`

**Flags**

| Flag | Default | Description |
|---|---|---|
| `-n, --limit int` | `20` | Maximum number of deployments to show |

## saba destroy

Delete an app, its containers, images and history

```sh
saba destroy [app] [flags]
```

```text
Permanently delete an app. Named volumes are kept unless --volumes is given.

This command never prompts; it requires --yes.
```

Aliases: `rm`, `delete`

**Examples**

```sh
saba destroy old-api --yes
saba destroy old-db --yes --volumes   # also delete its data
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--volumes` |  | Also delete the app's named volumes (data loss) |
| `-y, --yes` |  | Confirm the deletion |

## saba doctor

Diagnose problems with the server and its apps

```sh
saba doctor
```

```text
Check the connection, Docker, disk space, DNS of routed domains and the
health of every app. Each failed check comes with a suggested fix. Exits with
code 1 if any check fails.
```

## saba domains

Manage the domains routed to an app

```sh
saba domains
```

```text
Manage custom domains. HTTPS certificates are issued automatically once a
domain's DNS A record points at the server.

An app without custom domains is served at <app>.<server base domain>.
```

**Examples**

```sh
saba domains -a web
saba domains add -a web example.com www.example.com
saba domains remove -a web www.example.com
```

Subcommands: [`add`](#saba-domains-add), [`list`](#saba-domains-list), [`remove`](#saba-domains-remove)

## saba domains add

Route domains to the app

```sh
saba domains add <domain>...
```

## saba domains list

List an app's domains

```sh
saba domains list
```

Aliases: `ls`

## saba domains remove

Stop routing domains to the app

```sh
saba domains remove <domain>...
```

Aliases: `rm`

## saba env

Manage environment variables and secrets

```sh
saba env
```

```text
Manage an app's environment variables. Values are stored on the server.

Changing variables rolls out a new version with zero downtime, unless
--no-deploy is given (then the change applies on the next deploy or restart).

A value can reference another app's variables with ${{app.KEY}}, e.g.
DATABASE_URL='${{db.URL}}' — it is resolved at deploy time.
```

**Examples**

```sh
saba env                                  # list variables of the app in ./saba.toml
saba env set -a api SECRET=abc LOG_LEVEL=debug
saba env set -a api --no-deploy FEATURE_X=1
saba env unset -a api FEATURE_X
saba env import -a api .env.production
saba env get -a api SECRET
```

Subcommands: [`get`](#saba-env-get), [`import`](#saba-env-import), [`list`](#saba-env-list), [`set`](#saba-env-set), [`unset`](#saba-env-unset)

## saba env get

Print the value of one variable

```sh
saba env get <KEY>
```

## saba env import

Set variables from a dotenv file (use - for stdin)

```sh
saba env import <file> [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--no-deploy` |  | Save the change without rolling out a new version |

## saba env list

List environment variables

```sh
saba env list [flags]
```

Aliases: `ls`

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--resolved` |  | Show values with ${{app.KEY}} references expanded |

## saba env set

Set variables and roll out the change

```sh
saba env set <KEY=VALUE>... [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--no-deploy` |  | Save the change without rolling out a new version |

## saba env unset

Remove variables and roll out the change

```sh
saba env unset <KEY>... [flags]
```

Aliases: `rm`, `remove`

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--no-deploy` |  | Save the change without rolling out a new version |

## saba exec

Run a command inside a running container

```sh
saba exec [-a app] -- <command> [args...] [flags]
```

```text
Run a command in a running replica of the app. Output is streamed and the
command's exit code becomes saba's exit code.

Use -t for an interactive terminal (a shell, a REPL). With --json, stdout,
stderr and the exit code are returned as one document.
```

**Examples**

```sh
saba exec -a api -- ls -la /app
saba exec -a api -t -- sh
saba exec -a db -- pg_dump -U postgres app > backup.sql
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--replica int` | `1` | Replica to run the command in |
| `-t, --tty` |  | Allocate an interactive terminal |

## saba guide

Print the complete reference (written for AI agents and scripts)

```sh
saba guide
```

```text
Print a compact, complete reference of saba: concepts, every command, the
saba.toml schema, JSON output shapes, error codes and exit codes.

It is the fastest way for an AI agent to learn how to drive saba.
```

## saba init

Create a saba.toml for a project

```sh
saba init [path] [flags]
```

```text
Write a commented saba.toml that pins the app's name and settings. It is
optional: "saba deploy" works without it, taking the app name from the
directory and everything else from flags and what the server remembers.
```

**Examples**

```sh
saba init
saba init -a api --port 8080
saba init -a cache --image redis:7
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--force` |  | Overwrite an existing saba.toml |
| `--image string` |  | Deploy this image instead of building the directory |
| `--port int` |  | Port the app listens on |

## saba logs

Show app logs, or the log of a deployment

```sh
saba logs [app] [flags]
```

```text
Show the output of an app's running containers.

By default the last 100 lines are printed and the command exits; use --follow
to keep streaming. With --deployment, the build and rollout log of that
deployment is shown instead (use "latest" for the most recent one).

With --json, each line is one JSON event (NDJSON).
```

**Examples**

```sh
saba logs                          # last 100 lines of the app in ./saba.toml
saba logs api -n 500 --since 1h
saba logs api -f                   # stream until interrupted
saba logs api --deployment latest  # why did the last deploy fail?
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--deployment string` |  | Show the log of a deployment instead (e.g. v3 or latest) |
| `-f, --follow` |  | Keep streaming new output |
| `--replica int` |  | Only show this replica (default: all) |
| `--since string` |  | Only show output newer than this (e.g. 10m, 2h, or an RFC 3339 time) |
| `-n, --tail int` | `100` | Number of most recent lines to show |
| `-t, --timestamps` |  | Prefix each line with its timestamp |

## saba mcp

Run a Model Context Protocol server on stdio

```sh
saba mcp
```

```text
Expose saba as MCP tools so AI agents can deploy and operate apps.

Each tool maps to a CLI command and returns its --json output. The server
uses the same server selection as the CLI (--host, --context, saba.toml,
current context), fixed at startup.

Example client configuration:
  { "mcpServers": { "saba": { "command": "saba", "args": ["mcp"] } } }
  { "mcpServers": { "saba": { "command": "saba", "args": ["mcp", "--context", "prod"] } } }
```

## saba node

Add servers to scale apps across machines

```sh
saba node
```

```text
Spread an app's replicas across several servers.

The server running the saba daemon is the primary (node "local"). Every other
node runs the saba agent, which connects out to the primary over TLS. That one
encrypted connection carries everything: Docker control and the traffic to the
node's replicas. Nodes need Docker and no open ports, so they can sit behind
NAT; the primary only needs its HTTPS port reachable from them.

Place an app with "nodes" in saba.toml or --node on deploy:

  nodes = ["local", "worker1"]     # or ["*"] for every node
  replicas = 4                     # spread round-robin: 2 per server

Good to know:
  - Apps with volumes and databases run on a single node.
  - Containers reach each other by name only on the same server. An app that
    publishes a port (publish = ["10.0.0.1:5432:5432"]) is also reachable by
    name from other nodes, at its server's address (--address).
```

Aliases: `nodes`

**Examples**

```sh
saba node add worker1 root@203.0.113.20     # install the agent over SSH
saba node add homelab                       # print a join command to run on the node
saba node list
saba deploy -a web --node local --node worker1 --replicas 4
saba node remove worker1 --yes
```

Subcommands: [`add`](#saba-node-add), [`list`](#saba-node-list), [`remove`](#saba-node-remove)

## saba node add

Add a server to the cluster

```sh
saba node add <name> [user@host] [flags]
```

```text
Add a server as a node. With an SSH destination, saba installs everything:
it connects to the server, checks Docker (installing it with
--install-docker), installs the saba binary and the agent service, and waits
for the agent to connect.

Without a destination, saba prints a join command to run on the server
yourself. Use that for machines you cannot reach over SSH from here, such as a
home server behind NAT. The join code works once and expires after 24 hours.

The agent connects to the primary at --primary-address, by default the
primary's "address" setting or its public IP, on its HTTPS port (443).

Running it again for a joined node updates its --address. --rejoin issues a new
join code for a reinstalled machine.
```

**Examples**

```sh
saba node add worker1 root@203.0.113.20
saba node add worker1 deploy@10.0.0.2 --address 10.0.0.2 --primary-address 10.0.0.1
saba node add homelab
saba node add worker1 root@203.0.113.20 --rejoin
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--address string` |  | The node's IP on the network shared with the other servers (for cross-node access to published ports) |
| `--binary string` |  | Upload this local Linux binary instead of downloading a release (for unreleased builds) |
| `--install-docker` |  | Install Docker on the node if it is missing |
| `--primary-address string` |  | Address the node uses to reach the primary (default: the primary's address setting, or its public IP) |
| `--rejoin` |  | Issue a new join code and forget the node's previous key |

## saba node list

List the cluster's servers

```sh
saba node list
```

Aliases: `ls`

## saba node remove

Remove a server from the cluster

```sh
saba node remove <name> [flags]
```

```text
Remove a node. It refuses while apps are placed on the node. When the agent
was installed over SSH, it is uninstalled the same way unless --keep-agent is
given. Containers, images and volumes on the node are not touched.
```

Aliases: `rm`

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--keep-agent` |  | Do not uninstall the agent from the node |
| `-y, --yes` |  | Confirm the removal |

## saba restart

Restart an app with zero downtime (applies pending config)

```sh
saba restart [app]
```

```text
Roll out fresh containers of the current image, using the app's deploy
strategy. Saved-but-undeployed config and env changes take effect.
```

## saba rollback

Roll back to the previous (or a specific) version

```sh
saba rollback [app] [flags]
```

```text
Redeploy an earlier version: its image, config and environment. The rollback
is itself a zero-downtime deployment and gets a new version number.

Without --to, the most recent previously-live version is used. Run
"saba deployments" to see which versions can be restored.
```

**Examples**

```sh
saba rollback api
saba rollback api --to v12
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--to string` |  | Version to restore (default: the previous live version) |

## saba run

Run a one-off command in a fresh container of the app

```sh
saba run [-a app] -- <command> [args...]
```

```text
Start a new container from the app's live image, with its environment and
volumes, run the command to completion and remove the container. Useful for
migrations, consoles and scripts that should not run inside a serving replica.
```

**Examples**

```sh
saba run -a api -- npm run migrate
saba run -a api -- node scripts/backfill.js
```

## saba scale

Change the number of replicas

```sh
saba scale [app] <replicas>
```

**Examples**

```sh
saba scale api 3
saba scale 2            # app from ./saba.toml
```

## saba server

Set up and inspect servers

Subcommands: [`config`](#saba-server-config), [`info`](#saba-server-info), [`prune`](#saba-server-prune), [`setup`](#saba-server-setup)

## saba server config

Show or change server settings

```sh
saba server config
```

```text
Server settings:
  base_domain     apps without custom domains are served at <app>.<base_domain>
  acme_email      contact address for Let's Encrypt (applies after a daemon restart)
  keep_versions   old versions kept per app for rollbacks (default 5)
  address         this server's address on the network shared with other nodes

The DNS provider for wildcard certificates is configured with `saba certs dns`.
```

**Examples**

```sh
saba server config
saba server config set base_domain apps.example.com
```

Subcommands: [`set`](#saba-server-config-set)

## saba server config set

Change a server setting

```sh
saba server config set <key> <value>
```

## saba server info

Show the server's version and configuration

```sh
saba server info
```

## saba server prune

Free disk space (dangling images and old build cache)

```sh
saba server prune
```

## saba server setup

Install (or upgrade) saba on a server over SSH

```sh
saba server setup <user@host> [flags]
```

```text
Prepare a fresh Linux server:

  1. check for Docker (install it with --install-docker)
  2. download saba from GitHub releases to /usr/local/bin/saba
  3. install and start the saba daemon as a systemd service
  4. save the server as a context on this machine

The server downloads the build for its own architecture, so it does not matter
what machine you run this from. By default it installs the same version as this
CLI (the latest release for development builds); choose another with --version.

Requirements: SSH access as root or a user with passwordless sudo, and ports
80 and 443 free. Running it again upgrades saba in place; apps keep running.
```

**Examples**

```sh
saba server setup root@203.0.113.10
saba server setup deploy@example.com --name prod --email ops@example.com --install-docker
saba server setup root@host --base-domain apps.example.com   # needs a wildcard DNS record
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--base-domain string` | `auto` | Base domain for app URLs ("auto" = &lt;public-ip&gt;.sslip.io) |
| `--binary string` |  | Upload this local Linux binary instead of downloading a release (for unreleased builds) |
| `--email string` |  | Contact email for Let's Encrypt certificates |
| `--install-docker` |  | Install Docker with the official script if it is missing |
| `--name string` |  | Context name to save the server as (default: derived from the host) |
| `--no-context` |  | Do not save the server as a context |
| `--tls string` | `auto` | "auto" for Let's Encrypt HTTPS, "off" for plain HTTP |
| `--version string` |  | Release to install, e.g. v1.2.3 or "latest" (default: this CLI's version) |

## saba start

Start a stopped app

```sh
saba start [app]
```

## saba stats

Show CPU and memory usage of running containers

```sh
saba stats [app]
```

## saba status

Show the state of an app (or of all apps)

```sh
saba status [app]
```

```text
Show what is running: version, replicas, URLs, containers and recent deployments.

Without an app (and outside a project directory) it lists all apps.
```

**Examples**

```sh
saba status              # app from ./saba.toml, or all apps
saba status api
saba status api --json   # full machine-readable state
```

## saba stop

Stop an app's containers (keeps config, data and versions)

```sh
saba stop [app]
```

## saba version

Print the CLI version (and the server's, if reachable)

```sh
saba version
```
