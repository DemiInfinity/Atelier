#![allow(dead_code)]
//! A sandbox for running the real `atl` binary: its own home, config
//! folder, SSH folder and git configuration, and a mock studio. Nothing
//! reaches the real keychain, `~/.ssh`, config or network.

use std::path::{Path, PathBuf};
use std::process::Output;

use wiremock::matchers::{method, path};
use wiremock::{Mock, MockServer, ResponseTemplate};

pub const TOKEN: &str = "atd_0123456789abcdefghijABCDEFGHIJ0123456789";

pub struct Sandbox {
    pub dir: tempfile::TempDir,
    pub server: MockServer,
}

pub struct Ran {
    pub code: i32,
    pub stdout: String,
    pub stderr: String,
}

impl Ran {
    /// Standard output of a run that must have succeeded.
    pub fn ok(self) -> String {
        assert_eq!(
            self.code, 0,
            "stdout:\n{}\nstderr:\n{}",
            self.stdout, self.stderr
        );
        assert_eq!(self.stderr, "", "unexpected stderr");
        self.stdout
    }

    pub fn json(self) -> serde_json::Value {
        let out = self.ok();
        serde_json::from_str(&out).unwrap_or_else(|e| panic!("not JSON ({e}):\n{out}"))
    }
}

impl Sandbox {
    pub async fn new() -> Sandbox {
        let dir = tempfile::tempdir().unwrap();
        for sub in ["home", "config/atelier", "work"] {
            std::fs::create_dir_all(dir.path().join(sub)).unwrap();
        }
        Sandbox {
            dir,
            server: MockServer::start().await,
        }
    }

    pub fn root(&self) -> PathBuf {
        self.dir.path().canonicalize().unwrap()
    }
    pub fn home(&self) -> PathBuf {
        self.root().join("home")
    }
    pub fn config_dir(&self) -> PathBuf {
        self.root().join("config/atelier")
    }
    pub fn work(&self) -> PathBuf {
        self.root().join("work")
    }

    /// The environment every child process gets: nothing inherited but PATH.
    fn envs(&self) -> Vec<(&'static str, String)> {
        let s = |p: PathBuf| p.to_string_lossy().into_owned();
        vec![
            ("PATH", std::env::var("PATH").unwrap_or_default()),
            ("HOME", s(self.home())),
            ("ATELIER_CONFIG_DIR", s(self.config_dir())),
            ("ATELIER_SSH_DIR", s(self.home().join(".ssh"))),
            ("ATELIER_TOKEN_STORE", "file".into()),
            ("ATELIER_NO_BROWSER", "1".into()),
            ("ATELIER_RUNNER_SOCKET", s(self.root().join("control.sock"))),
            ("ATELIER_MESSAGE_SEED", "0".into()),
            ("GIT_CONFIG_GLOBAL", "/dev/null".into()),
            ("GIT_CONFIG_NOSYSTEM", "1".into()),
            ("GIT_AUTHOR_NAME", "Test".into()),
            ("GIT_AUTHOR_EMAIL", "test@example.com".into()),
            ("GIT_COMMITTER_NAME", "Test".into()),
            ("GIT_COMMITTER_EMAIL", "test@example.com".into()),
        ]
    }

    fn command(&self, signed_in: bool, cwd: &Path, args: &[&str]) -> assert_cmd::Command {
        let mut command = assert_cmd::Command::cargo_bin("atl").unwrap();
        command
            .env_clear()
            .envs(self.envs())
            .current_dir(cwd)
            .args(args);
        if signed_in {
            command
                .env("ATELIER_STUDIO", self.server.uri())
                .env("ATELIER_TOKEN", TOKEN);
        }
        command
    }

    async fn run(&self, signed_in: bool, cwd: &Path, args: &[&str]) -> Ran {
        let mut command = self.command(signed_in, cwd, args);
        // The mock studio lives on this runtime; don't block it.
        let output: Output = tokio::task::spawn_blocking(move || command.output().unwrap())
            .await
            .unwrap();
        Ran {
            code: output.status.code().unwrap_or(-1),
            stdout: String::from_utf8(output.stdout).unwrap(),
            stderr: String::from_utf8(output.stderr).unwrap(),
        }
    }

    /// `atl <args>` signed in (through `ATELIER_STUDIO`/`ATELIER_TOKEN`),
    /// outside any repository.
    pub async fn atl(&self, args: &[&str]) -> Ran {
        self.run(true, &self.work(), args).await
    }

