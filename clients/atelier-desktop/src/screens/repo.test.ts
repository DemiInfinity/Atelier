import { describe, expect, it } from "vitest";
import { DEFAULT_REPO_TAB } from "../routes";
import { repoTabs } from "./repo";

describe("a repository's tabs", () => {
  it("opens on History", () => {
    expect(DEFAULT_REPO_TAB).toBe("history");
  });

  it("puts History before Changes, then Pull requests and Issues", () => {
    const tabs = repoTabs({ changes: 3, branches: 2, open_pulls: 1, full_name: "DemiInfinity/Atelier" });
    expect(tabs.map((t) => t.label)).toEqual(["History", "Changes", "Pull requests", "Issues", "Branches", "Actions"]);
    expect(tabs.map((t) => t.id)).toEqual(["history", "changes", "pulls", "issues", "branches", "actions"]);
    expect(tabs.find((t) => t.id === "changes")?.count).toBe(3);
    expect(tabs.find((t) => t.id === "pulls")?.count).toBe(1);
  });

  it("leaves the studio's tabs out for a folder that isn't there", () => {
    const tabs = repoTabs({ changes: 0, branches: 1, open_pulls: 0, full_name: null });
    expect(tabs.map((t) => t.id)).toEqual(["history", "changes", "branches"]);
  });
});
