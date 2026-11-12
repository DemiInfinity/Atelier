# Roadmap

| Phase | Scope | Status |
|---|---|---|
| 1 | Push/clone over HTTPS and SSH (behind nginx), code browser, commits and diffs, passkey + GitHub sign-in, keys and tokens, backups, import from Gitea/GitHub | Done |
| 2 | Issues, pull requests (new overview + Files changed), branch rules, notifications, company accounts and profiles | Done |
| 3 | Actions (runners per device, YAML workflows, lockfile, checks), secrets, releases, packages (npm, containers, Go), LFS, push to create | Done |
| 4 | MCP server, agentic workflows, Tether (search, feed, mirroring), apps and bots (bridge + native + webhooks), security scanning | Done |
| 5 | Projects, Pages, web editor and device workspaces, snippets, ⌘K, mobile app, insights | Done |
| 6 | GitHub repos and profiles inside Atelier (with the Atelier badge), Claude Code engine for agent workflows, HTTPS, remote access (Cloudflare Tunnel, Tailscale), Atelier Connect (off by default), the desktop app and the `atl` command line | Done |
| Next | A full git client in the desktop app (staging by line, graph, rebase, conflicts, worktrees), an installer, signed apps, Windows and Linux apps | In progress |
| Later | Addons: Duo (free), Team and Company (Atelier Pro), a Client addon for freelancers | Ideas |

**Phase 1: done**
- Push and clone over HTTPS with scoped, expiring access tokens, and over SSH with your SSH keys
- Code browser with README rendering, branches and tags; commit history and diffs
- Sign-in with a password, a passkey (needs HTTPS) or Continue with GitHub (locked to your account); `atelier reset-password` from the server
- Company accounts
- Nightly backups (local folder and optional S3) with retention, downloads and `atelier restore`
- Import from Gitea/Forgejo, GitHub or any git URL, optionally kept in sync as a mirror (pushes to mirrors are refused)
- Docker image and Portainer stack behind nginx or Nginx Proxy Manager

**Phase 2: done**
- Issues with comments, labels, milestones, search qualifiers and `#123` links
- Pull requests: compare, merge status bar, Overview / Files changed, notes to self, squash or merge commit
- Branch rules enforced on every push (HTTPS and SSH) by a server-side hook
- "Closes #N" in a pull request closes the issue when it merges
- Notifications inbox with a bell and unread count; you're never notified about your own actions
- Profiles for you and company accounts: profile README, pinned repositories, Embers
- Importing issues, pull requests, labels and milestones from Gitea/Forgejo and GitHub, keeping the original numbers (the token needs read access to issues and pull requests)

**Phase 3: done**
- Actions: GitHub Actions–style YAML workflows in `.atelier/workflows/` (or `.github/workflows/`), triggered by push, pull request, schedule, manual run or release; matrix, needs, environments, live logs, re-run, cancel, approvals
- `atelier-runner` for macOS, Linux and Windows: runs jobs on your own devices, installs as a service, masks secrets, cleans up after each job
- Lockfile pinning every action to a commit (`.atelier/actions.lock`), and checks in the pull request status bar that branch rules can require
- Encrypted secrets and variables per repository and account, and environments with approvals and runner restrictions (key: `ATELIER_SECRET_KEY`, kept out of backups — keep your own copy)
- Releases with notes, assets and an upload API; Git LFS with file locking
- Package registries: npm (with optional public-npm cache), containers (OCI, needs HTTPS for Docker), Go module proxy, generic files
- Push to create: pushing to a repository that doesn't exist creates it (private); Settings → Repositories turns it off
- Agentic (plain-English) workflows moved to Phase 4, alongside the MCP server they need

**Phase 4: done**
- MCP server at `/mcp` (33 tools) with MCP tokens and a call log: Settings → MCP
- Agentic workflows: Markdown files in `.atelier/workflows/` run Claude on your own runners, with an approval gate, a tool allowlist and turn/token/time limits (`engine: claude` needs an `ANTHROPIC_API_KEY` secret; `engine: claude-code` runs Claude Code on a runner where you're signed in, so it uses your Claude subscription and needs no key: `atelier-runner claude-code enable`); templates in `docs/agents/`
- Tether: connect GitHub, search and feed, popular this week, repo links (home Atelier or GitHub), issue and comment sync, checks for GitHub pull requests, Merge on GitHub, and the opt-in "I use Atelier" badge
- Apps and bots: GitHub-style signed webhooks, bot accounts (comment, suggest, checks, push-only git access, never merge), an `/api/v3` subset, Review on GitHub through Tether
- Security: secret scanning with push protection (`.atelier/secret-scanning.yml` allowlist), masking in Actions logs, history scans, dependency update pull requests with OSV advisories
- Move or rename repositories between your accounts, with redirects from the old address

**Phase 5: done**
- Projects: boards, tables and roadmaps across repositories, custom fields, URL filters, saved views and automation
- Pages: static sites from a branch folder or an Actions workflow (`atelier/deploy-pages`), on their own origin (`ATELIER_PAGES_URL`); see `docs/pages.md`
- Web editor (edit, create, upload, rename, delete, commit or open a pull request; the same push checks apply) and device workspaces ("Open on…" clones a repository onto one of your runners and opens it in your editor: `atelier-runner workspaces enable`)
- Snippets: multi-file, versioned, cloneable over HTTPS
- ⌘K command palette for repositories, files, issues, snippets, commands and accounts
- Installable mobile app (PWA; installing needs HTTPS) and a phone-friendly layout
- Insights per repository (pulse, commits, code frequency, contributors, languages, Actions usage) and for your account

**Built to grow:** sessions, tokens, SSH keys and passkeys belong to a person, and one permission check covers the web, HTTPS and SSH, so the Duo / Team / Company addons can plug in later.
