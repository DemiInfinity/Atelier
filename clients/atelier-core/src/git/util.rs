//! Plumbing shared by the git client modules: running git with standard
//! input and extra environment, where the git folder is, checking what a
//! window sent before it gets near a command line.

use std::ffi::{OsStr, OsString};
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::atomic::{AtomicU64, Ordering};

use tokio::io::AsyncWriteExt;

use super::Git;
use crate::error::{Error, Result};

pub(crate) const FIELD: char = '\u{1f}';
pub(crate) const RECORD: char = '\u{1e}';

/// What one git run printed and how it ended. A non-zero exit is not an
/// error here: merges and applies say a lot through their exit code.
#[derive(Debug)]
pub(crate) struct Output {
    pub code: i32,
    pub stdout: Vec<u8>,
    pub stderr: String,
    pub command: String,
}

impl Output {
    pub fn ok(&self) -> bool {
        self.code == 0
    }

    pub fn text(&self) -> String {
        String::from_utf8_lossy(&self.stdout).into_owned()
    }

    pub fn trimmed(&self) -> String {
        String::from_utf8_lossy(&self.stdout).trim().to_string()
    }

    /// Everything git said, on one line (it uses both streams for refusals).
    pub fn said(&self) -> String {
        let out = String::from_utf8_lossy(&self.stdout);
        let lines: Vec<&str> = self
            .stderr
            .lines()
            .chain(out.lines())
            .map(str::trim)
            .filter(|l| !l.is_empty())
            .map(|l| {
                l.trim_start_matches("fatal: ")
                    .trim_start_matches("error: ")
            })
            .collect();
        lines.join(" · ")
    }

    pub fn error(&self) -> Error {
        let message = self.said();
        Error::Git {
            command: self.command.clone(),
            message: if message.is_empty() {
                format!("exit {}", self.code)
            } else {
                message
            },
        }
    }

    pub fn into_result(self) -> Result<Output> {
        if self.ok() {
            Ok(self)
        } else {
            Err(self.error())
        }
    }
}

/// How to run git this once.
#[derive(Default)]
pub(crate) struct Call<'a> {
    pub stdin: Option<&'a [u8]>,
    pub envs: Vec<(&'static str, OsString)>,
}

impl<'a> Call<'a> {
    pub fn stdin(mut self, bytes: &'a [u8]) -> Self {
        self.stdin = Some(bytes);
        self
    }

    pub fn env(mut self, key: &'static str, value: impl Into<OsString>) -> Self {
        self.envs.push((key, value.into()));
        self
    }

    /// Pathspecs are taken literally: no globs, no magic.
    pub fn literal(self) -> Self {
        self.env("GIT_LITERAL_PATHSPECS", "1")
    }

    /// Never opens an editor: whatever message git proposes is taken.
    pub fn no_editor(self) -> Self {
        self.env("GIT_EDITOR", "true")
            .env("GIT_SEQUENCE_EDITOR", "true")
            .env("GIT_MERGE_AUTOEDIT", "no")
    }

    /// For the app's own bookkeeping objects (snapshots kept for undo): a
    /// fixed identity, so they can be made before anyone set a name.
    pub fn bookkeeping(self) -> Self {
        self.env("GIT_AUTHOR_NAME", "Atelier")
            .env("GIT_AUTHOR_EMAIL", "atelier@localhost")
            .env("GIT_COMMITTER_NAME", "Atelier")
            .env("GIT_COMMITTER_EMAIL", "atelier@localhost")
    }
}

static COUNTER: AtomicU64 = AtomicU64::new(0);

/// A name no other call in this process (or another one) will pick.
pub(crate) fn unique(prefix: &str) -> String {
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);
    format!(
        "{prefix}-{}-{}-{}",
        std::process::id(),
        nanos,
        COUNTER.fetch_add(1, Ordering::Relaxed)
    )
}

pub(crate) fn now_unix() -> i64 {
    time::OffsetDateTime::now_utc().unix_timestamp()
}

pub(crate) fn rfc3339(unix: i64) -> String {
    time::OffsetDateTime::from_unix_timestamp(unix)
        .ok()
        .and_then(|t| {
            t.format(&time::format_description::well_known::Rfc3339)
                .ok()
        })
        .unwrap_or_default()
}

pub(crate) fn is_sha(text: &str) -> bool {
    (4..=64).contains(&text.len()) && text.bytes().all(|b| b.is_ascii_hexdigit())
}

