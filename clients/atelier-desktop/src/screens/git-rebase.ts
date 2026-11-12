// Tidy up commits (area "flow"): an interactive rebase as a list. Oldest
// first; per commit Keep / Reword / Squash / Fixup / Drop, drag to reorder,
// "What will happen", Start. The board is DeskRebase. Commands:
// `flowApi.rebasePlan` and `flowApi.tidy`; then `host.nav.after(outcome,
// "tidy")` (conflicts open Resolve conflicts; done lands on History with
// the Undo bar). Cancel is `host.nav.back()`.
//
// This is a stand-in; there was nothing here before.

import "../styles/git-flow.css";

import { flowApi, type PlanStep } from "../api";
import { h, plural } from "../dom";
import { factList, placeholder } from "../git-ui";
import type { RepoHost, TabView } from "./repo";

/** `base`: the commit to start from; `plan`: actions already chosen (from a commit's menu). */
export function gitTidyView(host: RepoHost, opts: { base?: string; plan?: PlanStep[] } = {}): TabView {
  let disposed = false;
  const facts = factList();
  const el = placeholder({
    area: "flow",
    title: "Tidy up commits",
    about: "Reword, squash, reorder and drop commits before you push. This view is being built.",
    facts: facts.el,
    actions: h("button", { type: "button", class: "btn sm", text: "Cancel", onclick: () => host.nav.back() }),
  });

  async function reload(): Promise<void> {
    try {
      const plan = await flowApi.rebasePlan(host.path, opts.base);
      if (disposed) return;
      const chosen = new Map((opts.plan ?? []).map((step) => [step.id, step.action]));
      facts.set([
        `Tidy up ${plural(plan.commits.length, "commit", "commits")} on ${plan.branch ?? "HEAD"}`,
        `Starts from ${plan.base_name ?? "a commit"} · ${plan.base_short}, which stays exactly as it is.`,
        ...plan.commits.map((c) => `${chosen.get(c.id) ?? "keep"} · ${c.short} · ${c.subject}${c.pushed ? " · pushed" : ""}`),
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
