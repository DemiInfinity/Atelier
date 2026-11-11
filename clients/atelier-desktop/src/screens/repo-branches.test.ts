import { describe, expect, it } from "vitest";
import { aheadBehind } from "./repo-branches";

describe("aheadBehind", () => {
  it("shows only the sides that aren't zero", () => {
    expect(aheadBehind(0, 0)).toBe("");
    expect(aheadBehind(2, 0)).toBe("↑2");
    expect(aheadBehind(0, 1)).toBe("↓1");
    expect(aheadBehind(2, 1)).toBe("↑2 ↓1");
  });
});
