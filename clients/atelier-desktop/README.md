# Atelier Desktop

Your studio's companion on this machine: what needs you today, your working
copies (changes, commits, history, branches), pull requests, the runner on
this device, adding repositories, settings, friendly messages, and a menu bar
icon with a small popover. A [Tauri v2](https://tauri.app) app: a Rust
backend built on [`atelier-core`](../atelier-core), and a small web frontend
in plain TypeScript (no UI framework) bundled by Vite.

The contract with the server is [`docs/app-api.md`](../../docs/app-api.md).

## Run and build

Needs Rust 1.82+, Node 22+, and `git` and `ssh-keygen` on the `PATH`. On macOS
the Xcode Command Line Tools are enough.

```sh
cd clients/atelier-desktop
npm install                      # once; the Tauri CLI is a dev dependency
npm run tauri dev                # the app with live reload
npm run tauri build -- --debug   # ../target/debug/bundle/macos/Atelier.app
npm run tauri build              # the release bundle (../target/release/bundle)
```

Checks:

```sh
npm run build && npm test                     # typecheck, bundle, vitest
cd .. && cargo fmt --check \
  && cargo clippy --all-targets -- -D warnings \
  && cargo test                               # whole workspace
```

The Rust tests run the command layer against a mock studio (wiremock) and
throwaway git repositories; they never touch the real keychain, `~/.ssh`,
your config or the network.

### Trying it without your studio

```sh
(cd .. && cargo build -p atelier-desktop --example mock_studio)
npm run tauri build -- --debug
scripts/try-with-mock.sh             # look around by hand
scripts/try-with-mock.sh selftest    # scripted walk through every screen
scripts/try-with-mock.sh selftest atl  # the same, starting as someone who signed in with atl
```

`try-with-mock.sh` starts `examples/mock_studio.rs` (the contract's example
answers, a device flow that approves itself, live events, a fake runner
socket, repositories that can be renamed and deleted, issues that can be
opened, commented on and closed, a pull request that is behind its base and
one that conflicts with it) and the app in a throwaway home with its own config, token file, SSH
folder and two working copies. `curl -X POST http://127.0.0.1:4780/_mock/notify`
makes a pop-up appear. The mock studio starts, like a real one, with this
device not allowed to rename or delete repositories;
`curl -X POST http://127.0.0.1:4780/_mock/admin/on` (or `/off`) is the switch
of Settings → Devices, and `mock_studio --can-admin` starts with it on. The
self-test types the address, clicks through first run, visits each screen and
prints what it sees as `[selftest] …` lines: a repository opening on History,
its own pull requests with their checks, issues, runs by pull request, pinning,
folding an owner's group, the refusal to rename, then (once the harness has
flipped the switch) a rename, a delete with the name typed, and the owner
chooser when publishing. Afterwards the script prints each folder's `origin`
and `[desktop]` from the config, to show that the folders are still there.

Environment (on top of the ones in [`../README.md`](../README.md), which the
app honours too: `ATELIER_CONFIG_DIR`, `ATELIER_TOKEN_STORE=file`,
`ATELIER_SSH_DIR`, `ATELIER_RUNNER_SOCKET`, `ATELIER_NO_BROWSER`,
`ATELIER_MESSAGE_SEED`):

| Variable | Effect |
| --- | --- |
| `ATELIER_DESKTOP_DRY_RUN` | Links, editor launches and native notifications are printed instead of done; the login item is left alone. |
| `ATELIER_DESKTOP_PICK` | A folder to answer the folder chooser with (no dialog). |
| `ATELIER_DESKTOP_SELFTEST` | A studio address: runs the scripted walk-through in the window. |

## How it is put together

```
src-tauri/src/
  desk.rs       Desk: settings, token store, studio client, sign-in, links.
                No Tauri in it, so tests drive it directly.
  today.rs      Today / sidebar / popover data (needs, working copies, feed)
  repo.rs       one working copy: changes, diffs, commit, history, branches
  manage.rs     rename / delete in the studio, pins, folded groups, runs
                by pull request
  pulls.rs      pull requests       device.rs   the runner and its jobs
  issues.rs     a repository's issues
  markdown.rs   Markdown → a typed tree (never HTML) for the window to draw
  add.rs        publish / clone / start new
  studio.rs     remote access, messages, the app's own settings
  notify.rs     a notify event → the words, colour and target of a pop-up
  tools.rs      the fixed table of editors and git tools
  paths.rs      which folders may be touched
  prefs.rs      [desktop] in the shared config.toml
  commands.rs   #[tauri::command] wrappers, one line each
  shell.rs      windows, menu bar icon and popover, background tasks
src/
  api.ts        every command and data shape (the contract with Rust)
  shell.ts      main window: sidebar, router, toasts, dialogs  → ctx.ts
  sideview.ts   the sidebar's repositories: Pinned, a group per owner
  manage.ts     pin, rename, delete: shared by the sidebar and the bar
  markdown.ts   draws that tree; Write / Preview over a text box
  screens/*.ts  one module per screen       tray.ts  the popover
                (repo.ts and its tabs: repo-history, repo-changes,
                pulls.ts scoped to the repository, repo-issues,
                repo-branches, repo-runs)
  logic/*.ts    pure view logic with tests  dom.ts   element builder
  styles/*.css  tokens in base.css, one file per screen
```

