# App API (desktop app and `atl` CLI)

The contract between the Atelier server, the Rust clients in `clients/`
(`atelier-core`, `atl`, `atelier-desktop`) and `atelier-runner`'s local control
channel. Everything here is versioned under `/api/app/v1`. JSON in, JSON out,
UTF-8, times as RFC 3339 UTC strings. Errors are
`{"error": "<machine_code>", "message": "<one plain sentence>"}` with a 4xx/5xx
status.

## 1. Devices and sign-in

A device (a desktop app or a CLI on one machine) never sees the owner's
password. It gets its own token after the owner approves it in the browser.

### Start
`POST /api/app/v1/device/start` (no auth, rate-limited per IP)
```json
{"name": "MacBook", "kind": "desktop", "os": "macos", "version": "0.1.0"}
```
`kind` is `desktop` or `cli`. Response `200`:
```json
{"device_code": "<64 hex, secret>", "user_code": "AMBER-4127",
 "verify_url": "https://studio.example/settings/devices/approve?code=AMBER-4127",
 "expires_in": 600, "interval": 2}
```
`user_code` is `<WORD>-<4 digits>`, WORD from a fixed list of 32 short words.

### Approve (browser)
`GET /settings/devices/approve?code=…` needs a signed-in owner session; shows the
device name, kind, OS and the code, with Approve and Deny. `POST` the same path
with `decision=approve|deny`. Through the remote-access tunnel this page is
refused (approve at home only).

### Poll
`POST /api/app/v1/device/poll` `{"device_code": "…"}`
- `202 {"status":"pending"}` until decided
- `200 {"token":"atd_…","device_id":7,"person":{"name":"Demi","login":"DemiInfinity"},"studio":{"name":"Atelier","url":"https://studio.example","version":"7f82d31"}}` once, on approval (the token is shown once; only its hash is stored)
- `410 {"error":"denied"}` or `{"error":"expired"}`

### Device tokens
`atd_` + 40 base62. Sent as `Authorization: Bearer atd_…`. Resolves to
`Actor{Kind:"token", Name:"device:<name>", PersonID, Scopes:["app","repo:read","repo:write"]}`.
A device token can do what the owner's read/write token can, plus the `app`
endpoints below. It can never administer repositories, change settings, manage
tokens/keys/passkeys, or approve other devices. Settings → Devices lists
devices (name, kind, OS, last seen, last IP) with Revoke. A revoked token gets
`401 {"error":"revoked"}`. Ordinary access tokens may also call the app API
when they carry the `app` scope.

### This device's SSH key
`POST /api/app/v1/device/ssh-key` `{"title":"MacBook (Atelier Desktop)","key":"ssh-ed25519 AAAA…"}`
→ `201 {"id":12,"fingerprint":"SHA256:…"}`; the same key again → `200` with the
existing id. Keys added this way are removed when the device is revoked.

## 2. Who and where

`GET /api/app/v1/me` →
```json
{"person":{"name":"Demi","login":"DemiInfinity","avatar_url":"/-/avatar/DemiInfinity/160.png"},
 "accounts":[{"name":"DemiInfinity","display":"Demi","kind":"user","commit_name":"Demi","commit_email":"demi@example.com"},
             {"name":"elyndrastudios","display":"Elyndra Studios LTD","kind":"company","commit_name":"Demi","commit_email":"demi@company.example"}],
 "studio":{"name":"Atelier","url":"https://studio.example","version":"7f82d31",
           "ssh":{"host":"studio.example","port":2223},"tether":true,"connect":false},
 "device":{"id":7,"name":"MacBook","kind":"desktop","can_admin":false}}
```

## 3. Status ("Today")