    pub async fn atl_in(&self, cwd: &Path, args: &[&str]) -> Ran {
        self.run(true, cwd, args).await
    }

    /// `atl <args>` in `cwd` with `typed` on standard input.
    pub async fn atl_typing(&self, cwd: &Path, args: &[&str], typed: &str) -> Ran {
        let mut command = self.command(true, cwd, args);
        command.write_stdin(typed.to_owned());
        let output: Output = tokio::task::spawn_blocking(move || command.output().unwrap())
            .await
            .unwrap();
        Ran {
            code: output.status.code().unwrap_or(-1),
            stdout: String::from_utf8(output.stdout).unwrap(),
            stderr: String::from_utf8(output.stderr).unwrap(),
        }
    }

    /// `atl <args>` with only what is on disk in the sandbox.
    pub async fn atl_bare(&self, args: &[&str]) -> Ran {
        self.run(false, &self.work(), args).await
    }

    pub fn git(&self, dir: &Path, args: &[&str]) -> String {
        let output = std::process::Command::new("git")
            .env_clear()
            .envs(self.envs())
            .arg("-C")
            .arg(dir)
            .args(args)
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "git {args:?}: {}",
            String::from_utf8_lossy(&output.stderr)
        );
        String::from_utf8_lossy(&output.stdout).trim().to_string()
    }

    /// A repository at `path` with one commit on `main`.
    pub fn repo(&self, path: &Path) {
        std::fs::create_dir_all(path).unwrap();
        self.git(path, &["init", "--quiet", "-b", "main"]);
        self.commit(path, "README.md");
    }

    pub fn commit(&self, repo: &Path, file: &str) {
        std::fs::write(repo.join(file), file).unwrap();
        self.git(repo, &["add", "--", file]);
        self.git(repo, &["commit", "--quiet", "-m", file]);
    }

    /// Makes `repo` track a throwaway bare remote, then renames the remote
    /// to `url` so it looks like a clone from the studio.
    pub fn track(&self, repo: &Path, url: &str) {
        let bare = self.root().join(format!(
            "remote-{}.git",
            repo.file_name().unwrap().to_string_lossy()
        ));
        std::fs::create_dir_all(&bare).unwrap();
        self.git(&bare, &["init", "--quiet", "--bare"]);
        self.git(repo, &["remote", "add", "origin", bare.to_str().unwrap()]);
        self.git(repo, &["push", "--quiet", "-u", "origin", "HEAD"]);
        self.git(repo, &["remote", "set-url", "origin", url]);
    }

    pub async fn get(&self, route: &str, body: &str) {
        self.respond("GET", route, 200, body).await;
    }

    pub async fn respond(&self, verb: &str, route: &str, status: u16, body: &str) {
        Mock::given(method(verb))
            .and(path(format!("/api/app/v1{route}")))
            .respond_with(
                ResponseTemplate::new(status).set_body_raw(body.to_owned(), "application/json"),
            )
            .mount(&self.server)
            .await;
    }

    /// How many requests of this kind the studio got.
    pub async fn count(&self, verb: &str, route: &str) -> usize {
        let route = format!("/api/app/v1{route}");
        self.server
            .received_requests()
            .await
            .unwrap()
            .iter()
            .filter(|r| r.method.as_str() == verb && r.url.path() == route)
            .count()
    }

    /// The JSON bodies of the requests of this kind, oldest first.
    pub async fn bodies(&self, verb: &str, route: &str) -> Vec<serde_json::Value> {
        let route = format!("/api/app/v1{route}");
        self.server
            .received_requests()
            .await
            .unwrap()
            .iter()
            .filter(|r| r.method.as_str() == verb && r.url.path() == route)
            .map(|r| r.body_json().unwrap_or(serde_json::Value::Null))
            .collect()
    }
}

// The contract's example answers, verbatim (docs/app-api.md).

pub const ME: &str = r#"{"person":{"name":"Demi","login":"DemiInfinity","avatar_url":"/-/avatar/DemiInfinity/160.png"},
 "accounts":[{"name":"DemiInfinity","display":"Demi","kind":"user","commit_name":"Demi","commit_email":"demi@example.com"},
             {"name":"elyndrastudios","display":"Elyndra Studios LTD","kind":"company","commit_name":"Demi","commit_email":"demi@company.example"}],
 "studio":{"name":"Atelier","url":"https://studio.example","version":"7f82d31",
           "ssh":{"host":"studio.example","port":2223},"tether":true,"connect":false},
 "device":{"id":7,"name":"MacBook","kind":"desktop"}}"#;

