//! Looking after a repository in the studio from its working copy: a new
//! name, deleting it there, pinning it in the sidebar, and its runs
//! gathered by pull request.

use std::time::Duration;

use atelier_core::api::{PullSummary, Repo, Run, RunsQuery};
use serde::Serialize;

use crate::desk::Desk;
use crate::device::{duration_label, status_badge};
use crate::error::{CmdError, CmdResult};
use crate::paths;
use crate::pulls::Badge;

/// What a rename did.
#[derive(Clone, Debug, Serialize)]
pub struct Renamed {
    pub full_name: String,
    pub name: String,
    /// The remotes of the working copy that now point at the new address.
    pub remotes: Vec<String>,
    /// Why the remotes couldn't be changed, when they couldn't. The rename
    /// itself still happened.
    pub remote_error: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct RunRow {
    pub run_id: u64,
    pub status: Badge,
    /// The run finished badly: its log is worth opening.
    pub failed: bool,
    /// `ci`.
    pub name: String,
    /// `push on feature/tunnel-ui`.
    pub detail: String,
    pub duration: String,
    pub started_at: String,
    pub web_path: Option<String>,
}

/// Runs that belong together: one pull request's, or the rest.
#[derive(Clone, Debug, Serialize)]
pub struct RunGroup {
    /// The pull request these runs are for; `None` for runs on branches
    /// without an open pull request.
    pub pull: Option<u64>,
    /// `Pull request 14 · Remote access switch in the tray menu`.
    pub label: String,
    pub runs: Vec<RunRow>,
}

#[derive(Clone, Debug, Serialize)]
pub struct RunsView {
    pub repo: String,
    pub groups: Vec<RunGroup>,
    /// The repository's Actions page in the studio.
    pub web_path: String,
}

fn run_row(run: &Run) -> RunRow {
    let detail = match (run.event.as_str(), run.branch.as_str()) {
        ("", "") => String::new(),
        ("", branch) => branch.to_string(),
        (event, "") => event.to_string(),
        (event, branch) => format!("{event} on {branch}"),
    };
    RunRow {
        run_id: run.run_id,
        status: status_badge(&run.status),
        failed: run.status == "failure",
        name: if run.workflow.is_empty() {
            format!("run {}", run.number.unwrap_or(run.run_id))
        } else {
            run.workflow.clone()
        },
        detail,
        duration: run.duration_s.map(duration_label).unwrap_or_default(),
        started_at: run.started_at.clone(),
        web_path: run.web_path.clone(),
    }
}

/// Gathers runs under the open pull request whose branch they ran on. The
/// studio doesn't say which pull request a run belongs to, so the branch
/// decides. Pull requests come first, in the order of their newest run;
/// everything else follows as "Other runs".
pub fn group_runs(runs: &[Run], pulls: &[PullSummary]) -> Vec<RunGroup> {
    let mut groups: Vec<RunGroup> = Vec::new();
    let mut rest = Vec::new();
    for run in runs {
        let pull = pulls
            .iter()
            .find(|p| !run.branch.is_empty() && p.head == run.branch);
        match pull {
            Some(pull) => match groups.iter_mut().find(|g| g.pull == Some(pull.number)) {
                Some(group) => group.runs.push(run_row(run)),
                None => groups.push(RunGroup {
                    pull: Some(pull.number),
                    label: format!("Pull request {} · {}", pull.number, pull.title),
                    runs: vec![run_row(run)],
                }),
            },
            None => rest.push(run_row(run)),
        }
    }
    if !rest.is_empty() {
        groups.push(RunGroup {
            pull: None,
            label: if groups.is_empty() {
                "Runs".to_string()
            } else {
                "Other runs".to_string()
            },
            runs: rest,
        });
    }
    groups
}

/// Where the owner lets a device rename and delete repositories.
pub const DEVICES_PAGE: &str = "/settings/devices";

/// The studio says no to a rename or a delete: say so kindly, and where it
/// can be changed or done instead. `device_not_allowed` means the switch
/// for this device is off; any other 403 means the person may not
/// administer the repository at all.
fn not_allowed(e: CmdError, repo: &Repo, machine: &str) -> CmdError {
    match e.kind.as_str() {
        "device_not_allowed" => CmdError::new(
            "device_not_allowed",
            format!("This {machine} isn’t allowed to rename or delete repositories yet."),
        )
        .hint(
            "You can allow it in your studio, under Settings → Devices, or do it in the browser.",
        ),
        "forbidden" | "http_403" => CmdError::new(
            "not_allowed",
            format!("You can’t rename or delete {} from here.", repo.name),
        )
        .hint(format!(
            "If you may, you can do it in the browser, on {}’s settings page.",
            repo.name
        )),
        _ => e,
    }
}

impl Desk {
    /// The studio repository a working copy belongs to, read fresh.
    async fn studio_repo_at(&self, path: &str) -> CmdResult<(std::path::PathBuf, Repo)> {
        let dir = self.repo_path(path).await?;
        let local = self.git().inspect(&dir).await?;
        self.forget_repos();
        let repos = self.repos(Duration::ZERO).await?;
        let repo = local.studio_repo(&repos).cloned().ok_or_else(|| {
            CmdError::new("not_in_studio", "This folder isn’t in your studio.")
                .hint("Only a repository in your studio can be renamed or deleted there.")
        })?;
        Ok((dir, repo))
    }

