import { describe, expect, it, vi } from "vitest";
import { h, mount } from "./dom";
import { sidebarGroups } from "./logic/sidebar";
import { worktreeRows } from "./sidebar-worktrees";
import { sideRepos, type SideActions } from "./sideview";
import { copy } from "./testdata";

describe("worktree rows in the sidebar", () => {
  it("are drawn under their repository's row and open like a repository", () => {
    const main = copy("Atelier");
    const tree = copy("fix-setup-race", {
      path: "/code/Atelier.worktrees/fix-setup-race",
      worktree_of: main.path,
      branch: "fix/setup-race",
      state: "Clean",
    });
    const open = vi.fn();
    const actions: SideActions = {
      currentKey: `repo:${tree.path}`,
      open: vi.fn(),
      more: vi.fn(),
      toggle: vi.fn(),
      publish: vi.fn(),
      othersOpen: false,
      toggleOthers: vi.fn(),
      under: (c) => worktreeRows(c, c.path === main.path ? [tree] : [], { currentKey: `repo:${tree.path}`, open }),
    };
    const nav = h("nav");
    mount(nav, sideRepos(sidebarGroups([main, copy("Tray")], [], []), [], actions));
    const rows = [...nav.querySelectorAll<HTMLElement>(".side-row, .side-worktree")].map((el) => el.textContent);
    expect(rows).toEqual(["Atelier", "fix/setup-raceClean", "Tray"]);
    const row = nav.querySelector<HTMLButtonElement>(".side-worktree")!;
    expect(row.getAttribute("aria-current")).toBe("page");
    row.click();
    expect(open).toHaveBeenCalledWith(tree);
  });
});
