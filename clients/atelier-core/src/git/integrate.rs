//! Bringing work together: merge, rebase, tidying up commits (an
//! interactive rebase driven from a plan, never an editor), cherry-pick,
//! revert and reset; and the one question every screen asks first: is
//! something still under way here, and how does it continue or stop.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use super::conflict::{Choice, ConflictKind};
use super::undo::{Pending, StashRef};
use super::util::{check_rev, count, plain_refusal, read_trimmed, short, Call, FIELD, RECORD};
use super::Git;
use crate::error::{Error, Result};

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum OperationKind {
    #[default]
    None,
    Merge,
    Rebase,
    CherryPick,
    Revert,
    /// Put-aside changes (or changes carried across a switch) that didn't
    /// fit cleanly: git has no name for this state, only conflicted files.
    StashApply,
}

/// One step of a rebase that is under way.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct PlanItem {
    pub id: String,
    pub short: String,
    /// `keep`, `reword`, `squash`, `fixup` or `drop`.
    pub action: String,
    pub subject: String,
    pub done: bool,
    /// The step it stopped at.
    pub current: bool,
}

/// What is under way in a working tree.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize)]
pub struct OperationState {
    pub kind: OperationKind,
    /// What started it, when the app did: `merge`, `pull`, `rebase`,
    /// `tidy`, `cherry_pick`, `revert`, `stash_apply`, `stash_pop`, `switch`.
    pub cause: Option<String>,
    /// `Merging main into feature/tunnel-ui`.
    pub title: String,
    /// Your side: the branch being worked on.
    pub yours: Option<String>,
    /// The other side: what is being brought in.
    pub theirs: Option<String>,
    pub step: Option<u32>,
    pub total: Option<u32>,
    pub plan: Vec<PlanItem>,
    /// Files that still have an unresolved conflict.
    pub conflicts: u32,
    pub can_continue: bool,
    pub can_skip: bool,
}

/// How an action ended: done, or stopped at conflicts.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct OpOutcome {
    pub done: bool,
    /// Plain words for the Undo bar (`Rebased feature/x onto main`).
    pub summary: String,
    pub conflicts: Vec<String>,
    pub head: Option<String>,
    /// The entry in Recent actions, once it is done.
    pub journal: Option<u64>,
    pub state: OperationState,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MergeMode {
    /// Fast-forward when possible, a merge commit otherwise.
    Auto,
    /// Always a merge commit.
    NoFf,
    /// Fast-forward or nothing.
    FfOnly,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ResetMode {
    /// The commits' changes stay, staged.
    Soft,
    /// The commits' changes stay, unstaged.
    Mixed,
    /// The commits' changes and anything uncommitted are thrown away
    /// (the uncommitted part goes to Recently discarded).
    Hard,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PlanAction {
    Keep,
    Reword,
    Squash,
    Fixup,
    Drop,
}

/// One line of a tidy-up plan, oldest commit first.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct PlanStep {
    pub id: String,
    pub action: PlanAction,
    /// The new message for `reword` (and, optionally, for `squash`).
    #[serde(default)]
    pub message: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct PlanCommit {
    pub id: String,
    pub short: String,
    pub subject: String,
    pub body: String,
    pub author: String,
    /// RFC 3339.
    pub time: String,
    /// Some remote has it: rewriting it means a force push.
    pub pushed: bool,
}

/// The commits a tidy-up would work on.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct RebasePlan {
    pub branch: Option<String>,
    /// The commit that stays exactly as it is.
    pub base: String,
    pub base_short: String,
    /// A branch that points at the base, when one does.
    pub base_name: Option<String>,
    /// Oldest first.
    pub commits: Vec<PlanCommit>,
    /// There is a merge among them, so they can't be tidied here.
    pub has_merges: bool,
}

/// What stepping a branch back would do, for the dialog that asks.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct ResetPreview {
    pub branch: Option<String>,
    pub target: String,
    pub target_short: String,
    /// Commits the branch steps back.
    pub commits: u64,
    /// How many of those some remote already has.
    pub pushed: u64,
    /// Uncommitted files right now.
    pub uncommitted: usize,
}

/// A note kept while something is under way: what started it, and which
/// regions of which conflicted files have been chosen so far.
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub(crate) struct Marker {
    pub cause: String,
    #[serde(default)]
    pub theirs: Option<String>,
    #[serde(default)]
    pub stash: Option<StashRef>,
    #[serde(default)]
    pub pop: bool,
    /// Names the stop the notes below belong to.
    #[serde(default)]
    pub stop: String,
    /// The files that were in conflict at this stop.
    #[serde(default)]
    pub paths: Vec<String>,
    #[serde(default)]
    pub choices: BTreeMap<String, Vec<Option<Choice>>>,
    /// How each resolved file was resolved: `yours`, `theirs` or `mixed`.
    #[serde(default)]
    pub resolved: BTreeMap<String, String>,
    #[serde(default)]
    pub kinds: BTreeMap<String, ConflictKind>,
}

