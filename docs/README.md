# saba documentation

saba turns Linux servers with Docker into an app platform: zero-downtime deploys, automatic
HTTPS, rollbacks, managed databases with backups, cron, and scaling across machines. It is one
binary that acts as both the server daemon and the CLI you drive it with, over plain SSH.

## Start here

- **[Getting started](getting-started.md)**: install the CLI, set up a server, deploy an app,
  add a domain and a database. About fifteen minutes.
- **[Concepts](concepts.md)**: apps, versions, how a deploy works, where configuration lives.
  Read this once; everything else builds on it.

## Guides

| Guide | What it covers |
|---|---|
| [Deploying](deploying.md) | Sources, automatic builds, health checks, rolling and recreate deploys, release commands, rollbacks |
| [Configuration](configuration.md) | Every `saba.toml` key, how settings are merged and remembered, CLI environment variables |
| [Environment variables](environment.md) | Secrets, references between apps, variables saba injects |
| [Domains and HTTPS](domains-and-https.md) | Automatic domains, custom domains, certificates, wildcards, your own certificates |
| [Databases and backups](databases.md) | Managed Postgres/MySQL/MariaDB/Redis/MongoDB, backups, S3 mirroring, disaster recovery |
| [Scaling](scaling.md) | Replicas, adding servers, placement, tunnel and direct traffic, failure behavior |
| [Operating apps](operations.md) | Status, logs, exec, one-off commands, cron, stop/start, destroy, diagnostics |
| [Servers](servers.md) | Server setup and upgrades, contexts, SSH, the daemon and systemd, files on disk |
| [Automation and AI agents](automation.md) | JSON output, exit and error codes, CI/CD, the MCP server |
| [Recipes](recipes.md) | Complete examples: web app with Postgres, static site, workers, preview environments |

## Reference

- [CLI reference](cli-reference.md): every command and flag, generated from the code.
- [Architecture](architecture.md): how the daemon, proxy, deploy engine and cluster fit together.
- [Security](security.md): the trust model, where secrets live, and how to harden a server.
- [Troubleshooting](troubleshooting.md): symptoms, causes and fixes.

## Getting help from the binary

Everything here is also reachable from the terminal:

```sh
saba --help                 # command overview
saba <command> --help       # details, flags and examples for one command
saba guide                  # a compact reference of the whole tool, written for AI agents
saba doctor                 # checks the server and every app, with a suggested fix per problem
```
