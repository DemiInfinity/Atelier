# The git client in Atelier Desktop

The desktop app is a full git client: staging by chunk and line, the commit
graph, branches, tags, remotes, putting changes aside, worktrees, merge,
rebase, tidying up commits, resolving conflicts, and an undo for all of it.

It is built in two layers:

- **The engine** is `atelier-core` (`src/git/*.rs`). It shells out to the
  `git` binary and returns typed, serialisable results. Every operation is
  tested on throwaway repositories (`atelier-core/tests/gitclient/`).
- **The app** wraps the engine in thin Tauri commands, one Rust module per
  area (`src-tauri/src/gitui_<area>.rs`), and draws it with one set of
  TypeScript files per area.

The design boards are `DeskFlow`, `DeskStage`, `DeskGraph`, `DeskBranches`,
`DeskWorktrees`, `DeskRebase`, `DeskConflict`, `DeskBlame`, `DeskSafety`,
`DeskRepoBar` and `DeskSidebar`.

## Who owns what

Four areas. An area's files are edited by that area only; the shared files
are edited by nobody while the areas are being built (ask in the ticket if
one of them is in your way).

| Area | Screens | Rust | TypeScript | Styles |
| --- | --- | --- | --- | --- |
| **stage** | Changes tab | `gitui_stage.rs` | `screens/git-stage.ts` (+ `.test.ts`) | `styles/git-stage.css` |
| **history** | History tab, file history, blame | `gitui_history.rs` | `screens/git-history.ts`, `screens/git-blame.ts` | `styles/git-history.css` |
| **branches** | Branches tab (branches, tags, put aside, remotes), worktrees, worktree rows in the sidebar | `gitui_branches.rs` | `screens/git-branches.ts`, `screens/git-worktrees.ts`, `sidebar-worktrees.ts` | `styles/git-branches.css` |
| **flow** | Tidy up commits, Resolve conflicts, the Undo bar, Recent actions, Recently discarded, the careful dialogs, the bar's Undo / Put aside buttons and in-progress banner | `gitui_flow.rs` | `screens/git-rebase.ts`, `screens/git-conflict.ts`, `undo.ts`, `git-dialogs.ts` | `styles/git-flow.css` |

Each area may add tests next to its files (`*.test.ts`, and Rust tests in
its own `src-tauri/tests/gitui_<area>.rs` using `World::git_fixture`).

Shared, not to be edited by an area: `atelier-core` (the engine),
`src-tauri/src/{lib,shell,commands,desk,repo,today}.rs`,
`src-tauri/tests/{common,gitui.rs}`, `src/{api,routes,shell,ctx,dom,dialogs,
git-nav,git-ui,sideview,selftest}.ts`, `src/logic/sidebar.ts`,
`src/screens/repo.ts`, `styles/{base,repo}.css`, `scripts/*`, this file.
The old tabs (`screens/repo-history.ts`, `repo-changes.ts`,
`repo-branches.ts`) stay as they are until their area's screen is on; they
are removed afterwards, not by the areas.

### Switching a screen on

Every new screen starts as a stand-in that reads through its area's
commands and lists what they answer. The History, Changes and Branches tabs
keep showing the screen that was there before until the new one says it is
ready:

```ts
// screens/git-stage.ts, git-history.ts, git-branches.ts; undo.ts; sidebar-worktrees.ts
export const READY = false; // set to true when the screen replaces the old one
```

