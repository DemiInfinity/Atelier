//! The commands the windows may call. Each is a thin wrapper around
//! [`Desk`]; the windows never see the device token and never talk to the
//! studio themselves.

use std::sync::Arc;

use tauri::{AppHandle, State};

use crate::add::{Added, FolderPlan, Folders, OpenedFolder, SearchView, Suggestion};
use crate::desk::{AppState, ConnectPoll, ConnectStarted, Desk};
use crate::device::{DeviceView, LogView, RunnerProbe};
use crate::error::{CmdError, CmdResult};
use crate::issues::{IssueView, IssuesView};
use crate::manage::{Renamed, RunsView};
use crate::notify::Target;
use crate::pulls::{CheckedOut, MergeResult, PullRow, PullView};
use crate::repo::{
    ChangesView, CommitResult, CommitView, DiffView, HistoryView, PushResult, RepoView,
};
use crate::shell;
use crate::studio::{MessagesView, PasskeyLink, RemoteView};
use crate::today::Overview;

type D<'a> = State<'a, Arc<Desk>>;

// ------------------------------------------------------------ app, sign-in

#[tauri::command]
pub async fn app_state(desk: D<'_>, fresh: Option<bool>) -> CmdResult<AppState> {
    Ok(if fresh == Some(true) {
        desk.app_state_fresh().await
    } else {
        desk.app_state().await
    })
}

#[tauri::command]
pub async fn connect_start(desk: D<'_>, address: String) -> CmdResult<ConnectStarted> {
    desk.connect_start(&address).await
}

#[tauri::command]
pub async fn connect_poll(app: AppHandle, desk: D<'_>) -> CmdResult<ConnectPoll> {
    let poll = desk.connect_poll().await?;
    if matches!(poll, ConnectPoll::Approved(_)) {
        shell::start_live(&app);
    }
    Ok(poll)
}

#[tauri::command]
pub async fn connect_reopen(desk: D<'_>) -> CmdResult<()> {
    desk.connect_reopen().await
}

#[tauri::command]
pub async fn connect_cancel(desk: D<'_>) -> CmdResult<()> {
    desk.connect_cancel().await;
    Ok(())
}

#[tauri::command]
pub async fn disconnect(app: AppHandle, desk: D<'_>) -> CmdResult<()> {
    shell::stop_live(&app);
    desk.disconnect()?;
    shell::set_badge(&app, false);
    Ok(())
}

#[tauri::command]
pub async fn find_folders(desk: D<'_>) -> CmdResult<Folders> {
    desk.find_folders().await
}

#[tauri::command]
pub async fn add_watch_folder(app: AppHandle, desk: D<'_>) -> CmdResult<Option<String>> {
    match shell::pick_folder(&app, &desk, "Choose a folder to look through").await {
        Some(folder) => desk.add_watch_folder(&folder).map(Some),
        None => Ok(None),
    }
}

#[tauri::command]
pub async fn remove_watch_folder(desk: D<'_>, folder: String) -> CmdResult<()> {
    desk.remove_watch_folder(&folder)
}

/// Asks for a new clone folder with the chooser.
#[tauri::command]
pub async fn choose_clone_root(app: AppHandle, desk: D<'_>) -> CmdResult<Option<String>> {
    match shell::pick_folder(&app, &desk, "Choose where clones go").await {
        Some(folder) => desk.set_clone_root(&folder).map(Some),
        None => Ok(None),
    }
}

#[tauri::command]
pub async fn suggest_folders(desk: D<'_>) -> CmdResult<Vec<Suggestion>> {
    Ok(desk.suggest_folders().await)
}

#[tauri::command]
pub async fn add_suggested_folders(desk: D<'_>, paths: Vec<String>) -> CmdResult<Vec<String>> {
    desk.add_suggested(&paths).await
}

#[tauri::command]
pub async fn folders_done(desk: D<'_>) -> CmdResult<()> {
    desk.mark_folders_done()
}

/// "Open a folder on this Mac…": asks with the chooser.
#[tauri::command]
pub async fn open_folder(app: AppHandle, desk: D<'_>) -> CmdResult<Option<OpenedFolder>> {
    match shell::pick_folder(&app, &desk, "Choose a folder to open").await {
        Some(folder) => desk.open_folder(&folder).await.map(Some),
        None => Ok(None),
    }
}

#[tauri::command]
pub async fn runner_probe(desk: D<'_>) -> CmdResult<RunnerProbe> {
    Ok(desk.runner_probe().await)
}

// --------------------------------------------------------------- overview

#[tauri::command]
pub async fn overview(app: AppHandle, desk: D<'_>) -> CmdResult<Overview> {
    let overview = desk.overview().await?;
    shell::set_badge(&app, !overview.needs.is_empty());
    Ok(overview)
}

