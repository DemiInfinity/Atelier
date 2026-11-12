//! "Today": what needs you, the working copies on this machine, what is
//! running here, and the studio's feed. Also feeds the sidebar and the menu
//! bar popover.

use std::time::Duration;

use atelier_core::api::{FeedItem, Need, NeedKind, Repo, RunningJob, Status, Thread};
use atelier_core::git::LocalRepo;
use atelier_core::runner::{PausedReason, RunnerStatus};
use serde::Serialize;

use crate::desk::{Desk, SCAN_DEPTH};
use crate::error::CmdResult;
use crate::notify::{pull_from_web_path, short_repo, Target};

/// What a button on a "needs you" row does.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum NeedAction {
    /// Show the agent on "This device" so it can be approved there.
    ApproveAgent {
        job_id: u64,
    },
    /// Show the log of a failed run.
    OpenLog {
        run_id: u64,
        repo: String,
        web_path: String,
    },
    OpenPull {
        repo: String,
        number: u64,
    },
    /// Push a working copy's commits.
    Push {
        path: String,
    },
    OpenWeb {
        web_path: String,
    },
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct NeedRow {
    pub id: String,
    /// `agents` (violet), `fail` (red), `github` (blue) or `yours` (amber).
    pub tone: &'static str,
    pub title: String,
    pub detail: String,
    pub label: String,
    pub action: NeedAction,
}

/// A working copy on this machine.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct WorkingCopy {
    pub path: String,
    pub name: String,
    /// The account it belongs to (`deminimmo`, `Elyndra Studios`), or where
    /// its remote points when the studio doesn't know it.
    pub owner: String,
    /// The owning account's name in the studio (`elyndrastudios`); empty
    /// when the studio doesn't know the folder. The sidebar groups by it.
    pub owner_name: String,
    /// `user` (the personal account) or `company`; empty when unknown.
    pub owner_kind: String,
    /// Pinned to the top of the sidebar.
    pub pinned: bool,
    /// `owner/name` in the studio, when it is there.
    pub full_name: Option<String>,
    pub branch: String,
    pub changes: u64,
    pub ahead: u64,
    pub behind: u64,
    /// `3 changes · 1 to push`, `2 behind`, `Clean`.
    pub state: String,
    /// `yours` (amber), `github` (blue: something to pull) or `quiet`.
    pub tone: &'static str,
    /// The one-click action: `open`, `push` or `pull`.
    pub action: &'static str,
    /// The short form for the sidebar: `3 · ↑1`, `↓2`.
    pub short: String,
    /// For a linked worktree: the main folder of its repository. The
    /// sidebar shows it under that repository, not as one of its own.
    pub worktree_of: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct RunningRow {
    pub job_id: u64,
    pub title: String,
    /// `Claude Code` for an agent on that engine.
    pub engine: Option<String>,
    pub detail: String,
    /// 0–1 for an agent that reports its turn.
    pub progress: Option<f32>,
    pub kind: String,
    pub started_at: String,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct FeedRow {
    /// `yours`, `github`, `connect` or `agents`.
    pub tone: &'static str,
    /// `You`, `via Tether`, `via Connect`, `Agents`.
    pub tag: &'static str,
    pub text: String,
    pub at: String,
    pub target: Option<Target>,
}

/// The runner on this machine in one line.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct RunnerBrief {
    /// It answers on its control socket.
    pub running: bool,
    pub taking_jobs: bool,
    pub jobs: usize,
    /// `Runner on · 1 job running`.
    pub line: String,
    /// `On · pauses on battery`.
    pub switch_line: String,
}

#[derive(Clone, Debug, Serialize)]
pub struct Overview {
    pub needs: Vec<NeedRow>,
    pub copies: Vec<WorkingCopy>,
    /// Git folders found here that aren't in the studio: their remote is
    /// elsewhere, or they have none. `owner` says where (`github.com`).
    pub others: Vec<WorkingCopy>,
    pub running: Vec<RunningRow>,
    /// Jobs of yours that other runners are working on.
    pub elsewhere: Vec<String>,
    pub feed: Vec<FeedRow>,
    pub unread: u64,
    pub pulls_open: u64,
    /// `in ~/Atelier and ~/Projects`.
    pub roots_label: String,
    pub runner: RunnerBrief,
    pub device_name: String,
    /// Why the studio's part is missing, when it couldn't be reached.
    pub studio_error: Option<String>,
}

