import { describe, expect, it, vi } from "vitest";
import type { IssueRow, RunGroup } from "../api";
import { issueCard, issueLine, labelChip } from "./repo-issues";
import { runGroup } from "./repo-runs";

const now = new Date("2026-11-11T12:00:00Z");

const issue = (over: Partial<IssueRow> = {}): IssueRow => ({
  number: 21,
  title: "Tray icon flickers",
  open: true,
  by: "you",
  via: null,
  labels: [{ name: "bug", color: "#d73a4a" }],
  comments: 2,
  created_at: "2026-11-08T12:00:00Z",
  updated_at: "2026-11-09T12:00:00Z",
  ...over,
});

describe("an issue in the list", () => {
  it("shows its number, title, labels, comments and age", () => {
    const pick = vi.fn();
    const card = issueCard(issue(), false, pick, now);
    expect(card.querySelector(".issue-number")?.textContent).toBe("#21");
    expect(card.querySelector(".pulls-card-title")?.textContent).toBe("#21 Tray icon flickers");
    expect(card.querySelector(".muted")?.textContent).toBe("by you · 2 comments · opened 3 days ago");
    expect(card.querySelector(".issue-label")?.textContent).toBe("bug");
    card.click();
    expect(pick).toHaveBeenCalledOnce();
  });

  it("leaves out what there is none of", () => {
    expect(issueLine({ by: "Mara", comments: 0, created_at: "" }, now)).toBe("by Mara");
    expect(issueLine({ by: "Mara", comments: 1, created_at: "" }, now)).toBe("by Mara · 1 comment");
    expect(issueCard(issue({ labels: [] }), false, () => {}, now).querySelector(".pulls-card-pills")).toBeNull();
  });

  it("gives an issue from GitHub the blue treatment", () => {
    const card = issueCard(issue({ via: { text: "via Tether", tone: "github" } }), true, () => {}, now);
    expect(card.classList.contains("tone-github")).toBe(true);
    expect(card.querySelector(".pill.tone-github")?.textContent).toBe("via Tether");
    expect(card.getAttribute("aria-current")).toBe("true");
    expect(issueCard(issue(), false, () => {}, now).classList.contains("tone-github")).toBe(false);
  });

  it("marks a closed one", () => {
    expect(issueCard(issue({ open: false }), false, () => {}, now).querySelector(".pill")?.textContent).toBe("Closed");
  });

  it("never turns what people wrote into markup", () => {
    const card = issueCard(issue({ title: "<img src=x onerror=alert(1)>", labels: [{ name: "<b>x</b>", color: null }] }), false, () => {}, now);
    expect(card.querySelector("img")).toBeNull();
    expect(card.querySelector("b")).toBeNull();
    expect(card.querySelector(".issue-label")?.textContent).toBe("<b>x</b>");
  });

  it("tints only a dot with a label's colour", () => {
    const dot = labelChip({ name: "bug", color: "#d73a4a" }).querySelector<HTMLElement>(".dot");
    expect(dot?.style.background).toBe("#d73a4a");
    expect(labelChip({ name: "plain", color: null }).querySelector<HTMLElement>(".dot")?.style.background).toBe("");
  });
});

describe("runs by pull request", () => {
  const group = (over: Partial<RunGroup> = {}): RunGroup => ({
    pull: 14,
    label: "Pull request 14 · Remote access switch",
    runs: [
      { run_id: 208, status: { text: "Failed", tone: "fail" }, failed: true, name: "ci", detail: "push on feature/tunnel-ui", duration: "2m 03s", started_at: "2026-11-11T11:00:00Z", web_path: "/x/actions/runs/208" },
      { run_id: 207, status: { text: "Passed", tone: "pass" }, failed: false, name: "ci", detail: "push on feature/tunnel-ui", duration: "1m 48s", started_at: "", web_path: null },
    ],
    ...over,
  });

  it("labels a group with its pull request and leads to it", () => {
    const actions = { open: vi.fn(), pull: vi.fn() };
    const el = runGroup(group(), actions, now);
    expect(el.querySelector("h3")?.textContent).toBe("Pull request 14 · Remote access switch");
    expect(el.classList.contains("for-pull")).toBe(true);
    const rows = [...el.querySelectorAll<HTMLButtonElement>(".runs-row")];
    expect(rows.map((r) => r.querySelector(".pill")?.textContent)).toEqual(["Failed", "Passed"]);
    expect(rows[0]?.title).toBe("Open the log");
    expect(rows[0]?.textContent).toContain("push on feature/tunnel-ui");
    rows[0]?.click();
    expect(actions.open).toHaveBeenCalledWith(expect.objectContaining({ run_id: 208 }));
    [...el.querySelectorAll<HTMLButtonElement>("button")].find((b) => b.textContent === "Open pull request")?.click();
    expect(actions.pull).toHaveBeenCalledWith(14);
  });

  it("has no pull request button for the other runs", () => {
    const el = runGroup(group({ pull: null, label: "Other runs" }), { open: vi.fn(), pull: vi.fn() }, now);
    expect(el.classList.contains("for-pull")).toBe(false);
    expect([...el.querySelectorAll("button")].some((b) => b.textContent === "Open pull request")).toBe(false);
  });
});
