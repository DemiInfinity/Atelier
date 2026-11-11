// The main window: the sidebar, the current screen, and the plumbing every
// screen shares (state, navigation, pop-ups, dialogs, studio events).

import { api, asError, on, type AppState, type Events, type NeedAction, type Overview, type Target, type Toast, type Tone } from "./api";
import type { Ctx, MenuItem, Screen } from "./ctx";
import { composeDialog, confirmDialog, conflictsDialog, logDialog, openMenu, promptDialog, typedConfirmDialog } from "./dialogs";
import { h, icon, mark, mount, settingsIcon } from "./dom";
import { needStep, routeFor } from "./logic/needs";
import { sidebarGroups, toggled } from "./logic/sidebar";
import { manageMenu } from "./manage";
import { sideRepos, type SideActions } from "./sideview";
import { sidebarKey, type Route } from "./routes";
import { addScreen } from "./screens/add";
import { deviceScreen } from "./screens/device";
import { messagesScreen } from "./screens/messages";
import { pullsScreen } from "./screens/pulls";
import { repoScreen } from "./screens/repo";
import { settingsScreen } from "./screens/settings";
import { todayScreen } from "./screens/today";
import { welcomeScreen } from "./screens/welcome";
import { toastLayer } from "./toastview";

export class Shell {
  private state!: AppState;
  private view: Overview | null = null;
  private route: Route = { screen: "today" };
  private screen: Screen | null = null;
  private connected = true;
  private refreshing: Promise<void> | null = null;
  private refreshAgain = false;
  private sayCount = 0;

  private readonly root: HTMLElement;
  private readonly sidebar = h("aside", { class: "sidebar" });
  private readonly stage = h("main", { class: "stage" });
  private readonly toasts = toastLayer((target) => this.follow(target));
  /** The sidebar's "Other folders" group starts closed. */
  private othersOpen = false;

  readonly ctx: Ctx = {
    app: () => this.state,
    reloadApp: async (fresh = false) => {
      this.state = await api.appState(fresh);
      this.drawSidebar();
      return this.state;
    },
    signedOut: async () => {
      this.state = await api.appState();
      if (!this.state.signed_in) this.welcome();
    },
    overview: () => this.view,
    refresh: () => this.refresh(),
    go: (route) => this.go(route),
    follow: (target) => this.follow(target),
    toast: (toast) => this.toasts.queue.push(toast),
    say: (text, sub, tone) => this.say(text, sub, tone),
    fail: (error) => this.fail(error),
    openLink: (url) => this.openLink(url),
    confirm: confirmDialog,
    prompt: promptDialog,
    confirmTyped: typedConfirmDialog,
    compose: (options) => composeDialog(options, { openLink: (url) => this.openLink(url) }),
    showLog: (repo, runId, webPath) => logDialog(repo, runId, webPath ?? null, (path) => void this.openWeb(path)),
    showConflicts: (options) =>
      conflictsDialog(options, this.state.editor.label, (path) =>
        void this.busy(null, async () => {
          const label = await api.repoOpen(path);
          this.say(`Opened in ${label}.`);
        }),
      ),
    runNeed: (action) => this.runNeed(action),
    busy: (button, work) => this.busy(button, work),
    menu: (anchor: HTMLElement, items: MenuItem[]) => openMenu(anchor, items),
  };

  constructor(root: HTMLElement) {
    this.root = root;
    document.body.appendChild(this.toasts.el);
  }

  async start(): Promise<void> {
    this.state = await api.appState();
    document.body.classList.add(`os-${this.state.os}`);
    this.listen();
    if (!this.state.signed_in) {
      this.welcome();
    } else if (!this.state.prefs.folders_done) {
      // Signed in elsewhere (with `atl`, say) and never shown where the
      // app looks: show "Find your folders" once.
      this.welcome("folders");
    } else {
      this.enter();
    }
  }

  /** The current route, for the self-test. */
  get where(): Route {
    return this.route;
  }

  // ------------------------------------------------------------- screens

