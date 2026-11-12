//! A pretend studio for trying the desktop app without touching a real one.
//! It serves the examples from `docs/app-api.md` on `/api/app/v1`, approves
//! a device by itself after two polls, and streams live events.
//!
//!     cargo run -p atelier-desktop --example mock_studio -- --port 4780 \
//!         [--runner-socket /tmp/x/control.sock]
//!
//! `POST /_mock/notify` (optionally with a notify event as JSON) pushes an
//! event to every connected app. With `--runner-socket` it also answers as
//! an `atelier-runner` that is busy with one agent.

use std::convert::Infallible;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use axum::extract::{Path, Query, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::sse::{Event, KeepAlive, Sse};
use axum::response::{Html, IntoResponse, Response};
use axum::routing::{get, patch, post};
use axum::{Json, Router};
use serde_json::{json, Value};
use tokio::sync::broadcast;
use tokio_stream::wrappers::BroadcastStream;
use tokio_stream::StreamExt;

const TOKEN: &str = "atd_mockmockmockmockmockmockmockmockmockmock";

struct Mock {
    origin: String,
    polls: AtomicU64,
    sequence: AtomicU64,
    remote_on: Mutex<bool>,
    messages: Mutex<Value>,
    approved_agents: Mutex<Vec<u64>>,
    /// Whether this device may rename and delete (off, as in a real studio,
    /// unless started with `--can-admin` or switched through `/_mock/admin`).
    can_admin: Mutex<bool>,
    /// `(full name, owner kind, github link, open pulls)`; renamed and
    /// deleted through the API.
    repos: Mutex<Vec<(String, String, Value, u64)>>,
    /// Full issues (with `body` and a `comments` list), newest first.
    issues: Mutex<Vec<Value>>,
    /// Pull requests whose base was merged in through `…/update`.
    updated_pulls: Mutex<Vec<u64>>,
    /// Pull requests an update found conflicts in; they say so from then on.
    conflicting_pulls: Mutex<Vec<u64>>,
    events: broadcast::Sender<(String, Value)>,
}

type App = State<Arc<Mock>>;

fn refuse(status: StatusCode, code: &str, message: &str) -> Response {
    (status, Json(json!({"error": code, "message": message}))).into_response()
}

/// Every route but the device flow wants the mock's token.
fn authed(headers: &HeaderMap) -> Result<(), Box<Response>> {
    let ok = headers
        .get("authorization")
        .and_then(|v| v.to_str().ok())
        .is_some_and(|v| v == format!("Bearer {TOKEN}"));
    if ok {
        Ok(())
    } else {
        Err(Box::new(refuse(
            StatusCode::UNAUTHORIZED,
            "unauthorized",
            "Your studio didn’t accept this device’s token.",
        )))
    }
}

macro_rules! guard {
    ($headers:expr) => {
        if let Err(response) = authed(&$headers) {
            return *response;
        }
    };
}

async fn device_start(State(mock): App, Json(body): Json<Value>) -> Response {
    mock.polls.store(0, Ordering::SeqCst);
    println!("device/start from {}", body["name"]);
    Json(json!({
        "device_code": "c".repeat(64), "user_code": "AMBER-4127",
        "verify_url": format!("{}/settings/devices/approve?code=AMBER-4127", mock.origin),
        "expires_in": 600, "interval": 1
    }))
    .into_response()
}

async fn device_poll(State(mock): App) -> Response {
    // "Approved in the browser" after two polls.
    if mock.polls.fetch_add(1, Ordering::SeqCst) < 2 {
        return (StatusCode::ACCEPTED, Json(json!({"status": "pending"}))).into_response();
    }
    println!("device approved");
    Json(json!({
        "token": TOKEN, "device_id": 7,
        "person": {"name": "Demi", "login": "DemiInfinity"},
        "studio": {"name": "Atelier", "url": mock.origin, "version": "mock"}
    }))
    .into_response()
}

async fn ssh_key(headers: HeaderMap, Json(body): Json<Value>) -> Response {
    guard!(headers);
    println!("ssh key registered: {}", body["title"]);
    (
        StatusCode::CREATED,
        Json(json!({"id": 12, "fingerprint": "SHA256:mockmockmockmock"})),
    )
        .into_response()
}

async fn me(State(mock): App, headers: HeaderMap) -> Response {
    guard!(headers);
    Json(json!({
        "person": {"name": "Demi", "login": "DemiInfinity", "avatar_url": null},
        "accounts": [
            {"name": "DemiInfinity", "display": "Demi", "kind": "user", "commit_name": "Demi", "commit_email": "demi@example.com"},
            {"name": "elyndrastudios", "display": "Elyndra Studios LTD", "kind": "company", "commit_name": "Demi", "commit_email": "demi@company.example"}
        ],
        "studio": {"name": "Atelier", "url": mock.origin, "version": "mock", "ssh": null, "tether": true, "connect": false},
        "device": {"id": 7, "name": "MacBook", "kind": "desktop", "can_admin": *mock.can_admin.lock().unwrap()}
    }))
    .into_response()
}

/// `POST /_mock/admin/on` or `/off`: the "Can rename and delete
/// repositories" switch of Settings → Devices.
async fn set_admin(State(mock): App, Path(state): Path<String>) -> Response {
    *mock.can_admin.lock().unwrap() = state == "on";
    println!("device can_admin = {}", state == "on");
    Json(json!({"can_admin": state == "on"})).into_response()
}

async fn status(State(mock): App, headers: HeaderMap) -> Response {
    guard!(headers);
    let approved = mock.approved_agents.lock().unwrap().clone();
    let mut needs = vec![
        json!({"id": "agent:31", "kind": "agent_approval", "thread": "agents", "title": "Approve weekly-tidy before it runs",
            "detail": "agent · wants issues.close and issues.comment", "repo": "DemiInfinity/Atelier",
            "web_path": "/DemiInfinity/Atelier/actions/runs/31", "action": {"label": "Review", "type": "approve_agent", "job_id": 31}}),
        json!({"id": "run:208", "kind": "run_failed", "thread": "yours", "title": "ci failed on feature/ssh",
            "detail": "test job · ran on Portainer host", "repo": "DemiInfinity/Atelier",
            "web_path": "/DemiInfinity/Atelier/actions/runs/208", "action": {"label": "Open log", "type": "open_run", "run_id": 208}}),
        json!({"id": "pull:DemiInfinity/Tray:9", "kind": "pull_ready", "thread": "yours", "title": "Security update is ready to merge",
            "detail": "okhttp 5.1 fixes 1 advisory · checks passed", "repo": "DemiInfinity/Tray",
            "web_path": "/DemiInfinity/Tray/pulls/9", "action": {"label": "Review", "type": "open_pull", "number": 9}}),
    ];
    if approved.contains(&31) {
        needs.remove(0);
    }
    Json(json!({
        "needs": needs,
        "running": [{"job_id": 412, "run_id": 209, "repo": "DemiInfinity/Atelier", "workflow": "review-helper", "job": "agent",
            "kind": "agent", "engine": "claude-code", "runner": "MacBook", "started_at": now(), "detail": "pull request 14"}],
        "feed": [
            {"thread": "yours", "text": "Merged “Pages custom domains” into Atelier main", "at": now(), "web_path": "/DemiInfinity/Atelier/pulls/13"},
            {"thread": "github", "text": "A reviewer approved pull request 63 on sparky-ventures", "at": now(), "web_path": "/elyndrastudios/sparky-ventures/pulls/63"},
            {"thread": "connect", "text": "Mara starred Atelier from her studio", "at": now(), "web_path": null, "url": "https://mara.example/"}
        ],
        "unread": 3
    }))
    .into_response()
}

fn repo(mock: &Mock, full_name: &str, kind: &str, github: Value, pulls: u64) -> Value {
    let (owner, name) = full_name.split_once('/').unwrap_or(("", full_name));
    json!({"full_name": full_name, "owner": owner, "owner_kind": kind, "name": name, "private": true,
        "description": "", "default_branch": "main", "language": "Go", "language_color": "#00ADD8",
        "clone_https": format!("{}/{full_name}.git", mock.origin), "clone_ssh": null,
        "github": github, "open_pulls": pulls, "updated_at": now()})
}

fn default_repos() -> Vec<(String, String, Value, u64)> {
    vec![
        ("DemiInfinity/Atelier".into(), "user".into(), Value::Null, 2),
        ("DemiInfinity/Marbl".into(), "user".into(), Value::Null, 0),
        ("DemiInfinity/Tray".into(), "user".into(), Value::Null, 1),
        (
            "elyndrastudios/sparky-ventures".into(),
            "company".into(),
            json!({"repo": "elyndrastudios/sparky-ventures", "home": "github"}),
            1,
        ),
    ]
}

async fn repos(State(mock): App, headers: HeaderMap) -> Response {
    guard!(headers);
    let list: Vec<Value> = mock
        .repos
        .lock()
        .unwrap()
        .iter()
        .map(|(full, kind, github, pulls)| repo(&mock, full, kind, github.clone(), *pulls))
        .collect();
    Json(json!({
        "repos": list,
        "github_hits": [{"full_name": "maralind/loopkit", "description": "", "atelier_badge": true}]
    }))
    .into_response()
}

/// The repository Atelier was renamed to (or still is), so its pull
/// requests, runs and issues follow a rename.
fn main_repo(mock: &Mock) -> String {
    mock.repos
        .lock()
        .unwrap()
        .first()
        .map(|r| r.0.clone())
        .unwrap_or_else(|| "DemiInfinity/Atelier".into())
}

/// Renaming and deleting need the device's switch to be on.
fn admin_refused(mock: &Mock) -> Option<Response> {
    (!*mock.can_admin.lock().unwrap()).then(|| {
        refuse(
            StatusCode::FORBIDDEN,
            "device_not_allowed",
            "This device isn't allowed to rename or delete repositories. Turn it on in Settings → Devices.",
        )
    })
}

async fn repo_rename(
    State(mock): App,
    headers: HeaderMap,
    Path((owner, name)): Path<(String, String)>,
    Json(body): Json<Value>,
) -> Response {
    guard!(headers);
    if let Some(no) = admin_refused(&mock) {
        return no;
    }
    let new = body["name"].as_str().unwrap_or_default().trim().to_string();
    if new.is_empty() || new.contains(char::is_whitespace) || new.contains('/') {
        return refuse(
            StatusCode::UNPROCESSABLE_ENTITY,
            "invalid_name",
            "That isn’t a name a repository can have.",
        );
    }
    let full = format!("{owner}/{name}");
    let renamed = format!("{owner}/{new}");
    let mut repos = mock.repos.lock().unwrap();
    if repos
        .iter()
        .any(|r| r.0.eq_ignore_ascii_case(&renamed) && !r.0.eq_ignore_ascii_case(&full))
    {
        return refuse(
            StatusCode::CONFLICT,
            "name_taken",
            &format!("There is already a repository called {new}."),
        );
    }
    let Some(entry) = repos.iter_mut().find(|r| r.0.eq_ignore_ascii_case(&full)) else {
        return refuse(
            StatusCode::NOT_FOUND,
            "not_found",
            "Your studio couldn’t find that.",
        );
    };
    println!("repo renamed: {full} -> {renamed}");
    entry.0 = renamed.clone();
    let (kind, github, pulls) = (entry.1.clone(), entry.2.clone(), entry.3);
    drop(repos);
    for issue in mock.issues.lock().unwrap().iter_mut() {
        if issue["repo"] == json!(full) {
            issue["repo"] = json!(renamed);
        }
    }
    Json(repo(&mock, &renamed, &kind, github, pulls)).into_response()
}

async fn repo_delete(
    State(mock): App,
    headers: HeaderMap,
    Path((owner, name)): Path<(String, String)>,
    body: Option<Json<Value>>,
) -> Response {
    guard!(headers);
    if let Some(no) = admin_refused(&mock) {
        return no;
    }
    let full = format!("{owner}/{name}");
    let confirm = body
        .as_ref()
        .and_then(|b| b["confirm"].as_str())
        .unwrap_or_default();
    if !confirm.eq_ignore_ascii_case(&full) {
        return refuse(
            StatusCode::UNPROCESSABLE_ENTITY,
            "confirm_mismatch",
            &format!("Send confirm with the repository's full name, {full}, to delete it."),
        );
    }
    let mut repos = mock.repos.lock().unwrap();
    let before = repos.len();
    repos.retain(|r| !r.0.eq_ignore_ascii_case(&full));
    if repos.len() == before {
        return refuse(
            StatusCode::NOT_FOUND,
            "not_found",
            "Your studio couldn’t find that.",
        );
    }
    println!("repo deleted: {full} (confirm {confirm})");
    StatusCode::NO_CONTENT.into_response()
}

/// The files pull request 15 conflicts in.
const CONFLICT_FILES: [&str; 2] = [
    "internal/ssh/handshake.go",
    "internal/ssh/handshake_test.go",
];

/// Pull request 14 is 3 commits behind `main` until it is updated; then its
/// checks start over. 15 is 2 behind and can't be updated: the update
/// answers with conflicts, and from then on the pull request says so itself.
fn pull(mock: &Mock, repo: &str, full: bool) -> Value {
    let updated = mock.updated_pulls.lock().unwrap().contains(&14);
    let mut pull = json!({"repo": repo, "number": 14, "title": "Remote access switch in the tray menu", "state": "open",
        "head": "feature/tunnel-ui", "base": "main", "author": "owner:DemiInfinity", "commits": 2, "files": 3, "via": null,
        "merge": {"can_merge": false, "reason": "1 check is still running and 1 agent is waiting for you.", "methods": ["squash", "merge"],
                  "on_github": false, "auto_merge": null},
        "checks": {"total": 4, "passed": 2, "failed": 0, "pending": 1, "waiting": 1, "state": "pending"},
        "behind": if updated { 0 } else { 3 }, "ahead": if updated { 3 } else { 2 }, "conflicts": false, "can_update": !updated,
        "web_path": format!("/{repo}/pulls/14"), "updated_at": now()});
    if updated {
        pull["commits"] = json!(3);
        pull["merge"]["reason"] =
            json!("3 checks are still running and 1 agent is waiting for you.");
        pull["checks"] = json!({"total": 4, "passed": 0, "failed": 0, "pending": 3, "waiting": 1, "state": "pending"});
    }
    if full {
        pull["conflict_files"] = json!([]);
        pull["checks"] = if updated {
            json!([
                {"name": "ci · test", "status": "pending", "detail": "MacBook · queued", "run_id": 211, "job_id": 420},
                {"name": "ci · lint", "status": "pending", "detail": "MacBook · queued", "run_id": 211, "job_id": 421},
                {"name": "image · build", "status": "pending", "detail": "Portainer host · queued", "run_id": 212, "job_id": 423},
                {"name": "review-helper", "status": "waiting", "detail": "agent · needs your approval to run", "run_id": 213, "job_id": 414, "action": "approve_agent"}
            ])
        } else {
            json!([
                {"name": "ci · test", "status": "success", "detail": "MacBook · 1m 48s", "run_id": 207, "job_id": 410},
                {"name": "ci · lint", "status": "success", "detail": "MacBook · 22s", "run_id": 207, "job_id": 411},
                {"name": "image · build", "status": "pending", "detail": "Portainer host · step 3 of 5", "run_id": 209, "job_id": 413},
                {"name": "review-helper", "status": "waiting", "detail": "agent · needs your approval to run", "run_id": 210, "job_id": 414, "action": "approve_agent"}
            ])
        };
        pull["comments"] = json!([
            {"author": "owner:DemiInfinity", "via": null, "kind": "person", "at": now(),
             "body": "Adds the switch under Open studio. Turning it off from the menu cuts every remote session, same as the banner in the browser."},
            {"author": "CodeRabbit", "via": "tether", "kind": "bot", "at": now(),
             "body": "toggleRemote ignores the error from SetRemote. If the studio is unreachable the menu will show the wrong state."}
        ]);
    }
    pull
}

/// `GET /pulls[?repo=owner/name]`. The summaries carry the checks at a
/// glance; one is as an older studio sends it (counts only).
async fn pulls(
    State(mock): App,
    headers: HeaderMap,
    Query(query): Query<std::collections::HashMap<String, String>>,
) -> Response {
    guard!(headers);
    let main = main_repo(&mock);
    let known: Vec<String> = mock
        .repos
        .lock()
        .unwrap()
        .iter()
        .map(|r| r.0.clone())
        .collect();
    let conflicting = mock.conflicting_pulls.lock().unwrap().contains(&15);
    let mut list = vec![
        pull(&mock, &main, false),
        json!({"repo": main, "number": 15, "title": "SSH handshake retries", "state": "open", "head": "feature/ssh", "base": "main",
         "author": "owner:DemiInfinity", "via": null, "merge": {"can_merge": false, "methods": ["squash"]},
         "checks": {"total": 2, "passed": 1, "failed": 1, "pending": 0, "waiting": 0, "state": "failing"},
         "behind": 2, "ahead": 4, "conflicts": conflicting, "can_update": !conflicting,
         "web_path": format!("/{main}/pulls/15")}),
        json!({"repo": "DemiInfinity/Tray", "number": 9, "title": "Update okhttp to 5.1", "state": "open", "head": "atelier-updates/okhttp", "base": "main",
         "author": "atelier-updates", "via": "bot", "merge": {"can_merge": true, "methods": ["squash"]},
         "checks": {"total": 2, "passed": 2, "failed": 0, "pending": 0, "waiting": 0, "state": "passing"},
         "web_path": "/DemiInfinity/Tray/pulls/9"}),
        json!({"repo": "elyndrastudios/sparky-ventures", "number": 63, "title": "Soften the pricing copy", "state": "open", "head": "copy", "base": "main",
         "author": "cursor agent on GitHub", "via": "tether", "merge": {"can_merge": true, "methods": ["merge"], "on_github": true},
         "checks": {"passed": 0, "failed": 0, "pending": 0, "waiting": 0}, "web_path": "/elyndrastudios/sparky-ventures/pulls/63"}),
    ];
    list.retain(|p| known.iter().any(|k| p["repo"] == json!(k)));
    if let Some(repo) = query.get("repo") {
        list.retain(|p| {
            p["repo"]
                .as_str()
                .is_some_and(|r| r.eq_ignore_ascii_case(repo))
        });
    }
    Json(json!({"pulls": list})).into_response()
}

async fn pull_detail(
    State(mock): App,
    headers: HeaderMap,
    Path((owner, name, number)): Path<(String, String, u64)>,
) -> Response {
    guard!(headers);
    if number == 14 {
        return Json(pull(&mock, &format!("{owner}/{name}"), true)).into_response();
    }
    if number == 15 {
        let conflicting = mock.conflicting_pulls.lock().unwrap().contains(&15);
        let files: &[&str] = if conflicting { &CONFLICT_FILES } else { &[] };
        return Json(json!({"repo": format!("{owner}/{name}"), "number": 15, "title": "SSH handshake retries", "state": "open",
            "head": "feature/ssh", "base": "main", "author": "owner:DemiInfinity", "commits": 4, "files": 2, "via": null,
            "merge": {"can_merge": false, "reason": "1 check failed.", "methods": ["squash"], "on_github": false, "auto_merge": null},
            "checks": [{"name": "ci · test", "status": "failure", "detail": "Portainer host · 2m 10s", "run_id": 208, "job_id": 415},
                       {"name": "ci · lint", "status": "success", "detail": "MacBook · 20s", "run_id": 208, "job_id": 416}],
            "behind": 2, "ahead": 4, "conflicts": conflicting, "conflict_files": files, "can_update": !conflicting,
            "comments": [], "web_path": format!("/{owner}/{name}/pulls/15")}))
        .into_response();
    }
    Json(json!({"repo": format!("{owner}/{name}"), "number": number, "title": format!("Pull request {number}"), "state": "open",
        "head": "feature", "base": "main", "author": "atelier-updates", "commits": 1, "files": 1,
        "merge": {"can_merge": true, "reason": "Every check passed.", "methods": ["squash", "merge"]},
        "checks": [{"name": "ci · test", "status": "success", "detail": "MacBook · 1m 02s", "run_id": 300, "job_id": 500}],
        "comments": [], "web_path": format!("/{owner}/{name}/pulls/{number}")}))
    .into_response()
}

async fn merge(headers: HeaderMap, Json(body): Json<Value>) -> Response {
    guard!(headers);
    println!("merge asked: {body}");
    if body["when_green"] == true {
        return (
            StatusCode::ACCEPTED,
            Json(json!({"auto_merge": {"method": body["method"], "by": "device:MacBook"}})),
        )
            .into_response();
    }
    Json(json!({"merged": true, "sha": "7f82d31aa"})).into_response()
}

/// `POST …/pulls/{n}/update`: 14 takes its base in (once; after that it is
/// current), 15 conflicts, anything else is already up to date.
async fn update_pull(
    State(mock): App,
    headers: HeaderMap,
    Path((owner, name, number)): Path<(String, String, u64)>,
) -> Response {
    guard!(headers);
    println!("update asked: {owner}/{name} pull request {number}");
    match number {
        14 => {
            let mut updated = mock.updated_pulls.lock().unwrap();
            if updated.contains(&14) {
                return Json(json!({"updated": false, "head_sha": "4f2a9c1e77aa01bc"}))
                    .into_response();
            }
            updated.push(14);
            Json(json!({"updated": true, "head_sha": "4f2a9c1e77aa01bc"})).into_response()
        }
        15 => {
            let mut conflicting = mock.conflicting_pulls.lock().unwrap();
            if !conflicting.contains(&15) {
                conflicting.push(15);
            }
            (
                StatusCode::CONFLICT,
                Json(json!({"error": "conflicts",
                    "message": "main and feature/ssh both changed 2 files. Nothing was changed.",
                    "files": CONFLICT_FILES})),
            )
                .into_response()
        }
        _ => Json(json!({"updated": false, "head_sha": "7f82d31aa"})).into_response(),
    }
}

async fn cancel_merge(headers: HeaderMap) -> Response {
    guard!(headers);
    Json(json!({"cancelled": true})).into_response()
}

async fn runs(
    State(mock): App,
    headers: HeaderMap,
    Query(query): Query<std::collections::HashMap<String, String>>,
) -> Response {
    guard!(headers);
    let main = main_repo(&mock);
    let mut list = vec![
        json!({"run_id": 207, "repo": main, "workflow": "ci", "event": "push", "branch": "feature/tunnel-ui", "status": "success",
         "started_at": now(), "duration_s": 108, "web_path": format!("/{main}/actions/runs/207"),
         "jobs": [{"job_id": 410, "name": "test", "status": "success", "runner": "MacBook", "duration_s": 108, "kind": "job"},
                  {"job_id": 411, "name": "lint", "status": "success", "runner": "MacBook", "duration_s": 22, "kind": "job"}]}),
        json!({"run_id": 208, "repo": main, "workflow": "ci", "event": "push", "branch": "feature/ssh", "status": "failure",
         "started_at": now(), "duration_s": 123, "web_path": format!("/{main}/actions/runs/208"),
         "jobs": [{"job_id": 421, "name": "test", "status": "failure", "runner": "MacBook", "duration_s": 123, "kind": "job"}]}),
        json!({"run_id": 206, "repo": main, "workflow": "pages", "event": "push", "branch": "main", "status": "success",
         "started_at": now(), "duration_s": 41, "web_path": format!("/{main}/actions/runs/206"),
         "jobs": [{"job_id": 409, "name": "build", "status": "success", "runner": "MacBook", "duration_s": 41, "kind": "job"}]}),
    ];
    if let Some(repo) = query.get("repo") {
        list.retain(|r| {
            r["repo"]
                .as_str()
                .is_some_and(|x| x.eq_ignore_ascii_case(repo))
        });
    }
    Json(json!({"runs": list})).into_response()
}

// ------------------------------------------------------------------ issues

fn default_issues(origin: &str) -> Vec<Value> {
    let issue = |number: u64,
                 title: &str,
                 state: &str,
                 author: &str,
                 via: Value,
                 labels: Value,
                 body: &str,
                 comments: Value| {
        json!({"repo": "DemiInfinity/Atelier", "number": number, "title": title, "state": state, "author": author, "via": via,
            "labels": labels, "body": body, "comments": comments, "created_at": now(), "updated_at": now(),
            "web_path": format!("/DemiInfinity/Atelier/issues/{number}"),
            "web_url": format!("{origin}/DemiInfinity/Atelier/issues/{number}")})
    };
    vec![
        issue(21, "Tray icon flickers", "open", "owner:DemiInfinity", Value::Null,
            json!([{"name": "bug", "color": "#d73a4a"}, {"name": "tray", "color": "#f2b85c"}]),
            concat!(
                "## What happens\n\n",
                "The icon flickers when a job starts. See #20 and https://example.com/tray-notes, or ask @maralind.\n\n",
                "- happens on **battery**\n- `<script>alert(1)</script>` stays text\n\n",
                "<script>alert(1)</script>\n\n",
                "Inline too: <img src=x onerror=alert(1)> and [a trap](javascript:alert(1)).\n\n",
                "- [x] reproduce it\n- [ ] find the redraw\n\n",
                "```go\nfunc redraw() { tray.SetIcon(icon) }\n```\n\n",
                "| power | flickers |\n|:--|--:|\n| battery | yes |\n| mains | *no* |\n\n",
                "> It was fine in ~~0.3~~ 0.2.\n\n---\n\n",
                "![a screenshot](https://example.com/shot.png) · [the docs](https://example.com/docs)"
            ),
            json!([{"id": 88, "author": "CodeRabbit[bot]", "via": "tether", "kind": "bot",
                "body": "Possibly the redraw in `tray.go`.\n\n1. set the icon **once**\n2. debounce the rest", "created_at": now()}])),
        issue(20, "Docs: say how to pair a runner", "open", "person:maralind", json!("tether"),
            json!([{"name": "docs", "color": "#7cc0ff"}]), "From GitHub: the README skips the pairing step.", json!([])),
        issue(18, "Clone over SSH fails on port 22", "closed", "owner:DemiInfinity", Value::Null, json!([]),
            "Fixed by using the studio's own port.", json!([])),
    ]
}

/// The list form of an issue: no body, comments as a count.
fn issue_summary(issue: &Value) -> Value {
    let mut out = issue.clone();
    if let Some(map) = out.as_object_mut() {
        map.remove("body");
        let n = issue["comments"].as_array().map_or(0, Vec::len);
        map.insert("comments".into(), json!(n));
    }
    out
}

async fn issues(
    State(mock): App,
    headers: HeaderMap,
    Query(query): Query<std::collections::HashMap<String, String>>,
) -> Response {
    guard!(headers);
    let state = query.get("state").map_or("open", String::as_str);
    let list: Vec<Value> = mock
        .issues
        .lock()
        .unwrap()
        .iter()
        .filter(|i| {
            query.get("repo").is_none_or(|r| {
                i["repo"]
                    .as_str()
                    .is_some_and(|x| x.eq_ignore_ascii_case(r))
            })
        })
        .filter(|i| state == "all" || i["state"] == json!(state))
        .map(issue_summary)
        .collect();
    Json(json!({"issues": list, "has_more": false})).into_response()
}

fn find_issue<'a>(
    issues: &'a mut [Value],
    owner: &str,
    name: &str,
    number: u64,
) -> Option<&'a mut Value> {
    let full = format!("{owner}/{name}");
    issues.iter_mut().find(|i| {
        i["number"] == json!(number)
            && i["repo"]
                .as_str()
                .is_some_and(|r| r.eq_ignore_ascii_case(&full))
    })
}

