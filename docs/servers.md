# Servers

How a server is set up and upgraded, how the CLI finds and reaches it, and what saba puts on
it.

## Requirements

- Linux with systemd, on x86-64 (`amd64`) or ARM64.
- Docker, or `--install-docker` to install it with Docker's official script.
- Ports 80 and 443 free and reachable from the internet (open them in your cloud firewall),
  plus SSH.
- SSH access as root, or as a user with passwordless `sudo`.

## Setting up a server

```sh
saba server setup root@203.0.113.10 [--name prod] [--email you@example.com] [--install-docker]
```

Step by step:

1. **Probe**: connects over SSH and checks the OS, architecture, whether you are root or have
   passwordless sudo, and whether Docker and systemd are present.
2. **Docker**: installs it from `https://get.docker.com` if it is missing and you passed
   `--install-docker`; otherwise stops and says so.
3. **Binary**: the server downloads saba for its own architecture from GitHub releases to
   `/usr/local/bin/saba`, verifies it against the release's checksums and checks that it runs
   before replacing an existing binary. The version is the same as your CLI's, or the latest
   release if your CLI is a development build. `--version v0.3.0` picks another.
4. **Service**: runs `saba daemon install`, which writes the systemd units, enables them and
   starts the daemon. When you connect as a non-root user, that user is also added to the
   `docker` group, which is what lets it reach the daemon without `sudo` from then on (it already
   has root through passwordless `sudo`, so this grants nothing new).
5. **Context**: waits for the daemon to answer, saves the server as a context on your machine and
   makes it the current one. `--name` names it; by default the name comes from the host (`web1`
   for `web1.example.com`, `prod` for an IP address). `--no-context` skips this step.

Other options:

| Flag | Effect |
|---|---|
| `--base-domain apps.example.com` | Base domain for app URLs. Default `auto`: `<public-ip>.sslip.io` |
| `--tls off` | Serve plain HTTP on port 80 only, e.g. behind another load balancer |
| `--binary ./dist/saba-linux-amd64` | Upload this build instead of downloading a release (for unreleased versions) |

The base domain and email are stored on the first start only; change them later with
`saba server config set`.

## Upgrading

Upgrade the CLI, then run setup again:

```sh
curl -fsSL https://raw.githubusercontent.com/egoist/saba/main/install.sh | sh
saba server setup root@203.0.113.10
```