fn sh_quote(path: &Path) -> String {
    format!("'{}'", path.to_string_lossy().replace('\'', r"'\''"))
}

impl Git {
    async fn marker_path(&self, repo: &Path) -> Result<PathBuf> {
        Ok(self.app_dir_local(repo).await?.join("op.json"))
    }

    pub(crate) async fn marker_read(&self, repo: &Path) -> Option<Marker> {
        let path = self.marker_path(repo).await.ok()?;
        serde_json::from_str(&std::fs::read_to_string(path).ok()?).ok()
    }

    pub(crate) async fn marker_write(&self, repo: &Path, marker: &Marker) -> Result<()> {
        let path = self.marker_path(repo).await?;
        let text = serde_json::to_string(marker)
            .map_err(|e| Error::Invalid(format!("Couldn’t write the note: {e}")))?;
        std::fs::write(&path, text)
            .map_err(|e| Error::io(format!("Couldn’t write {}", path.display()), e))
    }

    pub(crate) async fn marker_clear(&self, repo: &Path) {
        if let Ok(dir) = self.app_dir_local(repo).await {
            let _ = std::fs::remove_file(dir.join("op.json"));
            let _ = std::fs::remove_dir_all(dir.join("rebase"));
        }
    }

    /// What names this stop: the conflict notes are only good for it.
    async fn stop_key(&self, repo: &Path, kind: OperationKind) -> String {
        let Ok(dir) = self.git_dir(repo).await else {
            return String::new();
        };
        let read = |name: &str| read_trimmed(&dir.join(name)).unwrap_or_default();
        match kind {
            OperationKind::Merge => format!("merge:{}", read("MERGE_HEAD")),
            OperationKind::Rebase => format!(
                "rebase:{}:{}",
                read("rebase-merge/msgnum"),
                read("rebase-merge/stopped-sha")
            ),
            OperationKind::CherryPick => format!("pick:{}", read("CHERRY_PICK_HEAD")),
            OperationKind::Revert => format!("revert:{}", read("REVERT_HEAD")),
            OperationKind::StashApply => "apply".to_string(),
            OperationKind::None => String::new(),
        }
    }

    /// The marker for the current stop, made or refreshed as needed.
    pub(crate) async fn marker_current(&self, repo: &Path, kind: OperationKind) -> Result<Marker> {
        let stop = self.stop_key(repo, kind).await;
        let mut marker = self.marker_read(repo).await.unwrap_or_default();
        if marker.stop != stop {
            marker.stop = stop;
            marker.paths = self.unmerged_paths(repo).await?;
            marker.choices.clear();
            marker.resolved.clear();
            self.marker_write(repo, &marker).await?;
        }
        Ok(marker)
    }

    pub(crate) async fn operation_in_progress(&self, repo: &Path) -> Result<bool> {
        Ok(self.operation_state(repo).await?.kind != OperationKind::None)
    }