`GET /api/app/v1/status` →
```json
{"needs":[
   {"id":"agent:31","kind":"agent_approval","thread":"agents","title":"Approve weekly-tidy before it runs",
    "detail":"agent · wants issues.close and issues.comment","repo":"DemiInfinity/Atelier",
    "web_path":"/DemiInfinity/Atelier/actions/runs/31","action":{"label":"Review","type":"approve_agent","job_id":31}},
   {"id":"run:208","kind":"run_failed","thread":"yours","title":"ci failed on feature/ssh",
    "detail":"test job · ran on Portainer host","repo":"DemiInfinity/Atelier",
    "web_path":"/DemiInfinity/Atelier/actions/runs/208","action":{"label":"Open log","type":"open_run","run_id":208}},
   {"id":"pull:Tray:9","kind":"pull_ready","thread":"yours","title":"Security update is ready to merge",
    "detail":"okhttp 5.1 fixes 1 advisory · checks passed","repo":"DemiInfinity/Tray",
    "web_path":"/DemiInfinity/Tray/pulls/9","action":{"label":"Review","type":"open_pull","number":9}}],
 "running":[
   {"job_id":412,"run_id":209,"repo":"DemiInfinity/Atelier","workflow":"review-helper","job":"agent",
    "kind":"agent","engine":"claude-code","runner":"MacBook","started_at":"2026-11-11T09:14:02Z",
    "detail":"pull request 14 · turn 3 of 20"}],
 "feed":[{"thread":"yours","text":"Merged “Pages custom domains” into Atelier main","at":"…","web_path":"/…"},
         {"thread":"github","text":"A reviewer approved pull request 63 on sparky-ventures","at":"…","web_path":"/…"},
         {"thread":"connect","text":"Mara starred Atelier from her studio","at":"…","web_path":"/…"}],
 "unread":3}
```
`kind` ∈ `agent_approval | run_failed | pull_ready | pull_review | security_alert | preview_approval`.
`thread` ∈ `yours | github | agents | connect` (amber / blue / violet / violet).
`needs` is newest first, at most 30. Everything is filtered through `App.can`.
"Commits not pushed" is local; clients add it themselves.

## 4. Repositories

`GET /api/app/v1/repos` →
```json
{"repos":[{"full_name":"DemiInfinity/Atelier","owner":"DemiInfinity","owner_kind":"user","name":"Atelier",
  "private":true,"description":"…","default_branch":"main","language":"Go","language_color":"#00ADD8",
  "clone_https":"https://studio.example/DemiInfinity/Atelier.git",
  "clone_ssh":"ssh://git@studio.example:2223/DemiInfinity/Atelier.git",
  "github":{"repo":"DemiInfinity/Atelier","home":"atelier"},"open_pulls":1,"updated_at":"…"}]}
```
`github` is null when the repo isn't tethered. `GET /api/app/v1/repos?q=loop`
filters by name; `&github=1` also returns up to 10 GitHub hits through the
caller's Tether token as `{"github_hits":[{"full_name":"maralind/loopkit","description":"…","atelier_badge":true}]}`.

`POST /api/app/v1/repos` `{"owner":"DemiInfinity","name":"Ingle","private":true,"description":""}`
→ `201` with the repo object; `409 {"error":"name_taken"}`; `403` if the caller
can't create there. (Publishing a folder = this call, then `git push`.)

`POST /api/app/v1/repos/mirror` `{"github":"maralind/loopkit"}` → `202 {"full_name":"DemiInfinity/loopkit"}`
(the existing Tether "Mirror to studio").

`PATCH /api/app/v1/repos/{owner}/{name}` `{"name":"NewName"}` → `200` with the
repo object under its new name. The same rename as "Move or rename" on the
repository's settings page: the bare repository moves on disk, and the old
name keeps redirecting (web, clone URLs, this API). Needs admin on the
repository. `409 {"error":"name_taken"}`; `422 {"error":"invalid_name"}` (empty,
not a valid repository name, or a reserved word); `403` readable but not
admin; `404` not readable. Sending the current name answers `200` unchanged.

`DELETE /api/app/v1/repos/{owner}/{name}` `{"confirm":"owner/name"}` → `204`, no
body. Deletes the repository for good, like the settings page. Needs admin.
`confirm` must be the repository's full name (case-insensitive), otherwise
`422 {"error":"confirm_mismatch"}`; `403`/`404` as above.

**Who can rename and delete:** a device whose person switched on "Can rename
and delete repositories" for it in Settings → Devices (off by default, per
device), on repositories that person can administer when signed in on the web.
While it is off both endpoints answer
`403 {"error":"device_not_allowed","message":"This device isn't allowed to rename or delete repositories. Turn it on in Settings → Devices."}`.
`GET /me` says which it is in `device.can_admin`. The switch is a signed-in web
form (`POST /settings/devices/admin`, `id` and `on=1|0`); nothing in this API
sets it, and a device token can't post that form. It opens these two calls and
nothing else: the device's token keeps the scopes `app, repo:read, repo:write`
and still fails every other admin check (settings, secrets, branch rules,
tokens, moving between accounts). Ordinary access tokens with the `app` scope
have no such switch and get `403 forbidden`.