#[tauri::command]
pub async fn run_log(desk: D<'_>, repo: String, run_id: u64) -> CmdResult<LogView> {
    desk.run_log(&repo, run_id).await
}

// ------------------------------------------------------------- repository

#[tauri::command]
pub async fn repo_view(desk: D<'_>, path: String) -> CmdResult<RepoView> {
    desk.repo_view(&path).await
}

#[tauri::command]
pub async fn repo_changes(desk: D<'_>, path: String) -> CmdResult<ChangesView> {
    desk.repo_changes(&path).await
}

#[tauri::command]
pub async fn repo_diff(desk: D<'_>, path: String, file: String) -> CmdResult<DiffView> {
    desk.repo_diff(&path, &file).await
}

#[tauri::command]
pub async fn repo_commit(
    desk: D<'_>,
    path: String,
    files: Vec<String>,
    message: String,
    description: String,
    push: bool,
) -> CmdResult<CommitResult> {
    desk.repo_commit(&path, &files, &message, &description, push)
        .await
}

#[tauri::command]
pub async fn repo_discard(desk: D<'_>, path: String, files: Vec<String>) -> CmdResult<()> {
    desk.repo_discard(&path, &files).await
}

#[tauri::command]
pub async fn repo_undo_commit(desk: D<'_>, path: String) -> CmdResult<String> {
    desk.repo_undo_commit(&path).await
}

#[tauri::command]
pub async fn repo_history(desk: D<'_>, path: String) -> CmdResult<HistoryView> {
    desk.repo_history(&path).await
}

#[tauri::command]
pub async fn repo_commit_detail(desk: D<'_>, path: String, sha: String) -> CmdResult<CommitView> {
    desk.repo_commit_detail(&path, &sha).await
}

#[tauri::command]
pub async fn repo_branches(desk: D<'_>, path: String) -> CmdResult<Vec<atelier_core::git::Branch>> {
    desk.repo_branches(&path).await
}

#[tauri::command]
pub async fn repo_switch(desk: D<'_>, path: String, branch: String) -> CmdResult<()> {
    desk.repo_switch(&path, &branch).await
}

#[tauri::command]
pub async fn repo_new_branch(
    desk: D<'_>,
    path: String,
    name: String,
    from: Option<String>,
) -> CmdResult<()> {
    desk.repo_new_branch(&path, &name, from.as_deref()).await
}

#[tauri::command]
pub async fn repo_fetch(desk: D<'_>, path: String) -> CmdResult<()> {
    desk.repo_fetch(&path).await
}

#[tauri::command]
pub async fn repo_pull(desk: D<'_>, path: String) -> CmdResult<u64> {
    desk.repo_pull(&path).await
}

#[tauri::command]
pub async fn repo_push(desk: D<'_>, path: String) -> CmdResult<PushResult> {
    desk.repo_push(&path).await
}

#[tauri::command]
pub async fn repo_open(desk: D<'_>, path: String, preset: Option<String>) -> CmdResult<String> {
    desk.repo_open(&path, preset.as_deref()).await
}

#[tauri::command]
pub async fn repo_hand_over(desk: D<'_>, path: String) -> CmdResult<String> {
    desk.repo_hand_over(&path).await
}

// ------------------------------------------------------------------ links

#[tauri::command]
pub async fn open_web(desk: D<'_>, web_path: String) -> CmdResult<()> {
    desk.open_web(&web_path)
}

#[tauri::command]
pub async fn open_studio(desk: D<'_>) -> CmdResult<()> {
    desk.open_web("/")
}

#[tauri::command]
pub async fn open_github(desk: D<'_>, full_name: String) -> CmdResult<()> {
    desk.open_github(&full_name)
}

// ---------------------------------------------------------- pull requests

#[tauri::command]
pub async fn pulls_list(desk: D<'_>, repo: Option<String>) -> CmdResult<Vec<PullRow>> {
    desk.pulls_list(repo.as_deref()).await
}

#[tauri::command]
pub async fn pull_detail(desk: D<'_>, repo: String, number: u64) -> CmdResult<PullView> {
    desk.pull_detail(&repo, number).await
}

#[tauri::command]
pub async fn pull_merge(
    desk: D<'_>,
    repo: String,
    number: u64,
    method: String,
    when_green: bool,
) -> CmdResult<MergeResult> {
    desk.pull_merge(&repo, number, &method, when_green).await
}

#[tauri::command]
pub async fn pull_cancel_merge(desk: D<'_>, repo: String, number: u64) -> CmdResult<()> {
    desk.pull_cancel_merge(&repo, number).await
}

