//! The Branches tab (area "branches"): branches here and on each remote,
//! tags, remotes, the put-aside list and worktrees (also for the sidebar's
//! worktree rows). Thin wrappers over
//! `atelier_core::git::{branch, stash, worktree}`.
//!
//! Owned by the branches area: add commands here and list them in
//! [`handler`]. Every command of this module is named `branches_…`.

use std::path::{Path, PathBuf};
use std::sync::Arc;

use atelier_core::git::branch::{
    BranchInfo, DeleteCheck, ForcePushPreview, PullMode, Pushed, RemoteEntry, SwitchHow,
    SwitchOutcome, TagInfo,
};
use atelier_core::git::integrate::OpOutcome;
use atelier_core::git::stash::{StashDetail, StashEntry};
use atelier_core::git::worktree::{Worktree, WorktreeSource};
use serde::Serialize;
use tauri::{AppHandle, State};

use crate::desk::Desk;
use crate::error::{CmdError, CmdResult};
use crate::{paths, shell};

type D<'a> = State<'a, Arc<Desk>>;

/// One remote's branches, with which thread it belongs to.
#[derive(Clone, Debug, Serialize)]
pub struct RemoteGroup {
    pub remote: String,
    /// `studio` (amber), `github` (blue) or `other`.
    pub kind: &'static str,
    /// `On your studio`, `On GitHub`, `On upstream`.
    pub label: String,
    pub branches: Vec<BranchInfo>,
}

#[derive(Clone, Debug, Serialize)]
pub struct BranchesView {
    pub current: Option<String>,
    pub head: Option<String>,
    pub local: Vec<BranchInfo>,
    pub remotes: Vec<RemoteGroup>,
    /// Uncommitted files: a switch asks first when there are any.
    pub changes: usize,
    /// RFC 3339, when this copy last fetched.
    pub fetched_at: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct RemoteView {
    #[serde(flatten)]
    pub entry: RemoteEntry,
    pub kind: &'static str,
    /// `Your studio · fetch and push`, `GitHub`, or empty.
    pub what: String,
}

#[derive(Clone, Debug, Serialize)]
pub struct WorktreeView {
    #[serde(flatten)]
    pub tree: Worktree,
    /// The folder with the home folder as `~`.
    pub display_path: String,
}

#[derive(Clone, Debug, Serialize)]
pub struct SuggestedPath {
    pub path: String,
    pub display_path: String,
}

fn rfc3339(at: std::time::SystemTime) -> Option<String> {
    time::OffsetDateTime::from(at)
        .format(&time::format_description::well_known::Rfc3339)
        .ok()
}

impl Desk {
    pub async fn branches_list(&self, path: &str) -> CmdResult<BranchesView> {
        let repo = self.repo_path(path).await?;
        let git = self.git();
        let list = git.branch_list(&repo).await?;
        let kinds = self.remote_kinds(&repo).await.unwrap_or_default();
        let remotes = list
            .remotes
            .into_iter()
            .map(|group| {
                let kind = kinds.get(&group.remote).copied().unwrap_or("other");
                RemoteGroup {
                    label: match kind {
                        "studio" => "On your studio".to_string(),
                        "github" => "On GitHub".to_string(),
                        _ => format!("On {}", group.remote),
                    },
                    kind,
                    remote: group.remote,
                    branches: group.branches,
                }
            })
            .collect();
        Ok(BranchesView {
            current: list.current,
            head: list.head,
            local: list.local,
            remotes,
            changes: git.changes(&repo).await?.len(),
            fetched_at: git.last_fetch(&repo).await?.and_then(rfc3339),
        })
    }