- **Everything goes through `atelier-core`**: config, the keychain, the HTTP
  client, the event stream, the runner socket, git, SSH keys, messages.
- **The windows never see the token and never make HTTP calls.** They call
  commands; the CSP allows nothing but the app's own files and IPC. Their
  only Tauri permissions are events and window dragging.
- **One runtime, two background tasks** (started at sign-in): one owns the
  studio's `EventStream` and emits `notify` (already dressed as a pop-up),
  `status` and `remote` to the windows; one asks the runner socket every two
  seconds while a window or the popover is visible and emits `runner` when
  the answer changes. A third refreshes the menu bar dot every minute.
- **Safety.** Editors and tools are a built-in table launched without a
  shell. Every repository path is resolved and must be inside the clone root,
  a watch folder or a folder picked with the chooser. Only the studio's own
  origin and `https://github.com` are ever opened in the browser. Text from
  the studio and from git is only ever put in the page as text. Commits take
  exactly the ticked files (`git commit --only`); discard and undo are the
  only destructive actions and the window asks first.
- **Finding working copies.** The clone folder, the watch folders and
  picked folders are scanned for git folders. One is a working copy of a
  studio repository when *any* of its remotes points at it, whatever the
  remote is called (the branch's upstream remote wins, then `origin`);
  owner and name are compared without regard to case. Git folders that
  aren't in the studio are listed under "Other folders". Someone who signed
  in with `atl` sees "Find your folders" once, with folders in their home
  folder that contain git repositories suggested.
- **A repository** opens on History, always; its tabs are History, Changes,
  Pull requests, Issues, Branches and Actions (the last four only for a
  repository in the studio). Pull requests there are that repository's; the
  sidebar's "Pull requests" is every repository's. Each row carries its
  checks at a glance (`3 of 4 passed`, `1 failing`, `running`, `waiting for
  you`, `no checks`); jobs and logs stay a click away. Actions gathers runs
  under the open pull request whose branch they ran on: the studio doesn't
  say which pull request a run belongs to, so the branch decides.
- **Rename and delete** go to the studio (`PATCH` / `DELETE
  /repos/{owner}/{name}`). After a rename every remote of the folder that
  named the old address is pointed at the new one, keeping the rest of the
  address. Delete needs the repository's name typed, removes it from the
  studio only and never touches the folder, which then shows under "Other
  folders". A studio lets a device do either only once its owner switched
  that on in Settings → Devices (`device.can_admin` in `/me`, read again when
  the window comes back); until then the app says so and offers that page.
- **Markdown** in issue bodies and in issue and pull request comments is
  drawn, not shown as source. Rust parses it (`pulldown-cmark`: CommonMark,
  tables, strikethrough, task lists) into a small typed tree
  (`markdown.rs`); the window builds a fixed set of elements from that tree
  with `createElement` and `textContent` (`src/markdown.ts`). There is no
  node for raw HTML, so markup in the source is only ever text; nothing
  assigns `innerHTML` and the CSP is unchanged. Links are kept only for
  `http`, `https` and `mailto` addresses without credentials (anything else
  is its words), have no `href`, and go through `open_link`: the studio and
  GitHub open straight away, any other address is shown and asked about
  first. Images are never loaded: an image is its alt text, linking to the
  picture. `#12` goes to that issue or pull request, `@name` is styled text.
  The comment box and the new issue box have Write / Preview, through the
  same renderer. Titles and label names stay plain text; a label's colour is
  checked to be a hex colour in Rust before it tints a dot.
- **Update from the base.** A pull request that is behind its base says so
  on its row (`3 behind master`) and, when the studio says it can be
  updated, has "Update from master": the studio merges the base into the
  branch (`POST …/pulls/{n}/update`). Afterwards the pull request and its
  checks are read again; if the folder here is on that branch the app says
  it is now behind and offers Pull (never pulled unasked). Conflicts come
  back as a dialog with the files, the steps to resolve them by hand and
  "Open in <editor>"; a pull request the studio already knows to conflict
  shows a red "Conflicts", that same guidance in place of the button, and
  can't be merged. A push that landed meanwhile offers "Try again"; a
  repository at home on GitHub gets "Open on GitHub" instead.
- **State** lives where `atl` keeps it: `config.toml` (the app's own
  preferences under `[desktop]`: `pinned`, `collapsed` owner groups,
  `default_owner` for "This belongs to", …) and the keychain.

### Commands

| Area | Commands |
| --- | --- |
| App, first run | `app_state` `connect_start` `connect_poll` `connect_reopen` `connect_cancel` `find_folders` `suggest_folders` `add_suggested_folders` `folders_done` `runner_probe` `disconnect` |
| Today | `overview` `run_log` |
| Repository | `repo_view` `repo_changes` `repo_diff` `repo_commit` `repo_discard` `repo_undo_commit` `repo_history` `repo_commit_detail` `repo_branches` `repo_switch` `repo_new_branch` `repo_fetch` `repo_pull` `repo_push` `repo_open` `repo_hand_over` |
| Links | `open_web` `open_studio` `open_github` `open_link` `markdown_preview` |
| Looking after a repository | `repo_rename` `repo_delete` `repo_set_pinned` `set_group_collapsed` `repo_runs` |
| Issues | `issues_list` `issue_detail` `issue_create` `issue_comment` `issue_set_open` |
| Pull requests | `pulls_list` `pull_detail` `pull_merge` `pull_update` `pull_cancel_merge` `pull_checkout` `agent_approve` |
| This device | `device_view` `runner_set_taking` `runner_set_rule` `runner_tail` `job_stop` |
| Folders | `add_watch_folder` `remove_watch_folder` `choose_clone_root` `open_folder` |
| Add a repository | `choose_folder` `folder_plan` `folder_init` `publish` `search_repos` `clone_repo` `mirror_and_clone` `create_new` |
| Settings | `remote_view` `remote_set` `remote_link` `set_editor` `set_git_tool` `set_notify_pref` `set_app_pref` |
| Messages | `messages_view` `messages_set_tone` `messages_add` `messages_remove` `messages_set_rule` |
| Windows | `show_main` `hide_popover` `quit` (`selftest_*` for the walk-through) |

Events to the windows: `notify`, `status`, `remote`, `runner`, `connection`,
`signed-out`, `navigate` (main window), `popover-shown` (popover).

### Adding a screen

1. **Rust:** add what it needs to `Desk` (a new module if it is a new area),
   returning `Serialize` structs and `CmdResult`. Test it in
   `src-tauri/tests/` with `World` from `tests/common`.
2. Wrap it in `commands.rs` and list the command in `generate_handler!` in
   `shell.rs`.
3. **TypeScript:** add the shapes and the call to `src/api.ts`.
4. Add the route to `src/routes.ts`, write `src/screens/<name>.ts` exporting
   `(ctx, route) => Screen` with its styles in `src/styles/<name>.css`, and
   add the case to `Shell.make` in `src/shell.ts`. Build nodes with `h()`
   only; put anything worth testing in `src/logic/`.
5. Link to it from the sidebar or another screen with `ctx.go(...)`.

## Not in this version

- The runner is only shown and switched through its socket; the app never
  installs, starts or reconfigures it.
- "Also mirror it to GitHub" opens the repository's Tether settings in the
  studio after publishing: the app API has no call to create that mirror.
- "Never interrupt a full-screen app" is stored and synced but not enforced:
  detecting a full-screen app needs native calls this crate doesn't make.
- Clicking a native notification brings the window back on the right screen
  only where the system reports the click as a re-open (macOS); the in-app
  pop-ups always lead to the right place.

## Releasing

The debug and release bundles are unsigned. A release needs: a Developer ID
Application certificate and notarisation on macOS (`bundle.macOS.signingIdentity`
plus `APPLE_ID`/`APPLE_PASSWORD`/`APPLE_TEAM_ID` or an API key for
`tauri build`), the hardened runtime, and a `.dmg` target; an updater
(`tauri-plugin-updater` with a signing key and a feed, ideally served by the
studio); and Windows (code signing, the named-pipe runner client, launcher
table) and Linux (Secret Service, `libwebkit2gtk`, tray support varies)
builds, neither of which has been compiled or run yet. `macOSPrivateApi` is
on for the transparent popover, which rules out the Mac App Store.
