# Concepts

A handful of ideas explain nearly all of saba's behavior.

## One binary, two roles

The same `saba` binary is:

- **the daemon**, running on each server you set up as a primary. It owns ports 80 and 443
  through a built-in reverse proxy, drives Docker, runs deployments, cron jobs and backups, and
  keeps the platform's state in one file.
- **the CLI**, running wherever you are. It sends requests to the daemon.

The daemon opens no network port of its own. Its API is a unix socket on the server. The CLI
reaches it by running `ssh <server> saba dial-stdio`, which connects SSH's stdin and stdout to
that socket. So if `ssh <server>` works, saba works, and SSH is the only access control. See
[Servers](servers.md#how-the-cli-reaches-a-server).

## Apps

An **app** is a named service: `web`, `api`, `worker`. A name is 1 to 40 lowercase letters,
digits and dashes, starting with a letter, because it doubles as a DNS label and a container
hostname.

An app has:

- **configuration**: the image or build, port, domains, replicas, health check, resources,
  volumes, cron jobs and so on ([reference](configuration.md)),
- **environment variables**, including secrets ([guide](environment.md)),
- **deployments**: its version history.

Apps reach each other by name over a private Docker network: `http://api:8080`. A **database**
is an app of a special kind, created with `saba db create`: single replica, persistent volume,
generated credentials, never exposed to the internet.

## Versions and deployments

Every change to what runs creates a new numbered **version**: `v1`, `v2`, and so on. That covers
a deploy, a restart, a scale, an environment change, a domain change and a rollback. Each
version has its own containers (`saba-<app>-v<N>-<replica>`). Versions that build or pull get
their own image tag (`saba/<app>:v<N>`); a restart, scale or configuration change reuses the
image of the version before it, and a rollback the image of its target. Exactly one version is
**live** at a time.

A deployment's status is one of:

| Status | Meaning |
|---|---|
| `building` | Building or pulling the image |
| `deploying` | Running the release command, starting containers, waiting for health checks |
| `live` | Serving traffic |
| `superseded` | Was live, replaced by a newer version. Restorable while its image is kept |
| `failed` | Never went live. The error is recorded with the deployment |

## Deploys are atomic

A deploy either fully succeeds or changes nothing:

1. The image is built or pulled.
2. The **release command**, if any (database migrations, say), runs to completion in a one-off
   container.
3. New containers start **next to** the old ones.
4. Every new replica must pass its **health check**.
5. Traffic switches to the new version in one step.
6. The old version **drains**: requests already in flight finish. Then it stops.

If anything fails before step 5, the new containers are removed, the old version never stops
serving, and the app's saved configuration and environment are left exactly as they were. The
error explains what happened and includes the container's last output. Details in
[Deploying](deploying.md).

## Configuration is remembered

Settings sent with a deploy, from flags or `saba.toml`, are stored on the server with the
version that used them. Later deploys only need to say what changes: `saba deploy --replicas 3`
keeps every other setting. A rollback restores the configuration and environment of the version
it returns to, not only its code.

Because of this, deleting a key from `saba.toml` does **not** reset it on the server. To clear a
setting, set it to an empty value. See
[how settings are merged](configuration.md#how-settings-are-merged).

## App status

`saba status` and `saba apps` derive a status from the live version and its containers:

| Status | Meaning |
|---|---|
| `created` | Exists, never successfully deployed |
| `deploying` | A deployment is in progress |
| `running` | Every replica of the live version is running |
| `degraded` | Some replicas are down or restarting |
| `crashed` | No replica of the live version is running |
| `stopped` | Stopped on purpose with `saba stop` |
| `failed` | Never deployed successfully, and the last attempt failed |

## Servers, nodes and contexts

- A **server** set up with `saba server setup` runs the daemon. To you it is a **context**: a
  name on your machine (`prod`, `staging`) pointing at an SSH destination. Contexts keep
  separate environments separate; each server has its own apps.
- A server can grow into a cluster: its daemon is the **primary** (node `local`), and you add
  **worker nodes**, Docker hosts running the saba agent, to run more replicas. The primary still
  terminates all traffic and holds all state. See [Scaling](scaling.md).

## The proxy

All HTTP(S) traffic enters through the daemon's proxy, which routes by hostname to the live
replicas of an app. Because saba owns the proxy, it knows the exact moment a new version is
ready, can switch traffic atomically, can count in-flight requests to drain them, and can hold
requests briefly while a single-instance app is replaced. This is what makes zero-downtime
deploys reliable rather than best-effort. See [Architecture](architecture.md#the-proxy).

## What saba is not

- **Not a cluster scheduler.** You choose which servers run an app; saba does not move replicas
  around on its own or provide failover for the primary.
- **Not a build service.** Images are built on the server with Docker (BuildKit).
- **Not multi-tenant.** Anyone who can SSH into the server as root or a `docker` group member
  controls everything on it. See [Security](security.md).