    /// Makes a branch at `from` (HEAD when `None`); it isn't switched to.
    pub async fn branches_create(
        &self,
        path: &str,
        name: &str,
        from: Option<&str>,
    ) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().branch_create(&repo, name.trim(), from).await?)
    }

    pub async fn branches_rename(&self, path: &str, old: &str, new: &str) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().branch_rename(&repo, old, new.trim()).await?)
    }

    /// What deleting a branch would cost (safe, or commits only it has).
    pub async fn branches_delete_check(&self, path: &str, name: &str) -> CmdResult<DeleteCheck> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().branch_delete_check(&repo, name).await?)
    }

    /// Deletes a local branch; `force` when it has commits nothing else has.
    pub async fn branches_delete(&self, path: &str, name: &str, force: bool) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().branch_delete(&repo, name, force).await?)
    }

    /// Switches branch. With uncommitted changes and `ask`, nothing happens
    /// and the answer says a choice is needed: carry, put aside, or cancel.
    pub async fn branches_switch(
        &self,
        path: &str,
        target: &str,
        how: SwitchHow,
    ) -> CmdResult<SwitchOutcome> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().branch_switch(&repo, target, how).await?)
    }

    pub async fn branches_set_upstream(
        &self,
        path: &str,
        branch: &str,
        upstream: Option<&str>,
    ) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self
            .git()
            .branch_set_upstream(&repo, branch, upstream)
            .await?)
    }

    /// Fetches one remote, or all of them.
    pub async fn branches_fetch(
        &self,
        path: &str,
        remote: Option<&str>,
        prune: bool,
    ) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().fetch_remotes(&repo, remote, prune).await?)
    }

    /// Pulls the current branch with a merge, a rebase or fast-forward only.
    /// Conflicts come back as an outcome that isn't done.
    pub async fn branches_pull(&self, path: &str, mode: PullMode) -> CmdResult<OpOutcome> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().pull(&repo, mode).await?)
    }

    /// The remote a branch without an upstream goes to: the studio's,
    /// whatever it is called.
    async fn default_remote(&self, repo: &Path) -> Option<String> {
        let local = self.git().inspect(repo).await.ok()?;
        let _ = self.studio_repo_of(&local).await;
        let repos = self.repos_cached()?;
        local.studio_remote(&repos).map(|(r, _)| r.name.clone())
    }

    pub async fn branches_push(
        &self,
        path: &str,
        branch: Option<&str>,
        remote: Option<&str>,
    ) -> CmdResult<Pushed> {
        let repo = self.repo_path(path).await?;
        let git = self.git();
        match git.push_branch(&repo, branch, remote).await {
            // Several remotes and nothing followed yet: the studio's.
            Err(atelier_core::Error::Invalid(message)) if remote.is_none() => {
                match self.default_remote(&repo).await {
                    Some(studio) if message.starts_with("Choose which remote") => {
                        Ok(git.push_branch(&repo, branch, Some(&studio)).await?)
                    }
                    _ => Err(atelier_core::Error::Invalid(message).into()),
                }
            }
            other => Ok(other?),
        }
    }

    /// What a force push would replace, as of the last fetch.
    pub async fn branches_force_preview(
        &self,
        path: &str,
        branch: Option<&str>,
        remote: Option<&str>,
    ) -> CmdResult<ForcePushPreview> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().force_push_preview(&repo, branch, remote).await?)
    }

    /// Force push with lease: only if the remote still stands at `expected`
    /// (from the preview the person was shown).
    pub async fn branches_force_push(
        &self,
        path: &str,
        branch: Option<&str>,
        remote: Option<&str>,
        expected: &str,
    ) -> CmdResult<Pushed> {
        let repo = self.repo_path(path).await?;
        Ok(self
            .git()
            .push_force_with_lease(&repo, branch, remote, expected)
            .await?)
    }

    // ------------------------------------------------------------------ tags

    pub async fn branches_tags(&self, path: &str) -> CmdResult<Vec<TagInfo>> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().tag_list(&repo).await?)
    }

    /// Tags a commit (HEAD when `at` is `None`); annotated with a message.
    pub async fn branches_tag_create(
        &self,
        path: &str,
        name: &str,
        at: Option<&str>,
        message: Option<&str>,
    ) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self
            .git()
            .tag_create(&repo, name.trim(), at, message)
            .await?)
    }

    pub async fn branches_tag_delete(&self, path: &str, name: &str) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().tag_delete(&repo, name).await?)
    }

    pub async fn branches_tag_push(&self, path: &str, name: &str, remote: &str) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().tag_push(&repo, name, remote).await?)
    }

    // --------------------------------------------------------------- remotes

    pub async fn branches_remotes(&self, path: &str) -> CmdResult<Vec<RemoteView>> {
        let repo = self.repo_path(path).await?;
        let kinds = self.remote_kinds(&repo).await.unwrap_or_default();
        Ok(self
            .git()
            .remote_list(&repo)
            .await?
            .into_iter()
            .map(|entry| {
                let kind = kinds.get(&entry.name).copied().unwrap_or("other");
                RemoteView {
                    what: match kind {
                        "studio" => "Your studio · fetch and push".to_string(),
                        "github" => "GitHub".to_string(),
                        _ => String::new(),
                    },
                    kind,
                    entry,
                }
            })
            .collect())
    }

    pub async fn branches_remote_add(&self, path: &str, name: &str, url: &str) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().remote_add(&repo, name.trim(), url).await?)
    }

    pub async fn branches_remote_rename(&self, path: &str, old: &str, new: &str) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().remote_rename(&repo, old, new.trim()).await?)
    }

    pub async fn branches_remote_remove(&self, path: &str, name: &str) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().remote_remove(&repo, name).await?)
    }

    pub async fn branches_remote_set_url(
        &self,
        path: &str,
        name: &str,
        url: &str,
    ) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().remote_set_url(&repo, name, url).await?)
    }

    // ------------------------------------------------------------- put aside

    pub async fn branches_stashes(&self, path: &str) -> CmdResult<Vec<StashEntry>> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().stash_list(&repo).await?)
    }

    /// Puts changes aside: everything, or only `files`.
    pub async fn branches_stash_save(
        &self,
        path: &str,
        message: &str,
        include_untracked: bool,
        files: &[String],
    ) -> CmdResult<StashEntry> {
        let repo = self.repo_path(path).await?;
        Ok(self
            .git()
            .stash_save(&repo, message, include_untracked, files)
            .await?)
    }

    pub async fn branches_stash_show(&self, path: &str, id: &str) -> CmdResult<StashDetail> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().stash_show(&repo, id).await?)
    }

    pub async fn branches_stash_apply(&self, path: &str, id: &str) -> CmdResult<OpOutcome> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().stash_apply(&repo, id).await?)
    }

    pub async fn branches_stash_pop(&self, path: &str, id: &str) -> CmdResult<OpOutcome> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().stash_pop(&repo, id).await?)
    }

    /// Drops a put-aside entry; the answer is its entry in Recent actions.
    pub async fn branches_stash_drop(&self, path: &str, id: &str) -> CmdResult<u64> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().stash_drop(&repo, id).await?)
    }

    // ------------------------------------------------------------- worktrees

    fn worktree_view(&self, tree: Worktree) -> WorktreeView {
        WorktreeView {
            display_path: self.display_path(Path::new(&tree.path)),
            tree,
        }
    }

    pub async fn branches_worktrees(&self, path: &str) -> CmdResult<Vec<WorktreeView>> {
        let repo = self.repo_path(path).await?;
        Ok(self
            .git()
            .worktree_list(&repo)
            .await?
            .into_iter()
            .map(|tree| self.worktree_view(tree))
            .collect())
    }

    /// Where a worktree for `branch` goes by default.
    pub async fn branches_worktree_suggest(
        &self,
        path: &str,
        branch: &str,
    ) -> CmdResult<SuggestedPath> {
        let repo = self.repo_path(path).await?;
        let suggested = self.git().worktree_suggest_path(&repo, branch).await?;
        Ok(SuggestedPath {
            display_path: self.display_path(&suggested),
            path: suggested.to_string_lossy().into_owned(),
        })
    }

    /// A new worktree may go beside the repository (`<repo>.worktrees/…`)
    /// or inside a folder Atelier looks after; nowhere else.
    async fn worktree_target(&self, repo: &Path, target: &Path) -> CmdResult<PathBuf> {
        let refused = || {
            CmdError::new(
                "outside",
                "A worktree goes beside its repository, or in a folder Atelier looks after.",
            )
            .hint("Use the suggested folder, or choose one with the folder chooser.")
        };
        if !target.is_absolute() || !paths::is_plain(target) {
            return Err(refused());
        }
        let main = self.git().repo_identity(repo).await?.main;
        let name = main.file_name().ok_or_else(refused)?.to_string_lossy();
        let beside = main
            .parent()
            .ok_or_else(refused)?
            .join(format!("{name}.worktrees"));
        if target.starts_with(&beside) && target != beside {
            return Ok(target.to_path_buf());
        }
        // Otherwise its nearest folder that exists must be inside a root.
        let mut existing = target.parent();
        while let Some(dir) = existing {
            if dir.is_dir() {
                break;
            }
            existing = dir.parent();
        }
        let dir = existing.ok_or_else(refused)?;
        paths::validate(&self.roots(), &dir.to_string_lossy()).map_err(|_| refused())?;
        Ok(target.to_path_buf())
    }

    /// Makes a worktree. Without `target` it goes to the suggested folder.
    pub async fn branches_worktree_add(
        &self,
        path: &str,
        target: Option<&str>,
        source: &WorktreeSource,
    ) -> CmdResult<WorktreeView> {
        let repo = self.repo_path(path).await?;
        let git = self.git();
        let target = match target.map(str::trim).filter(|t| !t.is_empty()) {
            Some(target) => self.worktree_target(&repo, Path::new(target)).await?,
            None => {
                let name = match source {
                    WorktreeSource::Branch { name } | WorktreeSource::NewBranch { name, .. } => {
                        name.clone()
                    }
                    WorktreeSource::Detached { rev } => rev.chars().take(7).collect(),
                };
                git.worktree_suggest_path(&repo, &name).await?
            }
        };
        let tree = git.worktree_add(&repo, &target, source).await?;
        Ok(self.worktree_view(tree))
    }

    /// Removes a worktree's folder (its branch stays). Refused when it has
    /// uncommitted changes or is locked, unless `force`.
    pub async fn branches_worktree_remove(
        &self,
        path: &str,
        worktree: &str,
        force: bool,
    ) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self
            .git()
            .worktree_remove(&repo, Path::new(worktree), force)
            .await?)
    }

    pub async fn branches_worktree_lock(
        &self,
        path: &str,
        worktree: &str,
        reason: &str,
    ) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self
            .git()
            .worktree_lock(&repo, Path::new(worktree), reason)
            .await?)
    }

    pub async fn branches_worktree_unlock(&self, path: &str, worktree: &str) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self
            .git()
            .worktree_unlock(&repo, Path::new(worktree))
            .await?)
    }

    /// Forgets worktrees whose folders are gone; answers with their paths.
    pub async fn branches_worktree_prune(&self, path: &str) -> CmdResult<Vec<String>> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().worktree_prune(&repo).await?)
    }
}

