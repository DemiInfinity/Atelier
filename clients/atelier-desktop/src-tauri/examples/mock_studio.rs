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

use axum::extract::{Path, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::sse::{Event, KeepAlive, Sse};
use axum::response::{Html, IntoResponse, Response};
use axum::routing::{get, post};
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
        "device": {"id": 7, "name": "MacBook", "kind": "desktop"}
    }))
    .into_response()
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

async fn repos(State(mock): App, headers: HeaderMap) -> Response {
    guard!(headers);
    Json(json!({
        "repos": [
            repo(&mock, "DemiInfinity/Atelier", "user", Value::Null, 1),
            repo(&mock, "DemiInfinity/Marbl", "user", Value::Null, 0),
            repo(&mock, "DemiInfinity/Tray", "user", Value::Null, 1),
            repo(&mock, "elyndrastudios/sparky-ventures", "company", json!({"repo": "elyndrastudios/sparky-ventures", "home": "github"}), 1),
        ],
        "github_hits": [{"full_name": "maralind/loopkit", "description": "", "atelier_badge": true}]
    }))
    .into_response()
}

fn pull(full: bool) -> Value {
    let mut pull = json!({"repo": "DemiInfinity/Atelier", "number": 14, "title": "Remote access switch in the tray menu", "state": "open",
        "head": "feature/tunnel-ui", "base": "main", "author": "owner:DemiInfinity", "commits": 2, "files": 3, "via": null,
        "merge": {"can_merge": false, "reason": "1 check is still running and 1 agent is waiting for you.", "methods": ["squash", "merge"],
                  "on_github": false, "auto_merge": null},
        "checks": {"passed": 2, "failed": 0, "pending": 1, "waiting": 1},
        "web_path": "/DemiInfinity/Atelier/pulls/14", "updated_at": now()});
    if full {
        pull["checks"] = json!([
            {"name": "ci · test", "status": "success", "detail": "MacBook · 1m 48s", "run_id": 207, "job_id": 410},
            {"name": "ci · lint", "status": "success", "detail": "MacBook · 22s", "run_id": 207, "job_id": 411},
            {"name": "image · build", "status": "pending", "detail": "Portainer host · step 3 of 5", "run_id": 209, "job_id": 413},
            {"name": "review-helper", "status": "waiting", "detail": "agent · needs your approval to run", "run_id": 210, "job_id": 414, "action": "approve_agent"}
        ]);
        pull["comments"] = json!([
            {"author": "owner:DemiInfinity", "via": null, "kind": "person", "at": now(),
             "body": "Adds the switch under Open studio. Turning it off from the menu cuts every remote session, same as the banner in the browser."},
            {"author": "CodeRabbit", "via": "tether", "kind": "bot", "at": now(),
             "body": "toggleRemote ignores the error from SetRemote. If the studio is unreachable the menu will show the wrong state."}
        ]);
    }
    pull
}

async fn pulls(headers: HeaderMap) -> Response {
    guard!(headers);
    Json(json!({"pulls": [
        pull(false),
        {"repo": "DemiInfinity/Tray", "number": 9, "title": "Update okhttp to 5.1", "state": "open", "head": "atelier-updates/okhttp", "base": "main",
         "author": "atelier-updates", "via": "bot", "merge": {"can_merge": true, "methods": ["squash"]}, "checks": {"passed": 2, "failed": 0, "pending": 0, "waiting": 0},
         "web_path": "/DemiInfinity/Tray/pulls/9"},
        {"repo": "elyndrastudios/sparky-ventures", "number": 63, "title": "Soften the pricing copy", "state": "open", "head": "copy", "base": "main",
         "author": "cursor agent on GitHub", "via": "tether", "merge": {"can_merge": true, "methods": ["merge"], "on_github": true},
         "checks": {"passed": 1, "failed": 0, "pending": 0, "waiting": 0}, "web_path": "/elyndrastudios/sparky-ventures/pulls/63"}
    ]}))
    .into_response()
}

async fn pull_detail(
    headers: HeaderMap,
    Path((owner, name, number)): Path<(String, String, u64)>,
) -> Response {
    guard!(headers);
    if owner == "DemiInfinity" && name == "Atelier" && number == 14 {
        return Json(pull(true)).into_response();
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

async fn cancel_merge(headers: HeaderMap) -> Response {
    guard!(headers);
    Json(json!({"cancelled": true})).into_response()
}

async fn runs(headers: HeaderMap) -> Response {
    guard!(headers);
    Json(json!({"runs": [
        {"run_id": 207, "repo": "DemiInfinity/Atelier", "workflow": "ci", "event": "push", "branch": "feature/tunnel-ui", "status": "success",
         "started_at": now(), "duration_s": 108, "web_path": "/DemiInfinity/Atelier/actions/runs/207",
         "jobs": [{"job_id": 410, "name": "test", "status": "success", "runner": "MacBook", "duration_s": 108, "kind": "job"},
                  {"job_id": 411, "name": "lint", "status": "success", "runner": "MacBook", "duration_s": 22, "kind": "job"}]},
        {"run_id": 208, "repo": "DemiInfinity/Atelier", "workflow": "ci", "event": "push", "branch": "feature/ssh", "status": "failure",
         "started_at": now(), "duration_s": 123, "web_path": "/DemiInfinity/Atelier/actions/runs/208",
         "jobs": [{"job_id": 421, "name": "test", "status": "failure", "runner": "MacBook", "duration_s": 123, "kind": "job"}]}
    ]}))
    .into_response()
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
                    json!({"ok": true, "root": "~/Atelier", "presets": ["fork", "vscode", "cursor"], "items": []})
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
        .route("/pulls", get(pulls))
        .route("/repos/{owner}/{name}/pulls/{number}", get(pull_detail))
        .route(
            "/repos/{owner}/{name}/pulls/{number}/merge",
            post(merge).delete(cancel_merge),
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