fn no_issue() -> Response {
    refuse(
        StatusCode::NOT_FOUND,
        "not_found",
        "Your studio couldn’t find that.",
    )
}

async fn issue_detail(
    State(mock): App,
    headers: HeaderMap,
    Path((owner, name, number)): Path<(String, String, u64)>,
) -> Response {
    guard!(headers);
    match find_issue(&mut mock.issues.lock().unwrap(), &owner, &name, number) {
        Some(issue) => Json(issue.clone()).into_response(),
        None => no_issue(),
    }
}

async fn issue_create(
    State(mock): App,
    headers: HeaderMap,
    Path((owner, name)): Path<(String, String)>,
    Json(body): Json<Value>,
) -> Response {
    guard!(headers);
    let title = body["title"]
        .as_str()
        .unwrap_or_default()
        .trim()
        .to_string();
    if title.is_empty() || title.contains('\n') {
        return refuse(
            StatusCode::UNPROCESSABLE_ENTITY,
            "invalid_title",
            "An issue needs a title of one line.",
        );
    }
    let mut issues = mock.issues.lock().unwrap();
    let number = issues
        .iter()
        .filter_map(|i| i["number"].as_u64())
        .max()
        .unwrap_or(0)
        + 1;
    let issue = json!({"repo": format!("{owner}/{name}"), "number": number, "title": title, "state": "open",
        "author": "token:device:MacBook", "via": null, "labels": [], "body": body["body"].as_str().unwrap_or_default(),
        "comments": [], "created_at": now(), "updated_at": now(),
        "web_path": format!("/{owner}/{name}/issues/{number}"), "web_url": format!("{}/{owner}/{name}/issues/{number}", mock.origin)});
    println!("issue opened: {owner}/{name}#{number} {}", issue["title"]);
    issues.insert(0, issue.clone());
    (StatusCode::CREATED, Json(issue)).into_response()
}

