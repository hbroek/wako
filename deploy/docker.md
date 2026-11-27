# Running wako with Docker

The `Dockerfile` in the repository root builds a small image from `scratch`: just the
two programs and the CA bundle. It runs as a non-root user (65532), listens on port 8888,
keeps everything in the `/data` volume and has a built-in health check.

## 1. Build the image

```bash
git clone https://github.com/hbroek/wako.git
cd wako
docker build -t wako .
```

(`make docker` does the same and also stamps the version into the image.)

## 2. Run it

```bash
docker run -d --name wako --restart unless-stopped \
  -p 127.0.0.1:8888:8888 \
  -v wako-data:/data \
  -e WAKO_ADMIN_PASSWORD='choose-a-password' \
  wako
```

Open <http://127.0.0.1:8888/> and sign in as `admin`. The first space, `main`, is private; to let anyone read it, turn on **Anyone may read** under its **Settings**. If you leave out
`WAKO_ADMIN_PASSWORD`, a password is generated and printed once in `docker logs wako`
(a line `administrator "admin", password ...`); change it on the Account page.

Use a **named volume** (as above). Docker fills a new named volume with the image's
ownership, so the non-root user can write to it. With a bind mount of a host folder on Linux, make
the folder owned by that user first, or wako stops with "unable to open database file"
(Docker Desktop on a Mac or Windows PC maps ownership itself and needs nothing):

```bash
mkdir -p data && sudo chown 65532:65532 data && chmod 700 data
docker run ... -v "$PWD/data:/data" wako
```

The volume holds every wiki, the user accounts and the signing key. Back it up (see
below).

## 3. Put it behind HTTPS

For anything beyond your own machine, put a reverse proxy in front (Caddy, nginx or
Traefik; examples are in this folder: `Caddyfile`, `nginx.conf`) and give wako the
address people use:

```bash
docker run -d --name wako --restart unless-stopped \
  -p 127.0.0.1:8888:8888 \
  -v wako-data:/data \
  -e WAKO_PUBLIC_URL=https://wiki.example.com \
  -e WAKO_TRUSTED_PROXIES=172.16.0.0/12 \
  -e WAKO_ADMIN_PASSWORD='choose-a-password' \
  wako
```

| Variable | Why |
|---|---|
| `WAKO_PUBLIC_URL` | the address people use; for feed links and the host check |
| `WAKO_TRUSTED_PROXIES` | **required behind a proxy**, see below |
| `WAKO_ADMIN_PASSWORD` | first administrator, user `admin` |
| `WAKO_SITE_NAME` | optional |

**`WAKO_TRUSTED_PROXIES` matters.** wako only believes `X-Forwarded-Proto` and
`X-Forwarded-For` from proxies it is told about. Inside a container the proxy does not
look like `127.0.0.1` (Docker connects from its bridge network, normally in
`172.16.0.0/12`), so without this setting wako thinks every request is plain HTTP and does
not set the sign-in cookies: logging in appears to do nothing. If the proxy is a container
on a network of your own, list that network's range instead.

Optional AI features (off until you set a provider; a server administrator then switches
them on per space in the space's settings):

| Variable | Value |
|---|---|
| `WAKO_AI_PROVIDER` | `anthropic`, or `openai` for any OpenAI-compatible service |
| `WAKO_AI_KEY` | the service's API key (keep it out of shell history, for example with `--env-file`) |
| `WAKO_AI_MODEL` | optional; defaults to a current model of the provider |

A model running on your own computer cannot be reached from a server in the cloud.

## Docker Compose

```yaml
services:
  wako:
    build: .
    restart: unless-stopped
    ports: ["127.0.0.1:8888:8888"]
    volumes: ["wako-data:/data"]
    environment:
      WAKO_PUBLIC_URL: https://wiki.example.com
      WAKO_TRUSTED_PROXIES: 172.16.0.0/12
      WAKO_ADMIN_PASSWORD: choose-a-password
volumes:
  wako-data:
```

## Backups

`wakod backup` makes a consistent copy while the server runs:

```bash
docker exec wako /usr/local/bin/wakod backup -data-dir /data -to /data/backup-$(date +%F)
docker cp wako:/data/backup-$(date +%F) ./
```

Backups hold password hashes and the signing key; keep them private.

## Updating

```bash
cd wako && git pull
docker build -t wako .
docker stop wako && docker rm wako
docker run ...   # the same command as before; the data is in the volume
```

With Compose: `git pull && docker compose up -d --build`.

## Troubleshooting

| Symptom | Cause |
|---|---|
| Sign-in does nothing, or the page reloads | `WAKO_TRUSTED_PROXIES` is missing or does not include the proxy's network |
| `421 Misdirected Request` | the address in the browser is not the one in `WAKO_PUBLIC_URL`; fix the variable or the domain |
| A large upload fails with `internal error`, log says `disk I/O error (6410)` | SQLite found no writable temporary folder. Current versions use `/data/tmp` on their own; with an older one set `SQLITE_TMPDIR=/data` |
| Container restarts, log says `unable to open database file` | `/data` is a bind mount owned by root; use a volume, or `chown 65532:65532` the folder |
| Log says a space's database file is missing | the volume was replaced or emptied; restore a backup, or `wakod forget-space SLUG` |
| Uploads of large images or imports fail with 413 | a proxy limit; wako allows 25 MB images and 256 MB imports by default |
| AI says `x509: certificate signed by unknown authority` | the image has no CA bundle (fixed in the Dockerfile); rebuild |
| AI answers time out | the proxy's timeout is shorter than the model's; wako waits up to two minutes |

To see the health check's view: `docker exec wako /usr/local/bin/wakod healthcheck`.