pub(crate) fn short(sha: &str) -> String {
    sha.chars().take(7).collect()
}

pub(crate) fn check_path(path: &str) -> Result<&str> {
    let bad = path.is_empty()
        || path.starts_with('/')
        || path.contains('\0')
        || path.split('/').any(|part| part == ".." || part == ".git");
    if bad {
        Err(Error::Invalid(format!(
            "“{path}” isn’t a file in this repository."
        )))
    } else {
        Ok(path)
    }
}

/// A revision someone typed or a window sent: never an option, never
/// something with spaces or control characters in it.
pub(crate) fn check_rev(rev: &str) -> Result<&str> {
    let bad = rev.is_empty()
        || rev.starts_with('-')
        || rev
            .chars()
            .any(|c| c.is_control() || c.is_whitespace() || c == '\0');
    if bad {
        Err(Error::Invalid(format!(
            "“{rev}” isn’t a commit or a branch."
        )))
    } else {
        Ok(rev)
    }
}

/// `1 commit` / `3 commits`.
pub(crate) fn count(n: u64, one: &str, many: &str) -> String {
    if n == 1 {
        format!("1 {one}")
    } else {
        format!("{n} {many}")
    }
}

/// Git's refusals about uncommitted work, said plainly. Anything else
/// passes through as it was.
pub(crate) fn plain_refusal(error: Error, doing: &str) -> Error {
    match error {
        Error::Git { message, .. }
            if message.contains("would be overwritten")
                || message.contains("commit your changes or stash them")
                || message.contains("You have unstaged changes")
                || message.contains("Your index contains uncommitted changes")
                || message.contains("local changes") =>
        {
            Error::Invalid(format!(
                "{doing} would overwrite your uncommitted changes. Commit them or put them aside first."
            ))
        }
        other => other,
    }
}

impl Git {
    /// Runs git in `dir`; a non-zero exit comes back as an [`Output`].
    pub(crate) async fn exec<S: AsRef<OsStr>>(
        &self,
        dir: &Path,
        args: &[S],
        call: Call<'_>,
    ) -> Result<Output> {
        let mut command = self.command(Some(dir));
        command.args(args);
        for (key, value) in &call.envs {
            command.env(key, value);
        }
        if call.stdin.is_some() {
            command.stdin(Stdio::piped());
        }
        let mut child = command.spawn().map_err(|e| match e.kind() {
            std::io::ErrorKind::NotFound => Error::MissingProgram("git".into()),
            _ => Error::io("Couldn’t run git", e),
        })?;
        if let (Some(bytes), Some(mut stdin)) = (call.stdin, child.stdin.take()) {
            // Git may stop reading early (a refused patch); that is its
            // answer to give, not a failure of ours.
            let _ = stdin.write_all(bytes).await;
            let _ = stdin.shutdown().await;
        }
        let output = child
            .wait_with_output()
            .await
            .map_err(|e| Error::io("Couldn’t run git", e))?;
        let os_args: Vec<OsString> = args.iter().map(|a| a.as_ref().to_os_string()).collect();
        Ok(Output {
            code: output.status.code().unwrap_or(-1),
            stdout: output.stdout,
            stderr: String::from_utf8_lossy(&output.stderr).into_owned(),
            command: super::command_word(&os_args),
        })
    }

    /// Runs git and returns standard output as printed; fails on a non-zero exit.
    pub(crate) async fn text<S: AsRef<OsStr>>(&self, dir: &Path, args: &[S]) -> Result<String> {
        Ok(self
            .exec(dir, args, Call::default())
            .await?
            .into_result()?
            .text())
    }

    /// Like [`Git::text`], trimmed.
    pub(crate) async fn line<S: AsRef<OsStr>>(&self, dir: &Path, args: &[S]) -> Result<String> {
        Ok(self.text(dir, args).await?.trim().to_string())
    }

    /// Trimmed output, or `None` when git said no.
    pub(crate) async fn maybe<S: AsRef<OsStr>>(
        &self,
        dir: &Path,
        args: &[S],
    ) -> Result<Option<String>> {
        let out = self.exec(dir, args, Call::default()).await?;
        Ok(out.ok().then(|| out.trimmed()).filter(|s| !s.is_empty()))
    }

    /// This working tree's own git folder (`.git`, or `.git/worktrees/x`
    /// for a linked worktree).
    pub async fn git_dir(&self, repo: &Path) -> Result<PathBuf> {
        self.maybe(repo, &["rev-parse", "--absolute-git-dir"])
            .await?
            .map(PathBuf::from)
            .ok_or_else(|| Error::NotARepo(repo.to_path_buf()))
    }