async fn issue_comment(
    State(mock): App,
    headers: HeaderMap,
    Path((owner, name, number)): Path<(String, String, u64)>,
    Json(body): Json<Value>,
) -> Response {
    guard!(headers);
    let text = body["body"].as_str().unwrap_or_default().trim().to_string();
    if text.is_empty() {
        return refuse(
            StatusCode::UNPROCESSABLE_ENTITY,
            "empty_body",
            "Write something first.",
        );
    }
    let mut issues = mock.issues.lock().unwrap();
    let Some(issue) = find_issue(&mut issues, &owner, &name, number) else {
        return no_issue();
    };
    let comment = json!({"id": 100 + mock.sequence.fetch_add(1, Ordering::SeqCst), "author": "device:MacBook", "via": null,
        "kind": "person", "body": text, "created_at": now()});
    println!("issue {number} commented: {}", comment["body"]);
    if let Some(list) = issue["comments"].as_array_mut() {
        list.push(comment.clone());
    }
    (StatusCode::CREATED, Json(comment)).into_response()
}

async fn issue_patch(
    State(mock): App,
    headers: HeaderMap,
    Path((owner, name, number)): Path<(String, String, u64)>,
    Json(body): Json<Value>,
) -> Response {
    guard!(headers);
    let state = body["state"].as_str().unwrap_or_default();
    if !matches!(state, "open" | "closed") {
        return refuse(
            StatusCode::UNPROCESSABLE_ENTITY,
            "invalid_state",
            "An issue is open or closed.",
        );
    }
    let mut issues = mock.issues.lock().unwrap();
    let Some(issue) = find_issue(&mut issues, &owner, &name, number) else {
        return no_issue();
    };
    issue["state"] = json!(state);
    println!("issue {number} is now {state}");
    Json(issue.clone()).into_response()
}

