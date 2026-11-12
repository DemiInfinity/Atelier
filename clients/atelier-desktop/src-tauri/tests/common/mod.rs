#![allow(dead_code)]
//! A whole little world for one test: a mock studio, a temp home with its
//! own config, token file, SSH folder and clone root, and a git that never
//! reads the real configuration. Nothing here touches the real keychain,
//! `~/.ssh`, the real studio or the installed runner.

use std::path::{Path, PathBuf};
use std::sync::Arc;

use atelier_core::config::{Config, Env, Settings};
use atelier_core::token::default_store;
use atelier_core::{Git, Token};
use atelier_desktop::desk::Recorder;
use atelier_desktop::Desk;
use serde_json::{json, Value};
use wiremock::matchers::{header, method, path};
use wiremock::{Mock, MockServer, ResponseTemplate};

pub const TOKEN: &str = "atd_0123456789abcdefghijABCDEFGHIJ0123456789";
pub const API: &str = "/api/app/v1";

pub struct World {
    pub dir: tempfile::TempDir,
    pub server: MockServer,
    pub desk: Desk,
    pub effects: Arc<Recorder>,
    pub git: Git,
}

fn env_for(base: &Path) -> Env {
    Env::fixed([
        ("HOME", base.join("home").to_string_lossy().into_owned()),
        (
            "ATELIER_CONFIG_DIR",
            base.join("config").to_string_lossy().into_owned(),
        ),
        ("ATELIER_TOKEN_STORE", "file".to_string()),
        (
            "ATELIER_SSH_DIR",
            base.join("ssh").to_string_lossy().into_owned(),
        ),
        (
            "ATELIER_RUNNER_SOCKET",
            base.join("run/control.sock").to_string_lossy().into_owned(),
        ),
        ("ATELIER_MESSAGE_SEED", "0".to_string()),
    ])
}

impl World {
    /// Not signed in: an empty config.
    pub async fn fresh() -> World {
        Self::build(false).await
    }

    /// Signed in to the mock studio as the device "MacBook".
    pub async fn signed_in() -> World {
        let world = Self::build(true).await;
        world.mock_get("/me", me()).await;
        world
    }

    /// Signed in, with a `/me` of the test's own.
    pub async fn build_with_me(me: Value) -> World {
        let world = Self::build(true).await;
        world.mock_get("/me", me).await;
        world
    }

    async fn build(signed_in: bool) -> World {
        let dir = tempfile::tempdir().unwrap();
        let base = dir.path().canonicalize().unwrap();
        for sub in ["home/Atelier", "home/Projects", "config", "run"] {
            std::fs::create_dir_all(base.join(sub)).unwrap();
        }
        let server = MockServer::start().await;
        let env = env_for(&base);
        let mut config = Config {
            watch: vec!["~/Projects".into()],
            ..Config::default()
        };
        if signed_in {
            config.studio = Some(server.uri());
            config.device_id = Some(7);
            config.device_name = Some("MacBook".into());
            default_store(&base.join("config"), &env)
                .set(&server.uri(), &Token::new(TOKEN))
                .unwrap();
        }
        config.save(&base.join("config")).unwrap();
        let settings = Settings::with_dirs(config, base.join("config"), base.join("home"), env);
        let git = Git::new()
            .env("HOME", base.join("home"))
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env("GIT_AUTHOR_NAME", "Test")
            .env("GIT_AUTHOR_EMAIL", "test@example.com")
            .env("GIT_COMMITTER_NAME", "Test")
            .env("GIT_COMMITTER_EMAIL", "test@example.com");
        let effects = Arc::new(Recorder::default());
        let desk = Desk::with(settings, git.clone(), effects.clone());
        World {
            dir,
            server,
            desk,
            effects,
            git,
        }
    }

    pub fn base(&self) -> PathBuf {
        self.dir.path().canonicalize().unwrap()
    }

    pub fn home(&self) -> PathBuf {
        self.base().join("home")
    }

