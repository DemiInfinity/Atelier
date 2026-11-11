import { describe, expect, it } from "vitest";
import { initial, linkLife, sessionWords } from "./settings";

describe("sessionWords", () => {
  const now = new Date("2026-11-11T12:00:00Z");
  const session = { ip: "203.0.113.7", country: "NL", signed_in: true, class: "browser", since: "2026-11-11T11:50:00Z" };

  it("lists place, sign-in, class and time", () => {
    expect(sessionWords(session, now)).toBe("NL · signed in · browser · 10 min ago");
  });

  it("copes with an unknown place and a visitor who isn't signed in", () => {
    expect(sessionWords({ ...session, country: "", signed_in: false, class: "" }, now)).toBe(
      "unknown place · not signed in · 10 min ago",
    );
  });
});

describe("linkLife", () => {
  it("speaks in whole minutes, never zero", () => {
    expect(linkLife(600)).toBe("Works once, for 10 minutes.");
    expect(linkLife(60)).toBe("Works once, for 1 minute.");
    expect(linkLife(5)).toBe("Works once, for 1 minute.");
  });
});

describe("initial", () => {
  it("takes the first letter of the commit name, then the account name", () => {
    expect(initial({ commit_name: "demi", name: "x" })).toBe("D");
    expect(initial({ commit_name: " ", name: "elyndra" })).toBe("E");
    expect(initial({ commit_name: "", name: "" })).toBe("?");
  });
});
