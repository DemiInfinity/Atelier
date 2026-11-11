//! The real `atl` binary against a mock studio: the designed screens as
//! plain text (no terminal, so no colour and no questions) and as `--json`.

mod common;

use common::*;
use serde_json::json;

fn this_machine() -> &'static str {
    match std::env::consts::OS {
        "macos" => "this Mac",
        "windows" => "this PC",
        _ => "this machine",
    }
}

/// Two working copies under the clone root: Atelier on a feature branch
/// (1 commit ahead, 3 changed files) and Marbl on master (2 commits ahead).
fn working_copies(sb: &Sandbox) -> std::path::PathBuf {
    let atelier = sb.home().join("Atelier/DemiInfinity/Atelier");
    sb.repo(&atelier);
    sb.track(
        &atelier,
        "ssh://git@studio.example:2223/DemiInfinity/Atelier.git",
    );
    sb.git(
        &atelier,
        &[
            "checkout",
            "--quiet",
            "-b",
            "feature/tunnel-ui",
            "--track",
            "origin/main",
        ],
    );
    sb.commit(&atelier, "tray.go");
    for file in ["a", "b", "c"] {
        std::fs::write(atelier.join(file), "x").unwrap();
    }
    let marbl = sb.home().join("Atelier/DemiInfinity/Marbl");
    sb.repo(&marbl);
    sb.git(&marbl, &["branch", "-m", "master"]);
    // The scp form with other capitals: still the same repository.
    sb.track(&marbl, "git@Studio.Example:demiinfinity/marbl.git");
    sb.commit(&marbl, "one");
    sb.commit(&marbl, "two");
    atelier
}

