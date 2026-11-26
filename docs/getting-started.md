# Getting started

This guide takes you from nothing to a running app with HTTPS, a database and a rollback. You
need:

- **A Linux server** with systemd and ports 80 and 443 free. Any VPS works: 1 GB of memory is
  enough to start. Ubuntu, Debian, Fedora and most other distributions are fine.
- **SSH access** to it as root, or as a user with passwordless `sudo`, using a key. Check that
  `ssh root@203.0.113.10 true` works without a password prompt.
- **A machine to deploy from**: macOS or Linux.

You do not need a domain name yet. You do not need Docker on your own machine.

## 1. Install the CLI

```sh
curl -fsSL https://raw.githubusercontent.com/egoist/saba/main/install.sh | sh
```

This downloads the latest release for your OS and architecture to `/usr/local/bin`, or
`~/.local/bin` when that is not writable. Set `SABA_INSTALL_DIR` to choose another directory,
or `SABA_VERSION=v0.1.0` to pin a version. With Go installed,
`go install github.com/egoist/saba/cmd/saba@latest` works too.

```sh
saba version
```

## 2. Set up the server

```sh
saba server setup root@203.0.113.10 --install-docker --email you@example.com
```

This connects over SSH and:

1. checks the server runs Linux with systemd, and installs Docker if it is missing (only with
   `--install-docker`),
2. downloads the saba binary for the server's architecture from GitHub releases, the same
   version as your CLI, and verifies its checksum,
3. installs the daemon as a systemd service and starts it,
4. saves the server as a [context](servers.md#contexts) on your machine and selects it.

`--email` is the contact address for Let's Encrypt and is optional. When it finishes:

```text
✓ saba v0.1.0 is running on root@203.0.113.10
  Saved as context "prod" and selected it.
  Apps will be served at https://<app>.203-0-113-10.sslip.io
```

That last line is the **base domain**. Every app gets `https://<app>.<base domain>` automatically.
saba picks `<your-ip>.sslip.io`, a public DNS service that resolves any name under it to the
IP in the name, so URLs and certificates work without touching DNS. You can switch to your own
domain later.

Check the result:

```sh
saba server info
saba doctor
```

## 3. Deploy an app

Go to a project directory. saba builds it with its `Dockerfile` if it has one; otherwise it
detects Node.js, Bun, Go, Python, Rust and static sites and generates a build. For a quick test,
make a tiny static site:

```sh
mkdir hello && cd hello
echo '<h1>Hello from saba</h1>' > index.html
saba deploy
```

```text
▶ Deploying hello → root@203.0.113.10
  upload   Uploading source (1 file, 124 B)
  build    Building image (static)
  start    Starting 1 replica(s) of v1
  health   Waiting for v1 to become healthy (HTTP on port 80, timeout 1m0s)
  switch   Routing traffic to v1
✓ hello v1 is live (4.2s)
  https://hello.203-0-113-10.sslip.io
```

The app is named after the directory. Open the URL: the first HTTPS request triggers a
certificate from Let's Encrypt, which takes a few seconds once.

Change the page and deploy again:

```sh
echo '<h1>Hello again</h1>' > index.html
saba deploy
```

Version 2 starts next to version 1. Traffic moves over only after version 2 passes its health
check, and version 1 finishes its in-flight requests before it stops. Nobody visiting the site
sees an error. If version 2 had failed to start, version 1 would have kept serving and the command
would have exited with code 5 and the reason.

## 4. Look around

```sh
saba apps                  # every app on the server
saba status hello          # version, URL, containers, recent deployments
saba logs hello            # last 100 lines of output (add -f to follow)
saba deployments hello     # history; the ROLLBACK column shows what can be restored
```

`saba deploy` named the app after the directory, but other commands need to be told which app
you mean: as an argument, with `-a hello`, or through a `saba.toml`, which the next step creates.
From then on, commands run in this directory know the app.

## 5. Pin the configuration in saba.toml

Settings can come from flags, but a `saba.toml` in the project keeps them in version control:

```sh
saba init
```

This writes a commented file with the app name and detected port. A typical one:

```toml
name = "api"
port = 3000
replicas = 2
release_command = "npm run migrate"

[health]
path = "/healthz"

[env]
NODE_ENV = "production"
```

See [Configuration](configuration.md) for every key.

## 6. Add a secret

Secrets do not belong in `saba.toml`. Set them on the server:

```sh
saba env set STRIPE_KEY=sk_live_...
```

This stores the value and rolls out a new version that has it, again with no downtime. `saba env`
lists the variables. See [Environment variables](environment.md).

## 7. Add a database

```sh
saba db create main --engine postgres
saba db link main -a hello
```

The first command starts Postgres with a persistent volume and a generated password. The second
gives `hello` a `DATABASE_URL` that points at it and redeploys `hello`. The database is reachable
from your apps at `main:5432` and never from the internet.

Turn on daily backups, and ideally copy them off the server:

```sh
saba db backup schedule main @daily
saba db backup s3 set --bucket my-backups --region eu-central-1    # uses AWS_* env vars
```

See [Databases and backups](databases.md).

## 8. Use your own domain

Create a DNS `A` record for `www.example.com` pointing at the server's IP, then:

```sh
saba domains add www.example.com
```

The certificate is issued on the first HTTPS request. Custom domains replace the automatic one.
See [Domains and HTTPS](domains-and-https.md), including wildcard certificates.

## 9. Roll back

Something went wrong in production?

```sh
saba rollback
```

This redeploys the previous version: its image, configuration and environment, with the same
zero-downtime switch. `saba rollback --to v3` picks a specific version.

## Try it without a server

The daemon runs fine on a laptop with Docker, without root, HTTPS or a domain:

```sh
saba daemon --data-dir ./.saba --socket ./.saba/saba.sock --http :8080 --tls off --base-domain localhost &
export SABA_HOST=unix://$PWD/.saba/saba.sock

saba deploy -a hello --image nginx:alpine
curl http://hello.localhost:8080
```

Most browsers and `curl` resolve `*.localhost` to your machine.

## Next steps

- [Concepts](concepts.md): the model behind everything above.
- [Deploying](deploying.md): health checks, release commands, strategies, builds.
- [Scaling](scaling.md): more replicas, more servers.
- [Automation and AI agents](automation.md): CI/CD and the MCP server.
