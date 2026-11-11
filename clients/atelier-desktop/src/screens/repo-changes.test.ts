import { describe, expect, it } from "vitest";
import { cleanLine, commitLabel, statusLine } from "./repo-changes";

const base = { name: "Atelier", branch: "main", upstream: "origin/main", ahead: 0, behind: 0 };

describe("statusLine", () => {
  it("says where the branch stands against the studio", () => {
    expect(statusLine(base)).toBe("main · in step with your studio");
    expect(statusLine({ ...base, ahead: 1 })).toBe("main · 1 ahead of your studio");
    expect(statusLine({ ...base, ahead: 2, behind: 3 })).toBe("main · 2 ahead of your studio · 3 behind");
    expect(statusLine({ ...base, behind: 1 })).toBe("main · in step with your studio · 1 behind");
  });

  it("handles a branch with no upstream and a detached head", () => {
    expect(statusLine({ ...base, upstream: null })).toBe("main · not pushed to your studio yet");
    expect(statusLine({ ...base, branch: null })).toBe("detached · in step with your studio");
  });
});

describe("cleanLine", () => {
  it("picks the sentence for the nothing-to-commit banner", () => {
    expect(cleanLine(base)).toBe("Atelier is in step with your studio.");
    expect(cleanLine({ ...base, ahead: 1 })).toBe("1 commit is waiting to be pushed.");
    expect(cleanLine({ ...base, ahead: 3 })).toBe("3 commits are waiting to be pushed.");
    expect(cleanLine({ ...base, behind: 2 })).toBe("2 commits are waiting to be pulled.");
    expect(cleanLine({ ...base, upstream: null })).toBe("Atelier isn’t pushed to your studio yet.");
  });
});

describe("commitLabel", () => {
  it("counts the ticked files", () => {
    expect(commitLabel(0)).toBe("Commit");
    expect(commitLabel(1)).toBe("Commit 1 file");
    expect(commitLabel(3)).toBe("Commit 3 files");
  });
});
