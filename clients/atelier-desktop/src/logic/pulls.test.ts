import { describe, expect, it } from "vitest";
import type { PullRow } from "../api";
import { pickMethod, pickPull, shortRepo, visiblePulls } from "./pulls";

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
