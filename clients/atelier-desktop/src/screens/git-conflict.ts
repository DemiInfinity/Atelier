// Resolve conflicts (area "flow"): one view for every cause (merge, rebase,
// tidy-up, pull, cherry-pick, revert, applying put-aside changes, changes
// carried across a switch, a pull request's update). Files that need you
// and files resolved automatically; theirs and yours side by side with
// "Use this" / "Use both" per conflict; the result below. The board is
// DeskConflict. Commands: `flowApi.conflicts`, `.conflictFile`,
// `.conflictChoose`, `.conflictTake`, `.conflictMark`, `.conflictReopen`,
// `.continue`, `.skip`, `.abort`. "Stop and go back" is `flowApi.abort`
// then `host.nav.back()`; the finish button is `flowApi.continue` then
// `host.nav.after(outcome, cause)`.
//
// This is a stand-in; there was nothing here before.

import "../styles/git-flow.css";

import { flowApi } from "../api";
import { h } from "../dom";
import { factList, placeholder } from "../git-ui";
import type { ConflictCause } from "../routes";
import type { RepoHost, TabView } from "./repo";

/** `cause`: what led here, when the caller knows; the state says it too. */
export function gitConflictView(host: RepoHost, opts: { cause?: ConflictCause } = {}): TabView {
  let disposed = false;
  const facts = factList();
  const stop: HTMLButtonElement = h("button", {
    type: "button",
    class: "btn sm",
    text: "Stop and go back",
    onclick: () =>
      void host.ctx.busy(stop, async () => {
        const outcome = await flowApi.abort(host.path);
        host.ctx.say(outcome.summary);
        host.nav.back();
      }),
  });
  const el = placeholder({
    area: "flow",
    title: "Resolve conflicts",
    about: "Choose theirs, yours or both for each conflict, then finish. This view is being built.",
    facts: facts.el,
    actions: stop,
  });

  async function reload(): Promise<void> {
    try {
      const list = await flowApi.conflicts(host.path);
      if (disposed) return;
      if (list.state.kind === "none") {
        facts.set(["Nothing is under way here."]);
        stop.disabled = true;
        return;
      }
      stop.disabled = false;
      facts.set([
        `${list.state.title} — ${list.total - list.done} of ${list.total} need you`,
        ...(opts.cause ? [`Cause: ${opts.cause}`] : []),
        ...list.needs_you.map((f) => `${f.resolved ? "✓" : "!"} ${f.path} · ${f.resolved ? `resolved · ${f.how}` : f.whole_file_only ? "choose a whole file" : `${f.regions - f.chosen} of ${f.regions} left`}`),
        `Resolved automatically (${list.auto_resolved.length})`,
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
