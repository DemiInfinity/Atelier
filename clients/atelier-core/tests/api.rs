//! Every endpoint of sections 1–9: the request we send and how we read the
//! contract's example answers (copied verbatim from docs/app-api.md).

mod common;

use std::time::Duration;

use atelier_core::api::*;
use atelier_core::{Error, PollOptions};
use common::{anonymous, bearer, client};
use serde_json::json;
use wiremock::matchers::{body_json, header, method, path, query_param, query_param_is_missing};
use wiremock::{Mock, MockServer, ResponseTemplate};

fn raw(status: u16, body: &str) -> ResponseTemplate {
    ResponseTemplate::new(status).set_body_raw(body.to_owned(), "application/json")
}

const START: &str = r#"{"device_code": "<64 hex, secret>", "user_code": "AMBER-4127",
 "verify_url": "https://studio.example/settings/devices/approve?code=AMBER-4127",
 "expires_in": 600, "interval": 2}"#;

const GRANT: &str = r#"{"token":"atd_…","device_id":7,"person":{"name":"Demi","login":"DemiInfinity"},"studio":{"name":"Atelier","url":"https://studio.example","version":"7f82d31"}}"#;

const ME: &str = r#"{"person":{"name":"Demi","login":"DemiInfinity","avatar_url":"/-/avatar/DemiInfinity/160.png"},
 "accounts":[{"name":"DemiInfinity","display":"Demi","kind":"user","commit_name":"Demi","commit_email":"demi@example.com"},
             {"name":"elyndrastudios","display":"Elyndra Studios LTD","kind":"company","commit_name":"Demi","commit_email":"demi@company.example"}],
 "studio":{"name":"Atelier","url":"https://studio.example","version":"7f82d31",
           "ssh":{"host":"studio.example","port":2223},"tether":true,"connect":false},
 "device":{"id":7,"name":"MacBook","kind":"desktop"}}"#;

const STATUS: &str = r#"{"needs":[
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
 "unread":3}"#;

const REPOS: &str = r##"{"repos":[{"full_name":"DemiInfinity/Atelier","owner":"DemiInfinity","owner_kind":"user","name":"Atelier",
  "private":true,"description":"…","default_branch":"main","language":"Go","language_color":"#00ADD8",
  "clone_https":"https://studio.example/DemiInfinity/Atelier.git",
  "clone_ssh":"ssh://git@studio.example:2223/DemiInfinity/Atelier.git",
  "github":{"repo":"DemiInfinity/Atelier","home":"atelier"},"open_pulls":1,"updated_at":"…"}]}"##;

const GITHUB_HITS: &str =
    r#"{"github_hits":[{"full_name":"maralind/loopkit","description":"…","atelier_badge":true}]}"#;

const PULL: &str = r#"{"repo":"DemiInfinity/Atelier","number":14,"title":"Remote access switch in the tray menu","state":"open",
 "head":"feature/tunnel-ui","base":"main","author":"owner:DemiInfinity","commits":2,"files":3,
 "via":null,
 "merge":{"can_merge":false,"reason":"1 check is still running and 1 agent is waiting for you.","methods":["squash","merge"],
          "on_github":false,"auto_merge":null},
 "checks":[{"name":"ci · test","status":"success","detail":"MacBook · 1m 48s","run_id":207,"job_id":410},
           {"name":"image · build","status":"pending","detail":"Portainer host · step 3 of 5","run_id":209,"job_id":413},
           {"name":"review-helper","status":"waiting","detail":"agent · needs your approval to run","run_id":210,"job_id":414,"action":"approve_agent"}],
 "comments":[{"author":"CodeRabbit","via":"tether","kind":"bot","body":"…","at":"…"}],
 "web_path":"/DemiInfinity/Atelier/pulls/14"}"#;

/// "Summary = the same object without `comments` and with `checks` reduced".
const PULL_SUMMARIES: &str = r#"{"pulls":[{"repo":"DemiInfinity/Atelier","number":14,"title":"Remote access switch in the tray menu","state":"open",
 "head":"feature/tunnel-ui","base":"main","author":"owner:DemiInfinity","commits":2,"files":3,
 "via":null,
 "merge":{"can_merge":false,"reason":"1 check is still running and 1 agent is waiting for you.","methods":["squash","merge"],
          "on_github":false,"auto_merge":null},
 "checks":{"passed":2,"failed":0,"pending":1,"waiting":1},
 "web_path":"/DemiInfinity/Atelier/pulls/14"}]}"#;

const RUNS: &str = r#"{"runs":[{"run_id":207,"repo":"…","workflow":"ci","event":"push","branch":"feature/tunnel-ui","status":"success","started_at":"…","duration_s":108,
  "jobs":[{"job_id":410,"name":"test","status":"success","runner":"MacBook","duration_s":108,"kind":"job"}]}]}"#;

const LOG: &str = r#"{"lines":["…"],"truncated":true,"status":"running"}"#;

const WAITING: &str = r#"{"jobs":[{"job_id":31,"run_id":210,"repo":"…","workflow":"weekly-tidy",
  "reason":"first run","engine":"claude-code","tools":["issues.list","issues.comment","issues.close"],
  "write_tools":["issues.comment","issues.close"],"max_turns":20,"timeout_minutes":10}]}"#;

const REMOTE: &str =
    r#"{"on":false,"mode":"quick","address":null,"exposed":["web","git"],"connected":0}"#;

const WHO: &str = r#"{"sessions":[{"ip":"203.0.113.42","country":"NL","signed_in":true,"class":"web","since":"…"}]}"#;

const LINK: &str = r#"{"url":"https://<tunnel host>/…","expires_in":900}"#;

const MESSAGES: &str = r#"{"tone":"friendly",
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
 "rules":{"quiet_fullscreen":true,"terminal":true,"milestones":true}}"#;

// ------------------------------------------------------------------ devices

#[tokio::test]
async fn device_start_sends_the_device_and_reads_the_codes() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/device/start"))
        .and(body_json(
            json!({"name": "MacBook", "kind": "desktop", "os": "macos", "version": "0.1.0"}),
        ))
        .respond_with(raw(200, START))
        .expect(1)
        .mount(&server)
        .await;

    let request = DeviceStartRequest {
        name: "MacBook".into(),
        kind: DeviceKind::Desktop,
        os: "macos".into(),
        version: "0.1.0".into(),
    };
    let start = anonymous(&server).device_start(&request).await.unwrap();
    assert_eq!(start.device_code, "<64 hex, secret>");
    assert_eq!(start.user_code, "AMBER-4127");
    assert_eq!(
        start.verify_url,
        "https://studio.example/settings/devices/approve?code=AMBER-4127"
    );
    assert_eq!((start.expires_in, start.interval), (600, 2));
    // The device code is a secret: Debug must not show it.
    assert!(!format!("{start:?}").contains("64 hex"));
}

#[tokio::test]
async fn device_start_sends_no_authorization() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/device/start"))
        .respond_with(raw(200, START))
        .mount(&server)
        .await;
    // Even a client that has a token doesn't send it here.
    client(&server)
        .device_start(&DeviceStartRequest::new("MacBook", DeviceKind::Cli))
        .await
        .unwrap();
    let requests = server.received_requests().await.unwrap();
    assert!(!requests[0].headers.contains_key("authorization"));
    let body: serde_json::Value = requests[0].body_json().unwrap();
    assert_eq!(body["kind"], "cli");
    assert_eq!(body["version"], atelier_core::VERSION);
}