    /// What is under way in this working tree, if anything.
    pub async fn operation_state(&self, repo: &Path) -> Result<OperationState> {
        let dir = self.git_dir(repo).await?;
        let conflicts = self.unmerged_paths(repo).await?.len() as u32;
        let marker = self.marker_read(repo).await;
        let cause = marker.as_ref().map(|m| m.cause.clone());
        let mut state = OperationState {
            conflicts,
            can_continue: conflicts == 0,
            yours: self.current_branch(repo).await?,
            theirs: marker.as_ref().and_then(|m| m.theirs.clone()),
            cause: cause.clone(),
            ..OperationState::default()
        };
        let name_of = |sha: String| async move {
            let named = self
                .maybe(
                    repo,
                    &[
                        "name-rev",
                        "--name-only",
                        "--no-undefined",
                        "--exclude=refs/atelier/*",
                        &sha,
                    ],
                )
                .await
                .ok()
                .flatten()
                .filter(|n| !n.contains('~') && !n.contains('^'));
            named.unwrap_or_else(|| short(&sha))
        };

        let rebase = ["rebase-merge", "rebase-apply"]
            .into_iter()
            .map(|d| dir.join(d))
            .find(|d| d.is_dir());
        if let Some(rebase) = rebase {
            state.kind = OperationKind::Rebase;
            state.can_skip = true;
            let read = |name: &str| read_trimmed(&rebase.join(name));
            state.yours = read("head-name")
                .map(|n| n.trim_start_matches("refs/heads/").to_string())
                .filter(|n| n != "detached HEAD")
                .or(state.yours);
            state.step = read("msgnum")
                .or_else(|| read("next"))
                .and_then(|n| n.parse().ok());
            state.total = read("end")
                .or_else(|| read("last"))
                .and_then(|n| n.parse().ok());
            if state.theirs.is_none() {
                if let Some(onto) = read("onto") {
                    state.theirs = Some(name_of(onto).await);
                }
            }
            let done = std::fs::read_to_string(rebase.join("done")).unwrap_or_default();
            let todo = std::fs::read_to_string(rebase.join("git-rebase-todo")).unwrap_or_default();
            state.plan = plan_items(&done, &todo);
            let (step, total) = (state.step.unwrap_or(0), state.total.unwrap_or(0));
            let yours = state.yours.clone().unwrap_or_else(|| "HEAD".into());
            let theirs = state.theirs.clone().unwrap_or_default();
            state.title = match cause.as_deref() {
                Some("tidy") => format!("Tidying up commits — stopped at {step} of {total}"),
                Some("pull") => format!("Pulling into {yours} — stopped at {step} of {total}"),
                _ => format!("Rebasing {yours} onto {theirs} — stopped at {step} of {total}"),
            };
        } else if let Some(sha) = read_trimmed(&dir.join("CHERRY_PICK_HEAD")) {
            state.kind = OperationKind::CherryPick;
            state.can_skip = true;
            state.theirs = Some(short(&sha));
            state.title = format!(
                "Cherry-picking {} onto {}",
                short(&sha),
                state.yours.as_deref().unwrap_or("HEAD")
            );
        } else if let Some(sha) = read_trimmed(&dir.join("REVERT_HEAD")) {
            state.kind = OperationKind::Revert;
            state.can_skip = true;
            state.theirs = Some(short(&sha));
            state.title = format!("Reverting {}", short(&sha));
        } else if let Some(sha) = read_trimmed(&dir.join("MERGE_HEAD")) {
            state.kind = OperationKind::Merge;
            if state.theirs.is_none() {
                let first = sha.lines().next().unwrap_or_default().to_string();
                state.theirs = Some(name_of(first).await);
            }
            let yours = state.yours.as_deref().unwrap_or("HEAD");
            let theirs = state.theirs.as_deref().unwrap_or_default();
            state.title = match cause.as_deref() {
                Some("pull") => format!("Pulling {theirs} into {yours}"),
                _ => format!("Merging {theirs} into {yours}"),
            };
        } else if conflicts > 0 {
            state.kind = OperationKind::StashApply;
            state.title = match (
                cause.as_deref(),
                marker.as_ref().and_then(|m| m.stash.as_ref()),
            ) {
                (Some("switch"), _) => format!(
                    "Bringing your changes along to {}",
                    state.yours.as_deref().unwrap_or("the branch")
                ),
                (_, Some(stash)) => format!("Applying put-aside changes “{}”", stash.message),
                _ => "Applying put-aside changes".to_string(),
            };
            if state.cause.is_none() {
                state.cause = Some("stash_apply".to_string());
            }
        } else if marker
            .as_ref()
            .is_some_and(|m| matches!(m.cause.as_str(), "stash_apply" | "stash_pop" | "switch"))
        {
            // Every conflict of an apply was resolved: it waits to be finished.
            state.kind = OperationKind::StashApply;
            state.title = "Applying put-aside changes".to_string();
        } else {
            state.cause = None;
            state.theirs = None;
            state.can_continue = false;
            if marker.is_some() {
                self.marker_clear(repo).await;
            }
        }
        Ok(state)
    }

    async fn outcome(
        &self,
        repo: &Path,
        done: bool,
        summary: String,
        journal: Option<u64>,
    ) -> Result<OpOutcome> {
        Ok(OpOutcome {
            done,
            summary,
            conflicts: self.unmerged_paths(repo).await?,
            head: self.head_sha(repo).await?,
            journal,
            state: self.operation_state(repo).await?,
        })
    }

    /// Runs one history-moving command that was already written to the
    /// journal. It either finishes, stops at conflicts (the entry stays
    /// open until the action is continued or stopped), or didn't happen.
    /// `{commits}` in `summary` becomes the number of commits HEAD gained.
    #[allow(clippy::too_many_arguments)]
    pub(crate) async fn run_op(
        &self,
        repo: &Path,
        pending: Pending,
        args: &[String],
        call: Call<'_>,
        mut marker: Marker,
        summary: &str,
        doing: &str,
    ) -> Result<OpOutcome> {
        let before = self.head_sha(repo).await?;
        let out = self.exec(repo, args, call).await?;
        if out.ok() {
            self.marker_clear(repo).await;
            let after = self.head_sha(repo).await?;
            if before == after {
                self.journal_cancel(repo, pending).await;
                return self
                    .outcome(repo, true, "Already up to date.".into(), None)
                    .await;
            }
            let summary = self.fill_commits(repo, summary, before.as_deref()).await;
            let journal = self
                .journal_finish(repo, pending, Some(summary.clone()))
                .await?;
            return self.outcome(repo, true, summary, Some(journal)).await;
        }
        let state = self.operation_state(repo).await?;
        if state.kind == OperationKind::None {
            self.journal_cancel(repo, pending).await;
            return Err(plain_refusal(out.error(), doing));
        }
        if state.conflicts == 0
            && matches!(
                state.kind,
                OperationKind::CherryPick | OperationKind::Revert
            )
        {
            // Nothing left to apply: the changes are already here.
            let verb = if state.kind == OperationKind::Revert {
                "revert"
            } else {
                "cherry-pick"
            };
            let _ = self.exec(repo, &[verb, "--abort"], Call::default()).await;
            self.journal_cancel(repo, pending).await;
            return Err(Error::Invalid(
                "That commit’s changes are already here, so there is nothing to do.".into(),
            ));
        }
        marker.stop = self.stop_key(repo, state.kind).await;
        marker.paths = self.unmerged_paths(repo).await?;
        self.marker_write(repo, &marker).await?;
        self.outcome(repo, false, state.title.clone(), None).await
    }

