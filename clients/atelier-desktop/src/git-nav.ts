// How the git client's screens lead to each other. Screens never mount one
// another: they ask `host.nav`. That keeps the four areas in their own
// files, and keeps the three rules of the flow in one place:
//
//   1. every repository opens on History;
//   2. every heavy action (tidy up commits, resolve conflicts) can be
//      stopped, and stopping returns to where you were (`back`);
//   3. every finish lands on History with the Undo bar (`finish`).
//
// docs/git-client.md has the whole contract.

import { branchesApi, flowApi, type ForcePushPreview, type OpOutcome, type PlanStep, type ResetMode, type ResetPreview } from "./api";
import type { Ctx } from "./ctx";
import { discardDialog, forcePushDialog, recentActionsDialog, recentlyDiscardedDialog, resetDialog, switchDialog } from "./git-dialogs";
import { tabOfView, type BranchSection, type ConflictCause, type GitView, type RepoTab } from "./routes";
import type { UndoBar } from "./undo";

// ---------------------------------------------------------------- readiness

let preview = false;

/**
 * Shows every area's new screen whether or not it says it is ready. For
 * the self-test and for working on a screen before switching it on.
 */
export function setGitPreview(on: boolean): void {
  preview = on;
}

/**
 * Whether an area's new screen is used. Each screen module exports
 * `READY`; while it is false the screen that was there before stays.
 */
export function gitReady(flag: boolean): boolean {
  return flag || preview;
}

// ------------------------------------------------------------------- places

/** Where the repository screen is: a tab, maybe with a view over it. */
export interface Place {
  tab: RepoTab;
  view: GitView | null;
  /** History: the commit to select. */
  commit?: string;
  /** Branches: the section to show. */
  section?: BranchSection;
  /** Changes: the file to select. */
  file?: string;
}

/** What the repository screen gives the navigation. */
export interface NavHost {
  ctx: Ctx;
  path: string;
  place(): Place;
  show(place: Place): void;
  /** After anything that changed the repository: the bar, the tab, the sidebar. */
  reloadAll(): Promise<void>;
  /** The Undo bar, when the flow area is switched on. */
  undoBar(): UndoBar | null;
}

/** What is being thrown away, for the dialog that asks first. */
export interface DiscardRequest {
  /** `3 files`, `internal/tunnel/supervisor.go`, `3 lines of supervisor.go`. */
  what: string;
  files: number;
  additions: number;
  deletions: number;
}

export interface SwitchRequest {
  branch: string;
  /** Uncommitted files. */
  changes: number;
}

export interface GitNav {
  /** History; with `commit`, that commit selected. */
  openHistory(commit?: string): void;
  /** One file's history, inside History. */
  openFileHistory(file: string, at?: string): void;
  /** Blame of a file (at a commit, or the working tree), inside History. */
  openBlame(file: string, at?: string): void;
  /** Changes; with `file`, that file selected. */
  openChanges(file?: string): void;
  openBranches(section?: BranchSection): void;
  /** Worktrees, inside Branches. */
  openWorktrees(): void;
  /** Tidy up commits. Heavy: `back()` returns here. */
  openTidyUp(request?: { base?: string; plan?: PlanStep[] }): void;
  /** Resolve conflicts. Heavy: `back()` returns here. */
  openConflicts(cause?: ConflictCause): void;

  /** Stopped: go back to where the heavy action was started from. */
  back(): void;
  /** Finished: land on History with the Undo bar saying `text`. */
  finish(text: string, journal?: number | null, commit?: string): void;
  /**
   * What to do with the answer of any action that may stop at conflicts
   * (merge, rebase, pull, cherry-pick, revert, apply put-aside, tidy):
   * not done → Resolve conflicts; done → `finish`.
   */
  after(outcome: OpOutcome, cause: ConflictCause): void;

  showUndoBar(text: string, journal?: number | null): void;
  openRecentActions(): void;
  openRecentlyDiscarded(): void;

  /** Asks before uncommitted work is thrown away. The caller then discards. */
  openDiscardDialog(request: DiscardRequest): Promise<boolean>;
  /** "Undo commits back to here": shows what it costs, asks how, resets, and finishes on History. */
  openResetDialog(target: string): Promise<void>;
  /** "Force push safely": shows what is replaced, asks, pushes with the lease that was shown. */
  openForcePushDialog(branch?: string | null, remote?: string | null): Promise<void>;
  /** Switches branch; with uncommitted changes asks first: bring them along, put them aside, or cancel. */
  switchTo(target: string): Promise<void>;
}