## 5. Pull requests

`GET /api/app/v1/pulls?state=open` → every open pull request the caller can read,
across repos: `{"pulls":[<pull summary>]}`. `&repo=owner/name` keeps it to one
repository (the same list as the per-repo path below; `404` if the caller can't
read it). No paging: at most 100 pull requests per answer.
`GET /api/app/v1/repos/{owner}/{repo}/pulls?state=open|closed|merged|all`.
`GET /api/app/v1/repos/{owner}/{repo}/pulls/{n}` →
```json
{"repo":"DemiInfinity/Atelier","number":14,"title":"Remote access switch in the tray menu","state":"open",
 "head":"feature/tunnel-ui","base":"main","author":"owner:DemiInfinity","commits":2,"files":3,
 "via":null,
 "merge":{"can_merge":false,"reason":"1 check is still running and 1 agent is waiting for you.","methods":["squash","merge"],
          "on_github":false,"auto_merge":null},
 "checks":[{"name":"ci · test","status":"success","detail":"MacBook · 1m 48s","run_id":207,"job_id":410},
           {"name":"image · build","status":"pending","detail":"Portainer host · step 3 of 5","run_id":209,"job_id":413},
           {"name":"review-helper","status":"waiting","detail":"agent · needs your approval to run","run_id":210,"job_id":414,"action":"approve_agent"}],
 "comments":[{"author":"CodeRabbit","via":"tether","kind":"bot","body":"…","at":"…"}],
 "web_path":"/DemiInfinity/Atelier/pulls/14"}
```
Summary = the same object without `comments` and with `checks` reduced to
`{"total":4,"passed":2,"failed":0,"pending":1,"waiting":1,"state":"pending"}`,
counted from the checks of the head commit. `state` ∈ `none` (no checks) `|
failing` (any failed) `| pending` (any still running, or waiting for approval)
`| passing`. `pending` counts running and queued checks; a check waiting for
the person's approval is in `waiting` instead, so
`total = passed + failed + pending + waiting`. `status` ∈
`success | failure | pending | waiting | skipped`. `via` ∈ `null | "tether" | "connect" | "bot"`.

`POST …/pulls/{n}/merge` `{"method":"squash","when_green":false}`
- merges now → `200 {"merged":true,"sha":"…"}`; not mergeable → `409` with the reason
- `when_green:true` → `202 {"auto_merge":{"method":"squash","by":"device:MacBook"}}`:
  the server merges as soon as `can_merge` becomes true (re-checked on
  `run.completed`, check updates and approvals), and cancels it if a new commit
  is pushed by someone else, a check fails, or the pull request closes.
  `DELETE …/pulls/{n}/merge` cancels it.
- The same branch rules and "Merge on GitHub" behaviour as the web button.

## 5a. Issues

`GET /api/app/v1/issues?repo=owner/name&state=open|closed|all&page=1&per_page=30`
→ newest first:
```json
{"issues":[{"repo":"DemiInfinity/Atelier","number":21,"title":"Tray icon flickers","state":"open",
   "author":"owner:DemiInfinity","via":null,"labels":[{"name":"bug","color":"#d73a4a"}],"comments":2,
   "created_at":"…","updated_at":"…",
   "web_path":"/DemiInfinity/Atelier/issues/21","web_url":"https://studio.example/DemiInfinity/Atelier/issues/21"}],
 "has_more":false}
```
Without `repo` it lists across every repository the caller can read. `state`
defaults to `open` (`422 invalid_state` otherwise), `per_page` to 30 (at most
100). `author` is `<kind>:<name>` like a pull request's (`token:device:MacBook`
for an issue opened from a device); `via` as for pull requests. `state` is
`open` or `closed`. Pull requests are not issues and never appear here.

`GET /api/app/v1/repos/{owner}/{name}/issues/{number}` → the same object with
`"body"` (Markdown source) and `comments` as a list instead of a count:
`"comments":[{"id":88,"author":"CodeRabbit[bot]","via":"tether","kind":"bot","body":"…","created_at":"…"}]`
(oldest first; `author`, `via` and `kind` as in pull request comments).

`POST /api/app/v1/repos/{owner}/{name}/issues` `{"title":"…","body":"…"}` → `201`
with the full issue. `422 invalid_title` (one line, 1–256 characters),
`422 body_too_long` (65536 bytes).
`POST …/issues/{number}/comments` `{"body":"…"}` → `201` with the comment object;
`422 empty_body`.
`PATCH …/issues/{number}` `{"state":"open|closed"}` → `200` with the full issue
(closing closes as completed; asking for the state it already has changes
nothing); `422 invalid_state`.

