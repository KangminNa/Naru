# Naru

> Manage your reverse proxy from a screen like Nginx Proxy Manager — and push to deploy.

[한국어](README.md) · [Landing page](https://shelf.nakangmin.duckdns.org/) · MIT

Editing nginx config files to add a domain, setting up certbot cron jobs, keeping a table so ports don't collide —
I built this because I was tired of that. Naru is **a reverse proxy holding ports 80/443**, and you manage domains, certificates and upstreams from a screen.

App deployment sits on top. The proxy has to know about containers anyway, so it might as well start them too.

## Why

I used Nginx Proxy Manager a lot. Two things were missing.

- **I wanted a Caddy version.** Caddy issues and renews HTTPS automatically and speaks HTTP/3 out of the box, but you drive it with a CLI and config files. I wanted to drive it from a screen, like NPM.
- **I also needed CI/CD.** NPM only attaches domains. Naru builds, deploys and wires the domain when you push.

Naru (나루) is Korean for a ferry landing — where requests arrive and cross to each service, and where new versions dock.
It used to be called Shelf (v1, Node). v2 is a Go rewrite with Caddy as the engine and a new name. Containers (`shelf-*`) and webhook URLs carried over from v1 keep working.

> The code lives on the `v2` branch, and the live server has run v2 since 2026-10-10. It will be merged into `main` once cleanup is done.
> Design and progress log: [v2 plan](docs/design/v2.md) (Korean).

---

## What works today

**Web server — Caddy is the engine, you only see the screen**

- Add a domain and requests go to that service. Containers are reached by name — no host ports to publish.
- **Automatic HTTPS** — Caddy obtains and renews the certificate as soon as a domain exists. HTTP is redirected to HTTPS (307) **only once a certificate is there**,
  so the admin screen never locks itself out. Each domain shows "HTTPS · until when".
- Always on, no settings: HTTP/2 and HTTP/3, WebSockets, compression (gzip, zstd), forwarding the client's details to the app (`X-Forwarded-*`). Unknown hosts get 404.
- Naru draws the whole Caddy config and pushes it over the admin unix socket — there is no config file to edit by hand.

**Per-service web server settings** — response headers, allowed IPs, password protection (basic auth), maintenance mode, path routing (`/api` to another service), and an advanced box for Caddyfile directives.
Paste an nginx config and it is turned into those fields. Applying checks against the web server right away; if the web server refuses, the previous settings come back.

**Services — four kinds**

| Kind | What |
|---|---|
| Repository | Builds the repo's `Dockerfile` into a container. Build path for monorepos. Port from `EXPOSE` |
| Image | Pulls an image from Docker Hub or GHCR into a container |
| Static site | Serves a repo folder straight from the web server, no container. Hidden files (`.env`, `.github`…) are never published |
| External | Just an address for something already running (NAS, router, a program on the host). `localhost` means this server |

**Deploys**

- **Push to deploy** — a webhook URL and secret per service. GitHub HMAC · GitLab token · `?secret=` (registries). Only the configured branch.
- **No downtime** — the new container starts, and only when it answers does the web server switch to it and the old one go away. If the new one dies, the old one stays.
- Pushes that arrive during a deploy are merged into one deploy afterwards. Deploy steps and the full log stream live on the screen.
- **Roll back** — to that version's image or files, without rebuilding. Keeps 3 built images and 5 static releases.

**Watching · alerts · diagnosis**

- Every 30 seconds Naru checks services, the web server and Docker, and alerts **only on changes** — Discord, Slack or a generic signed webhook. Two bad checks in a row before something counts as down.
- **"Needs attention"** at the top of the home screen — apps that stopped, apps listening on a different port (with a [switch port] button), DNS not pointing here, certificates ending soon, webhooks that never arrived.
- **Logs tab** — app output and incoming requests on one screen, in time order. CPU and memory per service.

**First run · admin**

- A first-run wizard without any `.env`: account → admin address (with a DNS check) → HTTPS contact. The wizard link is printed only in the server log.
- Korean and English screens. Server CPU, memory and disk.

**Not there** — a switch to turn HSTS on from the screen (only addresses carried over from v1 have it), a dedicated redirect setting (use `redir` in the advanced box),
uploading certificates or DNS challenges (wildcards), TCP/UDP forwarding, multiple admin roles.

---

## Install (production)

On a Linux server with Docker and Docker Compose:

```bash
git clone -b v2 https://github.com/KangminNa/Naru.git naru && cd naru/deploy/docker
cp env.example .env && chmod 600 .env        # fill in ACME_EMAIL etc.
docker network create naru-net               # shared by app containers and the web server
docker compose up -d --build
docker compose logs naru | grep setup         # first-run link — only someone who can read the logs can create the admin
```

- Run the production setup **only from `deploy/docker/`**. The `compose.yml` at the repo root is for local development.
- Update: `git pull && docker compose up -d --build` (in `deploy/docker`). The web server keeps running; only Naru restarts.
- Only 80/443 are exposed. Naru's own screen is reachable at `127.0.0.1:8080` (on the server) and through the admin address.

Forgot your account? From the server shell:

```bash
docker exec naru naru users
 docker exec naru naru passwd <user> '<new password>'   # leading space: kept out of shell history. Signs out all sessions of that account
docker exec naru naru reset                             # deletes all accounts → the first-run wizard opens on the next start
```

## Try it locally

Docker is all you need (no Go install).

```bash
git clone -b v2 https://github.com/KangminNa/Naru.git && cd Naru
docker network create naru-net
docker compose up -d --build                 # project name naru-dev — never collides with production
docker compose logs naru | grep setup
```

The web server listens on `127.0.0.1:8088` (HTTP) and `8443` (HTTPS), with certificates from Caddy's internal CA.
Browsers send `*.localhost` to your own machine — e.g. set the admin address to `naru.localhost` and open `http://naru.localhost:8088`.

---

## Hacking on it

MIT. Fork it, change it, take only the parts you need. Things kept on purpose so it stays easy to change:

- **One object per job, relations only through interfaces** — packages import only `model` (data) and `contract` (interfaces). Concrete types meet in one place, `app` (composition).
- **The outside world sits behind tools** — Docker, git, Caddy, SQLite, files and HTTP each live in their own tool package, so tests swap in fakes and exercise the real composition through the screens.
- **The docs enforce the code** — `internal/archtest` checks rules R1–R12 from the [object design](docs/design/v2-objects.md) (Korean). A type the doc doesn't name fails the tests.
- **Two dependencies** — `modernc.org/sqlite` (pure-Go SQLite) and `golang.org/x/crypto` (scrypt, bcrypt). HTML is rendered on the server; no JavaScript framework.

```
cmd/naru/            server · shell recovery commands
internal/
├── model/ contract/ all data · all interfaces
├── access settings services kinds deploy webhook webserver views websettings watch notify   one package per job
├── web/ cli/        the way in
├── store/           SQLite (the only SQL)
├── docker git files caddy netcheck stats events system httppost                             tools that touch the outside world
├── app/             composition
└── archtest/        rule checks
```

```bash
scripts/go.sh test ./...        # with Go inside Docker
scripts/caddy-validate.sh       # does real Caddy accept the configs we draw?
```

---

## Running it — know this

- Naru mounts `/var/run/docker.sock`. It can control everything in the host's Docker, so **an admin account is root-equivalent by design**.
  Use it on your own server and give accounts only to people you would trust with the server. Use a long admin password.
- Deploying a service means running someone's code on your server. Deploy repositories you trust.
  If you deploy an app that gets docker.sock itself (like Portainer), put allowed IPs or password protection in front of it — that app is root-equivalent too.
- Secrets (git tokens, webhook secrets, environment variables) are stored in the data folder. Protect and back it up. They never reach the screen, the logs or the web server config.
- **Expose only 80/443.** Caddy's admin API is a unix socket shared only by Caddy and Naru.
- Built-in defenses: scrypt passwords, 15-minute lockout after 5 failed logins, Secure/httpOnly/SameSite cookies, rejecting forms from other sites, CSP,
  verified webhooks with a body size limit, validation of git URLs, branches, paths and image names.

---

## License

MIT © Kangmin Na
