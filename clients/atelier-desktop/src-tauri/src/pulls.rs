//! Pull requests across the studio: the list, one in detail, merging now or
//! when the checks pass, bringing the branch up to date with its base, and
//! checking the branch out here.

use std::time::Duration;

use atelier_core::api::{
    Check, CheckCounts, CheckStatus, ChecksState, MergeMethod, MergeOutcome, MergeRequest, Pull,
    PullComment, PullState, PullSummary, UpdateOutcome, Via,
};
use serde::Serialize;

use crate::desk::Desk;
use crate::error::{CmdError, CmdResult};
use crate::notify::short_repo;
use crate::paths;
use crate::today::NeedAction;

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct Badge {
    pub text: String,
    /// `pass`, `fail`, `yours`, `agents`, `github` or `quiet`.
    pub tone: &'static str,
}

fn badge(text: &str, tone: &'static str) -> Badge {
    Badge {
        text: text.to_string(),
        tone,
    }
}

/// A pull request's checks in a few words, for a row in a list.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct ChecksBrief {
    /// `none`, `pending`, `failing` or `passing`.
    pub state: &'static str,
    /// `3 of 4 passed`, `1 failing`, `running`, `no checks`.
    pub text: String,
    /// `pass`, `fail`, `yours` (running), `agents` (waiting for you) or `quiet`.
    pub tone: &'static str,
    /// The same in a full sentence, for a tooltip.
    pub title: String,
}

/// The checks of a pull request at a glance.
pub fn checks_brief(c: &CheckCounts) -> ChecksBrief {
    let total = c.count();
    let (state, text, tone) = match c.summary() {
        ChecksState::None => ("none", "no checks".to_string(), "quiet"),
        ChecksState::Failing => ("failing", format!("{} failing", c.failed.max(1)), "fail"),
        ChecksState::Pending if c.passed > 0 => (
            "pending",
            format!("{} of {total} passed", c.passed),
            "yours",
        ),
        ChecksState::Pending if c.pending == 0 && c.waiting > 0 => {
            ("pending", "waiting for you".to_string(), "agents")
        }
        ChecksState::Pending => ("pending", "running".to_string(), "yours"),
        ChecksState::Passing | ChecksState::Unknown => (
            "passing",
            match total {
                0 | 1 => "passed".to_string(),
                n => format!("all {n} passed"),
            },
            "pass",
        ),
    };
    let mut parts = Vec::new();
    for (n, word) in [
        (c.passed, "passed"),
        (c.failed, "failed"),
        (c.pending, "running"),
        (c.waiting, "waiting for you"),
    ] {
        if n > 0 {
            parts.push(format!("{n} {word}"));
        }
    }
    let title = if parts.is_empty() {
        "This pull request has no checks.".to_string()
    } else {
        format!(
            "{} · {}",
            if total == 1 {
                "1 check".to_string()
            } else {
                format!("{total} checks")
            },
            parts.join(", ")
        )
    };
    ChecksBrief {
        state,
        text,
        tone,
        title,
    }
}

#[derive(Clone, Debug, Serialize)]
pub struct PullRow {
    pub repo: String,
    pub repo_short: String,
    pub number: u64,
    pub title: String,
    pub by: String,
    pub status: Badge,
    pub via: Option<Badge>,
    pub checks: ChecksBrief,
    /// The branch it comes from.
    pub head: String,
    /// The branch it goes into.
    pub base: String,
    /// Commits of the base it doesn't have yet (0 when the studio doesn't say).
    pub behind: u64,
    /// It can't be merged with its base as it is.
    pub conflicts: bool,
}

#[derive(Clone, Debug, Serialize)]
pub struct CheckRow {
    pub status: Badge,
    pub name: String,
    pub detail: String,
    /// The label of the row's button (`Approve`), when it has one.
    pub action_label: Option<String>,
    pub action: Option<NeedAction>,
}

#[derive(Clone, Debug, Serialize)]
pub struct CommentRow {
    pub who: String,
    /// `bot · via Tether`.
    pub tag: String,
    /// `yours`, `github`, `agents` or `connect`.
    pub tone: &'static str,
    pub at: String,
    pub body: String,
    /// The body as Markdown, parsed (see [`crate::markdown`]).
    pub body_md: Vec<crate::markdown::Block>,
}