  /** First run: no sidebar, just the welcome steps. */
  private welcome(startAt?: "folders"): void {
    this.screen?.dispose?.();
    this.screen = null;
    this.view = null;
    this.toasts.queue.clear();
    const screen = welcomeScreen(
      this.ctx,
      async () => {
        await api.foldersDone().catch(() => {});
        this.state = await api.appState();
        this.enter();
      },
      startAt,
    );
    this.screen = screen;
    mount(this.root, screen.el);
  }

  /** Signed in: the sidebar and Today. */
  private enter(): void {
    mount(this.root, this.sidebar, this.stage);
    this.drawSidebar();
    this.go({ screen: "today" });
    void this.refresh();
  }

  go(route: Route): void {
    if (!this.state.signed_in) return;
    this.screen?.dispose?.();
    this.route = route;
    const screen = this.make(route);
    this.screen = screen;
    mount(this.stage, screen.el);
    this.drawSidebar();
    if (this.view) screen.onOverview?.(this.view);
  }

  private make(route: Route): Screen {
    switch (route.screen) {
      case "today":
        return todayScreen(this.ctx, route);
      case "pulls":
        return pullsScreen(this.ctx, route);
      case "repo":
        return repoScreen(this.ctx, route);
      case "device":
        return deviceScreen(this.ctx, route);
      case "add":
        return addScreen(this.ctx, route);
      case "settings":
        return settingsScreen(this.ctx, route);
      case "messages":
        return messagesScreen(this.ctx, route);
    }
  }

  follow(target: Target): void {
    const route = routeFor(target);
    if (route) this.go(route);
    else if (target.screen === "web") void this.openWeb(target.web_path);
  }

  private async openWeb(webPath: string): Promise<void> {
    try {
      await api.openWeb(webPath);
    } catch (e) {
      this.fail(e);
    }
  }

  // --------------------------------------------------------------- state

  refresh(): Promise<void> {
    if (this.refreshing) {
      this.refreshAgain = true;
      return this.refreshing;
    }
    this.refreshing = (async () => {
      // Yield first, so `refreshing` is assigned before anything below can
      // finish and clear it.
      await Promise.resolve();
      try {
        do {
          this.refreshAgain = false;
          if (!this.state.signed_in) return;
          this.view = await api.overview();
          this.drawSidebar();
          this.screen?.onOverview?.(this.view);
        } while (this.refreshAgain);
      } catch (e) {
        const err = asError(e);
        if (err.kind === "not_signed_in" || err.kind === "no_studio") return;
        this.fail(e);
      } finally {
        this.refreshing = null;
      }
    })();
    return this.refreshing;
  }

  private listen(): void {
    const forward = <K extends keyof Events>(name: K) =>
      void on(name, (payload) => {
        this.screen?.onEvent?.(name, payload);
      });
    void on("notify", (toast) => {
      this.toasts.queue.push(toast);
      void this.refresh();
    });
    void on("status", (payload) => {
      void this.refresh();
      this.screen?.onEvent?.("status", payload);
    });
    void on("runner", (payload) => {
      void this.refresh();
      this.screen?.onEvent?.("runner", payload);
    });
    forward("remote");
    void on("connection", (up) => {
      this.connected = up;
      this.drawSidebar();
      if (up) void this.refresh();
    });
    void on("signed-out", async () => {
      this.state = await api.appState();
      if (!this.state.signed_in) this.welcome();
    });
    void on("navigate", (target) => this.follow(target));
    // Coming back to the window is a good moment to look again.
    window.addEventListener("focus", () => {
      void this.refresh();
      // What this device may do (rename, delete) is switched in the studio.
      if (this.state.signed_in) {
        void api.appState(true).then(
          (state) => {
            if (state.signed_in) this.state = state;
          },
          () => {},
        );
      }
    });
  }

  // ------------------------------------------------------------- helpers