fn plural(n: u64, one: &str, many: &str) -> String {
    if n == 1 {
        format!("1 {one}")
    } else {
        format!("{n} {many}")
    }
}

/// The words, the colour and the one-click action for a working copy.
pub fn copy_state(
    changes: u64,
    ahead: u64,
    behind: u64,
    follows_github: bool,
) -> (String, &'static str, &'static str, String) {
    let mut parts = Vec::new();
    let mut short = Vec::new();
    if changes > 0 {
        parts.push(plural(changes, "change", "changes"));
        short.push(changes.to_string());
    }
    if ahead > 0 {
        parts.push(format!("{ahead} to push"));
        short.push(format!("↑{ahead}"));
    }
    if behind > 0 {
        parts.push(format!("{behind} behind"));
        short.push(format!("↓{behind}"));
    }
    if parts.is_empty() {
        parts.push("Clean".to_string());
    }
    if follows_github {
        parts.push("follows GitHub".to_string());
    }
    let tone = if changes > 0 || ahead > 0 {
        "yours"
    } else if behind > 0 {
        "github"
    } else {
        "quiet"
    };
    let action = match (changes, ahead, behind) {
        (0, a, 0) if a > 0 => "push",
        (0, 0, b) if b > 0 => "pull",
        _ => "open",
    };
    (parts.join(" · "), tone, action, short.join(" · "))
}

/// A working copy row from what git says and what the studio knows.
pub fn working_copy(
    local: &LocalRepo,
    repo: Option<&Repo>,
    owner_label: String,
    path: String,
) -> WorkingCopy {
    let follows_github = repo
        .and_then(|r| r.github.as_ref())
        .is_some_and(|g| g.home == "github");
    let (ahead, behind) = (local.ahead.unwrap_or(0), local.behind.unwrap_or(0));
    let (state, tone, action, short) =
        copy_state(local.changed_files, ahead, behind, follows_github);
    WorkingCopy {
        name: repo.map_or_else(|| local.folder_name(), |r| r.name.clone()),
        owner: owner_label,
        owner_name: repo.map(|r| r.owner.clone()).unwrap_or_default(),
        owner_kind: repo.map(|r| r.owner_kind.clone()).unwrap_or_default(),
        pinned: false,
        full_name: repo.map(|r| r.full_name.clone()),
        branch: local
            .branch
            .clone()
            .unwrap_or_else(|| "detached".to_string()),
        changes: local.changed_files,
        ahead,
        behind,
        state,
        tone,
        action,
        short,
        worktree_of: atelier_core::git::worktree::linked_worktree_main(std::path::Path::new(&path))
            .map(|main| main.to_string_lossy().into_owned()),
        path,
    }
}

fn need_tone(need: &Need) -> &'static str {
    match (need.kind, need.thread) {
        (NeedKind::RunFailed | NeedKind::SecurityAlert, _) => "fail",
        (NeedKind::AgentApproval, _) | (_, Thread::Agents | Thread::Connect) => "agents",
        (_, Thread::Github) => "github",
        _ => "yours",
    }
}

