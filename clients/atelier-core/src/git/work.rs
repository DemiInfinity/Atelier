//! Working-copy helpers for the desktop app: what changed, diffs, committing
//! exactly the chosen files, history, branches, and the two small undo
//! actions. Everything shells out to `git` with literal pathspecs, so a file
//! name can never be read as an option or a pattern.

use std::collections::{HashMap, HashSet};
use std::ffi::OsString;
use std::path::{Path, PathBuf};

use serde::Serialize;

use super::Git;
use crate::error::{Error, Result};

/// How a changed file differs from the last commit.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ChangeKind {
    Modified,
    Added,
    Deleted,
    Renamed,
    /// Not known to git yet.
    Untracked,
    /// Both sides changed it during a merge.
    Conflicted,
}

impl ChangeKind {
    /// The one-letter mark shown next to a file.
    pub fn letter(self) -> &'static str {
        match self {
            ChangeKind::Modified => "M",
            ChangeKind::Added | ChangeKind::Untracked => "A",
            ChangeKind::Deleted => "D",
            ChangeKind::Renamed => "R",
            ChangeKind::Conflicted => "!",
        }
    }
}

/// One changed file in a working copy.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct FileChange {
    /// Relative to the top of the working copy, with `/` separators.
    pub path: String,
    /// Where a renamed file used to be.
    pub old_path: Option<String>,
    pub kind: ChangeKind,
    /// Some of this change is already in the index.
    pub staged: bool,
    /// Lines added and removed against the last commit; `None` for binary
    /// files and files too large to count.
    pub additions: Option<u32>,
    pub deletions: Option<u32>,
    pub binary: bool,
}

/// A local branch.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct Branch {
    pub name: String,
    pub current: bool,
    pub upstream: Option<String>,
    pub ahead: u64,
    pub behind: u64,
    pub sha: String,
    pub subject: String,
}

/// What kind of name points at a commit.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum RefKind {
    /// The branch that is checked out.
    Head,
    Branch,
    Remote,
    Tag,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct RefLabel {
    pub kind: RefKind,
    /// `main`, `v0.9`; for a remote branch, the branch without the remote.
    pub name: String,
    /// The remote's name for [`RefKind::Remote`] (`origin`).
    pub remote: Option<String>,
}

/// How a lane of the history graph crosses one row.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Segment {
    /// The lane isn't there.
    #[default]
    None,
    /// Straight through, top to bottom.
    Full,
    /// Starts at this row's dot and runs down.
    Start,
    /// Comes in from the top and bends into lane 0 at this row's dot.
    Join,
}

/// One commit in [`History`].
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct Commit {
    pub sha: String,
    pub short: String,
    pub parents: Vec<String>,
    pub author: String,
    /// Author date, RFC 3339.
    pub date: String,
    pub subject: String,
    pub refs: Vec<RefLabel>,
    /// 0 = the line shared with the upstream; 1 = commits only here.
    pub lane: u8,
    pub line0: Segment,
    pub line1: Segment,
    /// Only on this machine: not in the upstream yet.
    pub unpushed: bool,
    /// Only in the upstream: a pull would bring it in.
    pub incoming: bool,
}

/// The current branch and its upstream as a two-lane graph, newest first.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize)]
pub struct History {
    pub branch: Option<String>,
    pub upstream: Option<String>,
    pub ahead: u64,
    pub behind: u64,
    pub commits: Vec<Commit>,
    /// There are more commits than were asked for.
    pub more: bool,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct FileStat {
    pub path: String,
    pub additions: Option<u32>,
    pub deletions: Option<u32>,
}

/// One commit with its message and the files it touched.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct CommitDetail {
    pub sha: String,
    pub short: String,
    pub author: String,
    pub email: String,
    pub date: String,
    pub subject: String,
    pub body: String,
    pub files: Vec<FileStat>,
    /// Some remote branch already has it.
    pub pushed: bool,
    /// It is the tip of the current branch.
    pub is_head: bool,
}

/// What a fast-forward pull did.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
pub struct Pulled {
    /// Commits brought in (0 = already up to date).
    pub commits: u64,
}

/// Files bigger than this aren't read to count their lines.
const COUNT_LIMIT: u64 = 1024 * 1024;

const FIELD: char = '\u{1f}';
const RECORD: char = '\u{1e}';

fn is_sha(text: &str) -> bool {
    (4..=64).contains(&text.len()) && text.bytes().all(|b| b.is_ascii_hexdigit())
}

fn check_sha(text: &str) -> Result<&str> {
    if is_sha(text) {
        Ok(text)
    } else {
        Err(Error::Invalid(format!("“{text}” isn’t a commit ID.")))
    }
}

