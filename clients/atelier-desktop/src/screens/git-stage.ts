// The Changes tab of the git client (area "stage"): files split into
// Staged and Not staged, the selected file's diff as chunks with a tick per
// line, Stage / Unstage / Discard for a file, a chunk or the ticked lines,
// the commit form with "Amend last commit", and "Put aside these changes…".
// The board is DeskStage. Commands: `stageApi` (and `branchesApi.stashSave`
// for put aside). Leaving the tab goes through `host.nav`.
//
// This is the stand-in: it proves the wiring and shows what the commands
// answer. While READY is false the repository screen keeps showing the
// Changes tab that was there before (repo-changes.ts).

import "../styles/git-stage.css";

import { stageApi, type StageView } from "../api";
import { plural } from "../dom";
import { factList, placeholder, plusMinus } from "../git-ui";
import type { RepoHost, TabView } from "./repo";

/** Set to true when this screen replaces the old Changes tab. */
export const READY = false;

/** `2 staged · 1 not staged`, or `Nothing to commit`. */
export function stageSummary(view: Pick<StageView, "staged" | "unstaged" | "conflicted">): string {
  const parts: string[] = [];
  if (view.staged.length > 0) parts.push(`${view.staged.length} staged`);
  if (view.unstaged.length > 0) parts.push(`${view.unstaged.length} not staged`);
  if (view.conflicted.length > 0) parts.push(`${plural(view.conflicted.length, "conflict", "conflicts")}`);
  return parts.length > 0 ? parts.join(" · ") : "Nothing to commit";
}

/** `file`: the file to select when the tab opens. */
export function gitStageTab(host: RepoHost, opts: { file?: string } = {}): TabView {
  let disposed = false;
  const facts = factList();
  const el = placeholder({
    area: "stage",
    title: "Changes",
    about: "Stage by file, chunk or line, then commit. This screen is being built.",
    facts: facts.el,
  });

  async function reload(): Promise<void> {
    try {
      const view = await stageApi.status(host.path);
      if (disposed) return;
      const line = (side: string) => (e: StageView["staged"][number]) => `${side}: ${e.path} ${plusMinus(e.additions, e.deletions)}`.trim();
      facts.set([
        stageSummary(view),
        ...view.staged.map(line("Staged")),
        ...view.unstaged.map(line("Not staged")),
        ...(opts.file ? [`Selected: ${opts.file}`] : []),
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