#[derive(Clone, Debug, Serialize)]
pub struct MethodChoice {
    pub id: &'static str,
    /// `Squash and merge`.
    pub label: &'static str,
}

#[derive(Clone, Debug, Serialize)]
pub struct PullView {
    pub repo: String,
    pub repo_short: String,
    pub number: u64,
    pub title: String,
    /// `open`, `closed` or `merged`.
    pub state: &'static str,
    pub head: String,
    pub base: String,
    pub by: String,
    pub commits: u64,
    pub files: u64,
    /// `Ready to merge`, `Not ready to merge yet`, …
    pub headline: String,
    pub reason: String,
    /// `pass` when it can merge, `yours` while waiting, `quiet` when done.
    pub tone: &'static str,
    pub can_merge: bool,
    pub on_github: bool,
    pub methods: Vec<MethodChoice>,
    /// `Squash · asked by device:MacBook`, while it waits for its checks.
    pub auto_merge: Option<String>,
    pub checks: Vec<CheckRow>,
    pub comments: Vec<CommentRow>,
    pub web_path: String,
    /// The working copy of this repository on this machine, if there is one.
    pub local_path: Option<String>,
    /// The branch that working copy is on.
    pub local_branch: Option<String>,
    /// Commits of the base the branch doesn't have yet.
    pub behind: u64,
    /// The branch conflicts with its base: merging and updating can't work
    /// until someone resolves it.
    pub conflicts: bool,
    pub conflict_files: Vec<String>,
    /// "Update from <base>" is worth offering: the studio says it would
    /// merge something (or, an older one: open, behind, no conflicts).
    pub can_update: bool,
    /// Behind, but the repository's home is GitHub, so the branch is updated
    /// there: the GitHub repository (`owner/name`) to open instead.
    pub update_on_github: Option<String>,
}

/// What "Update from <base>" did.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct BranchUpdate {
    /// `updated`, `up_to_date` or `conflicts`.
    pub outcome: &'static str,
    pub head: String,
    pub base: String,
    /// The branch's tip in the studio afterwards.
    pub head_sha: String,
    /// The studio's sentence about the conflicts.
    pub message: String,
    /// The files that conflict.
    pub files: Vec<String>,
    pub local_path: Option<String>,
    /// The working copy here is on the pull request's branch, so after an
    /// update it is behind the studio. Nothing is pulled without asking.
    pub local_on_branch: bool,
}

#[derive(Clone, Debug, Serialize)]
pub struct MergeResult {
    pub merged: bool,
    pub sha: String,
    /// It will merge by itself when the checks pass.
    pub scheduled: bool,
    pub toast: Option<crate::notify::Toast>,
}

#[derive(Clone, Debug, Serialize)]
pub struct CheckedOut {
    pub path: String,
    pub branch: String,
}

/// `owner:DemiInfinity` → `you` for the person themselves.
pub fn author_label(author: &str, login: Option<&str>) -> String {
    // An issue opened from a device is by `token:device:MacBook`.
    let author = author.strip_prefix("token:").unwrap_or(author);
    let (kind, name) = author.split_once(':').unwrap_or(("", author));
    match (kind, login) {
        ("owner" | "person" | "", Some(login)) if name.eq_ignore_ascii_case(login) => {
            "you".to_string()
        }
        ("device", _) => format!("you, from {name}"),
        _ => name.to_string(),
    }
}

pub fn summary_badge(pull: &PullSummary) -> Badge {
    match pull.state {
        PullState::Merged => return badge("Merged", "quiet"),
        PullState::Closed => return badge("Closed", "quiet"),
        _ => {}
    }
    let c = pull.checks;
    if pull.merge.auto_merge.is_some() {
        badge("Merges when checks pass", "yours")
    } else if c.failed > 0 {
        badge("Checks failed", "fail")
    } else if c.waiting > 0 {
        badge("Waiting for you", "agents")
    } else if c.pending > 0 {
        badge("Checks running", "yours")
    } else if pull.merge.can_merge {
        badge("Ready to merge", "pass")
    } else {
        badge("Not ready", "quiet")
    }
}

pub fn via_badge(via: Option<Via>) -> Option<Badge> {
    match via? {
        Via::Tether => Some(badge("via Tether", "github")),
        Via::Connect => Some(badge("via Connect", "agents")),
        Via::Bot => Some(badge("bot", "quiet")),
        Via::Unknown => None,
    }
}

