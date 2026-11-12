//! The Changes tab (area "stage"): what is staged and what isn't, a file's
//! diff as chunks, staging / unstaging / discarding a file, a chunk or
//! lines, and committing what is staged. Thin wrappers over
//! `atelier_core::git::stage`; the folder is checked like every other
//! repository command (`Desk::repo_path`).
//!
//! Owned by the stage area: add commands here and list them in [`handler`].
//! Every command of this module is named `stage_…`: the shell routes by
//! that prefix.

use std::sync::Arc;

use atelier_core::git::stage::{Committed, Selection, Side, StageDiff, StageStatus};
use serde::Serialize;
use tauri::State;

use crate::desk::Desk;
use crate::error::CmdResult;

type D<'a> = State<'a, Arc<Desk>>;

/// The Changes tab's list, with who the commit will be made as.
#[derive(Clone, Debug, Serialize)]
pub struct StageView {
    #[serde(flatten)]
    pub status: StageStatus,
    /// `Demi <demi@example.com>`, when git knows who commits here.
    pub committing_as: Option<String>,
}

impl Desk {
    pub async fn stage_status(&self, path: &str) -> CmdResult<StageView> {
        let repo = self.repo_path(path).await?;
        let git = self.git();
        let status = git.stage_status(&repo).await?;
        let committing_as = match git.identity(&repo).await? {
            (Some(name), Some(email)) => Some(format!("{name} <{email}>")),
            (Some(name), None) => Some(name),
            _ => None,
        };
        Ok(StageView {
            status,
            committing_as,
        })
    }

    pub async fn stage_diff(&self, path: &str, file: &str, side: Side) -> CmdResult<StageDiff> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().stage_diff(&repo, file, side).await?)
    }

    /// Stages a file, some chunks or some lines.
    pub async fn stage_add(
        &self,
        path: &str,
        file: &str,
        selection: &Selection,
        token: Option<&str>,
    ) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().stage(&repo, file, selection, token).await?)
    }

    /// Unstages a file, some chunks or some lines.
    pub async fn stage_remove(
        &self,
        path: &str,
        file: &str,
        selection: &Selection,
        token: Option<&str>,
    ) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().unstage(&repo, file, selection, token).await?)
    }

    pub async fn stage_add_all(&self, path: &str) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().stage_all(&repo).await?)
    }

    pub async fn stage_remove_all(&self, path: &str) -> CmdResult<()> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().unstage_all(&repo).await?)
    }

    /// Discards unstaged changes of a file, some chunks or some lines. What
    /// goes is kept in Recently discarded; the answer is that entry's id.
    pub async fn stage_discard(
        &self,
        path: &str,
        file: &str,
        selection: &Selection,
        token: Option<&str>,
    ) -> CmdResult<Option<u64>> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().discard(&repo, file, selection, token).await?)
    }

    /// Discards the unstaged changes of several files as one entry.
    pub async fn stage_discard_files(
        &self,
        path: &str,
        files: &[String],
    ) -> CmdResult<Option<u64>> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().discard_files(&repo, files).await?)
    }

    /// Discards everything uncommitted.
    pub async fn stage_discard_all(&self, path: &str) -> CmdResult<Option<u64>> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().discard_all(&repo).await?)
    }

    /// Commits what is staged; with `amend`, rewrites the last commit.
    pub async fn stage_commit(
        &self,
        path: &str,
        message: &str,
        description: &str,
        amend: bool,
    ) -> CmdResult<Committed> {
        let repo = self.repo_path(path).await?;
        Ok(self
            .git()
            .commit_staged(&repo, message, description, amend)
            .await?)
    }
}

#[tauri::command]
pub async fn stage_status(desk: D<'_>, path: String) -> CmdResult<StageView> {
    desk.stage_status(&path).await
}

#[tauri::command]
pub async fn stage_diff(
    desk: D<'_>,
    path: String,
    file: String,
    side: Side,
) -> CmdResult<StageDiff> {
    desk.stage_diff(&path, &file, side).await
}

#[tauri::command]
pub async fn stage_add(
    desk: D<'_>,
    path: String,
    file: String,
    selection: Selection,
    token: Option<String>,
) -> CmdResult<()> {
    desk.stage_add(&path, &file, &selection, token.as_deref())
        .await
}

#[tauri::command]
pub async fn stage_remove(
    desk: D<'_>,
    path: String,
    file: String,
    selection: Selection,
    token: Option<String>,
) -> CmdResult<()> {
    desk.stage_remove(&path, &file, &selection, token.as_deref())
        .await
}

#[tauri::command]
pub async fn stage_add_all(desk: D<'_>, path: String) -> CmdResult<()> {
    desk.stage_add_all(&path).await
}

#[tauri::command]
pub async fn stage_remove_all(desk: D<'_>, path: String) -> CmdResult<()> {
    desk.stage_remove_all(&path).await
}

#[tauri::command]
pub async fn stage_discard(
    desk: D<'_>,
    path: String,
    file: String,
    selection: Selection,
    token: Option<String>,
) -> CmdResult<Option<u64>> {
    desk.stage_discard(&path, &file, &selection, token.as_deref())
        .await
}

#[tauri::command]
pub async fn stage_discard_files(
    desk: D<'_>,
    path: String,
    files: Vec<String>,
) -> CmdResult<Option<u64>> {
    desk.stage_discard_files(&path, &files).await
}

#[tauri::command]
pub async fn stage_discard_all(desk: D<'_>, path: String) -> CmdResult<Option<u64>> {
    desk.stage_discard_all(&path).await
}

#[tauri::command]
pub async fn stage_commit(
    desk: D<'_>,
    path: String,
    message: String,
    description: String,
    amend: bool,
) -> CmdResult<Committed> {
    desk.stage_commit(&path, &message, &description, amend)
        .await
}

/// Every command of this area.
pub fn handler() -> impl Fn(tauri::ipc::Invoke) -> bool + Send + Sync + 'static {
    tauri::generate_handler![
        stage_status,
        stage_diff,
        stage_add,
        stage_remove,
        stage_add_all,
        stage_remove_all,
        stage_discard,
        stage_discard_files,
        stage_discard_all,
        stage_commit,
    ]
}
