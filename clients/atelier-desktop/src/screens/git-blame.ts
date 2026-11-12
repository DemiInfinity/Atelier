// File history and blame (area "history"), the two views inside the History
// tab. The board is DeskBlame. Commands: `historyApi.file`, `.blame`,
// `.blameBefore`, `.fileAt`, `.commitDiff`. "Show in graph" is
// `host.nav.openHistory(commit)`; switching between the two views is
// `host.nav.openFileHistory(file, at)` / `host.nav.openBlame(file, at)`.
//
// These are stand-ins; there was nothing here before, so they are shown as
// they are until the real views replace them.

import "../styles/git-history.css";

import { historyApi } from "../api";
import { h, plural } from "../dom";
import { factList, placeholder } from "../git-ui";
import type { RepoHost, TabView } from "./repo";

/** One file's history, across renames. `at`: the commit to start from (HEAD). */
export function gitFileHistoryView(host: RepoHost, opts: { file: string; at?: string }): TabView {
  let disposed = false;
  const facts = factList();
  const el = placeholder({
    area: "history",
    title: `History of ${opts.file}`,
    about: "Every commit that changed this file, across renames. This view is being built.",
    facts: facts.el,
    actions: [
      h("button", { type: "button", class: "btn sm", text: "‹ History", onclick: () => host.nav.openHistory() }),
      h("button", { type: "button", class: "btn sm", text: "Blame", onclick: () => host.nav.openBlame(opts.file, opts.at) }),
    ],
  });

  async function reload(): Promise<void> {
    try {
      const history = await historyApi.file(host.path, opts.file, opts.at);
      if (disposed) return;
      facts.set([
        plural(history.entries.length, "commit", "commits"),
        ...history.entries.map((e) => `${e.short} · ${e.subject}${e.old_path ? ` · was ${e.old_path}` : ""}`),
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

/** Who last changed each line. `at`: a commit; the working tree when left out. */
export function gitBlameView(host: RepoHost, opts: { file: string; at?: string }): TabView {
  let disposed = false;
  const facts = factList();
  const el = placeholder({
    area: "history",
    title: `Blame of ${opts.file}`,
    about: "Who last changed each line, with “Blame before this change”. This view is being built.",
    facts: facts.el,
    actions: [
      h("button", { type: "button", class: "btn sm", text: "‹ History", onclick: () => host.nav.openHistory() }),
      h("button", { type: "button", class: "btn sm", text: "File history", onclick: () => host.nav.openFileHistory(opts.file, opts.at) }),
    ],
  });

  async function reload(): Promise<void> {
    try {
      const blame = await historyApi.blame(host.path, opts.file, opts.at);
      if (disposed) return;
      facts.set([
        `${plural(blame.lines, "line", "lines")} in ${plural(blame.runs.length, "run", "runs")}`,
        ...blame.runs.slice(0, 20).map((r) => `${r.short} · ${r.author} · lines ${r.start}–${r.end} · ${r.summary}`),
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