#[tauri::command]
pub async fn pull_checkout(desk: D<'_>, repo: String, number: u64) -> CmdResult<CheckedOut> {
    desk.pull_checkout(&repo, number).await
}

#[tauri::command]
pub async fn agent_approve(desk: D<'_>, job_id: u64) -> CmdResult<bool> {
    desk.agent_approve(job_id).await
}

// ------------------------------------------------------------ this device

#[tauri::command]
pub async fn device_view(desk: D<'_>) -> CmdResult<DeviceView> {
    desk.device_view().await
}

#[tauri::command]
pub async fn runner_set_taking(desk: D<'_>, on: bool) -> CmdResult<bool> {
    desk.runner_set_taking(on).await
}

#[tauri::command]
pub async fn runner_set_rule(desk: D<'_>, rule: String, on: bool) -> CmdResult<()> {
    desk.runner_set_rule(&rule, on).await
}

#[tauri::command]
pub async fn runner_tail(desk: D<'_>, job_id: u64) -> CmdResult<Vec<String>> {
    desk.runner_tail(job_id).await
}

#[tauri::command]
pub async fn job_stop(desk: D<'_>, job_id: u64) -> CmdResult<()> {
    desk.job_stop(job_id).await
}

// ------------------------------------------------------- add a repository

#[tauri::command]
pub async fn choose_folder(app: AppHandle, desk: D<'_>) -> CmdResult<Option<FolderPlan>> {
    match shell::pick_folder(&app, &desk, "Choose a folder to publish").await {
        Some(folder) => desk.folder_plan(&folder).await.map(Some),
        None => Ok(None),
    }
}

#[tauri::command]
pub async fn folder_plan(desk: D<'_>, path: String) -> CmdResult<FolderPlan> {
    // Only for folders already allowed; picking a new one goes through
    // `choose_folder`.
    let real = crate::paths::validate(&desk.roots(), &path)?;
    desk.folder_plan(&real).await
}

#[tauri::command]
pub async fn folder_init(desk: D<'_>, path: String) -> CmdResult<()> {
    desk.folder_init(&path).await
}

#[tauri::command]
pub async fn publish(
    desk: D<'_>,
    path: String,
    owner: String,
    name: String,
    private: bool,
    replace_origin: bool,
) -> CmdResult<Added> {
    desk.publish(&path, &owner, &name, private, replace_origin)
        .await
}

#[tauri::command]
pub async fn search_repos(desk: D<'_>, q: String) -> CmdResult<SearchView> {
    desk.search(&q).await
}

#[tauri::command]
pub async fn clone_repo(desk: D<'_>, full_name: String) -> CmdResult<Added> {
    desk.clone_studio(&full_name).await
}

#[tauri::command]
pub async fn mirror_and_clone(desk: D<'_>, github: String) -> CmdResult<Added> {
    desk.mirror_and_clone(&github).await
}

#[tauri::command]
pub async fn create_new(
    desk: D<'_>,
    owner: String,
    name: String,
    private: bool,
    readme: bool,
    open_editor: bool,
) -> CmdResult<Added> {
    desk.create_new(&owner, &name, private, readme, open_editor)
        .await
}

// --------------------------------------------------------------- settings

#[tauri::command]
pub async fn remote_view(desk: D<'_>) -> CmdResult<RemoteView> {
    Ok(desk.remote_view().await)
}

#[tauri::command]
pub async fn remote_set(app: AppHandle, desk: D<'_>, on: bool) -> CmdResult<RemoteView> {
    let view = desk.remote_set(on).await?;
    shell::tell_all(&app, "remote", &view);
    Ok(view)
}

#[tauri::command]
pub async fn remote_link(desk: D<'_>) -> CmdResult<PasskeyLink> {
    desk.remote_link().await
}

#[tauri::command]
pub async fn set_editor(desk: D<'_>, id: String) -> CmdResult<()> {
    desk.set_editor(&id)
}

#[tauri::command]
pub async fn set_git_tool(desk: D<'_>, id: String) -> CmdResult<()> {
    desk.set_git_tool(&id)
}

#[tauri::command]
pub async fn set_notify_pref(desk: D<'_>, key: String, on: bool) -> CmdResult<()> {
    desk.set_notify_pref(&key, on)
}

#[tauri::command]
pub async fn set_app_pref(app: AppHandle, desk: D<'_>, key: String, on: bool) -> CmdResult<()> {
    desk.set_app_pref(&key, on)?;
    shell::apply_app_prefs(&app, &desk).map_err(|e| CmdError::new("app", e))
}

// --------------------------------------------------------------- messages

#[tauri::command]
pub async fn messages_view(desk: D<'_>) -> CmdResult<MessagesView> {
    desk.messages_view().await
}

