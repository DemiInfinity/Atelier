import { afterEach, describe, expect, it, vi } from "vitest";
import { api, type BranchUpdate, type ChecksBrief, type PullRow, type PullView } from "../api";
import type { ConfirmOptions, ConflictsOptions, Ctx } from "../ctx";
import { conflictsDialog } from "../dialogs";
import { checksChip, pullCard, pullsView } from "./pulls";

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
  base: "master",
  behind: 0,
  conflicts: false,
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

describe("a row that is behind or conflicts", () => {
  it("says how far behind its base it is", () => {
    const card = pullCard(row({ behind: 3 }), false, true, () => {});
    expect(card.querySelector(".pulls-behind")?.textContent).toBe("3 behind master");
    expect(pullCard(row(), false, true, () => {}).querySelector(".pulls-behind")).toBeNull();
    // Inside a repository too.
    expect(pullCard(row({ behind: 1 }), false, false, () => {}).querySelector(".pulls-behind")?.textContent).toBe("1 behind master");
  });

  it("marks conflicts in red", () => {
    const card = pullCard(row({ conflicts: true, behind: 2 }), false, false, () => {});
    const mark = card.querySelector(".pulls-conflicts");
    expect(mark?.textContent).toBe("Conflicts");
    expect(mark?.classList.contains("tone-fail")).toBe(true);
    expect(pullCard(row(), false, true, () => {}).querySelector(".pulls-conflicts")).toBeNull();
  });
});

// ------------------------------------------------- the screen, end to end

const view = (over: Partial<PullView> = {}): PullView => ({
  repo: "DemiInfinity/Atelier",
  repo_short: "Atelier",
  number: 14,
  title: "Remote access switch",
  state: "open",
  head: "feature/tunnel-ui",
  base: "master",
  by: "you",
  commits: 2,
  files: 3,
  headline: "Not ready to merge yet",
  reason: "1 check is still running.",
  tone: "yours",
  can_merge: false,
  on_github: false,
  methods: [{ id: "squash", label: "Squash and merge" }],
  auto_merge: null,
  checks: [],
  comments: [],
  web_path: "/DemiInfinity/Atelier/pulls/14",
  local_path: "/home/Atelier",
  local_branch: "feature/tunnel-ui",
  behind: 3,
  conflicts: false,
  conflict_files: [],
  can_update: true,
  update_on_github: null,
  ...over,
});

const update = (over: Partial<BranchUpdate> = {}): BranchUpdate => ({
  outcome: "updated",
  head: "feature/tunnel-ui",
  base: "master",
  head_sha: "4f2a9c1e",
  message: "",
  files: [],
  local_path: "/home/Atelier",
  local_on_branch: false,
  ...over,
});

function fakeCtx(answer = true) {
  const said: string[] = [];
  const conflicts: ConflictsOptions[] = [];
  const asked: ConfirmOptions[] = [];
  const failed: unknown[] = [];
  const ctx = {
    app: () => ({ machine: "Mac", editor: { id: "zed", label: "Zed" } }),
    refresh: async () => {},
    say: (text: string) => void said.push(text),
    fail: (e: unknown) => void failed.push(e),
    confirm: async (options: ConfirmOptions) => {
      asked.push(options);
      return answer;
    },
    showConflicts: (options: ConflictsOptions) => void conflicts.push(options),
    busy: async <T>(_button: HTMLButtonElement | null, work: () => Promise<T>) => {
      try {
        return await work();
      } catch (e) {
        failed.push(e);
        return undefined;
      }
    },
  } as unknown as Ctx;
  return { ctx, said, conflicts, asked, failed };
}

const settle = async () => {
  for (let i = 0; i < 20; i++) await Promise.resolve();
  await new Promise((resolve) => setTimeout(resolve, 0));
  for (let i = 0; i < 20; i++) await Promise.resolve();
};

const button = (el: HTMLElement, label: string) =>
  [...el.querySelectorAll<HTMLButtonElement>("button")].find((b) => b.textContent === label) ?? null;

