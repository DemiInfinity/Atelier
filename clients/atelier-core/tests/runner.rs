//! The runner's local control socket, against a fake runner.
#![cfg(unix)]

use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use atelier_core::runner::{PausedReason, RunnerRulesUpdate, WINDOWS_PIPE};
use atelier_core::{Error, RunnerClient};
use serde_json::{json, Value};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::UnixListener;

/// The contract's example answers, verbatim.
const STATUS: &str = r#"{"ok":true,"name":"MacBook","version":"7f82d31","server":"https://…","online":true,
   "taking_jobs":true,"paused_reason":null,"capabilities":["jobs","agents","claude-code","workspaces"],
   "rules":{"pause_on_battery":true,"max_parallel":1,"container_jobs":false},
   "current":[{"job_id":412,"repo":"…","workflow":"review-helper","kind":"agent","started_at":"…","detail":"turn 3 of 20"}]}"#;
const TAIL: &str = r#"{"ok":true,"lines":["→ pulls.get Atelier 14","…"]}"#;
const WORKSPACES: &str = r#"{"ok":true,"root":"/Users/x/Atelier","presets":["fork","vscode","cursor"],
   "items":[{"repo":"DemiInfinity/Atelier","path":"/Users/x/Atelier/DemiInfinity/Atelier","branch":"main","dirty":false}]}"#;

/// A runner that answers each request line with the next canned answer and
/// records what it was asked.
struct FakeRunner {
    _dir: tempfile::TempDir,
    socket: PathBuf,
    asked: Arc<Mutex<Vec<Value>>>,
}

impl FakeRunner {
    fn start(answers: &[&str]) -> FakeRunner {
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("control.sock");
        let listener = UnixListener::bind(&socket).unwrap();
        let asked = Arc::new(Mutex::new(Vec::new()));
        let seen = asked.clone();
        // One line each, as the runner writes them.
        let answers: Vec<String> = answers.iter().map(|a| a.replace('\n', " ")).collect();
        tokio::spawn(async move {
            for answer in answers {
                let (stream, _) = listener.accept().await.unwrap();
                let mut stream = BufReader::new(stream);
                let mut line = String::new();
                stream.read_line(&mut line).await.unwrap();
                seen.lock()
                    .unwrap()
                    .push(serde_json::from_str(&line).unwrap());
                if answer.is_empty() {
                    continue; // hang up without a word
                }
                stream
                    .get_mut()
                    .write_all(format!("{answer}\n").as_bytes())
                    .await
                    .unwrap();
            }
        });
        FakeRunner {
            _dir: dir,
            socket,
            asked,
        }
    }

    fn client(&self) -> RunnerClient {
        RunnerClient::new(&self.socket)
    }

    fn asked(&self) -> Vec<Value> {
        self.asked.lock().unwrap().clone()
    }
}

#[tokio::test]
async fn status() {
    let runner = FakeRunner::start(&[STATUS]);
    let status = runner.client().status().await.unwrap();
    assert_eq!(runner.asked(), [json!({"op":"status"})]);
    assert_eq!(status.name, "MacBook");
    assert_eq!(status.version, "7f82d31");
    assert_eq!(status.server, "https://…");
    assert!(status.online && status.taking_jobs);
    assert_eq!(status.paused_reason, None);
    assert_eq!(
        status.capabilities,
        ["jobs", "agents", "claude-code", "workspaces"]
    );
    assert!(status.rules.pause_on_battery);
    assert_eq!(status.rules.max_parallel, 1);
    assert!(!status.rules.container_jobs);
    let job = &status.current[0];
    assert_eq!((job.job_id, job.workflow.as_str()), (412, "review-helper"));
    assert_eq!(
        (job.kind.as_str(), job.detail.as_str()),
        ("agent", "turn 3 of 20")
    );
}

