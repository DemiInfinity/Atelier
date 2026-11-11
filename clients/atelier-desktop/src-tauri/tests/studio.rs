//! The studio side of the command layer against a mock studio: Today's
//! mapping, pull requests, agents, remote access, messages, links, tools.

mod common;

use atelier_desktop::notify::Target;
use atelier_desktop::today::NeedAction;
use common::*;
use serde_json::json;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::UnixListener;

const RUNNER_STATUS: &str = r#"{"ok":true,"name":"MacBook","version":"7f82d31","server":"https://studio.example","online":true,"taking_jobs":true,"paused_reason":null,"capabilities":["jobs","agents","claude-code","workspaces"],"rules":{"pause_on_battery":true,"max_parallel":1,"container_jobs":false},"current":[{"job_id":412,"repo":"DemiInfinity/Atelier","workflow":"review-helper","kind":"agent","started_at":"2026-11-11T09:14:02Z","detail":"turn 3 of 20"}]}"#;

/// A runner on the test's own socket that answers every request by `op`
/// and remembers what it was asked.
fn fake_runner(world: &World) -> std::sync::Arc<std::sync::Mutex<Vec<serde_json::Value>>> {
    let socket = world.base().join("run/control.sock");
    let listener = UnixListener::bind(&socket).unwrap();
    let asked = std::sync::Arc::new(std::sync::Mutex::new(Vec::new()));
    let seen = asked.clone();
    tokio::spawn(async move {
        loop {
            let Ok((stream, _)) = listener.accept().await else {
                return;
            };
            let mut stream = BufReader::new(stream);
            let mut line = String::new();
            if stream.read_line(&mut line).await.is_err() {
                continue;
            }
            let request: serde_json::Value = serde_json::from_str(&line).unwrap_or_default();
            let answer = match request["op"].as_str() {
                Some("status") | Some("set") => RUNNER_STATUS.to_string(),
                Some("tail") => r#"{"ok":true,"lines":["→ pulls.get Atelier 14","← 3 files"]}"#.to_string(),
                Some("workspaces") => r#"{"ok":true,"root":"/Users/x/Atelier","presets":["fork","vscode"],"items":[]}"#.to_string(),
                _ => r#"{"ok":true}"#.to_string(),
            };
            seen.lock().unwrap().push(request);
            let _ = stream
                .get_mut()
                .write_all(format!("{answer}\n").as_bytes())
                .await;
        }
    });
    asked
}

