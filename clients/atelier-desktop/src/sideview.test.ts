import { describe, expect, it, vi } from "vitest";
import type { WorkingCopy } from "./api";
import { h, mount } from "./dom";
import { sidebarGroups } from "./logic/sidebar";
import { copy } from "./testdata";
import { sideRepos, type SideActions } from "./sideview";

const company = (name: string, over: Partial<WorkingCopy> = {}) =>
  copy(name, { owner: "Elyndra Studios LTD", owner_name: "elyndrastudios", owner_kind: "company", full_name: `elyndrastudios/${name}`, ...over });
const loose = (name: string, over: Partial<WorkingCopy> = {}) =>
  copy(name, { owner: "github.com", owner_name: "", owner_kind: "", full_name: null, ...over });

function draw(copies: WorkingCopy[], others: WorkingCopy[], collapsed: string[], over: Partial<SideActions> = {}) {
  const actions: SideActions = {
    currentKey: "today",
    open: vi.fn(),
    more: vi.fn(),
    toggle: vi.fn(),
    publish: vi.fn(),
    othersOpen: false,
    toggleOthers: vi.fn(),
    ...over,
  };
  const nav = h("nav");
  mount(nav, sideRepos(sidebarGroups(copies, others, collapsed), others, actions));
  const groups = [...nav.querySelectorAll<HTMLElement>(".group")];
  return { nav, actions, groups, names: (g: HTMLElement) => [...g.querySelectorAll(".side-repo .clip")].map((n) => n.textContent) };
}

describe("the sidebar's repositories", () => {
  it("shows a header per owner with a count, and the repositories under it", () => {
    const { groups, names } = draw([copy("Atelier", { short: "3 · ↑1", tone: "yours" }), copy("Tray"), company("sparky")], [], []);
    expect(groups.map((g) => g.dataset.group)).toEqual(["demiinfinity", "elyndrastudios"]);
    const head = groups[0]?.querySelector(".group-toggle");
    expect(head?.getAttribute("aria-expanded")).toBe("true");
    expect(head?.textContent).toBe("DemiInfinity2");
    expect(names(groups[0] as HTMLElement)).toEqual(["Atelier", "Tray"]);
    expect(groups[0]?.querySelector(".state")?.textContent).toBe("3 · ↑1");
    expect(groups[1]?.querySelector(".group-toggle")?.textContent).toBe("Elyndra Studios LTD1");
  });

  it("hides a folded owner's repositories and asks to open it on a click", () => {
    const { groups, names, actions } = draw([copy("Atelier"), company("sparky"), company("nova")], [], ["elyndrastudios"]);
    const folded = groups[1] as HTMLElement;
    expect(names(folded)).toEqual([]);
    const head = folded.querySelector<HTMLButtonElement>(".group-toggle");
    expect(head?.getAttribute("aria-expanded")).toBe("false");
    expect(head?.querySelector(".count")?.textContent).toBe("2");
    head?.click();
    expect(actions.toggle).toHaveBeenCalledWith(expect.objectContaining({ key: "elyndrastudios", collapsed: true }));
  });

  it("puts pinned repositories in a Pinned group at the top, and not twice", () => {
    const { groups, names } = draw([copy("Atelier"), company("sparky", { pinned: true })], [], []);
    expect(groups.map((g) => g.dataset.group)).toEqual([":pinned", "demiinfinity"]);
    expect(groups[0]?.querySelector(".group-toggle .grow")?.textContent).toBe("Pinned");
    expect(names(groups[0] as HTMLElement)).toEqual(["sparky"]);
    expect(names(groups[1] as HTMLElement)).toEqual(["Atelier"]);
  });

  it("opens a repository, and its menu from the … button or a right-click", () => {
    const atelier = copy("Atelier");
    const { nav, actions } = draw([atelier], [], []);
    nav.querySelector<HTMLButtonElement>(".side-repo")?.click();
    expect(actions.open).toHaveBeenCalledWith(atelier);
    const more = nav.querySelector<HTMLButtonElement>(".side-more");
    expect(more?.getAttribute("aria-label")).toBe("More for Atelier");
    more?.click();
    expect(actions.more).toHaveBeenCalledWith(atelier, more);
    nav.querySelector(".side-row")?.dispatchEvent(new Event("contextmenu", { cancelable: true }));
    expect(actions.more).toHaveBeenCalledTimes(2);
  });

  it("marks the repository that is open", () => {
    const { nav } = draw([copy("Atelier"), copy("Tray")], [], [], { currentKey: "repo:/code/Tray" });
    expect([...nav.querySelectorAll(".side-repo")].map((b) => b.getAttribute("aria-current"))).toEqual([null, "page"]);
  });

  it("keeps other folders folded until asked, each with Publish", () => {
    const shut = draw([copy("Atelier")], [loose("loopkit")], []);
    const others = shut.groups[1] as HTMLElement;
    expect(others.dataset.group).toBe(":others");
    expect(others.querySelector(".group-toggle")?.textContent).toBe("Other folders1");
    expect(shut.names(others)).toEqual([]);
    others.querySelector<HTMLButtonElement>(".group-toggle")?.click();
    expect(shut.actions.toggleOthers).toHaveBeenCalled();

    const open = draw([copy("Atelier")], [loose("loopkit")], [], { othersOpen: true });
    const row = open.groups[1]?.querySelector(".side-row");
    expect(row?.querySelector(".side-repo .clip")?.textContent).toBe("loopkit");
    row?.querySelector<HTMLButtonElement>(".side-publish")?.click();
    expect(open.actions.publish).toHaveBeenCalledWith(expect.objectContaining({ name: "loopkit" }));
    // A repository in the studio has no Publish button.
    expect(open.groups[0]?.querySelector(".side-publish")).toBeNull();
  });

  it("shows text from git as text", () => {
    const { nav } = draw([copy("<img src=x onerror=1>")], [], []);
    expect(nav.querySelector("img")).toBeNull();
    expect(nav.querySelector(".side-repo .clip")?.textContent).toBe("<img src=x onerror=1>");
  });
});
