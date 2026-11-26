# Security

What saba trusts, what it exposes, where it keeps secrets, and how to harden a server.

## The trust model

**SSH is the access control.** The daemon has no users, passwords or tokens. Anyone who can
reach its control socket can do anything saba can do, and the socket is open only to root and
members of the `docker` group, who can already control every container on the machine. So saba
adds no new way in: controlling saba requires exactly the access that controlling Docker on the
server does.

In practice, whoever has an SSH key accepted for root (or for a user in the `docker` group, or
with sudo) on a server controls all apps on it, their secrets and their data. There are no
per-user or per-app permissions. Give separate teams or environments separate servers.

A **primary controls its nodes**: it holds a private key accepted by each node, for a user that
can run Docker there. Compromising the primary compromises the cluster.

## What is exposed

| Port | What |
|---|---|
| 22 | Your SSH server, as configured. saba does not change it |
| 80 | HTTP: ACME challenges, redirects to HTTPS |
| 443 | HTTPS for routed apps |

Nothing else: the control API is a unix socket, databases and internal apps are reachable only
from containers on the server, and replica ports are bound to the loopback interface. Exceptions
you create yourself:

- `publish` ports, which bind wherever you tell them to,
- nodes in direct mode, whose replica ports are open on their private address.

Requests for hostnames not routed to an app get a 404 and no certificate.

## Secrets

Environment variables, database passwords, the Cloudflare token and the S3 secret are stored in
`/var/lib/saba/state.json`, mode `0600`, owned by root. They are **not encrypted at rest**:
anyone with root on the server can read them, which matches the trust model above (root can read
any container's environment anyway).

- Values are given to containers through Docker's environment, never on a command line, so they
  do not show up in `ps`.
- `saba status`, `saba apps`, `saba deployments`, `saba server info`, `saba server config` and
  `saba db backup s3 show` never print secrets. `saba env`, `saba db url` and the database
  outputs do, because that is their purpose.
- The Cloudflare token and the S3 secret can be passed with `-` to read them from stdin, keeping
  them out of shell history.
- Database passwords are 128 random bits. Apps linked with `saba db link` hold a reference, not a
  copy.

Back up `state.json` as carefully as the secrets in it.

## Transport

- CLI to server: SSH, with your keys and your host-key checking.
- Primary to nodes: SSH with a key generated on the primary, verifying the host keys read during
  `saba node add` (or trusting the first key seen with `--no-authorize`). Replica traffic goes
  through that SSH connection unless a node is in direct mode, which is plain HTTP over the
  private network; only private addresses are accepted for it.
- Clients to apps: TLS 1.2 or newer, with HSTS (`max-age=31536000`). HTTP requests to routed
  hosts are redirected to HTTPS.
- To S3: HTTPS, unless you configure an `http://` endpoint. Requests are signed with AWS
  Signature Version 4.

## Builds and images

Builds run on the server with Docker. A Dockerfile can do anything Docker allows during a build,
so deploying a repository means trusting its Dockerfile, just as running it means trusting its
code. Uploaded archives are unpacked with path checks, so a malicious archive cannot write
outside its build directory.

The release binary that `saba server setup` installs is downloaded from GitHub releases by the
server and checked against the release's `checksums.txt` before it replaces anything.

## Backups

Dumps contain all of a database's data and are not encrypted by saba. On the server they are in
`/var/lib/saba/backups/`, root only. In S3:

- enable server-side encryption on the bucket,
- give saba a key scoped to that bucket (see the [policy](databases.md#providers)),
- consider object versioning or object lock: the server holds credentials that can delete
  objects, so a compromised server could delete its own off-site backups otherwise.

## Hardening checklist

- [ ] SSH: key authentication only (`PasswordAuthentication no`), and a dedicated deploy key for
      CI, ideally restricted with `from=` in `authorized_keys`.
- [ ] A firewall allowing only 22, 80 and 443 from the internet (and the private network for
      nodes in direct mode).
- [ ] Unattended security updates for the OS; keep Docker updated.
- [ ] Never `publish` a database on a public interface; use `127.0.0.1:` and an SSH tunnel.
- [ ] Resource limits (`[resources]`) on apps you do not fully trust.
- [ ] Database backups scheduled and mirrored to an encrypted, versioned bucket.
- [ ] A copy of `state.json` kept somewhere safe.
- [ ] Pin known hosts in CI (`DEPLOY_KNOWN_HOSTS` in the [CI example](automation.md#github-actions)).

## Reporting a vulnerability

Please report security issues privately through GitHub's security advisories for the repository
rather than in a public issue.
