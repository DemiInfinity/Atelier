import { describe, expect, it } from "vitest";
import type { FolderPlan } from "../api";
import { accountLabel, defaultOwner, nameOk, openedWords, ownerChooser, publishStep, TABS } from "./add";

const plan = (over: Partial<FolderPlan> = {}): FolderPlan => ({
  path: "/home/me/thing",
  display: "~/thing",
  name: "thing",
  is_repo: true,
  branch: "main",
  commits: 3,
  origin: null,
  in_studio: null,
  summary: "A git folder",
  ...over,
});

describe("publishStep", () => {
  it("waits for a folder", () => {
    expect(publishStep(null)).toEqual({ kind: "none", label: "Publish to Atelier", note: null });
  });

  it("offers to make a plain folder a git folder", () => {
    const step = publishStep(plan({ is_repo: false, commits: 0 }));
    expect(step.kind).toBe("init");
    expect(step.label).toBe("Make it a git folder");
  });

  it("opens a folder that is already in the studio", () => {
    const step = publishStep(plan({ in_studio: "me/thing" }));
    expect(step.kind).toBe("open");
    expect(step.note).toContain("me/thing");
  });

  it("asks for a first commit", () => {
    const step = publishStep(plan({ commits: 0 }));
    expect(step.kind).toBe("open");
    expect(step.note).toBe("It needs a first commit before it can be published.");
  });

  it("publishes a git folder with commits", () => {
    expect(publishStep(plan()).kind).toBe("publish");
  });
});

describe("accountLabel", () => {
  const base = { commit_name: "", commit_email: "" };
  it("tells personal from company accounts", () => {
    expect(accountLabel({ ...base, name: "me", display: "Me", kind: "user" })).toBe("me (personal)");
    expect(accountLabel({ ...base, name: "acme", display: "Acme Co", kind: "company" })).toBe("Acme Co (company)");
  });
});

describe("nameOk", () => {
  it("wants one word", () => {
    expect(nameOk("my-new-thing")).toBe(true);
    expect(nameOk("  padded  ")).toBe(true);
    expect(nameOk("")).toBe(false);
    expect(nameOk("   ")).toBe(false);
    expect(nameOk("two words")).toBe(false);
    expect(nameOk("owner/name")).toBe(false);
  });
});

describe("TABS", () => {
  it("lists the four ways in, opening a folder last", () => {
    expect(TABS.map((tab) => tab.id)).toEqual(["publish", "clone", "new", "open"]);
    expect(TABS[3]?.label).toBe("Open a folder");
  });
});

describe("openedWords", () => {
  it("has nothing to say about a folder that was opened", () => {
    expect(openedWords({ kind: "opened", remote: null })).toBeNull();
  });

  it("names the remote of a folder that lives elsewhere, and spots GitHub", () => {
    expect(openedWords({ kind: "elsewhere", remote: "origin → github.com/someone/thing" })).toEqual({
      line: "This folder's remote isn't your studio:",
      remote: "origin → github.com/someone/thing",
      github: true,
      canOpen: true,
    });
    const other = openedWords({ kind: "elsewhere", remote: "origin → git.example.org/x/y" });
    expect(other?.github).toBe(false);
    expect(openedWords({ kind: "elsewhere", remote: null })?.line).toBe("This folder's remote isn't your studio.");
  });

  it("offers to open a git folder with no remote, but not a plain folder", () => {
    expect(openedWords({ kind: "unpublished", remote: null })).toMatchObject({
      line: "This git folder has no remote yet.",
      canOpen: true,
    });
    expect(openedWords({ kind: "not_repo", remote: null })).toMatchObject({
      line: "This isn't a git folder yet.",
      canOpen: false,
    });
  });
});

describe("This belongs to", () => {
  const base = { commit_name: "", commit_email: "" };
  const accounts = [
    { ...base, name: "DemiInfinity", display: "Demi", kind: "user" },
    { ...base, name: "elyndrastudios", display: "Elyndra Studios LTD", kind: "company" },
    { ...base, name: "sidequest", display: "", kind: "company" },
  ];

  it("starts with the personal account, then with whoever was chosen last", () => {
    expect(defaultOwner(accounts, null)).toBe("DemiInfinity");
    expect(defaultOwner(accounts, "ElyndraStudios")).toBe("elyndrastudios");
    // An owner the studio no longer reports is forgotten.
    expect(defaultOwner(accounts, "gone")).toBe("DemiInfinity");
    expect(defaultOwner([], "gone")).toBe("");
  });

  it("lists every owner the studio reports, with what kind each is", () => {
    const chooser = ownerChooser("who", accounts, "elyndrastudios");
    expect(chooser.el.querySelector("legend")?.textContent).toBe("This belongs to");
    const options = [...chooser.el.querySelectorAll(".owner-option")];
    expect(options.map((o) => [o.querySelector(".owner-name")?.textContent, o.querySelector(".owner-kind")?.textContent])).toEqual([
      ["DemiInfinity", "personal account"],
      ["Elyndra Studios LTD", "company"],
      ["sidequest", "company"],
    ]);
    const radios = [...chooser.el.querySelectorAll<HTMLInputElement>("input[type=radio]")];
    expect(radios.map((r) => r.checked)).toEqual([false, true, false]);
    expect(chooser.value()).toBe("elyndrastudios");
  });

  it("follows a click and can be set from outside", () => {
    let changes = 0;
    const chooser = ownerChooser("who", accounts, "DemiInfinity", () => (changes += 1));
    const radios = [...chooser.el.querySelectorAll<HTMLInputElement>("input[type=radio]")];
    (radios[1] as HTMLInputElement).checked = true;
    radios[1]?.dispatchEvent(new Event("change"));
    expect(chooser.value()).toBe("elyndrastudios");
    expect(changes).toBe(1);
    chooser.set("sidequest");
    expect(chooser.value()).toBe("sidequest");
    expect(radios[2]?.checked).toBe(true);
    chooser.set("nobody");
    expect(chooser.value()).toBe("sidequest");
    chooser.disable(true);
    expect(radios.every((r) => r.disabled)).toBe(true);
  });

  it("says so when the studio reports nobody", () => {
    const chooser = ownerChooser("who", [], "");
    expect(chooser.el.querySelectorAll("input")).toHaveLength(0);
    expect(chooser.el.textContent).toContain("didn't say who");
  });
});