#[tauri::command]
pub async fn branches_list(desk: D<'_>, path: String) -> CmdResult<BranchesView> {
    desk.branches_list(&path).await
}

#[tauri::command]
pub async fn branches_create(
    desk: D<'_>,
    path: String,
    name: String,
    from: Option<String>,
) -> CmdResult<()> {
    desk.branches_create(&path, &name, from.as_deref()).await
}

#[tauri::command]
pub async fn branches_rename(desk: D<'_>, path: String, old: String, new: String) -> CmdResult<()> {
    desk.branches_rename(&path, &old, &new).await
}

#[tauri::command]
pub async fn branches_delete_check(
    desk: D<'_>,
    path: String,
    name: String,
) -> CmdResult<DeleteCheck> {
    desk.branches_delete_check(&path, &name).await
}

#[tauri::command]
pub async fn branches_delete(
    desk: D<'_>,
    path: String,
    name: String,
    force: bool,
) -> CmdResult<()> {
    desk.branches_delete(&path, &name, force).await
}

#[tauri::command]
pub async fn branches_switch(
    desk: D<'_>,
    path: String,
    target: String,
    how: SwitchHow,
) -> CmdResult<SwitchOutcome> {
    desk.branches_switch(&path, &target, how).await
}

#[tauri::command]
pub async fn branches_set_upstream(
    desk: D<'_>,
    path: String,
    branch: String,
    upstream: Option<String>,
) -> CmdResult<()> {
    desk.branches_set_upstream(&path, &branch, upstream.as_deref())
        .await
}

