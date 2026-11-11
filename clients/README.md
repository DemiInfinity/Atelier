# Atelier clients

Rust clients for an Atelier studio, as a Cargo workspace:

| Crate | What it is |
| --- | --- |
| `atelier-core` | Library: the app API client, live events, the runner's control socket, local git helpers, device sign-in, friendly messages. Shared by `atl` and the desktop app. |
| `atl` | The command-line tool. |

The contract with the server is [`docs/app-api.md`](../docs/app-api.md).

## Building

Needs Rust 1.82 or newer, plus `git` and `ssh-keygen` on the `PATH` at run time.

```sh
cd clients
cargo build --release          # target/release/atl
cargo test                     # mock studio, temp folders; no network, no keychain
cargo fmt --check && cargo clippy --all-targets -- -D warnings
```

On Linux the keychain is the Secret Service, which needs the D-Bus
development package to build (`libdbus-1-dev` on Debian/Ubuntu).

## Installing `atl`

```sh
cargo install --path clients/atl
atl completions zsh > ~/.zfunc/_atl      # or bash, fish, powershell, elvish
```

## Commands

```
atl login <studio>            sign this machine in (you approve it in the browser)
atl logout | atl whoami
atl status                    what needs you, what is running, where you are
atl repo list
atl repo clone NAME           from your studio; --github owner/name mirrors it first
atl repo publish              turn this folder into a repository
                              (--owner --name --public --description --yes --replace-origin --https)
atl repo rename NEW           a new name in your studio; this folder's remote follows (-R owner/name)
atl repo delete               from your studio only, after you type its name (--yes owner/name for scripts)
atl pr [N]                    pull requests here (-R owner/name, --all, --state)
atl pr merge N                [--when-green] [--method squash|merge|rebase] [--delete-branch] [--cancel]
atl pr update [N]             merge the base branch into the pull request, in your studio
                              (default: the open one for this branch; conflicts exit 1 and list the files)
atl issue [list]              this repository's issues (--state open|closed|all, -R owner/name)
atl issue view N | new TITLE [-b BODY] | close N
atl run [list]                recent runs (--limit, -R, --runner)
atl run log ID                the log of a run's failed jobs (--job: ID is a job id; --tail N)
atl run watch                 what is running, live
atl agent [list]              agents waiting for you
atl agent approve ID          shows tools, engine and limits, then asks (--yes, --follow)
atl open [REPO]               the working copy, in your editor
atl remote [status|on|off|who|link]
atl say [list MOMENT | add MOMENT "text" | remove MOMENT N | tone friendly|plain|off | off]
atl runner [status|pause|resume]     the runner on this machine
atl completions <shell>
```

Every command takes `--json` and then prints one JSON document on standard
output (`atl run watch --json` prints one object per line). Errors are one
plain sentence and the next step on standard error; with `--json`,
`{"error","message","hint"}`.

Exit codes: `0` fine, `1` error, `2` usage, `3` not signed in.

Colour is used only on a terminal and never with `NO_COLOR` set. Without a
terminal nothing is ever asked: commands that need a yes (`repo publish`,
`agent approve`) want `--yes`.

## Configuration

`config.toml` lives in `~/Library/Application Support/atelier/` (macOS),
`%APPDATA%\atelier\` (Windows) or `$XDG_CONFIG_HOME/atelier/` (Linux):

```toml
studio = "https://atelier.example.lan"
device_id = 7
device_name = "MacBook"
clone_root = "~/Atelier"          # where atl repo clone puts things: <root>/<owner>/<name>
watch = ["~/Projects"]            # also looked through for working copies
editor = "vscode"                 # vscode, cursor, zed, sublime, fork, xcode, or any command
git_tool = "fork"
runner_config = "~/Library/Application Support/atelier-runner/config.json"
ca_file = "/path/rootCA.pem"      # extra CA roots, for a private CA
```

`atl login` writes `studio`, `device_id` and `device_name`; the rest is yours.
Keys this version doesn't know are kept.

The device token is kept in the OS keychain (service `atelier`, account = the
studio URL), never in `config.toml`. Where there is no keychain it goes to
`tokens.json` next to the config, readable only by you, and `atl login` says so.

| Variable | Effect |
| --- | --- |
| `ATELIER_STUDIO`, `ATELIER_TOKEN` | Override the configured studio and the stored token (scripts, CI). |
| `SSL_CERT_FILE` | An extra CA bundle, on top of `ca_file` and the system roots. |
| `NO_COLOR` | No colour. |
| `ATELIER_CONFIG_DIR` | Use this folder instead of the per-OS one. |
| `ATELIER_TOKEN_STORE=file` | Skip the keychain; use `tokens.json`. |
| `ATELIER_SSH_DIR` | Where SSH keys are looked for and made (default `~/.ssh`). |
| `ATELIER_RUNNER_SOCKET` | The runner's control socket (default: `control.sock` next to `runner_config`). |
| `ATELIER_NO_BROWSER` | `atl login` prints the approval link instead of opening it. |
| `ATELIER_MESSAGE_SEED` | Makes the choice of friendly message repeatable. |

## SSH

`atl login` registers an SSH key for this device: `~/.ssh/id_ed25519.pub` if
there is one, otherwise a new dedicated key `~/.ssh/atelier_device_ed25519`
(no passphrase). When the dedicated key is in use, `atl repo clone` and
`atl repo publish` record it in that repository's own `.git/config`
(`core.sshCommand`), so plain `git push` keeps working. Nothing in `~/.ssh/config`
or your global git config is changed.