    async fn fill_commits(&self, repo: &Path, summary: &str, before: Option<&str>) -> String {
        if !summary.contains("{commits}") {
            return summary.to_string();
        }
        let n = match before {
            Some(before) => self
                .count_commits(repo, &[&format!("{before}..HEAD")])
                .await
                .unwrap_or(0),
            None => 0,
        };
        summary.replace("{commits}", &count(n, "commit", "commits"))
    }

    fn branch_label(branch: &Option<String>) -> String {
        branch.clone().unwrap_or_else(|| "HEAD".to_string())
    }

    /// Merges `branch` into what is checked out.
    pub async fn merge(&self, repo: &Path, branch: &str, mode: MergeMode) -> Result<OpOutcome> {
        let target = self.resolve_commit(repo, branch).await?;
        let current = self.current_branch(repo).await?;
        let here = Self::branch_label(&current);
        if self.head_sha(repo).await?.as_deref() == Some(target.as_str()) {
            return self
                .outcome(repo, true, "Already up to date.".into(), None)
                .await;
        }
        let summary = format!("Merged {branch} into {here}");
        let refs: Vec<String> = self.current_ref(repo).await?.into_iter().collect();
        let pending = self.journal_begin(repo, "merge", &summary, &refs).await?;
        let mut args: Vec<String> = vec!["merge".into(), "--no-edit".into()];
        match mode {
            MergeMode::Auto => args.push("--ff".into()),
            MergeMode::NoFf => args.push("--no-ff".into()),
            MergeMode::FfOnly => args.push("--ff-only".into()),
        }
        args.push(branch.to_string());
        let marker = Marker {
            cause: "merge".into(),
            theirs: Some(branch.to_string()),
            ..Marker::default()
        };
        self.run_op(
            repo,
            pending,
            &args,
            Call::default().no_editor(),
            marker,
            &summary,
            &format!("Merging {branch}"),
        )
        .await
        .map_err(|e| match e {
            Error::Git { message, .. } if message.contains("Not possible to fast-forward") => {
                Error::Invalid(format!(
                    "{here} and {branch} have both moved on, so this can’t be a fast-forward."
                ))
            }
            other => other,
        })
    }

    /// Replays the current branch's own commits on top of `onto`.
    pub async fn rebase_onto(&self, repo: &Path, onto: &str) -> Result<OpOutcome> {
        self.resolve_commit(repo, onto).await?;
        let current = self.current_branch(repo).await?;
        let here = Self::branch_label(&current);
        let summary = format!("Rebased {here} onto {onto}");
        let refs: Vec<String> = self.current_ref(repo).await?.into_iter().collect();
        let pending = self.journal_begin(repo, "rebase", &summary, &refs).await?;
        let args: Vec<String> = vec!["rebase".into(), onto.to_string()];
        let marker = Marker {
            cause: "rebase".into(),
            theirs: Some(onto.to_string()),
            ..Marker::default()
        };
        self.run_op(
            repo,
            pending,
            &args,
            Call::default().no_editor(),
            marker,
            &summary,
            &format!("Rebasing onto {onto}"),
        )
        .await
    }