#[tokio::test]
async fn today_maps_the_studio_status_and_adds_what_only_this_machine_knows() {
    let world = World::signed_in().await;
    world.mock_get("/status", status()).await;
    world
        .mock_get(
            "/repos",
            json!({"repos": [
                repo_json("DemiInfinity/Atelier", json!({"open_pulls": 1})),
                repo_json("DemiInfinity/Marbl", json!({})),
                repo_json("elyndrastudios/sparky-ventures", json!({"owner_kind": "company", "open_pulls": 2,
                    "github": {"repo": "elyndrastudios/sparky-ventures", "home": "github"}})),
            ]}),
        )
        .await;
    fake_runner(&world);

    // Atelier: one change. Marbl: two commits to push. sparky: clean.
    let (atelier, _) = world.copy("DemiInfinity/Atelier").await;
    write(&atelier, "notes.txt", "wip\n");
    let (marbl, _) = world.copy("DemiInfinity/Marbl").await;
    world.commit(&marbl, "a.txt", "a\n").await;
    world.commit(&marbl, "b.txt", "b\n").await;
    world.copy("elyndrastudios/sparky-ventures").await;
    // A git folder the studio doesn't know stays out of the list.
    let stray = world.home().join("Projects/scratch");
    std::fs::create_dir_all(&stray).unwrap();
    world.g(&stray, &["init", "--quiet"]).await;

    let o = world.desk.overview().await.unwrap();
    assert_eq!(o.studio_error, None);

    // Needs: the agent and the failure first, then the local push, then the rest.
    let needs: Vec<(&str, &str, &str)> = o
        .needs
        .iter()
        .map(|n| (n.tone, n.title.as_str(), n.label.as_str()))
        .collect();
    assert_eq!(
        needs,
        [
            ("agents", "Approve weekly-tidy before it runs", "Review"),
            ("fail", "ci failed on feature/ssh", "Open log"),
            ("yours", "2 commits are not pushed", "Push"),
            ("yours", "Security update is ready to merge", "Review"),
        ]
    );
    assert_eq!(
        o.needs[0].detail,
        "Atelier · agent · wants issues.close and issues.comment"
    );
    assert_eq!(o.needs[0].action, NeedAction::ApproveAgent { job_id: 31 });
    assert_eq!(
        o.needs[1].action,
        NeedAction::OpenLog {
            run_id: 208,
            repo: "DemiInfinity/Atelier".into(),
            web_path: "/DemiInfinity/Atelier/actions/runs/208".into()
        }
    );
    assert_eq!(o.needs[2].detail, "Marbl · main");
    assert_eq!(
        o.needs[2].action,
        NeedAction::Push {
            path: marbl.to_string_lossy().into_owned()
        }
    );
    assert_eq!(
        o.needs[3].action,
        NeedAction::OpenPull {
            repo: "DemiInfinity/Tray".into(),
            number: 9
        }
    );

    // Working copies, grouped by account, with their words and one-click action.
    let copies: Vec<(&str, &str, &str, &str, &str, &str)> = o
        .copies
        .iter()
        .map(|c| {
            (
                c.owner.as_str(),
                c.name.as_str(),
                c.state.as_str(),
                c.tone,
                c.action,
                c.short.as_str(),
            )
        })
        .collect();
    assert_eq!(
        copies,
        [
            ("DemiInfinity", "Atelier", "1 change", "yours", "open", "1"),
            ("DemiInfinity", "Marbl", "2 to push", "yours", "push", "↑2"),
            (
                "Elyndra Studios LTD",
                "sparky-ventures",
                "Clean · follows GitHub",
                "quiet",
                "open",
                ""
            ),
        ]
    );
    assert_eq!(o.copies[0].branch, "main");
    assert_eq!(o.roots_label, "in ~/Atelier and ~/Projects");
    assert_eq!(o.pulls_open, 3);
    assert_eq!(o.unread, 3);

    // Running here: the runner knows the turn, the studio what it is about.
    assert_eq!(o.running.len(), 1);
    assert_eq!(o.running[0].title, "review-helper");
    assert_eq!(o.running[0].engine.as_deref(), Some("Claude Code"));
    assert_eq!(
        o.running[0].detail,
        "Atelier · pull request 14 · turn 3 of 20"
    );
    assert_eq!(o.running[0].progress, Some(0.15));
    assert_eq!(o.elsewhere, ["image · build on Portainer host"]);
    assert!(o.runner.running && o.runner.taking_jobs);
    assert_eq!(o.runner.line, "Runner on · 1 job running");
    assert_eq!(o.runner.switch_line, "On · pauses on battery");

    // The feed keeps its three threads.
    let feed: Vec<(&str, &str)> = o.feed.iter().map(|f| (f.tone, f.tag)).collect();
    assert_eq!(
        feed,
        [
            ("yours", "You"),
            ("github", "via Tether"),
            ("connect", "via Connect")
        ]
    );
    assert_eq!(
        o.feed[0].target,
        Some(Target::Pull {
            repo: "DemiInfinity/Atelier".into(),
            number: 13
        })
    );
    assert_eq!(
        o.feed[2].target, None,
        "a link to someone else's studio isn't followed"
    );
}

#[tokio::test]
async fn today_still_shows_this_machine_when_the_studio_is_away() {
    let world = World::signed_in().await;
    world.copy("DemiInfinity/Atelier").await;
    world
        .mock("GET", "/status", 500, json!({"error": "internal"}))
        .await;
    world
        .mock("GET", "/repos", 500, json!({"error": "internal"}))
        .await;
    let o = world.desk.overview().await.unwrap();
    assert!(o.studio_error.is_some());
    assert_eq!(o.copies.len(), 1, "git folders are shown unmatched");
    assert_eq!(o.copies[0].full_name, None);
    assert!(!o.runner.running);
    assert_eq!(o.runner.line, "Runner isn’t running");
}