/// A "needs you" row from the studio's status.
pub fn need_row(need: &Need) -> NeedRow {
    let web = || NeedAction::OpenWeb {
        web_path: need.web_path.clone(),
    };
    let action = match need.action.as_ref() {
        Some(a) => match (a.kind.as_str(), a.job_id, a.run_id, a.number) {
            ("approve_agent", Some(job_id), _, _) => NeedAction::ApproveAgent { job_id },
            ("open_run", _, Some(run_id), _) => NeedAction::OpenLog {
                run_id,
                repo: need.repo.clone(),
                web_path: need.web_path.clone(),
            },
            ("open_pull", _, _, Some(number)) => NeedAction::OpenPull {
                repo: need.repo.clone(),
                number,
            },
            _ => match pull_from_web_path(&need.web_path) {
                Some((repo, number)) if a.kind == "open_pull" => {
                    NeedAction::OpenPull { repo, number }
                }
                _ => web(),
            },
        },
        None => web(),
    };
    let label = need
        .action
        .as_ref()
        .map(|a| a.label.clone())
        .filter(|l| !l.is_empty())
        .unwrap_or_else(|| "Open".to_string());
    let repo = short_repo(&need.repo);
    let detail = if repo.is_empty() || need.detail.starts_with(repo) {
        need.detail.clone()
    } else if need.detail.is_empty() {
        repo.to_string()
    } else {
        format!("{repo} · {}", need.detail)
    };
    NeedRow {
        id: need.id.clone(),
        tone: need_tone(need),
        title: need.title.clone(),
        detail,
        label,
        action,
    }
}

/// "Commits not pushed" is something only this machine knows.
pub fn push_need(copy: &WorkingCopy) -> Option<NeedRow> {
    (copy.ahead > 0).then(|| NeedRow {
        id: format!("local:push:{}", copy.path),
        tone: "yours",
        title: if copy.ahead == 1 {
            "1 commit is not pushed".to_string()
        } else {
            format!("{} commits are not pushed", copy.ahead)
        },
        detail: format!("{} · {}", copy.name, copy.branch),
        label: "Push".to_string(),
        action: NeedAction::Push {
            path: copy.path.clone(),
        },
    })
}

pub fn feed_row(item: &FeedItem) -> FeedRow {
    let (tone, tag) = match item.thread {
        Thread::Github => ("github", "via Tether"),
        Thread::Connect => ("connect", "via Connect"),
        Thread::Agents => ("agents", "Agents"),
        Thread::Yours | Thread::Unknown => ("yours", "You"),
    };
    let target = match pull_from_web_path(&item.web_path) {
        Some((repo, number)) => Some(Target::Pull { repo, number }),
        None if !item.web_path.is_empty() => Some(Target::Web {
            web_path: item.web_path.clone(),
        }),
        None => None,
    };
    FeedRow {
        tone,
        tag,
        text: item.text.clone(),
        at: item.at.clone(),
        target,
    }
}

/// `turn 3 of 20` → 0.15.
pub fn turn_progress(detail: &str) -> Option<f32> {
    let rest = detail.split("turn ").nth(1)?;
    let mut words = rest.split_whitespace();
    let turn: f32 = words.next()?.parse().ok()?;
    if words.next()? != "of" {
        return None;
    }
    let of: f32 = words
        .next()?
        .trim_matches(|c: char| !c.is_ascii_digit())
        .parse()
        .ok()?;
    (of > 0.0).then(|| (turn / of).clamp(0.0, 1.0))
}

pub fn engine_label(engine: &str) -> String {
    match engine {
        "claude-code" => "Claude Code".to_string(),
        "" => "Agent".to_string(),
        other => other.to_string(),
    }
}

/// What is running on this machine: the runner knows the turn, the studio
/// knows what the job is about.
pub fn running_rows(runner: Option<&RunnerStatus>, studio: &[RunningJob]) -> Vec<RunningRow> {
    let Some(runner) = runner else {
        return Vec::new();
    };
    runner
        .current
        .iter()
        .map(|job| {
            let known = studio.iter().find(|s| s.job_id == job.job_id);
            let mut parts: Vec<String> = Vec::new();
            let repo = short_repo(&job.repo);
            if !repo.is_empty() {
                parts.push(repo.to_string());
            }
            if let Some(known) = known.filter(|k| !k.detail.is_empty()) {
                // The studio's detail may already end with the turn.
                parts.push(
                    known
                        .detail
                        .split(" · turn ")
                        .next()
                        .unwrap_or_default()
                        .to_string(),
                );
            }
            if !job.detail.is_empty() {
                parts.push(job.detail.clone());
            }
            let engine = (job.kind == "agent").then(|| {
                engine_label(known.and_then(|k| k.engine.as_deref()).unwrap_or(
                    if runner.capabilities.iter().any(|c| c == "claude-code") {
                        "claude-code"
                    } else {
                        ""
                    },
                ))
            });
            RunningRow {
                job_id: job.job_id,
                title: if job.workflow.is_empty() {
                    format!("job {}", job.job_id)
                } else {
                    job.workflow.clone()
                },
                engine,
                progress: turn_progress(&job.detail),
                detail: parts.join(" · "),
                kind: job.kind.clone(),
                started_at: job.started_at.clone(),
            }
        })
        .collect()
}