describe("update from the base", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    document.body.replaceChildren();
  });

  async function open(detail: PullView, world = fakeCtx()) {
    vi.spyOn(api, "pullsList").mockResolvedValue([row({ behind: detail.behind, conflicts: detail.conflicts })]);
    const read = vi.spyOn(api, "pullDetail").mockResolvedValue(detail);
    const screen = pullsView(world.ctx, {}, { repo: "DemiInfinity/Atelier", name: "Atelier" });
    document.body.appendChild(screen.el);
    await settle();
    return { ...world, screen, read };
  }

  it("shows the button and how far behind only when the branch is behind", async () => {
    const behind = await open(view());
    expect(button(behind.screen.el, "Update from master")).not.toBeNull();
    expect(behind.screen.el.querySelector(".pulls-merge .pulls-behind")?.textContent).toBe("3 behind master");
    expect(behind.screen.el.querySelector(".pulls-card .pulls-behind")?.textContent).toBe("3 behind master");
    document.body.replaceChildren();
    const current = await open(view({ behind: 0, can_update: false }));
    expect(button(current.screen.el, "Update from master")).toBeNull();
    expect(current.screen.el.querySelector(".pulls-behind")).toBeNull();
  });

  it("updates, says so, and reads the pull request and its checks again", async () => {
    const world = await open(view());
    const post = vi.spyOn(api, "pullUpdate").mockResolvedValue(update());
    const pull = vi.spyOn(api, "repoPull");
    world.read.mockResolvedValue(view({ behind: 0, can_update: false, checks: [{ status: { text: "Running", tone: "yours" }, name: "ci · test", detail: "", action_label: null, action: null }] }));
    const reads = world.read.mock.calls.length;
    button(world.screen.el, "Update from master")?.click();
    await settle();
    expect(post).toHaveBeenCalledWith("DemiInfinity/Atelier", 14);
    expect(world.said).toEqual(["Updated from master."]);
    expect(world.read.mock.calls.length).toBeGreaterThan(reads);
    expect(button(world.screen.el, "Update from master")).toBeNull();
    expect(world.screen.el.querySelector(".pulls-check-name")?.textContent).toBe("ci · test");
    // The folder isn't on that branch: nothing is asked, nothing is pulled.
    expect(world.asked).toEqual([]);
    expect(pull).not.toHaveBeenCalled();
  });

  it("offers a pull when the folder is on that branch, and never pulls unasked", async () => {
    const no = await open(view(), fakeCtx(false));
    vi.spyOn(api, "pullUpdate").mockResolvedValue(update({ local_on_branch: true }));
    const pull = vi.spyOn(api, "repoPull").mockResolvedValue(2);
    button(no.screen.el, "Update from master")?.click();
    await settle();
    expect(no.asked.map((a) => [a.title, a.confirm])).toEqual([["Your local branch is now behind", "Pull"]]);
    expect(no.asked[0]?.body).toContain("feature/tunnel-ui in your studio has the new commits from master");
    expect(pull).not.toHaveBeenCalled();

    document.body.replaceChildren();
    const yes = await open(view(), fakeCtx(true));
    button(yes.screen.el, "Update from master")?.click();
    await settle();
    expect(pull).toHaveBeenCalledExactlyOnceWith("/home/Atelier");
    expect(yes.said).toEqual(["Updated from master.", "Pulled 2 commits."]);
  });

  it("says when there was nothing to bring in", async () => {
    const world = await open(view());
    vi.spyOn(api, "pullUpdate").mockResolvedValue(update({ outcome: "up_to_date", local_on_branch: true }));
    button(world.screen.el, "Update from master")?.click();
    await settle();
    expect(world.said).toEqual(["Already up to date with master."]);
    expect(world.asked).toEqual([]);
  });

  it("still works while a merge waits for green checks", async () => {
    const world = await open(view({ auto_merge: "Squash · asked by MacBook", headline: "Will merge when checks pass" }));
    const post = vi.spyOn(api, "pullUpdate").mockResolvedValue(update());
    expect(world.screen.el.querySelector(".pulls-auto-line")?.textContent).toContain("Merging when checks pass");
    const b = button(world.screen.el, "Update from master");
    expect(b?.disabled).toBe(false);
    b?.click();
    await settle();
    expect(post).toHaveBeenCalledOnce();
  });

  it("hands conflicts to the dialog with the files and the folder", async () => {
    const world = await open(view());
    vi.spyOn(api, "pullUpdate").mockResolvedValue(
      update({ outcome: "conflicts", message: "2 files conflict with master.", files: ["cmd/app/main.go", "README.md"], local_on_branch: true }),
    );
    const pull = vi.spyOn(api, "repoPull");
    button(world.screen.el, "Update from master")?.click();
    await settle();
    expect(world.conflicts).toEqual([
      { head: "feature/tunnel-ui", base: "master", message: "2 files conflict with master.", files: ["cmd/app/main.go", "README.md"], localPath: "/home/Atelier" },
    ]);
    expect(world.said).toEqual([]);
    expect(world.asked).toEqual([]);
    expect(pull).not.toHaveBeenCalled();
  });

  it("shows a refusal as an error and leaves the button there", async () => {
    const world = await open(view());
    vi.spyOn(api, "pullUpdate").mockRejectedValue({ kind: "not_open", message: "This pull request is already merged.", hint: null });
    button(world.screen.el, "Update from master")?.click();
    await settle();
    expect(world.failed).toHaveLength(1);
    expect(world.said).toEqual([]);
  });
});

