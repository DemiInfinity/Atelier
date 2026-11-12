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
  editors: PresetInfo[];
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
  /** For a linked worktree: the main folder (`path`) of its repository. */
  worktree_of: string | null;
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
  base: string;
  /** Commits of the base it doesn't have yet; 0 when up to date or unknown. */
  behind: number;
  /** It can't be merged with its base as it is. */
  conflicts: boolean;
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

/**
 * Markdown as the tree Rust parses it into (src-tauri/src/markdown.rs).
 * There is no node for raw HTML: markup in the source is text. A link's
 * `url` is always an http, https or mailto address.
 */
export type MdInline =
  | { t: "text"; text: string }
  | { t: "code"; text: string }
  | { t: "em"; inlines: MdInline[] }
  | { t: "strong"; inlines: MdInline[] }
  | { t: "strike"; inlines: MdInline[] }
  | { t: "link"; url: string; image: boolean; inlines: MdInline[] }
  | { t: "ref"; number: number }
  | { t: "mention"; name: string }
  | { t: "break" };

export type MdBlock =
  | { t: "heading"; level: number; inlines: MdInline[] }
  | { t: "paragraph"; inlines: MdInline[] }
  | { t: "code"; lang: string; text: string }
  | { t: "quote"; blocks: MdBlock[] }
  | { t: "list"; ordered: boolean; start: number; items: { task: boolean | null; blocks: MdBlock[] }[] }
  | { t: "table"; aligns: string[]; head: MdInline[][]; rows: MdInline[][][] }
  | { t: "rule" };

export interface IssueCommentRow {
  who: string;
  /** `bot · via Tether` */
  tag: string;
  tone: "yours" | "github" | "agents" | "connect";
  at: string;
  body: string;
  body_md: MdBlock[];
}

export interface IssueView {
  repo: string;
  number: number;
  title: string;
  open: boolean;
  by: string;
  via: Badge | null;
  labels: LabelView[];
  /** As written (Markdown source). */
  body: string;
  /** The same, parsed; drawn by `markdown()`. */
  body_md: MdBlock[];
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
  body_md: MdBlock[];
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
  /** The branch the working copy here is on. */
  local_branch: string | null;
  behind: number;
  /** Conflicts with the base: merging and updating are off until resolved. */
  conflicts: boolean;
  conflict_files: string[];
  /** "Update from <base>" is worth offering. */
  can_update: boolean;
  /** Behind, but at home on GitHub: the repository there (`owner/name`) to open instead. */
  update_on_github: string | null;
}

/** What "Update from <base>" did, in the studio. */
export interface BranchUpdate {
  outcome: "updated" | "up_to_date" | "conflicts";
  head: string;
  base: string;
  head_sha: string;
  /** The studio's sentence about the conflicts. */
  message: string;
  files: string[];
  local_path: string | null;
  /** The working copy here is on the pull request's branch (so it is behind now). */
  local_on_branch: boolean;
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

/**
 * Calls a command. The git client's areas may use this from their own files
 * for a command they added to their own `gitui_*.rs` (and nothing else:
 * commands that exist already have a typed function below).
 */
export const call = <T>(name: string, args?: Record<string, unknown>): Promise<T> => invoke<T>(name, args);

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

  // Links (only the studio and github.com are ever opened)
  openWeb: (webPath: string) => call<void>("open_web", { webPath }),
  openStudio: () => call<void>("open_studio"),
  /**
   * Opens a link someone wrote. Rejects with `link_outside` when it leads
   * outside the studio and GitHub and `sure` is false; with `link_refused`
   * for anything that isn't a web or mail address.
   */
  openLink: (url: string, sure: boolean) => call<void>("open_link", { url, sure }),
  /** Markdown as the tree `markdown()` draws, for "Preview". */
  markdownPreview: (text: string) => call<MdBlock[]>("markdown_preview", { text }),
  openGithub: (fullName: string) => call<void>("open_github", { fullName }),

