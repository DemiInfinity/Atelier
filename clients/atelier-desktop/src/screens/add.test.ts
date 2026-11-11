import { describe, expect, it } from "vitest";
import type { FolderPlan } from "../api";
import { accountLabel, nameOk, publishStep } from "./add";

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