#[tokio::test]
async fn this_device_reads_the_runner_and_flips_its_switches() {
    let world = World::signed_in().await;
    let asked = fake_runner(&world);
    world.mock_get("/status", status()).await;
    world
        .mock_get(
            "/agents/waiting",
            json!({"jobs": [{"job_id": 31, "run_id": 210, "repo": "DemiInfinity/Atelier", "workflow": "weekly-tidy",
                "reason": "first run", "engine": "claude-code", "tools": ["issues.list", "issues.comment", "issues.close"],
                "write_tools": ["issues.comment", "issues.close"], "max_turns": 20, "timeout_minutes": 10,
                "web_path": "/DemiInfinity/Atelier/actions/runs/210"}]}),
        )
        .await;
    world
        .mock_get(
            "/runs",
            json!({"runs": [{"run_id": 207, "repo": "DemiInfinity/Atelier", "workflow": "ci", "event": "push",
                "branch": "feature/tunnel-ui", "status": "success", "started_at": "2026-11-11T09:14:00Z", "duration_s": 108,
                "web_path": "/DemiInfinity/Atelier/actions/runs/207",
                "jobs": [{"job_id": 410, "name": "test", "status": "success", "runner": "MacBook", "duration_s": 108, "kind": "job"},
                         {"job_id": 411, "name": "lint", "status": "failure", "runner": "MacBook", "duration_s": 22, "kind": "job"},
                         {"job_id": 499, "name": "deploy", "status": "success", "runner": "Other", "duration_s": 5, "kind": "job"}]}]}),
        )
        .await;

    let view = world.desk.device_view().await.unwrap();
    assert!(view.running && view.installed && view.online && view.taking_jobs && view.claude_code);
    assert_eq!(view.name, "MacBook");
    assert_eq!(view.jobs.len(), 1);
    assert_eq!(view.jobs[0].tail, ["→ pulls.get Atelier 14", "← 3 files"]);
    assert_eq!(
        view.waiting[0].title,
        "weekly-tidy wants to run for the first time"
    );
    let runs: Vec<(&str, &str, &str, &str)> = view
        .runs
        .iter()
        .map(|r| {
            (
                r.status.text.as_str(),
                r.name.as_str(),
                r.wher.as_str(),
                r.duration.as_str(),
            )
        })
        .collect();
    assert_eq!(
        runs,
        [
            (
                "Passed",
                "ci · test",
                "Atelier · feature/tunnel-ui",
                "1m 48s"
            ),
            ("Failed", "ci · lint", "Atelier · feature/tunnel-ui", "22s"),
        ]
    );
    let rules: Vec<(&str, bool)> = view.rules.iter().map(|r| (r.id, r.on)).collect();
    assert_eq!(
        rules,
        [
            ("pause_on_battery", true),
            ("one_at_a_time", true),
            ("container_jobs", false)
        ]
    );
    let workspaces = view.workspaces.unwrap();
    assert_eq!(workspaces.presets, ["Fork", "VS Code"]);

    world.desk.runner_set_taking(false).await.unwrap();
    world
        .desk
        .runner_set_rule("one_at_a_time", false)
        .await
        .unwrap();
    world
        .desk
        .runner_set_rule("container_jobs", true)
        .await
        .unwrap();
    assert_eq!(
        world
            .desk
            .runner_set_rule("rm -rf", true)
            .await
            .unwrap_err()
            .kind,
        "invalid"
    );
    world.desk.job_stop(412).await.unwrap();
    let asked = asked.lock().unwrap();
    assert!(asked.contains(&json!({"op": "set", "taking_jobs": false})));
    assert!(asked.contains(&json!({"op": "set", "rules": {"max_parallel": 2}})));
    assert!(asked.contains(&json!({"op": "set", "rules": {"container_jobs": true}})));
    assert!(
        asked.contains(&json!({"op": "cancel", "job_id": 412})),
        "a job running here is stopped through the runner"
    );
}