async fn job_log(headers: HeaderMap, Path(id): Path<u64>) -> Response {
    guard!(headers);
    Json(json!({"lines": [format!("job {id}"), "--- FAIL: TestSSHHandshake (0.42s)", "    ssh_test.go:88: handshake timed out", "FAIL", "exit status 1"],
        "truncated": false, "status": "failure"}))
    .into_response()
}

async fn agents_waiting(State(mock): App, headers: HeaderMap) -> Response {
    guard!(headers);
    let jobs = if mock.approved_agents.lock().unwrap().contains(&31) {
        json!([])
    } else {
        json!([{"job_id": 31, "run_id": 210, "repo": "DemiInfinity/Atelier", "workflow": "weekly-tidy", "reason": "first run",
            "engine": "claude-code", "tools": ["issues.list", "issues.comment", "issues.close"],
            "write_tools": ["issues.comment", "issues.close"], "max_turns": 20, "timeout_minutes": 10,
            "web_path": "/DemiInfinity/Atelier/actions/runs/210"}])
    };
    Json(json!({"jobs": jobs})).into_response()
}

async fn approve(State(mock): App, headers: HeaderMap, Path(id): Path<u64>) -> Response {
    guard!(headers);
    println!("agent {id} approved");
    mock.approved_agents.lock().unwrap().push(id);
    let _ = mock
        .events
        .send(("status".into(), json!({"changed": ["needs"]})));
    Json(json!({"approved": true})).into_response()
}

