// Looking after a repository from wherever it is shown (the sidebar's "…"
// menu, the repository bar): pin it, rename it, delete it from the studio.

import { api, asError } from "./api";
import type { Ctx, MenuItem } from "./ctx";

/** What these actions need to know about a working copy. */
export interface Managed {
  path: string;
  name: string;
  /** `owner/name` in the studio; null when it isn't there. */
  full_name: string | null;
  pinned: boolean;
}

/** What to do after one of the actions changed something. */
export type After = (what: "pinned" | "renamed" | "deleted") => void | Promise<void>;

export async function setPinned(ctx: Ctx, repo: Managed, on: boolean, after?: After): Promise<void> {
  await ctx.busy(null, async () => {
    await api.repoSetPinned(repo.path, on);
    await ctx.refresh();
    await after?.("pinned");
  });
}

/** The page of the studio where a repository is renamed or deleted. */
export function settingsPath(fullName: string): string {
  return `/${fullName}/settings`;
}

/** Where the owner lets a device rename and delete repositories. */
export const DEVICES_PATH = "/settings/devices";

export interface Refusal {
  title: string;
  body: string;
  confirm: string;
  /** The page of the studio the button opens. */
  webPath: string;
}

/**
 * What to say when the studio won't let a rename or delete happen from
 * here, and where it leads; null for any other failure.
 */
export function refusal(err: { kind: string; message: string; hint: string | null }, fullName: string): Refusal | null {
  if (err.kind === "device_not_allowed") {
    return {
      title: err.message,
      body: err.hint ?? "You can allow it in your studio, under Settings → Devices.",
      confirm: "Open Devices settings",
      webPath: DEVICES_PATH,
    };
  }
  if (err.kind === "not_allowed") {
    return { title: err.message, body: err.hint ?? "You can do it in the browser.", confirm: "Open in browser", webPath: settingsPath(fullName) };
  }
  return null;
}

async function showRefusal(ctx: Ctx, words: Refusal): Promise<void> {
  const open = await ctx.confirm({ title: words.title, body: words.body, confirm: words.confirm });
  if (open) await api.openWeb(words.webPath).catch((e) => ctx.fail(e));
}

/**
 * True when this device may rename and delete. Asks the studio now (the
 * switch may have just been turned on); when it is off, says so before
 * anything is typed and offers the Devices page.
 */
async function allowed(ctx: Ctx): Promise<boolean> {
  let app = ctx.app();
  if (!app.can_admin) app = await ctx.reloadApp(true).catch(() => app);
  if (app.can_admin) return true;
  await showRefusal(ctx, {
    title: `This ${app.machine} isn’t allowed to rename or delete repositories yet.`,
    body: "You can allow it in your studio, under Settings → Devices, or do it in the browser.",
    confirm: "Open Devices settings",
    webPath: DEVICES_PATH,
  });
  return false;
}

/**
 * Runs a rename or delete. When the studio doesn't let it happen from
 * here, says so plainly and offers the right page in the browser.
 */
async function attempt(ctx: Ctx, fullName: string, work: () => Promise<void>): Promise<void> {
  try {
    await work();
  } catch (e) {
    const words = refusal(asError(e), fullName);
    if (words) await showRefusal(ctx, words);
    else ctx.fail(e);
  }
}

/** Asks for the new name, renames it in the studio and moves the folder's remotes along. */
export async function renameRepo(ctx: Ctx, repo: Managed, after?: After): Promise<void> {
  const fullName = repo.full_name;
  if (!fullName || !(await allowed(ctx))) return;
  const name = (
    await ctx.prompt({
      title: `Rename ${repo.name}`,
      label: "New name",
      value: repo.name,
      confirm: "Rename",
      mono: true,
    })
  )?.trim();
  if (!name || name === repo.name) return;
  await attempt(ctx, fullName, async () => {
    const renamed = await api.repoRename(repo.path, name);
    await ctx.refresh();
    ctx.say(`Renamed to ${renamed.name}.`, renamedLine(renamed.remotes, renamed.remote_error));
    await after?.("renamed");
  });
}

/** What happened to the folder's remotes after a rename, in a sentence. */
export function renamedLine(remotes: readonly string[], error: string | null): string {
  if (error) return `The folder's remote couldn't be updated: ${error}`;
  if (remotes.length === 0) return "The folder here needed no change.";
  if (remotes.length === 1) return `This folder's remote “${remotes[0]}” points at the new address.`;
  return `This folder's remotes ${remotes.join(", ")} point at the new address.`;
}

/** What the delete dialog says. The folder on this machine is never touched. */
export function deleteWords(repo: { name: string; full_name: string }, machine: string) {
  return {
    title: `Delete ${repo.name} from your studio?`,
    body: [
      `${repo.full_name} is deleted from your studio for good: its code there, pull requests, issues and runs. This can't be undone.`,
      `The folder on this ${machine} is kept exactly as it is. Afterwards it shows under “Other folders”.`,
    ],
    expect: repo.name,
    confirm: "Delete from studio",
  };
}

export async function deleteRepo(ctx: Ctx, repo: Managed, after?: After): Promise<void> {
  const fullName = repo.full_name;
  if (!fullName || !(await allowed(ctx))) return;
  const machine = ctx.app().machine;
  const typed = await ctx.confirmTyped(deleteWords({ name: repo.name, full_name: fullName }, machine));
  if (typed === null) return;
  await attempt(ctx, fullName, async () => {
    const deleted = await api.repoDelete(repo.path, typed);
    await ctx.refresh();
    ctx.say(`Deleted ${deleted} from your studio.`, `The folder on this ${machine} is still here, under “Other folders”.`);
    await after?.("deleted");
  });
}

/** The "…" menu for a working copy. */
export function manageMenu(ctx: Ctx, repo: Managed, after?: After): MenuItem[] {
  const items: MenuItem[] = [
    {
      label: repo.pinned ? "Unpin" : "Pin to the top",
      pick: () => void setPinned(ctx, repo, !repo.pinned, after),
    },
  ];
  if (repo.full_name) {
    items.push(
      { separator: true },
      { label: "Rename…", pick: () => void renameRepo(ctx, repo, after) },
      { label: "Delete from studio…", pick: () => void deleteRepo(ctx, repo, after) },
    );
  }
  return items;
}
