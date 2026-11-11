// The contract with the Rust side: every command the windows may call, and
// the shapes that come back. The windows never see the device token and
// never talk to the studio themselves; everything goes through here.
//
// Field names are the Rust ones (snake_case). Keep this file in step with
// src-tauri/src/commands.rs.

import { invoke } from "@tauri-apps/api/core";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";

// ----------------------------------------------------------------- shared

/** What a failed command says: one plain sentence and, maybe, a next step. */
export interface CmdError {
  kind: string;
  message: string;
  hint: string | null;
}

export function asError(e: unknown): CmdError {
  if (e && typeof e === "object" && "message" in e && typeof (e as CmdError).message === "string") {
    const err = e as Partial<CmdError>;
    return { kind: err.kind ?? "error", message: err.message ?? "", hint: err.hint ?? null };
  }
  return { kind: "error", message: String(e), hint: null };
}

/** The colour threads: amber yours, blue GitHub, violet agents/Connect. */
export type Tone = "yours" | "github" | "agents" | "connect" | "pass" | "fail" | "quiet";

/** Where a click leads. `web` opens a page of the studio in the browser. */
export type Target =
  | { screen: "today" }
  | { screen: "device" }
  | { screen: "pulls" }
  | { screen: "pull"; repo: string; number: number }
  | { screen: "repo"; path: string }
  | { screen: "web"; web_path: string }
  | { screen: "add"; tab: "publish" | "clone" | "new" | "open" }
  | { screen: "settings" };

export interface PresetInfo {
  id: string;
  label: string;
}

export interface Badge {
  text: string;
  tone: Tone;
}

// -------------------------------------------------------------- app state

export interface Account {
  name: string;
  display: string;
  /** `user` or `company`. */
  kind: string;
  commit_name: string;
  commit_email: string;
}

export interface NotifyPrefs {
  fail: boolean;
  agent: boolean;
  security: boolean;
  comment: boolean;
  pass: boolean;
}

export interface Prefs {
  notify: NotifyPrefs;
  autostart: boolean;
  menu_bar: boolean;
  hide_dock: boolean;
  picked: string[];
  /** "Find your folders" has been shown once. */
  folders_done: boolean;
  /** Working copies pinned to the top of the sidebar, by folder. */
  pinned: string[];
  /** Owner groups folded shut in the sidebar, by account name (lower case). */
  collapsed: string[];
  /** Who a new repository belonged to last time. */
  default_owner: string | null;
}

export interface AppState {
  signed_in: boolean;
  offline: boolean;
  note: string | null;
  studio_url: string | null;
  studio_host: string | null;
  studio_name: string | null;
  device_name: string;
  person_name: string | null;
  person_login: string | null;
  accounts: Account[];
  /** The owner lets this device rename and delete repositories (Settings → Devices in the studio). */
  can_admin: boolean;
  tether: boolean;
  connect: boolean;
  /** `~/Atelier` */
  clone_root: string;
  watch: string[];
  editor: PresetInfo;
  git_tool: PresetInfo | null;
  editors: PresetInfo[];
  git_tools: PresetInfo[];
  prefs: Prefs;
  os: "macos" | "windows" | "linux";
  /** "Mac", "PC" or "computer": for "On this Mac". */
  machine: string;
  version: string;
}

// ---------------------------------------------------------------- sign-in

export interface ConnectStarted {
  studio: string;
  studio_host: string;
  user_code: string;
  verify_url: string;
  expires_in: number;
  interval: number;
  opened: boolean;
}

export interface Connected {
  person_name: string;
  studio_name: string;
  device_name: string;
  token_place: "keychain" | "file" | "memory";
  ssh: { fingerprint: string; generated: boolean; key_path: string } | null;
  ssh_error: string | null;
}

export type ConnectPoll = { status: "pending"; seconds_left: number } | ({ status: "approved" } & Connected);

export interface FoundFolder {
  path: string;
  display: string;
  name: string;
  branch: string | null;
  /** `owner/name` in the studio when its remote points there. */
  repo: string | null;
  remote_host: string | null;
}