    /// The commits a tidy-up from `base` would work on. Without a base it
    /// starts where the branch left its upstream, or `main` / `master`.
    pub async fn rebase_plan(&self, repo: &Path, base: Option<&str>) -> Result<RebasePlan> {
        let branch = self.current_branch(repo).await?;
        let base = match base {
            Some(base) => self.resolve_commit(repo, base).await?,
            None => {
                let mut found = None;
                for candidate in ["@{upstream}", "main", "master"] {
                    if Some(candidate) == branch.as_deref() {
                        continue;
                    }
                    if let Some(sha) = self.maybe(repo, &["merge-base", "HEAD", candidate]).await? {
                        found = Some(sha);
                        break;
                    }
                }
                found.ok_or_else(|| {
                    Error::Invalid("Choose the commit to start tidying from.".into())
                })?
            }
        };
        let range = format!("{base}..HEAD");
        let format = format!("--format=%H{FIELD}%s{FIELD}%b{FIELD}%an{FIELD}%aI{RECORD}");
        let out = self
            .text(repo, &["log", "--reverse", "--no-color", &format, &range])
            .await?;
        let unpushed = self
            .text(repo, &["rev-list", &range, "--not", "--remotes"])
            .await?;
        let unpushed: std::collections::HashSet<&str> = unpushed.lines().collect();
        let commits: Vec<PlanCommit> = out
            .split(RECORD)
            .filter_map(|record| {
                let mut parts = record.trim_start_matches(['\n', '\r']).split(FIELD);
                let id = parts.next().filter(|s| !s.is_empty())?.to_string();
                Some(PlanCommit {
                    short: short(&id),
                    subject: parts.next()?.to_string(),
                    body: parts.next()?.trim().to_string(),
                    author: parts.next()?.to_string(),
                    time: parts.next()?.trim().to_string(),
                    pushed: !unpushed.contains(id.as_str()),
                    id,
                })
            })
            .collect();
        let merges = self
            .text(repo, &["rev-list", "--merges", "--max-count=1", &range])
            .await?;
        let base_name = self
            .maybe(
                repo,
                &[
                    "for-each-ref",
                    "--count=1",
                    "--format=%(refname:short)",
                    "--points-at",
                    &base,
                    "refs/heads",
                ],
            )
            .await?;
        Ok(RebasePlan {
            branch,
            base_short: short(&base),
            base,
            base_name,
            commits,
            has_merges: !merges.trim().is_empty(),
        })
    }

    /// Tidies up the commits after `base` according to `plan` (oldest
    /// first, every commit exactly once, in the order they should end up).
    /// Git's editor is never opened: the todo list and the new messages are
    /// written to the git folder and handed over.
    pub async fn rebase_interactive(
        &self,
        repo: &Path,
        base: &str,
        plan: &[PlanStep],
    ) -> Result<OpOutcome> {
        let current = self.rebase_plan(repo, Some(base)).await?;
        if current.has_merges {
            return Err(Error::Invalid(
                "There is a merge among these commits, so they can’t be tidied up here.".into(),
            ));
        }
        let known: Vec<&str> = current.commits.iter().map(|c| c.id.as_str()).collect();
        let mut seen = std::collections::HashSet::new();
        let mut steps: Vec<(&str, &PlanStep)> = Vec::new();
        for step in plan {
            let full = known
                .iter()
                .copied()
                .find(|id| !step.id.is_empty() && id.starts_with(step.id.as_str()))
                .ok_or_else(|| {
                    Error::Invalid(format!(
                        "{} isn’t one of the commits being tidied up.",
                        short(&step.id)
                    ))
                })?;
            if !seen.insert(full) {
                return Err(Error::Invalid(format!(
                    "{} is in the plan twice.",
                    short(full)
                )));
            }
            steps.push((full, step));
        }
        if steps.len() != known.len() {
            return Err(Error::Invalid(
                "The branch has changed since this plan was made. Look again.".into(),
            ));
        }
        let first_kept = steps.iter().find(|(_, s)| s.action != PlanAction::Drop);
        if first_kept
            .is_some_and(|(_, s)| matches!(s.action, PlanAction::Squash | PlanAction::Fixup))
        {
            return Err(Error::Invalid(
                "The first commit has nothing above it to fold into.".into(),
            ));
        }
        for (_, step) in &steps {
            let empty = step.message.as_deref().is_none_or(|m| m.trim().is_empty());
            if step.action == PlanAction::Reword && empty {
                return Err(Error::Invalid("Write the new message first.".into()));
            }
        }
        let unchanged = steps
            .iter()
            .zip(&known)
            .all(|((id, step), was)| id == was && step.action == PlanAction::Keep);
        if unchanged {
            return self
                .outcome(repo, true, "Nothing to tidy up.".into(), None)
                .await;
        }
        if self.tracked_changes(repo).await? > 0 {
            return Err(Error::Invalid(
                "Commit your changes or put them aside before tidying up.".into(),
            ));
        }

        // The todo list and the messages live in the git folder.
        let dir = self.app_dir_local(repo).await?.join("rebase");
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir)
            .map_err(|e| Error::io(format!("Couldn’t create {}", dir.display()), e))?;
        let mut todo = String::new();
        for (n, (id, step)) in steps.iter().enumerate() {
            let verb = match step.action {
                PlanAction::Keep | PlanAction::Reword => "pick",
                PlanAction::Squash => "squash",
                PlanAction::Fixup => "fixup",
                PlanAction::Drop => "drop",
            };
            todo.push_str(&format!("{verb} {id}\n"));
            let message = step
                .message
                .as_deref()
                .map(str::trim)
                .filter(|m| !m.is_empty());
            if let (Some(message), PlanAction::Reword | PlanAction::Squash) = (message, step.action)
            {
                let file = dir.join(format!("{n}.msg"));
                std::fs::write(&file, format!("{message}\n"))
                    .map_err(|e| Error::io(format!("Couldn’t write {}", file.display()), e))?;
                todo.push_str(&format!(
                    "exec git commit --amend --quiet --no-verify -F {}\n",
                    sh_quote(&file)
                ));
            }
        }
        let todo_file = dir.join("todo");
        std::fs::write(&todo_file, todo)
            .map_err(|e| Error::io(format!("Couldn’t write {}", todo_file.display()), e))?;