// ------------------------------------------------------------------ status

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn status_text() {
    let sb = Sandbox::new().await;
    sb.get("/status", STATUS).await;
    sb.get("/repos", REPOS).await;
    let atelier = working_copies(&sb);

    let out = sb.atl_in(&atelier, &["status"]).await.ok();
    assert_eq!(
        out,
        "\
Needs you
  ● Approve weekly-tidy before it runs  Atelier  agent · wants issues.close and issues.comment  atl agent approve 31
  ● ci failed on feature/ssh            Atelier  test job · ran on Portainer host               atl run log 208
  ● Security update is ready to merge   Tray     okhttp 5.1 fixes 1 advisory · checks passed    atl pr 9 -R DemiInfinity/Tray
  ● 2 commits not pushed                Marbl    master                                         git push

Running
  ◆ review-helper  Atelier  pull request 14 · turn 3 of 20 · MacBook

Here  Atelier · feature/tunnel-ui
  3 changed files · 1 commit ahead of your studio

From GitHub
  ● A reviewer approved pull request 63 on sparky-ventures  via Tether

From Connect
  ● Mara starred Atelier from her studio  via Connect
"
    );
    // No colour without a terminal.
    assert!(!out.contains('\u{1b}'));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn status_json() {
    let sb = Sandbox::new().await;
    sb.get("/status", STATUS).await;
    sb.get("/repos", REPOS).await;
    let atelier = working_copies(&sb);

    let out = sb.atl_in(&atelier, &["status", "--json"]).await.json();
    let contract: serde_json::Value = serde_json::from_str(STATUS).unwrap();
    assert_eq!(out["needs"], contract["needs"]);
    assert_eq!(out["running"], contract["running"]);
    assert_eq!(out["feed"], contract["feed"]);
    assert_eq!(out["unread"], 3);
    assert_eq!(
        out["here"],
        json!({"repo":"DemiInfinity/Atelier","path": atelier,"branch":"feature/tunnel-ui",
               "upstream":"origin/main","changed_files":3,"ahead":1,"behind":0})
    );
    assert_eq!(
        out["unpushed"],
        json!([
            {"repo":"DemiInfinity/Atelier","path": atelier,"branch":"feature/tunnel-ui","ahead":1},
            {"repo":"DemiInfinity/Marbl","path": sb.home().join("Atelier/DemiInfinity/Marbl"),"branch":"master","ahead":2}
        ])
    );
    assert_eq!(
        out.as_object().unwrap().keys().collect::<Vec<_>>(),
        ["feed", "here", "needs", "running", "unpushed", "unread"]
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn status_when_all_is_quiet_and_outside_a_repository() {
    let sb = Sandbox::new().await;
    sb.get(
        "/status",
        r#"{"needs":[],"running":[],"feed":[],"unread":0}"#,
    )
    .await;
    sb.get("/repos", r#"{"repos":[]}"#).await;
    assert_eq!(sb.atl(&["status"]).await.ok(), "Nothing needs you.\n");
    let out = sb.atl(&["status", "--json"]).await.json();
    assert_eq!(out["here"], json!(null));
    assert_eq!(out["unpushed"], json!([]));

    // A folder that isn't in the studio yet says how to get it there.
    let loose = sb.work().join("Ingle");
    sb.repo(&loose);
    assert_eq!(
        sb.atl_in(&loose, &["status"]).await.ok(),
        "\
Nothing needs you.

Here  Ingle · main
  no changes · this branch isn’t pushed yet
  Not in your studio yet: atl repo publish
"
    );
}

// ---------------------------------------------------------------------- pr

async fn pulls(sb: &Sandbox) {
    sb.get("/repos", REPOS).await;
    let list = format!(r#"{{"pulls":[{PULL_SUMMARY}]}}"#);
    sb.get("/pulls", &list).await;
    sb.get("/repos/DemiInfinity/Atelier/pulls", &list).await;
    sb.get("/repos/DemiInfinity/Atelier/pulls/14", PULL).await;
}

const PR_SCREEN: &str = "\
#14  Remote access switch in the tray menu   feature/tunnel-ui → main
     ✓ ci · test       MacBook · 1m 48s
     ◐ image · build   Portainer host · step 3 of 5
     ◆ review-helper   agent · needs your approval to run   atl agent approve 414

     CodeRabbit · via Tether
     toggleRemote ignores the error from SetRemote.

     Can’t merge yet: 1 check is still running and 1 agent is waiting for you.
";

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn pr_text() {
    let sb = Sandbox::new().await;
    pulls(&sb).await;
    let atelier = working_copies(&sb);

    // In the repository: its one open pull request, in full.
    assert_eq!(sb.atl_in(&atelier, &["pr"]).await.ok(), PR_SCREEN);
    assert_eq!(sb.atl_in(&atelier, &["pr", "14"]).await.ok(), PR_SCREEN);
    // From anywhere with -R.
    assert_eq!(
        sb.atl(&["pr", "14", "-R", "DemiInfinity/Atelier"])
            .await
            .ok(),
        PR_SCREEN
    );
    // Outside a repository, or with --all: every open pull request, in short.
    let all = "\
#14  Remote access switch in the tray menu   feature/tunnel-ui → main   Atelier
     2 of 4 passed   ✓ 2 passed   ◐ 1 running   ◆ 1 waiting for you
";
    assert_eq!(sb.atl(&["pr"]).await.ok(), all);
    assert_eq!(sb.atl_in(&atelier, &["pr", "--all"]).await.ok(), all);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn pr_json() {
    let sb = Sandbox::new().await;
    pulls(&sb).await;
    let atelier = working_copies(&sb);

    let detail = sb.atl_in(&atelier, &["pr", "14", "--json"]).await.json();
    assert_eq!(
        detail,
        serde_json::from_str::<serde_json::Value>(PULL).unwrap()
    );

    let list = sb.atl_in(&atelier, &["--json", "pr"]).await.json();
    assert_eq!(list["repo"], "DemiInfinity/Atelier");
    assert_eq!(
        list["pulls"][0],
        serde_json::from_str::<serde_json::Value>(PULL_SUMMARY).unwrap()
    );
    assert_eq!(sb.atl(&["pr", "--json"]).await.json()["repo"], json!(null));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn pr_merge() {
    let sb = Sandbox::new().await;
    pulls(&sb).await;
    sb.get("/messages", MESSAGES).await;
    let route = "/repos/DemiInfinity/Atelier/pulls/14/merge";
    let scope = ["-R", "DemiInfinity/Atelier"];

    // When green: the designed two lines. The method is the repository's first.
    sb.respond(
        "POST",
        route,
        202,
        r#"{"auto_merge":{"method":"squash","by":"device:MacBook"}}"#,
    )
    .await;
    let out = sb
        .atl(&[&["pr", "merge", "14", "--when-green"], &scope[..]].concat())
        .await
        .ok();
    assert_eq!(
        out,
        "Will squash and merge #14 when every check passes.\nYou’ll get a message when it lands.\n"
    );
    let out = sb
        .atl(
            &[
                &[
                    "pr",
                    "merge",
                    "14",
                    "--when-green",
                    "--method",
                    "merge",
                    "--json",
                ],
                &scope[..],
            ]
            .concat(),
        )
        .await
        .json();
    assert_eq!(
        out,
        json!({"number":14,"merged":false,"auto_merge":{"method":"squash","by":"device:MacBook"}})
    );
    assert_eq!(
        sb.bodies("POST", route).await,
        [
            json!({"method":"squash","when_green":true}),
            json!({"method":"merge","when_green":true})
        ]
    );

    // Cancel.
    sb.respond("DELETE", route, 204, "").await;
    assert_eq!(
        sb.atl(&[&["pr", "merge", "14", "--cancel"], &scope[..]].concat())
            .await
            .ok(),
        "#14 won’t merge by itself any more.\n"
    );
    assert_eq!(sb.count("DELETE", route).await, 1);

    // Merged now, with a friendly word.
    sb.server.reset().await;
    pulls(&sb).await;
    sb.get("/messages", MESSAGES).await;
    sb.respond(
        "POST",
        route,
        200,
        r#"{"merged":true,"sha":"e04a9d1c2b3a4f5e6d7c8b9a"}"#,
    )
    .await;
    assert_eq!(
        sb.atl(&[&["pr", "merge", "14"], &scope[..]].concat())
            .await
            .ok(),
        "✓ Merged #14 into main (e04a9d1)\natelier: Merged into main. Nicely done.\n"
    );

    // Not mergeable: the studio's reason, then the next step.
    sb.server.reset().await;
    pulls(&sb).await;
    sb.respond(
        "POST",
        route,
        409,
        r#"{"error":"not_mergeable","message":"1 check is still running and 1 agent is waiting for you."}"#,
    )
    .await;
    let refused = sb.atl(&[&["pr", "merge", "14"], &scope[..]].concat()).await;
    assert_eq!(refused.code, 1);
    assert_eq!(refused.stdout, "");
    assert_eq!(
        refused.stderr,
        "1 check is still running and 1 agent is waiting for you. To merge once everything passes: atl pr merge 14 --when-green.\n"
    );
}

// ------------------------------------------------------------------- agent

const AGENT_CARD: &str = "\
weekly-tidy · Atelier · first run
  May use     issues.list  issues.comment  issues.close
  Can change  issues.comment  issues.close
  Engine      Claude Code
  Limits      20 turns · 10 minutes

";

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn agent_approve_text() {
    let sb = Sandbox::new().await;
    sb.get("/agents/waiting", WAITING).await;
    sb.respond(
        "POST",
        "/agents/jobs/31/approve",
        200,
        r#"{"approved":true}"#,
    )
    .await;

    assert_eq!(
        sb.atl(&["agent"]).await.ok(),
        "◆ 31  weekly-tidy  Atelier  first run  atl agent approve 31\n"
    );

    // Nobody to ask and no --yes: show what it wants, approve nothing.
    let unasked = sb.atl(&["agent", "approve", "31"]).await;
    assert_eq!(unasked.code, 1);
    assert_eq!(unasked.stdout, AGENT_CARD);
    assert_eq!(
        unasked.stderr,
        "There’s nobody here to ask about letting this agent run. Add --yes to go ahead without being asked.\n"
    );
    assert_eq!(sb.count("POST", "/agents/jobs/31/approve").await, 0);

    let out = sb.atl(&["agent", "approve", "31", "--yes"]).await.ok();
    assert_eq!(
        out,
        format!(
            "{AGENT_CARD}✓ Approved. weekly-tidy can run now.\n  Watch it with atl run watch\n"
        )
    );
    assert_eq!(sb.count("POST", "/agents/jobs/31/approve").await, 1);

    // An id that isn't waiting.
    let missing = sb.atl(&["agent", "approve", "99", "--yes"]).await;
    assert_eq!(missing.code, 1);
    assert_eq!(
        missing.stderr,
        "No agent with id 99 is waiting. See who is with atl agent list.\n"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn agent_approve_json() {
    let sb = Sandbox::new().await;
    sb.get("/agents/waiting", WAITING).await;
    sb.respond(
        "POST",
        "/agents/jobs/31/approve",
        200,
        r#"{"approved":true}"#,
    )
    .await;
    let waiting: serde_json::Value = serde_json::from_str(WAITING).unwrap();

    assert_eq!(sb.atl(&["agent", "list", "--json"]).await.json(), waiting);
    let out = sb
        .atl(&["agent", "approve", "31", "--yes", "--json"])
        .await
        .json();
    assert_eq!(
        out,
        json!({"job_id": 31, "approved": true, "agent": waiting["jobs"][0]})
    );

    // Even with --json, no approval without --yes; the error is JSON too.
    let unasked = sb.atl(&["agent", "approve", "31", "--json"]).await;
    assert_eq!((unasked.code, unasked.stdout.as_str()), (1, ""));
    let error: serde_json::Value = serde_json::from_str(&unasked.stderr).unwrap();
    assert_eq!(error["error"], "needs_confirmation");
    assert_eq!(sb.count("POST", "/agents/jobs/31/approve").await, 1);
}

// ------------------------------------------------------------------ remote

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn remote_text() {
    let sb = Sandbox::new().await;
    sb.get("/remote", REMOTE_ON).await;
    sb.respond("POST", "/remote", 200, REMOTE_ON).await;
    let since = (time::OffsetDateTime::now_utc() - time::Duration::seconds(150))
        .format(&time::format_description::well_known::Rfc3339)
        .unwrap();
    sb.get(
        "/remote/who",
        &format!(r#"{{"sessions":[{{"ip":"203.0.113.42","country":"NL","signed_in":true,"class":"web","since":"{since}"}}]}}"#),
    )
    .await;
    sb.respond(
        "POST",
        "/remote/link",
        201,
        r#"{"url":"https://quiet-fox-lamp.trycloudflare.com/-/passkey/abc","expires_in":900}"#,
    )
    .await;

    let on = "\
Remote access is on.
  https://quiet-fox-lamp.trycloudflare.com
  Sign in there with a passkey. Web and git only.
  Need a passkey for that address? atl remote link
";
    assert_eq!(sb.atl(&["remote", "on"]).await.ok(), on);
    assert_eq!(sb.bodies("POST", "/remote").await, [json!({"on": true})]);
    assert_eq!(
        sb.atl(&["remote"]).await.ok(),
        format!("{on}  1 session connected from outside.\n")
    );
    assert_eq!(
        sb.atl(&["remote", "who"]).await.ok(),
        "  203.0.113.42   NL   signed in   web   2 min\n"
    );
    assert_eq!(
        sb.atl(&["remote", "link"]).await.ok(),
        "https://quiet-fox-lamp.trycloudflare.com/-/passkey/abc\nWorks once, for the next 15m. Open it on the device that needs a passkey.\n"
    );

    // Off: how many were signed out comes from the state just before.
    sb.server.reset().await;
    sb.get("/remote", REMOTE_ON).await;
    sb.respond("POST", "/remote", 200, REMOTE_OFF).await;
    assert_eq!(
        sb.atl(&["remote", "off"]).await.ok(),
        "Remote access is off. 1 session signed out.\nYour studio is home only again.\n"
    );
    assert_eq!(sb.bodies("POST", "/remote").await, [json!({"on": false})]);

    sb.server.reset().await;
    sb.get("/remote", REMOTE_OFF).await;
    sb.get("/remote/who", r#"{"sessions":[]}"#).await;
    assert_eq!(
        sb.atl(&["remote", "status"]).await.ok(),
        "Remote access is off.\nYour studio is home only.\n"
    );
    assert_eq!(
        sb.atl(&["remote", "who"]).await.ok(),
        "Nobody is connected from outside.\n"
    );

    // Through the tunnel it can't be turned on.
    sb.respond("POST", "/remote", 403, r#"{"error":"remote_only_off"}"#)
        .await;
    let refused = sb.atl(&["remote", "on"]).await;
    assert_eq!(refused.code, 1);
    assert_eq!(
        refused.stderr,
        "From outside, remote access can only be turned off. Turn it on from home.\n"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn remote_json() {
    let sb = Sandbox::new().await;
    sb.get("/remote", REMOTE_ON).await;
    sb.respond("POST", "/remote", 200, REMOTE_OFF).await;
    let who = r#"{"sessions":[{"ip":"203.0.113.42","country":"NL","signed_in":true,"class":"web","since":"…"}]}"#;
    sb.get("/remote/who", who).await;

    let on: serde_json::Value = serde_json::from_str(REMOTE_ON).unwrap();
    assert_eq!(sb.atl(&["remote", "--json"]).await.json(), on);
    assert_eq!(
        sb.atl(&["remote", "who", "--json"]).await.json(),
        serde_json::from_str::<serde_json::Value>(who).unwrap()
    );
    assert_eq!(
        sb.atl(&["remote", "off", "--json"]).await.json(),
        json!({"on":false,"mode":"quick","address":null,"exposed":["web","git"],"connected":0,"signed_out":1})
    );
}

// --------------------------------------------------------------------- say

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn say_text() {
    let sb = Sandbox::new().await;
    sb.get("/messages", MESSAGES).await;
    // The studio's answer after adding: push has 4 ours+yours... plus the new one.
    let added = MESSAGES.replace(
        r#""yours":["Shipped. Go outside."]"#,
        r#""yours":["Shipped. Go outside.","Away it goes."]"#,
    );
    sb.respond("PUT", "/messages", 200, &added).await;

    assert_eq!(
        sb.atl(&["say", "list", "push"]).await.ok(),
        "  \
Home and dry. {branch} is safe in your studio.
  Off it goes. {count} commits landed on {repo}.
  Pushed. Checks are warming up on {device}.
  Shipped. Go outside.   yours
"
    );
    assert_eq!(
        sb.atl(&["say", "add", "push", "Away it goes."]).await.ok(),
        "Added to your push messages (5 now).\n"
    );
    assert_eq!(
        sb.atl(&["say", "remove", "push", "1"]).await.ok(),
        "Removed “Shipped. Go outside.” from your push messages (5 now).\n"
    );
    assert_eq!(
        sb.atl(&["say", "off"]).await.ok(),
        "Messages are off. Plain output only.\n"
    );
    assert_eq!(
        sb.atl(&["say", "tone", "off"]).await.ok(),
        "Messages are off. Plain output only.\n"
    );
    assert_eq!(
        sb.atl(&["say", "tone", "plain"]).await.ok(),
        "Messages are plain.\n"
    );
    assert_eq!(
        sb.atl(&["say", "tone", "friendly"]).await.ok(),
        "Messages are friendly.\n"
    );
    // `yours` lists replace per moment; the tone is sent alone.
    assert_eq!(
        sb.bodies("PUT", "/messages").await,
        [
            json!({"yours":{"push":["Shipped. Go outside.","Away it goes."]}}),
            json!({"yours":{"push":[]}}),
            json!({"tone":"off"}),
            json!({"tone":"off"}),
            json!({"tone":"plain"}),
            json!({"tone":"friendly"})
        ]
    );

    assert_eq!(
        sb.atl(&["say"]).await.ok(),
        "\
Messages are friendly.
  agent  2 messages
  clean  2 messages
  fail   2 messages
  first  2 messages
  late   2 messages
  merge  2 messages
  mile   1 message
  pass   2 messages
  push   3 messages · 1 yours

Add your own: atl say add push \"Shipped. Go outside.\"
"
    );

    // Refused before anything is sent.
    let before = sb.count("PUT", "/messages").await;
    let unknown = sb.atl(&["say", "list", "eclipse"]).await;
    assert_eq!(unknown.code, 1);
    assert_eq!(
        unknown.stderr,
        "There is no “eclipse” moment. The moments are: agent, clean, fail, first, late, merge, mile, pass, push.\n"
    );
    let long = sb.atl(&["say", "add", "push", &"x".repeat(141)]).await;
    assert_eq!(long.code, 1);
    assert_eq!(
        long.stderr,
        "That message is 141 characters; the most is 140.\n"
    );
    let none = sb.atl(&["say", "remove", "clean", "1"]).await;
    assert_eq!(
        none.stderr,
        "You have no messages of your own for clean. See them with atl say list clean.\n"
    );
    assert_eq!(sb.count("PUT", "/messages").await, before);
    // An unknown tone is a usage error.
    assert_eq!(sb.atl(&["say", "tone", "loud"]).await.code, 2);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn say_json() {
    let sb = Sandbox::new().await;
    sb.get("/messages", MESSAGES).await;
    sb.respond(
        "PUT",
        "/messages",
        200,
        &MESSAGES.replace(r#""tone":"friendly""#, r#""tone":"off""#),
    )
    .await;
    let messages: serde_json::Value = serde_json::from_str(MESSAGES).unwrap();

    assert_eq!(sb.atl(&["say", "--json"]).await.json(), messages);
    assert_eq!(
        sb.atl(&["say", "list", "push", "--json"]).await.json(),
        json!({"moment":"push","ours":messages["moments"]["push"]["ours"],
               "yours":["Shipped. Go outside."],"plain":"Pushed {branch}."})
    );
    assert_eq!(
        sb.atl(&["say", "off", "--json"]).await.json(),
        json!({"tone":"off"})
    );
    assert_eq!(
        sb.atl(&["say", "add", "push", "x", "--json"]).await.json(),
        json!({"moment":"push","ours":messages["moments"]["push"]["ours"],"yours":["Shipped. Go outside."]})
    );
}

// --------------------------------------------------------------- runs, repos

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn runs_and_logs() {
    let sb = Sandbox::new().await;
    sb.get("/runs", RUNS).await;
    sb.get(
        "/jobs/412/log",
        r#"{"lines":["--- FAIL: TestSSH (0.01s)","exit status 1"],"truncated":true,"status":"failure"}"#,
    )
    .await;
    sb.respond("GET", "/jobs/999/log", 404, r#"{"error":"not_found"}"#)
        .await;

    let list = "\
✓ 207  ci  push · feature/tunnel-ui  Atelier  1m 48s
✗ 208  ci  push · feature/ssh        Atelier  1m 04s
    ✓ lint  MacBook         22s
    ✗ test  Portainer host  1m 04s
";
    assert_eq!(sb.atl(&["run"]).await.ok(), list);
    assert_eq!(sb.atl(&["run", "list", "--limit", "5"]).await.ok(), list);
    assert_eq!(
        sb.atl(&["run", "--json"]).await.json(),
        serde_json::from_str::<serde_json::Value>(RUNS).unwrap()
    );

    // `atl run log 208` (what `atl status` suggests): the failed job's log.
    let log = "\
✗ ci · test  job 412 · failure
  … earlier lines left out (--tail N for more than 200)
--- FAIL: TestSSH (0.01s)
exit status 1
";
    assert_eq!(sb.atl(&["run", "log", "208"]).await.ok(), log);
    // A job id works too, directly.
    assert_eq!(
        sb.atl(&["run", "log", "412", "--job", "--json"])
            .await
            .json(),
        json!({"run_id": null, "jobs":[{"job_id":412,"status":"failure","truncated":true,
               "lines":["--- FAIL: TestSSH (0.01s)","exit status 1"]}]})
    );
    let missing = sb.atl(&["run", "log", "999"]).await;
    assert_eq!(missing.code, 1);
    assert_eq!(
        missing.stderr,
        "There is no run or job with id 999. See recent ones with atl run list.\n"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn run_watch_follows_live_events() {
    let sb = Sandbox::new().await;
    sb.get("/status", STATUS).await;
    use wiremock::matchers::{method, path};
    wiremock::Mock::given(method("GET"))
        .and(path("/api/app/v1/events"))
        .respond_with(wiremock::ResponseTemplate::new(200).set_body_raw(
            "event: status\ndata: {\"changed\":[\"feed\"]}\n\n\
             event: notify\ndata: {\"id\":\"n:1\",\"kind\":\"run_failed\",\"thread\":\"yours\",\"title\":\"ci failed on feature/ssh\",\"detail\":\"Atelier · test\",\"repo\":\"DemiInfinity/Atelier\",\"moment\":\"fail\"}\n\n\
             event: status\ndata: {\"changed\":[\"needs\",\"running\"]}\n\n",
            "text/event-stream",
        ))
        .mount(&sb.server)
        .await;
    let running = "◆ review-helper  Atelier · pull request 14 · turn 3 of 20 · MacBook\n";
    assert_eq!(
        sb.atl(&["run", "watch", "--events", "2"]).await.ok(),
        format!("{running}✗ ci failed on feature/ssh  Atelier  Atelier · test\n\n{running}")
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn repo_list_and_clone() {
    let sb = Sandbox::new().await;
    // A real repository to clone from, standing in for the studio's SSH address.
    let source = sb.root().join("studio-Marbl");
    sb.repo(&source);
    let repos = REPOS.replace(
        "ssh://git@studio.example:2223/DemiInfinity/Marbl.git",
        source.to_str().unwrap(),
    );
    sb.get("/repos", &repos).await;

    assert_eq!(
        sb.atl(&["repo", "list"]).await.ok(),
        "\
DemiInfinity/Atelier  private  Go     1 pull request  on GitHub too
DemiInfinity/Marbl    public   Swift
"
    );
    assert_eq!(
        sb.atl(&["repo", "list", "--json"]).await.json(),
        serde_json::from_str::<serde_json::Value>(&repos).unwrap()
    );

    // By bare name, any capitals, into <clone_root>/<owner>/<name>.
    let dest = sb.home().join("Atelier/DemiInfinity/Marbl");
    assert_eq!(
        sb.atl(&["repo", "clone", "marbl"]).await.ok(),
        format!("Cloning DemiInfinity/Marbl… done\n✓ {}\n", dest.display())
    );
    assert!(dest.join("README.md").exists());
    let again = sb.atl(&["repo", "clone", "DemiInfinity/Marbl"]).await;
    assert_eq!(again.code, 1);
    assert_eq!(
        again.stderr,
        format!(
            "{} is already there. Open it with atl open DemiInfinity/Marbl.\n",
            dest.display()
        )
    );
    let unknown = sb.atl(&["repo", "clone", "nope"]).await;
    assert_eq!(
        unknown.stderr,
        "There is no repository called nope in your studio. See them all with atl repo list.\n"
    );
    assert_eq!(sb.atl(&["repo", "clone"]).await.code, 2);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn repo_clone_from_github_goes_through_the_studio() {
    let sb = Sandbox::new().await;
    let source = sb.root().join("studio-loopkit");
    sb.repo(&source);
    sb.respond(
        "POST",
        "/repos/mirror",
        202,
        r#"{"full_name":"DemiInfinity/loopkit"}"#,
    )
    .await;
    sb.get(
        "/repos",
        &json!({"repos":[{"full_name":"DemiInfinity/loopkit","owner":"DemiInfinity","name":"loopkit",
                          "clone_https":"https://studio.example/DemiInfinity/loopkit.git","clone_ssh": source}]})
            .to_string(),
    )
    .await;
    let dest = sb.work().join("lk");
    let out = sb
        .atl(&[
            "repo",
            "clone",
            "--github",
            "maralind/loopkit",
            "--path",
            dest.to_str().unwrap(),
            "--json",
        ])
        .await
        .json();
    assert_eq!(
        out,
        json!({"repo":"DemiInfinity/loopkit","path": dest,"url": source})
    );
    assert_eq!(
        sb.bodies("POST", "/repos/mirror").await,
        [json!({"github":"maralind/loopkit"})]
    );
    assert!(dest.join("README.md").exists());
}

// ----------------------------------------------------------------- publish

fn created(clone_ssh: &std::path::Path) -> String {
    json!({"full_name":"DemiInfinity/Ingle","owner":"DemiInfinity","owner_kind":"user","name":"Ingle",
           "private":true,"description":"","default_branch":"main",
           "clone_https":"https://studio.example/DemiInfinity/Ingle.git",
           "clone_ssh": clone_ssh,"github":null,"open_pulls":0,"updated_at":"2026-11-11T09:00:00Z"})
    .to_string()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn repo_publish() {
    let sb = Sandbox::new().await;
    sb.get("/me", ME).await;
    sb.get("/repos", REPOS).await;
    sb.get("/messages", MESSAGES).await;
    let studio_side = sb.root().join("studio-Ingle.git");
    std::fs::create_dir_all(&studio_side).unwrap();
    sb.git(&studio_side, &["init", "--quiet", "--bare"]);
    sb.respond("POST", "/repos", 201, &created(&studio_side))
        .await;

    let ingle = sb.work().join("Ingle");
    sb.repo(&ingle);
    sb.commit(&ingle, "hearth.txt");
    std::fs::write(ingle.join("notes.txt"), "not committed").unwrap();

    // Nobody to ask: say so, create nothing.
    let unasked = sb.atl_in(&ingle, &["repo", "publish"]).await;
    assert_eq!(unasked.code, 1);
    assert_eq!(
        unasked.stderr,
        "There’s nobody here to ask about publishing this folder. Add --yes to go ahead without being asked.\n"
    );
    assert_eq!(sb.count("POST", "/repos").await, 0);

    let out = sb.atl_in(&ingle, &["repo", "publish", "--yes"]).await.ok();
    assert_eq!(
        out,
        format!(
            "\
This folder isn’t in your studio yet.

  Account  › DemiInfinity (personal)
             Elyndra Studios LTD (company)
  Name     Ingle
  Private  yes

Creating DemiInfinity/Ingle… done
Pushing main (2 commits)… done

✓ {}/DemiInfinity/Ingle
  origin now points at your studio. Your folder stayed where it is.
atelier: Home and dry. main is safe in your studio.
",
            sb.server.uri()
        )
    );
    assert_eq!(
        sb.bodies("POST", "/repos").await,
        [json!({"owner":"DemiInfinity","name":"Ingle","private":true,"description":""})]
    );
    // Pushed, tracking, and the folder as it was.
    assert_eq!(
        sb.git(&studio_side, &["rev-parse", "refs/heads/main"]),
        sb.git(&ingle, &["rev-parse", "HEAD"])
    );
    assert_eq!(
        sb.git(&ingle, &["rev-parse", "--abbrev-ref", "@{upstream}"]),
        "origin/main"
    );
    assert_eq!(
        std::fs::read_to_string(ingle.join("notes.txt")).unwrap(),
        "not committed"
    );
    assert_eq!(sb.git(&ingle, &["status", "--porcelain"]), "?? notes.txt");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn repo_publish_flags_json_and_refusals() {
    let sb = Sandbox::new().await;
    sb.get("/me", ME).await;
    sb.get("/repos", REPOS).await;
    sb.get(
        "/messages",
        &MESSAGES.replace(r#""tone":"friendly""#, r#""tone":"off""#),
    )
    .await;
    let studio_side = sb.root().join("studio-Hearth.git");
    std::fs::create_dir_all(&studio_side).unwrap();
    sb.git(&studio_side, &["init", "--quiet", "--bare"]);
    sb.respond("POST", "/repos", 201, &created(&studio_side))
        .await;

    // Not a repository; nothing committed.
    let plain = sb.atl(&["repo", "publish", "--yes"]).await;
    assert_eq!(plain.code, 1);
    assert_eq!(
        plain.stderr,
        "This folder isn’t a git repository. Run git init here first.\n"
    );

    let ingle = sb.work().join("Ingle");
    sb.repo(&ingle);

    // An account that isn't yours.
    let wrong = sb
        .atl_in(&ingle, &["repo", "publish", "--yes", "--owner", "nobody"])
        .await;
    assert_eq!(
        wrong.stderr,
        "You have no account called nobody. Yours are: DemiInfinity, elyndrastudios.\n"
    );

    // origin points elsewhere: not replaced without being told to.
    sb.git(
        &ingle,
        &[
            "remote",
            "add",
            "origin",
            "https://github.com/someone/Ingle.git",
        ],
    );
    let kept = sb.atl_in(&ingle, &["repo", "publish", "--yes"]).await;
    assert_eq!(kept.code, 1);
    assert_eq!(
        kept.stderr,
        "origin already points at https://github.com/someone/Ingle.git. Add --replace-origin to point it at your studio.\n"
    );
    assert_eq!(sb.count("POST", "/repos").await, 0);
    assert_eq!(
        sb.git(&ingle, &["remote", "get-url", "origin"]),
        "https://github.com/someone/Ingle.git"
    );

    // Scripted: every choice as a flag, the answer as JSON, tone off = no message.
    let out = sb
        .atl_in(
            &ingle,
            &[
                "repo",
                "publish",
                "--yes",
                "--owner",
                "elyndrastudios",
                "--name",
                "Hearth",
                "--public",
                "--description",
                "A warm place",
                "--replace-origin",
                "--json",
            ],
        )
        .await
        .json();
    assert_eq!(
        sb.bodies("POST", "/repos").await,
        [
            json!({"owner":"elyndrastudios","name":"Hearth","private":false,"description":"A warm place"})
        ]
    );
    assert_eq!(out["repo"]["full_name"], "DemiInfinity/Ingle");
    assert_eq!(
        out["url"],
        format!("{}/DemiInfinity/Ingle", sb.server.uri())
    );
    assert_eq!(out["path"], json!(ingle));
    assert_eq!(
        (&out["branch"], &out["commits"]),
        (&json!("main"), &json!(1))
    );
    assert_eq!(out["origin"], json!(studio_side));
    assert_eq!(
        out["previous_origin"],
        "https://github.com/someone/Ingle.git"
    );
    assert_eq!(out["message"], json!(null));

    // Already in the studio: nothing to publish.
    let atelier = sb.work().join("Atelier");
    sb.repo(&atelier);
    sb.git(
        &atelier,
        &[
            "remote",
            "add",
            "origin",
            "git@studio.example:DemiInfinity/Atelier.git",
        ],
    );
    let already = sb.atl_in(&atelier, &["repo", "publish", "--yes"]).await;
    assert_eq!(
        already.stderr,
        "This folder is already in your studio as DemiInfinity/Atelier. Use git push.\n"
    );

    // The name is taken.
    sb.server.reset().await;
    sb.get("/me", ME).await;
    sb.get("/repos", REPOS).await;
    sb.respond("POST", "/repos", 409, r#"{"error":"name_taken"}"#)
        .await;
    let other = sb.work().join("Other");
    sb.repo(&other);
    let taken = sb.atl_in(&other, &["repo", "publish", "--yes"]).await;
    assert_eq!(taken.code, 1);
    assert_eq!(
        taken.stdout,
        "\
This folder isn’t in your studio yet.

  Account  › DemiInfinity (personal)
             Elyndra Studios LTD (company)
  Name     Other
  Private  yes

Creating DemiInfinity/Other… failed
"
    );
    assert_eq!(
        taken.stderr,
        "That name is already taken. Pick another with --name.\n"
    );
}

// ------------------------------------------------------------------- login

const GRANT: &str = r#"{"token":"atd_0123456789abcdefghijABCDEFGHIJ0123456789","device_id":7,"person":{"name":"Demi","login":"DemiInfinity"},"studio":{"name":"Atelier","url":"https://studio.example","version":"7f82d31"}}"#;

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn login_whoami_logout() {
    let sb = Sandbox::new().await;
    sb.respond(
        "POST",
        "/device/start",
        200,
        r#"{"device_code": "c0ffee", "user_code": "AMBER-4127",
            "verify_url": "https://studio.example/settings/devices/approve?code=AMBER-4127",
            "expires_in": 600, "interval": 1}"#,
    )
    .await;
    sb.respond("POST", "/device/poll", 200, GRANT).await;
    sb.respond(
        "POST",
        "/device/ssh-key",
        201,
        r#"{"id":12,"fingerprint":"SHA256:abc"}"#,
    )
    .await;
    sb.get("/me", ME).await;
    let studio = sb.server.uri();
    let token_file = sb.config_dir().join("tokens.json");

    // Not signed in yet: exit code 3 and the next step.
    let before = sb.atl_bare(&["whoami"]).await;
    assert_eq!(before.code, 3);
    assert_eq!(
        before.stderr,
        "You’re not signed in. Run atl login <studio>.\n"
    );

    let ran = sb.atl_bare(&["login", &studio, "--name", "MacBook"]).await;
    let (stdout, stderr) = (ran.stdout.clone(), ran.stderr.clone());
    assert_eq!(
        ran.ok(),
        format!(
            "\
Found Atelier at {studio}
Open this in your browser to approve {machine}:
  https://studio.example/settings/devices/approve?code=AMBER-4127
  Approve with your passkey. Check the code matches: AMBER-4127

✓ Approved as Demi
✓ Added {machine}’s SSH key to your studio
✓ Saved a device token in {file}, a file only you can read (no keychain in use)

You’re in. Try atl status.
Your password was never typed here.
",
            machine = this_machine(),
            file = token_file.display()
        )
    );
    // Neither the token nor the device code is ever printed.
    for secret in [TOKEN, "c0ffee"] {
        assert!(!stdout.contains(secret) && !stderr.contains(secret));
    }

    // What was sent.
    let start = &sb.bodies("POST", "/device/start").await[0];
    assert_eq!(start["name"], "MacBook");
    assert_eq!(start["kind"], "cli");
    assert_eq!(
        sb.bodies("POST", "/device/poll").await,
        [json!({"device_code":"c0ffee"})]
    );
    let key = &sb.bodies("POST", "/device/ssh-key").await[0];
    assert_eq!(key["title"], "MacBook (atl)");
    let public =
        std::fs::read_to_string(sb.home().join(".ssh/atelier_device_ed25519.pub")).unwrap();
    assert_eq!(key["key"], public.trim());

    // What was kept: the config (no token in it) and the private token file.
    let config = std::fs::read_to_string(sb.config_dir().join("config.toml")).unwrap();
    assert_eq!(
        config,
        format!("studio = \"{studio}\"\ndevice_id = 7\ndevice_name = \"MacBook\"\n")
    );
    let stored: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(&token_file).unwrap()).unwrap();
    assert_eq!(stored, json!({ studio.clone(): TOKEN }));
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let mode = std::fs::metadata(&token_file).unwrap().permissions().mode();
        assert_eq!(mode & 0o777, 0o600);
    }

    // Signed in from disk alone, sending the stored token.
    assert_eq!(
        sb.atl_bare(&["whoami"]).await.ok(),
        format!(
            "\
Demi (DemiInfinity) · Atelier at {studio}
  Device    MacBook
  Accounts  DemiInfinity (personal)
            Elyndra Studios LTD (company)
  Version   7f82d31
"
        )
    );
    let requests = sb.server.received_requests().await.unwrap();
    let me = requests
        .iter()
        .find(|r| r.url.path().ends_with("/me"))
        .unwrap();
    assert_eq!(
        me.headers.get("authorization").unwrap(),
        &format!("Bearer {TOKEN}")
    );
    assert_eq!(
        sb.atl_bare(&["whoami", "--json"]).await.json(),
        serde_json::from_str::<serde_json::Value>(ME).unwrap()
    );
    // The overview knows the studio now.
    let host = studio.trim_start_matches("http://");
    assert!(sb
        .atl_bare(&[])
        .await
        .ok()
        .starts_with(&format!("Atelier · your studio at {host}\n\n  atl status ")));

    assert_eq!(
        sb.atl_bare(&["logout"]).await.ok(),
        format!(
            "Signed out of {host} on {}.\nTo cut the device off for good, revoke it in Settings → Devices.\n",
            this_machine()
        )
    );
    assert!(!token_file.exists());
    let after = sb.atl_bare(&["whoami"]).await;
    assert_eq!(after.code, 3);
    assert_eq!(
        after.stderr,
        format!("You’re not signed in. Run atl login {host}.\n")
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn login_json_denied_and_revoked() {
    let sb = Sandbox::new().await;
    let start = r#"{"device_code":"c0ffee","user_code":"AMBER-4127","verify_url":"https://studio.example/approve","expires_in":600,"interval":1}"#;
    sb.respond("POST", "/device/start", 200, start).await;
    sb.respond("POST", "/device/poll", 200, GRANT).await;
    sb.respond(
        "POST",
        "/device/ssh-key",
        200,
        r#"{"id":12,"fingerprint":"SHA256:abc"}"#,
    )
    .await;
    let studio = sb.server.uri();

    // --json: the result on stdout, the conversation on stderr.
    let ran = sb
        .atl_bare(&["login", &studio, "--name", "MacBook", "--json"])
        .await;
    assert_eq!(ran.code, 0);
    assert!(ran.stderr.contains("AMBER-4127"));
    assert!(!ran.stdout.contains(TOKEN) && !ran.stderr.contains(TOKEN));
    let out: serde_json::Value = serde_json::from_str(&ran.stdout).unwrap();
    assert_eq!(out["studio"], studio);
    assert_eq!(out["person"]["login"], "DemiInfinity");
    assert_eq!(out["device"], json!({"id":7,"name":"MacBook","kind":"cli"}));
    assert_eq!(out["token_stored"], "file");
    assert_eq!(out["ssh_key"]["id"], 12);
    assert_eq!(out["ssh_key"]["generated"], true);

    // Denied in the browser.
    sb.server.reset().await;
    sb.respond("POST", "/device/start", 200, start).await;
    sb.respond("POST", "/device/poll", 410, r#"{"error":"denied"}"#)
        .await;
    let denied = sb.atl_bare(&["login", &studio, "--name", "MacBook"]).await;
    assert_eq!(denied.code, 1);
    let host = studio.trim_start_matches("http://");
    assert_eq!(
        denied.stderr,
        format!("The sign-in was denied in the browser. Run atl login {host} to try again.\n")
    );

    // A revoked device: exit 3, sign in again.
    sb.respond("GET", "/status", 401, r#"{"error":"revoked"}"#)
        .await;
    let revoked = sb.atl(&["status"]).await;
    assert_eq!(revoked.code, 3);
    assert_eq!(
        revoked.stderr,
        format!("This device was signed out of your studio. Run atl login {host} again.\n")
    );
    let revoked = sb.atl(&["status", "--json"]).await;
    assert_eq!(revoked.code, 3);
    assert_eq!(
        serde_json::from_str::<serde_json::Value>(&revoked.stderr).unwrap(),
        json!({"error":"revoked","message":"This device was signed out of your studio.",
               "hint": format!("Run atl login {host} again.")})
    );
}

// ------------------------------------------------- exit codes, help, runner

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn exit_codes_and_plain_errors() {
    let sb = Sandbox::new().await;
    // 2: usage.
    for args in [
        &["frobnicate"][..],
        &["pr", "merge"],
        &["status", "--bogus"],
        &["login"],
    ] {
        let ran = sb.atl(args).await;
        assert_eq!(ran.code, 2, "{args:?}");
        assert!(ran.stderr.contains("Usage: atl"), "{}", ran.stderr);
    }
    // 3: not signed in, for every command that needs the studio.
    for args in [
        &["status"][..],
        &["repo", "list"],
        &["pr"],
        &["run"],
        &["agent"],
        &["remote"],
        &["say"],
    ] {
        let ran = sb.atl_bare(args).await;
        assert_eq!(ran.code, 3, "{args:?}");
        assert_eq!(
            ran.stderr,
            "You’re not signed in. Run atl login <studio>.\n"
        );
        assert_eq!(ran.stdout, "");
    }
    // 1: the studio can't be reached (nothing mounted answers 404; a dead port is a network error).
    let unknown = sb.atl(&["status"]).await;
    assert_eq!(unknown.code, 1);
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let dead = format!("http://{}", listener.local_addr().unwrap());
    drop(listener);
    let mut command = assert_cmd::Command::cargo_bin("atl").unwrap();
    let output = command
        .env_clear()
        .env("ATELIER_CONFIG_DIR", sb.config_dir())
        .env("HOME", sb.home())
        .env("ATELIER_TOKEN_STORE", "file")
        .env("ATELIER_STUDIO", &dead)
        .env("ATELIER_TOKEN", TOKEN)
        .current_dir(sb.work())
        .args(["remote"])
        .output()
        .unwrap();
    assert_eq!(output.status.code(), Some(1));
    let stderr = String::from_utf8(output.stderr).unwrap();
    assert!(
        stderr.starts_with("Couldn’t reach your studio: "),
        "{stderr}"
    );
    assert!(
        stderr.ends_with("Check that you’re on your home network, or that remote access is on.\n")
    );
    assert!(!stderr.contains(TOKEN));
    assert_eq!(stderr.lines().count(), 1);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn overview_help_and_completions() {
    let sb = Sandbox::new().await;
    assert_eq!(
        sb.atl_bare(&[]).await.ok(),
        "\
Atelier · not signed in yet. Start with atl login <studio>.

  atl status            what needs you, across your studio
  atl repo publish      turn this folder into a repository
  atl repo clone NAME   clone from your studio or GitHub
  atl pr                pull requests for this repository
  atl issue             issues of this repository
  atl run               jobs and agents, live
  atl agent approve     let a waiting agent run
  atl open              open this repository in your editor
  atl remote on|off     reach your studio from outside
  atl say               your pop-up messages

Run atl <command> --help for details. Everything also takes --json.
"
    );
    let help = sb.atl_bare(&["--help"]).await.ok();
    for command in [
        "login",
        "logout",
        "whoami",
        "status",
        "repo",
        "pr",
        "run",
        "agent",
        "open",
        "remote",
        "say",
        "runner",
        "completions",
        "--json",
    ] {
        assert!(help.contains(command), "{command} missing from --help");
    }
    for shell in ["bash", "zsh", "fish", "powershell", "elvish"] {
        let script = sb.atl_bare(&["completions", shell]).await.ok();
        assert!(script.contains("atl"), "{shell}");
        assert!(script.contains("when-green"), "{shell}");
    }
    assert!(sb.atl_bare(&["--version"]).await.ok().starts_with("atl "));
}

#[cfg(unix)]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn runner_through_its_socket() {
    use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};

    let sb = Sandbox::new().await;
    // Not running is an answer, not an error.
    assert_eq!(
        sb.atl_bare(&["runner"]).await.ok(),
        format!(
            "The runner isn’t running on {}.\nStart it with atelier-runner run\n",
            this_machine()
        )
    );
    assert_eq!(
        sb.atl_bare(&["runner", "status", "--json"]).await.json(),
        json!({"ok": false, "error": "not running"})
    );
    // But pausing something that isn't there is.
    let pause = sb.atl_bare(&["runner", "pause"]).await;
    assert_eq!(pause.code, 1);
    assert_eq!(
        pause.stderr,
        "The runner isn’t running on this machine. Start it with atelier-runner run.\n"
    );

    // A fake runner: answers status/set as the contract says.
    let status = |taking: bool, reason: &str| {
        format!(
            r#"{{"ok":true,"name":"MacBook","version":"7f82d31","server":"https://studio.example","online":true,"taking_jobs":{taking},"paused_reason":{reason},"capabilities":["jobs","agents","claude-code","workspaces"],"rules":{{"pause_on_battery":true,"max_parallel":1,"container_jobs":false}},"current":[{{"job_id":412,"repo":"DemiInfinity/Atelier","workflow":"review-helper","kind":"agent","started_at":"2026-11-11T09:14:02Z","detail":"turn 3 of 20"}}]}}"#
        )
    };
    let listener = tokio::net::UnixListener::bind(sb.root().join("control.sock")).unwrap();
    let asked = std::sync::Arc::new(std::sync::Mutex::new(Vec::new()));
    let seen = asked.clone();
    let answers = [
        status(true, "null"),
        status(true, "null"),
        status(false, "\"user\""),
        status(false, "\"battery\""),
        status(true, "null"),
    ];
    tokio::spawn(async move {
        for answer in answers {
            let (stream, _) = listener.accept().await.unwrap();
            let mut stream = BufReader::new(stream);
            let mut line = String::new();
            stream.read_line(&mut line).await.unwrap();
            seen.lock()
                .unwrap()
                .push(serde_json::from_str::<serde_json::Value>(&line).unwrap());
            stream
                .get_mut()
                .write_all(format!("{answer}\n").as_bytes())
                .await
                .unwrap();
        }
    });

    assert_eq!(
        sb.atl_bare(&["runner", "status"]).await.ok(),
        "\
MacBook · online · taking jobs
  Studio   https://studio.example
  Rules    pauses on battery · 1 at a time · no container jobs
  Running  ◆ review-helper  Atelier · turn 3 of 20
"
    );
    let out = sb.atl_bare(&["runner", "--json"]).await.json();
    assert_eq!(out["ok"], true);
    assert_eq!(out["name"], "MacBook");
    assert_eq!(out["paused_reason"], json!(null));
    assert_eq!(out["current"][0]["job_id"], 412);

    assert_eq!(
        sb.atl_bare(&["runner", "pause"]).await.ok(),
        "Paused. MacBook won’t take new jobs.\n  1 job already running will finish.\n"
    );
    // Resumed by you, still held back by the battery rule.
    assert_eq!(
        sb.atl_bare(&["runner", "resume"]).await.ok(),
        "Resumed, but MacBook still isn’t taking jobs: paused while on battery.\n"
    );
    assert_eq!(
        sb.atl_bare(&["runner", "resume"]).await.ok(),
        "MacBook is taking jobs again.\n"
    );
    assert_eq!(
        *asked.lock().unwrap(),
        [
            json!({"op":"status"}),
            json!({"op":"status"}),
            json!({"op":"set","taking_jobs":false}),
            json!({"op":"set","taking_jobs":true}),
            json!({"op":"set","taking_jobs":true})
        ]
    );
}

// -------------------------------------------------------------------- open

#[cfg(unix)]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn open_uses_the_configured_editor() {
    use std::os::unix::fs::PermissionsExt;

    let sb = Sandbox::new().await;
    sb.get("/repos", REPOS).await;
    let atelier = working_copies(&sb);
    // A stand-in editor that notes what it was asked to open.
    let editor = sb.root().join("fake-editor");
    let opened = sb.root().join("opened.txt");
    std::fs::write(
        &editor,
        format!("#!/bin/sh\necho \"$1\" >> '{}'\n", opened.display()),
    )
    .unwrap();
    std::fs::set_permissions(&editor, std::fs::Permissions::from_mode(0o755)).unwrap();
    std::fs::write(
        sb.config_dir().join("config.toml"),
        format!("editor = \"{}\"\n", editor.display()),
    )
    .unwrap();
    let marbl = sb.home().join("Atelier/DemiInfinity/Marbl");

    // Here, from a subfolder; and another repository by name.
    std::fs::create_dir_all(atelier.join("internal/web")).unwrap();
    assert_eq!(
        sb.atl_in(&atelier.join("internal/web"), &["open"])
            .await
            .ok(),
        format!("Opened {} in {}.\n", atelier.display(), editor.display())
    );
    assert_eq!(
        sb.atl(&["open", "marbl", "--json"]).await.json(),
        json!({"path": marbl, "editor": editor})
    );
    assert_eq!(
        std::fs::read_to_string(&opened).unwrap(),
        format!("{}\n{}\n", atelier.display(), marbl.display())
    );

    let nowhere = sb.atl(&["open"]).await;
    assert_eq!(nowhere.code, 1);
    assert_eq!(
        nowhere.stderr,
        "You’re not inside a repository. Go to one, or say which: atl open NAME.\n"
    );
    std::fs::remove_dir_all(&marbl).unwrap();
    let missing = sb.atl(&["open", "Marbl"]).await;
    assert_eq!(
        missing.stderr,
        "DemiInfinity/Marbl isn’t cloned on this machine yet. Run atl repo clone DemiInfinity/Marbl.\n"
    );
}

// ------------------------------------------------ the server as it was built

/// The studio doesn't know agent turns ("pull request 14"); the runner on
/// this machine does. And a need may only have a page to open.
#[cfg(unix)]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn status_asks_the_runner_for_turns_and_links_open_web_needs() {
    use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};

    let sb = Sandbox::new().await;
    sb.get("/repos", r#"{"repos":[]}"#).await;
    sb.get(
        "/status",
        r#"{"needs":[{"id":"security:DemiInfinity/Tray:deps","kind":"security_alert","thread":"yours","title":"1 advisory in Tray",
                      "detail":"okhttp","repo":"DemiInfinity/Tray","web_path":"/DemiInfinity/Tray/security","at":"2026-11-11T09:00:00Z",
                      "action":{"label":"Open","type":"open_web"}}],
            "running":[{"job_id":412,"run_id":209,"repo":"DemiInfinity/Atelier","workflow":"review-helper","job":"agent","kind":"agent",
                        "engine":"claude-code","runner":"MacBook","started_at":"2026-11-11T09:14:02Z","detail":"pull request 14",
                        "web_path":"/DemiInfinity/Atelier/actions/runs/9"}],
            "feed":[],"unread":0}"#,
    )
    .await;
    let listener = tokio::net::UnixListener::bind(sb.root().join("control.sock")).unwrap();
    tokio::spawn(async move {
        loop {
            let (stream, _) = listener.accept().await.unwrap();
            let mut stream = BufReader::new(stream);
            let mut line = String::new();
            stream.read_line(&mut line).await.unwrap();
            let answer = r#"{"ok":true,"name":"MacBook","online":true,"taking_jobs":true,"paused_reason":null,"current":[{"job_id":412,"repo":"DemiInfinity/Atelier","workflow":"review-helper","kind":"agent","detail":"turn 3 of 20"}]}"#;
            stream
                .get_mut()
                .write_all(format!("{answer}\n").as_bytes())
                .await
                .unwrap();
        }
    });

    assert_eq!(
        sb.atl(&["status"]).await.ok(),
        format!(
            "\
Needs you
  ● 1 advisory in Tray  Tray  okhttp  {}/DemiInfinity/Tray/security

Running
  ◆ review-helper  Atelier  pull request 14 · turn 3 of 20 · MacBook
",
            sb.server.uri()
        )
    );
    let out = sb.atl(&["status", "--json"]).await.json();
    assert_eq!(
        out["running"][0]["detail"],
        "pull request 14 · turn 3 of 20"
    );
    assert_eq!(out["needs"][0]["at"], "2026-11-11T09:00:00Z");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn pr_merge_as_built() {
    let sb = Sandbox::new().await;
    let route = "/repos/DemiInfinity/Atelier/pulls/14/merge";
    sb.respond(
        "POST",
        route,
        202,
        r#"{"auto_merge":{"method":"rebase","by":"device:MacBook"}}"#,
    )
    .await;
    sb.respond("DELETE", route, 404, r#"{"error":"no_auto_merge"}"#)
        .await;
    let scope = ["-R", "DemiInfinity/Atelier"];

    assert_eq!(
        sb.atl(
            &[
                &[
                    "pr",
                    "merge",
                    "14",
                    "--when-green",
                    "--method",
                    "rebase",
                    "--delete-branch"
                ],
                &scope[..]
            ]
            .concat()
        )
        .await
        .ok(),
        "Will rebase and merge #14 when every check passes.\nYou’ll get a message when it lands.\n"
    );
    assert_eq!(
        sb.bodies("POST", route).await,
        [json!({"method":"rebase","when_green":true,"delete_branch":true})]
    );
    let none = sb
        .atl(&[&["pr", "merge", "14", "--cancel"], &scope[..]].concat())
        .await;
    assert_eq!(none.code, 1);
    assert_eq!(none.stderr, "That pull request wasn’t waiting to merge.\n");
}

// ------------------------------------------------- repo rename and delete

const RENAMED: &str = r##"{"full_name":"DemiInfinity/Studio","owner":"DemiInfinity","owner_kind":"user","name":"Studio",
  "private":true,"default_branch":"main",
  "clone_https":"https://studio.example/DemiInfinity/Studio.git",
  "clone_ssh":"ssh://git@studio.example:2223/DemiInfinity/Studio.git","github":null,"open_pulls":1}"##;

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn repo_rename_moves_the_remote_along() {
    let sb = Sandbox::new().await;
    sb.get("/repos", REPOS).await;
    sb.respond("PATCH", "/repos/DemiInfinity/Atelier", 200, RENAMED)
        .await;
    sb.respond(
        "PATCH",
        "/repos/DemiInfinity/Marbl",
        409,
        r#"{"error":"name_taken","message":"There is already a repository called Atelier."}"#,
    )
    .await;
    let atelier = working_copies(&sb);

    assert_eq!(
        sb.atl_in(&atelier, &["repo", "rename", "Studio"]).await.ok(),
        "Renamed DemiInfinity/Atelier to DemiInfinity/Studio.\nThis folder’s remote origin points at the new address.\n"
    );
    assert_eq!(
        sb.bodies("PATCH", "/repos/DemiInfinity/Atelier").await,
        [json!({"name": "Studio"})]
    );
    assert_eq!(
        sb.git(&atelier, &["remote", "get-url", "origin"]),
        "ssh://git@studio.example:2223/DemiInfinity/Studio.git"
    );

    // Named with -R from elsewhere: nothing here to move. A taken name says so.
    let taken = sb
        .atl(&["repo", "rename", "Atelier", "-R", "DemiInfinity/Marbl"])
        .await;
    assert_eq!(taken.code, 1);
    assert_eq!(
        taken.stderr,
        "There is already a repository called Atelier. Pick another with --name.\n"
    );
    // Not a name: the studio isn't asked.
    let bad = sb.atl_in(&atelier, &["repo", "rename", "two words"]).await;
    assert_eq!(bad.code, 1);
    assert!(bad.stderr.contains("isn’t a name a repository can have"));
    assert_eq!(sb.count("PATCH", "/repos/DemiInfinity/Atelier").await, 1);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn repo_delete_wants_the_name_and_leaves_the_folder() {
    let sb = Sandbox::new().await;
    sb.get("/repos", REPOS).await;
    sb.respond("DELETE", "/repos/DemiInfinity/Atelier", 204, "")
        .await;
    sb.respond(
        "DELETE",
        "/repos/DemiInfinity/Marbl",
        403,
        r#"{"error":"device_not_allowed","message":"This device isn't allowed to rename or delete repositories. Turn it on in Settings → Devices."}"#,
    )
    .await;
    let atelier = working_copies(&sb);

    // The wrong name typed, nothing typed, the wrong --yes: nothing is deleted.
    let wrong = sb
        .atl_typing(&atelier, &["repo", "delete"], "atelier\n")
        .await;
    assert_eq!(wrong.code, 1);
    assert!(wrong
        .stdout
        .contains("This deletes DemiInfinity/Atelier from your studio for good"));
    assert!(wrong.stdout.contains("is kept exactly as it is."));
    assert!(wrong.stdout.contains("Type Atelier to delete it: "));
    assert_eq!(
        wrong.stderr,
        "Nothing was deleted. Type the name exactly, or pass --yes DemiInfinity/Atelier.\n"
    );
    assert_eq!(
        sb.atl_typing(&atelier, &["repo", "delete"], "").await.code,
        1
    );
    let flag = sb
        .atl_in(&atelier, &["repo", "delete", "--yes", "Atelier"])
        .await;
    assert_eq!(flag.code, 1);
    assert_eq!(
        flag.stderr,
        "“Atelier” isn’t this repository’s full name. To delete it, pass --yes DemiInfinity/Atelier.\n"
    );
    let json = sb.atl_in(&atelier, &["repo", "delete", "--json"]).await;
    assert_eq!(json.code, 1);
    assert!(json.stderr.contains("needs_confirmation"));
    assert_eq!(sb.count("DELETE", "/repos/DemiInfinity/Atelier").await, 0);

    // A device that isn't allowed is told where to allow it.
    let refused = sb
        .atl(&[
            "repo",
            "delete",
            "-R",
            "DemiInfinity/Marbl",
            "--yes",
            "DemiInfinity/Marbl",
        ])
        .await;
    assert_eq!(refused.code, 1);
    assert!(refused.stderr.starts_with(
        "This device isn't allowed to rename or delete repositories. Turn it on in Settings → Devices. Allow it for this device at "
    ));
    assert!(refused.stderr.contains("/settings/devices"));

    // Typed right: gone from the studio, everything here as it was.
    let done = sb
        .atl_typing(&atelier, &["repo", "delete"], "Atelier\n")
        .await;
    assert_eq!(done.code, 0, "{}", done.stderr);
    assert!(done.stdout.ends_with(
        "Deleted DemiInfinity/Atelier from your studio.\nAny folder with it on this machine is still there.\n"
    ));
    assert_eq!(
        sb.bodies("DELETE", "/repos/DemiInfinity/Atelier").await,
        [json!({"confirm": "DemiInfinity/Atelier"})]
    );
    assert!(atelier.join("tray.go").is_file());
    assert_eq!(
        sb.git(&atelier, &["remote", "get-url", "origin"]),
        "ssh://git@studio.example:2223/DemiInfinity/Atelier.git"
    );
    // For scripts.
    assert_eq!(
        sb.atl(&[
            "repo",
            "delete",
            "-R",
            "DemiInfinity/Atelier",
            "--yes",
            "DemiInfinity/Atelier",
            "--json"
        ])
        .await
        .json(),
        json!({"deleted": true, "full_name": "DemiInfinity/Atelier"})
    );
}

// ------------------------------------------------------------------- issue

const ISSUES: &str = r##"{"issues":[
  {"repo":"DemiInfinity/Atelier","number":21,"title":"Tray icon flickers","state":"open","author":"owner:DemiInfinity","via":null,
   "labels":[{"name":"bug","color":"#d73a4a"},{"name":"tray","color":"#f2b85c"}],"comments":2,"created_at":"…","updated_at":"…",
   "web_path":"/DemiInfinity/Atelier/issues/21"},
  {"repo":"DemiInfinity/Atelier","number":20,"title":"Docs: pairing","state":"open","author":"person:maralind","via":"tether",
   "labels":[],"comments":0,"created_at":"…","updated_at":"…"}],"has_more":false}"##;

const ISSUE: &str = r##"{"repo":"DemiInfinity/Atelier","number":21,"title":"Tray icon flickers","state":"open","author":"owner:DemiInfinity","via":null,
  "labels":[{"name":"bug","color":"#d73a4a"}],"body":"The icon flickers.\n\nOn battery only.",
  "comments":[{"id":88,"author":"CodeRabbit[bot]","via":"tether","kind":"bot","body":"Possibly the redraw.","created_at":"…"}],
  "created_at":"…","updated_at":"…"}"##;

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn issue_list_view_new_and_close() {
    let sb = Sandbox::new().await;
    sb.get("/repos", REPOS).await;
    sb.get("/issues", ISSUES).await;
    sb.get("/repos/DemiInfinity/Atelier/issues/21", ISSUE).await;
    sb.respond(
        "POST",
        "/repos/DemiInfinity/Atelier/issues",
        201,
        r#"{"number":22,"title":"New thing","state":"open","comments":[]}"#,
    )
    .await;
    sb.respond("PATCH", "/repos/DemiInfinity/Atelier/issues/21", 200, "{}")
        .await;
    let atelier = working_copies(&sb);

    let list = "\
#21  Tray icon flickers   bug, tray · 2 comments · …
#20  Docs: pairing        … · via Tether
";
    assert_eq!(sb.atl_in(&atelier, &["issue"]).await.ok(), list);
    assert_eq!(sb.atl_in(&atelier, &["issue", "list"]).await.ok(), list);
    let asked = sb.server.received_requests().await.unwrap();
    let query = asked
        .iter()
        .find(|r| r.url.path().ends_with("/issues"))
        .and_then(|r| r.url.query().map(str::to_string))
        .unwrap();
    assert!(
        query.contains("repo=DemiInfinity%2FAtelier") && query.contains("state=open"),
        "{query}"
    );

    assert_eq!(
        sb.atl(&["issue", "view", "21", "-R", "DemiInfinity/Atelier"])
            .await
            .ok(),
        "\
#21  Tray icon flickers   open · by DemiInfinity
     bug

     The icon flickers.

     On battery only.

     CodeRabbit[bot] · via Tether · …
     Possibly the redraw.
"
    );
    assert_eq!(
        sb.atl_in(
            &atelier,
            &["issue", "new", "New thing", "--body", "Please."]
        )
        .await
        .ok(),
        "Opened issue #22 in Atelier.\n"
    );
    assert_eq!(
        sb.bodies("POST", "/repos/DemiInfinity/Atelier/issues")
            .await,
        [json!({"title": "New thing", "body": "Please."})]
    );
    assert_eq!(
        sb.atl_in(&atelier, &["issue", "close", "21"]).await.ok(),
        "Closed issue #21 in Atelier.\n"
    );
    assert_eq!(
        sb.bodies("PATCH", "/repos/DemiInfinity/Atelier/issues/21")
            .await,
        [json!({"state": "closed"})]
    );
    let json = sb
        .atl_in(&atelier, &["issue", "list", "--json"])
        .await
        .json();
    assert_eq!(json["repo"], "DemiInfinity/Atelier");
    assert_eq!(json["issues"][0]["number"], 21);
    // Outside a repository it needs to be told which.
    assert_eq!(sb.atl(&["issue"]).await.code, 1);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn pr_list_shows_each_pull_requests_checks() {
    let sb = Sandbox::new().await;
    sb.get("/repos", REPOS).await;
    sb.get(
        "/pulls",
        r#"{"pulls":[
          {"repo":"DemiInfinity/Atelier","number":14,"title":"Tunnel","head":"a","base":"main","state":"open",
           "checks":{"total":4,"passed":3,"failed":0,"pending":1,"waiting":0,"state":"pending"}},
          {"repo":"DemiInfinity/Atelier","number":15,"title":"SSH","head":"b","base":"main","state":"open",
           "checks":{"total":2,"passed":1,"failed":1,"pending":0,"waiting":0,"state":"failing"}},
          {"repo":"DemiInfinity/Marbl","number":3,"title":"Docs","head":"c","base":"main","state":"open",
           "checks":{"total":0,"passed":0,"failed":0,"pending":0,"waiting":0,"state":"none"}},
          {"repo":"DemiInfinity/Marbl","number":4,"title":"Old studio","head":"d","base":"main","state":"open"}]}"#,
    )
    .await;
    assert_eq!(
        sb.atl(&["pr"]).await.ok(),
        "\
#14  Tunnel   a → main   Atelier
     3 of 4 passed   ✓ 3 passed   ◐ 1 running

#15  SSH   b → main   Atelier
     1 failing   ✗ 1 failed   ✓ 1 passed

#3   Docs   c → main   Marbl
     no checks

#4   Old studio   d → main   Marbl
     no checks
"
    );
}

// ------------------------------------------------- pr update, behind, conflicts

fn behind_pull(number: u64, head: &str, extra: &str) -> String {
    format!(
        r#"{{"repo":"DemiInfinity/Atelier","number":{number},"title":"Pull {number}","state":"open","head":"{head}","base":"master",
            "merge":{{"can_merge":true,"methods":["squash"]}}{extra}}}"#
    )
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn pr_update() {
    let sb = Sandbox::new().await;
    sb.get("/repos", REPOS).await;
    let atelier = working_copies(&sb);
    let marbl = sb.home().join("Atelier/DemiInfinity/Marbl");
    let pulls = "/repos/DemiInfinity/Atelier/pulls";
    sb.get(
        pulls,
        &format!(
            r#"{{"pulls":[{},{}]}}"#,
            behind_pull(13, "docs", r#","behind":1"#),
            behind_pull(14, "feature/tunnel-ui", r#","behind":3"#)
        ),
    )
    .await;
    sb.get("/repos/DemiInfinity/Marbl/pulls", r#"{"pulls":[]}"#)
        .await;
    for (n, head) in [
        (13, "docs"),
        (14, "feature/tunnel-ui"),
        (15, "x"),
        (16, "y"),
    ] {
        sb.get(&format!("{pulls}/{n}"), &behind_pull(n, head, ""))
            .await;
    }
    sb.respond(
        "POST",
        &format!("{pulls}/14/update"),
        200,
        r#"{"updated":true,"head_sha":"4f2a9c1e77aa01bc"}"#,
    )
    .await;
    sb.respond(
        "POST",
        &format!("{pulls}/13/update"),
        200,
        r#"{"updated":false,"head_sha":"77aa01bc"}"#,
    )
    .await;
    sb.respond(
        "POST",
        &format!("{pulls}/15/update"),
        409,
        r#"{"error":"conflicts","message":"master and x both changed 2 files.","files":["cmd/app/main.go","README.md"]}"#,
    )
    .await;
    sb.respond(
        "POST",
        &format!("{pulls}/16/update"),
        422,
        r#"{"error":"not_open","message":"This pull request is already merged."}"#,
    )
    .await;

    // No number: the open pull request of the branch this folder is on.
    assert_eq!(
        sb.atl_in(&atelier, &["pr", "update"]).await.ok(),
        "✓ Updated #14 from master (4f2a9c1)\nYour local branch is behind now: git pull\n"
    );
    assert_eq!(sb.count("POST", &format!("{pulls}/14/update")).await, 1);
    // The request has no body.
    assert_eq!(
        sb.bodies("POST", &format!("{pulls}/14/update")).await,
        [json!(null)]
    );
    // By number from anywhere: nothing is said about a local branch.
    let scope = ["-R", "DemiInfinity/Atelier"];
    assert_eq!(
        sb.atl(&[&["pr", "update", "14"], &scope[..]].concat())
            .await
            .ok(),
        "✓ Updated #14 from master (4f2a9c1)\n"
    );
    assert_eq!(
        sb.atl(&[&["pr", "update", "14", "--json"], &scope[..]].concat())
            .await
            .json(),
        json!({"number": 14, "updated": true, "head_sha": "4f2a9c1e77aa01bc",
               "head": "feature/tunnel-ui", "base": "master", "local_behind": false})
    );
    assert_eq!(
        sb.atl_in(&atelier, &["pr", "update", "--json"])
            .await
            .json()["local_behind"],
        json!(true)
    );

    // Already current.
    assert_eq!(
        sb.atl_in(&atelier, &["pr", "update", "13"]).await.ok(),
        "#13 already has everything from master.\n"
    );
    assert_eq!(
        sb.atl_in(&atelier, &["pr", "update", "13", "--json"])
            .await
            .json(),
        json!({"number": 13, "updated": false, "head_sha": "77aa01bc", "head": "docs", "base": "master"})
    );

    // Conflicts: a non-zero exit and the files.
    let conflicts = sb.atl_in(&atelier, &["pr", "update", "15"]).await;
    assert_eq!(conflicts.code, 1);
    assert_eq!(conflicts.stdout, "");
    assert_eq!(
        conflicts.stderr,
        "#15 conflicts with master.\n  cmd/app/main.go\n  README.md\nResolve it here: git fetch, git merge origin/master, fix the files, git push.\n"
    );
    let conflicts = sb.atl_in(&atelier, &["pr", "update", "15", "--json"]).await;
    assert_eq!(conflicts.code, 1);
    assert_eq!(conflicts.stdout, "");
    assert_eq!(
        serde_json::from_str::<serde_json::Value>(&conflicts.stderr).unwrap(),
        json!({"error": "conflicts", "message": "master and x both changed 2 files.",
               "files": ["cmd/app/main.go", "README.md"],
               "hint": "Resolve it here: git fetch, git merge origin/master, fix the files, git push."})
    );

    // Closed or merged: the studio's refusal.
    let closed = sb.atl_in(&atelier, &["pr", "update", "16"]).await;
    assert_eq!(closed.code, 1);
    assert_eq!(closed.stderr, "This pull request is already merged.\n");

    // A push landed meanwhile; a rule on the branch; a home on GitHub.
    for (n, status, body, said) in [
        (
            17,
            409,
            r#"{"error":"branch_moved","message":"x changed while the update was running."}"#,
            "x changed while the update was running. Run it again.\n",
        ),
        (
            18,
            403,
            r#"{"error":"branch_protected","message":"x only takes changes through pull requests."}"#,
            "x only takes changes through pull requests.\n",
        ),
        (
            19,
            422,
            r#"{"error":"on_github"}"#,
            "This repository’s home is GitHub. Update the branch there.\n",
        ),
    ] {
        sb.respond("POST", &format!("{pulls}/{n}/update"), status, body)
            .await;
        let refused = sb.atl_in(&atelier, &["pr", "update", &n.to_string()]).await;
        assert_eq!(refused.code, 1, "{n}");
        assert_eq!(refused.stderr, said);
    }

    // A branch without a pull request, and no folder to tell the branch from.
    let none = sb.atl_in(&marbl, &["pr", "update"]).await;
    assert_eq!(none.code, 1);
    assert_eq!(
        none.stderr,
        "There is no open pull request for master. Name one: atl pr update N.\n"
    );
    let which = sb.atl(&[&["pr", "update"], &scope[..]].concat()).await;
    assert_eq!(which.code, 1);
    assert_eq!(
        which.stderr,
        "Say which pull request to update. Name it: atl pr update N.\n"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn pr_shows_behind_and_conflicts() {
    let sb = Sandbox::new().await;
    sb.get("/repos", REPOS).await;
    let list = format!(
        r#"{{"pulls":[{},{},{}]}}"#,
        behind_pull(14, "a", r#","behind":3,"ahead":2,"conflicts":false"#),
        behind_pull(15, "b", r#","behind":1,"conflicts":true"#),
        behind_pull(16, "c", r#","behind":0"#)
    );
    sb.get("/pulls", &list).await;
    assert_eq!(
        sb.atl(&["pr"]).await.ok(),
        "\
#14  Pull 14   a → master   Atelier
     no checks   3 behind master

#15  Pull 15   b → master   Atelier
     no checks   ✗ conflicts   1 behind master

#16  Pull 16   c → master   Atelier
     no checks
"
    );

    let pulls = "/repos/DemiInfinity/Atelier/pulls";
    sb.get(
        &format!("{pulls}/14"),
        &behind_pull(14, "a", r#","behind":3"#),
    )
    .await;
    sb.get(
        &format!("{pulls}/15"),
        &behind_pull(
            15,
            "b",
            r#","behind":1,"conflicts":true,"conflict_files":["cmd/app/main.go","README.md"]"#,
        ),
    )
    .await;
    let scope = ["-R", "DemiInfinity/Atelier"];
    assert_eq!(
        sb.atl(&[&["pr", "view", "14"], &scope[..]].concat())
            .await
            .ok(),
        "\
#14  Pull 14   a → master

     3 behind master: atl pr update 14

     Ready to merge: atl pr merge 14
"
    );
    // The studio says it can't be updated (a protected head): no command offered.
    sb.get(
        &format!("{pulls}/16"),
        &behind_pull(16, "c", r#","behind":2,"can_update":false"#),
    )
    .await;
    assert_eq!(
        sb.atl(&[&["pr", "16"], &scope[..]].concat()).await.ok(),
        "\
#16  Pull 16   c → master

     2 behind master.

     Ready to merge: atl pr merge 16
"
    );
    let conflicting = "\
#15  Pull 15   b → master

     Conflicts with master
       cmd/app/main.go
       README.md
     Can’t merge or update until it is resolved: git fetch, git merge origin/master, fix the files, git push.
";
    assert_eq!(
        sb.atl(&[&["pr", "view", "15"], &scope[..]].concat())
            .await
            .ok(),
        conflicting
    );
    // `atl pr N` is the same screen.
    assert_eq!(
        sb.atl(&[&["pr", "15"], &scope[..]].concat()).await.ok(),
        conflicting
    );
    let json = sb
        .atl(&[&["pr", "view", "15", "--json"], &scope[..]].concat())
        .await
        .json();
    assert_eq!(json["conflicts"], json!(true));
    assert_eq!(
        json["conflict_files"],
        json!(["cmd/app/main.go", "README.md"])
    );
    assert_eq!(json["behind"], json!(1));
}
