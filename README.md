# Atelier

**Your personal GitHub, self-hosted.**

Atelier is a self-hosted code home built for one person. It looks and works like GitHub: repos, code browser, commits, issues, pull requests, Actions and a contribution graph. It has no teams, sign-ups or admin sprawl. It runs on your own server, and every one of your devices plugs into it.

Tools like Gitea, Forgejo and GitLab are built for companies and teams. Atelier is built for one person.

## How it fits together

```
 your devices                      your server (Portainer)
┌──────────────┐                 ┌──────────────────────────┐
│ MacBook      │── runner ──┐    │  nginx  ─►  Atelier       │
│ other device │── runner ──┼──► │             (Go + SQLite) │──► GitHub (Tether)
│ …            │── runner ──┘    │                           │
└──────────────┘                 └──────────────────────────┘
        ▲                                     ▲
        └──── git push / clone over HTTPS ────┘   MCP clients (Claude) ─► /mcp
```

- **Server:** runs as a Docker stack on Portainer, behind your nginx reverse proxy.
- **Runners:** every device runs `atelier-runner`. It executes Actions jobs, finds repos already on that device, and powers "Open in editor" and "Show in Finder".
- **Sign-in:** a single owner signs in with a passkey or **Sign in with GitHub** (locked to your own GitHub account) when not on the local network. Git uses SSH keys or tokens; MCP and scripts use scoped tokens.

## Accounts

You have a personal account, and you can add **company accounts** (e.g. Elyndra Studios) to keep work repos separate from personal ones. Switch between them from the sidebar, the same way GitHub switches between you and your orgs.

A company account is a separate owner for repos, not a separate login: only you use Atelier, so there are no members, invites or permissions. Teammate sign-in can come later if it's ever needed.

## Tether

Running everything yourself shouldn't feel lonely. **Tether** is Atelier's link to GitHub:

- **Blended search:** one search box covers your repos first, then GitHub.
- **Explore and feed:** GitHub trending and the people you follow appear next to your own activity.
- **One-click mirroring:** pull any GitHub repo into Atelier, or mirror pushes out to GitHub.
- **Your identity:** your GitHub avatar, profile and stars carry over.

## Pull requests

Modelled on GitHub's new pull request overview: merge status and next steps sit at the top (checks, review, conflicts, then **Squash and merge**). There's a **Files changed** view with a file tree, note markers and a notes side panel. Pull requests double as self-review before you merge.

## Actions

- Classic YAML workflows (`.atelier/workflows/*.yml`).
- **Agentic workflows:** write the workflow in plain Markdown (`.atelier/workflows/*.md`) and it compiles to a locked YAML workflow. It runs an AI engine with only the MCP tools you allow.
- **Lockfile:** every action is pinned to an exact commit in `.atelier/actions.lock`.
- Jobs are routed to runners on your devices by label (`mac`, `linux`, `docker`, …).

## Releases

- Tag-based releases with Markdown notes. A release-notes agent can draft them from merged pull requests for you to edit.
- Downloadable files built by Actions (e.g. runner binaries per platform), plus source archives.
- A built-in container registry, so a release can publish the Atelier image your Portainer stack pulls.
- Drafts, and an optional Tether step that publishes the same release to GitHub Releases.

## MCP

Atelier is an MCP server at `https://<your-domain>/mcp`. Claude, or any other MCP client, can read and manage repos, code, issues, pull requests and Actions. Each token has its own tool scopes, and agentic workflows use the same tools.

## Apps and bots

- **GitHub-only bots (CodeRabbit, Cursor Bugbot, …)** work through a Tether bridge. Your PR is mirrored to a private GitHub repo, the bots review it there, and their comments sync back into the Atelier PR. You need your own account with each bot service.
- **Native bots:** Claude agents such as review-helper and release-notes.
- **Custom bots:** a webhook, a bot token and a GitHub-compatible API (`/api/v3`).
- Bots can comment, suggest and set checks. Pushing fixes is opt-in, and they can never merge.

## Packages and registries

- **npm registry** at `/npm/`: publish `@deminimmo/*` and `@elyndra/*` packages with `npm publish`, and optionally cache public npmjs so one registry serves everything (and works offline).
- Container registry (for the Portainer image), Go module proxy, Swift package registry, generic files.

## Everything else