pub fn runner_brief(status: Option<&RunnerStatus>) -> RunnerBrief {
    let Some(status) = status else {
        return RunnerBrief {
            running: false,
            taking_jobs: false,
            jobs: 0,
            line: "Runner isn’t running".to_string(),
            switch_line: "The runner isn’t running here".to_string(),
        };
    };
    let jobs = status.current.len();
    let doing = match jobs {
        0 => "idle".to_string(),
        1 => "1 job running".to_string(),
        n => format!("{n} jobs running"),
    };
    let (word, switch_line) = match (status.taking_jobs, status.paused_reason) {
        (true, _) if status.rules.pause_on_battery => ("on", "On · pauses on battery"),
        (true, _) => ("on", "On"),
        (false, Some(PausedReason::Battery)) => ("paused", "Paused · on battery"),
        (false, Some(PausedReason::JobsOff)) => ("off", "Off · this runner doesn’t take jobs"),
        (false, _) => ("off", "Off · jobs go to your other devices"),
    };
    RunnerBrief {
        running: true,
        taking_jobs: status.taking_jobs,
        jobs,
        line: format!("Runner {word} · {doing}"),
        switch_line: switch_line.to_string(),
    }
}

/// `in ~/Atelier and ~/Projects`.
pub fn roots_label(roots: &[String]) -> String {
    match roots {
        [] => String::new(),
        [one] => format!("in {one}"),
        [first @ .., last] => format!("in {} and {last}", first.join(", ")),
    }
}

impl Desk {
    /// The git folders on this machine: the ones the studio knows, and
    /// the rest. Without a studio list (offline) every folder is in the
    /// first list, unmatched.
    pub async fn all_copies(&self, repos: Option<&[Repo]>) -> (Vec<WorkingCopy>, Vec<WorkingCopy>) {
        let found = self.git().scan(&self.roots(), SCAN_DEPTH).await;
        let (mut copies, mut others) = (Vec::new(), Vec::new());
        for local in &found {
            let path = local.path.to_string_lossy().into_owned();
            let matched = repos.and_then(|repos| local.studio_repo(repos));
            match (matched, repos) {
                (Some(repo), _) => copies.push(working_copy(
                    local,
                    Some(repo),
                    self.account_label(&repo.owner),
                    path,
                )),
                (None, known) => {
                    // Where it lives instead, for the "Other folders" group.
                    let place = local
                        .remotes
                        .iter()
                        .find_map(|r| r.remote.as_ref())
                        .or(local.remote.as_ref())
                        .map(|r| r.host.clone())
                        .unwrap_or_else(|| "no remote".to_string());
                    let copy = working_copy(local, None, place, path);
                    if known.is_some() {
                        others.push(copy);
                    } else {
                        copies.push(copy);
                    }
                }
            }
        }
        let pinned = self.prefs().pinned;
        for copy in copies.iter_mut().chain(others.iter_mut()) {
            copy.pinned = pinned.contains(&copy.path);
        }
        let by_name = |a: &WorkingCopy, b: &WorkingCopy| {
            a.owner
                .to_lowercase()
                .cmp(&b.owner.to_lowercase())
                .then(a.name.to_lowercase().cmp(&b.name.to_lowercase()))
        };
        copies.sort_by(by_name);
        others.sort_by_key(|c| c.name.to_lowercase());
        (copies, others)
    }

    /// The working copies the studio knows (see [`Desk::all_copies`]).
    pub async fn working_copies(&self, repos: Option<&[Repo]>) -> Vec<WorkingCopy> {
        self.all_copies(repos).await.0
    }

