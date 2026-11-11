import { describe, expect, it } from "vitest";
import type { WorkingCopy } from "../api";
import { jumpBackIn, popoverStep, shortElapsed } from "./tray";

const copy = (name: string, changes = 0, ahead = 0, behind = 0): WorkingCopy => ({
  path: `/code/${name}`,
  name,
  owner: "demi",
  owner_name: "demi",
  owner_kind: "user",
  pinned: false,
  full_name: `demi/${name}`,
  branch: "main",
  changes,
  ahead,
  behind,
  state: "",
  tone: "quiet",
  action: "open",
  short: "",
});

describe("popoverStep", () => {
  it("pushes without leaving the popover", () => {
    expect(popoverStep({ type: "push", path: "/code/marbl" })).toEqual({ do: "push", path: "/code/marbl" });
  });

  it("sends an agent's approval to This device", () => {
    expect(popoverStep({ type: "approve_agent", job_id: 7 })).toEqual({ do: "show", target: { screen: "device" } });
  });

  it("opens a pull request in the main window", () => {
    expect(popoverStep({ type: "open_pull", repo: "demi/atelier", number: 14 })).toEqual({
      do: "show",
      target: { screen: "pull", repo: "demi/atelier", number: 14 },
    });
  });

  it("opens logs and pages in the studio", () => {
    expect(popoverStep({ type: "open_log", run_id: 3, repo: "demi/atelier", web_path: "/demi/atelier/runs/3" })).toEqual({
      do: "show",
      target: { screen: "web", web_path: "/demi/atelier/runs/3" },
    });
    expect(popoverStep({ type: "open_web", web_path: "/settings" })).toEqual({
      do: "show",
      target: { screen: "web", web_path: "/settings" },
    });
  });
});

describe("jumpBackIn", () => {
  it("puts changes first, then to push, then behind, then the rest", () => {
    const copies = [copy("clean"), copy("behind", 0, 0, 2), copy("ahead", 0, 1), copy("dirty", 3)];
    expect(jumpBackIn(copies, 4).map((c) => c.name)).toEqual(["dirty", "ahead", "behind", "clean"]);
  });

  it("shows at most three", () => {
    const copies = [copy("a"), copy("b", 1), copy("c"), copy("d", 0, 1), copy("e")];
    expect(jumpBackIn(copies).map((c) => c.name)).toEqual(["b", "d", "a"]);
  });

  it("keeps the given order among equals and leaves the list alone", () => {
    const copies = [copy("one", 1), copy("two", 5), copy("three", 2)];
    expect(jumpBackIn(copies).map((c) => c.name)).toEqual(["one", "two", "three"]);
    expect(copies.map((c) => c.name)).toEqual(["one", "two", "three"]);
  });

  it("is empty without working copies", () => {
    expect(jumpBackIn([])).toEqual([]);
  });
});

describe("shortElapsed", () => {
  const now = new Date("2026-11-11T10:00:00Z");

  it("counts seconds, minutes, then hours and minutes", () => {
    expect(shortElapsed("2026-11-11T09:59:12Z", now)).toBe("48s");
    expect(shortElapsed("2026-11-11T09:56:30Z", now)).toBe("3m");
    expect(shortElapsed("2026-11-11T08:55:00Z", now)).toBe("1h 05m");
  });

  it("never goes negative and says nothing for a bad time", () => {
    expect(shortElapsed("2026-11-11T10:00:05Z", now)).toBe("0s");
    expect(shortElapsed("not a time", now)).toBe("");
  });
});