pub fn check_row(check: &Check) -> CheckRow {
    let status = match check.status {
        CheckStatus::Success => badge("Passed", "pass"),
        CheckStatus::Failure => badge("Failed", "fail"),
        CheckStatus::Pending => badge("Running", "yours"),
        CheckStatus::Waiting => badge("Waiting", "agents"),
        CheckStatus::Skipped => badge("Skipped", "quiet"),
        CheckStatus::Unknown => badge("Unknown", "quiet"),
    };
    let action = match (check.action.as_deref(), check.job_id) {
        (Some("approve_agent"), Some(job_id)) => Some(NeedAction::ApproveAgent { job_id }),
        _ => None,
    };
    CheckRow {
        status,
        name: check.name.clone(),
        detail: check.detail.clone(),
        action_label: action.as_ref().map(|_| "Approve".to_string()),
        action,
    }
}

pub fn comment_row(comment: &PullComment, login: Option<&str>) -> CommentRow {
    let mut tags: Vec<&str> = Vec::new();
    match comment.kind.as_str() {
        "bot" => tags.push("bot"),
        "agent" => tags.push("agent"),
        _ => {}
    }
    let tone = match comment.via {
        Some(Via::Tether) => {
            tags.push("via Tether");
            "github"
        }
        Some(Via::Connect) => {
            tags.push("via Connect");
            "connect"
        }
        _ if comment.kind == "agent" => "agents",
        _ => "yours",
    };
    CommentRow {
        who: author_label(&comment.author, login),
        tag: tags.join(" · "),
        tone,
        at: comment.at.clone(),
        body: comment.body.clone(),
        body_md: crate::markdown::render(&comment.body),
    }
}

fn method_choice(method: MergeMethod) -> Option<MethodChoice> {
    match method {
        MergeMethod::Squash => Some(MethodChoice {
            id: "squash",
            label: "Squash and merge",
        }),
        MergeMethod::Merge => Some(MethodChoice {
            id: "merge",
            label: "Merge",
        }),
        MergeMethod::Rebase => Some(MethodChoice {
            id: "rebase",
            label: "Rebase and merge",
        }),
        MergeMethod::Unknown => None,
    }
}

fn parse_method(method: &str) -> CmdResult<MergeMethod> {
    match method {
        "squash" => Ok(MergeMethod::Squash),
        "merge" => Ok(MergeMethod::Merge),
        "rebase" => Ok(MergeMethod::Rebase),
        other => Err(CmdError::invalid(format!(
            "“{other}” isn’t a way to merge."
        ))),
    }
}

/// Anything not merged or closed: an unknown state reads as open.
fn is_open(state: PullState) -> bool {
    !matches!(state, PullState::Merged | PullState::Closed)
}

/// The local working copy of a pull request's repository: its path and the
/// branch it is on.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct LocalCopy {
    pub path: String,
    pub branch: String,
}

