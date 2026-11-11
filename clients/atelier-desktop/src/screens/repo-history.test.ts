import { describe, expect, it } from "vitest";
import { graph } from "./repo-history";

describe("graph", () => {
  it("draws a shared commit on the amber lane", () => {
    expect(graph({ lane: 0, line0: "full", line1: "none", incoming: false })).toEqual({
      l0: "M14 0 V40",
      l1: null,
      cx: 14,
      fill: "var(--amber)",
    });
  });

  it("starts a lane at the newest commit", () => {
    const top = graph({ lane: 1, line0: "none", line1: "start", incoming: false });
    expect(top).toEqual({ l0: null, l1: "M36 20 V40", cx: 36, fill: "#c9c3ba" });
    expect(graph({ lane: 0, line0: "start", line1: "full", incoming: false }).l0).toBe("M14 20 V40");
  });

  it("bends the local lane into the shared one where they meet", () => {
    const g = graph({ lane: 0, line0: "full", line1: "join", incoming: false });
    expect(g.l1).toBe("M36 0 V8 Q36 20 14 20");
    expect(g.l0).toBe("M14 0 V40");
  });

  it("never bends the shared lane", () => {
    expect(graph({ lane: 0, line0: "join", line1: "none", incoming: false }).l0).toBeNull();
  });

  it("paints commits that are still to be pulled blue", () => {
    expect(graph({ lane: 0, line0: "full", line1: "none", incoming: true }).fill).toBe("var(--blue2)");
    expect(graph({ lane: 1, line0: "full", line1: "full", incoming: true }).fill).toBe("var(--blue2)");
  });
});