fn remote_json(mock: &Mock) -> Value {
    let on = *mock.remote_on.lock().unwrap();
    json!({"on": on, "mode": "quick", "address": if on { json!("https://quiet-fox-lamp.trycloudflare.com") } else { Value::Null },
        "exposed": ["web", "git"], "connected": 0})
}

async fn remote_get(State(mock): App, headers: HeaderMap) -> Response {
    guard!(headers);
    Json(remote_json(&mock)).into_response()
}

async fn remote_set(State(mock): App, headers: HeaderMap, Json(body): Json<Value>) -> Response {
    guard!(headers);
    *mock.remote_on.lock().unwrap() = body["on"] == true;
    println!("remote access set to {}", body["on"]);
    let now = remote_json(&mock);
    let _ = mock.events.send(("remote".into(), now.clone()));
    Json(now).into_response()
}

async fn remote_who(headers: HeaderMap) -> Response {
    guard!(headers);
    Json(json!({"sessions": []})).into_response()
}

async fn remote_link(headers: HeaderMap) -> Response {
    guard!(headers);
    (StatusCode::CREATED, Json(json!({"url": "https://quiet-fox-lamp.trycloudflare.com/k/one-time", "expires_in": 900}))).into_response()
}

async fn messages_get(State(mock): App, headers: HeaderMap) -> Response {
    guard!(headers);
    Json(mock.messages.lock().unwrap().clone()).into_response()
}

