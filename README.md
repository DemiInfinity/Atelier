# Atelier

**Your own GitHub, on your own hardware, built for one person.**

Most code platforms are built for teams and companies. If you work alone, you use a fraction of them and carry the rest around in menus and settings. Atelier is a self-hosted code home with nothing in it you don't use: your repositories, issues, pull requests, automated checks and websites, running on a machine you own.

> **Status: early.** Atelier works, and its author uses it every day, but it is nowhere near finished. There is no installer yet, so you build it yourself, and things will change and break. See [Try it](#try-it).

## Why you might want it

- **It's yours.** One Docker stack and one database file on your own server. No account with us, no telemetry, no cloud in the middle.
- **Nothing leaves unless you say so.** Remote access is a switch, and it is off by default. Linking to GitHub is per repository.
- **You don't have to leave GitHub.** Link a repository and its pull requests, reviews and issues show up beside your own. Keep what's public there and the rest at home.
- **AI agents on your terms.** Off by default. Switch one on per repository, see what it may do, approve its first run. They run on your own machine with your own subscription.
- **One person, all of it.** No teams, seats, invites or admin screens to work around.

## What's in it

| | |
|---|---|
| **Code** | Repositories over HTTPS and SSH, a code browser, commits, diffs, branches, tags, a web editor |
| **Work** | Issues, pull requests with a merge status bar and a files view, branch rules, project boards, notifications |
| **Automation** | Actions that run GitHub-style workflows on your own devices, with every action pinned to an exact commit |
| **Shipping** | Releases, package registries (npm, containers, Go modules, files), static sites with Pages |
| **GitHub link (Tether)** | Search, feed, mirroring, issue and review sync, GitHub pull requests beside your own |
| **Agents** | Plain-language workflows that run Claude with an approval step and an allow-list of tools; an MCP server so your own AI tools can read and manage your studio |
| **Safety** | Passkeys, scoped tokens, encrypted secrets, secret scanning, nightly backups with restore |
| **Away from home** | Remote access through Cloudflare Tunnel or Tailscale, passkeys only, with a log of who connected |
| **Apps** | A desktop app and the `atl` command line (in [`clients/`](clients/README.md)), and an installable phone app |

Company accounts keep work for different clients or businesses apart. They are separate shelves for repositories, not separate people: only you sign in.

## How it fits together

```
 your devices                      your server
┌──────────────┐                 ┌──────────────────────────┐
│ laptop       │── runner ──┐    │  proxy  ─►  Atelier       │
│ other device │── runner ──┼──► │             (Go + SQLite) │──► GitHub (Tether, optional)
│ …            │── runner ──┘    │                           │
└──────────────┘                 └──────────────────────────┘
```

- **The server** is one Go program with one SQLite file. It runs in Docker behind your reverse proxy.
- **Runners** are a small program on each of your devices. They run your Actions jobs and only connect out to your server, so a laptop needs no open ports.

## Try it

You need Go 1.27 or newer and `git`.

```sh
git clone https://github.com/DemiInfinity/Atelier.git
cd Atelier
make run
```

Open http://localhost:3000. The first visit asks you to pick a username and password, and that's your studio. Everything it stores goes in `./data`.

To run it properly on a server with Docker, behind a proxy, with backups: see **[Installing and running Atelier](docs/install.md)**.

## Guides

- [Installing and running](docs/install.md): local, Docker, reverse proxy, GitHub sign-in, importing repositories
- [Runners](docs/runners.md): run Actions jobs on your own devices
- [Backups and restore](docs/backups.md)
- [Remote access](docs/remote-access.md): Cloudflare Tunnel or Tailscale
- [Pages](docs/pages.md): static sites
- [Agent workflow examples](docs/agents/)
- [Desktop app and `atl`](clients/README.md)
- [Roadmap](docs/roadmap.md): what's done and what's next

## Free and Pro

Atelier is free and open source for one person, and it will stay that way.

| | Free (this repository) | Atelier Pro (later) |
|---|---|---|
| People | One person, or two (Duo) | Team, Company |
| Company accounts | Up to 3 | More than 3 |
| Licence | AGPL-3.0 | Commercial |

Duo, Team and Company are planned and not built yet. Solo people shouldn't pay for features made for organisations, so the paid edition starts where the organisation does.

## Contributing

Atelier is not open to contributions yet: no pull requests, issues or feature requests for now. We'll say here when that changes. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Licence

Copyright © 2026 DemiInfinity.

Atelier is free software under the [GNU Affero General Public License v3.0](LICENSE): you may use, run, study, change and share it, and if you offer a changed version to others, including over a network, you must make your changes available under the same licence.

**Credit is required.** If you share Atelier or something built from it, or run a changed version for other people, keep the copyright notices and show the credit "Based on Atelier by DemiInfinity" with a link to this repository, and mark your version as changed. The exact terms are in [NOTICE](NOTICE).

The Team and Company editions (Atelier Pro) are separate, are not in this repository, and are under a commercial licence.