| Area | What it does |
|---|---|
| Import | Pull repos, issues, PRs, releases, wikis and workflows from Gitea/Forgejo, GitHub or any git URL, into your personal or company account |
| Backups and storage | Nightly backups to other devices (via runners) and S3, retention, one-repo or full restore; disk usage; Git LFS above a size threshold |
| Keys | Passkeys, Sign in with GitHub, SSH keys (`ssh://git@<domain>:2222/…`), scoped access tokens |
| Secrets and variables | Encrypted per repo, inherited per account, environments with approval (e.g. production) |
| Branch rules | PRs required, checks must pass, notes resolved, no force pushes; agents can't merge or push to protected branches |
| Notifications | Inbox filtered by Actions, agents, runners, Tether and releases; push to phone and Mac |
| Profiles | Personal and company profiles with README, pinned repos and Embers |
| Projects | Kanban, table and timeline across repos |
| Pages | Static sites from a branch or an Actions build, PR previews, custom domains served by your nginx |
| Web editor | Quick edits in the browser; "open workspace on a device" through a runner |
| Security | Secret scanning (push protection and Actions logs), dependency update PRs |
| Snippets | Small versioned code snippets, optionally mirrored as GitHub gists |
| ⌘K palette | Jump anywhere, run workflows, search GitHub |
| Mobile | Installable app: what needs you, running jobs, quick repos |

## Addons: Duo, Team and Company

Atelier's core is built for one person and stays that way. When you need other people in, you switch on an addon in **Settings → Addons**. Each one builds on the one before it, and turning one off never deletes anyone's work.

| Addon | For | Adds |
|---|---|---|
| **Core** | Just you | One owner. Company accounts are separate shelves for repos, not separate people |
| **Duo** | You and one other person | A second sign-in, per-repo sharing (read / write), mentions and review requests between you, a shared feed |
| **Team** | Small groups | People and teams inside an account, roles (owner, maintainer, member, read-only), invites, required reviewers, CODEOWNERS |
| **Company** | Organisations | SSO (OpenID Connect: Google, Microsoft, Okta…), audit log, required passkeys, org-wide policies for secrets, runners and branch rules |

How it's built:
- **Every action records an actor** (a person, a token, a bot or an agent), so history stays correct when more people arrive.
- **Sessions, tokens, SSH keys and passkeys belong to a person.** In Core there is only ever one.
- **One permission check** answers "can this person do this to this repo?". Core always says yes for the owner. The addons plug their rules into that same check.

## Running it

### Local development

You need Go 1.27+ and `git` on your `PATH`.

```sh
make run          # ATELIER_DATA=./data go run ./cmd/atelier
```

Open http://localhost:3000. The first visit goes to **Set up Atelier**, where you pick your username and password. That creates your personal account and signs you in. Everything lives in `./data`: `atelier.db` (SQLite) and `repos/` (bare git repositories).

Other targets: `make test`, `make build` (writes `bin/atelier`), `make docker` (builds `atelier:latest`). `atelier -version` prints the build version.

### Pushing your first repo

1. Create a repository with **New** (under your personal account or a company account).
2. Create an access token under **Settings → Access tokens**. Choose **read** (clone/fetch) or **write** (also push) and an expiry. The token (`atl_…`) is shown once. Only its hash is stored.
3. Push over HTTPS and use the token as the password (any username works):

   ```sh
   git remote add origin http://localhost:3000/<you>/<repo>.git
   git push -u origin main
   # Username: <you>   Password: atl_…
   ```

   Your OS credential helper (e.g. the macOS keychain) remembers it after the first push. Public repos can be cloned without a token. Private repos and every push need one.

### Docker and Portainer

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

### Continue with GitHub

1. On GitHub, go to **Settings → Developer settings → OAuth Apps → New OAuth App**.
2. Set **Homepage URL** to your `ATELIER_PUBLIC_URL`. Set **Authorization callback URL** to `<ATELIER_PUBLIC_URL>/auth/github/callback`.
3. Copy the client ID and generate a client secret.
4. Set `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET` and `ATELIER_GITHUB_LOGIN` (your GitHub username) in the stack, then redeploy. The sign-in page then shows **Continue with GitHub**. Only that one GitHub account can sign in. Password sign-in keeps working.

### Backups and restore

Atelier takes a snapshot every day at `ATELIER_BACKUP_TIME` (default `03:00`, server local time; `off` disables it), and whenever you click **Back up now** under **Settings → Backups and storage**. A snapshot is one `atelier-<time>.tar.gz` holding a consistent copy of the database (`VACUUM INTO`), a `git bundle` of every repository and a `manifest.json` with sizes and checksums. Snapshots go to `ATELIER_BACKUP_DIR` (default `<data>/backups`; the stack mounts a separate `atelier-backups` volume at `/backups`) and, if `ATELIER_BACKUP_S3_ENDPOINT`, `_BUCKET`, `_ACCESS_KEY` and `_SECRET_KEY` are set, to any S3-compatible bucket too (`_PREFIX` and `_REGION` are optional). Retention keeps 14 daily, 8 weekly and 6 monthly snapshots (`ATELIER_BACKUP_KEEP_DAILY`/`_WEEKLY`/`_MONTHLY`) in both places. The settings page shows history, lets you download snapshots and shows disk usage. `atelier backup` takes one from the shell.

