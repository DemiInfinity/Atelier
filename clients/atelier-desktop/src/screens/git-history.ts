// The History tab of the git client (area "history"): the graph of every
// branch (or one) with search and filters, the selected commit with its
// files, and the commit menu (new branch from here, tag, cherry-pick,
// revert, undo commits back to here, reword / squash → Tidy up commits).
// The board is DeskGraph. Commands: `historyApi`; the menu's actions use
// `branchesApi` / `flowApi` and go through `host.nav` (`nav.after`,
// `nav.openResetDialog`, `nav.openTidyUp`, `nav.openFileHistory`).
//
// This is the stand-in. While READY is false the repository screen keeps
// showing the History tab that was there before (repo-history.ts).

import "../styles/git-history.css";

import { historyApi } from "../api";
import { plural } from "../dom";
import { factList, placeholder } from "../git-ui";
import type { RepoHost, TabView } from "./repo";

/** Set to true when this screen replaces the old History tab. */
export const READY = false;

/** `commit`: the commit to select when the tab opens. */
export function gitHistoryTab(host: RepoHost, opts: { commit?: string } = {}): TabView {
  let disposed = false;
  const facts = factList();
  const el = placeholder({
    area: "history",
    title: "History",
    about: "The graph of every branch, with search and the commit menu. This screen is being built.",
    facts: facts.el,
  });

  async function reload(): Promise<void> {
    try {
      const page = await historyApi.graph(host.path, { limit: 12 });
      if (disposed) return;
      facts.set([
        `${plural(page.rows.length, "commit", "commits")} shown${page.more ? ", more below" : ""} · ${plural(page.columns, "lane", "lanes")}`,
        ...page.rows.map((row) => `${row.short} · column ${row.column} · ${row.subject}${row.id === opts.commit || (row.is_head && !opts.commit) ? " ←" : ""}`),
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
