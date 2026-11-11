import { describe, expect, it } from "vitest";
import { sentenceList, sshLine, stepMark, stepNames, timeLeft } from "./welcome";

describe("welcome steps", () => {
  it("names the machine in the steps", () => {
    const names = stepNames("PC");
    expect(names).toHaveLength(4);
    expect(names[1]?.title).toBe("Approve this PC");
    expect(names[3]?.detail).toContain("this PC");
  });

  it("marks steps before the current one as done", () => {
    expect([0, 1, 2, 3].map((i) => stepMark(i, 2))).toEqual(["done", "done", "current", "todo"]);
  });
});

describe("sentenceList", () => {
  it("joins with commas and a final and", () => {
    expect(sentenceList([]).join("")).toBe("");
    expect(sentenceList(["a"]).join("")).toBe("a");
    expect(sentenceList(["a", "b"]).join("")).toBe("a and b");
    expect(sentenceList(["a", "b", "c"]).join("")).toBe("a, b and c");
  });
});

describe("timeLeft", () => {
  it("rounds up to whole minutes", () => {
    expect(timeLeft(30)).toBe("less than a minute");
    expect(timeLeft(60)).toBe("another 1 minute");
    expect(timeLeft(61)).toBe("another 2 minutes");
  });
});

describe("sshLine", () => {
  const key = { fingerprint: "SHA256:abc", key_path: "/k" };

  it("says whether a key was made or reused", () => {
    expect(sshLine({ ssh: { ...key, generated: true }, ssh_error: null }, "Mac")?.text).toBe(
      "A new SSH key was made for this Mac and added to your studio",
    );
    const reused = sshLine({ ssh: { ...key, generated: false }, ssh_error: null }, "Mac");
    expect(reused?.text).toBe("This Mac's SSH key was added to your studio");
    expect(reused?.fingerprint).toBe("SHA256:abc");
    expect(reused?.failed).toBe(false);
  });

  it("passes on the error when the key couldn't be added", () => {
    expect(sshLine({ ssh: null, ssh_error: "The studio refused the key." }, "Mac")).toEqual({
      text: "The studio refused the key.",
      fingerprint: null,
      failed: true,
    });
    expect(sshLine({ ssh: null, ssh_error: null }, "Mac")).toBeNull();
  });
});