`READY` in `undo.ts` switches on the flow area's parts of the repository
screen (the Undo bar, the bar's Undo / Put aside buttons, the banner);
`READY` in `sidebar-worktrees.ts` nests linked worktrees under their
repository in the sidebar. File history, blame, worktrees, tidy up and
resolve conflicts have nothing older behind them and are always the new
view. To look at everything before it is switched on, call
`setGitPreview(true)` (from `git-nav.ts`; the self-test does).

### Adding a command

All of an area's commands are in its `gitui_<area>.rs`: a method on `Desk`
(testable without Tauri), a `#[tauri::command]` of the same name, and an
entry in that file's `handler()`. The shell routes by the first word of the
command's name (`stage_`, `history_`, `branches_`, `flow_`), so a new
command needs no change to `shell.rs`. The typed calls in `src/api.ts`
cover every command that exists now; for one you add, use the exported
`call` from your own file:

```ts
import { call } from "../api";
const lfs = (path: string) => call<LfsView>("stage_lfs", { path });
```

Rules every command keeps: take the folder as `path` and resolve it with
`self.repo_path(path)` first (inside the clone root, a watch folder, a
picked folder, or a linked worktree of such a repository); return
`CmdResult<T>` with a `Serialize` type; no shell, no `innerHTML`; text from
git is data, never markup or a command line.

## Navigation: `host.nav`

Screens never mount one another. Each gets a `RepoHost` (`screens/repo.ts`)
with `ctx` (toasts, dialogs, `busy`), `path`, `view()`, `reloadAll()` and
`nav` (`git-nav.ts`):

| Call | Leads to |
| --- | --- |
| `nav.openHistory(commit?)` | History, with that commit selected |
| `nav.openFileHistory(file, at?)` | One file's history (inside History) |
| `nav.openBlame(file, at?)` | Blame at a commit, or of the working tree (inside History) |
| `nav.openChanges(file?)` | Changes, with that file selected |
| `nav.openBranches(section?)` | Branches: `branches`, `tags`, `put-aside`, `remotes`, `worktrees` |
| `nav.openWorktrees()` | Worktrees (inside Branches) |
| `nav.openTidyUp({ base?, plan? })` | Tidy up commits; `plan` carries actions chosen in a commit's menu (Reword…, Squash into previous…) |
| `nav.openConflicts(cause?)` | Resolve conflicts; `cause`: `merge`, `rebase`, `tidy`, `pull`, `cherry-pick`, `revert`, `stash`, `switch`, `update` |
| `nav.back()` | Where the heavy view was opened from |
| `nav.finish(text, journal?, commit?)` | History, with the Undo bar saying `text` |
| `nav.after(outcome, cause)` | Not done → Resolve conflicts; done → `finish(outcome.summary, …)` |
| `nav.showUndoBar(text, journal?)` | The Undo bar (a pop-up until the flow area is on) |
| `nav.openRecentActions()` / `nav.openRecentlyDiscarded()` | The two lists |
| `nav.openDiscardDialog({ what, files, additions, deletions })` | Asks first; resolves `true` to go ahead (the caller then calls `stageApi.discard…`) |
| `nav.openResetDialog(target)` | "Undo commits back to here": previews, asks how, resets, finishes on History |
| `nav.openForcePushDialog(branch?, remote?)` | "Force push safely": previews, asks, pushes with the lease that was shown |
| `nav.switchTo(branch)` | Switches; with uncommitted changes asks: bring them along, put them aside, cancel |

A worktree opens like any repository: `ctx.go({ screen: "repo", path:
worktree.path })`. A route may also name a view directly:
`{ screen: "repo", path, view: { kind: "blame", file } }`.

### The three rules (DeskFlow)

1. **Every repository opens on History.** `DEFAULT_REPO_TAB`; a worktree
   too.
2. **Every heavy action can be stopped, and stopping returns where you
   were.** Tidy up commits and Resolve conflicts are the heavy views.
   `nav.openTidyUp` / `nav.openConflicts` remember the place they were
   opened from; "Cancel" / "Stop and go back" call `flowApi.abort(path)`
   (when something is under way) and then `nav.back()`. A conflict opened
   from a tidy-up goes back to the tidy-up, which goes back to where it
   was started.
3. **Every finish lands on History with the Undo bar.** After any action
   that may stop at conflicts, hand its answer to `nav.after(outcome,
   cause)`; after one that can't, call `nav.finish(text, journal)` or, for
   something that should stay on the current screen (a branch deleted, a
   put-aside entry dropped), `nav.showUndoBar(text, journal)` and
   `host.reloadAll()`.

While something is under way (`flowApi.state(path).kind !== "none"`) the
Changes tab is Resolve conflicts, and the bar shows the banner.

## Undo, in one paragraph

Every history- or worktree-changing engine call writes an entry to
`<git dir>/atelier/journal.jsonl` before it runs (where HEAD and the refs
were, and the uncommitted work as commits kept alive under
`refs/atelier/undo/<id>/…`). `flowApi.undo(path)` undoes the latest one;
`flowApi.goBack(path, entry)` steps back to before an entry. Both refuse,
with a reason and an offer, rather than lose work: `dirty` (offer to put
the changes aside first) or `moved` (commits were made since outside the
app; offer to go back anyway, they stay reachable from Recent actions).
Anything thrown away (a discarded file, chunk or line, a hard reset, a
forced worktree removal) is kept under `refs/atelier/discarded/<id>` and
listed by `flowApi.discarded(path)`; `flowApi.restoreDiscarded` brings it
back. Both lists are pruned after 30 days. Nothing of this is ever in the
working tree.

## Commands

Every command takes `path` (the working copy's folder) first. Types are in
`src/api.ts` under "The git client"; the Rust types are the engine's.

### stage — `stageApi`

| Call | Command | Answers |
| --- | --- | --- |
| `status(path)` | `stage_status` | `StageView`: `staged`, `unstaged`, `conflicted` (`StageEntry[]`), `head`, `branch`, `committing_as` |
| `diff(path, file, side)` | `stage_diff` | `StageDiff`: `chunks` (`Chunk[]` of `ChunkLine`), `token`, `binary` |
| `add(path, file, selection, token?)` | `stage_add` | — |
| `remove(path, file, selection, token?)` | `stage_remove` | — (unstage) |
| `addAll(path)` / `removeAll(path)` | `stage_add_all` / `stage_remove_all` | — |
| `discard(path, file, selection, token?)` | `stage_discard` | the Recently discarded entry, or `null` |
| `discardFiles(path, files)` | `stage_discard_files` | the entry, or `null` |
| `discardAll(path)` | `stage_discard_all` | the entry, or `null` |
| `commit(path, message, description, amend)` | `stage_commit` | `Committed` |

`Selection` is `{ kind: "file" }`, `{ kind: "chunks", chunks: number[] }`
or `{ kind: "lines", lines: { chunk, line }[] }` (`line` is
`ChunkLine.index`; only added and removed lines can be chosen). Send the
diff's `token` back with a chunk or line selection: if the file changed in
between the answer is a refusal ("…changed since you looked at it"), not a
mangled file. A binary file can only be taken whole. "Put aside (stash)
these changes…" is `branchesApi.stashSave(path, message, true, files?)`.

### history — `historyApi`

| Call | Command | Answers |
| --- | --- | --- |
| `graph(path, query?)` | `history_graph` | `GraphView`: `rows` (`GraphRow[]` with `column`, `color`, `edges`, `pills`), `more`, `columns`, `filtered` |
| `commit(path, id)` | `history_commit` | `CommitInfo` with `files` (`CommitFile[]`) |
| `commitDiff(path, id, file, oldFile?)` | `history_commit_diff` | `FileDiff` |
| `file(path, file, from?, limit?)` | `history_file` | `FileHistory` (follows renames) |
| `fileAt(path, id, file)` | `history_file_at` | `FileContent` |
| `blame(path, file, at?)` | `history_blame` | `Blame`: `runs` (`BlameRun[]`) |
| `blameBefore(path, file, commit)` | `history_blame_before` | `Blame` |

`GraphQuery`: `branch` (all branches when left out), `text` (message,
author, or the start of an id), `author`, `path`, `skip`, `limit`. The
lanes are laid out in Rust: draw each row's dot in `column`, and its
`edges` as `pass` (straight down column `from`), `in` (from the top of
column `from` into the dot) and `out` (from the dot to the bottom of column
`to`); `color` is the same number all the way down one line of work. With a
search the rows are a flat list (`filtered`, no edges). Pages continue the
graph: ask again with `skip`. For "Blame before this change" prefer the
run's own `previous` (`historyApi.blame(path, previous.path,
previous.commit)`): it knows the file's earlier name.

The commit menu uses other areas' calls: New branch from here →
`branchesApi.create`; Tag this commit → `branchesApi.tagCreate`;
Cherry-pick → `flowApi.cherryPick` then `nav.after(outcome,
"cherry-pick")`; Revert → `flowApi.revert` then `nav.after(outcome,
"revert")`; Undo commits back to here → `nav.openResetDialog(id)`; Reword /
Squash → `nav.openTidyUp({ plan })`.

### branches — `branchesApi`

| Call | Command | Answers |
| --- | --- | --- |
| `list(path)` | `branches_list` | `BranchesView`: `local`, `remotes` (`RemoteGroup[]`), `changes`, `fetched_at` |
| `create(path, name, from?)` | `branches_create` | — (not switched to) |
| `rename(path, old, name)` | `branches_rename` | — |
| `deleteCheck(path, name)` | `branches_delete_check` | `DeleteCheck`: `safe`, `unmerged`, `worktree` |
| `delete(path, name, force)` | `branches_delete` | — (local only) |
| `switch(path, target, how?)` | `branches_switch` | `SwitchOutcome` (prefer `nav.switchTo`) |
| `setUpstream(path, branch, upstream)` | `branches_set_upstream` | — |
| `fetch(path, remote?, prune?)` | `branches_fetch` | — |
| `pull(path, mode)` | `branches_pull` | `OpOutcome` → `nav.after(outcome, "pull")` |
| `push(path, branch?, remote?)` | `branches_push` | `Pushed` |
| `forcePreview(path, branch?, remote?)` | `branches_force_preview` | `ForcePushPreview` |
| `forcePush(path, expected, branch?, remote?)` | `branches_force_push` | `Pushed` (prefer `nav.openForcePushDialog`) |
| `tags(path)` · `tagCreate` · `tagDelete` · `tagPush` | `branches_tags` · `branches_tag_create` · `branches_tag_delete` · `branches_tag_push` | `TagInfo[]` · — |
| `remotes(path)` · `remoteAdd` · `remoteRename` · `remoteRemove` · `remoteSetUrl` | `branches_remotes` · `branches_remote_add` · `…_rename` · `…_remove` · `…_set_url` | `RemoteEntry[]` · — |
| `stashes(path)` | `branches_stashes` | `StashEntry[]` |
| `stashSave(path, message, includeUntracked?, files?)` | `branches_stash_save` | `StashEntry` |
| `stashShow(path, id)` | `branches_stash_show` | `StashDetail` (`FileDiff[]`) |
| `stashApply(path, id)` / `stashPop(path, id)` | `branches_stash_apply` / `branches_stash_pop` | `OpOutcome` → `nav.after(outcome, "stash")` |
| `stashDrop(path, id)` | `branches_stash_drop` | its Recent actions entry |
| `worktrees(path)` | `branches_worktrees` | `Worktree[]` |
| `worktreeSuggest(path, branch)` | `branches_worktree_suggest` | `{ path, display_path }` (`<repo>.worktrees/<branch-slug>`) |
| `worktreePick()` | `branches_worktree_pick` | a folder from the chooser, or `null` |
| `worktreeAdd(path, source, target?)` | `branches_worktree_add` | `Worktree` |
| `worktreeRemove(path, worktree, force?)` | `branches_worktree_remove` | — |
| `worktreeLock(path, worktree, reason?)` / `worktreeUnlock` / `worktreePrune(path)` | `branches_worktree_lock` / `…_unlock` / `…_prune` | — / — / the folders forgotten |

`switch` with `how: "ask"` and uncommitted changes changes nothing and
answers `needs_choice`; ask (bring along / put aside / cancel) and call
again with `"carry"` or `"put_aside"`. Carried changes that collide come
back in `conflicts`: open Resolve conflicts (`nav.switchTo` does all of
this). A branch open in another worktree says where (`BranchInfo.worktree`)
and can't be switched to or deleted. Deleting a branch here never deletes
it on a remote. `WorktreeSource` is `{ kind: "branch", name }`,
`{ kind: "new_branch", name, from? }` (`from`: a branch, a fetched ref
such as `origin/x` or `refs/pull/14/head`, or a commit) or
`{ kind: "detached", rev }`. A worktree's folder must be beside its
repository (the suggestion) or inside a folder Atelier looks after
(`worktreePick` makes the chosen folder one). Removing a worktree with
uncommitted changes is refused with the reason unless `force`; then the
changes go to Recently discarded first.

Merge and rebase from a branch row: `flowApi.merge(path, name)` /
`flowApi.rebase(path, name)` then `nav.after(outcome, "merge" |
"rebase")`.

The sidebar: `worktreeRows(copy, linked, actions)` in
`sidebar-worktrees.ts` is called for every repository row; `linked` are the
working copies of the overview whose `worktree_of` is that repository's
folder.

### flow — `flowApi`

| Call | Command | Answers |
| --- | --- | --- |
| `state(path)` | `flow_state` | `OperationState`: `kind` (`none`, `merge`, `rebase`, `cherry_pick`, `revert`, `stash_apply`), `cause`, `title`, `step`/`total`, `plan`, `conflicts`, `can_continue`, `can_skip` |
| `continue(path)` / `skip(path)` / `abort(path)` | `flow_continue` / `flow_skip` / `flow_abort` | `OpOutcome` |
| `merge(path, branch, mode?)` | `flow_merge` | `OpOutcome` |
| `rebase(path, onto)` | `flow_rebase` | `OpOutcome` |
| `rebasePlan(path, base?)` | `flow_rebase_plan` | `RebasePlan`: `base`, `commits` (oldest first), `has_merges` |
| `tidy(path, base, plan)` | `flow_tidy` | `OpOutcome` |
| `cherryPick(path, id)` / `revert(path, id)` | `flow_cherry_pick` / `flow_revert` | `OpOutcome` |
| `resetPreview(path, id)` / `reset(path, id, mode)` | `flow_reset_preview` / `flow_reset` | `ResetPreview` / `OpOutcome` |
| `conflicts(path)` | `flow_conflicts` | `ConflictList`: `needs_you`, `auto_resolved`, `done`/`total`, `state` |
| `conflictFile(path, file)` | `flow_conflict_file` | `ConflictDetail`: `regions`, `result`, `yours_label`, `theirs_label` |
| `conflictChoose(path, file, region, choice)` | `flow_conflict_choose` | `ConflictDetail` |
| `conflictTake(path, file, side)` | `flow_conflict_take` | — |
| `conflictMark(path, file)` / `conflictReopen(path, file)` | `flow_conflict_mark` / `flow_conflict_reopen` | — |
| `journal(path)` | `flow_journal` | `JournalEntry[]` (Recent actions, newest first) |
| `undo(path, options?)` / `goBack(path, entry, options?)` | `flow_undo` / `flow_go_back` | `UndoOutcome` (`refusal` when it didn't) |
| `discarded(path)` / `restoreDiscarded(path, entry, overwrite?)` | `flow_discarded` / `flow_restore_discarded` | `JournalEntry[]` / — |

`OpOutcome.done === false` means it stopped at conflicts (`conflicts`,
`state`); nothing is in Recent actions until it is finished
(`flowApi.continue`) and stopping it (`flowApi.abort`) leaves no trace.
A tidy-up plan is every commit of `rebasePlan` exactly once, oldest first,
in the order they should end up, each `keep`, `reword` (with `message`),
`squash` (both messages kept, or `message`), `fixup` or `drop`. Git's
editor is never opened. "Yours" is always the work of the person at the
keyboard: in a rebase or tidy-up that is the commit being replayed (the
engine swaps git's "ours" and "theirs" there). A `Region` is `common`
or `conflict` (`yours`, `theirs`, `base`, `choice`); a `Choice` is
`yours`, `theirs`, `both` (`yours_first`) or `custom` (`text`). Each
choice rewrites the file on disk; once every conflict of a file is chosen
it is marked resolved. Binary files and delete/modify conflicts
(`whole_file_only`) take `conflictTake`.

## Shared helpers to reuse

- `h()`, `s()`, `mount()`, `icon()`, `pill()`, `checkbox()`, `toggle()`,
  `ago()`, `when()`, `plural()` from `dom.ts`. Nodes are built with these
  only: **no `innerHTML`, no `style=""` attributes** (the CSP forbids both;
  use `style: { … }` props, which go through the CSSOM). Text from git is
  set as text.
- `ctx.confirm`, `ctx.prompt`, `ctx.confirmTyped`, `ctx.compose`,
  `ctx.menu` (the dialogs and menus of `dialogs.ts`), `ctx.say` /
  `ctx.toast` / `ctx.fail` (pop-ups) and `ctx.busy(button, work)` (disables
  the button, shows a failure as a pop-up).
- `git-ui.ts`: `shortId`, `plusMinus`, and the stand-in's `placeholder` /
  `factList` (delete their use from your screen when it is real).
- `logic/diff.ts` (`diffRows`, `fileMark`, `fileCounts`) for flat diffs
  (`FileDiff`), as the pull request and old Changes views use them.
- Colours and type are the tokens of `styles/base.css` (amber: yours,
  blue: GitHub, violet: agents); buttons are `.btn`, `.btn.sm`,
  `.btn.primary`, `.btn.hot`; lists are `.table`; pills are `pill()`.
- Pure view logic worth testing goes in a function of its own, with a
  test, as `stageSummary` in `git-stage.ts` shows.

## The fixture

`scripts/git-fixture.sh <folder>` builds a repository with everything the
screens need: `feature/tunnel-ui` (current) with 5 commits that aren't
pushed over `main`; `feature/backoff-rework`, which conflicts with it in
`internal/tunnel/supervisor.go` and `cmd/atelier-desktop/tray.go`;
`feature/ssh` (1 ahead, 1 behind `github/feature/ssh`); 3 changed files (2
staged, 1 not, the last with two chunks); 2 put-aside entries; tags `v0.9`
(annotated) and `v0.8`; the remotes `studio` and `github` (bare
repositories beside it in `<folder>.remotes/`); and a linked worktree on
`fix/setup-race` at `<folder>.worktrees/fix-setup-race`. The real git
configuration and its hooks never apply.

- In Rust tests: `let repo = world.git_fixture("DemiInfinity/Tunnel");`
  (`tests/common`), then `world.desk.<area>_…(&path)`. `tests/gitui.rs`
  shows every area's commands on it.
- By hand: `ATELIER_TRY_GIT=1 scripts/try-with-mock.sh` adds it to the
  throwaway home as `~/Atelier/DemiInfinity/Tunnel` ("Other folders" in the
  sidebar). To get a conflict: put the changes aside, then merge
  `feature/backoff-rework` (or rebase onto it).
- In the self-test: `ATELIER_TRY_GIT=1 scripts/try-with-mock.sh selftest`
  runs `gitClientWalk` (`selftest.ts`), which shows every new screen on the
  fixture (with `setGitPreview(true)`) and prints `[selftest] git <name>: …`.
  Add your area's steps there through `findGitFixture()`.
