import { describe, expect, it } from "vitest";
import type { PullRow } from "../api";
import { behindLabel, pickMethod, pickPull, resolveSteps, shortRepo, updateLabel, visiblePulls } from "./pulls";

const row = (repo: string, number: number): PullRow => ({
  repo,
  repo_short: shortRepo(repo),
  number,
  title: `Pull ${number}`,
  by: "you",
  status: { text: "Ready to merge", tone: "pass" },
  via: null,
  checks: { state: "passing", text: "passed", tone: "pass", title: "1 check · 1 passed" },
  head: "feature",
  base: "main",
  behind: 0,
  conflicts: false,
});

describe("behind the base", () => {
  it("says how far, and nothing when it isn't", () => {
    expect(behindLabel(3, "master")).toBe("3 behind master");
    expect(behindLabel(1, "")).toBe("1 behind");
    expect(behindLabel(0, "master")).toBeNull();
  });

  it("names the base on the button", () => {
    expect(updateLabel("master")).toBe("Update from master");
    expect(updateLabel("")).toBe("Update branch");
  });

  it("resolves by hand in plain steps: fetch, merge the base, fix, push", () => {
    expect(resolveSteps("feature/tray", "master").map((s) => s.command)).toEqual([
      "git fetch origin",
      "git switch feature/tray",
      "git merge origin/master",
      "git add -A && git commit",
      "git push",
    ]);
  });
});

describe("pull request list", () => {
  const rows = [row("demi/atelier", 14), row("demi/tray", 3), row("demi/atelier", 9)];

  it("shows everything without a filter", () => {
    expect(visiblePulls(rows, null)).toHaveLength(3);
  });

  it("narrows to one repository", () => {
    expect(visiblePulls(rows, "demi/atelier").map((r) => r.number)).toEqual([14, 9]);
    expect(visiblePulls(rows, "demi/none")).toEqual([]);
  });

  it("opens the first one when nothing is picked", () => {
    expect(pickPull(rows, null)).toEqual({ repo: "demi/atelier", number: 14 });
    expect(pickPull([], null)).toBeNull();
  });

  it("keeps the picked one, even when it left the list", () => {
    expect(pickPull(rows, { repo: "demi/gone", number: 1 })).toEqual({ repo: "demi/gone", number: 1 });
  });

  it("shortens a repository name", () => {
    expect(shortRepo("demi/atelier")).toBe("atelier");
    expect(shortRepo("atelier")).toBe("atelier");
  });
});

describe("merge method", () => {
  const methods = [
    { id: "squash", label: "Squash and merge" },
    { id: "merge", label: "Merge" },
  ];

  it("offers the first by default", () => {
    expect(pickMethod(methods, null)?.id).toBe("squash");
  });

  it("offers the remembered one when allowed", () => {
    expect(pickMethod(methods, "merge")?.id).toBe("merge");
    expect(pickMethod(methods, "rebase")?.id).toBe("squash");
  });

  it("offers nothing when there is no way to merge", () => {
    expect(pickMethod([], "merge")).toBeNull();
  });
});
