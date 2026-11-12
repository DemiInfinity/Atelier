// The screens of the main window.

import type { PlanStep } from "./api";

/** A repository's tabs, in the order they are shown. History comes first and is where a repository opens. */
export type RepoTab = "history" | "changes" | "pulls" | "issues" | "branches" | "actions";
export const DEFAULT_REPO_TAB: RepoTab = "history";
export type AddTab = "publish" | "clone" | "new" | "open";

/** The sections of the Branches tab. */
export type BranchSection = "branches" | "tags" | "put-aside" | "remotes" | "worktrees";

/** What a conflict came from: it decides the words around Resolve conflicts. */
export type ConflictCause = "merge" | "rebase" | "tidy" | "pull" | "cherry-pick" | "revert" | "stash" | "switch" | "update";

/**
 * A view of the git client that isn't a tab of its own: inside History
 * (one file's history, blame), inside Branches (worktrees), or a focused
 * work view that can be stopped (tidy up commits, resolve conflicts).
 */
export type GitView =
  | { kind: "file-history"; file: string; at?: string }
  | { kind: "blame"; file: string; at?: string }
  | { kind: "worktrees" }
  | { kind: "tidy"; base?: string; plan?: PlanStep[] }
  | { kind: "conflicts"; cause?: ConflictCause };

/** The tab a view belongs to (where its bar tab stays lit). */
export function tabOfView(view: GitView): RepoTab {
  switch (view.kind) {
    case "file-history":
    case "blame":
    case "tidy":
      return "history";
    case "worktrees":
      return "branches";
    case "conflicts":
      return "changes";
  }
}

export type Route =
  | { screen: "today" }
  | { screen: "pulls"; repo?: string; number?: number }
  /**
   * `view`: open on a view of the git client. `commit`: History with this
   * commit selected. `section`: the Branches tab on this section. `file`:
   * Changes with this file selected.
   */
  | { screen: "repo"; path: string; tab?: RepoTab; view?: GitView; commit?: string; section?: BranchSection; file?: string }
  /** `approve`: scroll to and highlight this waiting agent. */
  | { screen: "device"; approve?: number }
  /** `path`: a folder already known to the app, to publish (skips the chooser). */
  | { screen: "add"; tab?: AddTab; path?: string }
  | { screen: "settings" }
  | { screen: "messages" };

/** Which sidebar entry is lit for a route. */
export function sidebarKey(route: Route): string {
  switch (route.screen) {
    case "repo":
      return `repo:${route.path}`;
    case "messages":
      return "settings";
    case "add":
      return "none";
    default:
      return route.screen;
  }
}
