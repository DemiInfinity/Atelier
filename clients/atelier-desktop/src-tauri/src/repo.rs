//! One working copy: its bar, changed files and diffs, committing exactly
//! the ticked files, history, branches, fetch / pull / push, and the two
//! small undo actions.

use std::collections::HashMap;
use std::path::Path;
use std::time::Duration;

use atelier_core::api::Repo;
use atelier_core::diff::{self, DiffLine, FileStatus};
use atelier_core::git::{
    Branch, ChangeKind, CommitDetail, FileChange, RefKind, RemoteRef, Segment,
};
use atelier_core::messages::Vars;
use serde::Serialize;

use crate::desk::{local_now, Desk};
use crate::error::{CmdError, CmdResult};
use crate::notify::{self, Target, Toast};
use crate::tools;

/// Lines of one file's diff sent to the window; longer diffs are cut.
const MAX_DIFF_LINES: usize = 4000;
const HISTORY_LIMIT: usize = 300;

/// The bar above a repository's tabs.
#[derive(Clone, Debug, Serialize)]
pub struct RepoView {
    pub path: String,
    pub display_path: String,
    pub name: String,
    /// The account's display name, or empty when the studio doesn't know it.
    pub owner: String,
    pub full_name: Option<String>,
    pub branch: Option<String>,
    pub upstream: Option<String>,
    pub ahead: u64,
    pub behind: u64,
    pub changes: usize,
    pub branches: usize,
    pub open_pulls: u64,
    pub web_path: Option<String>,
    pub default_branch: Option<String>,
    pub on_github: bool,
    pub editor: tools::PresetInfo,
    /// `Demi · personal identity`.
    pub committing_as: Option<String>,
    /// RFC 3339, when this copy last fetched.
    pub fetched_at: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct ChangesView {
    pub files: Vec<FileChange>,
    /// A friendly line for when there is nothing to commit.
    pub clean: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct DiffView {
    pub path: String,
    pub old_path: Option<String>,
    /// `Go`, `Markdown · new file`.
    pub lang: String,
    pub status: FileStatus,
    pub binary: bool,
    pub additions: u32,
    pub deletions: u32,
    pub lines: Vec<DiffLine>,
    pub truncated: bool,
}

#[derive(Clone, Debug, Serialize)]
pub struct CommitResult {
    pub sha: String,
    pub short: String,
    pub pushed: bool,
    /// The commit was made but the push failed; why.
    pub push_error: Option<String>,
    pub toast: Option<Toast>,
}

#[derive(Clone, Debug, Serialize)]
pub struct PushResult {
    pub commits: u64,
    pub branch: String,
    pub toast: Option<Toast>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct Pill {
    pub text: String,
    /// `yours` (amber), `github` (blue) or `quiet` (grey).
    pub tone: &'static str,
}

#[derive(Clone, Debug, Serialize)]
pub struct HistoryRow {
    pub sha: String,
    pub short: String,
    pub subject: String,
    pub author: String,
    pub date: String,
    pub pills: Vec<Pill>,
    pub lane: u8,
    pub line0: Segment,
    pub line1: Segment,
    pub unpushed: bool,
    pub incoming: bool,
}

#[derive(Clone, Debug, Serialize)]
pub struct HistoryView {
    pub branch: Option<String>,
    pub upstream: Option<String>,
    pub ahead: u64,
    pub behind: u64,
    pub rows: Vec<HistoryRow>,
    pub more: bool,
}

#[derive(Clone, Debug, Serialize)]
pub struct CommitView {
    #[serde(flatten)]
    pub detail: CommitDetail,
    /// "Undo this commit" would work: it is the unpushed tip of the branch.
    pub can_undo: bool,
}

/// A language name for a file, from its extension.
pub fn language(path: &str) -> &'static str {
    let name = path.rsplit('/').next().unwrap_or(path);
    let ext = name.rsplit_once('.').map(|(_, e)| e.to_ascii_lowercase());
    match (name, ext.as_deref()) {
        ("Dockerfile", _) => "Dockerfile",
        ("Makefile", _) => "Makefile",
        (_, Some("rs")) => "Rust",
        (_, Some("go")) => "Go",
        (_, Some("ts" | "tsx")) => "TypeScript",
        (_, Some("js" | "jsx" | "mjs" | "cjs")) => "JavaScript",
        (_, Some("py")) => "Python",
        (_, Some("rb")) => "Ruby",
        (_, Some("swift")) => "Swift",
        (_, Some("kt" | "kts")) => "Kotlin",
        (_, Some("java")) => "Java",
        (_, Some("c" | "h")) => "C",
        (_, Some("cc" | "cpp" | "hpp")) => "C++",
        (_, Some("cs")) => "C#",
        (_, Some("php")) => "PHP",
        (_, Some("dart")) => "Dart",
        (_, Some("sh" | "bash" | "zsh")) => "Shell",
        (_, Some("md" | "markdown")) => "Markdown",
        (_, Some("html" | "htm")) => "HTML",
        (_, Some("css" | "scss")) => "CSS",
        (_, Some("json")) => "JSON",
        (_, Some("yml" | "yaml")) => "YAML",
        (_, Some("toml")) => "TOML",
        (_, Some("sql")) => "SQL",
        (_, Some("svelte")) => "Svelte",
        (_, Some("vue")) => "Vue",
        (_, Some("txt")) => "Text",
        _ => "",
    }
}

fn lang_label(path: &str, status: FileStatus, binary: bool) -> String {
    let mut parts: Vec<&str> = Vec::new();
    let lang = language(path);
    if binary {
        parts.push("Binary");
    } else if !lang.is_empty() {
        parts.push(lang);
    }
    match status {
        FileStatus::Added => parts.push("new file"),
        FileStatus::Deleted => parts.push("deleted"),
        FileStatus::Renamed => parts.push("renamed"),
        FileStatus::Copied => parts.push("copied"),
        FileStatus::Modified => {}
    }
    parts.join(" · ")
}

/// The pills for a commit's refs, in the three colour threads.
pub fn pills(
    refs: &[atelier_core::git::RefLabel],
    default_branch: Option<&str>,
    remote_kinds: &HashMap<String, &'static str>,
) -> Vec<Pill> {
    let locals: Vec<&str> = refs
        .iter()
        .filter(|r| matches!(r.kind, RefKind::Head | RefKind::Branch))
        .map(|r| r.name.as_str())
        .collect();
    let is_default = |name: &str| match default_branch {
        Some(default) => name == default,
        None => matches!(name, "main" | "master"),
    };
    let mut out: Vec<Pill> = Vec::new();
    for label in refs {
        let pill = match label.kind {
            RefKind::Head | RefKind::Branch => Pill {
                text: label.name.clone(),
                tone: if is_default(&label.name) {
                    "yours"
                } else {
                    "quiet"
                },
            },
            RefKind::Tag => Pill {
                text: label.name.clone(),
                tone: "quiet",
            },
            RefKind::Remote => {
                let remote = label.remote.as_deref().unwrap_or_default();
                let same = locals.contains(&label.name.as_str()) || is_default(&label.name);
                let (word, tone) = match remote_kinds.get(remote).copied() {
                    Some("studio") => ("studio".to_string(), "yours"),
                    Some("github") => ("GitHub".to_string(), "github"),
                    _ => (remote.to_string(), "quiet"),
                };
                Pill {
                    text: if same && tone != "quiet" {
                        word
                    } else {
                        format!("{word}/{}", label.name)
                    },
                    tone,
                }
            }
        };
        if !out.contains(&pill) {
            out.push(pill);
        }
    }
    out
}

fn rfc3339(at: std::time::SystemTime) -> Option<String> {
    time::OffsetDateTime::from(at)
        .format(&time::format_description::well_known::Rfc3339)
        .ok()
}

impl Desk {
    /// Which of a working copy's remotes is the studio (`studio`), which is
    /// GitHub (`github`), and which is something else (`other`).
    pub async fn remote_kinds(&self, repo: &Path) -> CmdResult<HashMap<String, &'static str>> {
        let local = self.git().inspect(repo).await?;
        let _ = self.studio_repo_of(&local).await;
        let studio_remote = self
            .repos_cached()
            .and_then(|repos| local.studio_remote(&repos).map(|(r, _)| r.name.clone()));
        let mut hosts: Vec<String> = Vec::new();
        if let Some(host) = self
            .settings()
            .studio()
            .and_then(|url| url::Url::parse(&url).ok())
            .and_then(|u| u.host_str().map(str::to_lowercase))
        {
            hosts.push(host);
        }
        if let Some(ssh) = self.me_cached().and_then(|me| me.studio.ssh) {
            hosts.push(ssh.host.to_lowercase());
        }
        Ok(local
            .remotes
            .iter()
            .map(|remote| {
                let host = remote.remote.as_ref().map(|r| r.host.as_str());
                let kind = match host {
                    Some("github.com") => "github",
                    Some(host) if hosts.iter().any(|h| h == host) => "studio",
                    _ if studio_remote.as_deref() == Some(remote.name.as_str()) => "studio",
                    _ => "other",
                };
                (remote.name.clone(), kind)
            })
            .collect())
    }

    /// The studio repository a working copy belongs to, if any.
    pub(crate) async fn studio_repo_of(
        &self,
        local: &atelier_core::git::LocalRepo,
    ) -> Option<Repo> {
        let repos = match self.repos(Duration::from_secs(300)).await {
            Ok(repos) => repos,
            Err(_) => self.repos_cached()?,
        };
        local.studio_repo(&repos).cloned()
    }

    pub async fn repo_view(&self, path: &str) -> CmdResult<RepoView> {
        let repo = self.repo_path(path).await?;
        let git = self.git();
        let local = git.inspect(&repo).await?;
        let studio = self.studio_repo_of(&local).await;
        let _ = self.me(Duration::from_secs(300)).await;
        let changes = git.changes(&repo).await?.len();
        let branches = git.branches(&repo).await?.len();
        let (name, email) = git.identity(&repo).await?;
        let settings = self.settings();
        let editor = tools::editor_or_default(settings.config.editor.as_deref());
        let info = |p: &tools::Preset| tools::PresetInfo {
            id: p.id.to_string(),
            label: p.label.to_string(),
        };
        let account_kind = studio.as_ref().map(|r| r.owner_kind.as_str());
        let committing_as = name.map(|name| {
            let kind = match account_kind {
                Some("user") => "personal identity".to_string(),
                Some("company") => "company identity".to_string(),
                _ => email.clone().unwrap_or_default(),
            };
            if kind.is_empty() {
                name
            } else {
                format!("{name} · {kind}")
            }
        });
        Ok(RepoView {
            display_path: self.display_path(&repo),
            path: repo.to_string_lossy().into_owned(),
            name: studio
                .as_ref()
                .map_or_else(|| local.folder_name(), |r| r.name.clone()),
            owner: studio
                .as_ref()
                .map(|r| self.account_label(&r.owner))
                .unwrap_or_default(),
            full_name: studio.as_ref().map(|r| r.full_name.clone()),
            branch: local.branch.clone(),
            upstream: local.upstream.clone(),
            ahead: local.ahead.unwrap_or(0),
            behind: local.behind.unwrap_or(0),
            changes,
            branches,
            open_pulls: studio.as_ref().map_or(0, |r| r.open_pulls),
            web_path: studio.as_ref().map(|r| format!("/{}", r.full_name)),
            default_branch: studio
                .as_ref()
                .map(|r| r.default_branch.clone())
                .filter(|b| !b.is_empty()),
            on_github: studio.as_ref().is_some_and(|r| r.github.is_some()),
            editor: info(editor),
            committing_as,
            fetched_at: git.last_fetch(&repo).await?.and_then(rfc3339),
        })
    }

    pub async fn repo_changes(&self, path: &str) -> CmdResult<ChangesView> {
        let repo = self.repo_path(path).await?;
        let files = self.git().changes(&repo).await?;
        let clean = if files.is_empty() {
            let messages = self.messages(Duration::from_secs(600)).await.ok();
            messages
                .and_then(|m| atelier_core::messages::pick(&m, "clean", &Vars::new(), self.seed()))
        } else {
            None
        };
        Ok(ChangesView { files, clean })
    }

    /// One changed file as numbered lines. The file must be in the list of
    /// changes; nothing else can be read this way.
    pub async fn repo_diff(&self, path: &str, file: &str) -> CmdResult<DiffView> {
        let repo = self.repo_path(path).await?;
        let git = self.git();
        let changes = git.changes(&repo).await?;
        let change = changes
            .iter()
            .find(|c| c.path == file)
            .ok_or_else(|| CmdError::invalid(format!("{file} has no changes.")))?;
        let text = match change.kind {
            ChangeKind::Untracked => git.diff_untracked(&repo, file).await?,
            _ => {
                git.diff_worktree(&repo, file, change.old_path.as_deref())
                    .await?
            }
        };
        let parsed = diff::parse_limited(&text, MAX_DIFF_LINES)
            .into_iter()
            .next()
            .unwrap_or_default();
        let status = match change.kind {
            ChangeKind::Untracked | ChangeKind::Added => FileStatus::Added,
            ChangeKind::Deleted => FileStatus::Deleted,
            ChangeKind::Renamed => FileStatus::Renamed,
            _ => parsed.status,
        };
        Ok(DiffView {
            path: file.to_string(),
            old_path: change.old_path.clone(),
            lang: lang_label(file, status, parsed.binary || change.binary),
            status,
            binary: parsed.binary || change.binary,
            additions: parsed.additions,
            deletions: parsed.deletions,
            lines: parsed.lines,
            truncated: parsed.truncated,
        })
    }

    fn message_vars(&self, repo_name: &str, branch: &str, count: u64) -> Vars {
        let now = local_now();
        Vars::new()
            .set("repo", repo_name)
            .set("branch", branch)
            .set("count", count)
            .set("device", format!("your {}", self.device_name()))
            .set("time", format!("{:02}:{:02}", now.hour(), now.minute()))
    }

    async fn repo_label(&self, repo: &Path) -> String {
        match self.git().inspect(repo).await {
            Ok(local) => match self.studio_repo_of(&local).await {
                Some(studio) => studio.name,
                None => local.folder_name(),
            },
            Err(_) => String::new(),
        }
    }

    /// Commits exactly the ticked files, and pushes when asked.
    pub async fn repo_commit(
        &self,
        path: &str,
        files: &[String],
        message: &str,
        description: &str,
        push: bool,
    ) -> CmdResult<CommitResult> {
        let repo = self.repo_path(path).await?;
        let git = self.git();
        let sha = git.commit_paths(&repo, files, message, description).await?;
        let short: String = sha.chars().take(7).collect();
        let mut result = CommitResult {
            sha,
            short,
            pushed: false,
            push_error: None,
            toast: None,
        };
        if push {
            match self.repo_push(path).await {
                Ok(pushed) => {
                    result.pushed = true;
                    result.toast = pushed.toast;
                }
                Err(e) => result.push_error = Some(e.message),
            }
            return Ok(result);
        }
        // Not pushed: maybe a milestone, or the first commit of the day.
        let messages = self.messages(Duration::from_secs(600)).await.ok();
        let name = self.repo_label(&repo).await;
        let branch = git.current_branch(&repo).await?.unwrap_or_default();
        let total = git.count_commits(&repo, &["HEAD"]).await?;
        let now = local_now();
        let midnight = now.replace_time(time::Time::MIDNIGHT).unix_timestamp();
        let since = format!("--since={midnight}");
        let today = git.count_commits(&repo, &[since.as_str(), "HEAD"]).await?;
        let moment = if total > 0 && total % 100 == 0 {
            Some("mile")
        } else if today == 1 {
            Some("first")
        } else {
            None
        };
        if let Some(moment) = moment {
            result.toast = notify::local(
                moment,
                messages.as_ref(),
                &self.message_vars(&name, &branch, total),
                &name,
                Target::Repo {
                    path: path.to_string(),
                },
                self.seed(),
            );
        }
        Ok(result)
    }

    pub async fn repo_push(&self, path: &str) -> CmdResult<PushResult> {
        let repo = self.repo_path(path).await?;
        let git = self.git();
        let local = git.inspect(&repo).await?;
        let branch = local.branch.clone().ok_or_else(|| {
            CmdError::invalid("You’re not on a branch, so there is nothing to push.")
        })?;
        // Without an upstream the push goes to the studio's remote, whatever
        // it is called; a folder with no studio remote uses its only one.
        let repos = match self.repos(Duration::from_secs(300)).await {
            Ok(repos) => repos,
            Err(_) => self.repos_cached().unwrap_or_default(),
        };
        let remote = local
            .studio_remote(&repos)
            .map(|(remote, _)| remote.name.clone())
            .or_else(|| local.remote_name.clone())
            .ok_or_else(|| {
                CmdError::new("no_remote", "This folder isn’t in your studio yet.")
                    .hint("Publish it from “Add a repository”.")
            })?;
        let commits = match local.upstream {
            Some(_) => local.ahead.unwrap_or(0),
            None => git.count_commits(&repo, &["HEAD"]).await?,
        };
        git.push(&repo, &remote, local.upstream.is_none())
            .await
            .map_err(|e| match &e {
                atelier_core::Error::Git { message, .. }
                    if message.contains("rejected")
                        || message.contains("fetch first")
                        || message.contains("non-fast-forward") =>
                {
                    CmdError::new(
                        "push_rejected",
                        "Your studio has commits this machine doesn’t have yet, so the push was refused.",
                    )
                    .hint("Pull first, then push again.")
                }
                _ => CmdError::from(e),
            })?;
        let messages = self.messages(Duration::from_secs(600)).await.ok();
        let name = match self.studio_repo_of(&local).await {
            Some(studio) => studio.name,
            None => local.folder_name(),
        };
        let late = local_now().hour() < 5 && messages.as_ref().is_some_and(|m| m.rules.milestones);
        let sub = format!(
            "{name} · {}",
            if commits == 1 {
                "1 commit".to_string()
            } else {
                format!("{commits} commits")
            }
        );
        let toast = notify::local(
            if late { "late" } else { "push" },
            messages.as_ref(),
            &self.message_vars(&name, &branch, commits),
            &sub,
            Target::Repo {
                path: path.to_string(),
            },
            self.seed(),
        );
        Ok(PushResult {
            commits,
            branch,
            toast,
        })
    }

    pub async fn repo_fetch(&self, path: &str) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().fetch(&repo, None).await?)
    }

    /// Fast-forward only. Returns how many commits came in.
    pub async fn repo_pull(&self, path: &str) -> CmdResult<u64> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().pull_ff(&repo).await?.commits)
    }

    pub async fn repo_branches(&self, path: &str) -> CmdResult<Vec<Branch>> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().branches(&repo).await?)
    }

    /// Switches to a local branch. Refuses, touching nothing, when
    /// uncommitted changes would be lost.
    pub async fn repo_switch(&self, path: &str, branch: &str) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        let git = self.git();
        if !git.branches(&repo).await?.iter().any(|b| b.name == branch) {
            return Err(CmdError::invalid(format!(
                "There is no branch called {branch} here."
            )));
        }
        Ok(git.switch_branch(&repo, branch).await?)
    }

    pub async fn repo_new_branch(
        &self,
        path: &str,
        name: &str,
        from: Option<&str>,
    ) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().create_branch(&repo, name.trim(), from).await?)
    }

    /// Discards the changes to the chosen files. The window asks first.
    pub async fn repo_discard(&self, path: &str, files: &[String]) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().discard_paths(&repo, files).await?)
    }

    /// Undoes the last commit if it isn't pushed, keeping its changes.
    pub async fn repo_undo_commit(&self, path: &str) -> CmdResult<String> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().undo_last_commit(&repo).await?)
    }

    pub async fn repo_history(&self, path: &str) -> CmdResult<HistoryView> {
        let repo = self.repo_path(path).await?;
        let git = self.git();
        let history = git.history(&repo, HISTORY_LIMIT).await?;
        let local = git.inspect(&repo).await?;
        let studio = self.studio_repo_of(&local).await;
        let studio_remote_name = self
            .repos_cached()
            .and_then(|repos| local.studio_remote(&repos).map(|(r, _)| r.name.clone()));

        // Which remote is the studio, which is GitHub.
        let studio_hosts: Vec<String> = {
            let mut hosts = Vec::new();
            if let Some(url) = self.settings().studio() {
                if let Some(host) = url::Url::parse(&url)
                    .ok()
                    .and_then(|u| u.host_str().map(str::to_lowercase))
                {
                    hosts.push(host);
                }
            }
            if let Some(ssh) = self.me_cached().and_then(|me| me.studio.ssh) {
                hosts.push(ssh.host.to_lowercase());
            }
            hosts
        };
        let mut remote_kinds: HashMap<String, &'static str> = HashMap::new();
        for commit in &history.commits {
            for label in &commit.refs {
                let Some(remote) = label.remote.as_deref() else {
                    continue;
                };
                if remote_kinds.contains_key(remote) {
                    continue;
                }
                let url = git.remote_url(&repo, remote).await?.unwrap_or_default();
                let host = RemoteRef::parse(&url).map(|r| r.host);
                let kind = match host.as_deref() {
                    Some("github.com") => "github",
                    Some(host) if studio_hosts.iter().any(|h| h == host) => "studio",
                    // A remote that is this repository in the studio by path.
                    _ if studio_remote_name.as_deref() == Some(remote) => "studio",
                    _ => "other",
                };
                remote_kinds.insert(remote.to_string(), kind);
            }
        }
        let default_branch = studio
            .as_ref()
            .map(|r| r.default_branch.as_str())
            .filter(|b| !b.is_empty());
        Ok(HistoryView {
            branch: history.branch,
            upstream: history.upstream,
            ahead: history.ahead,
            behind: history.behind,
            more: history.more,
            rows: history
                .commits
                .into_iter()
                .map(|c| HistoryRow {
                    pills: pills(&c.refs, default_branch, &remote_kinds),
                    sha: c.sha,
                    short: c.short,
                    subject: c.subject,
                    author: c.author,
                    date: c.date,
                    lane: c.lane,
                    line0: c.line0,
                    line1: c.line1,
                    unpushed: c.unpushed,
                    incoming: c.incoming,
                })
                .collect(),
        })
    }

    pub async fn repo_commit_detail(&self, path: &str, sha: &str) -> CmdResult<CommitView> {
        let repo = self.repo_path(path).await?;
        let detail = self.git().commit_detail(&repo, sha).await?;
        Ok(CommitView {
            can_undo: detail.is_head && !detail.pushed,
            detail,
        })
    }

    /// Opens the working copy in the chosen editor (or the configured one).
    pub async fn repo_open(&self, path: &str, preset: Option<&str>) -> CmdResult<String> {
        let repo = self.repo_path(path).await?;
        let settings = self.settings();
        let id = match preset {
            Some(id) => id,
            None => tools::editor_or_default(settings.config.editor.as_deref()).id,
        };
        self.launch(id, &repo)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use atelier_core::git::RefLabel;

    #[test]
    fn languages_and_labels() {
        assert_eq!(language("cmd/atelier-desktop/tray.go"), "Go");
        assert_eq!(language("docs/desktop.md"), "Markdown");
        assert_eq!(language("Dockerfile"), "Dockerfile");
        assert_eq!(language("a/b/noext"), "");
        assert_eq!(
            lang_label("a.go", FileStatus::Added, false),
            "Go · new file"
        );
        assert_eq!(lang_label("a.go", FileStatus::Modified, false), "Go");
        assert_eq!(lang_label("logo.png", FileStatus::Modified, true), "Binary");
        assert_eq!(lang_label("x", FileStatus::Deleted, false), "deleted");
    }

    #[test]
    fn pills_follow_the_three_threads() {
        let label = |kind, name: &str, remote: Option<&str>| RefLabel {
            kind,
            name: name.to_string(),
            remote: remote.map(str::to_string),
        };
        let kinds: HashMap<String, &'static str> = [
            ("origin".to_string(), "studio"),
            ("gh".to_string(), "github"),
            ("fork".to_string(), "other"),
        ]
        .into_iter()
        .collect();
        let got = pills(
            &[
                label(RefKind::Branch, "main", None),
                label(RefKind::Remote, "main", Some("origin")),
                label(RefKind::Remote, "main", Some("gh")),
                label(RefKind::Tag, "v0.9", None),
                label(RefKind::Remote, "wip", Some("fork")),
            ],
            Some("main"),
            &kinds,
        );
        let flat: Vec<(&str, &str)> = got.iter().map(|p| (p.text.as_str(), p.tone)).collect();
        assert_eq!(
            flat,
            [
                ("main", "yours"),
                ("studio", "yours"),
                ("GitHub", "github"),
                ("v0.9", "quiet"),
                ("fork/wip", "quiet")
            ]
        );
        let got = pills(
            &[
                label(RefKind::Head, "feature/x", None),
                label(RefKind::Remote, "other", Some("origin")),
            ],
            Some("main"),
            &kinds,
        );
        assert_eq!(
            got[0],
            Pill {
                text: "feature/x".into(),
                tone: "quiet"
            }
        );
        assert_eq!(got[1].text, "studio/other");
    }
}