#[tokio::test]
async fn set_taking_jobs_and_rules() {
    let paused = STATUS
        .replace(r#""taking_jobs":true"#, r#""taking_jobs":false"#)
        .replace(r#""paused_reason":null"#, r#""paused_reason":"user""#);
    let runner = FakeRunner::start(&[&paused, STATUS]);
    let client = runner.client();

    let status = client.set_taking_jobs(false).await.unwrap();
    assert!(!status.taking_jobs);
    assert_eq!(status.paused_reason, Some(PausedReason::User));

    let rules = RunnerRulesUpdate {
        pause_on_battery: Some(true),
        ..RunnerRulesUpdate::default()
    };
    assert!(
        client
            .set_rules(&rules)
            .await
            .unwrap()
            .rules
            .pause_on_battery
    );

    // Exactly the contract's request lines.
    assert_eq!(
        runner.asked(),
        [
            json!({"op":"set","taking_jobs":false}),
            json!({"op":"set","rules":{"pause_on_battery":true}})
        ]
    );
}

#[tokio::test]
async fn tail_cancel_workspaces() {
    let runner = FakeRunner::start(&[TAIL, r#"{"ok":true}"#, WORKSPACES]);
    let client = runner.client();
    assert_eq!(
        client.tail(412, Some(50)).await.unwrap(),
        ["→ pulls.get Atelier 14", "…"]
    );
    client.cancel(412).await.unwrap();
    let workspaces = client.workspaces().await.unwrap();
    assert_eq!(workspaces.root, "/Users/x/Atelier");
    assert_eq!(workspaces.presets, ["fork", "vscode", "cursor"]);
    let item = &workspaces.items[0];
    assert_eq!(item.repo, "DemiInfinity/Atelier");
    assert_eq!(item.path, "/Users/x/Atelier/DemiInfinity/Atelier");
    assert_eq!(item.branch, "main");
    assert!(!item.dirty);
    assert_eq!(
        runner.asked(),
        [
            json!({"op":"tail","job_id":412,"lines":50}),
            json!({"op":"cancel","job_id":412}),
            json!({"op":"workspaces"})
        ]
    );
}

#[tokio::test]
async fn paused_reasons_as_built() {
    let with = |reason: &str| {
        STATUS
            .replace(r#""taking_jobs":true"#, r#""taking_jobs":false"#)
            .replace(
                r#""paused_reason":null"#,
                &format!(r#""paused_reason":{reason}"#),
            )
            .replace(r#""kind":"agent""#, r#""kind":"job""#)
            .replace(r#""online":true"#, r#""online":false"#)
    };
    let answers = [
        with(r#""user""#),
        with(r#""battery""#),
        with(r#""jobs_off""#),
        with(r#""solar_flare""#),
    ];
    let answers: Vec<&str> = answers.iter().map(String::as_str).collect();
    let runner = FakeRunner::start(&answers);
    let client = runner.client();
    let mut reasons = Vec::new();
    for _ in 0..4 {
        let status = client.status().await.unwrap();
        assert!(!status.taking_jobs && !status.online);
        assert_eq!(status.current[0].kind, "job");
        reasons.push(status.paused_reason.unwrap());
    }
    assert_eq!(
        reasons,
        [
            PausedReason::User,
            PausedReason::Battery,
            PausedReason::JobsOff,
            PausedReason::Unknown
        ]
    );
}

#[tokio::test]
async fn set_refusals_and_tail_limits() {
    let refusal = r#"{"ok":false,"error":"nothing to change"}"#;
    let not_running = r#"{"ok":false,"error":"job 7 is not running"}"#;
    let runner = FakeRunner::start(&[STATUS, refusal, TAIL, TAIL, not_running]);
    let client = runner.client();
    let parallel = |n| RunnerRulesUpdate {
        max_parallel: Some(n),
        ..RunnerRulesUpdate::default()
    };

    // Refused here, without bothering the runner.
    for bad in [parallel(0), parallel(65), RunnerRulesUpdate::default()] {
        assert!(matches!(
            client.set_rules(&bad).await.unwrap_err(),
            Error::Invalid(_)
        ));
    }
    assert!(runner.asked().is_empty());
    client.set_rules(&parallel(64)).await.unwrap();
    // And the runner's own refusal is passed on.
    let err = client.set_rules(&parallel(1)).await.unwrap_err();
    assert!(
        matches!(&err, Error::Runner(m) if m == "nothing to change"),
        "{err:?}"
    );

    // Tail: the default is the runner's (50), the cap is 500, and a job
    // that isn't running now is an error from the runner.
    client.tail(412, None).await.unwrap();
    client.tail(412, Some(9000)).await.unwrap();
    let err = client.tail(7, Some(10)).await.unwrap_err();
    assert!(
        matches!(&err, Error::Runner(m) if m == "job 7 is not running"),
        "{err:?}"
    );
    assert_eq!(
        runner.asked(),
        [
            json!({"op":"set","rules":{"max_parallel":64}}),
            json!({"op":"set","rules":{"max_parallel":1}}),
            json!({"op":"tail","job_id":412}),
            json!({"op":"tail","job_id":412,"lines":500}),
            json!({"op":"tail","job_id":7,"lines":10})
        ]
    );
}

#[tokio::test]
async fn errors_from_the_runner() {
    let runner = FakeRunner::start(&[r#"{"ok":false,"error":"no such job"}"#, "nonsense", ""]);
    let client = runner.client();
    let err = client.cancel(1).await.unwrap_err();
    assert!(
        matches!(&err, Error::Runner(m) if m == "no such job"),
        "{err:?}"
    );
    assert_eq!(err.to_string(), "The runner said: no such job");
    assert!(matches!(
        client.status().await.unwrap_err(),
        Error::Runner(_)
    ));
    assert!(matches!(
        client.status().await.unwrap_err(),
        Error::Runner(_)
    ));
}

#[tokio::test]
async fn unknown_fields_are_tolerated() {
    let runner = FakeRunner::start(&[
        r#"{"ok":true,"name":"MacBook","gpu":{"count":2},"rules":{"night_only":true},"current":null}"#,
    ]);
    let status = runner.client().status().await.unwrap();
    assert_eq!(status.name, "MacBook");
    assert!(status.current.is_empty());
}

#[tokio::test]
async fn not_running() {
    let dir = tempfile::tempdir().unwrap();
    // No socket at all.
    let missing = RunnerClient::new(dir.path().join("control.sock"));
    assert!(matches!(
        missing.status().await.unwrap_err(),
        Error::RunnerNotRunning
    ));
    // A socket file left behind by a runner that is gone.
    let stale = dir.path().join("stale.sock");
    drop(std::os::unix::net::UnixListener::bind(&stale).unwrap());
    assert!(matches!(
        RunnerClient::new(&stale).status().await.unwrap_err(),
        Error::RunnerNotRunning
    ));
    // A path too long to be a socket: the runner runs without the control
    // channel there, so from here it is "not running" too.
    let long = dir.path().join("x".repeat(200)).join("control.sock");
    assert!(matches!(
        RunnerClient::new(&long).status().await.unwrap_err(),
        Error::RunnerNotRunning
    ));
}

#[tokio::test]
async fn a_runner_that_never_answers_times_out() {
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("control.sock");
    let listener = UnixListener::bind(&socket).unwrap();
    tokio::spawn(async move {
        let (_stream, _) = listener.accept().await.unwrap();
        tokio::time::sleep(Duration::from_secs(30)).await;
    });
    let client = RunnerClient::new(&socket).with_timeout(Duration::from_millis(100));
    assert!(matches!(
        client.status().await.unwrap_err(),
        Error::Runner(_)
    ));
}

#[test]
fn the_socket_sits_next_to_the_runner_config() {
    let client = RunnerClient::for_config(std::path::Path::new("/x/atelier-runner/config.json"));
    assert_eq!(
        client.path(),
        std::path::Path::new("/x/atelier-runner/control.sock")
    );
    assert_eq!(WINDOWS_PIPE, r"\\.\pipe\atelier-runner");
}