Reading needs read access, the rest write access (`403`; `404` when the caller
can't read the repository, or there is no such issue). Changes are recorded as
the device (`device:MacBook`) and fire the same events as the web pages
(`issue.opened`, `issue.commented`, `issue.closed`, `issue.reopened`), so
notifications, agents and live events follow.

## 6. Runs and agents

`GET /api/app/v1/runs?limit=20&runner=<name>&repo=<owner/name>` →
`{"runs":[{"run_id":207,"repo":"…","workflow":"ci","event":"push","branch":"feature/tunnel-ui","status":"success","started_at":"…","duration_s":108,
  "jobs":[{"job_id":410,"name":"test","status":"success","runner":"MacBook","duration_s":108,"kind":"job"}]}]}`
`GET /api/app/v1/jobs/{id}/log?tail=200` → `{"lines":["…"],"truncated":true,"status":"running"}` (masked, as on the web).

`GET /api/app/v1/agents/waiting` → `{"jobs":[{"job_id":31,"run_id":210,"repo":"…","workflow":"weekly-tidy",
  "reason":"first run","engine":"claude-code","tools":["issues.list","issues.comment","issues.close"],
  "write_tools":["issues.comment","issues.close"],"max_turns":20,"timeout_minutes":10}]}`
`POST /api/app/v1/agents/jobs/{id}/approve` → `200 {"approved":true}` (the same
approval as the web button, recorded as approved by the device).
`POST /api/app/v1/jobs/{id}/cancel` → `200 {"cancelled":true}`.

## 7. Remote access

`GET /api/app/v1/remote` → `{"on":false,"mode":"quick","address":null,"exposed":["web","git"],"connected":0}`
`POST /api/app/v1/remote` `{"on":true}` → the same object (address may be null
for a few seconds; poll or wait for the `remote` event). `{"on":false}` cuts
everything at once. Refused with `403 {"error":"remote_only_off"}` when the
request itself arrives through the tunnel and tries to turn it **on** or change it.
`GET /api/app/v1/remote/who` → `{"sessions":[{"ip":"203.0.113.42","country":"NL","signed_in":true,"class":"web","since":"…"}]}`
`POST /api/app/v1/remote/link` → `201 {"url":"https://<tunnel host>/…","expires_in":900}` (one-time passkey link; only when on, only from home).

## 8. Live events

`GET /api/app/v1/events` — Server-Sent Events, one JSON object per `data:` line,
a comment line `: ping` every 25 s. Only events the caller may see.
```
event: notify
data: {"id":"n:5512","kind":"run_failed","thread":"yours","title":"ci failed on feature/ssh","detail":"Atelier · test","repo":"DemiInfinity/Atelier","web_path":"/…","moment":"fail","at":"…"}

event: status
data: {"changed":["needs","running"]}

event: remote
data: {"on":true,"address":"https://quiet-fox-lamp.trycloudflare.com","connected":0}
```
`notify.moment` ∈ `push | pass | fail | merge | agent | github | security | null`
— the pop-up "moment" the client may dress with a friendly message. The
client never gets a `notify` for its own device's actions.

## 9. Friendly messages

Stored per person so every device says the same things.
`GET /api/app/v1/messages` →
```json
{"tone":"friendly",
 "moments":{"push":{"ours":["Home and dry. {branch} is safe in your studio.","Off it goes. {count} commits landed on {repo}.","Pushed. Checks are warming up on {device}."],"yours":["Shipped. Go outside."]},
            "clean":{"ours":["All tidy. The kettle is free.","Nothing to commit. Go and make something."],"yours":[]},
            "pass":{"ours":["Green across the board on {device}.","Checks passed. A little smugness is allowed."],"yours":[]},
            "fail":{"ours":["One test disagrees with you on {branch}.","Not this time. The log knows why."],"yours":[]},
            "merge":{"ours":["Merged into main. Nicely done.","In it goes. {repo} is {count} commits better."],"yours":[]},
            "first":{"ours":["Good morning. First one of the day.","And we are off."],"yours":[]},
            "late":{"ours":["Pushed at {time}. It will still be here tomorrow.","Saved. Now go to bed."],"yours":[]},
            "mile":{"ours":["That was commit {count} on {repo}."],"yours":[]},
            "agent":{"ours":["{workflow} left you a note.","{workflow} finished while you were busy."],"yours":[]}},
 "plain":{"push":"Pushed {branch}.","clean":"No changes.","pass":"Checks passed.","fail":"Checks failed.","merge":"Merged into {base}.","first":"Committed.","late":"Pushed {branch}.","mile":"Committed.","agent":"{workflow} finished."},
 "rules":{"quiet_fullscreen":true,"terminal":true,"milestones":true}}
```
`PUT /api/app/v1/messages` `{"tone":"friendly|plain|off","yours":{"push":["…"]},"rules":{…}}`
→ the full object. `yours` lists replace per moment (≤ 20 per moment, ≤ 140
characters each, plain text; placeholders `{repo} {branch} {base} {count} {device} {time} {workflow}`).

## 10. Runner local control (not a server API)

`atelier-runner` listens on a Unix socket next to its config
(`<config dir>/control.sock`, mode 0600; a named pipe `\\.\pipe\atelier-runner`
on Windows) while `run` is active. One JSON object per line, request then
response.

- `{"op":"status"}` → `{"ok":true,"name":"MacBook","version":"7f82d31","server":"https://…","online":true,
   "taking_jobs":true,"paused_reason":null,"capabilities":["jobs","agents","claude-code","workspaces"],
   "rules":{"pause_on_battery":true,"max_parallel":1,"container_jobs":false},
   "current":[{"job_id":412,"repo":"…","workflow":"review-helper","kind":"agent","started_at":"…","detail":"turn 3 of 20"}]}`
- `{"op":"set","taking_jobs":false}` / `{"op":"set","rules":{"pause_on_battery":true}}` → the status object (persisted in the runner config)
- `{"op":"tail","job_id":412,"lines":50}` → `{"ok":true,"lines":["→ pulls.get Atelier 14","…"]}`
- `{"op":"cancel","job_id":412}` → `{"ok":true}`
- `{"op":"workspaces"}` → `{"ok":true,"root":"/Users/x/Atelier","presets":["fork","vscode","cursor"],
   "items":[{"repo":"DemiInfinity/Atelier","path":"/Users/x/Atelier/DemiInfinity/Atelier","branch":"main","dirty":false}]}`
- Errors: `{"ok":false,"error":"…"}`.

`atelier-runner status [--json]` prints the same status by asking the socket
(or "not running").

## 11. Client configuration (shared by `atl` and the desktop app)

`<config dir>/atelier/config.toml` (`~/Library/Application Support/atelier/` on
macOS, `%APPDATA%\atelier\` on Windows, `$XDG_CONFIG_HOME/atelier/` on Linux):
```toml
studio = "https://atelier.infinityhomelab.lan"
device_id = 7
device_name = "MacBook"
clone_root = "~/Atelier"
watch = ["~/Projects"]
editor = "vscode"
git_tool = "fork"
runner_config = "~/Library/Application Support/atelier-runner/config.json"
```
The device token lives in the OS keychain (service `atelier`, account = studio
URL), never in this file. `ATELIER_TOKEN` and `ATELIER_STUDIO` override both for
scripts and CI. Extra CA roots: `ca_file = "/path/rootCA.pem"` (also honoured
from `SSL_CERT_FILE`), because home studios often use a private CA.

### Notes on section 10 as built
- `taking_jobs` is the effective state: false when paused by the user, by battery, or when the jobs capability is off. `paused_reason` is `null`, `"user"`, `"battery"` or `"jobs_off"` (`"user"` wins over `"battery"`).
- `current[].kind` is `"agent"` or `"job"`; `workflow` is the agent's workflow name, or the workflow file's base name without extension.
- `online` is false until the first poll succeeds.
- `set` with nothing to change, or `max_parallel` outside 1–64, returns `{"ok":false,"error":…}`.
- `tail` defaults to 50 lines, caps at 500, clips each line to 2000 bytes, and only works for a job that is running now.
- `atelier-runner status --json` prints `{"ok":false,"error":"not running"}` and exits 3 when nothing answers.
- If the socket path is longer than the OS limit (about 104 bytes on macOS) the runner runs without the control channel.

### Notes on sections 1–9 as built
**Everywhere:** extra error codes `bad_json` (400), `unauthorized` (401), `forbidden` (403), `not_found` (404), `too_large` (413, 64 KB body cap), `rate_limited` (429), `internal` (500). A repository the caller can't read is 404; 403 means readable but not writable. Session cookies are never accepted here.

**Devices:** `start` may answer 422 `invalid_name`/`invalid_kind`, 409 `not_set_up`, 429. An unknown or used `device_code` polls as 410 `expired`. Device names are unique per person ("MacBook 2"), so `/me` may return a different name than was sent. `ssh-key` is for device tokens only (403 `device_only`), plus 422 `invalid_key`, 409 `key_taken`.

**`/me`:** `accounts` is the list of accounts the person can create repositories under (use `name` as `owner` in `POST /repos`): their personal account first (`kind` `"user"`), then the company accounts they manage (`kind` `"company"`); `display` is the display name. `person.avatar_url`, `studio.ssh` and `device` can be `null`. `commit_name`/`commit_email` are the same for every account today. `studio.version` is a build string, not a git sha.

**`/status`:** each need also has `at`. Need ids use the full repo name (`pull:<owner>/<name>:<n>`, `review:…`, `security:<full>:secrets|deps`, `preview:<full>:<n>`). `action.type` can also be `open_web`. `run_id` is the global run id. `running[].detail` is `pull request N` or `<event> on <branch>` (the server doesn't know agent turns; ask the runner socket); `engine` is `null` for non-agent jobs; extra `web_path`. Feed items from Connect have `web_path: null` and an absolute `url`.

**Repos:** `language`, `language_color` and `clone_ssh` can be `null`. `owner` defaults to the caller, `private` to true; 422 `invalid_name`. With `github=1`, hits carry `web_path`; without Tether `github_hits` is `[]` plus a `github_error` string. Mirror: 409 `tether_off`, 422 `mirror_failed`.

**Pulls:** extra `updated_at`, `merge.green` (the when-green condition), `checks[].required`. `checks[].run_id`/`job_id` are `null` for bot checks; `action` is present only on waiting agent jobs. `comments[].kind` ∈ `person | bot | agent`. `methods` may include `rebase`. Merge takes optional `delete_branch`; errors 409 `not_open`/`not_mergeable`, 422 `invalid_method`, 502 `github_failed`. `when_green` always answers 202 and waits for all checks. `DELETE …/merge` → 200 `{"cancelled":true}` or 404 `no_auto_merge`.

**Runs and agents:** runs carry `number` and `web_path`; run `status` ∈ `success | failure | cancelled | queued | running | waiting | pending`; job status may be `skipped`; `started_at` and `runner` can be `null`. Log `tail` max 2000. `agents/waiting` items carry `detail` and `web_path`; `reason` ∈ `first run | tools changed | outside author | environment | needs approval`. Approve: 409 `not_waiting`. Cancel: 409 `finished`.

**Remote:** personal owner only. `mode` ∈ `quick | named | tailscale-private | tailscale-funnel` (the first two are Cloudflare Tunnel; `tailscale-private` is reachable from the owner's own tailnet devices only, `tailscale-funnel` from the internet). With Tailscale `address` is `https://<machine>.<tailnet>.ts.net` and stays the same across restarts; it is `null` until the machine has joined the tailnet, and nothing answers there until the tailnet allows it (HTTPS certificates, and Funnel for the public mode), which the web settings page explains. Clients should treat an unknown `mode` as "on, some other way" rather than fail. `exposed` may also contain `mcp`, `pages`, `api`, `connect`. In Tailscale modes `who[].ip` is the device's tailnet address (private) or the visitor's public address (Funnel), and `who[].country` is always `null`. `who[].country` may be `null`. 422 `invalid_request`, 409 `remote_unavailable`, `link` 409 `remote_off`.

**Events:** `notify.id` is `n:<sequence>` per server process. `notify.kind` ∈ `push | run_passed | run_failed | run_cancelled | agent_finished | agent_approval | job_waiting | pull_merged | pull_review | security_alert | auto_merge_cancelled` or an event name with underscores. `status.changed` ⊆ `needs | running | feed | unread`. More than 8 streams → 429 `too_many_streams`. Only a device's own token actions are suppressed; the person's web and SSH actions are delivered (that is how an SSH push becomes a `push` moment). A stream ends when its device is revoked.

**Messages:** `PUT` is partial (omitted fields kept; an empty list clears a moment). 422 codes: `invalid_tone`, `invalid_moment`, `too_many_messages`, `message_too_long`, `empty_message`, `invalid_message`, `invalid_placeholder`, `invalid_rule`.