#[tauri::command]
pub async fn branches_fetch(
    desk: D<'_>,
    path: String,
    remote: Option<String>,
    prune: bool,
) -> CmdResult<()> {
    desk.branches_fetch(&path, remote.as_deref(), prune).await
}

#[tauri::command]
pub async fn branches_pull(desk: D<'_>, path: String, mode: PullMode) -> CmdResult<OpOutcome> {
    desk.branches_pull(&path, mode).await
}

#[tauri::command]
pub async fn branches_push(
    desk: D<'_>,
    path: String,
    branch: Option<String>,
    remote: Option<String>,
) -> CmdResult<Pushed> {
    desk.branches_push(&path, branch.as_deref(), remote.as_deref())
        .await
}

#[tauri::command]
pub async fn branches_force_preview(
    desk: D<'_>,
    path: String,
    branch: Option<String>,
    remote: Option<String>,
) -> CmdResult<ForcePushPreview> {
    desk.branches_force_preview(&path, branch.as_deref(), remote.as_deref())
        .await
}

#[tauri::command]
pub async fn branches_force_push(
    desk: D<'_>,
    path: String,
    branch: Option<String>,
    remote: Option<String>,
    expected: String,
) -> CmdResult<Pushed> {
    desk.branches_force_push(&path, branch.as_deref(), remote.as_deref(), &expected)
        .await
}