    pub async fn overview(&self) -> CmdResult<Overview> {
        let settings = self.settings();
        let fresh = Duration::from_secs(20);
        let (status, repos, runner) = tokio::join!(
            async { self.watch(self.client()?.status().await) },
            self.repos(fresh),
            self.runner_status()
        );
        // Make sure account names are known for the owner labels.
        let _ = self.me(Duration::from_secs(300)).await;
        let mut studio_error = None;
        let status: Status = status.unwrap_or_else(|e| {
            studio_error = Some(e.message);
            Status::default()
        });
        let repos = match repos {
            Ok(repos) => Some(repos),
            Err(e) => {
                studio_error.get_or_insert(e.message);
                self.repos_cached()
            }
        };
        let (copies, others) = self.all_copies(repos.as_deref()).await;

        let mut needs: Vec<NeedRow> = status.needs.iter().map(need_row).collect();
        // Local needs sit among the studio's, after agents and failures.
        let urgent = needs
            .iter()
            .take_while(|n| matches!(n.tone, "agents" | "fail"))
            .count();
        let local: Vec<NeedRow> = copies.iter().filter_map(push_need).collect();
        needs.splice(urgent..urgent, local);

        let device_name = self.device_name();
        let elsewhere = status
            .running
            .iter()
            .filter(|job| {
                runner
                    .as_ref()
                    .is_none_or(|r| r.current.iter().all(|c| c.job_id != job.job_id))
            })
            .map(|job| {
                let what = if job.job.is_empty() || job.job == job.workflow {
                    job.workflow.clone()
                } else {
                    format!("{} · {}", job.workflow, job.job)
                };
                if job.runner.is_empty() {
                    what
                } else {
                    format!("{what} on {}", job.runner)
                }
            })
            .collect();
        let roots: Vec<String> = settings
            .repo_roots()
            .iter()
            .map(|r| self.display_path(r))
            .collect();
        Ok(Overview {
            needs,
            running: running_rows(runner.as_ref(), &status.running),
            elsewhere,
            feed: status.feed.iter().map(feed_row).collect(),
            unread: status.unread,
            pulls_open: repos
                .as_ref()
                .map_or(0, |r| r.iter().map(|r| r.open_pulls).sum()),
            copies,
            others,
            roots_label: roots_label(&roots),
            runner: runner_brief(runner.as_ref()),
            device_name,
            studio_error,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn copy_states_read_like_the_design() {
        assert_eq!(
            copy_state(3, 1, 0, false),
            (
                "3 changes · 1 to push".into(),
                "yours",
                "open",
                "3 · ↑1".into()
            )
        );
        assert_eq!(
            copy_state(0, 2, 0, false),
            ("2 to push".into(), "yours", "push", "↑2".into())
        );
        assert_eq!(
            copy_state(0, 0, 2, false),
            ("2 behind".into(), "github", "pull", "↓2".into())
        );
        assert_eq!(
            copy_state(0, 0, 0, false),
            ("Clean".into(), "quiet", "open", String::new())
        );
        assert_eq!(
            copy_state(0, 0, 5, true),
            (
                "5 behind · follows GitHub".into(),
                "github",
                "pull",
                "↓5".into()
            )
        );
        assert_eq!(copy_state(1, 0, 0, false).0, "1 change");
        // Diverged or dirty: nothing is done with one click.
        assert_eq!(copy_state(0, 1, 1, false).2, "open");
        assert_eq!(copy_state(2, 0, 3, false).2, "open");
    }

    #[test]
    fn turns_become_progress() {
        assert_eq!(turn_progress("turn 3 of 20"), Some(0.15));
        assert_eq!(turn_progress("pull request 14 · turn 10 of 20."), Some(0.5));
        assert_eq!(turn_progress("step 3 of 5"), None);
        assert_eq!(turn_progress("turn x of 20"), None);
        assert_eq!(turn_progress("turn 3 of 0"), None);
        assert_eq!(
            roots_label(&["~/Atelier".into(), "~/Projects".into()]),
            "in ~/Atelier and ~/Projects"
        );
        assert_eq!(roots_label(&["~/Atelier".into()]), "in ~/Atelier");
    }
}
