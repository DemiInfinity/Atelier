import { describe, expect, it } from "vitest";
import { approvalBody, atBottom, workflowName } from "./device";

describe("workflowName", () => {
  it("takes the name out of the titles the runner writes", () => {
    expect(workflowName("weekly-tidy wants to run for the first time")).toBe("weekly-tidy");
    expect(workflowName("review helper wants to run with different tools")).toBe("review helper");
    expect(workflowName("triage was started by someone outside your studio")).toBe("triage");
    expect(workflowName("deploy wants to use a protected environment")).toBe("deploy");
    expect(workflowName("notes needs your approval to run")).toBe("notes");
  });

  it("keeps a title it doesn't recognise", () => {
    expect(workflowName("Something else")).toBe("Something else");
    expect(workflowName(" wants to run")).toBe(" wants to run");
  });
});

describe("approvalBody", () => {
  it("joins the detail and the limits as two sentences", () => {
    expect(approvalBody("Atelier · it would use issues.list", "Claude Code · up to 20 turns")).toBe(
      "Atelier · it would use issues.list. Claude Code · up to 20 turns.",
    );
  });

  it("doesn't double full stops or leave an empty sentence", () => {
    expect(approvalBody("It uses no tools.", "")).toBe("It uses no tools.");
    expect(approvalBody("", "")).toBe("");
  });
});

describe("atBottom", () => {
  it("follows at the end and within a little slack", () => {
    expect(atBottom(810, 190, 1000)).toBe(true);
    expect(atBottom(800, 190, 1000)).toBe(true);
  });

  it("holds still once the person scrolled up", () => {
    expect(atBottom(400, 190, 1000)).toBe(false);
  });

  it("counts a transcript that fits as at its end", () => {
    expect(atBottom(0, 190, 120)).toBe(true);
  });
});