#[tokio::test]
async fn a_run_log_shows_the_failed_jobs() {
    let world = World::signed_in().await;
    world
        .mock_get(
            "/runs",
            json!({"runs": [{"run_id": 208, "repo": "DemiInfinity/Atelier", "workflow": "ci", "branch": "feature/ssh", "status": "failure",
                "jobs": [{"job_id": 420, "name": "lint", "status": "success"}, {"job_id": 421, "name": "test", "status": "failure"}]}]}),
        )
        .await;
    world
        .mock_get(
            "/jobs/421/log",
            json!({"lines": ["FAIL TestSSH", "exit 1"], "truncated": true, "status": "failure"}),
        )
        .await;
    let log = world
        .desk
        .run_log("DemiInfinity/Atelier", 208)
        .await
        .unwrap();
    assert_eq!(log.title, "ci on feature/ssh · Atelier");
    assert_eq!(log.lines, ["FAIL TestSSH", "exit 1"]);
    assert!(log.truncated);
    assert_eq!(
        world
            .desk
            .run_log("DemiInfinity/Atelier", 1)
            .await
            .unwrap_err()
            .kind,
        "not_found"
    );
}

fn pull_json() -> serde_json::Value {
    json!({"repo": "DemiInfinity/Atelier", "number": 14, "title": "Remote access switch in the tray menu", "state": "open",
        "head": "feature/tunnel-ui", "base": "main", "author": "owner:DemiInfinity", "commits": 2, "files": 3, "via": null,
        "merge": {"can_merge": false, "reason": "1 check is still running and 1 agent is waiting for you.", "methods": ["squash", "merge"],
                  "on_github": false, "auto_merge": null},
        "checks": [{"name": "ci · test", "status": "success", "detail": "MacBook · 1m 48s", "run_id": 207, "job_id": 410},
                   {"name": "image · build", "status": "pending", "detail": "Portainer host · step 3 of 5", "run_id": 209, "job_id": 413},
                   {"name": "review-helper", "status": "waiting", "detail": "agent · needs your approval to run", "run_id": 210, "job_id": 414, "action": "approve_agent"}],
        "comments": [{"author": "owner:DemiInfinity", "via": null, "kind": "person", "body": "Adds the switch.", "at": "2026-11-11T09:00:00Z"},
                     {"author": "CodeRabbit", "via": "tether", "kind": "bot", "body": "<b>toggleRemote</b> ignores the error.", "at": "2026-11-11T09:04:00Z"}],
        "web_path": "/DemiInfinity/Atelier/pulls/14"})
}