export interface Folders {
  roots: string[];
  folders: FoundFolder[];
  matched: number;
  studio_repos: number;
}

/** A folder in the home folder that has git repositories in it. */
export interface Suggestion {
  path: string;
  /** `~/Projects` */
  display: string;
  repos: number;
  /** Already looked through. */
  watched: boolean;
}

/** What "Open a folder on this Mac…" found. */
export interface OpenedFolder {
  /**
   * `opened`: in the studio, now a working copy (go to it).
   * `elsewhere`: a git folder whose remote isn't the studio (`remote` says which).
   * `unpublished`: a git folder with no remote.
   * `not_repo`: not a git folder.
   */
  kind: "opened" | "elsewhere" | "unpublished" | "not_repo";
  path: string;
  full_name: string | null;
  /** `origin → github.com/someone/thing` */
  remote: string | null;
  /** What publishing it would do; null for `opened`. */
  plan: FolderPlan | null;
}

export interface RunnerProbe {
  installed: boolean;
  binary: string | null;
  config: string | null;
  running: boolean;
  name: string | null;
  taking_jobs: boolean;
  capabilities: string[];
  how_web_path: string;
}

// ------------------------------------------------------------------ today

export type NeedAction =
  | { type: "approve_agent"; job_id: number }
  | { type: "open_log"; run_id: number; repo: string; web_path: string }
  | { type: "open_pull"; repo: string; number: number }
  | { type: "push"; path: string }
  | { type: "open_web"; web_path: string };

export interface NeedRow {
  id: string;
  tone: Tone;
  title: string;
  detail: string;
  label: string;
  action: NeedAction;
}

export interface WorkingCopy {
  path: string;
  name: string;
  /** The owner as shown (`Elyndra Studios LTD`), or where the remote is. */
  owner: string;
  /** The owning account's name in the studio; empty when it isn't there. */
  owner_name: string;
  /** `user` (personal), `company`, or empty. */
  owner_kind: string;
  pinned: boolean;
  full_name: string | null;
  branch: string;
  changes: number;
  ahead: number;
  behind: number;
  /** `3 changes · 1 to push` */
  state: string;
  tone: "yours" | "github" | "quiet";
  action: "open" | "push" | "pull";
  /** `3 · ↑1` for the sidebar. */
  short: string;
}

export interface RunningRow {
  job_id: number;
  title: string;
  engine: string | null;
  detail: string;
  /** 0–1 */
  progress: number | null;
  kind: string;
  started_at: string;
}

export interface FeedRow {
  tone: "yours" | "github" | "connect" | "agents";
  tag: string;
  text: string;
  at: string;
  target: Target | null;
}

export interface RunnerBrief {
  running: boolean;
  taking_jobs: boolean;
  jobs: number;
  /** `Runner on · 1 job running` */
  line: string;
  /** `On · pauses on battery` */
  switch_line: string;
}

export interface Overview {
  needs: NeedRow[];
  copies: WorkingCopy[];
  /**
   * Git folders found here that aren't in the studio. `owner` says where
   * their remote is (`github.com`) or `no remote`; `full_name` is null.
   */
  others: WorkingCopy[];
  running: RunningRow[];
  elsewhere: string[];
  feed: FeedRow[];
  unread: number;
  pulls_open: number;
  /** `in ~/Atelier and ~/Projects` */
  roots_label: string;
  runner: RunnerBrief;
  device_name: string;
  studio_error: string | null;
}

export interface LogView {
  title: string;
  lines: string[];
  truncated: boolean;
  status: string;
  web_path: string | null;
}

// ------------------------------------------------------------- repository