describe("when updating isn't plain sailing", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    document.body.replaceChildren();
  });

  async function open(detail: PullView, answer: boolean) {
    const world = fakeCtx(answer);
    vi.spyOn(api, "pullsList").mockResolvedValue([row({ behind: detail.behind })]);
    vi.spyOn(api, "pullDetail").mockResolvedValue(detail);
    const screen = pullsView(world.ctx, {}, null);
    document.body.appendChild(screen.el);
    await settle();
    return { ...world, el: screen.el };
  }

  it("offers to try again when a push landed meanwhile", async () => {
    const moved = { kind: "branch_moved", message: "feature/tunnel-ui changed while the update was running.", hint: null };
    const world = await open(view(), true);
    const post = vi.spyOn(api, "pullUpdate").mockRejectedValueOnce(moved).mockResolvedValueOnce(update());
    button(world.el, "Update from master")?.click();
    await settle();
    expect(world.asked.map((a) => [a.title, a.confirm])).toEqual([["The branch changed just now", "Try again"]]);
    expect(world.asked[0]?.body).toBe("feature/tunnel-ui changed while the update was running. Nothing was updated.");
    expect(post).toHaveBeenCalledTimes(2);
    expect(world.said).toEqual(["Updated from master."]);
    expect(world.failed).toEqual([]);

    document.body.replaceChildren();
    const no = await open(view(), false);
    post.mockReset().mockRejectedValue(moved);
    button(no.el, "Update from master")?.click();
    await settle();
    expect(post).toHaveBeenCalledOnce();
    expect(no.said).toEqual([]);
    expect(no.failed).toEqual([]);
  });

  it("shows the studio's sentence when a rule on the branch forbids it", async () => {
    const world = await open(view(), true);
    const refused = { kind: "branch_protected", message: "feature/tunnel-ui only takes changes through pull requests.", hint: null };
    vi.spyOn(api, "pullUpdate").mockRejectedValue(refused);
    button(world.el, "Update from master")?.click();
    await settle();
    expect(world.failed).toEqual([refused]);
    expect(world.asked).toEqual([]);
  });

  it("offers GitHub instead of the button for a repository at home there", async () => {
    const world = await open(view({ can_update: false, on_github: true, update_on_github: "maralind/loopkit" }), true);
    expect(button(world.el, "Update from master")).toBeNull();
    const openGithub = vi.spyOn(api, "openGithub").mockResolvedValue();
    button(world.el, "Open on GitHub ↗")?.click();
    await settle();
    expect(openGithub).toHaveBeenCalledExactlyOnceWith("maralind/loopkit");
    // Still says how far behind it is.
    expect(world.el.querySelector(".pulls-merge .pulls-behind")?.textContent).toBe("3 behind master");
  });

  it("has no button when the studio says it can't be updated", async () => {
    const world = await open(view({ can_update: false }), true);
    expect(button(world.el, "Update from master")).toBeNull();
    expect(button(world.el, "Open on GitHub ↗")).toBeNull();
  });
});