pub fn pull_view(
    pull: &Pull,
    login: Option<&str>,
    local: Option<LocalCopy>,
    github: Option<String>,
) -> PullView {
    let open = is_open(pull.state);
    let conflicts = open && pull.has_conflicts();
    let conflict_headline = format!("Conflicts with {}", pull.base);
    let (state, headline, tone) = match pull.state {
        PullState::Merged => ("merged", "Merged", "quiet"),
        PullState::Closed => ("closed", "Closed without merging", "quiet"),
        _ if conflicts => ("open", conflict_headline.as_str(), "fail"),
        _ if pull.merge.auto_merge.is_some() => ("open", "Will merge when checks pass", "yours"),
        _ if pull.merge.can_merge => ("open", "Ready to merge", "pass"),
        _ => ("open", "Not ready to merge yet", "yours"),
    };
    // With conflicts the studio's own reason stands when it refuses the
    // merge itself; otherwise the app says why the button is off.
    let reason = if conflicts && (pull.merge.can_merge || pull.merge.reason.trim().is_empty()) {
        format!(
            "{} and {} changed the same lines. Resolve the conflicts before merging.",
            pull.head, pull.base
        )
    } else {
        pull.merge.reason.clone()
    };
    let behind = if open { pull.behind() } else { 0 };
    PullView {
        repo: pull.repo.clone(),
        repo_short: short_repo(&pull.repo).to_string(),
        number: pull.number,
        title: pull.title.clone(),
        state,
        head: pull.head.clone(),
        base: pull.base.clone(),
        by: author_label(&pull.author, login),
        commits: pull.commits,
        files: pull.files,
        headline: headline.to_string(),
        reason,
        tone,
        can_merge: open && pull.merge.can_merge && !conflicts,
        on_github: pull.merge.on_github,
        methods: pull
            .merge
            .methods
            .iter()
            .filter_map(|m| method_choice(*m))
            .collect(),
        auto_merge: pull.merge.auto_merge.as_ref().map(|a| {
            let method = match a.method {
                MergeMethod::Squash => "Squash",
                MergeMethod::Merge => "Merge",
                MergeMethod::Rebase => "Rebase",
                MergeMethod::Unknown => "Merge",
            };
            if a.by.is_empty() {
                method.to_string()
            } else {
                format!("{method} · asked by {}", a.by.trim_start_matches("device:"))
            }
        }),
        checks: pull.checks.iter().map(check_row).collect(),
        comments: pull
            .comments
            .iter()
            .map(|c| comment_row(c, login))
            .collect(),
        web_path: pull.web_path.clone(),
        local_path: local.as_ref().map(|l| l.path.clone()),
        local_branch: local.map(|l| l.branch),
        behind,
        conflicts,
        conflict_files: if conflicts {
            pull.conflict_files.clone()
        } else {
            Vec::new()
        },
        can_update: open && !conflicts && !pull.merge.on_github && pull.can_update(),
        update_on_github: github
            .filter(|_| open && !conflicts && behind > 0 && pull.merge.on_github),
    }
}

impl Desk {
    fn login(&self) -> Option<String> {
        self.me_cached().map(|me| me.person.login)
    }

    /// The open pull requests: across the studio, or of one repository
    /// (`owner/name`).
    pub async fn pulls_list(&self, repo: Option<&str>) -> CmdResult<Vec<PullRow>> {
        let client = self.client()?;
        let pulls = match repo {
            Some(full_name) => {
                paths::split_full_name(full_name)?;
                self.watch(client.pulls_in(full_name).await)?
            }
            None => self.watch(client.pulls().await)?,
        };
        let _ = self.me(Duration::from_secs(300)).await;
        let login = self.login();
        Ok(pulls
            .iter()
            .map(|p| PullRow {
                repo: p.repo.clone(),
                repo_short: short_repo(&p.repo).to_string(),
                number: p.number,
                title: p.title.clone(),
                by: author_label(&p.author, login.as_deref()),
                status: summary_badge(p),
                via: via_badge(p.via),
                checks: checks_brief(&p.checks),
                head: p.head.clone(),
                base: p.base.clone(),
                behind: if is_open(p.state) { p.behind() } else { 0 },
                conflicts: is_open(p.state) && p.has_conflicts(),
            })
            .collect())
    }

    /// The working copy of `full_name` on this machine, if there is one.
    pub async fn local_copy_of(&self, full_name: &str) -> Option<String> {
        self.local_copy(full_name).await.map(|c| c.path)
    }

    /// That working copy, with the branch it is on right now.
    pub async fn local_copy(&self, full_name: &str) -> Option<LocalCopy> {
        let repos = match self.repos(Duration::from_secs(300)).await {
            Ok(repos) => repos,
            Err(_) => self.repos_cached()?,
        };
        self.working_copies(Some(&repos))
            .await
            .into_iter()
            .find(|c| c.full_name.as_deref() == Some(full_name))
            .map(|c| LocalCopy {
                path: c.path,
                branch: c.branch,
            })
    }

    pub async fn pull_detail(&self, repo: &str, number: u64) -> CmdResult<PullView> {
        let (owner, name) = paths::split_full_name(repo)?;
        let pull = self.watch(self.client()?.pull(owner, name, number).await)?;
        let _ = self.me(Duration::from_secs(300)).await;
        let local = self.local_copy(&pull.repo).await;
        // Where a GitHub-homed repository lives there, for "Open on GitHub".
        let github = self.repos_cached().and_then(|repos| {
            repos
                .iter()
                .find(|r| r.full_name.eq_ignore_ascii_case(&pull.repo))
                .and_then(|r| r.github.as_ref())
                .map(|g| g.repo.clone())
                .filter(|name| !name.is_empty())
        });
        Ok(pull_view(&pull, self.login().as_deref(), local, github))
    }