  // Pull requests
  /** Open pull requests: across the studio, or of one repository (`owner/name`). */
  pullsList: (repo?: string) => call<PullRow[]>("pulls_list", { repo: repo ?? null }),
  pullDetail: (repo: string, number: number) => call<PullView>("pull_detail", { repo, number }),
  pullMerge: (repo: string, number: number, method: string, whenGreen: boolean) =>
    call<MergeResult>("pull_merge", { repo, number, method, whenGreen }),
  /** Merges the base branch into the pull request's branch, in the studio. */
  pullUpdate: (repo: string, number: number) => call<BranchUpdate>("pull_update", { repo, number }),
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

// ===================================================================
// The git client. Four areas, each with its own commands
// (src-tauri/src/gitui_<area>.rs), its own types here and its own object
// of typed calls: `stageApi`, `historyApi`, `branchesApi`, `flowApi`.
// Every call takes the working copy's folder (`path`) first.
// docs/git-client.md says who owns what.
// ===================================================================

// ------------------------------------------------- git client: shared

export type FileStatus = "modified" | "added" | "deleted" | "renamed" | "copied";

/** One file of a diff, as flat numbered lines (`atelier_core::diff::FileDiff`). */
export interface FileDiff {
  /** null for a new file. */
  old_path: string | null;
  /** null for a deleted file. */
  new_path: string | null;
  status: FileStatus;
  binary: boolean;
  additions: number;
  deletions: number;
  lines: DiffLine[];
  truncated: boolean;
}

export interface RefLabel {
  kind: "head" | "branch" | "remote" | "tag";
  /** `main`, `v0.9`; for a remote branch, the branch without the remote. */
  name: string;
  remote: string | null;
}

/** A ref drawn as a pill, in the colour threads. */
export interface Pill {
  text: string;
  tone: "yours" | "github" | "quiet";
}

export type OperationKind = "none" | "merge" | "rebase" | "cherry_pick" | "revert" | "stash_apply";

export interface PlanItem {
  id: string;
  short: string;
  action: "keep" | "reword" | "squash" | "fixup" | "drop";
  subject: string;
  done: boolean;
  /** The step it stopped at. */
  current: boolean;
}

/** What is under way in a working copy. */
export interface OperationState {
  kind: OperationKind;
  /** What started it: `merge`, `pull`, `rebase`, `tidy`, `cherry_pick`, `revert`, `stash_apply`, `stash_pop`, `switch`. */
  cause: string | null;
  /** `Merging main into feature/tunnel-ui` */
  title: string;
  yours: string | null;
  theirs: string | null;
  step: number | null;
  total: number | null;
  plan: PlanItem[];
  /** Files that still have an unresolved conflict. */
  conflicts: number;
  can_continue: boolean;
  can_skip: boolean;
}

/** How an action ended: done, or stopped at conflicts (`done: false`). */
export interface OpOutcome {
  done: boolean;
  /** Plain words for the Undo bar: `Rebased feature/x onto main`. */
  summary: string;
  conflicts: string[];
  head: string | null;
  /** Its entry in Recent actions, once done. */
  journal: number | null;
  state: OperationState;
}

// -------------------------------------------------- git client: stage

export type StageSide = "staged" | "unstaged";

export interface StageEntry {
  path: string;
  old_path: string | null;
  kind: ChangeKind;
  additions: number | null;
  deletions: number | null;
  binary: boolean;
}

export interface HeadCommit {
  sha: string;
  short: string;
  subject: string;
  body: string;
  /** Some remote has it: amending rewrites shared history. */
  pushed: boolean;
}

export interface StageView {
  branch: string | null;
  /** The commit "Amend last commit" would rewrite. */
  head: HeadCommit | null;
  staged: StageEntry[];
  unstaged: StageEntry[];
  /** Neither staged nor not: resolve them first (flow area). */
  conflicted: StageEntry[];
  /** `Demi <demi@example.com>` */
  committing_as: string | null;
}

export interface ChunkLine {
  /** Position in the chunk: what a line selection sends back. */
  index: number;
  kind: "context" | "add" | "delete";
  old: number | null;
  new: number | null;
  text: string;
  /** The file ends here without a newline. */
  no_newline: boolean;
}

export interface Chunk {
  index: number;
  old_start: number;
  old_lines: number;
  new_start: number;
  new_lines: number;
  /** What git printed after the `@@`: usually the enclosing function. */
  section: string;
  additions: number;
  deletions: number;
  lines: ChunkLine[];
}

export interface StageDiff {
  path: string;
  old_path: string | null;
  side: StageSide;
  kind: ChangeKind;
  status: FileStatus;
  /** Binary files are staged or discarded whole. */
  binary: boolean;
  additions: number;
  deletions: number;
  chunks: Chunk[];
  /** Send this back with a chunk or line selection: a file that changed in between is refused, not mangled. */
  token: string;
}

export type Selection =
  | { kind: "file" }
  | { kind: "chunks"; chunks: number[] }
  | { kind: "lines"; lines: { chunk: number; line: number }[] };

export interface Committed {
  sha: string;
  short: string;
  subject: string;
  amended: boolean;
  journal: number | null;
}

export const stageApi = {
  status: (path: string) => call<StageView>("stage_status", { path }),
  diff: (path: string, file: string, side: StageSide) => call<StageDiff>("stage_diff", { path, file, side }),
  /** Stages a file, chunks or lines of its unstaged diff. */
  add: (path: string, file: string, selection: Selection, token?: string) =>
    call<void>("stage_add", { path, file, selection, token: token ?? null }),
  /** Unstages a file, chunks or lines of its staged diff. */
  remove: (path: string, file: string, selection: Selection, token?: string) =>
    call<void>("stage_remove", { path, file, selection, token: token ?? null }),
  addAll: (path: string) => call<void>("stage_add_all", { path }),
  removeAll: (path: string) => call<void>("stage_remove_all", { path }),
  /** Ask first (`nav.openDiscardDialog`). Resolves to the Recently discarded entry, or null when nothing went. */
  discard: (path: string, file: string, selection: Selection, token?: string) =>
    call<number | null>("stage_discard", { path, file, selection, token: token ?? null }),
  /** Ask first. Several files as one Recently discarded entry. */
  discardFiles: (path: string, files: string[]) => call<number | null>("stage_discard_files", { path, files }),
  /** Ask first. Everything uncommitted, untracked files too. */
  discardAll: (path: string) => call<number | null>("stage_discard_all", { path }),
  /** Commits what is staged; `amend` rewrites the last commit with it. */
  commit: (path: string, message: string, description: string, amend: boolean) =>
    call<Committed>("stage_commit", { path, message, description, amend }),
};

// ------------------------------------------------ git client: history

export type EdgeKind = "pass" | "in" | "out";

/** One line segment of the graph in one row. `pass`: straight down column `from`; `in`: from the top of column `from` into the dot; `out`: from the dot to the bottom of column `to`. */
export interface GraphEdge {
  kind: EdgeKind;
  from: number;
  to: number;
  /** The same number all the way down one line of work. */
  color: number;
}

export interface GraphRow {
  id: string;
  short: string;
  parents: string[];
  refs: RefLabel[];
  pills: Pill[];
  author: string;
  email: string;
  /** RFC 3339 */
  time: string;
  subject: string;
  body: string;
  /** The column of the commit's dot. */
  column: number;
  color: number;
  edges: GraphEdge[];
  /** No remote has it yet. */
  unpushed: boolean;
  is_head: boolean;
}

export interface GraphQuery {
  /** One branch (or any ref); every branch, remote and tag when left out. */
  branch?: string | null;
  /** Found in the message, the author, or the start of an id. */
  text?: string | null;
  author?: string | null;
  /** Only commits that touched this file or folder. */
  path?: string | null;
  skip?: number;
  /** 0 or left out: 200. */
  limit?: number;
}

export interface GraphView {
  rows: GraphRow[];
  more: boolean;
  /** The widest the graph gets on this page. */
  columns: number;
  /** A search is on: a flat list (no edges), not a graph. */
  filtered: boolean;
  branch: string | null;
  head: string | null;
}

export interface CommitFile {
  path: string;
  old_path: string | null;
  status: FileStatus;
  additions: number | null;
  deletions: number | null;
  binary: boolean;
}

export interface CommitInfo {
  id: string;
  short: string;
  parents: string[];
  author: string;
  email: string;
  time: string;
  committer: string;
  subject: string;
  body: string;
  refs: RefLabel[];
  pills: Pill[];
  /** Against the first parent, for a merge. */
  files: CommitFile[];
  pushed: boolean;
  is_head: boolean;
  /** Part of the branch that is checked out. */
  on_current: boolean;
}

export interface FileHistoryEntry {
  id: string;
  short: string;
  author: string;
  time: string;
  subject: string;
  /** The file's name in this commit. */
  path: string;
  /** What it was called before this commit renamed it. */
  old_path: string | null;
  status: FileStatus;
  additions: number | null;
  deletions: number | null;
}

export interface FileHistory {
  path: string;
  entries: FileHistoryEntry[];
  more: boolean;
}

export interface FileContent {
  path: string;
  rev: string;
  text: string;
  binary: boolean;
  truncated: boolean;
}

export interface BlameRun {
  commit: string;
  short: string;
  author: string;
  email: string;
  time: string;
  summary: string;
  /** First and last line, from 1. */
  start: number;
  end: number;
  /** The file's name in that commit. */
  path: string;
  /** Where "Blame before this change" looks next. */
  previous: { commit: string; path: string } | null;
  /** Changed in the working tree, not committed yet. */
  uncommitted: boolean;
  lines: string[];
}

export interface Blame {
  path: string;
  at: string | null;
  runs: BlameRun[];
  lines: number;
}

export const historyApi = {
  graph: (path: string, query: GraphQuery = {}) => call<GraphView>("history_graph", { path, query }),
  commit: (path: string, id: string) => call<CommitInfo>("history_commit", { path, id }),
  /** What one commit did to one file. `oldFile`: its name before, when the commit renamed it. */
  commitDiff: (path: string, id: string, file: string, oldFile?: string | null) =>
    call<FileDiff>("history_commit_diff", { path, id, file, oldFile: oldFile ?? null }),
  /** The commits that changed a file, across renames, starting at `from` (HEAD). */
  file: (path: string, file: string, from?: string | null, limit?: number) =>
    call<FileHistory>("history_file", { path, file, from: from ?? null, limit: limit ?? null }),
  fileAt: (path: string, id: string, file: string) => call<FileContent>("history_file_at", { path, id, file }),
  /** Blame at a commit, or of the working tree. */
  blame: (path: string, file: string, at?: string | null) => call<Blame>("history_blame", { path, file, at: at ?? null }),
  /** Blame as it was just before `commit` (a run's own `previous` is more exact across renames). */
  blameBefore: (path: string, file: string, commit: string) => call<Blame>("history_blame_before", { path, file, commit }),
};

// ----------------------------------------------- git client: branches

export interface BranchInfo {
  /** `main`; for a remote branch, without the remote. */
  name: string;
  full_ref: string;
  remote: string | null;
  current: boolean;
  upstream: string | null;
  /** The upstream was deleted on the remote. */
  upstream_gone: boolean;
  ahead: number;
  behind: number;
  sha: string;
  short: string;
  subject: string;
  author: string;
  time: string;
  /** The folder of another worktree that has it checked out. */
  worktree: string | null;
}

export interface RemoteGroup {
  remote: string;
  kind: "studio" | "github" | "other";
  /** `On your studio`, `On GitHub`, `On upstream` */
  label: string;
  branches: BranchInfo[];
}

export interface BranchesView {
  current: string | null;
  head: string | null;
  local: BranchInfo[];
  remotes: RemoteGroup[];
  /** Uncommitted files: Switch asks first when there are any. */
  changes: number;
  fetched_at: string | null;
}

export type SwitchHow = "ask" | "carry" | "put_aside";

export interface SwitchOutcome {
  switched: boolean;
  /** Uncommitted changes and `ask`: nothing happened; ask carry / put aside / cancel. */
  needs_choice: boolean;
  changes: number;
  put_aside: string | null;
  /** Carried changes that collided: open Resolve conflicts. */
  conflicts: string[];
  branch: string;
  journal: number | null;
}

export type PullMode = "merge" | "rebase" | "ff_only";

export interface DeleteCheck {
  branch: string;
  /** Nothing is lost by deleting it. */
  safe: boolean;
  /** Commits only this branch has. */
  unmerged: number;
  current: boolean;
  worktree: string | null;
}

export interface ForcePushPreview {
  branch: string;
  remote: string;
  remote_branch: string;
  /** The lease: send it back to `forcePush`. */
  expected: string | null;
  /** Commits on the remote that would go. */
  replaced: number;
  /** Commits from here that would arrive. */
  new: number;
}

export interface Pushed {
  branch: string;
  remote: string;
  forced: boolean;
}

export interface TagInfo {
  name: string;
  sha: string;
  short: string;
  annotated: boolean;
  subject: string;
  time: string;
}

export interface RemoteEntry {
  name: string;
  url: string;
  push_url: string | null;
  kind: "studio" | "github" | "other";
  /** `Your studio · fetch and push` */
  what: string;
}

export interface StashEntry {
  /** The stash commit: what every action takes. */
  id: string;
  short: string;
  position: number;
  message: string;
  branch: string | null;
  time: string;
  files: number;
}

export interface StashDetail {
  entry: StashEntry;
  files: FileDiff[];
}

export interface Worktree {
  path: string;
  /** With the home folder as `~`. */
  display_path: string;
  /** The repository's main folder. */
  main: boolean;
  /** The one these questions were asked from. */
  current: boolean;
  branch: string | null;
  head: string | null;
  head_short: string | null;
  locked: boolean;
  lock_reason: string | null;
  /** Its folder is gone; `worktreePrune` forgets it. */
  prunable: boolean;
  /** Uncommitted files. */
  dirty: number;
  upstream: string | null;
  ahead: number;
  behind: number;
  subject: string | null;
  time: string | null;
}

export type WorktreeSource =
  | { kind: "branch"; name: string }
  /** `from`: a branch, a fetched ref (`origin/x`, `refs/pull/14/head`) or a commit; HEAD when left out. */
  | { kind: "new_branch"; name: string; from?: string | null }
  | { kind: "detached"; rev: string };

export const branchesApi = {
  list: (path: string) => call<BranchesView>("branches_list", { path }),
  /** Makes a branch at `from` (HEAD); it isn't switched to. */
  create: (path: string, name: string, from?: string | null) => call<void>("branches_create", { path, name, from: from ?? null }),
  rename: (path: string, old: string, name: string) => call<void>("branches_rename", { path, old, new: name }),
  deleteCheck: (path: string, name: string) => call<DeleteCheck>("branches_delete_check", { path, name }),
  /** Local only. `force` when `deleteCheck` says it isn't safe (ask first). */
  delete: (path: string, name: string, force: boolean) => call<void>("branches_delete", { path, name, force }),
  /** A local branch, or `remote/branch` (which gets a local follower). */
  switch: (path: string, target: string, how: SwitchHow = "ask") => call<SwitchOutcome>("branches_switch", { path, target, how }),
  setUpstream: (path: string, branch: string, upstream: string | null) =>
    call<void>("branches_set_upstream", { path, branch, upstream }),
  /** One remote, or all of them when left out. */
  fetch: (path: string, remote?: string | null, prune = true) => call<void>("branches_fetch", { path, remote: remote ?? null, prune }),
  /** `done: false` means conflicts: `nav.after(outcome, "pull")`. */
  pull: (path: string, mode: PullMode) => call<OpOutcome>("branches_pull", { path, mode }),
  push: (path: string, branch?: string | null, remote?: string | null) =>
    call<Pushed>("branches_push", { path, branch: branch ?? null, remote: remote ?? null }),
  forcePreview: (path: string, branch?: string | null, remote?: string | null) =>
    call<ForcePushPreview>("branches_force_preview", { path, branch: branch ?? null, remote: remote ?? null }),
  /** With lease: only if the remote still stands at `expected` (from the preview shown). Ask first. */
  forcePush: (path: string, expected: string, branch?: string | null, remote?: string | null) =>
    call<Pushed>("branches_force_push", { path, branch: branch ?? null, remote: remote ?? null, expected }),

  tags: (path: string) => call<TagInfo[]>("branches_tags", { path }),
  /** Annotated when there is a message. `at`: a commit (HEAD). */
  tagCreate: (path: string, name: string, at?: string | null, message?: string | null) =>
    call<void>("branches_tag_create", { path, name, at: at ?? null, message: message ?? null }),
  tagDelete: (path: string, name: string) => call<void>("branches_tag_delete", { path, name }),
  tagPush: (path: string, name: string, remote: string) => call<void>("branches_tag_push", { path, name, remote }),

  remotes: (path: string) => call<RemoteEntry[]>("branches_remotes", { path }),
  remoteAdd: (path: string, name: string, url: string) => call<void>("branches_remote_add", { path, name, url }),
  remoteRename: (path: string, old: string, name: string) => call<void>("branches_remote_rename", { path, old, new: name }),
  remoteRemove: (path: string, name: string) => call<void>("branches_remote_remove", { path, name }),
  remoteSetUrl: (path: string, name: string, url: string) => call<void>("branches_remote_set_url", { path, name, url }),

  stashes: (path: string) => call<StashEntry[]>("branches_stashes", { path }),
  /** Puts changes aside: everything, or only `files`. */
  stashSave: (path: string, message: string, includeUntracked = true, files: string[] = []) =>
    call<StashEntry>("branches_stash_save", { path, message, includeUntracked, files }),
  stashShow: (path: string, id: string) => call<StashDetail>("branches_stash_show", { path, id }),
  /** `done: false` means conflicts: `nav.after(outcome, "stash")`. */
  stashApply: (path: string, id: string) => call<OpOutcome>("branches_stash_apply", { path, id }),
  stashPop: (path: string, id: string) => call<OpOutcome>("branches_stash_pop", { path, id }),
  /** Resolves to its entry in Recent actions (it can be brought back). */
  stashDrop: (path: string, id: string) => call<number>("branches_stash_drop", { path, id }),

  worktrees: (path: string) => call<Worktree[]>("branches_worktrees", { path }),
  worktreeSuggest: (path: string, branch: string) =>
    call<{ path: string; display_path: string }>("branches_worktree_suggest", { path, branch }),
  /** Opens the folder chooser for a parent folder; null when cancelled. */
  worktreePick: () => call<string | null>("branches_worktree_pick"),
  /** Without `target` it goes to the suggested folder beside the repository. */
  worktreeAdd: (path: string, source: WorktreeSource, target?: string | null) =>
    call<Worktree>("branches_worktree_add", { path, target: target ?? null, source }),
  /** Refused while it has uncommitted changes or is locked, unless `force` (ask first). */
  worktreeRemove: (path: string, worktree: string, force = false) =>
    call<void>("branches_worktree_remove", { path, worktree, force }),
  worktreeLock: (path: string, worktree: string, reason?: string) =>
    call<void>("branches_worktree_lock", { path, worktree, reason: reason ?? null }),
  worktreeUnlock: (path: string, worktree: string) => call<void>("branches_worktree_unlock", { path, worktree }),
  worktreePrune: (path: string) => call<string[]>("branches_worktree_prune", { path }),
};

// --------------------------------------------------- git client: flow

export type MergeMode = "auto" | "no_ff" | "ff_only";
export type ResetMode = "soft" | "mixed" | "hard";
export type PlanAction = "keep" | "reword" | "squash" | "fixup" | "drop";

/** One line of a tidy-up plan, oldest commit first. */
export interface PlanStep {
  id: string;
  action: PlanAction;
  /** The new message for `reword` (and, optionally, `squash`). */
  message?: string | null;
}

export interface PlanCommit {
  id: string;
  short: string;
  subject: string;
  body: string;
  author: string;
  time: string;
  /** Some remote has it: rewriting it means a force push. */
  pushed: boolean;
}

export interface RebasePlan {
  branch: string | null;
  /** The commit that stays exactly as it is: pass it to `tidy`. */
  base: string;
  base_short: string;
  base_name: string | null;
  /** Oldest first. */
  commits: PlanCommit[];
  has_merges: boolean;
}

export interface ResetPreview {
  branch: string | null;
  target: string;
  target_short: string;
  /** Commits the branch steps back. */
  commits: number;
  /** How many of those a remote already has. */
  pushed: number;
  uncommitted: number;
}

export type Choice =
  | { kind: "yours" }
  | { kind: "theirs" }
  | { kind: "both"; yours_first: boolean }
  | { kind: "custom"; text: string };

export type ConflictKind =
  | "both_modified"
  | "both_added"
  | "deleted_by_you"
  | "deleted_by_them"
  | "added_by_you"
  | "added_by_them"
  | "both_deleted";

export interface ConflictFile {
  path: string;
  kind: ConflictKind;
  binary: boolean;
  resolved: boolean;
  /** Binary, or one side deleted it: only `conflictTake` applies. */
  whole_file_only: boolean;
  regions: number;
  chosen: number;
  /** `yours`, `theirs`, `mixed` or `edited`, once resolved. */
  how: string | null;
}

export interface ConflictList {
  state: OperationState;
  needs_you: ConflictFile[];
  /** Files that merged without help. */
  auto_resolved: string[];
  done: number;
  total: number;
}

export type Region =
  | { kind: "common"; lines: string[]; yours_start: number; theirs_start: number }
  | {
      kind: "conflict";
      /** Counts conflicts only, from 0. */
      index: number;
      yours: string[];
      theirs: string[];
      /** null when the file is new on both sides. */
      base: string[] | null;
      yours_start: number;
      theirs_start: number;
      choice: Choice | null;
    };

export interface ResultLine {
  text: string;
  from: "common" | "yours" | "theirs" | "custom" | "undecided";
  region: number | null;
}

export interface ConflictDetail {
  path: string;
  kind: ConflictKind;
  binary: boolean;
  resolved: boolean;
  whole_file_only: boolean;
  /** The branch names to put over the two panes. */
  yours_label: string;
  theirs_label: string;
  regions: Region[];
  total: number;
  chosen: number;
  /** The file as it will be. */
  result: ResultLine[];
}

export interface JournalEntry {
  id: number;
  unix: number;
  /** RFC 3339 */
  time: string;
  kind: string;
  /** `Rebased feature/x onto main` */
  title: string;
  /** It was undone since. */
  undone: boolean;
  /** What going back to before this does, in plain words. */
  how: string;
  /** For `discard` entries. */
  discarded: { files: number; additions: number; deletions: number; paths: string[]; restored: boolean } | null;
}

export interface UndoOptions {
  /** Put the current uncommitted changes aside first. */
  put_aside?: boolean;
  /** Go back although commits were made since outside Atelier. */
  force?: boolean;
}

export interface UndoOutcome {
  done: boolean;
  /** `Undid “Rebased feature/x onto main”` */
  title: string;
  /** Why not, and what would let it: `nothing`, `busy`, `dirty` (offer put aside) or `moved` (offer force). */
  refusal: { reason: "nothing" | "busy" | "dirty" | "moved"; message: string; can_put_aside: boolean; can_force: boolean } | null;
  put_aside: string | null;
  journal: number | null;
}

export const flowApi = {
  /** What is under way: nothing, a merge, a rebase (step n of m, its plan), a cherry-pick, a revert, applying put-aside changes. */
  state: (path: string) => call<OperationState>("flow_state", { path }),
  continue: (path: string) => call<OpOutcome>("flow_continue", { path }),
  skip: (path: string) => call<OpOutcome>("flow_skip", { path }),
  /** Stop and go back: everything as it was before it started. */
  abort: (path: string) => call<OpOutcome>("flow_abort", { path }),

  merge: (path: string, branch: string, mode: MergeMode = "auto") => call<OpOutcome>("flow_merge", { path, branch, mode }),
  rebase: (path: string, onto: string) => call<OpOutcome>("flow_rebase", { path, onto }),
  /** The commits a tidy-up from `base` would work on (from the upstream or main when left out). */
  rebasePlan: (path: string, base?: string | null) => call<RebasePlan>("flow_rebase_plan", { path, base: base ?? null }),
  /** Every commit of the plan exactly once, in the order they should end up, oldest first. */
  tidy: (path: string, base: string, plan: PlanStep[]) => call<OpOutcome>("flow_tidy", { path, base, plan }),
  cherryPick: (path: string, id: string) => call<OpOutcome>("flow_cherry_pick", { path, id }),
  revert: (path: string, id: string) => call<OpOutcome>("flow_revert", { path, id }),
  resetPreview: (path: string, id: string) => call<ResetPreview>("flow_reset_preview", { path, id }),
  /** Ask first (`nav.openResetDialog`). `hard` keeps the uncommitted part in Recently discarded. */
  reset: (path: string, id: string, mode: ResetMode) => call<OpOutcome>("flow_reset", { path, id, mode }),

  conflicts: (path: string) => call<ConflictList>("flow_conflicts", { path }),
  conflictFile: (path: string, file: string) => call<ConflictDetail>("flow_conflict_file", { path, file }),
  /** Chooses what one conflict becomes (null takes the choice back). The file is marked resolved once all are chosen. */
  conflictChoose: (path: string, file: string, region: number, choice: Choice | null) =>
    call<ConflictDetail>("flow_conflict_choose", { path, file, region, choice }),
  /** One side's whole file: the only choice for a binary file or a deletion. */
  conflictTake: (path: string, file: string, side: "yours" | "theirs") => call<void>("flow_conflict_take", { path, file, side }),
  /** After editing by hand: refused while conflict markers are left. */
  conflictMark: (path: string, file: string) => call<void>("flow_conflict_mark", { path, file }),
  conflictReopen: (path: string, file: string) => call<void>("flow_conflict_reopen", { path, file }),

  /** Recent actions, newest first. */
  journal: (path: string) => call<JournalEntry[]>("flow_journal", { path }),
  undo: (path: string, options: UndoOptions = {}) => call<UndoOutcome>("flow_undo", { path, options }),
  /** "Go back to before this". */
  goBack: (path: string, entry: number, options: UndoOptions = {}) => call<UndoOutcome>("flow_go_back", { path, entry, options }),
  /** Recently discarded (kept 30 days), newest first. */
  discarded: (path: string) => call<JournalEntry[]>("flow_discarded", { path }),
  /** `overwrite`: replace the files when the old changes no longer fit on top. */
  restoreDiscarded: (path: string, entry: number, overwrite = false) =>
    call<void>("flow_restore_discarded", { path, entry, overwrite }),
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