#[tokio::test]
async fn pull_requests_list_detail_merge_when_green_and_cancel() {
    let world = World::signed_in().await;
    world
        .mock_get(
            "/pulls",
            json!({"pulls": [
                {"repo": "DemiInfinity/Atelier", "number": 14, "title": "Remote access switch in the tray menu", "state": "open",
                 "author": "owner:DemiInfinity", "merge": {"can_merge": false}, "checks": {"passed": 2, "failed": 0, "pending": 1, "waiting": 0}},
                {"repo": "elyndrastudios/sparky-ventures", "number": 63, "title": "Soften the pricing copy", "state": "open",
                 "author": "cursor agent on GitHub", "via": "tether", "merge": {"can_merge": true}, "checks": {"passed": 1, "failed": 0, "pending": 0, "waiting": 0}}
            ]}),
        )
        .await;
    world
        .mock_get("/repos/DemiInfinity/Atelier/pulls/14", pull_json())
        .await;
    world.mock_get("/repos", json!({"repos": []})).await;

    let list = world.desk.pulls_list(None).await.unwrap();
    let rows: Vec<(&str, &str, &str, Option<&str>)> = list
        .iter()
        .map(|p| {
            (
                p.repo_short.as_str(),
                p.by.as_str(),
                p.status.text.as_str(),
                p.via.as_ref().map(|v| v.text.as_str()),
            )
        })
        .collect();
    assert_eq!(
        rows,
        [
            ("Atelier", "you", "Checks running", None),
            (
                "sparky-ventures",
                "cursor agent on GitHub",
                "Ready to merge",
                Some("via Tether")
            ),
        ]
    );

    let pull = world
        .desk
        .pull_detail("DemiInfinity/Atelier", 14)
        .await
        .unwrap();
    assert_eq!(pull.headline, "Not ready to merge yet");
    assert!(!pull.can_merge);
    assert_eq!(pull.by, "you");
    assert_eq!(
        pull.methods.iter().map(|m| m.label).collect::<Vec<_>>(),
        ["Squash and merge", "Merge"]
    );
    let checks: Vec<(&str, &str, Option<&str>)> = pull
        .checks
        .iter()
        .map(|c| {
            (
                c.status.text.as_str(),
                c.status.tone,
                c.action_label.as_deref(),
            )
        })
        .collect();
    assert_eq!(
        checks,
        [
            ("Passed", "pass", None),
            ("Running", "yours", None),
            ("Waiting", "agents", Some("Approve"))
        ]
    );
    assert_eq!(
        pull.checks[2].action,
        Some(NeedAction::ApproveAgent { job_id: 414 })
    );
    assert_eq!(pull.comments[1].tag, "bot · via Tether");
    assert_eq!(pull.comments[1].tone, "github");
    assert_eq!(
        pull.comments[1].body, "<b>toggleRemote</b> ignores the error.",
        "bodies are passed as text, untouched"
    );
    assert_eq!(pull.local_path, None);

    // Merge when the checks pass.
    world
        .mock(
            "POST",
            "/repos/DemiInfinity/Atelier/pulls/14/merge",
            202,
            json!({"auto_merge": {"method": "squash", "by": "device:MacBook"}}),
        )
        .await;
    let result = world
        .desk
        .pull_merge("DemiInfinity/Atelier", 14, "squash", true)
        .await
        .unwrap();
    assert!(result.scheduled && !result.merged);
    assert_eq!(
        world
            .received("POST", "/repos/DemiInfinity/Atelier/pulls/14/merge")
            .await,
        [json!({"method": "squash", "when_green": true})]
    );
    // And cancel.
    world
        .mock(
            "DELETE",
            "/repos/DemiInfinity/Atelier/pulls/14/merge",
            200,
            json!({"cancelled": true}),
        )
        .await;
    world
        .desk
        .pull_cancel_merge("DemiInfinity/Atelier", 14)
        .await
        .unwrap();
    assert_eq!(
        world
            .received("DELETE", "/repos/DemiInfinity/Atelier/pulls/14/merge")
            .await
            .len(),
        1
    );

    assert_eq!(
        world
            .desk
            .pull_merge("DemiInfinity/Atelier", 14, "fast", false)
            .await
            .unwrap_err()
            .kind,
        "invalid"
    );
    assert_eq!(
        world.desk.pull_detail("../etc", 1).await.unwrap_err().kind,
        "invalid"
    );
    // No working copy here: checking out says what to do.
    let err = world
        .desk
        .pull_checkout("DemiInfinity/Atelier", 14)
        .await
        .unwrap_err();
    assert_eq!(err.kind, "no_copy");
}

#[tokio::test]
async fn merging_now_says_so_and_a_refusal_keeps_the_studios_reason() {
    let world = World::signed_in().await;
    world
        .mock_get(
            "/repos/DemiInfinity/Tray/pulls/9",
            json!({"repo": "DemiInfinity/Tray", "number": 9, "base": "main", "state": "merged"}),
        )
        .await;
    world.mock_get("/messages", messages("friendly")).await;
    world
        .mock(
            "POST",
            "/repos/DemiInfinity/Tray/pulls/9/merge",
            200,
            json!({"merged": true, "sha": "7f82d31"}),
        )
        .await;
    let result = world
        .desk
        .pull_merge("DemiInfinity/Tray", 9, "merge", false)
        .await
        .unwrap();
    assert!(result.merged);
    assert_eq!(result.sha, "7f82d31");
    let toast = result.toast.unwrap();
    assert_eq!(toast.text, "Merged into main. Nicely done.");
    assert_eq!(toast.sub, "Tray · pull request 9");

    world
        .mock(
            "POST",
            "/repos/DemiInfinity/Atelier/pulls/14/merge",
            409,
            json!({"error": "not_mergeable", "message": "1 check is still running."}),
        )
        .await;
    let err = world
        .desk
        .pull_merge("DemiInfinity/Atelier", 14, "squash", false)
        .await
        .unwrap_err();
    assert_eq!(
        (err.kind.as_str(), err.message.as_str()),
        ("not_mergeable", "1 check is still running.")
    );
}

