// The careful-action dialogs (area "flow"): discarding uncommitted work,
// undoing commits (reset), force pushing safely, what to do with changes
// when switching, Recent actions and Recently discarded.
//
// Owned by the flow area. The signatures are the contract `git-nav.ts`
// calls; the bodies below are stand-ins built on the plain confirm dialog,
// with the words of the DeskSafety board. Replace them with the real
// dialogs (three choices for reset and for switch, the lists) and set
// READY.

import { flowApi, type ForcePushPreview, type ResetMode, type ResetPreview } from "./api";
import type { Ctx } from "./ctx";
import { plural } from "./dom";
import type { DiscardRequest, GitNav, NavHost, SwitchRequest } from "./git-nav";

export const READY = false;

/** "Discard changes in 3 files?" Resolves true to go ahead. */
export function discardDialog(ctx: Ctx, request: DiscardRequest): Promise<boolean> {
  const lines: string[] = [];
  if (request.additions > 0) lines.push(`${plural(request.additions, "line", "lines")} added`);
  if (request.deletions > 0) lines.push(`${request.deletions} removed`);
  const lost = lines.length > 0 ? `: ${lines.join(", ")}` : "";
  return ctx.confirm({
    title: `Discard changes in ${request.what}?`,
    body: `You lose your uncommitted edits to ${request.what}${lost}. Commits are not touched. A copy goes to Recently discarded for 30 days, so you can bring it back.`,
    confirm: "Discard",
    danger: true,
  });
}

/**
 * "Undo commits back to 7f82d31": what should happen to the work in them.
 * Resolves to the choice, or null when cancelled. (Stand-in: offers only
 * "Keep my changes, unstaged".)
 */
export async function resetDialog(ctx: Ctx, preview: ResetPreview): Promise<ResetMode | null> {
  const branch = preview.branch ?? "HEAD";
  const where =
    preview.pushed === 0
      ? `${preview.commits === 1 ? "It is" : `All ${preview.commits} are`} only on this ${ctx.app().machine}.`
      : `${preview.pushed} of them ${preview.pushed === 1 ? "is" : "are"} already pushed.`;
  const sure = await ctx.confirm({
    title: `Undo commits back to ${preview.target_short}`,
    body: `${branch} steps back ${plural(preview.commits, "commit", "commits")}. ${where} The work stays in your files, ready to pick through again.`,
    confirm: `Undo ${plural(preview.commits, "commit", "commits")}`,
  });
  return sure ? "mixed" : null;
}

/** "Force push safely". Resolves true to go ahead. */
export function forcePushDialog(ctx: Ctx, preview: ForcePushPreview): Promise<boolean> {
  return ctx.confirm({
    title: "Force push safely",
    body: `This replaces ${plural(preview.replaced, "commit", "commits")} on ${preview.remote} with ${preview.new} new ${preview.new === 1 ? "one" : "ones"} on ${preview.remote_branch}. It stops if someone pushed in between (with lease).`,
    confirm: "Force push",
    danger: true,
  });
}

/**
 * Switching with uncommitted changes: bring them along, put them aside, or
 * cancel (null). (Stand-in: offers only "put them aside".)
 */
export async function switchDialog(ctx: Ctx, request: SwitchRequest): Promise<"carry" | "put_aside" | null> {
  const sure = await ctx.confirm({
    title: `Switch to ${request.branch}?`,
    body: `You have ${plural(request.changes, "uncommitted change", "uncommitted changes")}. They are put aside first; bring them back from Branches › Put aside.`,
    confirm: "Put aside and switch",
  });
  return sure ? "put_aside" : null;
}

/** Recent actions: what Undo steps back through, each with "Go back to before this". */
export function recentActionsDialog(host: NavHost, nav: GitNav): void {
  void nav;
  void host.ctx.busy(null, async () => {
    const entries = await flowApi.journal(host.path);
    const latest = entries[0];
    host.ctx.say(
      latest ? `${plural(entries.length, "recent action", "recent actions")}` : "No recent actions yet.",
      latest ? `Latest: ${latest.title}` : "",
    );
  });
}

/** Recently discarded: thrown-away changes, kept for 30 days, each with "Bring back". */
export function recentlyDiscardedDialog(host: NavHost, nav: GitNav): void {
  void nav;
  void host.ctx.busy(null, async () => {
    const entries = await flowApi.discarded(host.path);
    const latest = entries[0];
    host.ctx.say(
      latest ? `${plural(entries.length, "entry", "entries")} in Recently discarded` : "Nothing was discarded lately.",
      latest ? `Latest: ${latest.title}` : "",
    );
  });
}
