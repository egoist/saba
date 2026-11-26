# Recipes

Complete setups for common situations. Each assumes a server set up with
`saba server setup` and selected as the current context.

## A web app with PostgreSQL

A Node.js API with migrations, a health check, two replicas and nightly backups.

```toml
# saba.toml
name = "api"
port = 3000
replicas = 2
release_command = "npm run migrate"

[health]
path = "/healthz"

[env]
NODE_ENV = "production"

[[cron]]
name = "cleanup"
schedule = "0 4 * * *"
command = "node scripts/cleanup.js"
```

```sh
saba db create main --engine postgres
saba db backup schedule main @daily --keep 14
saba deploy -e SESSION_SECRET="$(openssl rand -hex 32)" -e DATABASE_URL='${{main.URL}}'
saba domains add api.example.com
```

The first deploy creates `api` with both variables; they are remembered, so later deploys are
just `saba deploy`. Each one runs the migrations in a one-off container before the new version
starts; if they fail, the old version keeps serving. The app reads `process.env.PORT` and
`process.env.DATABASE_URL`:

```js
app.get("/healthz", async (req, res) => {
  await db.query("select 1");
  res.send("ok");
});
app.listen(process.env.PORT, "0.0.0.0");
```

Because two versions run side by side for a moment during each deploy, keep migrations
backward compatible: add columns before the code that uses them, drop them a deploy later.

## A single-page app

A Vite/React/Vue build served by nginx, with client-side routes falling back to `index.html`.
Add a Dockerfile, so saba builds it instead of detecting a Node.js server:

```dockerfile
FROM node:lts-slim AS build
WORKDIR /app
COPY package*.json ./
RUN npm ci
COPY . .
RUN npm run build

FROM nginx:alpine
COPY --from=build /app/dist /usr/share/nginx/html
RUN printf 'server {\n  listen 80;\n  root /usr/share/nginx/html;\n  location / { try_files $uri /index.html; }\n}\n' > /etc/nginx/conf.d/default.conf
```

```sh
saba deploy -a www
saba domains add -a www example.com www.example.com
```

A plain folder of HTML files needs no Dockerfile: saba serves `index.html` from the project root,
`public/` or `dist/` with nginx.

## A background worker with Redis

A queue consumer that does not serve HTTP, scaled to three replicas:

```toml
# saba.toml
name = "worker"
worker = true
replicas = 3
command = "node worker.js"
stop_timeout = "60s"      # let a job in progress finish on SIGTERM
```

```sh
saba db create queue --engine redis
saba deploy -e REDIS_URL='${{queue.URL}}'
```

With `worker = true` there is no port and no routing; a replica counts as healthy once it has
stayed up for three seconds. Handle `SIGTERM` by finishing the current job, then exiting.

## Several services from one repository

```text
repo/
├── web/        Next.js frontend, public
├── api/        Go API, only reachable from web
└── worker/     Python worker
```

```toml
# api/saba.toml
name = "api"
internal = true
port = 8080
```

```toml
# web/saba.toml
name = "web"

[env]
API_URL = "http://api:8080"
```

```sh
saba deploy ./api
saba deploy ./worker -a worker --worker
saba deploy ./web
```

Each directory is uploaded and built on its own. `api` has no public URL; `web` reaches it at
`http://api:8080` over the private network.

## Preview environments per pull request

Deploy each pull request as its own app, and delete it when the pull request closes:

```yaml
# .github/workflows/preview.yml
name: Preview
on:
  pull_request:
    types: [opened, synchronize, reopened, closed]

concurrency: preview-${{ github.event.number }}

jobs:
  preview:
    runs-on: ubuntu-latest
    env:
      SABA_HOST: deploy@203.0.113.10
      APP: web-pr-${{ github.event.number }}
    steps:
      - uses: actions/checkout@v4
      - name: Install saba and SSH credentials
        run: |
          curl -fsSL https://raw.githubusercontent.com/egoist/saba/main/install.sh | SABA_INSTALL_DIR="$HOME/.local/bin" sh
          echo "$HOME/.local/bin" >> "$GITHUB_PATH"
          install -m 700 -d ~/.ssh
          echo "${{ secrets.DEPLOY_SSH_KEY }}" > ~/.ssh/id_ed25519 && chmod 600 ~/.ssh/id_ed25519
          echo "${{ secrets.DEPLOY_KNOWN_HOSTS }}" >> ~/.ssh/known_hosts

      - name: Deploy
        if: github.event.action != 'closed'
        run: |
          saba deploy -a "$APP" -q
          echo "Preview: $(saba status "$APP" --json | jq -r '.urls[0]')" >> "$GITHUB_STEP_SUMMARY"

      - name: Clean up
        if: github.event.action == 'closed'
        run: saba destroy "$APP" --yes --volumes || [ $? -eq 3 ]
```

Each preview gets `https://web-pr-42.<base domain>` and the project's `saba.toml` settings.
Keep production domains out of `saba.toml` (add them with `saba domains add -a web ...`),
otherwise every preview would try to claim them; or override with `--domain`. A wildcard
certificate for the base domain makes previews come up without per-app certificate issuance. The
`|| [ $? -eq 3 ]` makes cleanup succeed when the preview was never deployed.

## Separate staging and production

Use one server per environment and a context for each:

```sh
saba server setup root@198.51.100.5 --name staging
saba server setup root@203.0.113.10 --name prod
saba context use staging
```

```sh
saba deploy                 # goes to staging, the current context
saba -c prod deploy         # production, explicitly
```

Pinning a project with `context = "staging"` in `saba.toml` makes the safe environment the
default for everyone working on it; production then always needs `-c prod`.

## Scaling out to a second server

```sh
saba node add worker1 root@10.0.0.2 --address 10.0.0.2 --direct
saba server config set address 10.0.0.1
saba db create main --engine postgres --publish 10.0.0.1:5432:5432
saba deploy -a api --node local --node worker1 --replicas 4 -e DATABASE_URL='${{main.URL}}'
```

`node add` logs in to the node from your machine to authorize the primary's key; if only the
primary can reach `10.0.0.2`, see [adding servers](scaling.md#adding-servers) for
`--no-authorize`.

The primary keeps the database and terminates HTTPS; `api` runs two replicas on each server.
Publishing the database on the primary's private address lets the replicas on `worker1` reach it
by its name, `main`. Create it before deploying `api`: containers learn names on other servers
when they start.

See [Scaling](scaling.md) for what works across servers and what does not.

## Moving a database into saba

Import an existing PostgreSQL database by streaming a dump into a new one:

```sh
saba db create main --engine postgres
pg_dump --clean --if-exists --no-owner --no-acl "$OLD_DATABASE_URL" | saba db restore main - --yes
saba db link main -a api
```