    pub async fn mock_get(&self, route: &str, body: Value) {
        Mock::given(method("GET"))
            .and(path(format!("{API}{route}")))
            .and(header("authorization", format!("Bearer {TOKEN}")))
            .respond_with(ResponseTemplate::new(200).set_body_json(body))
            .mount(&self.server)
            .await;
    }

    pub async fn mock(&self, verb: &str, route: &str, status: u16, body: Value) {
        Mock::given(method(verb))
            .and(path(format!("{API}{route}")))
            .and(header("authorization", format!("Bearer {TOKEN}")))
            .respond_with(ResponseTemplate::new(status).set_body_json(body))
            .mount(&self.server)
            .await;
    }

    /// The JSON bodies the studio received on `route`.
    pub async fn received(&self, verb: &str, route: &str) -> Vec<Value> {
        let wanted = format!("{API}{route}");
        self.server
            .received_requests()
            .await
            .unwrap_or_default()
            .into_iter()
            .filter(|r| r.method.as_str() == verb && r.url.path() == wanted)
            .map(|r| serde_json::from_slice(&r.body).unwrap_or(Value::Null))
            .collect()
    }

    pub async fn g(&self, dir: &Path, args: &[&str]) -> String {
        self.git
            .run(Some(dir), args)
            .await
            .unwrap_or_else(|e| panic!("git {args:?}: {e}"))
    }

    pub async fn commit(&self, repo: &Path, file: &str, text: &str) {
        write(repo, file, text);
        self.g(repo, &["add", "--", file]).await;
        self.g(
            repo,
            &["commit", "--quiet", "-m", &format!("change {file}")],
        )
        .await;
    }