fn check_path(path: &str) -> Result<&str> {
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

impl Git {
    /// The same git with pathspecs taken literally (no globs, no magic).
    fn literal(&self) -> Git {
        self.clone().env("GIT_LITERAL_PATHSPECS", "1")
    }

    /// Runs git and returns standard output exactly as printed. Exit codes
    /// in `also_ok` count as success (`git diff --no-index` exits 1 when the
    /// files differ).
    pub async fn run_raw<I, S>(
        &self,
        dir: Option<&Path>,
        args: I,
        also_ok: &[i32],
    ) -> Result<String>
    where
        I: IntoIterator<Item = S>,
        S: AsRef<std::ffi::OsStr>,
    {
        let args: Vec<OsString> = args
            .into_iter()
            .map(|a| a.as_ref().to_os_string())
            .collect();
        let output = self
            .command(dir)
            .args(&args)
            .output()
            .await
            .map_err(|e| match e.kind() {
                std::io::ErrorKind::NotFound => Error::MissingProgram("git".into()),
                _ => Error::io("Couldn’t run git", e),
            })?;
        let code = output.status.code().unwrap_or(-1);
        if output.status.success() || also_ok.contains(&code) {
            return Ok(String::from_utf8_lossy(&output.stdout).into_owned());
        }
        Err(super::failure(&args, &output))
    }

    /// True once the repository has at least one commit.
    pub async fn has_head(&self, repo: &Path) -> Result<bool> {
        Ok(self
            .try_run(repo, &["rev-parse", "--verify", "--quiet", "HEAD"])
            .await?
            .is_some())
    }

    /// The current branch, or `None` with a detached HEAD.
    pub async fn current_branch(&self, repo: &Path) -> Result<Option<String>> {
        Ok(self
            .try_run(repo, &["symbolic-ref", "--short", "-q", "HEAD"])
            .await?
            .filter(|b| !b.is_empty()))
    }

    /// Every changed file: modified, staged, deleted, renamed, untracked.
    /// Ignored files are left out. Sorted by path.
    pub async fn changes(&self, repo: &Path) -> Result<Vec<FileChange>> {
        let status = self
            .run_raw(
                Some(repo),
                ["status", "--porcelain=v1", "-z", "--untracked-files=all"],
                &[],
            )
            .await
            .map_err(|e| match e {
                Error::Git { .. } => Error::NotARepo(repo.to_path_buf()),
                other => other,
            })?;
        let mut changes = parse_status(&status);

        // Line counts against the last commit, staged and unstaged together.
        let numstat = if self.has_head(repo).await? {
            self.run_raw(
                Some(repo),
                ["diff", "--numstat", "-z", "--no-renames", "HEAD"],
                &[],
            )
            .await?
        } else {
            self.run_raw(
                Some(repo),
                ["diff", "--numstat", "-z", "--no-renames", "--cached"],
                &[],
            )
            .await?
        };
        let counts = parse_numstat(&numstat);
        for change in &mut changes {
            if let Some(&(additions, deletions)) = counts.get(change.path.as_str()) {
                change.additions = additions;
                change.deletions = deletions;
                change.binary = additions.is_none();
                // A rename's removed lines are counted on the old path.
                if change.kind == ChangeKind::Renamed {
                    change.deletions = Some(0);
                }
            } else if change.kind == ChangeKind::Untracked {
                match count_lines(&repo.join(&change.path)) {
                    Counted::Lines(n) => {
                        change.additions = Some(n);
                        change.deletions = Some(0);
                    }
                    Counted::Binary => change.binary = true,
                    Counted::Skipped => {}
                }
            }
        }
        changes.sort_by(|a, b| a.path.cmp(&b.path));
        Ok(changes)
    }

    /// The diff of one changed file against the last commit, staged and
    /// unstaged changes together (`git diff HEAD`). `old_path` is where a
    /// renamed file used to be. Before the first commit this is the staged
    /// content (`git diff --cached`).
    pub async fn diff_worktree(
        &self,
        repo: &Path,
        path: &str,
        old_path: Option<&str>,
    ) -> Result<String> {
        check_path(path)?;
        let mut args = vec![
            "-c",
            "core.quotePath=false",
            "diff",
            "--no-color",
            "--no-ext-diff",
            "-M",
        ];
        args.push(if self.has_head(repo).await? {
            "HEAD"
        } else {
            "--cached"
        });
        args.push("--");
        if let Some(old) = old_path {
            args.push(check_path(old)?);
        }
        args.push(path);
        self.literal().run_raw(Some(repo), args, &[]).await
    }

    /// The staged part of one file's changes (`git diff --cached`).
    pub async fn diff_staged(&self, repo: &Path, path: &str) -> Result<String> {
        check_path(path)?;
        self.literal()
            .run_raw(
                Some(repo),
                [
                    "-c",
                    "core.quotePath=false",
                    "diff",
                    "--no-color",
                    "--no-ext-diff",
                    "-M",
                    "--cached",
                    "--",
                    path,
                ],
                &[],
            )
            .await
    }

    /// An untracked file as a diff that adds every line.
    pub async fn diff_untracked(&self, repo: &Path, path: &str) -> Result<String> {
        check_path(path)?;
        let null = if cfg!(windows) { "NUL" } else { "/dev/null" };
        self.literal()
            .run_raw(
                Some(repo),
                [
                    "-c",
                    "core.quotePath=false",
                    "diff",
                    "--no-color",
                    "--no-ext-diff",
                    "--no-index",
                    "--",
                    null,
                    path,
                ],
                &[1],
            )
            .await
    }

    /// Commits exactly `paths` (as they are in the working tree) and nothing
    /// else: untracked ones are added first, and anything else already in
    /// the index stays there, uncommitted. Returns the new commit's id.
    pub async fn commit_paths(
        &self,
        repo: &Path,
        paths: &[String],
        message: &str,
        description: &str,
    ) -> Result<String> {
        let message = message.trim();
        if message.is_empty() {
            return Err(Error::Invalid("Write a commit message first.".into()));
        }
        if paths.is_empty() {
            return Err(Error::Invalid("Tick at least one file to commit.".into()));
        }
        let changes = self.changes(repo).await?;
        let by_path: HashMap<&str, &FileChange> =
            changes.iter().map(|c| (c.path.as_str(), c)).collect();
        let mut untracked: Vec<&str> = Vec::new();
        let mut specs: Vec<&str> = Vec::new();
        for path in paths {
            check_path(path)?;
            let Some(change) = by_path.get(path.as_str()) else {
                return Err(Error::Invalid(format!("{path} has no changes to commit.")));
            };
            if change.kind == ChangeKind::Conflicted {
                return Err(Error::Invalid(format!(
                    "{path} still has a merge conflict. Sort that out first."
                )));
            }
            if change.kind == ChangeKind::Untracked {
                untracked.push(path);
            }
            if let Some(old) = change.old_path.as_deref() {
                specs.push(old);
            }
            specs.push(path);
        }
        let git = self.literal();
        if !untracked.is_empty() {
            let mut args = vec!["add", "--"];
            args.extend(untracked.iter().copied());
            git.run(Some(repo), args).await?;
        }
        let mut args = vec!["commit", "--quiet", "--only", "-m", message];
        let description = description.trim();
        if !description.is_empty() {
            args.extend(["-m", description]);
        }
        args.push("--");
        args.extend(specs.iter().copied());
        if let Err(e) = git.run(Some(repo), args).await {
            // Leave things as they were: take the files we added back out.
            if !untracked.is_empty() {
                let mut args = vec!["rm", "--quiet", "--cached", "--"];
                args.extend(untracked.iter().copied());
                let _ = git.run(Some(repo), args).await;
            }
            return Err(e);
        }
        self.run(Some(repo), ["rev-parse", "HEAD"]).await
    }

    /// Throws away the changes to `paths`: tracked files go back to the last
    /// commit, new files are deleted. There is no undo.
    pub async fn discard_paths(&self, repo: &Path, paths: &[String]) -> Result<()> {
        if paths.is_empty() {
            return Ok(());
        }
        let changes = self.changes(repo).await?;
        let by_path: HashMap<&str, &FileChange> =
            changes.iter().map(|c| (c.path.as_str(), c)).collect();
        let git = self.literal();
        for path in paths {
            check_path(path)?;
            let Some(change) = by_path.get(path.as_str()) else {
                continue;
            };
            match change.kind {
                ChangeKind::Untracked => {
                    git.run(Some(repo), ["clean", "--quiet", "--force", "--", path])
                        .await?;
                }
                ChangeKind::Added => {
                    git.run(
                        Some(repo),
                        ["rm", "--quiet", "--force", "--cached", "--", path],
                    )
                    .await?;
                    git.run(Some(repo), ["clean", "--quiet", "--force", "--", path])
                        .await?;
                }
                ChangeKind::Renamed => {
                    let old = change.old_path.as_deref().unwrap_or(path);
                    git.run(
                        Some(repo),
                        ["rm", "--quiet", "--force", "--cached", "--", path],
                    )
                    .await?;
                    git.run(Some(repo), ["clean", "--quiet", "--force", "--", path])
                        .await?;
                    git.run(
                        Some(repo),
                        [
                            "restore",
                            "--source=HEAD",
                            "--staged",
                            "--worktree",
                            "--",
                            old,
                        ],
                    )
                    .await?;
                }
                ChangeKind::Modified | ChangeKind::Deleted | ChangeKind::Conflicted => {
                    git.run(
                        Some(repo),
                        [
                            "restore",
                            "--source=HEAD",
                            "--staged",
                            "--worktree",
                            "--",
                            path,
                        ],
                    )
                    .await?;
                }
            }
        }
        Ok(())
    }

    /// True when some remote-tracking branch already contains `rev`.
    pub async fn is_pushed(&self, repo: &Path, rev: &str) -> Result<bool> {
        let out = self
            .try_run(
                repo,
                &[
                    "for-each-ref",
                    "--count=1",
                    "--contains",
                    rev,
                    "refs/remotes",
                ],
            )
            .await?
            .unwrap_or_default();
        Ok(!out.trim().is_empty())
    }

    /// Undoes the last commit, keeping its changes staged (`reset --soft`).
    /// Refused when the commit is already pushed, is the first one, or is a
    /// merge. Returns the subject of the commit that was undone.
    pub async fn undo_last_commit(&self, repo: &Path) -> Result<String> {
        if !self.has_head(repo).await? {
            return Err(Error::Invalid("There is no commit to undo.".into()));
        }
        let parents = self
            .run(Some(repo), ["rev-list", "--parents", "-n", "1", "HEAD"])
            .await?;
        match parents.split_whitespace().count() {
            0 | 1 => {
                return Err(Error::Invalid(
                    "The first commit of a repository can’t be undone here.".into(),
                ))
            }
            2 => {}
            _ => {
                return Err(Error::Invalid(
                    "The last commit is a merge. Undo it in your git tool.".into(),
                ))
            }
        }
        if self.is_pushed(repo, "HEAD").await? {
            return Err(Error::Invalid(
                "The last commit is already pushed, so undoing it here would rewrite history."
                    .into(),
            ));
        }
        let subject = self
            .run(Some(repo), ["log", "-1", "--format=%s", "HEAD"])
            .await?;
        self.run(Some(repo), ["reset", "--quiet", "--soft", "HEAD~1"])
            .await?;
        Ok(subject)
    }

    /// Local branches, the current one first, then by name.
    pub async fn branches(&self, repo: &Path) -> Result<Vec<Branch>> {
        let format = "%(HEAD)%1f%(refname:short)%1f%(upstream:short)%1f%(upstream:track,nobracket)%1f%(objectname:short)%1f%(contents:subject)";
        let out = self
            .run_raw(
                Some(repo),
                ["for-each-ref", &format!("--format={format}"), "refs/heads"],
                &[],
            )
            .await?;
        let mut branches: Vec<Branch> = out
            .lines()
            .filter_map(|line| {
                let mut parts = line.split(FIELD);
                let current = parts.next()? == "*";
                let name = parts.next()?.to_string();
                let upstream = Some(parts.next()?.to_string()).filter(|u| !u.is_empty());
                let (ahead, behind) = parse_track(parts.next()?);
                Some(Branch {
                    name,
                    current,
                    upstream,
                    ahead,
                    behind,
                    sha: parts.next()?.to_string(),
                    subject: parts.next().unwrap_or_default().to_string(),
                })
            })
            .collect();
        branches.sort_by(|a, b| b.current.cmp(&a.current).then(a.name.cmp(&b.name)));
        Ok(branches)
    }

    async fn check_branch_name(&self, name: &str) -> Result<()> {
        let ok = !name.is_empty()
            && !name.starts_with('-')
            && self
                .try_run(Path::new("."), &["check-ref-format", "--branch", name])
                .await?
                .is_some();
        if ok {
            Ok(())
        } else {
            Err(Error::Invalid(format!("“{name}” can’t be a branch name.")))
        }
    }

    /// Switches to `name`: a local branch, or a branch of the remote that
    /// gets a local one tracking it. Uncommitted changes come along when
    /// they don't touch what differs between the branches; when they would
    /// be overwritten nothing is touched and the answer says so.
    pub async fn switch_branch(&self, repo: &Path, name: &str) -> Result<()> {
        self.check_branch_name(name).await?;
        match self.run(Some(repo), ["switch", "--quiet", name]).await {
            Ok(_) => Ok(()),
            Err(Error::Git { message, .. })
                if message.contains("would be overwritten")
                    || message.contains("commit your changes or stash them") =>
            {
                let files: Vec<&str> = message
                    .split(" · ")
                    .skip(1)
                    .take_while(|part| !part.starts_with("Please") && !part.starts_with("Aborting"))
                    .collect();
                let what = match files.as_slice() {
                    [] => "some files".to_string(),
                    [one] => (*one).to_string(),
                    [one, rest @ ..] => format!("{one} and {} more", rest.len()),
                };
                Err(Error::Invalid(format!(
                    "Switching to {name} would lose your uncommitted changes to {what}. Commit or discard them first."
                )))
            }
            Err(Error::Git { message, .. }) if message.contains("invalid reference") => Err(
                Error::Invalid(format!("There is no branch called {name} here.")),
            ),
            Err(e) => Err(e),
        }
    }

    /// Makes a new branch at `start` (a commit id; the current commit when
    /// `None`) and switches to it.
    pub async fn create_branch(&self, repo: &Path, name: &str, start: Option<&str>) -> Result<()> {
        self.check_branch_name(name).await?;
        let mut args = vec!["switch", "--quiet", "-c", name];
        if let Some(start) = start {
            args.push(check_sha(start)?);
        }
        self.run(Some(repo), args)
            .await
            .map(|_| ())
            .map_err(|e| match e {
                Error::Git { message, .. } if message.contains("already exists") => {
                    Error::Invalid(format!("There is already a branch called {name}."))
                }
                other => other,
            })
    }

    /// Pulls the current branch, fast-forward only. When the branch and its
    /// upstream have both moved on nothing changes and the answer says so.
    pub async fn pull_ff(&self, repo: &Path) -> Result<Pulled> {
        let before = self.try_run(repo, &["rev-parse", "HEAD"]).await?;
        match self.run(Some(repo), ["pull", "--quiet", "--ff-only"]).await {
            Ok(_) => {}
            Err(Error::Git { message, .. })
                if message.contains("Not possible to fast-forward")
                    || message.contains("divergent")
                    || message.contains("diverging") =>
            {
                return Err(Error::Invalid(
                    "This branch and its upstream have both moved on, so it can’t simply be pulled. Merge or rebase in your git tool."
                        .into(),
                ))
            }
            Err(Error::Git { message, .. }) if message.contains("no tracking information") => {
                return Err(Error::Invalid(
                    "This branch doesn’t follow a branch in your studio yet. Push it first.".into(),
                ))
            }
            Err(Error::Git { message, .. }) if message.contains("would be overwritten") => {
                return Err(Error::Invalid(
                    "Pulling would overwrite your uncommitted changes. Commit or discard them first."
                        .into(),
                ))
            }
            Err(e) => return Err(e),
        }
        let commits = match before {
            Some(before) if !before.is_empty() => self
                .try_run(repo, &["rev-list", "--count", &format!("{before}..HEAD")])
                .await?
                .and_then(|n| n.parse().ok())
                .unwrap_or(0),
            _ => 0,
        };
        Ok(Pulled { commits })
    }

    /// The current branch together with its upstream, newest first, at most
    /// `limit` commits, as a two-lane graph.
    pub async fn history(&self, repo: &Path, limit: usize) -> Result<History> {
        let mut history = History {
            branch: self.current_branch(repo).await?,
            ..History::default()
        };
        if !self.has_head(repo).await? {
            return Ok(history);
        }
        history.upstream = self
            .try_run(
                repo,
                &[
                    "rev-parse",
                    "--abbrev-ref",
                    "--symbolic-full-name",
                    "@{upstream}",
                ],
            )
            .await?
            .filter(|u| !u.is_empty());

        // Which commits are only here, and which only in the upstream.
        let (mut only_here, mut only_there) = (HashSet::new(), HashSet::new());
        if history.upstream.is_some() {
            let sides = self
                .try_run(repo, &["rev-list", "--left-right", "HEAD...@{upstream}"])
                .await?
                .unwrap_or_default();
            for line in sides.lines() {
                if let Some(sha) = line.strip_prefix('<') {
                    only_here.insert(sha.to_string());
                } else if let Some(sha) = line.strip_prefix('>') {
                    only_there.insert(sha.to_string());
                }
            }
        }
        history.ahead = only_here.len() as u64;
        history.behind = only_there.len() as u64;

        let limit = limit.clamp(1, 2000);
        let format =
            format!("--format=%H{FIELD}%h{FIELD}%P{FIELD}%an{FIELD}%aI{FIELD}%s{FIELD}%D{RECORD}");
        let count = format!("--max-count={}", limit + 1);
        let mut args = vec![
            "log",
            "--topo-order",
            "--decorate=full",
            "--no-color",
            count.as_str(),
            format.as_str(),
            "HEAD",
        ];
        if history.upstream.is_some() {
            args.push("@{upstream}");
        }
        let out = self.run_raw(Some(repo), args, &[]).await?;
        let mut commits: Vec<Commit> = out
            .split(RECORD)
            .filter_map(|record| {
                let record = record.trim_start_matches(['\n', '\r']);
                let mut parts = record.split(FIELD);
                let sha = parts.next().filter(|s| is_sha(s))?.to_string();
                let short = parts.next()?.to_string();
                let parents = parts
                    .next()?
                    .split_whitespace()
                    .map(str::to_string)
                    .collect();
                let author = parts.next()?.to_string();
                let date = parts.next()?.to_string();
                let subject = parts.next()?.to_string();
                let refs = parse_decoration(parts.next().unwrap_or_default());
                let unpushed = only_here.contains(&sha);
                let incoming = only_there.contains(&sha);
                Some(Commit {
                    sha,
                    short,
                    parents,
                    author,
                    date,
                    subject,
                    refs,
                    lane: u8::from(unpushed),
                    line0: Segment::None,
                    line1: Segment::None,
                    unpushed,
                    incoming,
                })
            })
            .collect();
        history.more = commits.len() > limit;
        commits.truncate(limit);
        lay_out(&mut commits);
        history.commits = commits;
        Ok(history)
    }

    /// One commit: message, author, the files it touched.
    pub async fn commit_detail(&self, repo: &Path, sha: &str) -> Result<CommitDetail> {
        let sha = check_sha(sha)?;
        let format =
            format!("--format=%H{FIELD}%h{FIELD}%an{FIELD}%ae{FIELD}%aI{FIELD}%s{FIELD}%b");
        let rev = format!("{sha}^{{commit}}");
        let out = self
            .run_raw(Some(repo), ["log", "-1", "--no-color", &format, &rev], &[])
            .await
            .map_err(|e| match e {
                Error::Git { .. } => Error::Invalid(format!("There is no commit {sha} here.")),
                other => other,
            })?;
        let mut parts = out.trim_end().split(FIELD);
        let mut next = || parts.next().unwrap_or_default().to_string();
        let (full, short, author, email, date, subject, body) =
            (next(), next(), next(), next(), next(), next(), next());
        let numstat = self
            .run_raw(
                Some(repo),
                [
                    "-c",
                    "core.quotePath=false",
                    "show",
                    "--no-color",
                    "--numstat",
                    "-z",
                    "--no-renames",
                    "--diff-merges=first-parent",
                    "--format=",
                    &rev,
                ],
                &[],
            )
            .await?;
        let mut files: Vec<FileStat> = parse_numstat(&numstat)
            .into_iter()
            .map(|(path, (additions, deletions))| FileStat {
                path: path.to_string(),
                additions,
                deletions,
            })
            .collect();
        files.sort_by(|a, b| a.path.cmp(&b.path));
        let head = self.try_run(repo, &["rev-parse", "HEAD"]).await?;
        Ok(CommitDetail {
            pushed: self.is_pushed(repo, &full).await?,
            is_head: head.as_deref() == Some(full.as_str()),
            sha: full,
            short,
            author,
            email,
            date,
            subject,
            body: body.trim().to_string(),
            files,
        })
    }

    /// Sets who commits in this repository, in its own `.git/config` only.
    /// The global git config is never touched.
    pub async fn set_identity(&self, repo: &Path, name: &str, email: &str) -> Result<()> {
        if !name.trim().is_empty() {
            self.run(Some(repo), ["config", "--local", "user.name", name.trim()])
                .await?;
        }
        if !email.trim().is_empty() {
            self.run(
                Some(repo),
                ["config", "--local", "user.email", email.trim()],
            )
            .await?;
        }
        Ok(())
    }

    /// Who commits here (`user.name`, `user.email`), from whichever config
    /// git would use.
    pub async fn identity(&self, repo: &Path) -> Result<(Option<String>, Option<String>)> {
        let name = self.try_run(repo, &["config", "user.name"]).await?;
        let email = self.try_run(repo, &["config", "user.email"]).await?;
        Ok((
            name.filter(|n| !n.is_empty()),
            email.filter(|e| !e.is_empty()),
        ))
    }

    /// How many commits `git rev-list --count <args>` finds; 0 when there
    /// are none or no HEAD.
    pub async fn count_commits(&self, repo: &Path, args: &[&str]) -> Result<u64> {
        let mut full = vec!["rev-list", "--count"];
        full.extend(args);
        Ok(self
            .try_run(repo, &full)
            .await?
            .and_then(|n| n.trim().parse().ok())
            .unwrap_or(0))
    }

    /// When this repository last fetched, if it ever did.
    pub async fn last_fetch(&self, repo: &Path) -> Result<Option<std::time::SystemTime>> {
        let Some(git_dir) = self
            .try_run(repo, &["rev-parse", "--absolute-git-dir"])
            .await?
        else {
            return Ok(None);
        };
        Ok(std::fs::metadata(PathBuf::from(git_dir).join("FETCH_HEAD"))
            .and_then(|m| m.modified())
            .ok())
    }

    /// Makes a new repository in `dir` (created if missing) on `branch`.
    pub async fn init(&self, dir: &Path, branch: &str) -> Result<()> {
        std::fs::create_dir_all(dir)
            .map_err(|e| Error::io(format!("Couldn’t create {}", dir.display()), e))?;
        self.check_branch_name(branch).await?;
        self.run(Some(dir), ["init", "--quiet", "-b", branch])
            .await
            .map(|_| ())
    }
}

/// `git status --porcelain=v1 -z`: `XY path\0`, and for renames and copies
/// `XY new\0old\0`.
fn parse_status(out: &str) -> Vec<FileChange> {
    let mut changes = Vec::new();
    let mut entries = out.split('\0');
    while let Some(entry) = entries.next() {
        if entry.len() < 4 {
            continue;
        }
        let (x, y) = (entry.as_bytes()[0], entry.as_bytes()[1]);
        let Some(path) = entry.get(3..) else { continue };
        let mut old_path = None;
        if matches!(x, b'R' | b'C') || matches!(y, b'R' | b'C') {
            old_path = entries.next().map(str::to_string);
        }
        let kind = match (x, y) {
            (b'?', b'?') => ChangeKind::Untracked,
            (b'!', b'!') => continue,
            (b'U', _) | (_, b'U') | (b'A', b'A') | (b'D', b'D') => ChangeKind::Conflicted,
            (b'R', _) | (_, b'R') => ChangeKind::Renamed,
            (_, b'D') | (b'D', _) => ChangeKind::Deleted,
            (b'A', _) | (b'C', _) => ChangeKind::Added,
            _ => ChangeKind::Modified,
        };
        // A copy is a new file as far as committing goes.
        if kind == ChangeKind::Added {
            old_path = None;
        }
        changes.push(FileChange {
            path: path.to_string(),
            old_path,
            kind,
            staged: !matches!(x, b' ' | b'?'),
            additions: None,
            deletions: None,
            binary: false,
        });
    }
    changes
}

/// `git diff --numstat -z --no-renames`: `added\tdeleted\tpath\0`, with `-`
/// for both counts on a binary file.
fn parse_numstat(out: &str) -> HashMap<&str, (Option<u32>, Option<u32>)> {
    out.split('\0')
        .filter_map(|entry| {
            let mut parts = entry.trim_start_matches('\n').splitn(3, '\t');
            let additions = parts.next()?;
            let deletions = parts.next()?;
            let path = parts.next().filter(|p| !p.is_empty())?;
            Some((path, (additions.parse().ok(), deletions.parse().ok())))
        })
        .collect()
}

/// `ahead 2, behind 1` / `gone` / empty.
fn parse_track(track: &str) -> (u64, u64) {
    let mut ahead = 0;
    let mut behind = 0;
    for part in track.split(',') {
        let mut words = part.split_whitespace();
        match (words.next(), words.next().and_then(|n| n.parse().ok())) {
            (Some("ahead"), Some(n)) => ahead = n,
            (Some("behind"), Some(n)) => behind = n,
            _ => {}
        }
    }
    (ahead, behind)
}

/// `HEAD -> refs/heads/main, refs/remotes/origin/main, tag: refs/tags/v1`.
fn parse_decoration(decoration: &str) -> Vec<RefLabel> {
    let mut labels = Vec::new();
    for part in decoration.split(", ") {
        let part = part.trim();
        let (is_head, name) = match part.strip_prefix("HEAD -> ") {
            Some(rest) => (true, rest),
            None => (false, part),
        };
        let name = name.strip_prefix("tag: ").unwrap_or(name);
        if let Some(branch) = name.strip_prefix("refs/heads/") {
            labels.push(RefLabel {
                kind: if is_head {
                    RefKind::Head
                } else {
                    RefKind::Branch
                },
                name: branch.to_string(),
                remote: None,
            });
        } else if let Some(rest) = name.strip_prefix("refs/remotes/") {
            if let Some((remote, branch)) = rest.split_once('/') {
                if branch != "HEAD" {
                    labels.push(RefLabel {
                        kind: RefKind::Remote,
                        name: branch.to_string(),
                        remote: Some(remote.to_string()),
                    });
                }
            }
        } else if let Some(tag) = name.strip_prefix("refs/tags/") {
            labels.push(RefLabel {
                kind: RefKind::Tag,
                name: tag.to_string(),
                remote: None,
            });
        }
    }
    labels
}

/// Fills in how the two lanes cross each row. Lane 0 (shared with the
/// upstream) starts at its first commit and runs to the bottom. Lane 1
/// (commits only here) starts at its first commit and bends into lane 0 at
/// the first commit both sides have.
fn lay_out(commits: &mut [Commit]) {
    let first0 = commits.iter().position(|c| c.lane == 0);
    let first1 = commits.iter().position(|c| c.lane == 1);
    let last1 = commits.iter().rposition(|c| c.lane == 1);
    // Where lane 1 lands: the first shared commit after the last local one.
    let join = last1.and_then(|last| {
        commits
            .iter()
            .enumerate()
            .skip(last + 1)
            .find(|(_, c)| c.lane == 0 && !c.incoming)
            .map(|(i, _)| i)
    });
    for (i, commit) in commits.iter_mut().enumerate() {
        commit.line0 = match first0 {
            Some(first) if i == first => Segment::Start,
            Some(first) if i > first => Segment::Full,
            _ => Segment::None,
        };
        commit.line1 = match (first1, join) {
            (Some(first), _) if i == first => Segment::Start,
            (Some(first), Some(join)) if i > first && i < join => Segment::Full,
            (Some(_), Some(join)) if i == join => Segment::Join,
            (Some(first), None) if i > first => Segment::Full,
            _ => Segment::None,
        };
    }
}

enum Counted {
    Lines(u32),
    Binary,
    Skipped,
}

fn count_lines(path: &Path) -> Counted {
    let Ok(meta) = std::fs::symlink_metadata(path) else {
        return Counted::Skipped;
    };
    if !meta.is_file() || meta.len() > COUNT_LIMIT {
        return Counted::Skipped;
    }
    let Ok(bytes) = std::fs::read(path) else {
        return Counted::Skipped;
    };
    if bytes.contains(&0) {
        return Counted::Binary;
    }
    let newlines = bytes.iter().filter(|&&b| b == b'\n').count();
    let unfinished = usize::from(bytes.last().is_some_and(|&b| b != b'\n'));
    Counted::Lines((newlines + unfinished) as u32)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn status_entries_are_read_with_renames_and_spaces() {
        let out = " M src/a.rs\0A  new file.txt\0R  moved.rs\0old.rs\0?? notes/todo.md\0 D gone.rs\0UU both.rs\0MM half.rs\0";
        let changes = parse_status(out);
        let summary: Vec<(&str, ChangeKind, bool)> = changes
            .iter()
            .map(|c| (c.path.as_str(), c.kind, c.staged))
            .collect();
        assert_eq!(
            summary,
            [
                ("src/a.rs", ChangeKind::Modified, false),
                ("new file.txt", ChangeKind::Added, true),
                ("moved.rs", ChangeKind::Renamed, true),
                ("notes/todo.md", ChangeKind::Untracked, false),
                ("gone.rs", ChangeKind::Deleted, false),
                ("both.rs", ChangeKind::Conflicted, true),
                ("half.rs", ChangeKind::Modified, true),
            ]
        );
        assert_eq!(changes[2].old_path.as_deref(), Some("old.rs"));
        assert_eq!(changes[0].old_path, None);
    }

    #[test]
    fn numstat_reads_counts_and_binary() {
        let counts = parse_numstat("3\t1\tsrc/a b.rs\0-\t-\timg.png\0");
        assert_eq!(counts["src/a b.rs"], (Some(3), Some(1)));
        assert_eq!(counts["img.png"], (None, None));
    }

    #[test]
    fn track_and_decoration() {
        assert_eq!(parse_track("ahead 2, behind 1"), (2, 1));
        assert_eq!(parse_track("behind 4"), (0, 4));
        assert_eq!(parse_track("gone"), (0, 0));
        let labels = parse_decoration(
            "HEAD -> refs/heads/feature/x, refs/remotes/origin/feature/x, refs/remotes/origin/HEAD, tag: refs/tags/v0.9, refs/heads/main",
        );
        let flat: Vec<(RefKind, &str, Option<&str>)> = labels
            .iter()
            .map(|l| (l.kind, l.name.as_str(), l.remote.as_deref()))
            .collect();
        assert_eq!(
            flat,
            [
                (RefKind::Head, "feature/x", None),
                (RefKind::Remote, "feature/x", Some("origin")),
                (RefKind::Tag, "v0.9", None),
                (RefKind::Branch, "main", None),
            ]
        );
        assert!(parse_decoration("").is_empty());
    }

    #[test]
    fn paths_and_ids_are_checked() {
        assert!(check_path("src/a.rs").is_ok());
        assert!(
            check_path("-rf").is_ok(),
            "literal pathspecs after -- are safe"
        );
        assert!(check_path("../outside").is_err());
        assert!(check_path("/etc/passwd").is_err());
        assert!(check_path(".git/config").is_err());
        assert!(check_path("").is_err());
        assert!(check_sha("7f82d31").is_ok());
        assert!(check_sha("--all").is_err());
        assert!(check_sha("HEAD").is_err());
    }
}
