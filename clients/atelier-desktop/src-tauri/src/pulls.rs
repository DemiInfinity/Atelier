//! Pull requests across the studio: the list, one in detail, merging now or
//! when the checks pass, and checking the branch out here.

use std::time::Duration;

use atelier_core::api::{
    Check, CheckStatus, MergeMethod, MergeOutcome, MergeRequest, Pull, PullComment, PullState,
    PullSummary, Via,
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

#[derive(Clone, Debug, Serialize)]
pub struct PullRow {
    pub repo: String,
    pub repo_short: String,
    pub number: u64,
    pub title: String,
    pub by: String,
    pub status: Badge,
    pub via: Option<Badge>,
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

pub fn pull_view(pull: &Pull, login: Option<&str>, local_path: Option<String>) -> PullView {
    let (state, headline, tone) = match pull.state {
        PullState::Merged => ("merged", "Merged", "quiet"),
        PullState::Closed => ("closed", "Closed without merging", "quiet"),
        _ if pull.merge.auto_merge.is_some() => ("open", "Will merge when checks pass", "yours"),
        _ if pull.merge.can_merge => ("open", "Ready to merge", "pass"),
        _ => ("open", "Not ready to merge yet", "yours"),
    };
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
        reason: pull.merge.reason.clone(),
        tone,
        can_merge: state == "open" && pull.merge.can_merge,
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
        local_path,
    }
}

impl Desk {
    fn login(&self) -> Option<String> {
        self.me_cached().map(|me| me.person.login)
    }

    pub async fn pulls_list(&self) -> CmdResult<Vec<PullRow>> {
        let pulls = self.watch(self.client()?.pulls().await)?;
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
            })
            .collect())
    }

    /// The working copy of `full_name` on this machine, if there is one.
    pub async fn local_copy_of(&self, full_name: &str) -> Option<String> {
        let repos = match self.repos(Duration::from_secs(300)).await {
            Ok(repos) => repos,
            Err(_) => self.repos_cached()?,
        };
        self.working_copies(Some(&repos))
            .await
            .into_iter()
            .find(|c| c.full_name.as_deref() == Some(full_name))
            .map(|c| c.path)
    }

    pub async fn pull_detail(&self, repo: &str, number: u64) -> CmdResult<PullView> {
        let (owner, name) = paths::split_full_name(repo)?;
        let pull = self.watch(self.client()?.pull(owner, name, number).await)?;
        let _ = self.me(Duration::from_secs(300)).await;
        let local = self.local_copy_of(&pull.repo).await;
        Ok(pull_view(&pull, self.login().as_deref(), local))
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
    }
}
