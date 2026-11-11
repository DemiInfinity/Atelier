// A working copy: the bar (branch, fetch/pull/push, open in an editor) and
// the tabs under it. The bar stays mounted while the tabs swap, so switching
// between History, Changes and the rest never goes through `ctx.go`. A
// repository always opens on History.

import "../styles/repo.css";

import { api, asError, type Events, type RepoView } from "../api";
import type { Ctx, MenuItem, Screen } from "../ctx";
import { h, icon, mount } from "../dom";
import { manageMenu } from "../manage";
import { DEFAULT_REPO_TAB, type RepoTab, type Route } from "../routes";
import { pullsView } from "./pulls";
import { aheadBehind, branchesTab } from "./repo-branches";
import { changesTab } from "./repo-changes";
import { historyTab } from "./repo-history";
import { issuesTab } from "./repo-issues";
import { runsTab } from "./repo-runs";

export interface TabSpec {
  id: RepoTab;
  label: string;
  count: number | null;
}

/**
 * The tabs of a repository, in order: History first, then Changes. Pull
 * requests, Issues and Actions only exist for a repository in the studio.
 */
export function repoTabs(view: Pick<RepoView, "changes" | "branches" | "open_pulls" | "full_name">): TabSpec[] {
  const inStudio = view.full_name !== null;
  return [
    { id: "history", label: "History", count: null },
    { id: "changes", label: "Changes", count: view.changes },
    ...(inStudio
      ? ([
          { id: "pulls", label: "Pull requests", count: view.open_pulls },
          { id: "issues", label: "Issues", count: null },
        ] as TabSpec[])
      : []),
    { id: "branches", label: "Branches", count: view.branches },
    ...(inStudio ? ([{ id: "actions", label: "Actions", count: null }] as TabSpec[]) : []),
  ];
}

/** What the tabs may ask of the screen around them. */
export interface RepoHost {
  ctx: Ctx;
  /** The working copy's folder: every repo command takes it. */
  path: string;
  /** The bar's data as last read. */
  view(): RepoView;
  /** Reads the view again and redraws the bar. */
  reloadBar(): Promise<void>;
  /** After anything that changed the repository: the bar, the current tab, the sidebar. */
  reloadAll(): Promise<void>;
  showTab(tab: RepoTab): void;
  push(button: HTMLButtonElement | null): Promise<void>;
  /** Switches branch; a refusal is shown as it was said. */
  switchBranch(name: string, button: HTMLButtonElement | null): Promise<void>;
  /** Asks for a name and makes a branch (from `from`, or from where you are). */
  newBranch(from: string | null): Promise<void>;
}

/** One tab's body. `reload` never rejects: it shows what went wrong itself. */
export interface TabView {
  el: HTMLElement;
  reload(): Promise<void>;
  /** The bar's data was read again. */
  onView?(view: RepoView): void;
  /** An event from the Rust side, for tabs that follow the studio. */
  onEvent?<K extends keyof Events>(name: K, payload: Events[K]): void;
  dispose?(): void;
}

