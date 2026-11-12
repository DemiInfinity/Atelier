// Worktree rows in the sidebar (area "branches"): under a repository's row,
// one row per linked worktree (its branch and state), as on the
// DeskWorktrees board. `shell.ts` calls this for every repository row it
// draws; the overview already carries the linked worktrees (a working copy
// whose `worktree_of` is the repository's folder).
//
// Owned by the branches area. While READY is false nothing is nested and
// linked worktrees stay ordinary rows of the sidebar, as before.

import type { WorkingCopy } from "./api";
import { h, type Child } from "./dom";

export const READY = false;

export interface WorktreeRowActions {
  /** The sidebar key of the screen that is showing (`repo:/path`). */
  currentKey: string;
  /** Opens that worktree as a repository (on History, like any other). */
  open(worktree: WorkingCopy): void;
}

/** The rows to draw under `copy`: its linked worktrees. */
export function worktreeRows(copy: WorkingCopy, linked: readonly WorkingCopy[], actions: WorktreeRowActions): Child {
  void copy;
  return linked.map((tree) =>
    h(
      "button",
      {
        type: "button",
        class: "side-link side-worktree",
        title: `${tree.branch} · ${tree.state}`,
        "aria-current": actions.currentKey === `repo:${tree.path}` ? "page" : undefined,
        onclick: () => actions.open(tree),
      },
      h("span", { class: "grow clip mono", text: tree.branch }),
      h("span", { class: `state tone-${tree.tone}`, text: tree.short || tree.state }),
    ),
  );
}