    /// "Update from <base>": the studio merges the base branch into the pull
    /// request's branch. Conflicts are an answer (with the files), not an
    /// error. The working copy here is never touched: when it is on that
    /// branch the answer says so, and the window offers to pull.
    pub async fn pull_update(&self, repo: &str, number: u64) -> CmdResult<BranchUpdate> {
        let (owner, name) = paths::split_full_name(repo)?;
        let client = self.client()?;
        let pull = self.watch(client.pull(owner, name, number).await)?;
        let outcome = self.watch(client.update_pull(owner, name, number).await)?;
        let local = self.local_copy(&pull.repo).await;
        let local_on_branch = local
            .as_ref()
            .is_some_and(|l| !pull.head.is_empty() && l.branch == pull.head);
        let local_path = local.map(|l| l.path);
        let mut update = BranchUpdate {
            outcome: "up_to_date",
            head: pull.head,
            base: pull.base,
            head_sha: String::new(),
            message: String::new(),
            files: Vec::new(),
            local_path,
            local_on_branch,
        };
        match outcome {
            UpdateOutcome::Updated { head_sha } => {
                update.outcome = "updated";
                update.head_sha = head_sha;
            }
            UpdateOutcome::UpToDate { head_sha } => update.head_sha = head_sha,
            UpdateOutcome::Conflicts { message, files } => {
                update.outcome = "conflicts";
                update.message = message;
                update.files = files;
            }
        }
        Ok(update)
    }

    /// Merges now, or asks the studio to merge once the checks pass.
    pub async fn pull_merge(
        &self,
        repo: &str,
        number: u64,
        method: &str,
        when_green: bool,
    ) -> CmdResult<MergeResult> {
        let (owner, name) = paths::split_full_name(repo)?;
        let request = MergeRequest {
            method: parse_method(method)?,
            when_green,
            delete_branch: None,
        };
        let client = self.client()?;
        let outcome = self.watch(client.merge_pull(owner, name, number, request).await)?;
        self.forget_repos();
        Ok(match outcome {
            MergeOutcome::Merged { merged, sha } => {
                // The device's own actions aren't echoed as events, so the
                // friendly word for a merge is said here.
                let messages = self.messages(Duration::from_secs(600)).await.ok();
                let base = client
                    .pull(owner, name, number)
                    .await
                    .map(|p| p.base)
                    .unwrap_or_default();
                let vars = atelier_core::messages::Vars::new()
                    .set("repo", name)
                    .set("base", base)
                    .set("device", format!("your {}", self.device_name()));
                let toast = merged
                    .then(|| {
                        crate::notify::local(
                            "merge",
                            messages.as_ref(),
                            &vars,
                            &format!("{name} · pull request {number}"),
                            crate::notify::Target::Pull {
                                repo: repo.to_string(),
                                number,
                            },
                            self.seed(),
                        )
                    })
                    .flatten();
                MergeResult {
                    merged,
                    sha,
                    scheduled: false,
                    toast,
                }
            }
            MergeOutcome::Scheduled { .. } => MergeResult {
                merged: false,
                sha: String::new(),
                scheduled: true,
                toast: None,
            },
        })
    }

    pub async fn pull_cancel_merge(&self, repo: &str, number: u64) -> CmdResult<()> {
        let (owner, name) = paths::split_full_name(repo)?;
        self.watch(self.client()?.cancel_auto_merge(owner, name, number).await)
    }

    /// Fetches and switches the local working copy to the pull request's
    /// branch. Needs a working copy of the repository on this machine.
    pub async fn pull_checkout(&self, repo: &str, number: u64) -> CmdResult<CheckedOut> {
        let (owner, name) = paths::split_full_name(repo)?;
        let pull = self.watch(self.client()?.pull(owner, name, number).await)?;
        let path = self.local_copy_of(&pull.repo).await.ok_or_else(|| {
            CmdError::new(
                "no_copy",
                format!("{} isn’t on this machine yet.", short_repo(&pull.repo)),
            )
            .hint("Clone it from “Add a repository” first.")
        })?;
        let dir = self.repo_path(&path).await?;
        let git = self.git();
        git.fetch(&dir, None).await?;
        git.switch_branch(&dir, &pull.head).await?;
        Ok(CheckedOut {
            path,
            branch: pull.head,
        })
    }

