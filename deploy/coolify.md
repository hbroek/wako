# Deploying wako with Coolify

These steps deploy from the private GitHub repository `hbroek/wako`, using the
`Dockerfile` in the repository root. The image is built from `scratch`, runs as a
non-root user (65532), listens on port 8888 and has a built-in health check.

Written against Coolify v4. Menu names can differ a little between versions.

## 1. Let Coolify read the repository

The repository is private, so Coolify needs access. Either way works:

- **GitHub App** (easiest if you use Coolify for several repositories): in Coolify,
  *Sources → + Add → GitHub App*, follow the prompts, and give it access to `hbroek/wako`.
- **Deploy key**: in Coolify, *Keys & Tokens → Private Keys → + Add* (generate one), then
  in GitHub open the repository → *Settings → Deploy keys → Add deploy key*, paste the
  public key, leave "Allow write access" off.

## 2. Create the application

1. *Projects →* your project *→ + New Resource → Application*.
2. Choose **Private Repository (with GitHub App)** or **(with Deploy Key)** to match step 1.
3. Repository `hbroek/wako`, branch `main`.
4. **Build Pack: Dockerfile.** Base directory `/`, Dockerfile location `/Dockerfile`.
5. **Ports Exposes: `8888`.**
6. **Domains:** `https://wiki.example.com` (your domain; its DNS must point at the Coolify server).

## 3. Storage

*Persistent Storage → + Add*, a **volume** (not a bind mount) with destination path
`/data`. A Coolify-managed volume is initialised with the image's ownership, so the
non-root user can write to it. A bind mount of a host folder would be owned by root and
wako would stop with "unable to open database file".

This volume holds every wiki, the user accounts and the signing key. Back it up
(Coolify's scheduled volume backups work; so does `wakod backup`, which makes a
consistent copy while the server runs).

## 4. Environment variables

*Environment Variables*, then add:

| Variable | Value | Why |
|---|---|---|
| `WAKO_PUBLIC_URL` | `https://wiki.example.com` | the address people use; for feed links |
| `WAKO_TRUSTED_PROXIES` | `10.0.0.0/8,172.16.0.0/12,192.168.0.0/16` | **required behind Coolify**, see below |
| `WAKO_ADMIN_PASSWORD` | a password you choose | first administrator, user `admin` (otherwise one is generated and printed once in the logs) |
| `WAKO_SITE_NAME` | e.g. `Henry's wiki` | optional |

**`WAKO_TRUSTED_PROXIES` matters.** Coolify's proxy (Traefik or Caddy) is another
container, and wako only believes `X-Forwarded-Proto` and `X-Forwarded-For` from proxies
it is told about. Without this setting wako thinks every request is plain HTTP and will
not set the sign-in cookies, so logging in appears to do nothing. The ranges above are
Docker's private networks, which is where the proxy sits.

Optional AI features (off until you set a provider; a server administrator then switches
them on per space in the space's settings):

| Variable | Value |
|---|---|
| `WAKO_AI_PROVIDER` | `anthropic`, or `openai` for any OpenAI-compatible service |
| `WAKO_AI_KEY` | the service's API key (mark it as a secret in Coolify) |
| `WAKO_AI_MODEL` | optional; defaults to a current model of the provider |

A model running on your own computer cannot be reached from a server in the cloud.

## 5. Deploy and check

1. Press **Deploy**. The first build compiles Go and takes a few minutes.
2. When Coolify shows the application as *healthy*, open `https://wiki.example.com/` and
   sign in as `admin`.
3. If you did not set `WAKO_ADMIN_PASSWORD`, find the generated password in the
   application's *Logs* (a line `administrator "admin", password ...`, printed once).
   Change it on the Account page.

## 6. Updating

Push to `main`. Turn on *Auto Deploy* (or add Coolify's webhook to the repository) and
Coolify rebuilds and replaces the container; the health check keeps the old one running
until the new one is up. Data is in the volume, so nothing is lost.

## Troubleshooting

| Symptom | Cause |
|---|---|
| Sign-in does nothing, or the page reloads | `WAKO_TRUSTED_PROXIES` is missing or does not include the proxy's network |
| `421 Misdirected Request` | the address in the browser is not the one in `WAKO_PUBLIC_URL`; fix the variable or the domain |
| Container restarts, log says `unable to open database file` | `/data` is a bind mount owned by root; use a volume |
| Log says a space's database file is missing | the volume was replaced or emptied; restore a backup, or `wakod forget-space SLUG` |
| Uploads of large images or imports fail with 413 | a proxy limit; wako allows 25 MB images and 256 MB imports by default |
| AI says `x509: certificate signed by unknown authority` | the image has no CA bundle (fixed in the Dockerfile); redeploy a build that includes it |
| AI answers time out | the proxy's timeout is shorter than the model's; wako waits up to two minutes |

To see the health check's view: `docker exec <container> /usr/local/bin/wakod healthcheck`.
