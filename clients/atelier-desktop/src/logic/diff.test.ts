import { describe, expect, it } from "vitest";
import type { DiffLine, FileChange } from "../api";
import { diffRows, fileCounts, fileMark, initialTicks, tickedPaths } from "./diff";

const line = (kind: DiffLine["kind"], old: number | null, neu: number | null, text: string): DiffLine => ({
  kind,
  old,
  new: neu,
  text,
});

const file = (path: string, kind: FileChange["kind"], extra: Partial<FileChange> = {}): FileChange => ({
  path,
  old_path: null,
  kind,
  staged: false,
  additions: 1,
  deletions: 0,
  binary: false,
  ...extra,
});

describe("diff rendering", () => {
  it("numbers each side and marks adds and deletes", () => {
    const rows = diffRows([
      line("hunk", null, null, "@@ -41,3 +41,4 @@ func build() {"),
      line("context", 41, 41, "\tm := NewMenu()"),
      line("delete", 42, null, '\tm.Add(Item{Label: "Quit"})'),
      line("add", null, 42, "\tm.AddSeparator()"),
      line("add", null, 43, '\tm.Add(Item{Label: "Quit", Do: t.quit})'),
      line("context", 43, 44, "}"),
    ]);
    expect(rows.map((r) => [r.kind, r.a, r.b, r.mark])).toEqual([
      ["hunk", "", "", ""],
      ["ctx", "41", "41", ""],
      ["del", "42", "", "−"],
      ["add", "", "42", "+"],
      ["add", "", "43", "+"],
      ["ctx", "43", "44", ""],
    ]);
    expect(rows[0]?.text).toBe("@@ -41,3 +41,4 @@ func build() {");
    // Text is passed through untouched: markup stays text.
    expect(diffRows([line("add", null, 1, "<img src=x onerror=alert(1)>")])[0]?.text).toBe("<img src=x onerror=alert(1)>");
  });

  it("says when a file has no final newline", () => {
    const rows = diffRows([line("delete", 1, null, "old"), line("no_newline", null, null, "No newline at end of file")]);
    expect(rows[1]).toEqual({ kind: "note", a: "", b: "", mark: "", text: "No newline at the end of the file" });
  });

  it("is empty for a binary or unchanged file", () => {
    expect(diffRows([])).toEqual([]);
  });
});

describe("changed files", () => {
  it("marks each kind", () => {
    expect(fileMark(file("a", "modified"))).toEqual({ letter: "M", tone: "yours" });
    expect(fileMark(file("a", "added"))).toEqual({ letter: "A", tone: "pass" });
    expect(fileMark(file("a", "untracked"))).toEqual({ letter: "A", tone: "pass" });
    expect(fileMark(file("a", "deleted"))).toEqual({ letter: "D", tone: "fail" });
    expect(fileMark(file("a", "renamed"))).toEqual({ letter: "R", tone: "yours" });
    expect(fileMark(file("a", "conflicted")).letter).toBe("!");
  });

  it("shows counts, or bin for binary files", () => {
    expect(fileCounts(file("a", "modified", { additions: 9, deletions: 2 }))).toEqual({ add: "+9", del: "−2" });
    expect(fileCounts(file("a", "modified", { additions: null, deletions: null, binary: true }))).toEqual({ add: "bin", del: "" });
    expect(fileCounts(file("a", "untracked", { additions: null, deletions: null }))).toEqual({ add: "", del: "" });
  });

  it("ticks everything but conflicts, and remembers choices across refreshes", () => {
    const files = [file("a.go", "modified"), file("b.go", "untracked"), file("c.go", "conflicted")];
    const ticks = initialTicks(files);
    expect(tickedPaths(files, ticks)).toEqual(["a.go", "b.go"]);

    ticks.set("b.go", false);
    const later = [...files, file("d.go", "added")];
    const kept = initialTicks(later, ticks);
    expect(tickedPaths(later, kept)).toEqual(["a.go", "d.go"]);
    // A file that went away is forgotten.
    expect(initialTicks([files[0]!], ticks).has("b.go")).toBe(false);
  });
});