Restore from the server's shell. Whatever is replaced is moved aside to `pre-restore-<time>`, never deleted:

```sh
# one repository; Atelier can keep running
docker exec -it atelier atelier restore /backups/atelier-20261110-030000.tar.gz --repo demi/atelier
# everything; stop Atelier first (restore refuses while it runs unless you pass --force)
docker stop atelier
docker run --rm -v atelier_atelier-data:/data -v atelier_atelier-backups:/backups atelier:latest restore /backups/atelier-20261110-030000.tar.gz
docker start atelier
```

### Importing repositories

**Settings → Import** brings repositories in from Gitea/Forgejo (server URL and token: your own and your organizations' repositories), GitHub (token) or any git URL (optional username and token), into your personal account or a company account. Pick the repositories and their names; they import in the background, with visibility and descriptions carried over. Tick **Keep syncing as a mirror** to fetch from the source every `ATELIER_MIRROR_INTERVAL` minutes (default 30). Only mirrors store their token (in the database, never shown again); one-off imports forget it when they finish. Issues and pull requests aren't imported yet.

## Deploying behind nginx

See [`deploy/nginx/atelier.conf`](deploy/nginx/atelier.conf). The important parts are:

- `client_max_body_size 0` so large pushes aren't rejected
- `proxy_buffering off` and `proxy_request_buffering off` so git pack data streams
- WebSocket upgrade headers for live Actions logs, runners and MCP
- `X-Forwarded-Proto` and `X-Forwarded-Host`, which Atelier trusts when "Behind a reverse proxy" is on, so clone URLs show `https://`
- SSH isn't HTTP, so either publish port 2222 straight from the container or add an nginx `stream {}` block for it
- Pages and PR previews use extra server names (`*.atelier.<domain>`) on the same proxy

## Planned stack

- **Go**: a single server binary that calls the real `git` binary for smart HTTP
- **SQLite**: one data file, no database server
- **Server-rendered UI** with a GitHub-like layout and Atelier's own look
- **Docker image** for the Portainer stack, plus a native `atelier-runner` per device

## Roadmap

| Phase | Scope | Status |
|---|---|---|
| 1 | Push/clone over HTTPS and SSH (behind nginx), code browser, commits and diffs, passkey + GitHub sign-in, keys and tokens, backups, import from Gitea/GitHub | Done |
| 2 | Issues, pull requests (new overview + Files changed), branch rules, notifications, company accounts and profiles | In progress |
| 3 | Actions (runners per device, YAML + agentic workflows, lockfile), secrets, releases, packages (npm, containers, Go, Swift), LFS | Planned |
| 4 | MCP server, Tether (search, feed, mirroring), apps and bots (bridge + native + webhooks), security scanning | Planned |
| 5 | Projects, Pages, web editor and device workspaces, snippets, ⌘K, mobile app, insights | Planned |
| Later | Atelier Connect: separate studios following, starring and sending pull requests to each other (ActivityPub / ForgeFed). Addons: Duo, Team, Company | Ideas |

**Phase 1: done**
- Push and clone over HTTPS with scoped, expiring access tokens, and over SSH with your SSH keys
- Code browser with README rendering, branches and tags; commit history and diffs
- Sign-in with a password, a passkey (needs HTTPS) or Continue with GitHub (locked to your account); `atelier reset-password` from the server
- Company accounts
- Nightly backups (local folder and optional S3) with retention, downloads and `atelier restore`
- Import from Gitea/Forgejo, GitHub or any git URL, optionally kept in sync as a mirror (pushes to mirrors are refused)
- Docker image and Portainer stack behind nginx or Nginx Proxy Manager

**Phase 2: done so far**
- Issues with comments, labels, milestones, search qualifiers and `#123` links
- Pull requests: compare, merge status bar, Overview / Files changed, notes to self, squash or merge commit
- Branch rules enforced on every push (HTTPS and SSH) by a server-side hook
- "Closes #N" in a pull request closes the issue when it merges

**Phase 2: still to do**
- Notifications inbox
- Profiles (profile README, pinned repos, Embers)
- Importing issues and pull requests from Gitea/GitHub

Design canvas: https://claude.ai/artifact/Pk6J4jME2YuBfMdhRHKX2q
