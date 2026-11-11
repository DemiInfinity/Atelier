import { describe, expect, it } from "vitest";
import type { WorkingCopy } from "../api";
import { copy } from "../testdata";
import { ownerKey, PINNED_KEY, sidebarGroups, toggled } from "./sidebar";

const company = (name: string, over: Partial<WorkingCopy> = {}) =>
  copy(name, { owner: "Elyndra Studios LTD", owner_name: "elyndrastudios", owner_kind: "company", full_name: `elyndrastudios/${name}`, ...over });

describe("sidebarGroups", () => {
  it("makes one group per owner, the personal account first", () => {
    const groups = sidebarGroups([company("sparky"), copy("Tray"), company("nova"), copy("Atelier")], [], []);
    expect(groups.map((g) => [g.key, g.label, g.kind, g.copies.map((c) => c.name)])).toEqual([
      ["demiinfinity", "DemiInfinity", "user", ["Tray", "Atelier"]],
      ["elyndrastudios", "Elyndra Studios LTD", "company", ["sparky", "nova"]],
    ]);
    expect(groups.every((g) => !g.collapsed)).toBe(true);
  });

  it("orders companies by name after the personal account", () => {
    const zed = copy("z", { owner: "Zed Co", owner_name: "zed", owner_kind: "company" });
    const groups = sidebarGroups([zed, company("sparky"), copy("Atelier")], [], []);
    expect(groups.map((g) => g.label)).toEqual(["DemiInfinity", "Elyndra Studios LTD", "Zed Co"]);
  });

  it("remembers which owners are folded shut", () => {
    const groups = sidebarGroups([copy("Atelier"), company("sparky")], [], ["elyndrastudios"]);
    expect(groups.map((g) => g.collapsed)).toEqual([false, true]);
    // A folded group keeps its repositories: the header still counts them.
    expect(groups[1]?.copies).toHaveLength(1);
  });

  it("moves pinned repositories into a Pinned group on top", () => {
    const loose = copy("loopkit", { full_name: null, owner: "github.com", owner_name: "", owner_kind: "", pinned: true });
    const groups = sidebarGroups([copy("Atelier"), company("sparky", { pinned: true }), copy("Tray")], [loose], []);
    expect(groups.map((g) => [g.key, g.copies.map((c) => c.name)])).toEqual([
      [PINNED_KEY, ["sparky", "loopkit"]],
      ["demiinfinity", ["Atelier", "Tray"]],
    ]);
    expect(groups[0]?.label).toBe("Pinned");
    expect(groups[0]?.kind).toBe("pinned");
  });

  it("has no Pinned group when nothing is pinned, and can fold it when there is", () => {
    expect(sidebarGroups([copy("Atelier")], [], [PINNED_KEY]).map((g) => g.key)).toEqual(["demiinfinity"]);
    expect(sidebarGroups([copy("Atelier", { pinned: true })], [], [PINNED_KEY])[0]?.collapsed).toBe(true);
  });

  it("groups folders the studio could not be asked about by where they live", () => {
    const offline = copy("thing", { owner: "github.com", owner_name: "", owner_kind: "", full_name: null });
    expect(ownerKey(offline)).toBe("github.com");
    expect(sidebarGroups([offline], [], [])[0]?.kind).toBe("other");
  });
});

describe("toggled", () => {
  it("adds and removes a fold without repeating it", () => {
    expect(toggled([], "a", true)).toEqual(["a"]);
    expect(toggled(["a"], "a", true)).toEqual(["a"]);
    expect(toggled(["a", "b"], "a", false)).toEqual(["b"]);
  });
});
