import { afterEach, describe, expect, it, vi } from "vitest";
import type { OpOutcome } from "./api";
import type { Ctx } from "./ctx";
import { createGitNav, gitReady, isHeavy, setGitPreview, type NavHost, type Place } from "./git-nav";

function world(start: Place = { tab: "history", view: null }) {
  let place = start;
  const shown: Place[] = [];
  const said: string[] = [];
  const bar: { text: string | null } = { text: null };
  const host: NavHost = {
    ctx: { say: (text: string) => said.push(text) } as unknown as Ctx,
    path: "/code/Atelier",
    place: () => place,
    show: (next) => {
      place = next;
      shown.push(next);
    },
    reloadAll: vi.fn(async () => {}),
    undoBar: () => ({ el: document.createElement("div"), show: (text) => (bar.text = text), hide: () => (bar.text = null) }),
  };
  return { nav: createGitNav(host), host, shown, said, bar, at: () => place };
}

const outcome = (over: Partial<OpOutcome>): OpOutcome => ({
  done: true,
  summary: "Rebased feature/tunnel-ui onto main",
  conflicts: [],
  head: "abc1234",
  journal: 7,
  state: { kind: "none", cause: null, title: "", yours: null, theirs: null, step: null, total: null, plan: [], conflicts: 0, can_continue: false, can_skip: false },
  ...over,
});

describe("the flow between the git client's screens", () => {
  it("opens views inside their tab", () => {
    const { nav, at } = world();
    nav.openBlame("internal/tunnel/supervisor.go", "c58e2aa");
    expect(at()).toEqual({ tab: "history", view: { kind: "blame", file: "internal/tunnel/supervisor.go", at: "c58e2aa" } });
    nav.openWorktrees();
    expect(at()).toEqual({ tab: "branches", view: { kind: "worktrees" } });
    nav.openBranches("put-aside");
    expect(at()).toEqual({ tab: "branches", view: null, section: "put-aside" });
    nav.openHistory("e04a9d1");
    expect(at()).toEqual({ tab: "history", view: null, commit: "e04a9d1" });
    nav.openChanges("README.md");
    expect(at()).toEqual({ tab: "changes", view: null, file: "README.md" });
  });

  it("returns where you were when a heavy action is stopped", () => {
    const { nav, at } = world({ tab: "branches", view: null, section: "branches" });
    nav.openTidyUp({ base: "7f82d31" });
    expect(at().view).toEqual({ kind: "tidy", base: "7f82d31", plan: undefined });
    expect(isHeavy(at().view)).toBe(true);
    // It stops halfway: Resolve opens the conflict view over it.
    nav.openConflicts("tidy");
    expect(at()).toEqual({ tab: "changes", view: { kind: "conflicts", cause: "tidy" } });
    // Stop and go back: the tidy-up, then where that was started from.
    nav.back();
    expect(at().view?.kind).toBe("tidy");
    nav.back();
    expect(at()).toEqual({ tab: "branches", view: null, section: "branches" });
    // Nothing left to return to: History.
    nav.back();
    expect(at()).toEqual({ tab: "history", view: null });
  });

  it("lands every finish on History with the Undo bar", () => {
    const { nav, at, bar, host } = world({ tab: "branches", view: null });
    nav.openConflicts("merge");
    nav.finish("Merged main into feature/tunnel-ui", 12, "abc1234");
    expect(at()).toEqual({ tab: "history", view: null, commit: "abc1234" });
    expect(bar.text).toBe("Merged main into feature/tunnel-ui");
    expect(host.reloadAll).toHaveBeenCalled();
    // The way back was used up by finishing.
    nav.back();
    expect(at()).toEqual({ tab: "history", view: null });
  });

  it("sends an action's answer to the conflict view or to the finish", () => {
    const { nav, at, bar } = world({ tab: "branches", view: null });
    nav.after(outcome({ done: false, conflicts: ["a.go"], journal: null }), "rebase");
    expect(at()).toEqual({ tab: "changes", view: { kind: "conflicts", cause: "rebase" } });
    expect(bar.text).toBeNull();
    nav.after(outcome({}), "rebase");
    expect(at()).toEqual({ tab: "history", view: null, commit: "abc1234" });
    expect(bar.text).toBe("Rebased feature/tunnel-ui onto main");
  });

  it("says it in a pop-up when the Undo bar isn't there yet", () => {
    const { nav, host, said } = world();
    host.undoBar = () => null;
    nav.showUndoBar("Put aside “wip tray”");
    expect(said).toEqual(["Put aside “wip tray”"]);
  });
});

describe("switching the new screens on", () => {
  afterEach(() => setGitPreview(false));

  it("uses a screen once it says it is ready, or when previewing", () => {
    expect(gitReady(false)).toBe(false);
    expect(gitReady(true)).toBe(true);
    setGitPreview(true);
    expect(gitReady(false)).toBe(true);
  });
});
