// Small decisions of the menu bar popover, kept apart so they can be
// tested without a window.

import type { NeedAction, Target, WorkingCopy } from "../api";

/** What a "needs you" button does from the popover. */
export type PopoverStep =
  /** Push right here; no window needed. */
  | { do: "push"; path: string }
  /** Bring the main window up on a screen (or a page of the studio). */
  | { do: "show"; target: Target };

export function popoverStep(action: NeedAction): PopoverStep {
  switch (action.type) {
    case "push":
      return { do: "push", path: action.path };
    case "approve_agent":
      // Approving shows the tools and limits first, on "This device".
      return { do: "show", target: { screen: "device" } };
    case "open_pull":
      return { do: "show", target: { screen: "pull", repo: action.repo, number: action.number } };
    case "open_log":
    case "open_web":
      return { do: "show", target: { screen: "web", web_path: action.web_path } };
  }
}

/** Lower comes first: work in progress, then work to share, then work to fetch. */
function urgency(copy: WorkingCopy): number {
  if (copy.changes > 0) return 0;
  if (copy.ahead > 0) return 1;
  if (copy.behind > 0) return 2;
  return 3;
}

/** The working copies under "Jump back in": the ones with something going on first, in their own order otherwise. */
export function jumpBackIn(copies: readonly WorkingCopy[], max = 3): WorkingCopy[] {
  return copies
    .map((copy, at) => ({ copy, at, rank: urgency(copy) }))
    .sort((a, b) => a.rank - b.rank || a.at - b.at)
    .slice(0, Math.max(0, max))
    .map((entry) => entry.copy);
}

/** `48s`, `3m`, `1h 05m`: the running card has little room. */
export function shortElapsed(iso: string, now: Date = new Date()): string {
  const then = new Date(iso);
  if (Number.isNaN(then.getTime())) return "";
  const seconds = Math.max(0, Math.round((now.getTime() - then.getTime()) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  return `${Math.floor(minutes / 60)}h ${String(minutes % 60).padStart(2, "0")}m`;
}