    /// Gives the repository a new name in the studio, then points the
    /// working copy's remotes that named the old address at the new one.
    pub async fn repo_rename(&self, path: &str, new_name: &str) -> CmdResult<Renamed> {
        let new_name = paths::safe_segment(new_name.trim())?;
        let (dir, old) = self.studio_repo_at(path).await?;
        if new_name == old.name {
            return Err(CmdError::invalid("That is its name already."));
        }
        let client = self.client()?;
        let new = self
            .watch(client.rename_repo(&old.owner, &old.name, new_name).await)
            .map_err(|e| not_allowed(e, &old, self.machine()))?;
        self.forget_repos();
        let (remotes, remote_error) = if new.name.is_empty() {
            // An answer without the repository: leave the remotes be.
            (Vec::new(), None)
        } else {
            match self.git().retarget_remotes(&dir, &old, &new).await {
                Ok(remotes) => (remotes, None),
                Err(e) => (Vec::new(), Some(CmdError::from(e).message)),
            }
        };
        Ok(Renamed {
            full_name: if new.full_name.is_empty() {
                format!("{}/{new_name}", old.owner)
            } else {
                new.full_name
            },
            name: new_name.to_string(),
            remotes,
            remote_error,
        })
    }

    /// Deletes the repository from the studio. `typed` must be its name
    /// (or `owner/name`), exactly. The folder on this machine is never
    /// touched. Returns the full name of what was deleted.
    pub async fn repo_delete(&self, path: &str, typed: &str) -> CmdResult<String> {
        let (_, repo) = self.studio_repo_at(path).await?;
        let typed = typed.trim();
        if typed != repo.name && typed != repo.full_name {
            return Err(CmdError::new(
                "confirm_mismatch",
                format!("Type {} to delete it.", repo.name),
            ));
        }
        let client = self.client()?;
        self.watch(client.delete_repo(&repo.owner, &repo.name).await)
            .map_err(|e| not_allowed(e, &repo, self.machine()))?;
        self.forget_repos();
        Ok(repo.full_name)
    }

    /// Pins a working copy to the top of the sidebar, or lets it go.
    pub async fn set_pinned(&self, path: &str, on: bool) -> CmdResult<()> {
        let key = if on {
            self.repo_path(path).await?.to_string_lossy().into_owned()
        } else {
            // A pinned folder that has gone can still be unpinned.
            path.to_string()
        };
        self.update_prefs(|prefs| {
            prefs.pinned.retain(|p| p != &key && p != path);
            if on {
                prefs.pinned.push(key.clone());
            }
        })
        .map(|_| ())
    }

    /// Folds an owner's group in the sidebar shut, or opens it again.
    pub fn set_group_collapsed(&self, owner: &str, collapsed: bool) -> CmdResult<()> {
        let key = owner.trim().to_lowercase();
        if key.is_empty() || key.len() > 200 {
            return Err(CmdError::invalid("That isn’t a group in the sidebar."));
        }
        self.update_prefs(|prefs| {
            prefs.collapsed.retain(|c| c != &key);
            if collapsed {
                prefs.collapsed.push(key.clone());
            }
        })
        .map(|_| ())
    }

    /// A repository's recent runs, gathered by pull request.
    pub async fn repo_runs(&self, repo: &str) -> CmdResult<RunsView> {
        paths::split_full_name(repo)?;
        let client = self.client()?;
        let query = RunsQuery {
            limit: Some(40),
            runner: None,
            repo: Some(repo.to_string()),
        };
        let (runs, pulls) = tokio::join!(client.runs(&query), client.pulls_in(repo));
        let mut runs = self.watch(runs)?;
        // An older studio may ignore the filter.
        runs.retain(|r| r.repo.is_empty() || r.repo.eq_ignore_ascii_case(repo));
        // Without the pull requests the runs are still worth showing.
        let pulls = pulls.unwrap_or_default();
        Ok(RunsView {
            repo: repo.to_string(),
            groups: group_runs(&runs, &pulls),
            web_path: format!("/{repo}/actions"),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn run(id: u64, branch: &str, status: &str) -> Run {
        Run {
            run_id: id,
            workflow: "ci".into(),
            event: "push".into(),
            branch: branch.into(),
            status: status.into(),
            duration_s: Some(62),
            ..Run::default()
        }
    }

    #[test]
    fn runs_gather_under_their_pull_request() {
        let pulls = [
            PullSummary {
                number: 14,
                title: "Tunnel switch".into(),
                head: "feature/tunnel-ui".into(),
                ..PullSummary::default()
            },
            PullSummary {
                number: 15,
                title: "Quiet".into(),
                head: "quiet".into(),
                ..PullSummary::default()
            },
        ];
        let runs = [
            run(3, "main", "success"),
            run(2, "feature/tunnel-ui", "failure"),
            run(1, "feature/tunnel-ui", "success"),
            run(0, "", "success"),
        ];
        let groups = group_runs(&runs, &pulls);
        assert_eq!(groups.len(), 2);
        assert_eq!(groups[0].pull, Some(14));
        assert_eq!(groups[0].label, "Pull request 14 · Tunnel switch");
        assert_eq!(
            groups[0].runs.iter().map(|r| r.run_id).collect::<Vec<_>>(),
            [2, 1]
        );
        assert!(groups[0].runs[0].failed);
        assert_eq!(groups[0].runs[0].detail, "push on feature/tunnel-ui");
        assert_eq!(groups[0].runs[0].duration, "1m 02s");
        assert_eq!(
            (groups[1].pull, groups[1].label.as_str()),
            (None, "Other runs")
        );
        assert_eq!(groups[1].runs.len(), 2);
        assert_eq!(groups[1].runs[1].detail, "push");

        // No pull requests known: one plain group.
        let plain = group_runs(&runs, &[]);
        assert_eq!(plain.len(), 1);
        assert_eq!(plain[0].label, "Runs");
        assert!(group_runs(&[], &pulls).is_empty());
    }
}