        let total = known.len() as u64;
        let left = steps
            .iter()
            .filter(|(_, s)| matches!(s.action, PlanAction::Keep | PlanAction::Reword))
            .count() as u64;
        let summary = format!(
            "Tidied up {} into {left}",
            count(total, "commit", "commits")
        );
        let refs: Vec<String> = self.current_ref(repo).await?.into_iter().collect();
        let pending = self.journal_begin(repo, "tidy", &summary, &refs).await?;
        let args: Vec<String> = [
            "rebase",
            "--interactive",
            "--no-autosquash",
            "--empty=drop",
            &current.base,
        ]
        .iter()
        .map(|a| a.to_string())
        .collect();
        let call = Call::default().no_editor().env(
            "GIT_SEQUENCE_EDITOR",
            format!("cp {}", sh_quote(&todo_file)),
        );
        let marker = Marker {
            cause: "tidy".into(),
            theirs: current.base_name.clone(),
            ..Marker::default()
        };
        self.run_op(repo, pending, &args, call, marker, &summary, "Tidying up")
            .await
    }

    async fn single_parent(&self, repo: &Path, rev: &str) -> Result<(String, String)> {
        let sha = self.resolve_commit(repo, rev).await?;
        let line = self
            .line(repo, &["rev-list", "--parents", "-n", "1", &sha])
            .await?;
        if line.split_whitespace().count() > 2 {
            return Err(Error::Invalid(format!(
                "{} is a merge, which can’t be picked or reverted here.",
                short(&sha)
            )));
        }
        let subject = self.line(repo, &["log", "-1", "--format=%s", &sha]).await?;
        Ok((sha, subject))
    }

    /// Copies one commit onto the current branch.
    pub async fn cherry_pick(&self, repo: &Path, rev: &str) -> Result<OpOutcome> {
        let (sha, _) = self.single_parent(repo, rev).await?;
        let here = Self::branch_label(&self.current_branch(repo).await?);
        let summary = format!("Cherry-picked {} onto {here}", short(&sha));
        let refs: Vec<String> = self.current_ref(repo).await?.into_iter().collect();
        let pending = self
            .journal_begin(repo, "cherry_pick", &summary, &refs)
            .await?;
        let args: Vec<String> = vec!["cherry-pick".into(), sha.clone()];
        let marker = Marker {
            cause: "cherry_pick".into(),
            theirs: Some(short(&sha)),
            ..Marker::default()
        };
        self.run_op(
            repo,
            pending,
            &args,
            Call::default().no_editor(),
            marker,
            &summary,
            "Cherry-picking",
        )
        .await
    }

    /// Adds a commit that undoes `rev`.
    pub async fn revert(&self, repo: &Path, rev: &str) -> Result<OpOutcome> {
        let (sha, subject) = self.single_parent(repo, rev).await?;
        let summary = format!("Reverted “{subject}”");
        let refs: Vec<String> = self.current_ref(repo).await?.into_iter().collect();
        let pending = self.journal_begin(repo, "revert", &summary, &refs).await?;
        let args: Vec<String> = vec!["revert".into(), "--no-edit".into(), sha.clone()];
        let marker = Marker {
            cause: "revert".into(),
            theirs: Some(short(&sha)),
            ..Marker::default()
        };
        self.run_op(
            repo,
            pending,
            &args,
            Call::default().no_editor(),
            marker,
            &summary,
            "Reverting",
        )
        .await
    }

    /// What stepping the current branch back to `rev` would do.
    pub async fn reset_preview(&self, repo: &Path, rev: &str) -> Result<ResetPreview> {
        let target = self.resolve_commit(repo, rev).await?;
        let range = format!("{target}..HEAD");
        let commits = self.count_commits(repo, &[&range]).await?;
        let local = self
            .count_commits(repo, &[&range, "--not", "--remotes"])
            .await?;
        Ok(ResetPreview {
            branch: self.current_branch(repo).await?,
            target_short: short(&target),
            target,
            commits,
            pushed: commits.saturating_sub(local),
            uncommitted: self
                .text(repo, &["status", "--porcelain"])
                .await?
                .lines()
                .count(),
        })
    }

    /// Steps the current branch back (or over) to `rev`.
    pub async fn reset(&self, repo: &Path, rev: &str, mode: ResetMode) -> Result<OpOutcome> {
        check_rev(rev)?;
        let preview = self.reset_preview(repo, rev).await?;
        if self.operation_in_progress(repo).await? {
            return Err(Error::Invalid(
                "Something is still under way here. Finish it or stop it first.".into(),
            ));
        }
        let what = if preview.commits > 0 {
            format!("Reset {}", count(preview.commits, "commit", "commits"))
        } else {
            format!(
                "Moved {} to {}",
                Self::branch_label(&preview.branch),
                preview.target_short
            )
        };
        let summary = match mode {
            ResetMode::Soft => format!("{what} (kept changes staged)"),
            ResetMode::Mixed => format!("{what} (kept changes)"),
            ResetMode::Hard => format!("{what} (threw the changes away)"),
        };
        if mode == ResetMode::Hard && self.is_dirty(repo).await? {
            // The uncommitted part of what goes is kept on its own too.
            let before = self.snapshot_tree(repo, None).await?;
            let base = self.line(repo, &["rev-parse", "HEAD^{tree}"]).await?;
            self.discard_record(
                repo,
                "Discarded uncommitted changes (reset)",
                &before,
                &base,
                true,
            )
            .await?;
        }
        let refs: Vec<String> = self.current_ref(repo).await?.into_iter().collect();
        let pending = self.journal_begin(repo, "reset", &summary, &refs).await?;
        let flag = match mode {
            ResetMode::Soft => "--soft",
            ResetMode::Mixed => "--mixed",
            ResetMode::Hard => "--hard",
        };
        let out = self
            .exec(
                repo,
                &["reset", "--quiet", flag, &preview.target],
                Call::default(),
            )
            .await?;
        if !out.ok() {
            self.journal_cancel(repo, pending).await;
            return Err(out.error());
        }
        let journal = self
            .journal_finish(repo, pending, Some(summary.clone()))
            .await?;
        self.outcome(repo, true, summary, Some(journal)).await
    }

    /// Carries on with what is under way, once its conflicts are resolved.
    pub async fn operation_continue(&self, repo: &Path) -> Result<OpOutcome> {
        let state = self.operation_state(repo).await?;
        if state.kind == OperationKind::None {
            return Err(Error::Invalid("Nothing is under way here.".into()));
        }
        if state.conflicts > 0 {
            return Err(Error::Invalid(format!(
                "{} still to resolve first.",
                count(
                    u64::from(state.conflicts),
                    "file has a conflict",
                    "files have conflicts"
                )
            )));
        }
        let marker = self.marker_read(repo).await.unwrap_or_default();
        let args: Vec<&str> = match state.kind {
            OperationKind::Merge => vec!["commit", "--quiet", "--no-edit"],
            OperationKind::Rebase => vec!["rebase", "--continue"],
            OperationKind::CherryPick => vec!["cherry-pick", "--continue"],
            OperationKind::Revert => vec!["revert", "--continue"],
            OperationKind::StashApply => {
                // The resolved files are left as plain changes, not staged.
                self.exec(repo, &["reset", "--quiet"], Call::default())
                    .await?
                    .into_result()?;
                if let (true, Some(stash)) = (marker.pop, marker.stash.as_ref()) {
                    if let Some(at) = self.stash_position(repo, &stash.sha).await? {
                        let name = format!("stash@{{{at}}}");
                        let _ = self
                            .exec(repo, &["stash", "drop", "--quiet", &name], Call::default())
                            .await;
                    }
                }
                self.marker_clear(repo).await;
                let journal = self.journal_finish_open(repo).await?;
                let summary = match (&marker.stash, marker.cause.as_str()) {
                    (_, "switch") => "Switched, with your changes".to_string(),
                    (Some(stash), _) => format!("Applied put-aside “{}”", stash.message),
                    _ => "Applied the put-aside changes".to_string(),
                };
                return self.outcome(repo, true, summary, journal).await;
            }
            OperationKind::None => unreachable!("handled above"),
        };
        let out = self.exec(repo, &args, Call::default().no_editor()).await?;
        self.after_step(repo, state, out).await
    }

    /// Leaves out the commit a rebase, cherry-pick or revert stopped at.
    pub async fn operation_skip(&self, repo: &Path) -> Result<OpOutcome> {
        let state = self.operation_state(repo).await?;
        let args = match state.kind {
            OperationKind::Rebase => ["rebase", "--skip"],
            OperationKind::CherryPick => ["cherry-pick", "--skip"],
            OperationKind::Revert => ["revert", "--skip"],
            _ => {
                return Err(Error::Invalid(
                    "There is no step to skip here. Finish it, or stop and go back.".into(),
                ))
            }
        };
        let out = self.exec(repo, &args, Call::default().no_editor()).await?;
        self.after_step(repo, state, out).await
    }

    async fn after_step(
        &self,
        repo: &Path,
        was: OperationState,
        out: super::util::Output,
    ) -> Result<OpOutcome> {
        let now = self.operation_state(repo).await?;
        if now.kind == OperationKind::None {
            let entry = self.journal_open(repo).await?;
            self.marker_clear(repo).await;
            let journal = self.journal_finish_open(repo).await?;
            let summary = entry.map(|e| e.title).unwrap_or_else(|| match was.kind {
                OperationKind::Merge => "Merged".to_string(),
                OperationKind::Rebase => "Rebased".to_string(),
                OperationKind::CherryPick => "Cherry-picked".to_string(),
                _ => "Done".to_string(),
            });
            return self.outcome(repo, true, summary, journal).await;
        }
        if now.conflicts > 0 || out.ok() {
            // The next stop: fresh notes for its conflicts.
            self.marker_current(repo, now.kind).await?;
            return self.outcome(repo, false, now.title.clone(), None).await;
        }
        let said = out.said();
        if said.contains("nothing to commit") || said.contains("is now empty") {
            return Err(Error::Invalid(
                "This step has nothing left in it. Skip it instead.".into(),
            ));
        }
        Err(out.error())
    }

    /// Stops what is under way and puts everything back as it was before
    /// it started.
    pub async fn operation_abort(&self, repo: &Path) -> Result<OpOutcome> {
        let state = self.operation_state(repo).await?;
        let args = match state.kind {
            OperationKind::None => return Err(Error::Invalid("Nothing is under way here.".into())),
            OperationKind::Merge => ["merge", "--abort"],
            OperationKind::Rebase => ["rebase", "--abort"],
            OperationKind::CherryPick => ["cherry-pick", "--abort"],
            OperationKind::Revert => ["revert", "--abort"],
            OperationKind::StashApply => {
                if !self.restore_open(repo).await? {
                    return Err(Error::Invalid(
                        "This wasn’t started in Atelier, so it can’t be put back automatically. Resolve the files, or use your git tool.".into(),
                    ));
                }
                self.marker_clear(repo).await;
                return self
                    .outcome(repo, true, "Stopped. Everything is as it was.".into(), None)
                    .await;
            }
        };
        self.exec(repo, &args, Call::default())
            .await?
            .into_result()?;
        self.marker_clear(repo).await;
        self.journal_cancel_open(repo).await?;
        self.outcome(repo, true, "Stopped. Everything is as it was.".into(), None)
            .await
    }
}