  /** A link from Markdown: straight to the studio or GitHub, anywhere else only after a yes. */
  private openLink(url: string): void {
    void this.busy(null, async () => {
      try {
        await api.openLink(url, false);
      } catch (e) {
        if (asError(e).kind !== "link_outside") throw e;
        const mail = url.startsWith("mailto:");
        const sure = await confirmDialog({
          title: mail ? "Write to this address?" : "Open this link in your browser?",
          body: mail ? url.slice("mailto:".length) : url,
          confirm: mail ? "Open mail" : "Open link",
        });
        if (sure) await api.openLink(url, true);
      }
    });
  }

  private say(text: string, sub = "", tone: Tone = "yours"): void {
    this.sayCount += 1;
    this.toasts.queue.push({
      id: `say:${this.sayCount}`,
      text,
      sub,
      tone,
      moment: null,
      action: null,
      target: routeTarget(this.route),
    });
  }

  private fail(error: unknown): void {
    const err = asError(error);
    if (err.kind === "unauthorized" || err.kind === "revoked") {
      void api.appState().then((state) => {
        this.state = state;
        if (!state.signed_in) this.welcome();
      });
    }
    this.sayCount += 1;
    this.toasts.queue.push({
      id: `fail:${err.kind}:${err.message}`,
      text: err.message || "Something went wrong.",
      sub: err.hint ?? "",
      tone: "fail",
      moment: null,
      action: null,
      target: routeTarget(this.route),
    });
  }

  private async busy<T>(button: HTMLButtonElement | null, work: () => Promise<T>): Promise<T | undefined> {
    if (button?.disabled) return undefined;
    if (button) {
      button.disabled = true;
      button.classList.add("busy");
    }
    try {
      return await work();
    } catch (e) {
      this.fail(e);
      return undefined;
    } finally {
      if (button) {
        button.disabled = false;
        button.classList.remove("busy");
      }
    }
  }

  private async runNeed(action: NeedAction): Promise<void> {
    const step = needStep(action);
    switch (step.do) {
      case "go":
        this.go(step.route);
        return;
      case "log":
        logDialog(step.repo, step.runId, step.webPath, (path) => void this.openWeb(path));
        return;
      case "web":
        await this.openWeb(step.webPath);
        return;
      case "push":
        try {
          const pushed = await api.repoPush(step.path);
          if (pushed.toast) this.toasts.queue.push(pushed.toast);
          else this.say(`Pushed ${pushed.branch}.`);
        } catch (e) {
          this.fail(e);
        }
        await this.refresh();
        return;
    }
  }

  /** "Add a folder…": the chooser, then look again. */
  private async addFolder(button: HTMLButtonElement | null): Promise<void> {
    await this.busy(button, async () => {
      const added = await api.addWatchFolder();
      if (added === null) return;
      this.state = await api.appState();
      await this.refresh();
      const found = (this.view?.copies.length ?? 0) + (this.view?.others.length ?? 0);
      this.say(`Looking through ${added}.`, found === 0 ? "No git folders were found in it." : "");
    });
  }

  // ------------------------------------------------------------- sidebar

  /** Folds an owner's group shut or open, and remembers it. */
  private fold(key: string, shut: boolean): void {
    this.state.prefs.collapsed = toggled(this.state.prefs.collapsed, key, shut);
    this.drawSidebar();
    api.setGroupCollapsed(key, shut).catch((e) => this.fail(e));
  }

  /** After a pin, rename or delete from the sidebar: the open repository looks again. */
  private afterManage(path: string): void {
    if (this.route.screen === "repo" && this.route.path === path) this.screen?.onOverview?.(this.view as Overview);
  }