#[tokio::test]
async fn checking_out_a_pull_request_switches_the_local_copy() {
    let world = World::signed_in().await;
    let (repo, bare) = world.copy("DemiInfinity/Atelier").await;
    // Someone pushes the branch to the studio.
    let other = world.base().join("other");
    world
        .g(
            &world.base(),
            &[
                "clone",
                "--quiet",
                bare.to_str().unwrap(),
                other.to_str().unwrap(),
            ],
        )
        .await;
    world
        .g(&other, &["switch", "--quiet", "-c", "feature/tunnel-ui"])
        .await;
    world.commit(&other, "tray.go", "package main\n").await;
    world
        .g(
            &other,
            &["push", "--quiet", "-u", "origin", "feature/tunnel-ui"],
        )
        .await;

    world
        .mock_get(
            "/repos",
            json!({"repos": [repo_json("DemiInfinity/Atelier", json!({}))]}),
        )
        .await;
    world
        .mock_get("/repos/DemiInfinity/Atelier/pulls/14", pull_json())
        .await;
    let out = world
        .desk
        .pull_checkout("DemiInfinity/Atelier", 14)
        .await
        .unwrap();
    assert_eq!(out.branch, "feature/tunnel-ui");
    assert_eq!(out.path, repo.to_string_lossy());
    assert_eq!(
        world.g(&repo, &["branch", "--show-current"]).await,
        "feature/tunnel-ui"
    );
    assert!(repo.join("tray.go").exists());
    let pull = world
        .desk
        .pull_detail("DemiInfinity/Atelier", 14)
        .await
        .unwrap();
    assert_eq!(pull.local_path.as_deref(), Some(out.path.as_str()));
}

#[tokio::test]
async fn approving_an_agent() {
    let world = World::signed_in().await;
    world
        .mock(
            "POST",
            "/agents/jobs/31/approve",
            200,
            json!({"approved": true}),
        )
        .await;
    world
        .mock(
            "POST",
            "/agents/jobs/32/approve",
            409,
            json!({"error": "not_waiting"}),
        )
        .await;
    assert!(world.desk.agent_approve(31).await.unwrap());
    let err = world.desk.agent_approve(32).await.unwrap_err();
    assert_eq!(err.kind, "not_waiting");
    assert_eq!(
        err.message,
        "That agent isn’t waiting for approval any more."
    );
}

#[tokio::test]
async fn remote_access_on_off_who_and_the_passkey_link() {
    let world = World::signed_in().await;
    world
        .mock_get("/remote", json!({"on": false, "mode": "quick", "address": null, "exposed": ["web", "git"], "connected": 0}))
        .await;
    let view = world.desk.remote_view().await;
    assert!(view.available && !view.on);
    assert_eq!(
        view.line,
        "Your studio can only be reached at home. Turn this on to reach it from anywhere."
    );
    assert_eq!(view.switch_line, "Off · your studio is home only");

    world
        .mock("POST", "/remote", 200,
            json!({"on": true, "mode": "quick", "address": "https://quiet-fox-lamp.trycloudflare.com", "exposed": ["web", "git"], "connected": 1}))
        .await;
    world
        .mock_get("/remote/who", json!({"sessions": [{"ip": "203.0.113.42", "country": "NL", "signed_in": true, "class": "web", "since": "2026-11-11T09:00:00Z"}]}))
        .await;
    let view = world.desk.remote_set(true).await.unwrap();
    assert!(view.on);
    assert_eq!(
        view.line,
        "Your studio is reachable from outside at https://quiet-fox-lamp.trycloudflare.com. 1 person is connected."
    );
    assert_eq!(view.sessions[0].ip, "203.0.113.42");
    assert_eq!(
        world.received("POST", "/remote").await,
        [json!({"on": true})]
    );

    world
        .mock(
            "POST",
            "/remote/link",
            201,
            json!({"url": "https://quiet-fox-lamp.trycloudflare.com/k/abc", "expires_in": 900}),
        )
        .await;
    let link = world.desk.remote_link().await.unwrap();
    assert_eq!(link.expires_in, 900);
    // The link is for another device: this app would refuse to open it.
    assert!(!world.desk.url_allowed(&link.url));
}

#[tokio::test]
async fn remote_access_that_is_not_offered_is_shown_as_unavailable() {
    let world = World::signed_in().await;
    world
        .mock(
            "GET",
            "/remote",
            403,
            json!({"error": "forbidden", "message": "Remote access is for the studio’s owner."}),
        )
        .await;
    let view = world.desk.remote_view().await;
    assert!(!view.available && !view.on);
    assert_eq!(view.line, "Remote access is for the studio’s owner.");
}

