#![allow(dead_code)]

use std::path::{Path, PathBuf};

use atelier_core::{Error, Git};

pub struct Sandbox {
    dir: tempfile::TempDir,
    pub git: Git,
}

impl Sandbox {
    pub fn new() -> Sandbox {
        let dir = tempfile::tempdir().unwrap();
        let home = dir.path().join("home");
        std::fs::create_dir_all(&home).unwrap();
        // No global or system config, no identity from the environment: each
        // repository sets its own.
        let git = Git::new()
            .env("HOME", &home)
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env("GIT_AUTHOR_DATE", "2026-11-10T09:00:00Z")
            .env("GIT_COMMITTER_DATE", "2026-11-10T09:00:00Z");
        Sandbox { dir, git }
    }

    pub fn path(&self, name: &str) -> PathBuf {
        self.dir.path().canonicalize().unwrap().join(name)
    }

    pub async fn g(&self, dir: &Path, args: &[&str]) -> String {
        self.git
            .run(Some(dir), args)
            .await
            .unwrap_or_else(|e| panic!("git {args:?}: {e}"))
    }

    pub async fn repo(&self, name: &str) -> PathBuf {
        let path = self.path(name);
        std::fs::create_dir_all(&path).unwrap();
        self.g(&path, &["init", "--quiet", "-b", "main"]).await;
        self.g(&path, &["config", "user.name", "Test Person"]).await;
        self.g(&path, &["config", "user.email", "test@example.com"])
            .await;
        path
    }

    /// Writes `file`, commits it with `message`, returns the commit id.
    pub async fn commit(&self, repo: &Path, file: &str, text: &str, message: &str) -> String {
        write(repo, file, text);
        self.g(repo, &["add", "--", file]).await;
        self.g(repo, &["commit", "--quiet", "-m", message]).await;
        self.head(repo).await
    }

    pub async fn head(&self, repo: &Path) -> String {
        self.g(repo, &["rev-parse", "HEAD"]).await
    }

    pub async fn subjects(&self, repo: &Path, range: &str) -> Vec<String> {
        self.g(repo, &["log", "--format=%s", range])
            .await
            .lines()
            .map(str::to_string)
            .collect()
    }

    /// The staged content of a file.
    pub async fn staged(&self, repo: &Path, file: &str) -> String {
        self.git
            .run_raw(Some(repo), ["show", &format!(":{file}")], &[])
            .await
            .unwrap()
    }

    pub async fn status(&self, repo: &Path) -> String {
        self.git
            .run_raw(Some(repo), ["status", "--porcelain"], &[])
            .await
            .unwrap()
    }

    /// A repository with one commit on `main`, pushed to a bare repository
    /// it follows as `origin`.
    pub async fn repo_with_remote(&self, name: &str) -> (PathBuf, PathBuf) {
        let bare = self.path(&format!("{name}.git"));
        std::fs::create_dir_all(&bare).unwrap();
        self.g(&bare, &["init", "--quiet", "--bare", "-b", "main"])
            .await;
        let repo = self.repo(name).await;
        self.commit(&repo, "README.md", "hello\n", "Start").await;
        self.g(&repo, &["remote", "add", "origin", bare.to_str().unwrap()])
            .await;
        self.g(&repo, &["push", "--quiet", "-u", "origin", "main"])
            .await;
        (repo, bare)
    }

    /// A second clone of `bare`, to push from "someone else".
    pub async fn clone_of(&self, bare: &Path, name: &str) -> PathBuf {
        let path = self.path(name);
        self.g(
            &self.path(""),
            &[
                "clone",
                "--quiet",
                bare.to_str().unwrap(),
                path.to_str().unwrap(),
            ],
        )
        .await;
        self.g(&path, &["config", "user.name", "Someone Else"])
            .await;
        self.g(&path, &["config", "user.email", "else@example.com"])
            .await;
        path
    }

    /// `main` and `feature` that both change the same line of `a.txt` and
    /// of `b.txt`; `feature` also adds `own.txt`, `main` adds `clean.txt`.
    /// Ends on `feature`.
    pub async fn conflicting(&self, name: &str) -> PathBuf {
        let repo = self.repo(name).await;
        self.commit(&repo, "a.txt", "one\ntwo\nthree\nfour\nfive\n", "Add a")
            .await;
        self.commit(&repo, "b.txt", "alpha\nbeta\ngamma\n", "Add b")
            .await;
        self.g(&repo, &["switch", "--quiet", "-c", "feature"]).await;
        self.commit(
            &repo,
            "a.txt",
            "one\ntwo yours\nthree\nfour\nfive\n",
            "Yours: a",
        )
        .await;
        self.commit(&repo, "b.txt", "alpha\nbeta yours\ngamma\n", "Yours: b")
            .await;
        self.commit(&repo, "own.txt", "mine\n", "Yours: own").await;
        self.g(&repo, &["switch", "--quiet", "main"]).await;
        self.commit(
            &repo,
            "a.txt",
            "one\ntwo theirs\nthree\nfour\nfive\n",
            "Theirs: a",
        )
        .await;
        self.commit(&repo, "b.txt", "alpha\nbeta theirs\ngamma\n", "Theirs: b")
            .await;
        self.commit(&repo, "clean.txt", "tidy\n", "Theirs: clean")
            .await;
        self.g(&repo, &["switch", "--quiet", "feature"]).await;
        repo
    }
}

pub fn write(repo: &Path, file: &str, text: &str) {
    let path = repo.join(file);
    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
    std::fs::write(path, text).unwrap();
}

pub fn read(repo: &Path, file: &str) -> String {
    std::fs::read_to_string(repo.join(file)).unwrap()
}

/// The message of a refusal (`Error::Invalid`).
pub fn invalid<T: std::fmt::Debug>(result: Result<T, Error>) -> String {
    match result {
        Err(Error::Invalid(message)) => message,
        other => panic!("expected a refusal, got {other:?}"),
    }
}

/// Twenty numbered lines.
pub fn twenty() -> String {
    (1..=20).map(|n| format!("line {n}\n")).collect()
}