  private drawSidebar(): void {
    if (!this.state?.signed_in) return;
    const key = sidebarKey(this.route);
    const view = this.view;
    const current = (id: string) => (key === id ? "page" : undefined);
    const line = this.state.offline || !this.connected ? "Can’t reach your studio" : "Connected to your studio";

    const link = (id: "today" | "pulls", label: string, count: number, hot: boolean) =>
      h(
        "button",
        { type: "button", class: "side-link", "aria-current": current(id), onclick: () => this.go({ screen: id }) },
        icon(id),
        h("span", { class: "grow", text: label }),
        count > 0 ? h("span", { class: ["count", hot && "hot"], text: count }) : null,
      );

    const groups = sidebarGroups(view?.copies ?? [], view?.others ?? [], this.state.prefs.collapsed);
    const actions: SideActions = {
      currentKey: key,
      open: (copy) => this.go({ screen: "repo", path: copy.path }),
      more: (copy, anchor) => openMenu(anchor, manageMenu(this.ctx, copy, () => this.afterManage(copy.path))),
      toggle: (group) => this.fold(group.key, !group.collapsed),
      publish: (copy) => this.go({ screen: "add", tab: "publish", path: copy.path }),
      othersOpen: this.othersOpen,
      toggleOthers: () => {
        this.othersOpen = !this.othersOpen;
        this.drawSidebar();
      },
    };

    mount(
      this.sidebar,
      h("div", { class: "drag-strip", "data-tauri-drag-region": true }),
      h(
        "button",
        { type: "button", class: "brand", title: "Settings", onclick: () => this.go({ screen: "settings" }) },
        mark(28),
        h(
          "span",
          { class: "col grow" },
          h("span", { class: "brand-name", text: "Atelier" }),
          h(
            "span",
            { class: "brand-line" },
            h("span", { class: ["dot xs", this.state.offline || !this.connected ? "warn" : "ok"], "aria-hidden": "true" }),
            line,
          ),
        ),
      ),
      h(
        "nav",
        { class: "links", "aria-label": "Studio" },
        link("today", "Today", view?.needs.length ?? 0, true),
        link("pulls", "Pull requests", view?.pulls_open ?? 0, false),
      ),
      h(
        "div",
        { class: "section-head" },
        h("span", { class: "eyebrow grow", text: `On this ${this.state.machine}` }),
        h(
          "button",
          { type: "button", class: "add", "aria-label": "Add a repository", title: "Add a repository", onclick: () => this.go({ screen: "add" }) },
          icon("plus", 12, 2.4),
        ),
      ),
      h(
        "nav",
        { class: "repos", "aria-label": "Repositories" },
        sideRepos(groups, view?.others ?? [], actions),
        view && view.copies.length === 0 && view.others.length === 0
          ? h(
              "div",
              { class: "side-empty" },
              h("span", { text: `No repositories found on this ${this.state.machine} yet` }),
              h("button", { type: "button", class: "btn sm", text: "Add a folder…", onclick: (e) => void this.addFolder(e.currentTarget as HTMLButtonElement) }),
              h("button", { type: "button", class: "btn sm", text: "Clone…", onclick: () => this.go({ screen: "add", tab: "clone" }) }),
            )
          : null,
      ),
      h(
        "div",
        { class: "foot" },
        h(
          "button",
          { type: "button", class: "side-link side-device", "aria-current": current("device"), onclick: () => this.go({ screen: "device" }) },
          h("span", { class: ["dot sm", !(view?.runner.running ?? false) && "off"], "aria-hidden": "true" }),
          h(
            "span",
            { class: "col grow" },
            h("span", { style: { fontWeight: "500" }, text: view?.device_name ?? this.state.device_name }),
            h("span", { class: "muted small", text: view?.runner.line ?? "" }),
          ),
        ),
        h(
          "button",
          { type: "button", class: "side-link", "aria-current": current("settings"), onclick: () => this.go({ screen: "settings" }) },
          settingsIcon(),
          "Settings",
        ),
      ),
    );
  }
}

function routeTarget(route: Route): Target {
  switch (route.screen) {
    case "repo":
      return { screen: "repo", path: route.path };
    case "pulls":
      return { screen: "pulls" };
    case "device":
      return { screen: "device" };
    default:
      return { screen: "today" };
  }
}

/** A toast for tests and screens that build one by hand. */
export function note(id: string, text: string, sub = "", tone: Tone = "yours"): Toast {
  return { id, text, sub, tone, moment: null, action: null, target: { screen: "today" } };
}