export interface RepoView {
  path: string;
  display_path: string;
  name: string;
  owner: string;
  full_name: string | null;
  branch: string | null;
  upstream: string | null;
  ahead: number;
  behind: number;
  changes: number;
  branches: number;
  open_pulls: number;
  web_path: string | null;
  default_branch: string | null;
  on_github: boolean;
  editor: PresetInfo;
  git_tool: PresetInfo | null;
  /** `Demi · personal identity` */
  committing_as: string | null;
  /** RFC 3339 */
  fetched_at: string | null;
}

export type ChangeKind = "modified" | "added" | "deleted" | "renamed" | "untracked" | "conflicted";

export interface FileChange {
  path: string;
  old_path: string | null;
  kind: ChangeKind;
  staged: boolean;
  additions: number | null;
  deletions: number | null;
  binary: boolean;
}

export interface ChangesView {
  files: FileChange[];
  /** A friendly line for when there is nothing to commit. */
  clean: string | null;
}

export type LineKind = "hunk" | "context" | "add" | "delete" | "no_newline";

export interface DiffLine {
  kind: LineKind;
  old: number | null;
  new: number | null;
  text: string;
}

export interface DiffView {
  path: string;
  old_path: string | null;
  /** `Go · new file` */
  lang: string;
  status: "modified" | "added" | "deleted" | "renamed" | "copied";
  binary: boolean;
  additions: number;
  deletions: number;
  lines: DiffLine[];
  truncated: boolean;
}

export interface Toast {
  id: string;
  text: string;
  sub: string;
  tone: Tone;
  moment: string | null;
  action: { label: string; target: Target } | null;
  target: Target;
}

export interface CommitResult {
  sha: string;
  short: string;
  pushed: boolean;
  push_error: string | null;
  toast: Toast | null;
}

export interface PushResult {
  commits: number;
  branch: string;
  toast: Toast | null;
}

export type Segment = "none" | "full" | "start" | "join";

export interface HistoryRow {
  sha: string;
  short: string;
  subject: string;
  author: string;
  /** RFC 3339 */
  date: string;
  pills: { text: string; tone: "yours" | "github" | "quiet" }[];
  /** 0 = shared with the upstream (amber), 1 = only here (grey). */
  lane: 0 | 1;
  line0: Segment;
  line1: Segment;
  unpushed: boolean;
  incoming: boolean;
}

export interface HistoryView {
  branch: string | null;
  upstream: string | null;
  ahead: number;
  behind: number;
  rows: HistoryRow[];
  more: boolean;
}

export interface CommitView {
  sha: string;
  short: string;
  author: string;
  email: string;
  date: string;
  subject: string;
  body: string;
  files: { path: string; additions: number | null; deletions: number | null }[];
  pushed: boolean;
  is_head: boolean;
  can_undo: boolean;
}

export interface Branch {
  name: string;
  current: boolean;
  upstream: string | null;
  ahead: number;
  behind: number;
  sha: string;
  subject: string;
}

// ---------------------------------------------------------- pull requests

/** A pull request's checks in a few words. */
export interface ChecksBrief {
  state: "none" | "pending" | "failing" | "passing";
  /** `3 of 4 passed`, `1 failing`, `running`, `no checks` */
  text: string;
  tone: Tone;
  /** The same as a sentence, for a tooltip. */
  title: string;
}

export interface PullRow {
  repo: string;
  repo_short: string;
  number: number;
  title: string;
  by: string;
  status: Badge;
  via: Badge | null;
  checks: ChecksBrief;
  head: string;
}

// ------------------------------------------------ looking after a repository

export interface Renamed {
  full_name: string;
  name: string;
  /** The remotes of the working copy that now point at the new address. */
  remotes: string[];
  remote_error: string | null;
}

export interface RepoRunRow {
  run_id: number;
  status: Badge;
  failed: boolean;
  name: string;
  /** `push on feature/tunnel-ui` */
  detail: string;
  duration: string;
  started_at: string;
  web_path: string | null;
}

export interface RunGroup {
  /** The pull request these runs are for; null for the rest. */
  pull: number | null;
  label: string;
  runs: RepoRunRow[];
}

export interface RunsView {
  repo: string;
  groups: RunGroup[];
  web_path: string;
}