const HEAVY = new Set<GitView["kind"]>(["tidy", "conflicts"]);

export function createGitNav(host: NavHost): GitNav {
  const { ctx, path } = host;
  /** Where to return to when a heavy view is stopped, innermost last. */
  const returns: Place[] = [];

  /** An ordinary move: nothing to come back to. */
  function go(place: Place): void {
    returns.length = 0;
    host.show(place);
  }

  /** A heavy view: remember where we are (once per level). */
  function enter(view: GitView): void {
    const from = host.place();
    if (from.view?.kind !== view.kind) returns.push(from);
    host.show({ tab: tabOfView(view), view });
  }

  function showUndoBar(text: string, journal?: number | null): void {
    const bar = host.undoBar();
    if (bar) bar.show(text, journal ?? null);
    else ctx.say(text);
  }

  function finish(text: string, journal?: number | null, commit?: string): void {
    returns.length = 0;
    host.show({ tab: "history", view: null, commit });
    void host.reloadAll();
    showUndoBar(text, journal);
  }

  const nav: GitNav = {
    openHistory: (commit) => go({ tab: "history", view: null, commit }),
    openFileHistory: (file, at) => go({ tab: "history", view: { kind: "file-history", file, at } }),
    openBlame: (file, at) => go({ tab: "history", view: { kind: "blame", file, at } }),
    openChanges: (file) => go({ tab: "changes", view: null, file }),
    openBranches: (section) => go({ tab: "branches", view: null, section }),
    openWorktrees: () => go({ tab: "branches", view: { kind: "worktrees" } }),
    openTidyUp: (request) => enter({ kind: "tidy", base: request?.base, plan: request?.plan }),
    openConflicts: (cause) => enter({ kind: "conflicts", cause }),

    back() {
      const to = returns.pop() ?? { tab: "history" as RepoTab, view: null };
      host.show(to);
      void host.reloadAll();
    },
    finish,
    after(outcome, cause) {
      if (outcome.done) {
        // A step of a tidy-up that finished from the conflict view goes
        // back to nothing: the tidy-up itself is done too.
        finish(outcome.summary, outcome.journal, outcome.head ?? undefined);
      } else {
        enter({ kind: "conflicts", cause });
        void host.reloadAll();
      }
    },

    showUndoBar,
    openRecentActions: () => recentActionsDialog(host, nav),
    openRecentlyDiscarded: () => recentlyDiscardedDialog(host, nav),

    openDiscardDialog: (request) => discardDialog(ctx, request),

    async openResetDialog(target) {
      await ctx.busy(null, async () => {
        const preview: ResetPreview = await flowApi.resetPreview(path, target);
        const mode: ResetMode | null = await resetDialog(ctx, preview);
        if (mode === null) return;
        const outcome = await flowApi.reset(path, preview.target, mode);
        finish(outcome.summary, outcome.journal);
      });
    },

    async openForcePushDialog(branch, remote) {
      await ctx.busy(null, async () => {
        const preview: ForcePushPreview = await branchesApi.forcePreview(path, branch, remote);
        if (preview.expected === null) {
          // Nothing there to replace: an ordinary push does it.
          await branchesApi.push(path, preview.branch, preview.remote);
        } else {
          if (!(await forcePushDialog(ctx, preview))) return;
          await branchesApi.forcePush(path, preview.expected, preview.branch, preview.remote);
        }
        ctx.say(`Pushed ${preview.branch} to ${preview.remote}.`);
        await host.reloadAll();
      });
    },

    async switchTo(target) {
      await ctx.busy(null, async () => {
        let outcome = await branchesApi.switch(path, target, "ask");
        if (outcome.needs_choice) {
          const how = await switchDialog(ctx, { branch: outcome.branch, changes: outcome.changes });
          if (how === null) return;
          outcome = await branchesApi.switch(path, target, how);
        }
        if (outcome.conflicts.length > 0) {
          enter({ kind: "conflicts", cause: "switch" });
        } else if (outcome.switched) {
          ctx.say(`Switched to ${outcome.branch}.`, outcome.put_aside ? "Your changes were put aside." : "");
        }
        await host.reloadAll();
      });
    },
  };
  return nav;
}

/** True for the views that can be stopped and return where you were. */
export function isHeavy(view: GitView | null): boolean {
  return view !== null && HEAVY.has(view.kind);
}