pub const STATUS: &str = r#"{"needs":[
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

pub const REPOS: &str = r##"{"repos":[{"full_name":"DemiInfinity/Atelier","owner":"DemiInfinity","owner_kind":"user","name":"Atelier",
  "private":true,"description":"…","default_branch":"main","language":"Go","language_color":"#00ADD8",
  "clone_https":"https://studio.example/DemiInfinity/Atelier.git",
  "clone_ssh":"ssh://git@studio.example:2223/DemiInfinity/Atelier.git",
  "github":{"repo":"DemiInfinity/Atelier","home":"atelier"},"open_pulls":1,"updated_at":"…"},
 {"full_name":"DemiInfinity/Marbl","owner":"DemiInfinity","owner_kind":"user","name":"Marbl",
  "private":false,"description":"","default_branch":"master","language":"Swift","language_color":"#F05138",
  "clone_https":"https://studio.example/DemiInfinity/Marbl.git",
  "clone_ssh":"ssh://git@studio.example:2223/DemiInfinity/Marbl.git",
  "github":null,"open_pulls":0,"updated_at":"…"}]}"##;

pub const PULL: &str = r#"{"repo":"DemiInfinity/Atelier","number":14,"title":"Remote access switch in the tray menu","state":"open",
 "head":"feature/tunnel-ui","base":"main","author":"owner:DemiInfinity","commits":2,"files":3,
 "via":null,
 "merge":{"can_merge":false,"reason":"1 check is still running and 1 agent is waiting for you.","methods":["squash","merge"],
          "on_github":false,"auto_merge":null},
 "checks":[{"name":"ci · test","status":"success","detail":"MacBook · 1m 48s","run_id":207,"job_id":410},
           {"name":"image · build","status":"pending","detail":"Portainer host · step 3 of 5","run_id":209,"job_id":413},
           {"name":"review-helper","status":"waiting","detail":"agent · needs your approval to run","run_id":210,"job_id":414,"action":"approve_agent"}],
 "comments":[{"author":"CodeRabbit","via":"tether","kind":"bot","body":"toggleRemote ignores the error from SetRemote.","at":"…"}],
 "web_path":"/DemiInfinity/Atelier/pulls/14"}"#;

pub const PULL_SUMMARY: &str = r#"{"repo":"DemiInfinity/Atelier","number":14,"title":"Remote access switch in the tray menu","state":"open",
 "head":"feature/tunnel-ui","base":"main","author":"owner:DemiInfinity","commits":2,"files":3,
 "via":null,
 "merge":{"can_merge":false,"reason":"1 check is still running and 1 agent is waiting for you.","methods":["squash","merge"],
          "on_github":false,"auto_merge":null},
 "checks":{"passed":2,"failed":0,"pending":1,"waiting":1},
 "web_path":"/DemiInfinity/Atelier/pulls/14"}"#;

pub const RUNS: &str = r#"{"runs":[{"run_id":207,"repo":"DemiInfinity/Atelier","workflow":"ci","event":"push","branch":"feature/tunnel-ui","status":"success","started_at":"…","duration_s":108,
  "jobs":[{"job_id":410,"name":"test","status":"success","runner":"MacBook","duration_s":108,"kind":"job"}]},
 {"run_id":208,"repo":"DemiInfinity/Atelier","workflow":"ci","event":"push","branch":"feature/ssh","status":"failure","started_at":"…","duration_s":64,
  "jobs":[{"job_id":411,"name":"lint","status":"success","runner":"MacBook","duration_s":22,"kind":"job"},
          {"job_id":412,"name":"test","status":"failure","runner":"Portainer host","duration_s":64,"kind":"job"}]}]}"#;

pub const WAITING: &str = r#"{"jobs":[{"job_id":31,"run_id":210,"repo":"DemiInfinity/Atelier","workflow":"weekly-tidy",
  "reason":"first run","engine":"claude-code","tools":["issues.list","issues.comment","issues.close"],
  "write_tools":["issues.comment","issues.close"],"max_turns":20,"timeout_minutes":10}]}"#;

pub const REMOTE_OFF: &str =
    r#"{"on":false,"mode":"quick","address":null,"exposed":["web","git"],"connected":0}"#;
pub const REMOTE_ON: &str = r#"{"on":true,"mode":"quick","address":"https://quiet-fox-lamp.trycloudflare.com","exposed":["web","git"],"connected":1}"#;

pub const MESSAGES: &str = r#"{"tone":"friendly",
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
