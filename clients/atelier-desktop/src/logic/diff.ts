// Turns the structured diff lines from the Rust side into what the diff
// viewer draws: two line-number gutters, a mark, and the text.

import type { DiffLine, FileChange } from "../api";

export interface DiffRow {
  /** `hunk`, `ctx`, `add`, `del` or `note`: the row's class. */
  kind: "hunk" | "ctx" | "add" | "del" | "note";
  /** Old line number, or empty. */
  a: string;
  /** New line number, or empty. */
  b: string;
  /** `+`, `−` (a real minus sign) or empty. */
  mark: string;
  text: string;
}

export function diffRows(lines: DiffLine[]): DiffRow[] {
  return lines.map((line) => {
    switch (line.kind) {
      case "hunk":
        return { kind: "hunk", a: "", b: "", mark: "", text: line.text };
      case "add":
        return { kind: "add", a: "", b: num(line.new), mark: "+", text: line.text };
      case "delete":
        return { kind: "del", a: num(line.old), b: "", mark: "−", text: line.text };
      case "no_newline":
        return { kind: "note", a: "", b: "", mark: "", text: "No newline at the end of the file" };
      case "context":
        return { kind: "ctx", a: num(line.old), b: num(line.new), mark: "", text: line.text };
    }
  });
}

function num(n: number | null): string {
  return n === null ? "" : String(n);
}

/** The one-letter mark next to a changed file, and its colour class. */
export function fileMark(file: Pick<FileChange, "kind">): { letter: string; tone: "pass" | "yours" | "fail" } {
  switch (file.kind) {
    case "added":
    case "untracked":
      return { letter: "A", tone: "pass" };
    case "deleted":
      return { letter: "D", tone: "fail" };
    case "renamed":
      return { letter: "R", tone: "yours" };
    case "conflicted":
      return { letter: "!", tone: "fail" };
    case "modified":
      return { letter: "M", tone: "yours" };
  }
}

/** `+9` / `−2` for a file row; binary files have no counts. */
export function fileCounts(file: Pick<FileChange, "additions" | "deletions" | "binary">): { add: string; del: string } {
  if (file.binary) return { add: "bin", del: "" };
  return {
    add: file.additions === null ? "" : `+${file.additions}`,
    del: file.deletions === null ? "" : `−${file.deletions}`,
  };
}

/**
 * Which files start ticked: everything except conflicts, which can't be
 * committed until they are resolved.
 */
export function initialTicks(files: FileChange[], previous: ReadonlyMap<string, boolean> = new Map()): Map<string, boolean> {
  const ticks = new Map<string, boolean>();
  for (const file of files) {
    ticks.set(file.path, previous.get(file.path) ?? file.kind !== "conflicted");
  }
  return ticks;
}

/** The paths to commit, in list order. */
export function tickedPaths(files: FileChange[], ticks: ReadonlyMap<string, boolean>): string[] {
  return files.filter((f) => ticks.get(f.path) === true).map((f) => f.path);
}