#[tauri::command]
pub async fn branches_tags(desk: D<'_>, path: String) -> CmdResult<Vec<TagInfo>> {
    desk.branches_tags(&path).await
}

#[tauri::command]
pub async fn branches_tag_create(
    desk: D<'_>,
    path: String,
    name: String,
    at: Option<String>,
    message: Option<String>,
) -> CmdResult<()> {
    desk.branches_tag_create(&path, &name, at.as_deref(), message.as_deref())
        .await
}

#[tauri::command]
pub async fn branches_tag_delete(desk: D<'_>, path: String, name: String) -> CmdResult<()> {
    desk.branches_tag_delete(&path, &name).await
}

#[tauri::command]
pub async fn branches_tag_push(
    desk: D<'_>,
    path: String,
    name: String,
    remote: String,
) -> CmdResult<()> {
    desk.branches_tag_push(&path, &name, &remote).await
}

#[tauri::command]
pub async fn branches_remotes(desk: D<'_>, path: String) -> CmdResult<Vec<RemoteView>> {
    desk.branches_remotes(&path).await
}

#[tauri::command]
pub async fn branches_remote_add(
    desk: D<'_>,
    path: String,
    name: String,
    url: String,
) -> CmdResult<()> {
    desk.branches_remote_add(&path, &name, &url).await
}

#[tauri::command]
pub async fn branches_remote_rename(
    desk: D<'_>,
    path: String,
    old: String,
    new: String,
) -> CmdResult<()> {
    desk.branches_remote_rename(&path, &old, &new).await
}

#[tauri::command]
pub async fn branches_remote_remove(desk: D<'_>, path: String, name: String) -> CmdResult<()> {
    desk.branches_remote_remove(&path, &name).await
}

#[tauri::command]
pub async fn branches_remote_set_url(
    desk: D<'_>,
    path: String,
    name: String,
    url: String,
) -> CmdResult<()> {
    desk.branches_remote_set_url(&path, &name, &url).await
}

#[tauri::command]
pub async fn branches_stashes(desk: D<'_>, path: String) -> CmdResult<Vec<StashEntry>> {
    desk.branches_stashes(&path).await
}

#[tauri::command]
pub async fn branches_stash_save(
    desk: D<'_>,
    path: String,
    message: String,
    include_untracked: bool,
    files: Option<Vec<String>>,
) -> CmdResult<StashEntry> {
    desk.branches_stash_save(
        &path,
        &message,
        include_untracked,
        &files.unwrap_or_default(),
    )
    .await
}

