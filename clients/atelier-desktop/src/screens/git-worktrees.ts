// Worktrees (area "branches"), the view inside the Branches tab: the
// repository's folders, "New worktree…" (from a branch, a pull request's
// ref or a new branch; the folder suggested beside the repository), Open,
// Remove… (it says why it can't when the folder has changes), lock.
// The board is DeskWorktrees. Commands: `branchesApi.worktree*`. "Open" is
// `host.ctx.go({ screen: "repo", path: worktree.path })`: a worktree opens
// on History like any repository. "Open in editor" / "Reveal" are
// `api.repoOpen(worktree.path, preset)`.
//
// This is a stand-in; there was nothing here before.

import "../styles/git-branches.css";

import { branchesApi } from "../api";
import { h, plural } from "../dom";
import { factList, placeholder } from "../git-ui";
import type { RepoHost, TabView } from "./repo";

export function gitWorktreesView(host: RepoHost): TabView {
  let disposed = false;
  const facts = factList();
  const el = placeholder({
    area: "branches",
    title: "Worktrees",
    about: "This same repository open in a second folder on another branch. This view is being built.",
    facts: facts.el,
    actions: h("button", { type: "button", class: "btn sm", text: "‹ Branches", onclick: () => host.nav.openBranches() }),
  });

  async function reload(): Promise<void> {
    try {
      const trees = await branchesApi.worktrees(host.path);
      if (disposed) return;
      facts.set([
        plural(trees.length, "worktree", "worktrees"),
        ...trees.map(
          (t) =>
            `${t.display_path} · ${t.branch ?? t.head_short ?? "detached"} · ${t.dirty > 0 ? plural(t.dirty, "change", "changes") : "clean"}${t.main ? " · main folder" : ""}${t.locked ? ` · locked${t.lock_reason ? `: ${t.lock_reason}` : ""}` : ""}`,
        ),
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
