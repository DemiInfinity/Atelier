//! Bringing work together and staying safe (area "flow"): what is under
//! way and continue / skip / stop; merge, rebase, tidy up commits,
//! cherry-pick, revert, reset; resolving conflicts; Recent actions, Undo
//! and Recently discarded. Thin wrappers over
//! `atelier_core::git::{integrate, conflict, undo}`.
//!
//! Owned by the flow area: add commands here and list them in [`handler`].
//! Every command of this module is named `flow_…`.

use std::sync::Arc;

use atelier_core::git::conflict::{Choice, ConflictDetail, ConflictList, TakeSide};
use atelier_core::git::integrate::{
    MergeMode, OpOutcome, OperationState, PlanStep, RebasePlan, ResetMode, ResetPreview,
};
use atelier_core::git::undo::{JournalEntry, UndoOptions, UndoOutcome};
use tauri::State;

use crate::desk::Desk;
use crate::error::CmdResult;

type D<'a> = State<'a, Arc<Desk>>;

impl Desk {
    /// What is under way in the folder: nothing, a merge, a rebase (step n
    /// of m, with its plan), a cherry-pick, a revert, or applying put-aside
    /// changes.
    pub async fn flow_state(&self, path: &str) -> CmdResult<OperationState> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().operation_state(&repo).await?)
    }

    pub async fn flow_continue(&self, path: &str) -> CmdResult<OpOutcome> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().operation_continue(&repo).await?)
    }

    pub async fn flow_skip(&self, path: &str) -> CmdResult<OpOutcome> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().operation_skip(&repo).await?)
    }

    /// Stop and go back: everything as it was before it started.
    pub async fn flow_abort(&self, path: &str) -> CmdResult<OpOutcome> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().operation_abort(&repo).await?)
    }

    pub async fn flow_merge(
        &self,
        path: &str,
        branch: &str,
        mode: MergeMode,
    ) -> CmdResult<OpOutcome> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().merge(&repo, branch, mode).await?)
    }

    pub async fn flow_rebase(&self, path: &str, onto: &str) -> CmdResult<OpOutcome> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().rebase_onto(&repo, onto).await?)
    }

    /// The commits a tidy-up from `base` would work on.
    pub async fn flow_rebase_plan(&self, path: &str, base: Option<&str>) -> CmdResult<RebasePlan> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().rebase_plan(&repo, base).await?)
    }

    /// Tidies up the commits after `base` by the plan (oldest first).
    pub async fn flow_tidy(
        &self,
        path: &str,
        base: &str,
        plan: &[PlanStep],
    ) -> CmdResult<OpOutcome> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().rebase_interactive(&repo, base, plan).await?)
    }

    pub async fn flow_cherry_pick(&self, path: &str, id: &str) -> CmdResult<OpOutcome> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().cherry_pick(&repo, id).await?)
    }

    pub async fn flow_revert(&self, path: &str, id: &str) -> CmdResult<OpOutcome> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().revert(&repo, id).await?)
    }

    pub async fn flow_reset_preview(&self, path: &str, id: &str) -> CmdResult<ResetPreview> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().reset_preview(&repo, id).await?)
    }

    pub async fn flow_reset(&self, path: &str, id: &str, mode: ResetMode) -> CmdResult<OpOutcome> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().reset(&repo, id, mode).await?)
    }

    // ------------------------------------------------------------ conflicts

    pub async fn flow_conflicts(&self, path: &str) -> CmdResult<ConflictList> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().conflict_list(&repo).await?)
    }

    pub async fn flow_conflict_file(&self, path: &str, file: &str) -> CmdResult<ConflictDetail> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().conflict_file(&repo, file).await?)
    }

    /// Chooses what one conflict of a file becomes (`None` takes it back).
    pub async fn flow_conflict_choose(
        &self,
        path: &str,
        file: &str,
        region: u32,
        choice: Option<Choice>,
    ) -> CmdResult<ConflictDetail> {
        let repo = self.repo_path(path).await?;
        Ok(self
            .git()
            .conflict_choose(&repo, file, region, choice)
            .await?)
    }

    /// Takes one side's whole file.
    pub async fn flow_conflict_take(
        &self,
        path: &str,
        file: &str,
        side: TakeSide,
    ) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().conflict_take(&repo, file, side).await?)
    }

    /// Marks a file resolved as it is on disk (edited by hand).
    pub async fn flow_conflict_mark(&self, path: &str, file: &str) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().conflict_mark_resolved(&repo, file).await?)
    }

    pub async fn flow_conflict_reopen(&self, path: &str, file: &str) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().conflict_reopen(&repo, file).await?)
    }

    // ----------------------------------------------------------------- undo

    /// Recent actions, newest first.
    pub async fn flow_journal(&self, path: &str) -> CmdResult<Vec<JournalEntry>> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().journal_list(&repo).await?)
    }

    pub async fn flow_undo(&self, path: &str, options: UndoOptions) -> CmdResult<UndoOutcome> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().undo_last(&repo, options).await?)
    }

    /// "Go back to before this".
    pub async fn flow_go_back(
        &self,
        path: &str,
        entry: u64,
        options: UndoOptions,
    ) -> CmdResult<UndoOutcome> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().go_back_to_before(&repo, entry, options).await?)
    }

    /// Recently discarded, newest first.
    pub async fn flow_discarded(&self, path: &str) -> CmdResult<Vec<JournalEntry>> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().discarded_list(&repo).await?)
    }

    /// Brings discarded changes back. `overwrite`: replace the files when
    /// the changes no longer fit on top of what is there now.
    pub async fn flow_restore_discarded(
        &self,
        path: &str,
        entry: u64,
        overwrite: bool,
    ) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self
            .git()
            .restore_discarded(&repo, entry, overwrite)
            .await?)
    }
}