#[tokio::test]
async fn messages_tone_add_remove_and_rules_go_through_the_studio() {
    let world = World::signed_in().await;
    world.mock_get("/messages", messages("friendly")).await;
    world.mock("PUT", "/messages", 200, messages("plain")).await;

    let view = world.desk.messages_view().await.unwrap();
    assert_eq!(view.tone, "friendly");
    let keys: Vec<&str> = view.moments.iter().map(|m| m.key.as_str()).collect();
    assert_eq!(
        keys,
        ["push", "clean", "pass", "fail", "merge", "first", "late", "mile", "agent"]
    );
    let push = &view.moments[0];
    assert_eq!(push.label, "After a push");
    assert_eq!(
        push.ours[0].text,
        "Home and dry. {branch} is safe in your studio."
    );
    assert_eq!(
        push.ours[0].preview,
        "Home and dry. main is safe in your studio."
    );
    assert_eq!(
        push.ours[2].preview,
        "Pushed. Checks are warming up on your MacBook."
    );
    assert_eq!(push.yours[0].text, "Shipped. Go outside.");
    assert_eq!(push.plain, "Pushed main.");
    assert_eq!(view.moments[8].tone, "agents");

    assert_eq!(
        world.desk.messages_set_tone("plain").await.unwrap().tone,
        "plain"
    );
    world
        .desk
        .messages_add("push", "  Off   to the races.  ")
        .await
        .unwrap();
    world.desk.messages_remove("push", 1).await.unwrap();
    world
        .desk
        .messages_set_rule("milestones", false)
        .await
        .unwrap();
    let sent = world.received("PUT", "/messages").await;
    assert_eq!(
        sent,
        [
            json!({"tone": "plain"}),
            json!({"yours": {"push": ["Shipped. Go outside.", "Off to the races."]}}),
            json!({"yours": {"push": []}}),
            json!({"rules": {"quiet_fullscreen": true, "terminal": true, "milestones": false}}),
        ]
    );

    // Refused before the studio is asked.
    let before = world.received("PUT", "/messages").await.len();
    assert_eq!(
        world
            .desk
            .messages_set_tone("shouty")
            .await
            .unwrap_err()
            .kind,
        "invalid"
    );
    assert_eq!(
        world
            .desk
            .messages_add("push", &"x".repeat(141))
            .await
            .unwrap_err()
            .kind,
        "invalid"
    );
    assert_eq!(
        world
            .desk
            .messages_add("nope", "hi")
            .await
            .unwrap_err()
            .kind,
        "invalid"
    );
    assert_eq!(
        world
            .desk
            .messages_remove("push", 9)
            .await
            .unwrap_err()
            .kind,
        "invalid"
    );
    assert_eq!(
        world
            .desk
            .messages_set_rule("loud", true)
            .await
            .unwrap_err()
            .kind,
        "invalid"
    );
    assert_eq!(world.received("PUT", "/messages").await.len(), before);
}

#[tokio::test]
async fn only_the_studio_and_github_are_ever_opened() {
    let world = World::signed_in().await;
    let studio = world.server.uri();
    world
        .desk
        .open_web("/DemiInfinity/Atelier/pulls/14")
        .unwrap();
    world.desk.open_github("maralind/loopkit").unwrap();
    world.desk.open_url("https://github.com/maralind").unwrap();
    assert_eq!(
        *world.effects.opened.lock().unwrap(),
        [
            format!("{studio}/DemiInfinity/Atelier/pulls/14"),
            "https://github.com/maralind/loopkit".to_string(),
            "https://github.com/maralind".to_string(),
        ]
    );
    for refused in [
        "https://evil.example/",
        "http://github.com/x",
        "https://github.com.evil.example/x",
        "https://user@github.com/x",
        "https://github.com:8443/x",
        "file:///etc/passwd",
        "javascript:alert(1)",
        "not a url",
    ] {
        assert_eq!(
            world.desk.open_url(refused).unwrap_err().kind,
            "link_refused",
            "{refused}"
        );
    }
    // A "web path" that tries to leave the studio.
    for refused in [
        "//evil.example/x",
        "https://evil.example",
        "\\\\evil",
        "relative/path",
    ] {
        assert_eq!(
            world.desk.open_web(refused).unwrap_err().kind,
            "link_refused",
            "{refused}"
        );
    }
    assert!(world.desk.open_github("a/b/c").is_err());
    assert_eq!(world.effects.opened.lock().unwrap().len(), 3);
}