describe("a pull request that conflicts with its base", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    document.body.replaceChildren();
  });

  const conflicting = (over: Partial<PullView> = {}) =>
    view({
      headline: "Conflicts with master",
      reason: "feature/tunnel-ui and master changed the same lines. Resolve the conflicts before merging.",
      tone: "fail",
      can_merge: false,
      can_update: false,
      conflicts: true,
      conflict_files: ["cmd/app/main.go", "<b>README</b>.md"],
      ...over,
    });

  async function open(detail: PullView) {
    const world = fakeCtx();
    vi.spyOn(api, "pullsList").mockResolvedValue([row({ behind: 3, conflicts: true })]);
    vi.spyOn(api, "pullDetail").mockResolvedValue(detail);
    const screen = pullsView(world.ctx, {}, null);
    document.body.appendChild(screen.el);
    await settle();
    return { ...world, el: screen.el };
  }

  it("lists the files and the steps instead of the update button, and can't merge", async () => {
    const { el, said } = await open(conflicting());
    expect(el.querySelector(".pulls-card .pulls-conflicts")?.textContent).toBe("Conflicts");
    expect(el.querySelector(".pulls-merge")?.classList.contains("is-conflicting")).toBe(true);
    expect(el.querySelector(".pulls-merge-headline")?.textContent).toBe("Conflicts with master");
    expect(el.querySelector(".pulls-merge .soft")?.textContent).toContain("Resolve the conflicts before merging.");
    expect(button(el, "Update from master")).toBeNull();
    expect(button(el, "Squash and merge")?.disabled).toBe(true);
    expect([...el.querySelectorAll(".conflicts-files li")].map((li) => li.textContent)).toEqual(["cmd/app/main.go", "<b>README</b>.md"]);
    expect(el.querySelector(".conflicts-files b")).toBeNull();
    expect(el.querySelector(".conflicts-label")?.textContent).toBe("2 files conflict with master");
    expect([...el.querySelectorAll(".conflicts-steps code")].map((c) => c.textContent)).toContain("git merge origin/master");
    const openIn = vi.spyOn(api, "repoOpen").mockResolvedValue("Zed");
    button(el, "Open in Zed")?.click();
    await settle();
    expect(openIn).toHaveBeenCalledExactlyOnceWith("/home/Atelier");
    expect(said).toEqual(["Opened in Zed."]);
  });

  it("says so when the repository isn't on this machine", async () => {
    const { el } = await open(conflicting({ local_path: null, local_branch: null }));
    expect(button(el, "Open in Zed")).toBeNull();
    expect(el.querySelector(".pulls-conflicts-foot")?.textContent).toBe("This repository isn’t on this Mac yet. Check out the branch first.");
  });
});

describe("the conflicts dialog", () => {
  afterEach(() => {
    document.body.replaceChildren();
  });

  const options: ConflictsOptions = {
    head: "feature/tray",
    base: "master",
    message: "2 files conflict with master.",
    files: ["cmd/app/main.go", "README.md"],
    localPath: "/home/Atelier",
  };

  it("lists the files, the steps and opens the folder in the editor", () => {
    const opened: string[] = [];
    conflictsDialog(options, "Zed", (path) => opened.push(path));
    const dialog = document.querySelector<HTMLElement>(".dialog");
    expect(dialog?.getAttribute("aria-label")).toBe("Conflicts with master");
    expect(dialog?.querySelector(".dialog-head")?.textContent).toBe("feature/tray conflicts with master");
    expect(dialog?.textContent).toContain("2 files conflict with master.");
    expect([...(dialog?.querySelectorAll(".conflicts-files li") ?? [])].map((li) => li.textContent)).toEqual(["cmd/app/main.go", "README.md"]);
    expect([...(dialog?.querySelectorAll(".conflicts-steps code") ?? [])].map((c) => c.textContent)).toEqual([
      "git fetch origin",
      "git switch feature/tray",
      "git merge origin/master",
      "git add -A && git commit",
      "git push",
    ]);
    button(dialog as HTMLElement, "Open in Zed")?.click();
    expect(opened).toEqual(["/home/Atelier"]);
    expect(document.querySelector(".dialog")).toBeNull();
  });

  it("without a folder here there is nothing to open, and without files only the steps", () => {
    conflictsDialog({ ...options, message: "", files: [], localPath: null }, "Zed", () => {});
    const dialog = document.querySelector<HTMLElement>(".dialog") as HTMLElement;
    expect(button(dialog, "Open in Zed")).toBeNull();
    expect(dialog.querySelector(".conflicts-files")).toBeNull();
    expect(dialog.textContent).toContain("Nothing was changed.");
    expect(dialog.textContent).toContain("This repository isn’t on this machine yet.");
    button(dialog, "Close")?.click();
    expect(document.querySelector(".dialog")).toBeNull();
  });
});