#[tauri::command]
pub async fn flow_state(desk: D<'_>, path: String) -> CmdResult<OperationState> {
    desk.flow_state(&path).await
}

#[tauri::command]
pub async fn flow_continue(desk: D<'_>, path: String) -> CmdResult<OpOutcome> {
    desk.flow_continue(&path).await
}

#[tauri::command]
pub async fn flow_skip(desk: D<'_>, path: String) -> CmdResult<OpOutcome> {
    desk.flow_skip(&path).await
}

#[tauri::command]
pub async fn flow_abort(desk: D<'_>, path: String) -> CmdResult<OpOutcome> {
    desk.flow_abort(&path).await
}

#[tauri::command]
pub async fn flow_merge(
    desk: D<'_>,
    path: String,
    branch: String,
    mode: MergeMode,
) -> CmdResult<OpOutcome> {
    desk.flow_merge(&path, &branch, mode).await
}

#[tauri::command]
pub async fn flow_rebase(desk: D<'_>, path: String, onto: String) -> CmdResult<OpOutcome> {
    desk.flow_rebase(&path, &onto).await
}

#[tauri::command]
pub async fn flow_rebase_plan(
    desk: D<'_>,
    path: String,
    base: Option<String>,
) -> CmdResult<RebasePlan> {
    desk.flow_rebase_plan(&path, base.as_deref()).await
}

#[tauri::command]
pub async fn flow_tidy(
    desk: D<'_>,
    path: String,
    base: String,
    plan: Vec<PlanStep>,
) -> CmdResult<OpOutcome> {
    desk.flow_tidy(&path, &base, &plan).await
}

#[tauri::command]
pub async fn flow_cherry_pick(desk: D<'_>, path: String, id: String) -> CmdResult<OpOutcome> {
    desk.flow_cherry_pick(&path, &id).await
}

#[tauri::command]
pub async fn flow_revert(desk: D<'_>, path: String, id: String) -> CmdResult<OpOutcome> {
    desk.flow_revert(&path, &id).await
}

#[tauri::command]
pub async fn flow_reset_preview(desk: D<'_>, path: String, id: String) -> CmdResult<ResetPreview> {
    desk.flow_reset_preview(&path, &id).await
}

#[tauri::command]
pub async fn flow_reset(
    desk: D<'_>,
    path: String,
    id: String,
    mode: ResetMode,
) -> CmdResult<OpOutcome> {
    desk.flow_reset(&path, &id, mode).await
}

#[tauri::command]
pub async fn flow_conflicts(desk: D<'_>, path: String) -> CmdResult<ConflictList> {
    desk.flow_conflicts(&path).await
}

#[tauri::command]
pub async fn flow_conflict_file(
    desk: D<'_>,
    path: String,
    file: String,
) -> CmdResult<ConflictDetail> {
    desk.flow_conflict_file(&path, &file).await
}

#[tauri::command]
pub async fn flow_conflict_choose(
    desk: D<'_>,
    path: String,
    file: String,
    region: u32,
    choice: Option<Choice>,
) -> CmdResult<ConflictDetail> {
    desk.flow_conflict_choose(&path, &file, region, choice)
        .await
}

#[tauri::command]
pub async fn flow_conflict_take(
    desk: D<'_>,
    path: String,
    file: String,
    side: TakeSide,
) -> CmdResult<()> {
    desk.flow_conflict_take(&path, &file, side).await
}

#[tauri::command]
pub async fn flow_conflict_mark(desk: D<'_>, path: String, file: String) -> CmdResult<()> {
    desk.flow_conflict_mark(&path, &file).await
}

#[tauri::command]
pub async fn flow_conflict_reopen(desk: D<'_>, path: String, file: String) -> CmdResult<()> {
    desk.flow_conflict_reopen(&path, &file).await
}

#[tauri::command]
pub async fn flow_journal(desk: D<'_>, path: String) -> CmdResult<Vec<JournalEntry>> {
    desk.flow_journal(&path).await
}

#[tauri::command]
pub async fn flow_undo(
    desk: D<'_>,
    path: String,
    options: Option<UndoOptions>,
) -> CmdResult<UndoOutcome> {
    desk.flow_undo(&path, options.unwrap_or_default()).await
}

#[tauri::command]
pub async fn flow_go_back(
    desk: D<'_>,
    path: String,
    entry: u64,
    options: Option<UndoOptions>,
) -> CmdResult<UndoOutcome> {
    desk.flow_go_back(&path, entry, options.unwrap_or_default())
        .await
}

#[tauri::command]
pub async fn flow_discarded(desk: D<'_>, path: String) -> CmdResult<Vec<JournalEntry>> {
    desk.flow_discarded(&path).await
}

#[tauri::command]
pub async fn flow_restore_discarded(
    desk: D<'_>,
    path: String,
    entry: u64,
    overwrite: bool,
) -> CmdResult<()> {
    desk.flow_restore_discarded(&path, entry, overwrite).await
}

/// Every command of this area.
pub fn handler() -> impl Fn(tauri::ipc::Invoke) -> bool + Send + Sync + 'static {
    tauri::generate_handler![
        flow_state,
        flow_continue,
        flow_skip,
        flow_abort,
        flow_merge,
        flow_rebase,
        flow_rebase_plan,
        flow_tidy,
        flow_cherry_pick,
        flow_revert,
        flow_reset_preview,
        flow_reset,
        flow_conflicts,
        flow_conflict_file,
        flow_conflict_choose,
        flow_conflict_take,
        flow_conflict_mark,
        flow_conflict_reopen,
        flow_journal,
        flow_undo,
        flow_go_back,
        flow_discarded,
        flow_restore_discarded,
    ]
}
