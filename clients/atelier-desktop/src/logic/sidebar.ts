// How the sidebar's repositories are grouped: pinned ones on top, then one
// group per owner (the personal account first, companies after it by name).

import type { WorkingCopy } from "../api";

/** The key of the "Pinned" group in the remembered folds. */
export const PINNED_KEY = ":pinned";

export interface SideGroup {
  /** What the fold is remembered under: the account name in lower case. */
  key: string;
  /** `Pinned`, `DemiInfinity`, `Elyndra Studios LTD`. */
  label: string;
  kind: "pinned" | "user" | "company" | "other";
  copies: WorkingCopy[];
  collapsed: boolean;
}

/** The key a working copy's owner group is remembered under. */
export function ownerKey(copy: Pick<WorkingCopy, "owner" | "owner_name">): string {
  return (copy.owner_name || copy.owner || "other").toLowerCase();
}

/**
 * The groups, in order. `copies` are the studio's repositories on this
 * machine, `others` the folders it doesn't know; a pinned one of either
 * kind moves to "Pinned" and leaves its own group.
 */
export function sidebarGroups(
  copies: readonly WorkingCopy[],
  others: readonly WorkingCopy[],
  collapsed: readonly string[],
): SideGroup[] {
  const folded = (key: string) => collapsed.includes(key);
  const groups: SideGroup[] = [];
  const pinned = [...copies, ...others].filter((copy) => copy.pinned);
  if (pinned.length > 0) {
    groups.push({ key: PINNED_KEY, label: "Pinned", kind: "pinned", copies: pinned, collapsed: folded(PINNED_KEY) });
  }
  const owners = new Map<string, SideGroup>();
  for (const copy of copies) {
    if (copy.pinned) continue;
    const key = ownerKey(copy);
    let group = owners.get(key);
    if (!group) {
      const kind = copy.owner_kind === "user" ? "user" : copy.owner_kind === "company" ? "company" : "other";
      group = { key, label: copy.owner || "Other folders", kind, copies: [], collapsed: folded(key) };
      owners.set(key, group);
    }
    group.copies.push(copy);
  }
  const rank = (group: SideGroup) => (group.kind === "user" ? 0 : group.kind === "company" ? 1 : 2);
  groups.push(
    ...[...owners.values()].sort((a, b) => rank(a) - rank(b) || a.label.toLowerCase().localeCompare(b.label.toLowerCase())),
  );
  return groups;
}

/** The remembered folds after one group was opened or shut. */
export function toggled(collapsed: readonly string[], key: string, shut: boolean): string[] {
  const rest = collapsed.filter((k) => k !== key);
  return shut ? [...rest, key] : rest;
}
