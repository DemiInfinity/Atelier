// The Branches tab of the git client (area "branches"): branches here and
// on each remote, tags, put aside (stashes), remotes, and the way into
// Worktrees. The board is DeskBranches. Commands: `branchesApi`. Switch is
// `host.nav.switchTo(name)` (it asks about uncommitted changes); merge and
// rebase are `flowApi.merge` / `.rebase` followed by `host.nav.after(…)`;
// force push is `host.nav.openForcePushDialog()`; "Tidy up commits…" is
// `host.nav.openTidyUp()`; worktrees are `host.nav.openWorktrees()`.
//
// This is the stand-in. While READY is false the repository screen keeps
// showing the Branches tab that was there before (repo-branches.ts).

import "../styles/git-branches.css";

import { branchesApi } from "../api";
import { h } from "../dom";
import { factList, placeholder } from "../git-ui";
import type { BranchSection } from "../routes";
import type { RepoHost, TabView } from "./repo";

/** Set to true when this screen replaces the old Branches tab. */
export const READY = false;

/** `section`: the section to show when the tab opens. */
export function gitBranchesTab(host: RepoHost, opts: { section?: BranchSection } = {}): TabView {
  let disposed = false;
  const facts = factList();
  const el = placeholder({
    area: "branches",
    title: "Branches",
    about: "Branches, tags, put aside, remotes and worktrees. This screen is being built.",
    facts: facts.el,
    actions: h("button", { type: "button", class: "btn sm", text: "Worktrees ›", onclick: () => host.nav.openWorktrees() }),
  });

  async function reload(): Promise<void> {
    try {
      const [list, tags, stashes, remotes] = await Promise.all([
        branchesApi.list(host.path),
        branchesApi.tags(host.path),
        branchesApi.stashes(host.path),
        branchesApi.remotes(host.path),
      ]);
      if (disposed) return;
      facts.set([
        `Section: ${opts.section ?? "branches"}`,
        ...list.local.map((b) => `${b.current ? "● " : ""}${b.name} · ↑${b.ahead} ↓${b.behind} · ${b.upstream ?? "not on a remote yet"}`),
        ...list.remotes.map((g) => `${g.label}: ${g.branches.map((b) => b.name).join(", ")}`),
        `Tags: ${tags.map((t) => t.name).join(", ") || "none"}`,
        `Put aside: ${stashes.map((s) => s.message).join(", ") || "nothing"}`,
        `Remotes: ${remotes.map((r) => r.name).join(", ") || "none"}`,
      ]);
    } catch (e) {
      if (!disposed) facts.fail(e);
    }
  }

  return {
    el,
    reload,
    dispose() {
      disposed = true;
    },
  };
}
