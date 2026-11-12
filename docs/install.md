# Installing and running Atelier

Atelier is early: there is no installer yet. These are the ways to run it today.

## Local development

You need Go 1.27+ and `git` on your `PATH`.

```sh
make run          # ATELIER_DATA=./data go run ./cmd/atelier
```

Open http://localhost:3000. The first visit goes to **Set up Atelier**, where you pick your username and password. That creates your personal account and signs you in. Everything lives in `./data`: `atelier.db` (SQLite) and `repos/` (bare git repositories).

Other targets: `make test`, `make build` (writes `bin/atelier`), `make docker` (builds `atelier:latest`). `atelier -version` prints the build version.

## Pushing your first repo

1. Create a repository with **New** (under your personal account or a company account).
2. Create an access token under **Settings → Access tokens**. Choose **read** (clone/fetch) or **write** (also push) and an expiry. The token (`atl_…`) is shown once. Only its hash is stored.
3. Push over HTTPS and use the token as the password (any username works):

   ```sh
   git remote add origin http://localhost:3000/<you>/<repo>.git
   git push -u origin main
   # Username: <you>   Password: atl_…
   ```

   Your OS credential helper (e.g. the macOS keychain) remembers it after the first push. Public repos can be cloned without a token. Private repos and every push need one.

## Docker and Portainer

The `Dockerfile` builds a small Alpine image with `git` and `git-http-backend`. It runs as the non-root `atelier` user (uid 1000) and keeps all data in the `/data` volume.

1. Build the image on your server: `make docker` (or `docker build -t atelier:latest .`). You can also let Portainer build it from this repo (see the comments in the stack file).
2. In Portainer, add a stack from [`deploy/portainer/stack.yml`](deploy/portainer/stack.yml). Set `ATELIER_PUBLIC_URL` to your real URL (e.g. `https://git.example.com`). Keep `ATELIER_TRUST_PROXY=true` when nginx sits in front.
3. The stack publishes the container on `127.0.0.1:3000` only, so nginx on the host is the way in. Install [`deploy/nginx/atelier.conf`](deploy/nginx/atelier.conf) (see below) and reload nginx.
4. Visit your URL and complete setup. Back up the `atelier-data` volume.

| Variable | Default | Meaning |
|---|---|---|
| `ATELIER_LISTEN` | `:3000` | Listen address |
| `ATELIER_DATA` | `./data` (`/data` in Docker) | Database and repositories |
| `ATELIER_PUBLIC_URL` | derived from the request | External URL, used for clone URLs, cookies and OAuth |
| `ATELIER_TRUST_PROXY` | `false` | Honour `X-Forwarded-*` from nginx |
| `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`, `ATELIER_GITHUB_LOGIN` | empty | Continue with GitHub (all three needed) |

## Continue with GitHub

1. On GitHub, go to **Settings → Developer settings → OAuth Apps → New OAuth App**.
2. Set **Homepage URL** to your `ATELIER_PUBLIC_URL`. Set **Authorization callback URL** to `<ATELIER_PUBLIC_URL>/auth/github/callback`.
3. Copy the client ID and generate a client secret.
4. Set `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET` and `ATELIER_GITHUB_LOGIN` (your GitHub username) in the stack, then redeploy. The sign-in page then shows **Continue with GitHub**. Only that one GitHub account can sign in. Password sign-in keeps working.

## Importing repositories

**Settings → Import** brings repositories in from Gitea/Forgejo (server URL and token: your own and your organizations' repositories), GitHub (token) or any git URL (optional username and token), into your personal account or a company account. Pick the repositories and their names; they import in the background, with visibility and descriptions carried over. Tick **Keep syncing as a mirror** to fetch from the source every `ATELIER_MIRROR_INTERVAL` minutes (default 30). Only mirrors store their token (in the database, never shown again); one-off imports forget it when they finish. Issues and pull requests aren't imported yet.

## Deploying behind nginx

See [`deploy/nginx/atelier.conf`](deploy/nginx/atelier.conf). The important parts are:

- `client_max_body_size 0` so large pushes aren't rejected
- `proxy_buffering off` and `proxy_request_buffering off` so git pack data streams
- WebSocket upgrade headers for live Actions logs, runners and MCP
- `X-Forwarded-Proto` and `X-Forwarded-Host`, which Atelier trusts when "Behind a reverse proxy" is on, so clone URLs show `https://`
- SSH isn't HTTP, so either publish port 2222 straight from the container or add an nginx `stream {}` block for it
- Pages and PR previews use extra server names (`*.atelier.<domain>`) on the same proxy

## More

- [Runners](runners.md): run Actions jobs on your own devices
- [Backups and restore](backups.md)
- [Remote access](remote-access.md): Cloudflare Tunnel or Tailscale
- [Pages](pages.md)
