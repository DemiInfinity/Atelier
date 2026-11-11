// Small decisions of the pull requests screen, kept apart so they can be
// tested without a window.

import type { PullRow } from "../api";

export interface PullKey {
  repo: string;
  number: number;
}

/** `owner/name` → `name`, for when no row tells us the short name. */
export function shortRepo(repo: string): string {
  const at = repo.lastIndexOf("/");
  return at >= 0 ? repo.slice(at + 1) : repo;
}

/** The rows the list shows: all of them, or one repository's. */
export function visiblePulls(rows: readonly PullRow[], onlyRepo: string | null): PullRow[] {
  return onlyRepo ? rows.filter((row) => row.repo === onlyRepo) : [...rows];
}

/**
 * Which pull request is open on the right. What the person picked stays
 * (even when it left the list, as a pull request that was just merged does);
 * otherwise the first one shown.
 */
export function pickPull(visible: readonly PullRow[], current: PullKey | null): PullKey | null {
  if (current) return current;
  const first = visible[0];
  return first ? { repo: first.repo, number: first.number } : null;
}

/** The merge method to offer: the remembered one when this pull request allows it, else the first. */
export function pickMethod<M extends { id: string }>(methods: readonly M[], remembered: string | null): M | null {
  return methods.find((m) => m.id === remembered) ?? methods[0] ?? null;
}