// ----------------------------------------------------------------- issues

export interface LabelView {
  name: string;
  /** `#rrggbb`, checked on the Rust side; null when there is none. */
  color: string | null;
}

export interface IssueRow {
  number: number;
  title: string;
  open: boolean;
  by: string;
  /** `via Tether` (blue) when it came from GitHub. */
  via: Badge | null;
  labels: LabelView[];
  comments: number;
  created_at: string;
  updated_at: string;
}

export interface IssuesView {
  rows: IssueRow[];
  has_more: boolean;
  page: number;
}

export interface IssueCommentRow {
  who: string;
  /** `bot · via Tether` */
  tag: string;
  tone: "yours" | "github" | "agents" | "connect";
  at: string;
  body: string;
}

export interface IssueView {
  repo: string;
  number: number;
  title: string;
  open: boolean;
  by: string;
  via: Badge | null;
  labels: LabelView[];
  /** Plain text as written (Markdown source); only ever shown as text. */
  body: string;
  comments: IssueCommentRow[];
  created_at: string;
  web_path: string;
}

export interface CheckRow {
  status: Badge;
  name: string;
  detail: string;
  action_label: string | null;
  action: NeedAction | null;
}

export interface CommentRow {
  who: string;
  /** `bot · via Tether` */
  tag: string;
  tone: "yours" | "github" | "agents" | "connect";
  at: string;
  body: string;
}

export interface PullView {
  repo: string;
  repo_short: string;
  number: number;
  title: string;
  state: "open" | "closed" | "merged";
  head: string;
  base: string;
  by: string;
  commits: number;
  files: number;
  /** `Not ready to merge yet` */
  headline: string;
  reason: string;
  tone: Tone;
  can_merge: boolean;
  on_github: boolean;
  methods: { id: string; label: string }[];
  /** Set while it waits to merge by itself. */
  auto_merge: string | null;
  checks: CheckRow[];
  comments: CommentRow[];
  web_path: string;
  local_path: string | null;
}

export interface MergeResult {
  merged: boolean;
  sha: string;
  scheduled: boolean;
  toast: Toast | null;
}

// ------------------------------------------------------------ this device

export interface LiveJob extends RunningRow {
  tail: string[];
}

export interface WaitingRow {
  job_id: number;
  title: string;
  detail: string;
  limits: string;
  web_path: string | null;
}

export interface RunRow {
  status: Badge;
  name: string;
  /** `Atelier · feature/tunnel-ui` (the field is spelled `wher`). */
  wher: string;
  duration: string;
  started_at: string;
  web_path: string | null;
}

export interface RuleRow {
  id: "pause_on_battery" | "one_at_a_time" | "container_jobs";
  title: string;
  detail: string;
  on: boolean;
}

export interface DeviceView {
  name: string;
  running: boolean;
  installed: boolean;
  online: boolean;
  taking_jobs: boolean;
  paused: "user" | "battery" | "jobs_off" | "other" | null;
  capabilities: string[];
  jobs: LiveJob[];
  waiting: WaitingRow[];
  runs: RunRow[];
  rules: RuleRow[];
  claude_code: boolean;
  workspaces: { root: string; presets: string[] } | null;
  how_web_path: string;
  studio_error: string | null;
}

// ------------------------------------------------------- add a repository

export interface FolderPlan {
  path: string;
  display: string;
  name: string;
  is_repo: boolean;
  branch: string | null;
  commits: number;
  origin: string | null;
  in_studio: string | null;
  /** `A git folder · 42 commits on main · not in your studio yet` */
  summary: string;
}

export interface Added {
  path: string;
  full_name: string;
  web_path: string;
  already: boolean;
  note: string | null;
}

export interface Hit {
  /** `deminimmo / loop-notes` */
  title: string;
  full_name: string;
  line: string;
  source: "studio" | "github";
  action: "clone" | "mirror" | "open";
  label: string;
  local_path: string | null;
}

