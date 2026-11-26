# Environment variables

Environment variables are how apps get their settings and secrets. saba stores them on the
server, versions them with each deployment and resolves references between apps.

## Setting and reading

```sh
saba env                                   # list, in dotenv format
saba env set STRIPE_KEY=sk_live_... LOG_LEVEL=debug
saba env unset LOG_LEVEL
saba env get STRIPE_KEY                    # print one value, nothing else
saba env import .env.production            # set everything in a dotenv file
cat secrets.env | saba env import -        # ...or from stdin
```

Every change rolls out a new version that has it, with the same zero-downtime switch as a
deploy. A single `set` or `import` with many variables is one rollout.

`saba env` prints dotenv format, so its output can be saved and imported again:

```sh
saba env -a api > api.env                      # back up
saba env import -a api2 api.env                # into another app, new or existing
```

Values are quoted when they contain spaces, quotes, `#`, `$`, backslashes or newlines.

Variables can also be passed at deploy time with `-e KEY=VALUE` or `--env-file FILE`, and
non-secret ones can live in the `[env]` table of [`saba.toml`](configuration.md#env).

## Saving without deploying

```sh
saba env set --no-deploy A=1 B=2
saba env unset --no-deploy OLD
saba restart                    # one rollout applies all of it
```

`--no-deploy` stores the change without creating a version. `saba status` then shows
`config or env changes are saved but not deployed`, and the next deploy or `saba restart`
applies them.

Changes to an app that is **stopped**, or whose deploys have all failed, are always saved without
deploying, so setting a variable never starts an app as a side effect.

That includes apps that do not exist yet. Setting variables for a new name creates the app
without starting it, so secrets can be in place before the first deploy:

```sh
saba env set -a api STRIPE_KEY=sk_live_...
saba db link main -a api
saba deploy -a api                # starts with both
```

Passing them to the first deploy (`saba deploy -e KEY=VALUE` or `--env-file`) works as well.
Removing variables from an app that does not exist is an error.

## References between apps

A value can refer to another app's variables with `${{app.KEY}}`:

```sh
saba env set DATABASE_URL='${{main.URL}}?sslmode=disable'
saba env set CACHE_URL='${{cache.URL}}' AUTH_TOKEN='${{auth.SERVICE_TOKEN}}'
```

Use single quotes in the shell so it does not expand `$`.

- A reference is looked up first in the target app's **outputs** (the connection details of a
  [managed database](databases.md#connection-details): `URL`, `HOST`, `PORT`, `USER`,
  `PASSWORD`, `DATABASE`), then in its environment.
- The value is stored as written and **resolved at deploy time**. If the referenced value
  changes, apps that use it see the new value on their next deploy or restart.
- References are checked when you set them: naming an app or variable that does not exist is
  an error, with a list of what is available.
- An app that others reference cannot be destroyed until the references are removed, so a
  deploy never ends up pointing at nothing.

`saba env --resolved` shows the values with references expanded. `saba db link` is a shortcut
that sets the conventional variable (`DATABASE_URL`, `REDIS_URL` or `MONGODB_URL`) to a
reference.

## Variables saba sets

Every replica gets these, in addition to your own:

| Variable | Value |
|---|---|
| `PORT` | The app's HTTP port, unless you set `PORT` yourself |
| `SABA_APP` | The app's name |
| `SABA_VERSION` | The version number, e.g. `14` |
| `SABA_REPLICA` | The replica's number, from 1 |
| `SABA_NODE` | The server the replica runs on (`local` for the primary) |

Release commands, cron jobs and `saba run` get `PORT`, `SABA_APP` and `SABA_VERSION`.

## Versioning

Each deployment records the environment it ran with. A [rollback](deploying.md#rollbacks)
restores the environment of the version it returns to, not just its image: if `v12` worked and
`v13` broke because of a variable, `saba rollback --to v12` fixes both.

## How values are handled

- Values may contain any characters, including newlines, so PEM keys and JSON fit.
- Values reach the container through Docker's environment, not through command-line arguments,
  so they do not appear in the server's process list.
- `saba status`, `saba apps` and `saba deployments` never print values. `saba env` does: that
  is what it is for.
- On the server, values are stored in the daemon's state file, readable only by root. They are
  not encrypted at rest. See [Security](security.md#secrets).
