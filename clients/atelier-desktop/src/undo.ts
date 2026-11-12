// Undo (area "flow"): the Undo bar that follows every finished action, the
// repository bar's Undo and Put aside buttons, and the banner that says
// something is still under way.
//
// Owned by the flow area. `screens/repo.ts` mounts what these functions
// return and never looks inside; the bodies below are small working
// stand-ins. Replace them (⌘Z, the Undo menu with Recent actions…, the
// styled bar) and set READY.

import "./styles/git-flow.css";

import { branchesApi, flowApi, type UndoOptions } from "./api";
import { h, mount, type Child } from "./dom";
import type { RepoHost } from "./screens/repo";

export const READY = false;

export interface UndoBar {
  el: HTMLElement;
  /** Shows the bar: `Rebased feature/tunnel-ui onto main · Undo`. It stays until the next action. */
  show(text: string, journal: number | null): void;
  hide(): void;
}

/**
 * Undoes the latest action. A refusal is asked about (put the changes
 * aside first; go back although commits were made since) or said.
 * Resolves true when something was undone.
 */
export async function runUndo(host: RepoHost, options: UndoOptions = {}): Promise<boolean> {
  const { ctx, path } = host;
  const outcome = await flowApi.undo(path, options);
  if (outcome.done) {
    ctx.say(outcome.title, outcome.put_aside ? "Your changes were put aside first." : "");
    await host.reloadAll();
    return true;
  }
  const refusal = outcome.refusal;
  if (!refusal) return false;
  if (refusal.can_put_aside && !options.put_aside) {
    const sure = await ctx.confirm({ title: "Put your changes aside first?", body: refusal.message, confirm: "Put aside and undo" });
    return sure ? runUndo(host, { ...options, put_aside: true }) : false;
  }
  if (refusal.can_force && !options.force) {
    const sure = await ctx.confirm({ title: "Go back anyway?", body: refusal.message, confirm: "Go back", danger: true });
    return sure ? runUndo(host, { ...options, force: true }) : false;
  }
  ctx.say(refusal.message, "", "quiet");
  return false;
}

export function undoBar(host: RepoHost): UndoBar {
  const text = h("span", { class: "grow clip" });
  const undo: HTMLButtonElement = h("button", {
    type: "button",
    class: "btn sm",
    text: "Undo",
    onclick: () =>
      void host.ctx.busy(undo, async () => {
        if (await runUndo(host)) bar.hide();
      }),
  });
  const el = h("div", { class: "undo-bar hidden", role: "status" }, text, undo);
  const bar: UndoBar = {
    el,
    show(words) {
      text.textContent = words;
      el.classList.remove("hidden");
    },
    hide() {
      el.classList.add("hidden");
    },
  };
  return bar;
}

/** The repository bar's Undo and Put aside buttons (before Fetch). */
export function repoBarButtons(host: RepoHost): Child {
  const { ctx, path } = host;
  const undo: HTMLButtonElement = h("button", {
    type: "button",
    class: "btn",
    text: "Undo",
    title: "Undo the last action here",
    onclick: () => void ctx.busy(undo, () => runUndo(host)),
  });
  const aside: HTMLButtonElement = h("button", {
    type: "button",
    class: "btn",
    text: "Put aside",
    title: "Put your uncommitted changes aside (stash)",
    onclick: () =>
      void ctx.busy(aside, async () => {
        const message = await ctx.prompt({ title: "Put changes aside", label: "What are they?", placeholder: "Half-done tunnel status badge", confirm: "Put aside" });
        if (message === null) return;
        const entry = await branchesApi.stashSave(path, message);
        host.nav.showUndoBar(`Put aside “${entry.message}”`);
        await host.reloadAll();
      }),
  });
  return [undo, aside];
}

export interface ProgressBanner {
  el: HTMLElement;
  /** Reads what is under way again and redraws. Never rejects. */
  refresh(): Promise<void>;
}

/** The strip under the bar while a merge, rebase, … is under way. */
export function progressBanner(host: RepoHost): ProgressBanner {
  const { ctx, path } = host;
  const el = h("div", { class: "flow-banner hidden", role: "status" });
  return {
    el,
    async refresh() {
      try {
        const state = await flowApi.state(path);
        if (state.kind === "none") {
          el.classList.add("hidden");
          return;
        }
        const stop: HTMLButtonElement = h("button", {
          type: "button",
          class: "btn sm",
          text: "Stop and go back",
          onclick: () =>
            void ctx.busy(stop, async () => {
              const outcome = await flowApi.abort(path);
              ctx.say(outcome.summary);
              host.nav.back();
            }),
        });
        mount(
          el,
          h("span", { class: "grow clip", text: state.title }),
          h("button", { type: "button", class: "btn sm", text: "Resolve…", onclick: () => host.nav.openConflicts() }),
          stop,
        );
        el.classList.remove("hidden");
      } catch {
        // A passing failure: the banner stays as it was.
      }
    },
  };
}