export interface SearchView {
  hits: Hit[];
  github_note: string | null;
}

// --------------------------------------------------------------- settings

export interface RemoteSession {
  ip: string;
  country: string;
  signed_in: boolean;
  class: string;
  since: string;
}

export interface RemoteView {
  available: boolean;
  on: boolean;
  address: string | null;
  connected: number;
  exposed: string[];
  line: string;
  switch_line: string;
  sessions: RemoteSession[];
  error: string | null;
}

// --------------------------------------------------------------- messages

export interface MessageLine {
  text: string;
  preview: string;
}

export interface MomentView {
  key: string;
  label: string;
  tone: Tone;
  sub: string;
  ours: MessageLine[];
  yours: MessageLine[];
  plain: string;
}

export interface MessagesView {
  tone: "friendly" | "plain" | "off";
  moments: MomentView[];
  rules: { quiet_fullscreen: boolean; terminal: boolean; milestones: boolean };
}

// --------------------------------------------------------------- commands

const call = <T>(name: string, args?: Record<string, unknown>): Promise<T> => invoke<T>(name, args);

export const api = {
  /** `fresh`: ask the studio again now instead of using what was read in the last minute. */
  appState: (fresh = false) => call<AppState>("app_state", { fresh }),

  // First run
  connectStart: (address: string) => call<ConnectStarted>("connect_start", { address }),
  connectPoll: () => call<ConnectPoll>("connect_poll"),
  connectReopen: () => call<void>("connect_reopen"),
  connectCancel: () => call<void>("connect_cancel"),
  findFolders: () => call<Folders>("find_folders"),
  /** Opens the folder chooser; resolves to the folder as shown (`~/Code`), or null when cancelled. */
  addWatchFolder: () => call<string | null>("add_watch_folder"),
  /** Stops looking through a watch folder (as shown, `~/Projects`). Nothing on disk changes. */
  removeWatchFolder: (folder: string) => call<void>("remove_watch_folder", { folder }),
  /** Opens the chooser for a new clone folder; resolves to it as shown, or null when cancelled. */
  chooseCloneRoot: () => call<string | null>("choose_clone_root"),
  /** Folders in the home folder that contain git repositories (quick, bounded). */
  suggestFolders: () => call<Suggestion[]>("suggest_folders"),
  /** Starts looking through suggested folders (`Suggestion.path`). Resolves to the ones added, as shown. */
  addSuggestedFolders: (paths: string[]) => call<string[]>("add_suggested_folders", { paths }),
  /** Remembers that "Find your folders" has been shown. */
  foldersDone: () => call<void>("folders_done"),
  /** "Open a folder on this Mac…": opens the chooser; null when cancelled. */
  openFolder: () => call<OpenedFolder | null>("open_folder"),
  runnerProbe: () => call<RunnerProbe>("runner_probe"),
  /** Removes the token and the configuration from this machine. Ask first. */
  disconnect: () => call<void>("disconnect"),

  // Today
  overview: () => call<Overview>("overview"),
  runLog: (repo: string, runId: number) => call<LogView>("run_log", { repo, runId }),

  // A repository (path = WorkingCopy.path)
  repoView: (path: string) => call<RepoView>("repo_view", { path }),
  repoChanges: (path: string) => call<ChangesView>("repo_changes", { path }),
  repoDiff: (path: string, file: string) => call<DiffView>("repo_diff", { path, file }),
  /** Commits exactly `files`. With `push`, pushes afterwards. */
  repoCommit: (path: string, files: string[], message: string, description: string, push: boolean) =>
    call<CommitResult>("repo_commit", { path, files, message, description, push }),
  /** Destructive: ask first. */
  repoDiscard: (path: string, files: string[]) => call<void>("repo_discard", { path, files }),
  /** Destructive: ask first. Resolves to the subject of the undone commit. */
  repoUndoCommit: (path: string) => call<string>("repo_undo_commit", { path }),
  repoHistory: (path: string) => call<HistoryView>("repo_history", { path }),
  repoCommitDetail: (path: string, sha: string) => call<CommitView>("repo_commit_detail", { path, sha }),
  repoBranches: (path: string) => call<Branch[]>("repo_branches", { path }),
  repoSwitch: (path: string, branch: string) => call<void>("repo_switch", { path, branch }),
  repoNewBranch: (path: string, name: string, from: string | null) =>
    call<void>("repo_new_branch", { path, name, from }),
  repoFetch: (path: string) => call<void>("repo_fetch", { path }),
  /** Fast-forward only. Resolves to how many commits came in. */
  repoPull: (path: string) => call<number>("repo_pull", { path }),
  repoPush: (path: string) => call<PushResult>("repo_push", { path }),
  /** Opens in an editor preset (the configured one when omitted). Resolves to its label. */
  repoOpen: (path: string, preset?: string) => call<string>("repo_open", { path, preset: preset ?? null }),
  /** Hands over to the configured git tool. Resolves to its label. */
  repoHandOver: (path: string) => call<string>("repo_hand_over", { path }),

  // Links (only the studio and github.com are ever opened)
  openWeb: (webPath: string) => call<void>("open_web", { webPath }),
  openStudio: () => call<void>("open_studio"),
  openGithub: (fullName: string) => call<void>("open_github", { fullName }),

  // Pull requests
  /** Open pull requests: across the studio, or of one repository (`owner/name`). */
  pullsList: (repo?: string) => call<PullRow[]>("pulls_list", { repo: repo ?? null }),
  pullDetail: (repo: string, number: number) => call<PullView>("pull_detail", { repo, number }),
  pullMerge: (repo: string, number: number, method: string, whenGreen: boolean) =>
    call<MergeResult>("pull_merge", { repo, number, method, whenGreen }),
  pullCancelMerge: (repo: string, number: number) => call<void>("pull_cancel_merge", { repo, number }),
  pullCheckout: (repo: string, number: number) =>
    call<{ path: string; branch: string }>("pull_checkout", { repo, number }),
  agentApprove: (jobId: number) => call<boolean>("agent_approve", { jobId }),

  // Looking after a repository (path = WorkingCopy.path)
  /** Renames it in the studio and points the folder's remotes at the new address. */
  repoRename: (path: string, name: string) => call<Renamed>("repo_rename", { path, name }),
  /** Deletes it from the studio only. `typed` is the name the person typed. Ask first. */
  repoDelete: (path: string, typed: string) => call<string>("repo_delete", { path, typed }),
  repoSetPinned: (path: string, on: boolean) => call<void>("repo_set_pinned", { path, on }),
  /** Folds an owner's group in the sidebar shut (or opens it); remembered. */
  setGroupCollapsed: (owner: string, collapsed: boolean) => call<void>("set_group_collapsed", { owner, collapsed }),
  repoRuns: (repo: string) => call<RunsView>("repo_runs", { repo }),

  // Issues (repo = `owner/name`)
  issuesList: (repo: string, state: "open" | "closed", page = 1) => call<IssuesView>("issues_list", { repo, state, page }),
  issueDetail: (repo: string, number: number) => call<IssueView>("issue_detail", { repo, number }),
  /** Resolves to the new issue's number when the studio says it. */
  issueCreate: (repo: string, title: string, body: string) => call<number | null>("issue_create", { repo, title, body }),
  issueComment: (repo: string, number: number, body: string) => call<void>("issue_comment", { repo, number, body }),
  issueSetOpen: (repo: string, number: number, open: boolean) => call<void>("issue_set_open", { repo, number, open }),

  // This device
  deviceView: () => call<DeviceView>("device_view"),
  runnerSetTaking: (on: boolean) => call<boolean>("runner_set_taking", { on }),
  runnerSetRule: (rule: RuleRow["id"], on: boolean) => call<void>("runner_set_rule", { rule, on }),
  runnerTail: (jobId: number) => call<string[]>("runner_tail", { jobId }),
  jobStop: (jobId: number) => call<void>("job_stop", { jobId }),

  // Add a repository
  /** Opens the folder chooser; null when cancelled. */
  chooseFolder: () => call<FolderPlan | null>("choose_folder"),
  folderPlan: (path: string) => call<FolderPlan>("folder_plan", { path }),
  /** `git init` in a picked folder that isn't a repository yet. */
  folderInit: (path: string) => call<void>("folder_init", { path }),
  /** Fails with kind `origin_exists` when origin points elsewhere; ask, then pass replaceOrigin. */
  publish: (path: string, owner: string, name: string, isPrivate: boolean, replaceOrigin: boolean) =>
    call<Added>("publish", { path, owner, name, private: isPrivate, replaceOrigin }),
  searchRepos: (q: string) => call<SearchView>("search_repos", { q }),
  cloneRepo: (fullName: string) => call<Added>("clone_repo", { fullName }),
  mirrorAndClone: (github: string) => call<Added>("mirror_and_clone", { github }),
  createNew: (owner: string, name: string, isPrivate: boolean, readme: boolean, openEditor: boolean) =>
    call<Added>("create_new", { owner, name, private: isPrivate, readme, openEditor }),

  // Settings
  remoteView: () => call<RemoteView>("remote_view"),
  /** Ask before turning it on; off is immediate. */
  remoteSet: (on: boolean) => call<RemoteView>("remote_set", { on }),
  remoteLink: () => call<{ url: string; expires_in: number }>("remote_link"),
  setEditor: (id: string) => call<void>("set_editor", { id }),
  setGitTool: (id: string) => call<void>("set_git_tool", { id }),
  setNotifyPref: (key: keyof NotifyPrefs, on: boolean) => call<void>("set_notify_pref", { key, on }),
  setAppPref: (key: "autostart" | "menu_bar" | "hide_dock", on: boolean) =>
    call<void>("set_app_pref", { key, on }),

  // Messages
  messagesView: () => call<MessagesView>("messages_view"),
  messagesSetTone: (tone: MessagesView["tone"]) => call<MessagesView>("messages_set_tone", { tone }),
  messagesAdd: (moment: string, text: string) => call<MessagesView>("messages_add", { moment, text }),
  /** `number` is 1-based within the moment's own ("yours") messages. */
  messagesRemove: (moment: string, number: number) => call<MessagesView>("messages_remove", { moment, number }),
  messagesSetRule: (rule: keyof MessagesView["rules"], on: boolean) =>
    call<MessagesView>("messages_set_rule", { rule, on }),

  // Windows
  /** From the popover: shows the main window, optionally on a screen. */
  showMain: (target?: Target) => call<void>("show_main", { target: target ?? null }),
  hidePopover: () => call<void>("hide_popover"),
  quit: () => call<void>("quit"),

  selftestPlan: () => call<string | null>("selftest_plan"),
  selftestLog: (line: string) => call<void>("selftest_log", { line }),
  selftestPopover: () => call<void>("selftest_popover"),
};

// ----------------------------------------------------------------- events

/** Events the Rust side sends to the windows. */
export interface Events {
  /** A pop-up, already dressed with friendly words. */
  notify: Toast;
  /** Something in the studio changed (`needs`, `running`, `feed`, `unread`). */
  status: { changed: string[] };
  /** Remote access changed (from the studio, or from a switch in another window). */
  remote: { on: boolean; address?: string | null; connected?: number };
  /** The runner's status changed (null: it isn't running here). */
  runner: unknown;
  /** The live connection to the studio came up or went down. */
  connection: boolean;
  /** The studio refused this device's token. */
  "signed-out": null;
  /** Main window only: go to this screen. */
  navigate: Target;
  /** Popover only: it was just shown. */
  "popover-shown": null;
}

export function on<K extends keyof Events>(name: K, handler: (payload: Events[K]) => void): Promise<UnlistenFn> {
  return listen<Events[K]>(name, (event) => handler(event.payload));
}