    pub async fn agent_approve(&self, job_id: u64) -> CmdResult<bool> {
        Ok(self
            .watch(self.client()?.approve_agent(job_id).await)?
            .approved)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use atelier_core::api::{AutoMerge, CheckCounts, MergeInfo};

    fn summary(checks: CheckCounts, can_merge: bool) -> PullSummary {
        PullSummary {
            checks,
            merge: MergeInfo {
                can_merge,
                ..MergeInfo::default()
            },
            ..PullSummary::default()
        }
    }

    #[test]
    fn list_badges() {
        let c = |passed, failed, pending, waiting| CheckCounts {
            passed,
            failed,
            pending,
            waiting,
            ..CheckCounts::default()
        };
        assert_eq!(
            summary_badge(&summary(c(2, 0, 0, 0), true)),
            badge("Ready to merge", "pass")
        );
        assert_eq!(
            summary_badge(&summary(c(2, 0, 1, 1), false)),
            badge("Waiting for you", "agents")
        );
        assert_eq!(
            summary_badge(&summary(c(2, 0, 1, 0), false)),
            badge("Checks running", "yours")
        );
        assert_eq!(
            summary_badge(&summary(c(1, 1, 1, 1), false)),
            badge("Checks failed", "fail")
        );
        assert_eq!(
            summary_badge(&summary(c(0, 0, 0, 0), false)),
            badge("Not ready", "quiet")
        );
        let mut waiting = summary(c(1, 0, 1, 0), false);
        waiting.merge.auto_merge = Some(AutoMerge::default());
        assert_eq!(summary_badge(&waiting).text, "Merges when checks pass");
        let mut merged = summary(c(1, 0, 0, 0), false);
        merged.state = PullState::Merged;
        assert_eq!(summary_badge(&merged), badge("Merged", "quiet"));
        assert_eq!(
            via_badge(Some(Via::Tether)),
            Some(badge("via Tether", "github"))
        );
        assert_eq!(via_badge(None), None);
    }

    #[test]
    fn checks_at_a_glance() {
        let c = |passed, failed, pending, waiting| CheckCounts {
            passed,
            failed,
            pending,
            waiting,
            ..CheckCounts::default()
        };
        let brief = |counts: CheckCounts| {
            let b = checks_brief(&counts);
            (b.state, b.text, b.tone)
        };
        assert_eq!(brief(c(0, 0, 0, 0)), ("none", "no checks".into(), "quiet"));
        assert_eq!(
            brief(c(3, 0, 1, 0)),
            ("pending", "3 of 4 passed".into(), "yours")
        );
        assert_eq!(brief(c(0, 0, 2, 0)), ("pending", "running".into(), "yours"));
        assert_eq!(
            brief(c(0, 0, 0, 1)),
            ("pending", "waiting for you".into(), "agents")
        );
        assert_eq!(
            brief(c(2, 1, 1, 0)),
            ("failing", "1 failing".into(), "fail")
        );
        assert_eq!(
            brief(c(4, 0, 0, 0)),
            ("passing", "all 4 passed".into(), "pass")
        );
        assert_eq!(brief(c(1, 0, 0, 0)), ("passing", "passed".into(), "pass"));
        // What a newer studio sends wins over the counts.
        let sent = CheckCounts {
            total: Some(5),
            state: Some(ChecksState::Pending),
            passed: 3,
            ..CheckCounts::default()
        };
        assert_eq!(brief(sent), ("pending", "3 of 5 passed".into(), "yours"));
        assert_eq!(
            checks_brief(&c(2, 1, 1, 0)).title,
            "4 checks · 2 passed, 1 failed, 1 running"
        );
        assert_eq!(
            checks_brief(&c(0, 0, 0, 0)).title,
            "This pull request has no checks."
        );
    }

    fn detail(behind: Option<u64>, conflicts: Option<bool>, files: &[&str]) -> Pull {
        Pull {
            repo: "DemiInfinity/Atelier".into(),
            number: 14,
            head: "feature/tray".into(),
            base: "master".into(),
            behind,
            conflicts,
            conflict_files: files.iter().map(|f| f.to_string()).collect(),
            merge: MergeInfo {
                can_merge: true,
                ..MergeInfo::default()
            },
            ..Pull::default()
        }
    }

    #[test]
    fn behind_its_base_offers_an_update() {
        let view = pull_view(&detail(Some(3), None, &[]), None, None, None);
        assert_eq!(
            (view.behind, view.can_update, view.conflicts),
            (3, true, false)
        );
        assert!(view.can_merge);
        assert_eq!(view.headline, "Ready to merge");
        // Up to date, or a studio that doesn't say: nothing to offer.
        assert!(!pull_view(&detail(Some(0), None, &[]), None, None, None).can_update);
        assert!(!pull_view(&detail(None, None, &[]), None, None, None).can_update);
        // Waiting to merge by itself doesn't take the offer away.
        let mut waiting = detail(Some(2), None, &[]);
        waiting.merge.can_merge = false;
        waiting.merge.auto_merge = Some(AutoMerge::default());
        let view = pull_view(&waiting, None, None, None);
        assert!(view.can_update && view.auto_merge.is_some());
        // A merged one is never behind.
        let mut merged = detail(Some(2), Some(true), &["a.go"]);
        merged.state = PullState::Merged;
        let view = pull_view(&merged, None, None, None);
        assert_eq!(
            (view.behind, view.can_update, view.conflicts),
            (0, false, false)
        );
        assert!(view.conflict_files.is_empty());
    }

    #[test]
    fn the_studios_word_on_updating_wins_and_github_homes_update_there() {
        // Behind, but the head branch is protected: the studio says no.
        let mut protected = detail(Some(3), Some(false), &[]);
        protected.can_update = Some(false);
        let view = pull_view(&protected, None, None, None);
        assert_eq!((view.behind, view.can_update), (3, false));
        assert_eq!(view.update_on_github, None);
        // At home on GitHub: no button, the GitHub repository instead.
        let mut github = detail(Some(2), None, &[]);
        github.merge.on_github = true;
        let view = pull_view(&github, None, None, Some("maralind/loopkit".into()));
        assert!(!view.can_update);
        assert_eq!(view.update_on_github.as_deref(), Some("maralind/loopkit"));
        // Not behind: nothing to open either.
        github.behind = Some(0);
        let view = pull_view(&github, None, None, Some("maralind/loopkit".into()));
        assert_eq!(view.update_on_github, None);
    }

    #[test]
    fn conflicts_turn_merging_and_updating_off() {
        let view = pull_view(
            &detail(Some(3), Some(true), &["cmd/app/main.go", "README.md"]),
            None,
            Some(LocalCopy {
                path: "/home/Atelier".into(),
                branch: "feature/tray".into(),
            }),
            None,
        );
        assert!(view.conflicts && !view.can_merge && !view.can_update);
        assert_eq!(view.headline, "Conflicts with master");
        assert_eq!(view.tone, "fail");
        assert_eq!(
            view.reason,
            "feature/tray and master changed the same lines. Resolve the conflicts before merging."
        );
        assert_eq!(view.conflict_files, ["cmd/app/main.go", "README.md"]);
        assert_eq!(view.local_path.as_deref(), Some("/home/Atelier"));
        assert_eq!(view.local_branch.as_deref(), Some("feature/tray"));
        // The files alone say it, and the studio's own reason is kept.
        let mut named = detail(None, None, &["a.go"]);
        named.merge.can_merge = false;
        named.merge.reason = "This branch has conflicts with master.".into();
        let view = pull_view(&named, None, None, None);
        assert!(view.conflicts);
        assert_eq!(view.reason, "This branch has conflicts with master.");
    }

    #[test]
    fn authors() {
        assert_eq!(
            author_label("owner:DemiInfinity", Some("demiinfinity")),
            "you"
        );
        assert_eq!(author_label("owner:Mara", Some("DemiInfinity")), "Mara");
        assert_eq!(
            author_label("atelier-updates", Some("DemiInfinity")),
            "atelier-updates"
        );
        assert_eq!(author_label("device:MacBook", None), "you, from MacBook");
        assert_eq!(
            author_label("token:device:MacBook", Some("DemiInfinity")),
            "you, from MacBook"
        );
        assert_eq!(
            author_label("person:maralind", Some("DemiInfinity")),
            "maralind"
        );
    }
}