#[tauri::command]
pub async fn messages_set_tone(desk: D<'_>, tone: String) -> CmdResult<MessagesView> {
    desk.messages_set_tone(&tone).await
}

#[tauri::command]
pub async fn messages_add(desk: D<'_>, moment: String, text: String) -> CmdResult<MessagesView> {
    desk.messages_add(&moment, &text).await
}

#[tauri::command]
pub async fn messages_remove(
    desk: D<'_>,
    moment: String,
    number: usize,
) -> CmdResult<MessagesView> {
    desk.messages_remove(&moment, number).await
}

#[tauri::command]
pub async fn messages_set_rule(desk: D<'_>, rule: String, on: bool) -> CmdResult<MessagesView> {
    desk.messages_set_rule(&rule, on).await
}

// ---------------------------------------------------------------- windows

/// Shows the main window, optionally on a particular screen. Called from
/// the menu bar popover.
#[tauri::command]
pub async fn show_main(app: AppHandle, target: Option<Target>) -> CmdResult<()> {
    shell::show_main(&app, target);
    Ok(())
}

#[tauri::command]
pub async fn hide_popover(app: AppHandle) -> CmdResult<()> {
    shell::hide_popover(&app);
    Ok(())
}

#[tauri::command]
pub async fn quit(app: AppHandle) -> CmdResult<()> {
    app.exit(0);
    Ok(())
}

/// A line for the self-test log. Does nothing unless the app was started
/// with `ATELIER_DESKTOP_SELFTEST`.
#[tauri::command]
pub async fn selftest_log(desk: D<'_>, line: String) -> CmdResult<()> {
    if desk.env_flag(shell::ENV_SELFTEST) {
        eprintln!("[selftest] {}", line.replace('\n', " ⏎ "));
    }
    Ok(())
}

/// Where the self-test should point the app, when it is on.
#[tauri::command]
pub async fn selftest_plan(app: AppHandle, desk: D<'_>) -> CmdResult<Option<String>> {
    let plan = desk.settings().env().get(shell::ENV_SELFTEST);
    if plan.is_some() {
        // A window that is covered by others gets its page suspended by the
        // system, which would stall the walk-through.
        shell::keep_in_front(&app);
    }
    Ok(plan)
}

/// Shows the menu bar popover without a click on its icon, for the
/// self-test's pictures. Does nothing unless the self-test is on.
#[tauri::command]
pub async fn selftest_popover(app: AppHandle, desk: D<'_>) -> CmdResult<()> {
    if desk.env_flag(shell::ENV_SELFTEST) {
        shell::show_popover_at(&app, 120.0, 80.0);
    }
    Ok(())
}

// ------------------------------------------- looking after a repository

#[tauri::command]
pub async fn repo_rename(desk: D<'_>, path: String, name: String) -> CmdResult<Renamed> {
    desk.repo_rename(&path, &name).await
}

/// Deletes the repository from the studio only; `typed` is its name, typed
/// by the person. The folder stays.
#[tauri::command]
pub async fn repo_delete(desk: D<'_>, path: String, typed: String) -> CmdResult<String> {
    desk.repo_delete(&path, &typed).await
}

#[tauri::command]
pub async fn repo_set_pinned(desk: D<'_>, path: String, on: bool) -> CmdResult<()> {
    desk.set_pinned(&path, on).await
}

#[tauri::command]
pub async fn set_group_collapsed(desk: D<'_>, owner: String, collapsed: bool) -> CmdResult<()> {
    desk.set_group_collapsed(&owner, collapsed)
}

#[tauri::command]
pub async fn repo_runs(desk: D<'_>, repo: String) -> CmdResult<RunsView> {
    desk.repo_runs(&repo).await
}

// ------------------------------------------------------------------ issues

#[tauri::command]
pub async fn issues_list(
    desk: D<'_>,
    repo: String,
    state: String,
    page: u32,
) -> CmdResult<IssuesView> {
    desk.issues_list(&repo, &state, page).await
}

#[tauri::command]
pub async fn issue_detail(desk: D<'_>, repo: String, number: u64) -> CmdResult<IssueView> {
    desk.issue_detail(&repo, number).await
}

#[tauri::command]
pub async fn issue_create(
    desk: D<'_>,
    repo: String,
    title: String,
    body: String,
) -> CmdResult<Option<u64>> {
    desk.issue_create(&repo, &title, &body).await
}

#[tauri::command]
pub async fn issue_comment(desk: D<'_>, repo: String, number: u64, body: String) -> CmdResult<()> {
    desk.issue_comment(&repo, number, &body).await
}

#[tauri::command]
pub async fn issue_set_open(desk: D<'_>, repo: String, number: u64, open: bool) -> CmdResult<()> {
    desk.issue_set_open(&repo, number, open).await
}