    /// The git client's fixture (`scripts/git-fixture.sh`) under the clone
    /// root: feature/tunnel-ui with 5 commits to push, 3 changed files (2
    /// staged), 2 put-aside entries, two tags, the remotes `studio` and
    /// `github`, a branch that conflicts (`feature/backoff-rework`) and a
    /// linked worktree at `<folder>.worktrees/fix-setup-race`.
    pub fn git_fixture(&self, name: &str) -> PathBuf {
        let repo = self.home().join("Atelier").join(name);
        std::fs::create_dir_all(repo.parent().unwrap()).unwrap();
        let script = Path::new(env!("CARGO_MANIFEST_DIR")).join("../scripts/git-fixture.sh");
        let out = std::process::Command::new("sh")
            .arg(&script)
            .arg(&repo)
            .env("HOME", self.home())
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .output()
            .expect("sh runs");
        assert!(
            out.status.success(),
            "git-fixture.sh: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        repo
    }

    /// A bare repository standing in for the studio's copy.
    pub async fn bare(&self, name: &str) -> PathBuf {
        let bare = self.base().join("studio").join(format!("{name}.git"));
        std::fs::create_dir_all(&bare).unwrap();
        self.g(&bare, &["init", "--quiet", "--bare", "-b", "main"])
            .await;
        bare
    }

    /// A working copy under the clone root whose `origin` is the studio's
    /// address for `full_name` (so the app matches it) and whose branch
    /// follows a local bare repository through the remote `local` (so
    /// fetch, pull and push work without a network).
    pub async fn copy(&self, full_name: &str) -> (PathBuf, PathBuf) {
        let bare = self.bare(&full_name.replace('/', "-")).await;
        let repo = self.home().join("Atelier").join(full_name);
        std::fs::create_dir_all(&repo).unwrap();
        self.g(&repo, &["init", "--quiet", "-b", "main"]).await;
        self.commit(&repo, "README.md", "hello\n").await;
        self.g(&repo, &["remote", "add", "origin", &studio_url(full_name)])
            .await;
        self.g(&repo, &["remote", "add", "local", bare.to_str().unwrap()])
            .await;
        self.g(&repo, &["push", "--quiet", "-u", "local", "main"])
            .await;
        (repo, bare)
    }
}

pub fn write(repo: &Path, file: &str, text: &str) {
    let path = repo.join(file);
    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
    std::fs::write(path, text).unwrap();
}

pub fn studio_url(full_name: &str) -> String {
    format!("https://studio.test/{full_name}.git")
}

pub fn repo_json(full_name: &str, extra: Value) -> Value {
    let (owner, name) = full_name.split_once('/').unwrap();
    let mut repo = json!({
        "full_name": full_name, "owner": owner, "owner_kind": "user", "name": name,
        "private": true, "description": "", "default_branch": "main",
        "language": "Go", "language_color": "#00ADD8",
        "clone_https": studio_url(full_name), "clone_ssh": null,
        "github": null, "open_pulls": 0, "updated_at": "2026-11-11T09:00:00Z"
    });
    if let (Some(base), Some(extra)) = (repo.as_object_mut(), extra.as_object()) {
        for (k, v) in extra {
            base.insert(k.clone(), v.clone());
        }
    }
    repo
}

/// `/me`, as in the contract.
pub fn me() -> Value {
    json!({
        "person": {"name": "Demi", "login": "DemiInfinity", "avatar_url": null},
        "accounts": [
            {"name": "DemiInfinity", "display": "Demi", "kind": "user", "commit_name": "Demi", "commit_email": "demi@example.com"},
            {"name": "elyndrastudios", "display": "Elyndra Studios LTD", "kind": "company", "commit_name": "Demi at Elyndra", "commit_email": "demi@company.example"}
        ],
        "studio": {"name": "Atelier", "url": "https://studio.example", "version": "7f82d31",
                   "ssh": {"host": "studio.example", "port": 2223}, "tether": true, "connect": false},
        "device": {"id": 7, "name": "MacBook", "kind": "desktop"}
    })
}

/// `/messages`, as in the contract.
pub fn messages(tone: &str) -> Value {
    json!({
        "tone": tone,
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

/// `/status`, as in the contract.
pub fn status() -> Value {
    json!({
        "needs": [
            {"id": "agent:31", "kind": "agent_approval", "thread": "agents", "title": "Approve weekly-tidy before it runs",
             "detail": "agent · wants issues.close and issues.comment", "repo": "DemiInfinity/Atelier",
             "web_path": "/DemiInfinity/Atelier/actions/runs/31", "action": {"label": "Review", "type": "approve_agent", "job_id": 31}},
            {"id": "run:208", "kind": "run_failed", "thread": "yours", "title": "ci failed on feature/ssh",
             "detail": "test job · ran on Portainer host", "repo": "DemiInfinity/Atelier",
             "web_path": "/DemiInfinity/Atelier/actions/runs/208", "action": {"label": "Open log", "type": "open_run", "run_id": 208}},
            {"id": "pull:DemiInfinity/Tray:9", "kind": "pull_ready", "thread": "yours", "title": "Security update is ready to merge",
             "detail": "okhttp 5.1 fixes 1 advisory · checks passed", "repo": "DemiInfinity/Tray",
             "web_path": "/DemiInfinity/Tray/pulls/9", "action": {"label": "Review", "type": "open_pull", "number": 9}}
        ],
        "running": [
            {"job_id": 412, "run_id": 209, "repo": "DemiInfinity/Atelier", "workflow": "review-helper", "job": "agent",
             "kind": "agent", "engine": "claude-code", "runner": "MacBook", "started_at": "2026-11-11T09:14:02Z",
             "detail": "pull request 14"},
            {"job_id": 413, "run_id": 209, "repo": "DemiInfinity/Atelier", "workflow": "image", "job": "build",
             "kind": "job", "engine": null, "runner": "Portainer host", "started_at": "2026-11-11T09:14:02Z",
             "detail": "push on feature/tunnel-ui"}
        ],
        "feed": [
            {"thread": "yours", "text": "Merged “Pages custom domains” into Atelier main", "at": "2026-11-11T08:00:00Z", "web_path": "/DemiInfinity/Atelier/pulls/13"},
            {"thread": "github", "text": "A reviewer approved pull request 63 on sparky-ventures", "at": "2026-11-11T07:00:00Z", "web_path": "/elyndrastudios/sparky-ventures/pulls/63"},
            {"thread": "connect", "text": "Mara starred Atelier from her studio", "at": "2026-11-11T06:00:00Z", "web_path": null, "url": "https://mara.example/"}
        ],
        "unread": 3
    })
}
