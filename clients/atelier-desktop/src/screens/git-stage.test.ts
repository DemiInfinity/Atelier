import { beforeEach, describe, expect, it, vi } from "vitest";
import type { StageEntry, StageView } from "../api";

// What `stageApi.status` answers, and the folders it was asked about.
const fake: { answer: () => Promise<StageView>; asked: string[] } = { answer: () => Promise.reject(new Error("unset")), asked: [] };
vi.mock("../api", async (original) => ({
  ...(await original<typeof import("../api")>()),
  stageApi: {
    status: (path: string) => {
      fake.asked.push(path);
      return fake.answer();
    },
  },
}));

import { gitStageTab, stageSummary } from "./git-stage";
import type { RepoHost } from "./repo";

const entry = (path: string, over: Partial<StageEntry> = {}): StageEntry => ({
  path,
  old_path: null,
  kind: "modified",
  additions: 1,
  deletions: 0,
  binary: false,
  ...over,
});

const view = (over: Partial<StageView> = {}): StageView => ({
  branch: "feature/tunnel-ui",
  head: null,
  staged: [],
  unstaged: [],
  conflicted: [],
  committing_as: null,
  ...over,
});

describe("stageSummary", () => {
  it("counts each side and leaves out the empty ones", () => {
    expect(stageSummary(view())).toBe("Nothing to commit");
    expect(stageSummary(view({ staged: [entry("a"), entry("b")], unstaged: [entry("c")] }))).toBe("2 staged · 1 not staged");
    expect(stageSummary(view({ unstaged: [entry("c")] }))).toBe("1 not staged");
    expect(stageSummary(view({ conflicted: [entry("x", { kind: "conflicted" })] }))).toBe("1 conflict");
  });
});

describe("the Changes tab", () => {
  beforeEach(() => {
    fake.asked = [];
  });

  it("reads the status of the folder it was given and shows it as text", async () => {
    const answer = view({
      staged: [entry("cmd/atelier-desktop/tray.go", { additions: 9, deletions: 2 })],
      unstaged: [entry("<img src=x onerror=alert(1)>.go", { additions: 12, deletions: 3 })],
    });
    fake.answer = () => Promise.resolve(answer);
    const tab = gitStageTab({ path: "/code/Atelier" } as RepoHost);
    await tab.reload();
    expect(fake.asked).toEqual(["/code/Atelier"]);
    const lines = [...tab.el.querySelectorAll("li")].map((li) => li.textContent);
    expect(lines).toEqual([
      "1 staged · 1 not staged",
      "Staged: cmd/atelier-desktop/tray.go +9 −2",
      "Not staged: <img src=x onerror=alert(1)>.go +12 −3",
    ]);
    // A file name is never markup.
    expect(tab.el.querySelector("img")).toBeNull();
  });

  it("says what went wrong instead of throwing", async () => {
    fake.answer = () => Promise.reject({ kind: "outside", message: "That folder isn’t one Atelier looks after.", hint: null });
    const tab = gitStageTab({ path: "/elsewhere" } as RepoHost);
    await tab.reload();
    expect(tab.el.textContent).toContain("That folder isn’t one Atelier looks after.");
  });
});
