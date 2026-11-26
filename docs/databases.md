# Databases and backups

saba runs databases next to your apps: one command creates one with a persistent volume and
generated credentials, another connects it to an app, and backups can be scheduled and copied
to S3-compatible storage. The same backups work for any app that keeps data in volumes (see
[Backing up app volumes](#backing-up-app-volumes)).

## Creating a database

```sh
saba db create main --engine postgres
```

```text
▶ Creating postgres database main → root@203.0.113.10
✓ main is ready
  URL    postgres://postgres:3f9c...@main:5432/main
```

| Engine | `--engine` | Default version | Port | Variable set by `db link` |
|---|---|---|---|---|
| PostgreSQL | `postgres` | 17 | 5432 | `DATABASE_URL` |
| MySQL | `mysql` | 8.4 | 3306 | `DATABASE_URL` |
| MariaDB | `mariadb` | 11 | 3306 | `DATABASE_URL` |
| Redis | `redis` | 7 | 6379 | `REDIS_URL` |
| MongoDB | `mongo` | 8 | 27017 | `MONGODB_URL` |

Options:

| Flag | Description |
|---|---|
| `--version 16` | Image tag of the official image for the engine |
| `--memory 1g` | Memory limit |
| `--node worker1` | Run it on another [node](scaling.md) instead of the primary |
| `--publish 127.0.0.1:5432:5432` | Also expose it on a host port (see [connecting from your machine](#connecting-from-your-machine)) |

A database is an app of a special kind:

- it runs a single replica, on a single node, with the `recreate` strategy,
- its data lives in the named volume `saba-<name>-data`,
- it is reachable by other apps on the same server at `<name>:<port>`, and never from the
  internet,
- its password is 32 random hexadecimal characters, generated when it is created. The
  database is named after the app, with dashes turned into underscores.

Readiness is checked with the engine's own client (`pg_isready`, `mysqladmin ping`,
`redis-cli ping`, ...), so `db create` returns only when the database accepts connections.

## Connecting an app

```sh
saba db link main -a api
```

This sets `DATABASE_URL='${{main.URL}}'` on `api` and rolls `api` out. The value is a
[reference](environment.md#references-between-apps): it is resolved when `api` deploys, so `api`
never stores a copy of the credentials. Use `--as PRIMARY_DB_URL` for another variable name, or
`--no-deploy` to save it for the next deploy.

### Connection details

Every database exposes these values to references (`${{main.HOST}}`):

| Name | Example |
|---|---|
| `URL` | `postgres://postgres:3f9c...@main:5432/main` |
| `HOST` | `main` |
| `PORT` | `5432` |
| `USER` | `postgres` (`root` for MySQL, MariaDB and MongoDB, `default` for Redis) |
| `PASSWORD` | the generated password |
| `DATABASE` | `main` (not for Redis) |

```sh
saba db url main            # print the URL
saba db url main --json     # all of the values above
```

## Working with the data

```sh
saba exec -a main -t -- psql -U postgres main                # interactive shell
saba exec -a main -- psql -U postgres main -c 'select 1'     # one query
saba exec -a main -t -- mysql -uroot -p                      # MySQL / MariaDB
saba exec -a cache -t -- redis-cli                           # already authenticated
```

### Connecting from your machine

Databases are not reachable from outside the server. For a GUI client, publish the database's
port on the server's loopback interface, then tunnel to it over SSH:

```sh
saba db publish main 127.0.0.1:5432:5432       # or --publish on `db create`
ssh -N -L 5432:127.0.0.1:5432 root@203.0.113.10
```

and connect to `localhost:5432` with the user and password from `saba db url main --json`.
`saba db publish main` shows the current setting and `saba db publish main --clear` removes it.
Changing it restarts the database briefly. Publishing on `0.0.0.0` would expose the database to
the internet; do not do that.

### Upgrading

Minor versions are an image change:

```sh
saba deploy -a main --image postgres:17.6
```

The database is stopped, started on the new image, and restored to the old one if it fails to
become ready. (A database can only be deployed from an image: deploying a source directory to it
is refused.) **Major versions of PostgreSQL cannot read each other's data directory.** Upgrade
by dumping and restoring into a new database:

```sh
saba db create main2 --engine postgres --version 18
saba db backup create main -o main.sql.gz
saba db restore main2 main.sql.gz --yes
saba env set -a api DATABASE_URL='${{main2.URL}}'
```

### Deleting

```sh
saba env unset -a api DATABASE_URL     # remove references first
saba destroy main --yes                # keeps the data volume and backups
saba destroy main --yes --volumes      # also deletes the data and the backups on the server
```

Backups in S3 are never deleted by `destroy`.

## Backups

Backups of PostgreSQL, MySQL, MariaDB and MongoDB are compressed dumps made with the engine's
own tool, run inside the database's container while it keeps serving. Redis, which has no dump
tool, is backed up as an archive of its data volume, like any
[app with volumes](#backing-up-app-volumes); its restore stops Redis for a moment.

| Engine | Dump | Restore |
|---|---|---|
| PostgreSQL | `pg_dump --clean --if-exists --no-owner --no-acl` | `psql` in a **single transaction**: a failed restore changes nothing |
| MySQL | `mysqldump --single-transaction --routines --triggers --databases` | `mysql` |
| MariaDB | `mariadb-dump --single-transaction --routines --triggers --databases` | `mariadb` |
| MongoDB | `mongodump --archive` | `mongorestore --archive --drop` |

### Making and listing backups

```sh
saba db backup create main                  # dump now, store on the server
saba db backup create main -o ./            # ...and download it to this directory
saba db backup create main -o - > dump.gz   # ...or to stdout
saba db backup list main
```

```text
ID                          KIND       SIZE     LOCATION  CREATED
20261126T030000Z-scheduled  scheduled  14.2 MB  local+s3  3h ago
20261125T171204Z-manual     manual     14.1 MB  local+s3  13h ago
```

The ID is the UTC time the backup was made. `latest` can stand in for an ID when downloading or
restoring; deleting needs the explicit ID.
`LOCATION` says where copies exist: on the server (`local`), in S3 (`s3`), or both.

Backups are stored on the server in `/var/lib/saba/backups/<database>/`, gzip-compressed, as
`<id>.sql.gz` (or `.archive.gz` for MongoDB). A dump is written under a temporary name and only
renamed when it completes, so a failed backup never looks like a good one. Only one backup or
restore of a database runs at a time.

```sh
saba db backup download main latest -o ./
saba db backup delete main 20261125T171204Z-manual --yes
```

### Scheduling

```sh
saba db backup schedule main @daily            # 00:00 UTC every day
saba db backup schedule main "0 3 * * *" --keep 14
saba db backup schedule main                   # show the current schedule
saba db backup schedule main off
```

The schedule is a [cron expression](configuration.md#cron) in UTC. Scheduled backups are pruned
to the newest `--keep` (default 7), on the server and in S3. Manual backups are never pruned
automatically. Changing the schedule does not restart the database.

### Restoring

```sh
saba db restore main --backup latest --yes                      # a stored backup
saba db restore main --backup 20261125T171204Z-manual --yes
saba db restore main ./main.sql.gz --yes                        # a local file, plain or gzip
pg_dump "$OLD_DATABASE_URL" | saba db restore main - --yes      # stdin
```

A restore replaces data, so it requires `--yes`. The database must be running. What happens to
existing data depends on the engine:

- **PostgreSQL**: objects in the dump are dropped and recreated, inside one transaction.
  Objects not in the dump are left alone.
- **MySQL / MariaDB**: tables in the dump are dropped and recreated. Not atomic: a restore that
  fails midway leaves the tables it already processed restored.
- **MongoDB**: collections in the dump are dropped and restored. Not atomic either.

A failed restore reports the database client's output under `details.output` with `--json`.

## Backing up app volumes

An app that keeps data in [named volumes](configuration.md#top-level) (`volumes =
["data:/data"]`: uploads, SQLite files, a search index) is backed up with `saba backup`, which
takes the app like every other command (`-a`, `saba.toml` or `SABA_APP`):

```sh
saba backup create -a uploads                     # archive the volumes now
saba backup create -a uploads --stop              # ...with the app stopped meanwhile
saba backup schedule -a uploads @daily --keep 14  # automatically (add --stop to stop it each time)
saba backup list -a uploads
saba backup download -a uploads latest -o ./
saba backup restore -a uploads --backup latest --yes
saba backup restore -a uploads ./uploads-20270101T030000Z-scheduled.tar.gz --yes
```

A backup is a `.tar.gz` with a small manifest and one top-level directory per volume, holding
its files with their permissions, owners (numeric), timestamps, symlinks and hard links. It is
made by a short-lived `busybox` container that mounts the volumes read-only next to the app, on
the server the app runs on, so the app's own image needs no tools and works even when it is
distroless. Bind mounts (`/host/path:/path`) are not included.

Everything else is shared with database backups: IDs, storage in
`/var/lib/saba/backups/<app>/`, retention, [S3 mirroring](#off-site-backups-with-s3),
download and delete. `saba backup` also works for databases (it dumps them), so one set of
commands covers every app.

**Consistency.** By default the archive is made while the app keeps running, which is right
for files that are written whole (uploads, generated assets). Data that changes in place while
it is read, like an SQLite database or a search index, can be caught half-written. For those,
`--stop` stops the app for the duration: the proxy holds incoming requests (up to a minute),
in-flight requests finish first, and the app is started and health-checked again afterwards,
so requests are delayed, not refused. A small SQLite database takes well under a second.

**Restoring** replaces the contents of the volumes in the archive:

1. the whole archive is read and checked first: a truncated or corrupt file, paths that leave
   the volume, or a link pointing out of it are refused, and nothing changes,
2. the app is stopped (requests wait, as with `--stop`),
3. each volume in the archive is emptied and unpacked,
4. the app is started and health-checked.

Volumes of the app that are not in the archive are left alone; volumes in the archive that the
app does not have are reported as skipped. Volumes are matched by name, so an archive restores
into another app with the same volume names too (`saba backup restore -a staging-uploads
./uploads-....tar.gz --yes`). Any `.tar` or `.tar.gz` laid out with one directory per volume
works, so data can also be imported from elsewhere.

## Off-site backups with S3

Backups on the server do not survive losing the server. Configure S3-compatible object storage
once, and every backup is uploaded there too:

```sh
saba db backup s3 set --bucket my-backups --region eu-central-1 --prefix prod
```

The access key and secret default to `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` in your
shell. Pass `--access-key-id` and `--secret-access-key -` (read from stdin) to give them
explicitly. Before saving anything, the server writes, reads, lists and deletes a small test
object; wrong credentials, a missing bucket or missing permissions are reported then, not at
3 a.m. `--create-bucket` creates the bucket if it does not exist.

Once set:

- every new backup, manual or scheduled, is uploaded right after it is made,
- existing backups are uploaded in the background,
- `list`, `download`, `restore` and `delete` also work with backups that exist **only** in the
  bucket,
- retention and `delete` apply to the bucket,
- objects are stored as `<prefix>/<database>/<id>.<format>.gz`.

If an upload fails, the backup still succeeds on the server, the result carries a warning, and
the backup is uploaded later: after the next successful upload, and in an hourly sweep. Until
every backup is in the bucket, `saba doctor` reports a failing `backup-storage` check. Running `saba db backup s3 set` again with
the same settings also uploads everything that is missing.

```sh
saba db backup s3 show
saba db backup s3 remove        # stop uploading; nothing in the bucket is deleted
```

### Providers

| Provider | Settings |
|---|---|
| AWS S3 | `--bucket b --region eu-central-1` (the endpoint is derived from the region) |
| Cloudflare R2 | `--bucket b --endpoint https://<account-id>.r2.cloudflarestorage.com --region auto` |
| Backblaze B2 | `--bucket b --endpoint https://s3.<region>.backblazeb2.com --region <region>` |
| MinIO, RustFS, Garage, ... | `--bucket b --endpoint https://minio.internal:9000` |

saba uses path-style addressing (`https://endpoint/bucket/key`) for every endpoint except AWS,
which gets virtual-hosted addressing. `--path-style=true|false` overrides that. Large backups
are uploaded in 64 MB parts.

On AWS, the key needs this policy on the bucket (add `s3:CreateBucket` for `--create-bucket`):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Action": ["s3:ListBucket"], "Resource": "arn:aws:s3:::my-backups" },
    {
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:GetObject", "s3:DeleteObject", "s3:AbortMultipartUpload"],
      "Resource": "arn:aws:s3:::my-backups/*"
    }
  ]
}
```

### Good practice

- **Use a distinct `--prefix` per server** (`prod`, `staging`). Backups are grouped by database
  name, so two servers with a database called `main` sharing a bucket and prefix would mix their
  backups.
- saba does not encrypt dumps. Enable server-side encryption on the bucket, and treat anyone who
  can read the bucket as able to read your data.
- For protection against deletion (a compromised server holds credentials that can delete),
  enable versioning or object lock on the bucket and use a lifecycle rule to expire old versions.

## Disaster recovery

The server is gone. With S3 configured, on a new server:

```sh
saba server setup root@198.51.100.20 --name prod
saba db backup s3 set --bucket my-backups --region eu-central-1 --prefix prod
saba db create main --engine postgres            # the same name as before
saba db backup list main                         # the old server's backups, LOCATION s3
saba db restore main --backup latest --yes
```

Then deploy your apps and link them again (`saba db link main -a api`). The new database has a
new password; apps linked by reference pick it up when they deploy.

Practice this once before you need it.