Setup is idempotent: it replaces the binary and restarts the daemon. Apps keep running
throughout, because containers belong to Docker, not to the daemon. The restart itself does not
refuse or drop requests: ports 80 and 443 are held by systemd (see [socket
activation](#the-daemon-and-systemd)), new connections wait in the kernel's queue for the moment
it takes the new daemon to start, and requests in flight on the old one get up to 30 seconds to
finish.

Two exceptions:

- A deployment in progress at that moment is marked failed when the daemon comes back. Run it
  again.
- Upgrading a server set up with v0.1.0, which predates socket activation, moves the ports to
  systemd once, with a brief interruption.

## Contexts

A context is a name on your machine for a server. Commands use one in this order:

1. `--host user@host` or `SABA_HOST`: an SSH destination, `local`, or `unix:///path/to.sock`,
2. `--context name` or `SABA_CONTEXT`,
3. `context = "name"` in the project's `saba.toml`,
4. the current context,
5. a daemon on this machine, when there is a socket at `/run/saba/saba.sock`.

```sh
saba context add staging deploy@staging.example.com
saba context use staging
saba context                     # list; * marks the current one
saba -c prod status              # one command against another context
saba context remove staging      # forgets it; the server is not touched
```

Contexts are stored in a TOML file described in [Configuration](configuration.md#the-context-file).
Each server is independent: apps, databases and settings on `staging` are unrelated to those on
`prod`.

## How the CLI reaches a server

The daemon listens on a unix socket only, `/run/saba/saba.sock`. For each request the CLI runs:

```sh
ssh <destination> saba dial-stdio
```

which connects SSH's input and output to that socket. Consequences:

- Anything your `ssh` can do works: keys and agents, `~/.ssh/config` host aliases, `ProxyJump`
  jump hosts, hardware keys.
- A destination can be `user@host`, a host alias, or `ssh://user@host:2222` for another port.
- The CLI shares one SSH connection between commands (OpenSSH `ControlMaster`, kept for two
  minutes), so only the first command pays for the handshake. `SABA_SSH_MUX=0` turns that off.
- When the CLI runs without a terminal (scripts, CI, agents), SSH runs in batch mode: it fails
  instead of prompting for a password or an unknown host key.
- `SABA_SSH_OPTS` adds options to every SSH invocation:

  ```sh
  export SABA_SSH_OPTS="-i ~/.ssh/deploy_key -o StrictHostKeyChecking=accept-new"
  ```

On the server itself, commands talk to the socket directly. The socket belongs to root and the
`docker` group, so root and `docker` group members can use it; nobody else can.

## Server settings

```sh
saba server info                                  # version, Docker, TLS mode, settings
saba server config                                # all settings
saba server config set base_domain apps.example.com
```

| Key | Description |
|---|---|
| `base_domain` | Apps without custom domains are served at `<app>.<base_domain>` |
| `acme_email` | Contact address for Let's Encrypt; applies after a daemon restart |
| `keep_versions` | Superseded versions per app that keep their images for rollbacks (default 5) |
| `address` | This server's address on the network shared with nodes. See [Scaling](scaling.md#talking-between-apps-on-different-servers) |
| `cloudflare_api_token` | Enables wildcard certificates for Cloudflare zones. Pass `-` to read it from stdin |
| `dns_hook` | Executable that manages DNS-01 records for other DNS hosts |

The S3 destination for backups is set with `saba db backup s3 set`. Secrets are never printed in
full.

## The daemon and systemd

`saba daemon install` writes two units:

- **`saba.socket`** owns ports 80 and 443 (`ListenStream=80`, `ListenStream=443`).
- **`saba.service`** runs `saba daemon` and receives those sockets from systemd.

Because systemd holds the listening sockets, they stay open while the daemon restarts. That is
what makes upgrades seamless. The service uses `KillMode=process`, so stopping it never stops
app containers.

```sh
systemctl status saba
journalctl -u saba -f                  # the daemon's own log
saba daemon install --print-unit       # print the units instead of installing them
saba daemon uninstall                  # stop and remove the service; apps and data stay
```

For verbose logs, run `systemctl edit saba`, add the lines below, then `systemctl restart saba`:

```ini
[Service]
Environment=SABA_DEBUG=1
```

### Daemon flags

`saba daemon` runs in the foreground. These are the flags the service is installed with:

| Flag | Default | Description |
|---|---|---|
| `--data-dir` | `/var/lib/saba` | State, certificates, logs and backups |
| `--socket` | `/run/saba/saba.sock` | Control socket |
| `--http`, `--https` | `:80`, `:443` | Listen addresses, when not passed by systemd |
| `--tls` | `auto` | `auto` for Let's Encrypt, `off` for plain HTTP |
| `--base-domain` | — | Seeds `base_domain` on the first start; `auto` derives `<public-ip>.sslip.io` |
| `--acme-email` | — | Seeds `acme_email` on the first start |
| `--acme-directory` | Let's Encrypt | Another ACME CA, e.g. the Let's Encrypt staging environment |
| `--acme-ca-file` | — | Extra root certificate to trust for a private ACME CA |

### Running a daemon locally

For development or a quick try, without root, HTTPS or systemd:

```sh
saba daemon --data-dir ./.saba --socket ./.saba/saba.sock --http :8080 --tls off --base-domain localhost
SABA_HOST=unix://$PWD/.saba/saba.sock saba status
```

## What saba puts on a server

| Path | Contents |
|---|---|
| `/usr/local/bin/saba` | The binary |
| `/etc/systemd/system/saba.socket`, `saba.service` | The service |
| `/run/saba/saba.sock` | Control socket |
| `/run/saba/nodes/` | SSH control sockets and forwarded Docker sockets for [nodes](scaling.md) |
| `/var/lib/saba/state.json` | All state: apps, configuration, **environment variables including secrets**, deployment history, nodes, settings. Root only |
| `/var/lib/saba/logs/<app>/v<N>.ndjson` | Deployment logs |
| `/var/lib/saba/backups/<database>/` | Database backups |
| `/var/lib/saba/certs/` | Issued certificates and ACME account keys |
| `/var/lib/saba/certs/store/` | Wildcard and uploaded certificates |
| `/var/lib/saba/cluster/` | The key the primary uses to reach nodes, and their pinned host keys |
| `/var/lib/saba/tmp/` | Uploads and builds in progress; emptied on start |

In Docker:

| Object | Name |
|---|---|
| Network | `saba` |
| App containers | `saba-<app>-v<version>-<replica>` |
| One-off containers | `saba-<app>-release-v<version>`, `saba-<app>-run-<id>`, `saba-<app>-cron-<job>-<id>` |
| Images | `saba/<app>:v<version>` |
| Volumes | `saba-<app>-<volume>` |

Containers carry `saba.managed`, `saba.app`, `saba.version`, `saba.replica` and `saba.role`
labels. On start, the daemon removes containers of versions that are no longer live, but never
touches containers of apps it does not know about.

**Back up `/var/lib/saba/state.json`** if you want to be able to rebuild a server's apps exactly;
it is small. It contains secrets, so store it accordingly.

On your own machine, saba keeps contexts in its config file and SSH connection sockets in
`~/.cache/saba/ssh/` (`~/Library/Caches/saba/ssh/` on macOS).

## Moving to a new server

There is no one-step migration. The reliable path:

1. `saba server setup` the new server.
2. Restore databases from S3 backups (see [disaster recovery](databases.md#disaster-recovery)).
3. Deploy each app to it (`saba -c new deploy` in each project), set their secrets.
4. Switch DNS to the new server, then retire the old one.

## Removing saba from a server

```sh
saba daemon uninstall                    # on the server, as root
docker ps -aq --filter label=saba.managed=true | xargs -r docker rm -f
rm -rf /var/lib/saba /usr/local/bin/saba
```

This deletes every app's containers and saba's data, including backups on the server. Volumes
(`docker volume ls`) are left for you to remove.
