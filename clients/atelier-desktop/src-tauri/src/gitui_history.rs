//! The History tab (area "history"): the commit graph with search and
//! paging, one commit and its files, one file's history, and blame. Thin
//! wrappers over `atelier_core::git::{history, blame}`.
//!
//! Owned by the history area: add commands here and list them in
//! [`handler`]. Every command of this module is named `history_…`.

use std::sync::Arc;

use atelier_core::diff::FileDiff;
use atelier_core::git::blame::Blame;
use atelier_core::git::history::{CommitInfo, FileContent, FileHistory, GraphQuery, GraphRow};
use serde::Serialize;
use tauri::State;

use crate::desk::Desk;
use crate::error::CmdResult;
use crate::repo::{pills, Pill};

type D<'a> = State<'a, Arc<Desk>>;

/// A row of the graph with its ref pills in the three colour threads.
#[derive(Clone, Debug, Serialize)]
pub struct GraphRowView {
    #[serde(flatten)]
    pub row: GraphRow,
    pub pills: Vec<Pill>,
}

#[derive(Clone, Debug, Serialize)]
pub struct GraphView {
    pub rows: Vec<GraphRowView>,
    pub more: bool,
    pub columns: u32,
    pub filtered: bool,
    pub branch: Option<String>,
    pub head: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct CommitInfoView {
    #[serde(flatten)]
    pub info: CommitInfo,
    pub pills: Vec<Pill>,
}

impl Desk {
    pub async fn history_graph(&self, path: &str, query: &GraphQuery) -> CmdResult<GraphView> {
        let repo = self.repo_path(path).await?;
        let page = self.git().graph(&repo, query).await?;
        let kinds = self.remote_kinds(&repo).await.unwrap_or_default();
        Ok(GraphView {
            rows: page
                .rows
                .into_iter()
                .map(|row| GraphRowView {
                    pills: pills(&row.refs, None, &kinds),
                    row,
                })
                .collect(),
            more: page.more,
            columns: page.columns,
            filtered: page.filtered,
            branch: page.branch,
            head: page.head,
        })
    }

    pub async fn history_commit(&self, path: &str, id: &str) -> CmdResult<CommitInfoView> {
        let repo = self.repo_path(path).await?;
        let info = self.git().commit_info(&repo, id).await?;
        let kinds = self.remote_kinds(&repo).await.unwrap_or_default();
        Ok(CommitInfoView {
            pills: pills(&info.refs, None, &kinds),
            info,
        })
    }

    /// What one commit did to one file.
    pub async fn history_commit_diff(
        &self,
        path: &str,
        id: &str,
        file: &str,
        old_file: Option<&str>,
    ) -> CmdResult<FileDiff> {
        let repo = self.repo_path(path).await?;
        Ok(self
            .git()
            .commit_file_diff(&repo, id, file, old_file)
            .await?)
    }

    /// The commits that changed a file, across renames.
    pub async fn history_file(
        &self,
        path: &str,
        file: &str,
        from: Option<&str>,
        limit: usize,
    ) -> CmdResult<FileHistory> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().file_history(&repo, file, from, limit).await?)
    }

    /// A file as it was at a commit.
    pub async fn history_file_at(
        &self,
        path: &str,
        id: &str,
        file: &str,
    ) -> CmdResult<FileContent> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().file_at(&repo, id, file).await?)
    }

    /// Who last changed each line, at a commit or in the working tree.
    pub async fn history_blame(
        &self,
        path: &str,
        file: &str,
        at: Option<&str>,
    ) -> CmdResult<Blame> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().blame(&repo, file, at).await?)
    }

    /// The blame as it was just before `commit`.
    pub async fn history_blame_before(
        &self,
        path: &str,
        file: &str,
        commit: &str,
    ) -> CmdResult<Blame> {
        let repo = self.repo_path(path).await?;
        Ok(self.git().blame_before(&repo, file, commit).await?)
    }
}

#[tauri::command]
pub async fn history_graph(desk: D<'_>, path: String, query: GraphQuery) -> CmdResult<GraphView> {
    desk.history_graph(&path, &query).await
}

#[tauri::command]
pub async fn history_commit(desk: D<'_>, path: String, id: String) -> CmdResult<CommitInfoView> {
    desk.history_commit(&path, &id).await
}

#[tauri::command]
pub async fn history_commit_diff(
    desk: D<'_>,
    path: String,
    id: String,
    file: String,
    old_file: Option<String>,
) -> CmdResult<FileDiff> {
    desk.history_commit_diff(&path, &id, &file, old_file.as_deref())
        .await
}

#[tauri::command]
pub async fn history_file(
    desk: D<'_>,
    path: String,
    file: String,
    from: Option<String>,
    limit: Option<usize>,
) -> CmdResult<FileHistory> {
    desk.history_file(&path, &file, from.as_deref(), limit.unwrap_or(0))
        .await
}

#[tauri::command]
pub async fn history_file_at(
    desk: D<'_>,
    path: String,
    id: String,
    file: String,
) -> CmdResult<FileContent> {
    desk.history_file_at(&path, &id, &file).await
}

#[tauri::command]
pub async fn history_blame(
    desk: D<'_>,
    path: String,
    file: String,
    at: Option<String>,
) -> CmdResult<Blame> {
    desk.history_blame(&path, &file, at.as_deref()).await
}

#[tauri::command]
pub async fn history_blame_before(
    desk: D<'_>,
    path: String,
    file: String,
    commit: String,
) -> CmdResult<Blame> {
    desk.history_blame_before(&path, &file, &commit).await
}

/// Every command of this area.
pub fn handler() -> impl Fn(tauri::ipc::Invoke) -> bool + Send + Sync + 'static {
    tauri::generate_handler![
        history_graph,
        history_commit,
        history_commit_diff,
        history_file,
        history_file_at,
        history_blame,
        history_blame_before,
    ]
}