/// The steps of a rebase from git's `done` and `git-rebase-todo` files.
fn plan_items(done: &str, todo: &str) -> Vec<PlanItem> {
    let mut items: Vec<PlanItem> = Vec::new();
    let done_lines = done.lines().count();
    let last_done = done
        .lines()
        .enumerate()
        .filter(|(_, l)| !l.trim().is_empty() && !l.starts_with('#'))
        .map(|(n, _)| n)
        .last();
    for (n, line) in done.lines().chain(todo.lines()).enumerate() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let mut parts = line.splitn(3, ' ');
        let (verb, id, subject) = (
            parts.next().unwrap_or_default(),
            parts.next().unwrap_or_default(),
            parts.next().unwrap_or_default(),
        );
        let action = match verb {
            "pick" | "p" => "keep",
            "reword" | "r" => "reword",
            "squash" | "s" => "squash",
            "fixup" | "f" => "fixup",
            "drop" | "d" => "drop",
            "edit" | "e" => "keep",
            "exec" | "x" => {
                // The app's own way of giving a commit a new message.
                if line.contains("commit --amend") {
                    if let Some(last) = items.last_mut() {
                        if last.action == "keep" {
                            last.action = "reword".to_string();
                        }
                    }
                }
                continue;
            }
            _ => continue,
        };
        items.push(PlanItem {
            id: id.to_string(),
            short: short(id),
            action: action.to_string(),
            subject: subject.trim_start_matches("# ").to_string(),
            done: n < done_lines,
            current: Some(n) == last_done,
        });
    }
    // The stop is at a commit, even when the last line done was an `exec`.
    if !items.iter().any(|i| i.current) {
        if let Some(last) = items.iter_mut().rev().find(|i| i.done) {
            last.current = true;
        }
    }
    items
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_rebase_in_progress_reads_as_steps() {
        let done = "drop d41c7f6 debug: log every packet\npick 3d90a77 Supervisor: reconnect\nsquash 3c5d8a0 Supervisor: cap the backoff\n";
        let todo = "pick b71c2e9 Tray: show who's connected\npick e04a9d1 Add remote access switch\nexec git commit --amend --quiet -F '/x/4.msg'\n\n# Rebase 7f82d31..e04a9d1\n";
        let items = plan_items(done, todo);
        let flat: Vec<(&str, &str, bool, bool)> = items
            .iter()
            .map(|i| (i.short.as_str(), i.action.as_str(), i.done, i.current))
            .collect();
        assert_eq!(
            flat,
            [
                ("d41c7f6", "drop", true, false),
                ("3d90a77", "keep", true, false),
                ("3c5d8a0", "squash", true, true),
                ("b71c2e9", "keep", false, false),
                ("e04a9d1", "reword", false, false),
            ]
        );
        assert_eq!(items[3].subject, "Tray: show who's connected");
    }

    #[test]
    fn paths_are_quoted_for_the_shell() {
        assert_eq!(sh_quote(Path::new("/a b/it's")), r"'/a b/it'\''s'");
    }
}