async fn messages_put(State(mock): App, headers: HeaderMap, Json(body): Json<Value>) -> Response {
    guard!(headers);
    let mut messages = mock.messages.lock().unwrap();
    if let Some(tone) = body.get("tone") {
        messages["tone"] = tone.clone();
    }
    if let Some(yours) = body.get("yours").and_then(Value::as_object) {
        for (moment, list) in yours {
            messages["moments"][moment]["yours"] = list.clone();
        }
    }
    if let Some(rules) = body.get("rules") {
        messages["rules"] = rules.clone();
    }
    println!("messages changed: {body}");
    Json(messages.clone()).into_response()
}

async fn events(State(mock): App, headers: HeaderMap) -> Response {
    guard!(headers);
    println!("an app is listening for events");
    let stream = BroadcastStream::new(mock.events.subscribe()).filter_map(|item| {
        let (name, data) = item.ok()?;
        Some(Ok::<_, Infallible>(
            Event::default().event(name).data(data.to_string()),
        ))
    });
    Sse::new(stream)
        .keep_alive(
            KeepAlive::new()
                .interval(Duration::from_secs(25))
                .text("ping"),
        )
        .into_response()
}

/// `POST /_mock/notify`: sends the body as a notify event, or a failed run.
async fn push_notify(State(mock): App, body: Option<Json<Value>>) -> Response {
    let n = mock.sequence.fetch_add(1, Ordering::SeqCst) + 1;
    let mut event = body.map(|Json(b)| b).filter(Value::is_object).unwrap_or_else(|| {
        json!({"kind": "run_failed", "thread": "yours", "title": "ci failed on feature/ssh", "detail": "Atelier · test",
            "repo": "DemiInfinity/Atelier", "web_path": "/DemiInfinity/Atelier/actions/runs/208", "moment": "fail"})
    });
    event["id"] = json!(format!("n:{n}"));
    event["at"] = json!(now());
    let listeners = mock
        .events
        .send(("notify".into(), event.clone()))
        .unwrap_or(0);
    let _ = mock
        .events
        .send(("status".into(), json!({"changed": ["needs", "feed"]})));
    Json(json!({"sent": event, "listeners": listeners})).into_response()
}