export function repoScreen(ctx: Ctx, route: Extract<Route, { screen: "repo" }>): Screen {
  const path = route.path;
  let view: RepoView | null = null;
  let tab: RepoTab = route.tab ?? DEFAULT_REPO_TAB;
  let current: TabView | null = null;
  let disposed = false;
  // True while our own `ctx.refresh()` is on its way, so the overview it
  // brings doesn't make us read everything a second time.
  let ownRefresh = false;

  const bar = h("header", { class: "repo-bar", "data-tauri-drag-region": true });
  const body = h("div", { class: "repo-body" });
  const el = h("div", { class: "screen repo" }, h("div", { class: "loading", text: "Opening the folder…" }));

  const host: RepoHost = {
    ctx,
    path,
    view: () => view as RepoView,
    reloadBar: loadView,
    reloadAll,
    showTab,
    push,
    switchBranch,
    newBranch,
  };

  async function loadView(): Promise<void> {
    const next = await api.repoView(path);
    if (disposed) return;
    view = next;
    drawBar();
    current?.onView?.(next);
  }

  async function reloadAll(): Promise<void> {
    try {
      await loadView();
    } catch (e) {
      ctx.fail(e);
    }
    await current?.reload();
    ownRefresh = true;
    void ctx.refresh().finally(() => {
      ownRefresh = false;
    });
  }

  /** Looks again without fuss (the window came back, the studio said something). */
  async function lookAgain(): Promise<void> {
    if (disposed || !view) return;
    try {
      await loadView();
    } catch {
      // A passing failure: what is on screen stays until the next look.
    }
    if (!disposed && tab === "changes") await current?.reload();
  }

  function makeTab(id: RepoTab): TabView {
    const fullName = view?.full_name ?? null;
    switch (id) {
      case "changes":
        return changesTab(host);
      case "branches":
        return branchesTab(host);
      case "pulls":
        if (fullName) return pullsView(ctx, {}, { repo: fullName, name: (view as RepoView).name });
        break;
      case "issues":
        if (fullName) return issuesTab(host, fullName);
        break;
      case "actions":
        if (fullName) return runsTab(host, fullName);
        break;
    }
    return historyTab(host);
  }

  function showTab(next: RepoTab): void {
    current?.dispose?.();
    // A tab that needs the studio falls back to History without it.
    tab = view && repoTabs(view).some((t) => t.id === next) ? next : DEFAULT_REPO_TAB;
    current = makeTab(tab);
    mount(body, current.el);
    drawBar();
    // The studio tabs read by themselves when they are made.
    if (tab === "history" || tab === "changes" || tab === "branches") void current.reload();
  }

  /** After a pin, a rename or a delete: read everything again. */
  async function afterManage(): Promise<void> {
    const before = view?.full_name ?? null;
    try {
      await loadView();
    } catch (e) {
      ctx.fail(e);
      return;
    }
    // Renamed or gone from the studio: the studio tabs belong to the old name.
    if ((view?.full_name ?? null) !== before && (tab === "pulls" || tab === "issues" || tab === "actions")) showTab(tab);
  }

  function isPinned(): boolean {
    const overview = ctx.overview();
    return [...(overview?.copies ?? []), ...(overview?.others ?? [])].some((c) => c.path === (view?.path ?? path) && c.pinned);
  }

  // ---------------------------------------------------------------- actions

  async function push(button: HTMLButtonElement | null): Promise<void> {
    await ctx.busy(button, async () => {
      const result = await api.repoPush(path);
      if (result.toast) ctx.toast(result.toast);
      else ctx.say(`Pushed ${result.branch}.`);
      await reloadAll();
    });
  }

  async function switchBranch(name: string, button: HTMLButtonElement | null): Promise<void> {
    // `busy` shows a refusal (uncommitted changes would be lost) through `ctx.fail`.
    await ctx.busy(button, async () => {
      await api.repoSwitch(path, name);
      ctx.say(`Switched to ${name}.`);
      await reloadAll();
    });
  }

  async function newBranch(from: string | null): Promise<void> {
    const name = (
      await ctx.prompt({
        title: "New branch",
        label: "Branch name",
        placeholder: "feature/short-name",
        confirm: "Create branch",
        mono: true,
      })
    )?.trim();
    if (!name) return;
    await ctx.busy(null, async () => {
      await api.repoNewBranch(path, name, from);
      ctx.say(`Made the branch ${name}.`);
      await reloadAll();
    });
  }

  async function branchMenu(anchor: HTMLButtonElement): Promise<void> {
    const branches = await ctx.busy(anchor, () => api.repoBranches(path));
    if (!branches || disposed) return;
    const items: MenuItem[] = branches.map((b) => ({
      label: b.name,
      hint: aheadBehind(b.ahead, b.behind) || undefined,
      current: b.current,
      mono: true,
      pick: () => {
        if (!b.current) void switchBranch(b.name, null);
      },
    }));
    if (items.length > 0) items.push({ separator: true });
    items.push({ label: "New branch…", pick: () => void newBranch(null) });
    ctx.menu(anchor, items);
  }

  function openIn(button: HTMLButtonElement | null, preset?: string): void {
    void ctx.busy(button, async () => {
      const label = await api.repoOpen(path, preset);
      ctx.say(`Opened in ${label}.`);
    });
  }

  // -------------------------------------------------------------------- bar

  function drawBar(): void {
    if (!view) return;
    const v = view;

    const branchIcon = icon("branch", 13);
    branchIcon.classList.add("repo-branch-icon");
    const branch = h(
      "button",
      {
        type: "button",
        class: "btn repo-branch",
        title: "Switch branch",
        onclick: (e) => void branchMenu(e.currentTarget as HTMLButtonElement),
      },
      branchIcon,
      h("span", { class: "clip", text: v.branch ?? "detached" }),
      icon("chevron", 10, 2.4),
    );

    const fetch = h("button", {
      type: "button",
      class: "btn",
      text: "Fetch",
      onclick: (e) =>
        void ctx.busy(e.currentTarget as HTMLButtonElement, async () => {
          await api.repoFetch(path);
          ctx.say("Fetched.");
          await reloadAll();
        }),
    });
    const pull = h("button", {
      type: "button",
      class: "btn",
      text: v.behind > 0 ? `Pull ↓${v.behind}` : "Pull",
      onclick: (e) =>
        void ctx.busy(e.currentTarget as HTMLButtonElement, async () => {
          const n = await api.repoPull(path);
          ctx.say(n > 0 ? `Pulled ${n === 1 ? "1 commit" : `${n} commits`}.` : "Already up to date.");
          await reloadAll();
        }),
    });
    const pushButton = h("button", {
      type: "button",
      class: ["btn", v.ahead > 0 && "hot"],
      text: v.ahead > 0 ? `Push ↑${v.ahead}` : "Push",
      onclick: (e) => void push(e.currentTarget as HTMLButtonElement),
    });

    const open = h(
      "span",
      { class: "repo-split" },
      h("button", {
        type: "button",
        class: "btn",
        text: `Open in ${v.editor.label}`,
        onclick: (e) => openIn(e.currentTarget as HTMLButtonElement),
      }),
      h(
        "button",
        {
          type: "button",
          class: "btn",
          "aria-label": "Open in another editor",
          title: "Open in another editor",
          onclick: (e) =>
            ctx.menu(
              e.currentTarget as HTMLButtonElement,
              ctx.app().editors.map((editor) => ({
                label: editor.label,
                current: editor.id === v.editor.id,
                pick: () => openIn(null, editor.id),
              })),
            ),
        },
        icon("chevron", 10, 2.4),
      ),
    );

    const handOver = v.git_tool
      ? h("button", {
          type: "button",
          class: "btn",
          text: `Hand over to ${v.git_tool.label}`,
          title: "For anything heavier than this app does",
          onclick: (e) =>
            void ctx.busy(e.currentTarget as HTMLButtonElement, async () => {
              const label = await api.repoHandOver(path);
              ctx.say(`Handed over to ${label}.`);
            }),
        })
      : null;

    const webPath = v.web_path;
    const studio = webPath
      ? h("button", {
          type: "button",
          class: "btn link",
          text: "View in studio ↗",
          onclick: (e) => void ctx.busy(e.currentTarget as HTMLButtonElement, () => api.openWeb(webPath)),
        })
      : [
          h("span", { class: "muted small repo-quiet", text: "Not in your studio yet" }),
          h("button", {
            type: "button",
            class: "btn",
            text: "Publish…",
            onclick: () => ctx.go({ screen: "add", tab: "publish", path: v.path }),
          }),
        ];

    const tabButton = (spec: TabSpec) =>
      h(
        "button",
        {
          type: "button",
          class: "repo-tab",
          dataset: { tab: spec.id },
          "aria-current": tab === spec.id ? "page" : undefined,
          onclick: () => {
            if (tab !== spec.id) showTab(spec.id);
          },
        },
        spec.label,
        spec.count !== null && spec.count > 0 ? h("span", { class: "count", text: spec.count }) : null,
      );

    const pinned = isPinned();
    const managed = { path: v.path, name: v.name, full_name: v.full_name, pinned };
    const pin: HTMLButtonElement = h(
      "button",
      {
        type: "button",
        class: ["btn repo-icon-btn repo-pin", pinned && "on"],
        title: pinned ? "Unpin from the top of the sidebar" : "Pin to the top of the sidebar",
        "aria-label": pinned ? "Unpin" : "Pin",
        "aria-pressed": pinned ? "true" : "false",
        onclick: () =>
          void ctx.busy(pin, async () => {
            await api.repoSetPinned(v.path, !pinned);
            await ctx.refresh();
            drawBar();
          }),
      },
      icon("pin", 14, 2),
    );
    const more: HTMLButtonElement = h(
      "button",
      {
        type: "button",
        class: "btn repo-icon-btn repo-more",
        title: "More",
        "aria-label": `More for ${v.name}`,
        "aria-haspopup": "menu",
        onclick: () => ctx.menu(more, manageMenu(ctx, managed, () => afterManage())),
      },
      icon("more", 14, 3),
    );

    mount(
      bar,
      h(
        "div",
        { class: "repo-bar-top", "data-tauri-drag-region": true },
        h(
          "h1",
          { class: "repo-title" },
          v.owner ? h("span", { class: "owner", text: `${v.owner} / ` }) : null,
          v.name,
        ),
        branch,
        h("div", { class: "repo-actions" }, fetch, pull, pushButton, open, handOver, studio, pin, more),
      ),
      h("nav", { class: "repo-tabs", "aria-label": "Repository", "data-tauri-drag-region": true }, repoTabs(v).map(tabButton)),
    );
  }

  // ------------------------------------------------------------------ start

  const onFocus = () => void lookAgain();
  window.addEventListener("focus", onFocus);

  void (async () => {
    try {
      await loadView();
    } catch (e) {
      if (disposed) return;
      const err = asError(e);
      mount(
        el,
        h("header", { class: "screen-head", "data-tauri-drag-region": true }, h("h1", { class: "h-screen", text: "This folder can’t be opened" })),
        h(
          "div",
          { class: "screen-body repo-gone" },
          h("p", { class: "selectable", text: err.message || "The folder couldn’t be read." }),
          err.hint ? h("p", { class: "muted", text: err.hint }) : null,
          h("p", { class: "muted mono small selectable", text: path }),
          h("div", null, h("button", { type: "button", class: "btn", text: "Back to Today", onclick: () => ctx.go({ screen: "today" }) })),
        ),
      );
      return;
    }
    if (disposed) return;
    mount(el, bar, body);
    showTab(tab);
  })();

  return {
    el,
    dispose() {
      disposed = true;
      window.removeEventListener("focus", onFocus);
      current?.dispose?.();
    },
    onOverview() {
      if (!ownRefresh) void lookAgain();
      else drawBar();
    },
    onEvent(name, payload) {
      current?.onEvent?.(name, payload);
    },
  };
}