#[tokio::test]
async fn device_poll_pending_then_approved() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/device/poll"))
        .and(body_json(json!({"device_code": "abc"})))
        .respond_with(raw(202, r#"{"status":"pending"}"#))
        .up_to_n_times(1)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/device/poll"))
        .and(body_json(json!({"device_code": "abc"})))
        .respond_with(raw(200, GRANT))
        .mount(&server)
        .await;

    let client = anonymous(&server);
    assert_eq!(
        client.device_poll("abc").await.unwrap(),
        DevicePoll::Pending
    );
    let DevicePoll::Approved(grant) = client.device_poll("abc").await.unwrap() else {
        panic!("expected approval");
    };
    assert_eq!(grant.token.expose(), "atd_…");
    assert_eq!(grant.device_id, 7);
    assert_eq!(grant.person.name, "Demi");
    assert_eq!(grant.person.login, "DemiInfinity");
    assert_eq!(grant.studio.name, "Atelier");
    assert_eq!(grant.studio.url, "https://studio.example");
    assert_eq!(grant.studio.version, "7f82d31");
    assert!(!format!("{grant:?}").contains("atd_"));
}

fn start_with(interval: u64, expires_in: u64) -> DeviceStart {
    DeviceStart {
        device_code: "abc".into(),
        user_code: "AMBER-4127".into(),
        verify_url: String::new(),
        expires_in,
        interval,
    }
}

const FAST: PollOptions = PollOptions {
    min_interval: Duration::from_millis(20),
};

#[tokio::test]
async fn wait_for_device_polls_until_approved() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/device/poll"))
        .respond_with(raw(202, r#"{"status":"pending"}"#))
        .up_to_n_times(3)
        .expect(3)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/device/poll"))
        .respond_with(raw(200, GRANT))
        .expect(1)
        .mount(&server)
        .await;
    let grant = anonymous(&server)
        .wait_for_device(&start_with(0, 600), FAST)
        .await
        .unwrap();
    assert_eq!(grant.device_id, 7);
}

#[tokio::test]
async fn wait_for_device_respects_the_interval() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/device/poll"))
        .respond_with(raw(202, r#"{"status":"pending"}"#))
        .up_to_n_times(1)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/device/poll"))
        .respond_with(raw(200, GRANT))
        .mount(&server)
        .await;
    let began = std::time::Instant::now();
    anonymous(&server)
        .wait_for_device(&start_with(1, 600), FAST)
        .await
        .unwrap();
    // Two polls, one second before each.
    assert!(began.elapsed() >= Duration::from_millis(1900));
}

#[tokio::test]
async fn wait_for_device_denied() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/device/poll"))
        .respond_with(raw(410, r#"{"error":"denied"}"#))
        .mount(&server)
        .await;
    let err = anonymous(&server)
        .wait_for_device(&start_with(0, 600), FAST)
        .await
        .unwrap_err();
    assert!(matches!(err, Error::DeviceDenied), "{err:?}");
}

#[tokio::test]
async fn wait_for_device_expired_by_the_server() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/device/poll"))
        .respond_with(raw(410, r#"{"error":"expired"}"#))
        .mount(&server)
        .await;
    let err = anonymous(&server)
        .wait_for_device(&start_with(0, 600), FAST)
        .await
        .unwrap_err();
    assert!(matches!(err, Error::DeviceExpired), "{err:?}");
}

#[tokio::test]
async fn wait_for_device_gives_up_when_the_code_runs_out() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/device/poll"))
        .respond_with(raw(202, r#"{"status":"pending"}"#))
        .mount(&server)
        .await;
    // Expires in one second; never approved.
    let err = anonymous(&server)
        .wait_for_device(
            &start_with(0, 1),
            PollOptions {
                min_interval: Duration::from_millis(300),
            },
        )
        .await
        .unwrap_err();
    assert!(matches!(err, Error::DeviceExpired), "{err:?}");
    let polls = server.received_requests().await.unwrap().len();
    assert!((1..=3).contains(&polls), "{polls} polls");
}

#[tokio::test]
async fn ssh_key_created_and_already_known() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/device/ssh-key"))
        .and(header("authorization", bearer().as_str()))
        .and(body_json(
            json!({"title":"MacBook (Atelier Desktop)","key":"ssh-ed25519 AAAA…"}),
        ))
        .respond_with(raw(201, r#"{"id":12,"fingerprint":"SHA256:…"}"#))
        .up_to_n_times(1)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/device/ssh-key"))
        .respond_with(raw(200, r#"{"id":12,"fingerprint":"SHA256:…"}"#))
        .mount(&server)
        .await;
    let client = client(&server);
    let first = client
        .register_ssh_key("MacBook (Atelier Desktop)", "ssh-ed25519 AAAA…\n")
        .await
        .unwrap();
    assert_eq!((first.id, first.fingerprint.as_str()), (12, "SHA256:…"));
    assert!(first.created);
    let again = client
        .register_ssh_key("MacBook (Atelier Desktop)", "ssh-ed25519 AAAA…")
        .await
        .unwrap();
    assert_eq!(again.id, 12);
    assert!(!again.created);
}

// -------------------------------------------------------------- me, status

#[tokio::test]
async fn me() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/me"))
        .and(header("authorization", bearer().as_str()))
        .respond_with(raw(200, ME))
        .expect(1)
        .mount(&server)
        .await;
    let me = client(&server).me().await.unwrap();
    assert_eq!(me.person.login, "DemiInfinity");
    assert_eq!(
        me.person.avatar_url.as_deref(),
        Some("/-/avatar/DemiInfinity/160.png")
    );
    assert_eq!(me.accounts.len(), 2);
    assert_eq!(me.accounts[1].name, "elyndrastudios");
    assert_eq!(me.accounts[1].display, "Elyndra Studios LTD");
    assert_eq!(me.accounts[1].kind, "company");
    assert_eq!(me.accounts[1].commit_email, "demi@company.example");
    assert_eq!(
        me.studio.ssh,
        Some(SshEndpoint {
            host: "studio.example".into(),
            port: 2223
        })
    );
    assert!(me.studio.tether);
    assert!(!me.studio.connect);
    let device = me.device.unwrap();
    assert_eq!((device.id, device.name.as_str()), (7, "MacBook"));
    assert_eq!(device.kind, DeviceKind::Desktop);
}

#[tokio::test]
async fn status() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/status"))
        .and(header("authorization", bearer().as_str()))
        .respond_with(raw(200, STATUS))
        .mount(&server)
        .await;
    let status = client(&server).status().await.unwrap();
    assert_eq!(status.unread, 3);
    assert_eq!(status.needs.len(), 3);

    let agent = &status.needs[0];
    assert_eq!(agent.id, "agent:31");
    assert_eq!(agent.kind, NeedKind::AgentApproval);
    assert_eq!(agent.thread, Thread::Agents);
    assert_eq!(agent.title, "Approve weekly-tidy before it runs");
    assert_eq!(agent.repo, "DemiInfinity/Atelier");
    let action = agent.action.as_ref().unwrap();
    assert_eq!(action.label, "Review");
    assert_eq!(action.kind, "approve_agent");
    assert_eq!(action.job_id, Some(31));

    let failed = &status.needs[1];
    assert_eq!(failed.kind, NeedKind::RunFailed);
    assert_eq!(failed.thread, Thread::Yours);
    assert_eq!(failed.action.as_ref().unwrap().run_id, Some(208));

    let pull = &status.needs[2];
    assert_eq!(pull.kind, NeedKind::PullReady);
    assert_eq!(pull.web_path, "/DemiInfinity/Tray/pulls/9");
    assert_eq!(pull.action.as_ref().unwrap().kind, "open_pull");
    assert_eq!(pull.action.as_ref().unwrap().number, Some(9));

    let running = &status.running[0];
    assert_eq!((running.job_id, running.run_id), (412, 209));
    assert_eq!(running.workflow, "review-helper");
    assert_eq!(running.kind, "agent");
    assert_eq!(running.engine.as_deref(), Some("claude-code"));
    assert_eq!(running.runner, "MacBook");
    assert_eq!(running.started_at, "2026-11-11T09:14:02Z");
    assert_eq!(running.detail, "pull request 14 · turn 3 of 20");

    let threads: Vec<Thread> = status.feed.iter().map(|f| f.thread).collect();
    assert_eq!(threads, [Thread::Yours, Thread::Github, Thread::Connect]);
    assert_eq!(
        status.feed[1].text,
        "A reviewer approved pull request 63 on sparky-ventures"
    );
}

// -------------------------------------------------------------------- repos

#[tokio::test]
async fn repos_list() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/repos"))
        .and(query_param_is_missing("q"))
        .and(header("authorization", bearer().as_str()))
        .respond_with(raw(200, REPOS))
        .expect(1)
        .mount(&server)
        .await;
    let repos = client(&server).repos().await.unwrap();
    let repo = &repos[0];
    assert_eq!(repo.full_name, "DemiInfinity/Atelier");
    assert_eq!(
        (repo.owner.as_str(), repo.name.as_str()),
        ("DemiInfinity", "Atelier")
    );
    assert_eq!(repo.owner_kind, "user");
    assert!(repo.private);
    assert_eq!(repo.default_branch, "main");
    assert_eq!(repo.language.as_deref(), Some("Go"));
    assert_eq!(repo.language_color.as_deref(), Some("#00ADD8"));
    assert_eq!(
        repo.clone_https,
        "https://studio.example/DemiInfinity/Atelier.git"
    );
    assert_eq!(
        repo.clone_ssh,
        "ssh://git@studio.example:2223/DemiInfinity/Atelier.git"
    );
    let github = repo.github.as_ref().unwrap();
    assert_eq!(
        (github.repo.as_str(), github.home.as_str()),
        ("DemiInfinity/Atelier", "atelier")
    );
    assert_eq!(repo.open_pulls, 1);
}

#[tokio::test]
async fn repos_search_with_github_hits() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/repos"))
        .and(query_param("q", "loop"))
        .and(query_param("github", "1"))
        .respond_with(raw(200, GITHUB_HITS))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/repos"))
        .and(query_param("q", "loop"))
        .and(query_param_is_missing("github"))
        .respond_with(raw(200, r#"{"repos":[]}"#))
        .expect(1)
        .mount(&server)
        .await;
    let client = client(&server);
    let found = client.search_repos("loop", true).await.unwrap();
    assert!(found.repos.is_empty());
    assert_eq!(found.github_hits[0].full_name, "maralind/loopkit");
    assert!(found.github_hits[0].atelier_badge);
    let plain = client.search_repos("loop", false).await.unwrap();
    assert!(plain.github_hits.is_empty());
}

#[tokio::test]
async fn repo_create_and_its_refusals() {
    let server = MockServer::start().await;
    let created = serde_json::from_str::<serde_json::Value>(REPOS).unwrap()["repos"][0].clone();
    Mock::given(method("POST"))
        .and(path("/api/app/v1/repos"))
        .and(body_json(
            json!({"owner":"DemiInfinity","name":"Ingle","private":true,"description":""}),
        ))
        .respond_with(ResponseTemplate::new(201).set_body_json(created))
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/repos"))
        .and(body_json(
            json!({"owner":"DemiInfinity","name":"Taken","private":true,"description":""}),
        ))
        .respond_with(raw(409, r#"{"error":"name_taken"}"#))
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/repos"))
        .and(body_json(
            json!({"owner":"someone","name":"Ingle","private":false,"description":"x"}),
        ))
        .respond_with(raw(
            403,
            r#"{"error":"forbidden","message":"You can’t create repositories under someone."}"#,
        ))
        .mount(&server)
        .await;

    let client = client(&server);
    let mut request = CreateRepo {
        owner: "DemiInfinity".into(),
        name: "Ingle".into(),
        private: true,
        description: String::new(),
    };
    let repo = client.create_repo(&request).await.unwrap();
    assert_eq!(repo.full_name, "DemiInfinity/Atelier");

    request.name = "Taken".into();
    let taken = client.create_repo(&request).await.unwrap_err();
    assert_eq!(taken.code(), Some("name_taken"));
    assert_eq!(taken.status(), Some(409));
    assert_eq!(taken.to_string(), "That name is already taken.");

    let forbidden = client
        .create_repo(&CreateRepo {
            owner: "someone".into(),
            name: "Ingle".into(),
            private: false,
            description: "x".into(),
        })
        .await
        .unwrap_err();
    assert_eq!(forbidden.status(), Some(403));
    assert_eq!(
        forbidden.to_string(),
        "You can’t create repositories under someone."
    );
}

#[tokio::test]
async fn repo_mirror() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/repos/mirror"))
        .and(body_json(json!({"github":"maralind/loopkit"})))
        .respond_with(raw(202, r#"{"full_name":"DemiInfinity/loopkit"}"#))
        .expect(1)
        .mount(&server)
        .await;
    let mirrored = client(&server)
        .mirror_repo("maralind/loopkit")
        .await
        .unwrap();
    assert_eq!(mirrored.full_name, "DemiInfinity/loopkit");
}

// -------------------------------------------------------------------- pulls

#[tokio::test]
async fn pulls_across_repos_and_per_repo() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/pulls"))
        .and(query_param("state", "open"))
        .respond_with(raw(200, PULL_SUMMARIES))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/repos/DemiInfinity/Atelier/pulls"))
        .and(query_param("state", "merged"))
        .respond_with(raw(200, r#"{"pulls":[]}"#))
        .expect(1)
        .mount(&server)
        .await;
    let client = client(&server);
    let pulls = client.pulls().await.unwrap();
    let pull = &pulls[0];
    assert_eq!(
        (pull.repo.as_str(), pull.number),
        ("DemiInfinity/Atelier", 14)
    );
    assert_eq!(pull.state, PullState::Open);
    assert_eq!(
        (pull.head.as_str(), pull.base.as_str()),
        ("feature/tunnel-ui", "main")
    );
    assert_eq!(
        pull.checks,
        CheckCounts {
            passed: 2,
            failed: 0,
            pending: 1,
            waiting: 1,
            ..CheckCounts::default()
        }
    );
    // An older studio sends no total and no state: both are worked out.
    assert_eq!(pull.checks.count(), 4);
    assert_eq!(pull.checks.summary(), ChecksState::Pending);
    assert!(client
        .repo_pulls("DemiInfinity", "Atelier", PullState::Merged)
        .await
        .unwrap()
        .is_empty());
}

#[tokio::test]
async fn pull_detail() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/repos/DemiInfinity/Atelier/pulls/14"))
        .and(header("authorization", bearer().as_str()))
        .respond_with(raw(200, PULL))
        .mount(&server)
        .await;
    let pull = client(&server)
        .pull("DemiInfinity", "Atelier", 14)
        .await
        .unwrap();
    assert_eq!(pull.title, "Remote access switch in the tray menu");
    assert_eq!(pull.author, "owner:DemiInfinity");
    assert_eq!((pull.commits, pull.files), (2, 3));
    assert_eq!(pull.via, None);
    assert!(!pull.merge.can_merge);
    assert_eq!(
        pull.merge.reason,
        "1 check is still running and 1 agent is waiting for you."
    );
    assert_eq!(
        pull.merge.methods,
        [MergeMethod::Squash, MergeMethod::Merge]
    );
    assert!(!pull.merge.on_github);
    assert_eq!(pull.merge.auto_merge, None);
    let statuses: Vec<CheckStatus> = pull.checks.iter().map(|c| c.status).collect();
    assert_eq!(
        statuses,
        [
            CheckStatus::Success,
            CheckStatus::Pending,
            CheckStatus::Waiting
        ]
    );
    assert_eq!(pull.checks[0].name, "ci · test");
    assert_eq!(pull.checks[0].detail, "MacBook · 1m 48s");
    assert_eq!(
        (pull.checks[0].run_id, pull.checks[0].job_id),
        (Some(207), Some(410))
    );
    assert_eq!(pull.checks[0].action, None);
    assert_eq!(pull.checks[2].action.as_deref(), Some("approve_agent"));
    assert_eq!(pull.comments[0].author, "CodeRabbit");
    assert_eq!(pull.comments[0].via, Some(Via::Tether));
    assert_eq!(pull.comments[0].kind, "bot");
    assert_eq!(pull.web_path, "/DemiInfinity/Atelier/pulls/14");
}

#[tokio::test]
async fn merge_now_when_green_refused_and_cancel() {
    let server = MockServer::start().await;
    let merge_path = "/api/app/v1/repos/DemiInfinity/Atelier/pulls/14/merge";
    Mock::given(method("POST"))
        .and(path(merge_path))
        .and(body_json(json!({"method":"squash","when_green":false})))
        .respond_with(raw(200, r#"{"merged":true,"sha":"…"}"#))
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path(merge_path))
        .and(body_json(json!({"method":"squash","when_green":true})))
        .respond_with(raw(
            202,
            r#"{"auto_merge":{"method":"squash","by":"device:MacBook"}}"#,
        ))
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path(merge_path))
        .and(body_json(json!({"method":"merge","when_green":false})))
        .respond_with(raw(
            409,
            r#"{"error":"not_mergeable","message":"1 check is still running and 1 agent is waiting for you."}"#,
        ))
        .mount(&server)
        .await;
    Mock::given(method("DELETE"))
        .and(path(merge_path))
        .and(header("authorization", bearer().as_str()))
        .respond_with(ResponseTemplate::new(204))
        .expect(1)
        .mount(&server)
        .await;

    let client = client(&server);
    let merge = |method, when_green| {
        client.merge_pull(
            "DemiInfinity",
            "Atelier",
            14,
            MergeRequest {
                method,
                when_green,
                delete_branch: None,
            },
        )
    };
    assert_eq!(
        merge(MergeMethod::Squash, false).await.unwrap(),
        MergeOutcome::Merged {
            merged: true,
            sha: "…".into()
        }
    );
    assert_eq!(
        merge(MergeMethod::Squash, true).await.unwrap(),
        MergeOutcome::Scheduled {
            auto_merge: AutoMerge {
                method: MergeMethod::Squash,
                by: "device:MacBook".into()
            }
        }
    );
    let refused = merge(MergeMethod::Merge, false).await.unwrap_err();
    assert_eq!(refused.status(), Some(409));
    assert_eq!(
        refused.to_string(),
        "1 check is still running and 1 agent is waiting for you."
    );
    client
        .cancel_auto_merge("DemiInfinity", "Atelier", 14)
        .await
        .unwrap();
}

// ---------------------------------------------------------- runs and agents

#[tokio::test]
async fn runs_with_filters() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/runs"))
        .and(query_param("limit", "20"))
        .and(query_param("runner", "MacBook"))
        .and(query_param("repo", "DemiInfinity/Atelier"))
        .respond_with(raw(200, RUNS))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/runs"))
        .and(query_param_is_missing("limit"))
        .and(query_param_is_missing("runner"))
        .and(query_param_is_missing("repo"))
        .respond_with(raw(200, r#"{"runs":null}"#))
        .expect(1)
        .mount(&server)
        .await;
    let client = client(&server);
    let runs = client
        .runs(&RunsQuery {
            limit: Some(20),
            runner: Some("MacBook".into()),
            repo: Some("DemiInfinity/Atelier".into()),
        })
        .await
        .unwrap();
    let run = &runs[0];
    assert_eq!(run.run_id, 207);
    assert_eq!((run.workflow.as_str(), run.event.as_str()), ("ci", "push"));
    assert_eq!(run.branch, "feature/tunnel-ui");
    assert_eq!(run.status, "success");
    assert_eq!(run.duration_s, Some(108));
    let job = &run.jobs[0];
    assert_eq!((job.job_id, job.name.as_str()), (410, "test"));
    assert_eq!((job.runner.as_str(), job.kind.as_str()), ("MacBook", "job"));
    assert_eq!(job.duration_s, Some(108));
    assert!(client.runs(&RunsQuery::default()).await.unwrap().is_empty());
}

#[tokio::test]
async fn job_log() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/jobs/410/log"))
        .and(query_param("tail", "200"))
        .respond_with(raw(200, LOG))
        .expect(1)
        .mount(&server)
        .await;
    let log = client(&server).job_log(410, Some(200)).await.unwrap();
    assert_eq!(log.lines, ["…"]);
    assert!(log.truncated);
    assert_eq!(log.status, "running");
}

#[tokio::test]
async fn agents_waiting_approve_and_cancel() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/agents/waiting"))
        .respond_with(raw(200, WAITING))
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/agents/jobs/31/approve"))
        .and(header("authorization", bearer().as_str()))
        .respond_with(raw(200, r#"{"approved":true}"#))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/jobs/412/cancel"))
        .respond_with(raw(200, r#"{"cancelled":true}"#))
        .expect(1)
        .mount(&server)
        .await;
    let client = client(&server);
    let waiting = client.agents_waiting().await.unwrap();
    let job = &waiting[0];
    assert_eq!((job.job_id, job.run_id), (31, 210));
    assert_eq!(job.workflow, "weekly-tidy");
    assert_eq!(job.reason, "first run");
    assert_eq!(job.engine, "claude-code");
    assert_eq!(job.tools, ["issues.list", "issues.comment", "issues.close"]);
    assert_eq!(job.write_tools, ["issues.comment", "issues.close"]);
    assert_eq!((job.max_turns, job.timeout_minutes), (Some(20), Some(10)));
    assert!(client.approve_agent(31).await.unwrap().approved);
    assert!(client.cancel_job(412).await.unwrap().cancelled);
}

// ------------------------------------------------------------------- remote

#[tokio::test]
async fn remote_get_set_who_link() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/remote"))
        .respond_with(raw(200, REMOTE))
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/remote"))
        .and(body_json(json!({"on": true})))
        .respond_with(raw(
            200,
            r#"{"on":true,"mode":"quick","address":"https://quiet-fox-lamp.trycloudflare.com","exposed":["web","git"],"connected":0}"#,
        ))
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/remote"))
        .and(body_json(json!({"on": false})))
        .respond_with(raw(200, REMOTE))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/remote/who"))
        .respond_with(raw(200, WHO))
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/remote/link"))
        .respond_with(raw(201, LINK))
        .mount(&server)
        .await;

    let client = client(&server);
    let remote = client.remote().await.unwrap();
    assert!(!remote.on);
    assert_eq!(remote.mode, "quick");
    assert_eq!(remote.address, None);
    assert_eq!(remote.exposed, ["web", "git"]);
    assert_eq!(remote.connected, 0);

    let on = client.set_remote(true).await.unwrap();
    assert!(on.on);
    assert_eq!(
        on.address.as_deref(),
        Some("https://quiet-fox-lamp.trycloudflare.com")
    );
    assert!(!client.set_remote(false).await.unwrap().on);

    let who = client.remote_who().await.unwrap();
    assert_eq!(who[0].ip, "203.0.113.42");
    assert_eq!(who[0].country, "NL");
    assert!(who[0].signed_in);
    assert_eq!(who[0].class, "web");

    let link = client.remote_link().await.unwrap();
    assert_eq!(link.url, "https://<tunnel host>/…");
    assert_eq!(link.expires_in, 900);
}

#[tokio::test]
async fn remote_on_through_the_tunnel_is_refused() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/remote"))
        .respond_with(raw(403, r#"{"error":"remote_only_off"}"#))
        .mount(&server)
        .await;
    let err = client(&server).set_remote(true).await.unwrap_err();
    assert_eq!(err.code(), Some("remote_only_off"));
    assert_eq!(err.status(), Some(403));
}

// ----------------------------------------------------------------- messages

#[tokio::test]
async fn messages_get_and_put() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/messages"))
        .respond_with(raw(200, MESSAGES))
        .mount(&server)
        .await;
    Mock::given(method("PUT"))
        .and(path("/api/app/v1/messages"))
        .and(header("authorization", bearer().as_str()))
        .and(body_json(json!({"tone":"plain","yours":{"push":["…"]}})))
        .respond_with(raw(200, MESSAGES))
        .expect(1)
        .mount(&server)
        .await;

    let client = client(&server);
    let messages = client.messages().await.unwrap();
    assert_eq!(messages.tone, Tone::Friendly);
    assert_eq!(messages.moments.len(), 9);
    assert_eq!(messages.moments["push"].ours.len(), 3);
    assert_eq!(messages.moments["push"].yours, ["Shipped. Go outside."]);
    assert!(messages.moments["clean"].yours.is_empty());
    assert_eq!(messages.plain["merge"], "Merged into {base}.");
    assert_eq!(
        messages.rules,
        MessageRules {
            quiet_fullscreen: true,
            terminal: true,
            milestones: true
        }
    );

    let update = MessagesUpdate {
        tone: Some(Tone::Plain),
        yours: Some([("push".to_string(), vec!["…".to_string()])].into()),
        rules: None,
    };
    assert_eq!(client.put_messages(&update).await.unwrap(), messages);
}

#[tokio::test]
async fn messages_put_rules_only() {
    let server = MockServer::start().await;
    Mock::given(method("PUT"))
        .and(path("/api/app/v1/messages"))
        .and(body_json(json!({
            "rules": {"quiet_fullscreen": true, "terminal": false, "milestones": true}
        })))
        .respond_with(raw(200, MESSAGES))
        .expect(1)
        .mount(&server)
        .await;
    let update = MessagesUpdate {
        rules: Some(MessageRules {
            terminal: false,
            ..MessageRules::default()
        }),
        ..MessagesUpdate::default()
    };
    client(&server).put_messages(&update).await.unwrap();
}

// ------------------------------------------------- errors and compatibility

#[tokio::test]
async fn errors_carry_the_servers_code_and_sentence() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/me"))
        .respond_with(raw(401, r#"{"error":"revoked"}"#))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/status"))
        .respond_with(raw(
            500,
            r#"{"error":"internal","message":"The database is busy."}"#,
        ))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/remote"))
        .respond_with(ResponseTemplate::new(502).set_body_string("<html>bad gateway</html>"))
        .mount(&server)
        .await;
    let client = client(&server);

    let revoked = client.me().await.unwrap_err();
    assert!(matches!(&revoked, Error::Api { status: 401, code, .. } if code == "revoked"));
    assert!(revoked.needs_sign_in());
    assert_eq!(
        revoked.to_string(),
        "This device was signed out of your studio."
    );

    let busy = client.status().await.unwrap_err();
    assert_eq!(busy.code(), Some("internal"));
    assert_eq!(busy.to_string(), "The database is busy.");
    assert!(!busy.needs_sign_in());

    let gateway = client.remote().await.unwrap_err();
    assert_eq!(gateway.status(), Some(502));
    assert_eq!(gateway.code(), Some("http_502"));
}

#[tokio::test]
async fn calls_without_a_token_fail_before_the_network() {
    let server = MockServer::start().await;
    let err = anonymous(&server).me().await.unwrap_err();
    assert!(matches!(err, Error::NotSignedIn));
    assert!(err.needs_sign_in());
    assert!(server.received_requests().await.unwrap().is_empty());
}

#[tokio::test]
async fn an_unreachable_studio_is_a_network_error() {
    // A port nothing listens on.
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    drop(listener);
    let client = atelier_core::Client::new(
        &format!("http://{address}"),
        Some(atelier_core::Token::new(common::TOKEN)),
        atelier_core::ClientOptions::default(),
    )
    .unwrap();
    let err = client.me().await.unwrap_err();
    assert!(matches!(err, Error::Network(_)), "{err:?}");
    assert!(!err.to_string().contains(common::TOKEN));
}

#[tokio::test]
async fn unknown_fields_and_values_are_tolerated() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/status"))
        .respond_with(raw(
            200,
            r#"{"needs":[{"id":"x:1","kind":"something_new","thread":"somewhere_new","title":"New thing",
                 "action":{"label":"Do","type":"do_new","target":{"deep":[1,2,3]}},"priority":9}],
                "running":[{"job_id":1,"run_id":2,"gpu":true}],
                "feed":null,"unread":0,"weather":{"today":"rain"}}"#,
        ))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/repos/a/b/pulls/1"))
        .respond_with(raw(
            200,
            r#"{"number":1,"state":"draft","via":"carrier-pigeon","labels":["x"],
                "merge":{"methods":["squash","fast-forward"],"queue":{"position":2}},
                "checks":[{"name":"c","status":"neutral","extra":1}],
                "comments":[{"author":"a","via":"smoke-signal"}]}"#,
        ))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/messages"))
        .respond_with(raw(
            200,
            r#"{"tone":"sarcastic","moments":{"brand_new":{"ours":["Hi."],"theirs":["x"]}},"voice":"alto"}"#,
        ))
        .mount(&server)
        .await;
    let client = client(&server);

    let status = client.status().await.unwrap();
    assert_eq!(status.needs[0].kind, NeedKind::Unknown);
    assert_eq!(status.needs[0].thread, Thread::Unknown);
    assert_eq!(status.needs[0].action.as_ref().unwrap().kind, "do_new");
    assert_eq!(status.running[0].job_id, 1);
    assert!(status.feed.is_empty());

    let pull = client.pull("a", "b", 1).await.unwrap();
    assert_eq!(pull.state, PullState::Unknown);
    assert_eq!(pull.via, Some(Via::Unknown));
    assert_eq!(
        pull.merge.methods,
        [MergeMethod::Squash, MergeMethod::Unknown]
    );
    assert_eq!(pull.checks[0].status, CheckStatus::Unknown);
    assert_eq!(pull.comments[0].via, Some(Via::Unknown));

    let messages = client.messages().await.unwrap();
    assert_eq!(messages.tone, Tone::Unknown);
    assert_eq!(messages.moments["brand_new"].ours, ["Hi."]);
}

#[tokio::test]
async fn missing_optional_fields_do_not_fail() {
    let server = MockServer::start().await;
    for (route, body) in [
        ("/api/app/v1/me", "{}"),
        ("/api/app/v1/status", "{}"),
        (
            "/api/app/v1/repos",
            r#"{"repos":[{"full_name":"a/b","github":null,"language":null}]}"#,
        ),
        ("/api/app/v1/pulls", r#"{"pulls":[{"number":3}]}"#),
        ("/api/app/v1/repos/a/b/pulls/3", r#"{"number":3}"#),
        (
            "/api/app/v1/runs",
            r#"{"runs":[{"run_id":1,"duration_s":null,"jobs":null}]}"#,
        ),
        ("/api/app/v1/jobs/1/log", "{}"),
        ("/api/app/v1/agents/waiting", r#"{"jobs":[{"job_id":1}]}"#),
        ("/api/app/v1/remote", "{}"),
        ("/api/app/v1/remote/who", "{}"),
        ("/api/app/v1/messages", "{}"),
    ] {
        Mock::given(method("GET"))
            .and(path(route))
            .respond_with(raw(200, body))
            .mount(&server)
            .await;
    }
    let client = client(&server);
    let me = client.me().await.unwrap();
    assert!(me.accounts.is_empty() && me.device.is_none() && me.studio.ssh.is_none());
    assert_eq!(client.status().await.unwrap(), Status::default());
    let repos = client.repos().await.unwrap();
    assert_eq!(repos[0].full_name, "a/b");
    assert!(repos[0].github.is_none() && repos[0].language.is_none());
    assert_eq!(
        client.pulls().await.unwrap()[0].checks,
        CheckCounts::default()
    );
    let pull = client.pull("a", "b", 3).await.unwrap();
    assert!(pull.checks.is_empty() && pull.comments.is_empty());
    let runs = client.runs(&RunsQuery::default()).await.unwrap();
    assert!(runs[0].jobs.is_empty() && runs[0].duration_s.is_none());
    assert!(client.job_log(1, None).await.unwrap().lines.is_empty());
    let waiting = client.agents_waiting().await.unwrap();
    assert!(waiting[0].tools.is_empty() && waiting[0].max_turns.is_none());
    assert_eq!(client.remote().await.unwrap(), Remote::default());
    assert!(client.remote_who().await.unwrap().is_empty());
    let messages = client.messages().await.unwrap();
    assert_eq!(messages.tone, Tone::Friendly);
    assert!(messages.rules.terminal);
}

#[tokio::test]
async fn path_segments_are_escaped_and_a_base_path_is_kept() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/forge/api/app/v1/repos/My%20Org/a%2Fb/pulls/2"))
        .respond_with(raw(200, r#"{"number":2}"#))
        .expect(1)
        .mount(&server)
        .await;
    let client = atelier_core::Client::new(
        &format!("{}/forge/", server.uri()),
        Some(atelier_core::Token::new(common::TOKEN)),
        atelier_core::ClientOptions::default(),
    )
    .unwrap();
    assert_eq!(client.pull("My Org", "a/b", 2).await.unwrap().number, 2);
    assert_eq!(
        client.web_url("/x/y"),
        format!("{}/forge/x/y", server.uri())
    );
}

// ------------------------------------------------ the server as it was built

/// "Notes on sections 1–9 as built": nulls where the contract shows values,
/// the extra fields, and the wider sets of values.
#[tokio::test]
async fn answers_as_the_server_was_built() {
    let server = MockServer::start().await;
    for (route, body) in [
        (
            "/api/app/v1/me",
            r#"{"person":{"name":"Demi","login":"DemiInfinity","avatar_url":null},
                "accounts":[],"studio":{"name":"Atelier","url":"https://s","version":"dev build 12","ssh":null,"tether":false,"connect":false},
                "device":null}"#,
        ),
        (
            "/api/app/v1/status",
            r#"{"needs":[{"id":"security:DemiInfinity/Tray:deps","kind":"security_alert","thread":"yours","title":"1 advisory",
                          "detail":"","repo":"DemiInfinity/Tray","web_path":"/DemiInfinity/Tray/security","at":"2026-11-11T09:00:00Z",
                          "action":{"label":"Open","type":"open_web"}}],
                "running":[{"job_id":5,"run_id":900,"repo":"a/b","workflow":"ci","job":"test","kind":"job","engine":null,
                            "runner":"MacBook","started_at":"2026-11-11T09:14:02Z","detail":"push on main","web_path":"/a/b/actions/runs/3"}],
                "feed":[{"thread":"connect","text":"Mara starred Atelier","at":"2026-11-11T08:00:00Z","web_path":null,"url":"https://mara.example/x"}],
                "unread":1}"#,
        ),
        (
            "/api/app/v1/repos",
            r#"{"repos":[{"full_name":"a/b","owner":"a","name":"b","language":null,"language_color":null,"clone_ssh":null,
                          "clone_https":"https://s/a/b.git","github":null}],
                "github_hits":[],"github_error":"Tether isn’t connected."}"#,
        ),
        (
            "/api/app/v1/repos/a/b/pulls/1",
            r#"{"number":1,"state":"open","updated_at":"2026-11-11T09:00:00Z",
                "merge":{"can_merge":true,"methods":["squash","merge","rebase"],"green":true},
                "checks":[{"name":"CodeRabbit","status":"success","run_id":null,"job_id":null,"required":false},
                          {"name":"ci","status":"skipped","run_id":4,"job_id":5,"required":true}],
                "comments":[{"author":"review-helper","via":null,"kind":"agent","body":"ok","at":"…"}]}"#,
        ),
        (
            "/api/app/v1/runs",
            r#"{"runs":[{"run_id":900,"number":3,"web_path":"/a/b/actions/runs/3","repo":"a/b","workflow":"ci","status":"queued",
                         "started_at":null,"duration_s":null,
                         "jobs":[{"job_id":5,"name":"test","status":"skipped","runner":null,"duration_s":null,"kind":"job"}]}]}"#,
        ),
        (
            "/api/app/v1/agents/waiting",
            r#"{"jobs":[{"job_id":31,"run_id":210,"repo":"a/b","workflow":"weekly-tidy","reason":"tools changed",
                         "detail":"wants issues.close","web_path":"/a/b/actions/runs/31","engine":"claude-code","tools":[],"write_tools":[]}]}"#,
        ),
        (
            "/api/app/v1/remote/who",
            r#"{"sessions":[{"ip":"203.0.113.42","country":null,"signed_in":false,"class":"git","since":"2026-11-11T09:00:00Z"}]}"#,
        ),
        (
            "/api/app/v1/remote",
            r#"{"on":true,"mode":"quick","address":null,"exposed":["web","git","mcp","pages","api","connect"],"connected":0}"#,
        ),
    ] {
        Mock::given(method("GET"))
            .and(path(route))
            .respond_with(raw(200, body))
            .mount(&server)
            .await;
    }
    let client = client(&server);

    let me = client.me().await.unwrap();
    assert!(me.person.avatar_url.is_none() && me.studio.ssh.is_none() && me.device.is_none());
    assert_eq!(me.studio.version, "dev build 12");

    let status = client.status().await.unwrap();
    let need = &status.needs[0];
    assert_eq!(need.id, "security:DemiInfinity/Tray:deps");
    assert_eq!(need.kind, NeedKind::SecurityAlert);
    assert_eq!(need.at.as_deref(), Some("2026-11-11T09:00:00Z"));
    let action = need.action.as_ref().unwrap();
    assert_eq!(action.kind, "open_web");
    assert_eq!(
        (action.job_id, action.run_id, action.number),
        (None, None, None)
    );
    assert_eq!(status.running[0].engine, None);
    assert_eq!(status.running[0].detail, "push on main");
    assert_eq!(
        status.running[0].web_path.as_deref(),
        Some("/a/b/actions/runs/3")
    );
    assert_eq!(status.feed[0].web_path, "");
    assert_eq!(
        status.feed[0].url.as_deref(),
        Some("https://mara.example/x")
    );

    let found = client.search_repos("", false).await.unwrap();
    assert_eq!(
        found.github_error.as_deref(),
        Some("Tether isn’t connected.")
    );
    let repo = &found.repos[0];
    assert!(repo.language.is_none() && repo.language_color.is_none());
    assert_eq!(repo.clone_ssh, "");

    let pull = client.pull("a", "b", 1).await.unwrap();
    assert_eq!(pull.updated_at.as_deref(), Some("2026-11-11T09:00:00Z"));
    assert_eq!(pull.merge.green, Some(true));
    assert_eq!(
        pull.merge.methods,
        [MergeMethod::Squash, MergeMethod::Merge, MergeMethod::Rebase]
    );
    assert_eq!((pull.checks[0].run_id, pull.checks[0].job_id), (None, None));
    assert_eq!(pull.checks[0].required, Some(false));
    assert_eq!(pull.checks[1].status, CheckStatus::Skipped);
    assert_eq!(pull.comments[0].kind, "agent");

    let runs = client.runs(&RunsQuery::default()).await.unwrap();
    assert_eq!(
        (runs[0].number, runs[0].status.as_str()),
        (Some(3), "queued")
    );
    assert_eq!(runs[0].started_at, "");
    assert_eq!(runs[0].web_path.as_deref(), Some("/a/b/actions/runs/3"));
    assert_eq!(
        (
            runs[0].jobs[0].status.as_str(),
            runs[0].jobs[0].runner.as_str()
        ),
        ("skipped", "")
    );

    let waiting = client.agents_waiting().await.unwrap();
    assert_eq!(waiting[0].reason, "tools changed");
    assert_eq!(waiting[0].detail.as_deref(), Some("wants issues.close"));

    assert_eq!(client.remote_who().await.unwrap()[0].country, "");
    assert_eq!(client.remote().await.unwrap().exposed.len(), 6);
}