#[tokio::test]
async fn editors_are_presets_launched_without_a_shell() {
    let world = World::signed_in().await;
    let (repo, _) = world.copy("DemiInfinity/Atelier").await;
    let path = repo.to_string_lossy().into_owned();
    world.mock_get("/repos", json!({"repos": []})).await;

    // The configured editor (none yet: VS Code), then a chosen one, then the git tool.
    assert_eq!(world.desk.repo_open(&path, None).await.unwrap(), "VS Code");
    world.desk.set_editor("zed").unwrap();
    assert_eq!(world.desk.repo_open(&path, None).await.unwrap(), "Zed");
    assert_eq!(
        world.desk.repo_open(&path, Some("finder")).await.unwrap(),
        "Finder"
    );
    assert_eq!(world.desk.repo_hand_over(&path).await.unwrap(), "Fork");
    world.desk.set_git_tool("terminal").unwrap();
    assert_eq!(world.desk.repo_hand_over(&path).await.unwrap(), "Terminal");

    let launched = world.effects.launched.lock().unwrap().clone();
    assert_eq!(launched.len(), 5);
    if cfg!(target_os = "macos") {
        assert_eq!(
            launched[0],
            (
                "open".to_string(),
                vec![
                    "-a".to_string(),
                    "Visual Studio Code".to_string(),
                    "--".to_string(),
                    path.clone()
                ]
            )
        );
        assert_eq!(
            launched[2],
            ("open".to_string(), vec!["--".to_string(), path.clone()])
        );
    }
    for (_, args) in &launched {
        assert_eq!(
            args.last(),
            Some(&path),
            "the folder is always one whole argument"
        );
    }

    // Anything that isn't a preset is refused, and never becomes a command.
    assert_eq!(
        world
            .desk
            .repo_open(&path, Some("sh -c 'curl x | sh'"))
            .await
            .unwrap_err()
            .kind,
        "invalid"
    );
    assert_eq!(
        world.desk.set_editor("/bin/sh").unwrap_err().kind,
        "invalid"
    );
    assert_eq!(world.desk.set_editor("fork").unwrap_err().kind, "invalid");
    assert_eq!(
        world.desk.set_git_tool("vscode").unwrap_err().kind,
        "invalid"
    );
    world.desk.set_git_tool("none").unwrap();
    assert_eq!(
        world.desk.repo_hand_over(&path).await.unwrap_err().kind,
        "invalid"
    );
    assert_eq!(world.effects.launched.lock().unwrap().len(), 5);

    // What was chosen is in the shared config, where atl reads it too.
    let config = atelier_core::Config::load(&world.base().join("config")).unwrap();
    assert_eq!(config.editor.as_deref(), Some("zed"));
    assert_eq!(config.git_tool.as_deref(), Some("none"));
    // A command someone put in the config by hand for atl is never run here.
    world
        .desk
        .update_config(|c| c.editor = Some("my-script --flag".into()))
        .unwrap();
    assert_eq!(world.desk.repo_open(&path, None).await.unwrap(), "VS Code");
}

#[tokio::test]
async fn preferences_are_kept_in_the_shared_config() {
    let world = World::signed_in().await;
    world.desk.set_notify_pref("pass", true).unwrap();
    world.desk.set_notify_pref("fail", false).unwrap();
    world.desk.set_app_pref("hide_dock", true).unwrap();
    assert_eq!(
        world
            .desk
            .set_notify_pref("everything", true)
            .unwrap_err()
            .kind,
        "invalid"
    );
    assert_eq!(
        world.desk.set_app_pref("root", true).unwrap_err().kind,
        "invalid"
    );
    let prefs = world.desk.prefs();
    assert!(prefs.notify.pass && !prefs.notify.fail && prefs.hide_dock && prefs.menu_bar);
    let text = std::fs::read_to_string(world.base().join("config/config.toml")).unwrap();
    assert!(text.contains("[desktop]") && text.contains("device_name = \"MacBook\""));
}
