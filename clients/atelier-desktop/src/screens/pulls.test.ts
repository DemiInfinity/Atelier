import { describe, expect, it, vi } from "vitest";
import type { ChecksBrief, PullRow } from "../api";
import { checksChip, pullCard } from "./pulls";

const checks = (over: Partial<ChecksBrief> = {}): ChecksBrief => ({
  state: "pending",
  text: "3 of 4 passed",
  tone: "yours",
  title: "4 checks · 3 passed, 1 running",
  ...over,
});

const row = (over: Partial<PullRow> = {}): PullRow => ({
  repo: "DemiInfinity/Atelier",
  repo_short: "Atelier",
  number: 14,
  title: "Remote access switch",
  by: "you",
  status: { text: "Checks running", tone: "yours" },
  via: null,
  checks: checks(),
  head: "feature/tunnel-ui",
  ...over,
});

describe("checksChip", () => {
  it("is a dot and a few words in the state's colour", () => {
    const chip = checksChip(checks());
    expect(chip.textContent).toBe("3 of 4 passed");
    expect(chip.classList.contains("tone-yours")).toBe(true);
    expect(chip.classList.contains("is-pending")).toBe(true);
    expect(chip.querySelector(".dot")).not.toBeNull();
    expect(chip.title).toBe("4 checks · 3 passed, 1 running");
    expect(chip.getAttribute("aria-label")).toBe("Checks: 3 of 4 passed");
  });

  it("says failing, running, waiting, passed and no checks", () => {
    const cases: [ChecksBrief["state"], string, ChecksBrief["tone"]][] = [
      ["failing", "1 failing", "fail"],
      ["pending", "running", "yours"],
      ["pending", "waiting for you", "agents"],
      ["passing", "all 4 passed", "pass"],
      ["none", "no checks", "quiet"],
    ];
    for (const [state, text, tone] of cases) {
      const chip = checksChip(checks({ state, text, tone }));
      expect(chip.textContent).toBe(text);
      expect(chip.className).toBe(`checks-chip tone-${tone} is-${state}`);
    }
  });
});

describe("pullCard", () => {
  it("shows the checks on the row, across the studio and inside a repository", () => {
    const pick = vi.fn();
    const global = pullCard(row(), false, true, pick);
    expect(global.querySelector(".checks-chip")?.textContent).toBe("3 of 4 passed");
    expect(global.querySelector(".muted")?.textContent).toBe("Atelier · you");
    expect(global.getAttribute("aria-current")).toBe("false");
    global.click();
    expect(pick).toHaveBeenCalledOnce();

    const scoped = pullCard(row({ via: { text: "via Tether", tone: "github" } }), true, false, pick);
    expect(scoped.querySelector(".muted")?.textContent).toBe("pull request 14 · you");
    expect(scoped.getAttribute("aria-current")).toBe("true");
    expect([...scoped.querySelectorAll(".pill")].map((p) => p.textContent)).toEqual(["Checks running", "via Tether"]);
  });

  it("keeps a title's markup as text", () => {
    const card = pullCard(row({ title: "<b>bold</b>" }), false, true, () => {});
    expect(card.querySelector("b")).toBeNull();
    expect(card.querySelector(".pulls-card-title")?.textContent).toBe("<b>bold</b>");
  });
});