#[tokio::test]
async fn merge_and_cancel_as_built() {
    let server = MockServer::start().await;
    let merge_path = "/api/app/v1/repos/a/b/pulls/1/merge";
    Mock::given(method("POST"))
        .and(path(merge_path))
        .and(body_json(
            json!({"method":"rebase","when_green":false,"delete_branch":true}),
        ))
        .respond_with(raw(200, r#"{"merged":true,"sha":"abc"}"#))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("DELETE"))
        .and(path(merge_path))
        .respond_with(raw(200, r#"{"cancelled":true}"#))
        .up_to_n_times(1)
        .mount(&server)
        .await;
    Mock::given(method("DELETE"))
        .and(path(merge_path))
        .respond_with(raw(404, r#"{"error":"no_auto_merge"}"#))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/jobs/1/log"))
        .and(query_param("tail", "2000"))
        .respond_with(raw(200, "{}"))
        .expect(1)
        .mount(&server)
        .await;
    let client = client(&server);
    let request = MergeRequest {
        method: MergeMethod::Rebase,
        when_green: false,
        delete_branch: Some(true),
    };
    assert!(matches!(
        client.merge_pull("a", "b", 1, request).await.unwrap(),
        MergeOutcome::Merged { merged: true, .. }
    ));
    client.cancel_auto_merge("a", "b", 1).await.unwrap();
    let none = client.cancel_auto_merge("a", "b", 1).await.unwrap_err();
    assert_eq!(
        (none.status(), none.code()),
        (Some(404), Some("no_auto_merge"))
    );
    assert_eq!(
        none.to_string(),
        "That pull request wasn’t waiting to merge."
    );
    // The log tail is capped at the server's 2000.
    client.job_log(1, Some(50_000)).await.unwrap();
}

#[tokio::test]
async fn repo_rename_and_its_refusals() {
    let server = MockServer::start().await;
    let mut renamed = serde_json::from_str::<serde_json::Value>(REPOS).unwrap()["repos"][0].clone();
    renamed["name"] = json!("Studio");
    renamed["full_name"] = json!("DemiInfinity/Studio");
    renamed["clone_https"] = json!("https://studio.example/DemiInfinity/Studio.git");
    Mock::given(method("PATCH"))
        .and(path("/api/app/v1/repos/DemiInfinity/Atelier"))
        .and(header("authorization", bearer().as_str()))
        .and(body_json(json!({"name": "Studio"})))
        .respond_with(ResponseTemplate::new(200).set_body_json(renamed))
        .expect(1)
        .mount(&server)
        .await;
    for (name, status, body) in [
        ("Tray", 409, r#"{"error":"name_taken"}"#),
        ("no good", 422, r#"{"error":"invalid_name"}"#),
        ("Nope", 403, r#"{"error":"forbidden"}"#),
    ] {
        Mock::given(method("PATCH"))
            .and(path("/api/app/v1/repos/DemiInfinity/Atelier"))
            .and(body_json(json!({"name": name})))
            .respond_with(raw(status, body))
            .mount(&server)
            .await;
    }
    let client = client(&server);
    let repo = client
        .rename_repo("DemiInfinity", "Atelier", "Studio")
        .await
        .unwrap();
    assert_eq!(repo.full_name, "DemiInfinity/Studio");
    assert_eq!(repo.name, "Studio");
    assert_eq!(
        repo.clone_https,
        "https://studio.example/DemiInfinity/Studio.git"
    );

    let taken = client
        .rename_repo("DemiInfinity", "Atelier", "Tray")
        .await
        .unwrap_err();
    assert_eq!(
        (taken.status(), taken.code()),
        (Some(409), Some("name_taken"))
    );
    assert_eq!(taken.to_string(), "That name is already taken.");
    let invalid = client
        .rename_repo("DemiInfinity", "Atelier", "no good")
        .await
        .unwrap_err();
    assert_eq!(invalid.status(), Some(422));
    assert_eq!(
        invalid.to_string(),
        "That isn’t a name a repository can have."
    );
    let forbidden = client
        .rename_repo("DemiInfinity", "Atelier", "Nope")
        .await
        .unwrap_err();
    assert_eq!(forbidden.status(), Some(403));
}

#[tokio::test]
async fn repo_delete_sends_the_full_name_to_confirm() {
    let server = MockServer::start().await;
    Mock::given(method("DELETE"))
        .and(path("/api/app/v1/repos/DemiInfinity/Atelier"))
        .and(header("authorization", bearer().as_str()))
        .and(body_json(json!({"confirm": "DemiInfinity/Atelier"})))
        .respond_with(ResponseTemplate::new(204))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("DELETE"))
        .and(path("/api/app/v1/repos/DemiInfinity/Other"))
        .respond_with(raw(
            422,
            r#"{"error":"confirm_mismatch","message":"Type the repository’s full name to delete it."}"#,
        ))
        .mount(&server)
        .await;
    let client = client(&server);
    client.delete_repo("DemiInfinity", "Atelier").await.unwrap();
    let refused = client
        .delete_repo("DemiInfinity", "Other")
        .await
        .unwrap_err();
    assert_eq!(refused.status(), Some(422));
    assert_eq!(
        refused.to_string(),
        "Type the repository’s full name to delete it."
    );
}

#[tokio::test]
async fn pulls_of_one_repo_with_the_checks_summary() {
    let server = MockServer::start().await;
    // A studio that doesn't know `repo` yet answers with everything.
    Mock::given(method("GET"))
        .and(path("/api/app/v1/pulls"))
        .and(query_param("state", "open"))
        .and(query_param("repo", "demiinfinity/atelier"))
        .respond_with(raw(
            200,
            r#"{"pulls":[
              {"repo":"DemiInfinity/Atelier","number":14,"title":"A","state":"open",
               "checks":{"total":4,"passed":3,"failed":0,"pending":1,"state":"pending"}},
              {"repo":"DemiInfinity/Atelier","number":15,"title":"B","state":"open",
               "checks":{"total":2,"passed":1,"failed":1,"pending":0,"state":"failing"}},
              {"repo":"DemiInfinity/Atelier","number":16,"title":"C","state":"open",
               "checks":{"total":0,"passed":0,"failed":0,"pending":0,"state":"none"}},
              {"repo":"DemiInfinity/Atelier","number":17,"title":"D","state":"open",
               "checks":{"total":2,"passed":2,"failed":0,"pending":0,"state":"some_new_word"}},
              {"repo":"DemiInfinity/Atelier","number":18,"title":"E","state":"open"},
              {"repo":"DemiInfinity/Tray","number":9,"title":"elsewhere","state":"open",
               "checks":{"total":2,"passed":2,"failed":0,"pending":0,"state":"passing"}}]}"#,
        ))
        .expect(1)
        .mount(&server)
        .await;
    let pulls = client(&server)
        .pulls_in("demiinfinity/atelier")
        .await
        .unwrap();
    assert_eq!(
        pulls.iter().map(|p| p.number).collect::<Vec<_>>(),
        [14, 15, 16, 17, 18]
    );
    assert_eq!(pulls[0].checks.total, Some(4));
    assert_eq!(pulls[0].checks.state, Some(ChecksState::Pending));
    assert_eq!(pulls[0].checks.summary(), ChecksState::Pending);
    assert_eq!(pulls[1].checks.summary(), ChecksState::Failing);
    assert_eq!(pulls[2].checks.summary(), ChecksState::None);
    // A state this version doesn't know falls back to the counts.
    assert_eq!(pulls[3].checks.state, Some(ChecksState::Unknown));
    assert_eq!(pulls[3].checks.summary(), ChecksState::Passing);
    // No `checks` at all (an older studio, or a hiccup): no checks.
    assert_eq!(pulls[4].checks, CheckCounts::default());
    assert_eq!(pulls[4].checks.summary(), ChecksState::None);
}

#[test]
fn owners_are_optional_and_join_the_accounts() {
    let me: Me = serde_json::from_str(ME).unwrap();
    assert!(me.owners.is_empty());
    assert_eq!(
        me.all_owners()
            .iter()
            .map(|a| a.name.as_str())
            .collect::<Vec<_>>(),
        ["DemiInfinity", "elyndrastudios"]
    );
    let mut with: serde_json::Value = serde_json::from_str(ME).unwrap();
    with["owners"] = json!([
        {"name": "demiinfinity", "display_name": "Demi", "kind": "personal"},
        {"name": "elyndrastudios", "display_name": "Elyndra Studios LTD", "kind": "company"},
        {"name": "sidequest", "display_name": "Side Quest", "kind": "company"}
    ]);
    let me: Me = serde_json::from_value(with).unwrap();
    let all = me.all_owners();
    assert_eq!(
        all.iter()
            .map(|a| (a.name.as_str(), a.display.as_str(), a.kind.as_str()))
            .collect::<Vec<_>>(),
        [
            ("DemiInfinity", "Demi", "user"),
            ("elyndrastudios", "Elyndra Studios LTD", "company"),
            ("sidequest", "Side Quest", "company")
        ]
    );
    assert_eq!(all[2].commit_email, "demi@example.com");
    // Only `owners` (no accounts): the personal one still comes first.
    let me: Me = serde_json::from_value(json!({"owners": [
        {"name": "co", "display_name": "Co", "kind": "company"},
        {"name": "me", "display_name": "", "kind": "personal"}]}))
    .unwrap();
    assert_eq!(
        me.all_owners()
            .iter()
            .map(|a| (a.name.as_str(), a.display.as_str(), a.kind.as_str()))
            .collect::<Vec<_>>(),
        [("me", "me", "user"), ("co", "Co", "company")]
    );
}

#[tokio::test]
async fn issues_list_read_open_comment_and_close() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/issues"))
        .and(query_param("repo", "DemiInfinity/Atelier"))
        .and(query_param("state", "closed"))
        .and(query_param("page", "2"))
        .and(query_param("per_page", "30"))
        .respond_with(raw(
            200,
            r##"{"issues":[{"repo":"DemiInfinity/Atelier","number":7,"title":"Tray icon is blurry","state":"closed",
              "author":"owner:DemiInfinity","via":"tether","labels":[{"name":"bug","color":"#d73a4a"},{"name":"tray","color":null}],
              "comments":3,"web_path":"/DemiInfinity/Atelier/issues/7","created_at":"2026-11-01T10:00:00Z","updated_at":"2026-11-02T10:00:00Z",
              "web_url":"https://studio.example/DemiInfinity/Atelier/issues/7","later":1},
              {"number":8,"labels":null}],"has_more":true}"##,
        ))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/issues"))
        .and(query_param("repo", "DemiInfinity/Atelier"))
        .and(query_param_is_missing("state"))
        .and(query_param_is_missing("page"))
        .respond_with(raw(200, r#"{"issues":null}"#))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/repos/DemiInfinity/Atelier/issues/7"))
        .respond_with(raw(
            200,
            r##"{"repo":"DemiInfinity/Atelier","number":7,"title":"Tray icon is blurry","state":"open",
              "author":"DemiInfinity","labels":[{"name":"bug","color":"#d73a4a"}],
              "body":"On a **retina** screen.\n<script>alert(1)</script>",
              "comments":[{"id":88,"author":"CodeRabbit[bot]","via":"tether","kind":"bot","body":"Same here.","created_at":"2026-11-01T11:00:00Z"}],
              "created_at":"2026-11-01T10:00:00Z","updated_at":"2026-11-02T10:00:00Z",
              "web_url":"https://studio.example/DemiInfinity/Atelier/issues/7"}"##,
        ))
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/repos/DemiInfinity/Atelier/issues"))
        .and(header("authorization", bearer().as_str()))
        .and(body_json(json!({"title": "New thing", "body": "Please."})))
        .respond_with(raw(
            201,
            r#"{"number":9,"title":"New thing","state":"open","comments":0}"#,
        ))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path(
            "/api/app/v1/repos/DemiInfinity/Atelier/issues/7/comments",
        ))
        .and(body_json(json!({"body": "Fixed in main."})))
        .respond_with(ResponseTemplate::new(201))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("PATCH"))
        .and(path("/api/app/v1/repos/DemiInfinity/Atelier/issues/7"))
        .and(body_json(json!({"state": "closed"})))
        .respond_with(raw(200, r#"{"number":7,"state":"closed"}"#))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("PATCH"))
        .and(path("/api/app/v1/repos/DemiInfinity/Atelier/issues/7"))
        .and(body_json(json!({"state": "open"})))
        .respond_with(raw(200, "{}"))
        .expect(1)
        .mount(&server)
        .await;

    let client = client(&server);
    let list = client
        .issues(&IssuesQuery {
            repo: "DemiInfinity/Atelier".into(),
            state: "closed".into(),
            page: Some(2),
            per_page: Some(30),
        })
        .await
        .unwrap();
    assert!(list.has_more);
    let issue = &list.issues[0];
    assert_eq!((issue.number, issue.comments), (7, 3));
    assert_eq!(issue.state, "closed");
    assert_eq!(issue.via, Some(Via::Tether));
    assert_eq!(issue.web_path, "/DemiInfinity/Atelier/issues/7");
    assert_eq!(list.issues[1].via, None);
    assert_eq!(
        issue.labels,
        [
            Label {
                name: "bug".into(),
                color: "#d73a4a".into()
            },
            Label {
                name: "tray".into(),
                color: String::new()
            }
        ]
    );
    assert_eq!(
        issue.web_url,
        "https://studio.example/DemiInfinity/Atelier/issues/7"
    );
    assert_eq!(list.issues[1].number, 8);
    assert!(list.issues[1].labels.is_empty());

    let empty = client
        .issues(&IssuesQuery {
            repo: "DemiInfinity/Atelier".into(),
            ..IssuesQuery::default()
        })
        .await
        .unwrap();
    assert!(empty.issues.is_empty() && !empty.has_more);

    let full = client.issue("DemiInfinity", "Atelier", 7).await.unwrap();
    assert!(full.body.contains("<script>"), "the body is kept as text");
    assert_eq!(full.comments.len(), 1);
    let said = &full.comments[0];
    assert_eq!(
        (said.id, said.author.as_str(), said.kind.as_str(), said.via),
        (88, "CodeRabbit[bot]", "bot", Some(Via::Tether))
    );

    let made = client
        .create_issue("DemiInfinity", "Atelier", "New thing", "Please.")
        .await
        .unwrap();
    // The answer carries a count where the detail has a list: read as none.
    assert_eq!((made.number, made.comments.len()), (9, 0));
    client
        .comment_on_issue("DemiInfinity", "Atelier", 7, "Fixed in main.")
        .await
        .unwrap();
    client
        .set_issue_open("DemiInfinity", "Atelier", 7, false)
        .await
        .unwrap();
    client
        .set_issue_open("DemiInfinity", "Atelier", 7, true)
        .await
        .unwrap();
}

#[test]
fn the_device_says_whether_it_may_administer() {
    let me: Me = serde_json::from_str(ME).unwrap();
    let device = me.device.unwrap();
    assert_eq!(device.can_admin, None);
    assert!(!device.may_admin(), "older studios don't say: no");
    // What wasn't sent isn't made up when the answer is passed on.
    assert!(serde_json::to_value(&device)
        .unwrap()
        .get("can_admin")
        .is_none());
    let me: Me = serde_json::from_value(
        json!({"device": {"id": 7, "name": "MacBook", "kind": "desktop", "can_admin": true}}),
    )
    .unwrap();
    assert!(me.device.unwrap().may_admin());
    for off in [json!(false), json!(null)] {
        let me: Me =
            serde_json::from_value(json!({"device": {"id": 7, "can_admin": off}})).unwrap();
        assert!(!me.device.unwrap().may_admin());
    }
    let me: Me = serde_json::from_value(json!({"device": null})).unwrap();
    assert_eq!(me.device, None);
}

#[tokio::test]
async fn update_pull_updated_current_conflicts_and_refusals() {
    let server = MockServer::start().await;
    let at = |n: u64| format!("/api/app/v1/repos/DemiInfinity/Atelier/pulls/{n}/update");
    for (n, status, body) in [
        (14, 200, r#"{"updated":true,"head_sha":"4f2a9c1e"}"#),
        (15, 200, r#"{"updated":false,"head_sha":"77aa01bc"}"#),
        (
            16,
            409,
            r#"{"error":"conflicts","message":"main and feature/tray both changed 2 files.","files":["cmd/app/main.go","README.md"]}"#,
        ),
        (
            17,
            422,
            r#"{"error":"not_open","message":"This pull request is already merged."}"#,
        ),
        (18, 403, r#"{"error":"forbidden"}"#),
        // Leniently: no flag, the sha under another name, files as null.
        (19, 200, r#"{"sha":"abc1234","later":1}"#),
        (20, 409, r#"{"error":"conflicts","files":null}"#),
        (21, 409, r#"{"error":"busy","message":"Try again."}"#),
        (22, 200, ""),
    ] {
        Mock::given(method("POST"))
            .and(path(at(n)))
            .and(header("authorization", bearer().as_str()))
            .respond_with(raw(status, body))
            .mount(&server)
            .await;
    }
    let client = client(&server);
    let update = |n| client.update_pull("DemiInfinity", "Atelier", n);

    assert_eq!(
        update(14).await.unwrap(),
        UpdateOutcome::Updated {
            head_sha: "4f2a9c1e".into()
        }
    );
    assert_eq!(
        update(15).await.unwrap(),
        UpdateOutcome::UpToDate {
            head_sha: "77aa01bc".into()
        }
    );
    assert_eq!(
        update(16).await.unwrap(),
        UpdateOutcome::Conflicts {
            message: "main and feature/tray both changed 2 files.".into(),
            files: vec!["cmd/app/main.go".into(), "README.md".into()],
        }
    );
    let closed = update(17).await.unwrap_err();
    assert_eq!(
        (closed.status(), closed.code()),
        (Some(422), Some("not_open"))
    );
    assert_eq!(closed.to_string(), "This pull request is already merged.");
    let forbidden = update(18).await.unwrap_err();
    assert_eq!(
        (forbidden.status(), forbidden.code()),
        (Some(403), Some("forbidden"))
    );
    assert_eq!(
        forbidden.to_string(),
        "This device isn’t allowed to do that."
    );
    assert_eq!(
        update(19).await.unwrap(),
        UpdateOutcome::Updated {
            head_sha: "abc1234".into()
        }
    );
    assert_eq!(
        update(20).await.unwrap(),
        UpdateOutcome::Conflicts {
            message: "This branch conflicts with its base.".into(),
            files: vec![],
        }
    );
    // A push that landed meanwhile, and the other refusals, by their codes.
    for (n, status, code) in [
        (23, 409, "branch_moved"),
        (24, 422, "on_github"),
        (25, 403, "branch_protected"),
        (26, 422, "branch_missing"),
        (27, 422, "unrelated"),
    ] {
        Mock::given(method("POST"))
            .and(path(at(n)))
            .respond_with(raw(status, &format!(r#"{{"error":"{code}"}}"#)))
            .mount(&server)
            .await;
        let refused = update(n).await.unwrap_err();
        assert_eq!(
            (refused.status(), refused.code()),
            (Some(status), Some(code))
        );
        assert!(!refused.to_string().contains("HTTP"), "{refused}");
    }
    // Another 409 stays an error.
    let busy = update(21).await.unwrap_err();
    assert_eq!((busy.status(), busy.code()), (Some(409), Some("busy")));
    assert_eq!(
        update(22).await.unwrap(),
        UpdateOutcome::Updated {
            head_sha: String::new()
        }
    );

    // The request carries no body.
    let sent = server.received_requests().await.unwrap();
    assert!(sent.iter().all(|r| r.body.is_empty()));
}

#[tokio::test]
async fn pulls_say_how_far_behind_they_are_and_whether_they_conflict() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/pulls"))
        .respond_with(raw(
            200,
            r#"{"pulls":[
              {"repo":"a/b","number":1,"head":"x","base":"master","behind":3,"ahead":2,"conflicts":false},
              {"repo":"a/b","number":2,"head":"y","base":"master","behind":1,"conflicts":true},
              {"repo":"a/b","number":3,"head":"z","base":"master","behind":null,"conflicts":null},
              {"repo":"a/b","number":4,"head":"w","base":"master"}]}"#,
        ))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/repos/a/b/pulls/2"))
        .respond_with(raw(
            200,
            r#"{"repo":"a/b","number":2,"head":"y","base":"master","behind":1,"ahead":4,"conflicts":true,"conflict_files":["a.go","docs/b.md"]}"#,
        ))
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/repos/a/b/pulls/3"))
        .respond_with(raw(
            200,
            r#"{"repo":"a/b","number":3,"conflict_files":null}"#,
        ))
        .mount(&server)
        .await;
    let client = client(&server);
    let pulls = client.pulls().await.unwrap();
    assert_eq!(
        pulls
            .iter()
            .map(|p| (p.behind, p.ahead, p.conflicts))
            .collect::<Vec<_>>(),
        [
            (Some(3), Some(2), Some(false)),
            (Some(1), None, Some(true)),
            (None, None, None),
            (None, None, None),
        ]
    );
    assert_eq!(
        pulls.iter().map(|p| p.behind()).collect::<Vec<_>>(),
        [3, 1, 0, 0]
    );
    assert!(pulls[1].has_conflicts() && !pulls[0].has_conflicts() && !pulls[3].has_conflicts());
    // No `can_update` from an older studio: behind and no conflicts.
    assert_eq!(
        pulls.iter().map(|p| p.can_update()).collect::<Vec<_>>(),
        [true, false, false, false]
    );
    // When the studio says, that wins (a protected head, or one on GitHub).
    let said: PullSummary =
        serde_json::from_str(r#"{"behind":3,"conflicts":false,"can_update":false}"#).unwrap();
    assert!(!said.can_update());
    let detail: Pull = serde_json::from_str(r#"{"behind":0,"can_update":true}"#).unwrap();
    assert!(detail.can_update());

    let pull = client.pull("a", "b", 2).await.unwrap();
    assert_eq!((pull.behind(), pull.ahead), (1, Some(4)));
    assert!(pull.has_conflicts());
    assert_eq!(pull.conflict_files, ["a.go", "docs/b.md"]);
    let quiet = client.pull("a", "b", 3).await.unwrap();
    assert!(!quiet.has_conflicts() && quiet.conflict_files.is_empty());
    // What an older studio never sent isn't written back out.
    let json = serde_json::to_value(&quiet).unwrap();
    for key in [
        "behind",
        "ahead",
        "conflicts",
        "conflict_files",
        "can_update",
    ] {
        assert!(json.get(key).is_none(), "{key}");
    }
}