#[tauri::command]
pub async fn branches_stash_show(desk: D<'_>, path: String, id: String) -> CmdResult<StashDetail> {
    desk.branches_stash_show(&path, &id).await
}

#[tauri::command]
pub async fn branches_stash_apply(desk: D<'_>, path: String, id: String) -> CmdResult<OpOutcome> {
    desk.branches_stash_apply(&path, &id).await
}

#[tauri::command]
pub async fn branches_stash_pop(desk: D<'_>, path: String, id: String) -> CmdResult<OpOutcome> {
    desk.branches_stash_pop(&path, &id).await
}

#[tauri::command]
pub async fn branches_stash_drop(desk: D<'_>, path: String, id: String) -> CmdResult<u64> {
    desk.branches_stash_drop(&path, &id).await
}

#[tauri::command]
pub async fn branches_worktrees(desk: D<'_>, path: String) -> CmdResult<Vec<WorktreeView>> {
    desk.branches_worktrees(&path).await
}

#[tauri::command]
pub async fn branches_worktree_suggest(
    desk: D<'_>,
    path: String,
    branch: String,
) -> CmdResult<SuggestedPath> {
    desk.branches_worktree_suggest(&path, &branch).await
}

/// Asks for the parent folder of a new worktree with the chooser. The
/// folder becomes one Atelier looks after, so a worktree may go inside it.
#[tauri::command]
pub async fn branches_worktree_pick(app: AppHandle, desk: D<'_>) -> CmdResult<Option<String>> {
    match shell::pick_folder(&app, &desk, "Choose where the worktree goes").await {
        Some(folder) => {
            let real = desk.register_picked(&folder)?;
            Ok(Some(real.to_string_lossy().into_owned()))
        }
        None => Ok(None),
    }
}

#[tauri::command]
pub async fn branches_worktree_add(
    desk: D<'_>,
    path: String,
    target: Option<String>,
    source: WorktreeSource,
) -> CmdResult<WorktreeView> {
    desk.branches_worktree_add(&path, target.as_deref(), &source)
        .await
}

#[tauri::command]
pub async fn branches_worktree_remove(
    desk: D<'_>,
    path: String,
    worktree: String,
    force: bool,
) -> CmdResult<()> {
    desk.branches_worktree_remove(&path, &worktree, force).await
}

#[tauri::command]
pub async fn branches_worktree_lock(
    desk: D<'_>,
    path: String,
    worktree: String,
    reason: Option<String>,
) -> CmdResult<()> {
    desk.branches_worktree_lock(&path, &worktree, reason.as_deref().unwrap_or_default())
        .await
}

#[tauri::command]
pub async fn branches_worktree_unlock(
    desk: D<'_>,
    path: String,
    worktree: String,
) -> CmdResult<()> {
    desk.branches_worktree_unlock(&path, &worktree).await
}

#[tauri::command]
pub async fn branches_worktree_prune(desk: D<'_>, path: String) -> CmdResult<Vec<String>> {
    desk.branches_worktree_prune(&path).await
}

/// Every command of this area.
pub fn handler() -> impl Fn(tauri::ipc::Invoke) -> bool + Send + Sync + 'static {
    tauri::generate_handler![
        branches_list,
        branches_create,
        branches_rename,
        branches_delete_check,
        branches_delete,
        branches_switch,
        branches_set_upstream,
        branches_fetch,
        branches_pull,
        branches_push,
        branches_force_preview,
        branches_force_push,
        branches_tags,
        branches_tag_create,
        branches_tag_delete,
        branches_tag_push,
        branches_remotes,
        branches_remote_add,
        branches_remote_rename,
        branches_remote_remove,
        branches_remote_set_url,
        branches_stashes,
        branches_stash_save,
        branches_stash_show,
        branches_stash_apply,
        branches_stash_pop,
        branches_stash_drop,
        branches_worktrees,
        branches_worktree_suggest,
        branches_worktree_pick,
        branches_worktree_add,
        branches_worktree_remove,
        branches_worktree_lock,
        branches_worktree_unlock,
        branches_worktree_prune,
    ]
}
