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
| 1 | Push/clone over HTTPS and SSH (behind nginx), code browser, commits and diffs, passkey + GitHub sign-in, keys and tokens, backups, import from Gitea/GitHub | Planned |
| 2 | Issues, pull requests (new overview + Files changed), branch rules, notifications, company accounts and profiles | Planned |
| 3 | Actions (runners per device, YAML + agentic workflows, lockfile), secrets, releases, packages (npm, containers, Go, Swift), LFS | Planned |
| 4 | MCP server, Tether (search, feed, mirroring), apps and bots (bridge + native + webhooks), security scanning | Planned |
| 5 | Projects, Pages, web editor and device workspaces, snippets, ⌘K, mobile app, insights | Planned |

Design canvas: https://claude.ai/artifact/Pk6J4jME2YuBfMdhRHKX2q