    /// The git folder shared by every worktree of this repository.
    pub async fn common_dir(&self, repo: &Path) -> Result<PathBuf> {
        let dir = self
            .maybe(repo, &["rev-parse", "--git-common-dir"])
            .await?
            .ok_or_else(|| Error::NotARepo(repo.to_path_buf()))?;
        let path = PathBuf::from(dir);
        let path = if path.is_absolute() {
            path
        } else {
            repo.join(path)
        };
        Ok(path.canonicalize().unwrap_or(path))
    }

    /// The app's own corner of the shared git folder (journal, markers,
    /// scratch files). Never inside the working tree.
    pub(crate) async fn app_dir(&self, repo: &Path) -> Result<PathBuf> {
        let dir = self.common_dir(repo).await?.join("atelier");
        std::fs::create_dir_all(&dir)
            .map_err(|e| Error::io(format!("Couldn’t create {}", dir.display()), e))?;
        Ok(dir)
    }

    /// The app's corner of this worktree's own git folder.
    pub(crate) async fn app_dir_local(&self, repo: &Path) -> Result<PathBuf> {
        let dir = self.git_dir(repo).await?.join("atelier");
        std::fs::create_dir_all(&dir)
            .map_err(|e| Error::io(format!("Couldn’t create {}", dir.display()), e))?;
        Ok(dir)
    }

    /// The full id of the commit `rev` names.
    pub async fn resolve_commit(&self, repo: &Path, rev: &str) -> Result<String> {
        let rev = check_rev(rev)?;
        let spec = format!("{rev}^{{commit}}");
        self.maybe(
            repo,
            &[
                "rev-parse",
                "--verify",
                "--quiet",
                "--end-of-options",
                &spec,
            ],
        )
        .await?
        .filter(|s| is_sha(s))
        .ok_or_else(|| Error::Invalid(format!("There is no commit {rev} here.")))
    }

    /// The commit HEAD is at, or `None` before the first commit.
    pub async fn head_sha(&self, repo: &Path) -> Result<Option<String>> {
        self.maybe(repo, &["rev-parse", "--verify", "--quiet", "HEAD"])
            .await
    }

    /// `name` can be a branch (or tag, or remote) name.
    pub(crate) async fn check_name(&self, repo: &Path, name: &str, what: &str) -> Result<()> {
        let full = format!("refs/heads/{name}");
        let ok = !name.is_empty()
            && !name.starts_with('-')
            && !name.chars().any(char::is_whitespace)
            && self
                .exec(repo, &["check-ref-format", &full], Call::default())
                .await?
                .ok();
        if ok {
            Ok(())
        } else {
            Err(Error::Invalid(format!("“{name}” can’t be a {what} name.")))
        }
    }

    /// True when there is anything uncommitted: staged, changed or untracked.
    pub async fn is_dirty(&self, repo: &Path) -> Result<bool> {
        Ok(!self
            .text(repo, &["status", "--porcelain", "--untracked-files=normal"])
            .await?
            .trim()
            .is_empty())
    }

    /// Tracked files with staged or unstaged changes (untracked ones never
    /// stop a switch or a rebase).
    pub(crate) async fn tracked_changes(&self, repo: &Path) -> Result<usize> {
        Ok(self
            .text(repo, &["status", "--porcelain", "--untracked-files=no"])
            .await?
            .lines()
            .filter(|l| !l.trim().is_empty())
            .count())
    }

    /// Paths with an unresolved conflict in the index.
    pub(crate) async fn unmerged_paths(&self, repo: &Path) -> Result<Vec<String>> {
        let out = self
            .text(
                repo,
                &[
                    "diff",
                    "--name-only",
                    "--diff-filter=U",
                    "-z",
                    "--no-renames",
                ],
            )
            .await?;
        let mut paths: Vec<String> = out
            .split('\0')
            .filter(|p| !p.is_empty())
            .map(str::to_string)
            .collect();
        paths.sort();
        paths.dedup();
        Ok(paths)
    }
}

/// Reads a small file of git's own (`MERGE_HEAD`, `rebase-merge/msgnum`).
pub(crate) fn read_trimmed(path: &Path) -> Option<String> {
    std::fs::read_to_string(path)
        .ok()
        .map(|s| s.trim().to_string())
}
