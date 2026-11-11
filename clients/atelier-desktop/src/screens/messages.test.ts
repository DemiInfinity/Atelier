import { describe, expect, it } from "vitest";
import type { MessagesView, MomentView } from "../api";
import { linesOf, previewOf, selected } from "./messages";

const moment = (over: Partial<MomentView> = {}): MomentView => ({
  key: "push",
  label: "After a push",
  tone: "yours",
  sub: "Atelier · main",
  ours: [
    { text: "Home and dry. {branch}", preview: "Home and dry. main" },
    { text: "Off it goes.", preview: "Off it goes." },
  ],
  yours: [{ text: "Shipped.", preview: "Shipped." }],
  plain: "Pushed main.",
  ...over,
});

describe("linesOf", () => {
  it("lists ours, then yours numbered from one", () => {
    const lines = linesOf(moment());
    expect(lines.map((l) => l.mine)).toEqual([false, false, true]);
    expect(lines[2]?.own).toBe(1);
  });
});

describe("previewOf", () => {
  it("shows the picked line's preview and wraps around", () => {
    expect(previewOf("friendly", moment(), 0).text).toBe("Home and dry. main");
    expect(previewOf("friendly", moment(), 2).text).toBe("Shipped.");
    expect(previewOf("friendly", moment(), 3).text).toBe("Home and dry. main");
    expect(previewOf("friendly", moment(), 0)).toMatchObject({ sub: "Atelier · main", tone: "yours" });
  });

  it("shows the plain line in plain tone, or when there are no lines", () => {
    expect(previewOf("plain", moment(), 1).text).toBe("Pushed main.");
    expect(previewOf("friendly", moment({ ours: [], yours: [] }), 4).text).toBe("Pushed main.");
  });

  it("says so when messages are off", () => {
    expect(previewOf("off", moment(), 0)).toEqual({
      text: "Messages are off",
      sub: "Nothing pops up. The app stays quiet.",
      tone: null,
    });
  });
});

describe("selected", () => {
  const view: MessagesView = {
    tone: "friendly",
    moments: [moment(), moment({ key: "fail", label: "Checks fail" })],
    rules: { quiet_fullscreen: true, terminal: true, milestones: true },
  };

  it("keeps the selected moment, else falls back to the first", () => {
    expect(selected(view, "fail")?.key).toBe("fail");
    expect(selected(view, "gone")?.key).toBe("push");
    expect(selected(view, null)?.key).toBe("push");
    expect(selected({ ...view, moments: [] }, "push")).toBeNull();
  });
});
