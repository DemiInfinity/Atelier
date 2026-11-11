import { describe, expect, it } from "vitest";
import { lookingLine, othersLine, rootsWords } from "./today";

describe("rootsWords", () => {
  it("drops the leading in", () => {
    expect(rootsWords("in ~/Atelier and ~/Projects")).toBe("~/Atelier and ~/Projects");
    expect(rootsWords("~/Atelier")).toBe("~/Atelier");
    expect(rootsWords("in ~/inbox")).toBe("~/inbox");
    expect(rootsWords("")).toBe("");
  });
});

describe("lookingLine", () => {
  it("names where Atelier looks", () => {
    expect(lookingLine("in ~/Atelier")).toBe(
      "Atelier looks in ~/Atelier for git folders. Add the folder where you keep your projects, or clone one from your studio.",
    );
  });

  it("copes with no folders at all", () => {
    expect(lookingLine("")).toContain("isn't looking in any folder yet");
  });
});

describe("othersLine", () => {
  it("speaks of one folder or several", () => {
    expect(othersLine(1)).toBe("1 other git folder here isn't in your studio. It is under Other folders in the sidebar.");
    expect(othersLine(21)).toBe(
      "21 other git folders here aren't in your studio. They are under Other folders in the sidebar.",
    );
  });
});