async fn page() -> Html<&'static str> {
    Html("<!doctype html><title>Mock studio</title><p>This is the mock studio for Atelier Desktop. Nothing to see here.</p>")
}

fn now() -> String {
    let secs = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    // Days since the epoch to a civil date (Howard Hinnant's algorithm).
    let (days, rem) = (secs / 86_400, secs % 86_400);
    let z = days as i64 + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let (d, m) = (
        doy - (153 * mp + 2) / 5 + 1,
        if mp < 10 { mp + 3 } else { mp - 9 },
    );
    let y = yoe + era * 400 + i64::from(m <= 2);
    format!(
        "{y:04}-{m:02}-{d:02}T{:02}:{:02}:{:02}Z",
        rem / 3600,
        (rem % 3600) / 60,
        rem % 60
    )
}

fn default_messages() -> Value {
    json!({
        "tone": "friendly",
        "moments": {
            "push": {"ours": ["Home and dry. {branch} is safe in your studio.", "Off it goes. {count} commits landed on {repo}.", "Pushed. Checks are warming up on {device}."], "yours": ["Shipped. Go outside."]},
            "clean": {"ours": ["All tidy. The kettle is free.", "Nothing to commit. Go and make something."], "yours": []},
            "pass": {"ours": ["Green across the board on {device}.", "Checks passed. A little smugness is allowed."], "yours": []},
            "fail": {"ours": ["One test disagrees with you on {branch}.", "Not this time. The log knows why."], "yours": []},
            "merge": {"ours": ["Merged into main. Nicely done.", "In it goes. {repo} is {count} commits better."], "yours": []},
            "first": {"ours": ["Good morning. First one of the day.", "And we are off."], "yours": []},
            "late": {"ours": ["Pushed at {time}. It will still be here tomorrow.", "Saved. Now go to bed."], "yours": []},
            "mile": {"ours": ["That was commit {count} on {repo}."], "yours": []},
            "agent": {"ours": ["{workflow} left you a note.", "{workflow} finished while you were busy."], "yours": []}
        },
        "plain": {"push": "Pushed {branch}.", "clean": "No changes.", "pass": "Checks passed.", "fail": "Checks failed.", "merge": "Merged into {base}.", "first": "Committed.", "late": "Pushed {branch}.", "mile": "Committed.", "agent": "{workflow} finished."},
        "rules": {"quiet_fullscreen": true, "terminal": true, "milestones": true}
    })
}

/// Answers on a Unix socket the way `atelier-runner` does, with one agent
/// at work.
#[cfg(unix)]
async fn fake_runner(socket: std::path::PathBuf, origin: String) {
    use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
    let _ = std::fs::remove_file(&socket);
    let listener = match tokio::net::UnixListener::bind(&socket) {
        Ok(listener) => listener,
        Err(e) => {
            eprintln!("couldn't listen on {}: {e}", socket.display());
            return;
        }
    };
    println!("fake runner on {}", socket.display());
    let taking = Arc::new(Mutex::new(true));
    let started = now();
    let turn = Arc::new(AtomicU64::new(3));
    loop {
        let Ok((stream, _)) = listener.accept().await else {
            return;
        };
        let (taking, started, origin, turn) = (
            taking.clone(),
            started.clone(),
            origin.clone(),
            turn.clone(),
        );
        tokio::spawn(async move {
            let mut stream = BufReader::new(stream);
            let mut line = String::new();
            if stream.read_line(&mut line).await.is_err() {
                return;
            }
            let request: Value = serde_json::from_str(&line).unwrap_or_default();
            if let Some(on) = request.get("taking_jobs").and_then(Value::as_bool) {
                *taking.lock().unwrap() = on;
                println!("runner: taking jobs = {on}");
            }
            let on = *taking.lock().unwrap();
            let answer = match request["op"].as_str() {
                Some("status") | Some("set") => {
                    json!({"ok": true, "name": "MacBook", "version": "mock", "server": origin, "online": true,
                    "taking_jobs": on, "paused_reason": if on { Value::Null } else { json!("user") },
                    "capabilities": ["jobs", "agents", "claude-code", "workspaces"],
                    "rules": {"pause_on_battery": true, "max_parallel": 1, "container_jobs": false},
                    "current": [{"job_id": 412, "repo": "DemiInfinity/Atelier", "workflow": "review-helper", "kind": "agent",
                        "started_at": started, "detail": format!("turn {} of 20", turn.load(Ordering::SeqCst))}]})
                }
                Some("tail") => {
                    json!({"ok": true, "lines": ["→ pulls.get  Atelier 14", "← Remote access switch in the tray menu · 3 files",
                    "→ pulls.diff Atelier 14", "← 19 lines added, 3 removed", "  Reading tray.go: the new switch calls toggleRemote", "  without checking the result…"]})
                }
                Some("workspaces") => {
                    json!({"ok": true, "root": "~/Atelier", "presets": ["vscode", "cursor"], "items": []})
                }
                Some("cancel") => json!({"ok": true}),
                _ => json!({"ok": false, "error": "unknown op"}),
            };
            let _ = stream
                .get_mut()
                .write_all(format!("{answer}\n").as_bytes())
                .await;
        });
    }
}

#[tokio::main]
async fn main() {
    let args: Vec<String> = std::env::args().collect();
    let flag = |name: &str| {
        args.iter()
            .position(|a| a == name)
            .and_then(|i| args.get(i + 1))
            .cloned()
    };
    let port: u16 = flag("--port").and_then(|p| p.parse().ok()).unwrap_or(4780);
    let origin = format!("http://127.0.0.1:{port}");
    let (events_tx, _) = broadcast::channel(64);
    let mock = Arc::new(Mock {
        origin: origin.clone(),
        polls: AtomicU64::new(0),
        sequence: AtomicU64::new(0),
        remote_on: Mutex::new(false),
        messages: Mutex::new(default_messages()),
        approved_agents: Mutex::new(Vec::new()),
        can_admin: Mutex::new(args.iter().any(|a| a == "--can-admin")),
        repos: Mutex::new(default_repos()),
        issues: Mutex::new(default_issues(&origin)),
        updated_pulls: Mutex::new(Vec::new()),
        conflicting_pulls: Mutex::new(Vec::new()),
        events: events_tx,
    });

    #[cfg(unix)]
    if let Some(socket) = flag("--runner-socket") {
        tokio::spawn(fake_runner(socket.into(), origin.clone()));
    }

    let api = Router::new()
        .route("/device/start", post(device_start))
        .route("/device/poll", post(device_poll))
        .route("/device/ssh-key", post(ssh_key))
        .route("/me", get(me))
        .route("/status", get(status))
        .route("/repos", get(repos))
        .route(
            "/repos/{owner}/{name}",
            patch(repo_rename).delete(repo_delete),
        )
        .route("/pulls", get(pulls))
        .route("/issues", get(issues))
        .route("/repos/{owner}/{name}/issues", post(issue_create))
        .route(
            "/repos/{owner}/{name}/issues/{number}",
            get(issue_detail).patch(issue_patch),
        )
        .route(
            "/repos/{owner}/{name}/issues/{number}/comments",
            post(issue_comment),
        )
        .route("/repos/{owner}/{name}/pulls/{number}", get(pull_detail))
        .route(
            "/repos/{owner}/{name}/pulls/{number}/merge",
            post(merge).delete(cancel_merge),
        )
        .route(
            "/repos/{owner}/{name}/pulls/{number}/update",
            post(update_pull),
        )
        .route("/runs", get(runs))
        .route("/jobs/{id}/log", get(job_log))
        .route("/agents/waiting", get(agents_waiting))
        .route("/agents/jobs/{id}/approve", post(approve))
        .route("/remote", get(remote_get).post(remote_set))
        .route("/remote/who", get(remote_who))
        .route("/remote/link", post(remote_link))
        .route("/messages", get(messages_get).put(messages_put))
        .route("/events", get(events));
    let app = Router::new()
        .nest("/api/app/v1", api)
        .route("/_mock/notify", post(push_notify))
        .route("/_mock/admin/{state}", post(set_admin))
        .fallback(page)
        .with_state(mock);

    let listener = tokio::net::TcpListener::bind(("127.0.0.1", port))
        .await
        .expect("the port is free");
    println!("mock studio on {origin}");
    axum::serve(listener, app)
        .await
        .expect("the mock studio runs");
}
