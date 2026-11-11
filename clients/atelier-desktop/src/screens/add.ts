// Add a repository: a card with three ways in. Publish a folder that is
// already on this machine, clone one from the studio (or from GitHub, by
// mirroring it into the studio first), or start a new one from nothing.

import "../styles/add.css";
import { api, asError, type Account, type Added, type FolderPlan, type Hit, type SearchView } from "../api";
import type { Ctx, Screen } from "../ctx";
import { checkbox, h, mount, type Child } from "../dom";
import type { AddTab, Route } from "../routes";

// ------------------------------------------------------------------ logic

/** How an account reads in the account menu. */
export function accountLabel(account: Account): string {
  return account.kind === "user" ? `${account.name} (personal)` : `${account.display} (company)`;
}

export interface PublishStep {
  /** `none`: no folder yet. `init`: not a git folder. `open`: nothing to publish. */
  kind: "none" | "init" | "open" | "publish";
  label: string;
  /** Why the button isn't "Publish", in a sentence. */
  note: string | null;
}

/** What the primary button does for a picked folder. */
export function publishStep(plan: FolderPlan | null): PublishStep {
  if (!plan) return { kind: "none", label: "Publish to Atelier", note: null };
  if (!plan.is_repo) {
    return {
      kind: "init",
      label: "Make it a git folder",
      note: "This isn't a git folder yet. Atelier can make it one; your files stay as they are.",
    };
  }
  if (plan.in_studio) {
    return { kind: "open", label: "Open it", note: `This folder is already in your studio as ${plan.in_studio}.` };
  }
  if (plan.commits === 0) {
    return { kind: "open", label: "Open it", note: "It needs a first commit before it can be published." };
  }
  return { kind: "publish", label: "Publish to Atelier", note: null };
}

/** A repository name is one word: no spaces or slashes, and not empty. */
export function nameOk(name: string): boolean {
  const trimmed = name.trim();
  return trimmed.length > 0 && !/[\s/\\]/.test(trimmed);
}

// ----------------------------------------------------------------- screen

interface TabView {
  panel: HTMLElement;
  foot: HTMLElement;
  primary: HTMLButtonElement;
}

const TABS: { id: AddTab; label: string }[] = [
  { id: "publish", label: "Publish a folder" },
  { id: "clone", label: "Clone" },
  { id: "new", label: "Start new" },
];

export function addScreen(ctx: Ctx, route: Extract<Route, { screen: "add" }>): Screen {
  const app = ctx.app();
  const machine = app.machine;
  let disposed = false;
  let current: AddTab = route.tab ?? "publish";
  let searchTimer: ReturnType<typeof setTimeout> | undefined;

  // Each tab is built the first time it is opened and kept, so what was
  // typed survives a look at another tab.
  const built = new Map<AddTab, TabView>();

  const tablist = h("div", { class: "add-tabs", role: "tablist", "aria-label": "How to add it" });
  const body = h("div", { class: "add-body", role: "tabpanel", id: "add-panel" });
  const footer = h("footer", { class: "add-foot" });

  const el = h(
    "div",
    { class: "screen add-screen" },
    h("div", { class: "drag-strip", "data-tauri-drag-region": true }),
    h(
      "div",
      { class: "add-ground" },
      h(
        "section",
        { class: "add-card", "aria-label": "Add a repository" },
        h("header", { class: "add-head" }, h("h1", { class: "add-title", text: "Add a repository" }), tablist),
        body,
        footer,
      ),
    ),
  );

  function draw(): void {
    mount(
      tablist,
      TABS.map((tab) =>
        h("button", {
          type: "button",
          role: "tab",
          class: "add-tab",
          "aria-selected": tab.id === current ? "true" : "false",
          "aria-controls": "add-panel",
          text: tab.label,
          onclick: () => {
            current = tab.id;
            draw();
          },
        }),
      ),
    );
    let view = built.get(current);
    if (!view) {
      view = current === "publish" ? publishTab() : current === "clone" ? cloneTab() : newTab();
      built.set(current, view);
    }
    mount(body, view.panel);
    mount(
      footer,
      view.foot,
      h("button", { type: "button", class: "btn lg", text: "Cancel", onclick: () => ctx.go({ screen: "today" }) }),
      view.primary,
    );
  }

  function accountSelect(id: string): HTMLSelectElement {
    return h(
      "select",
      { id, class: "select" },
      app.accounts.map((account) => h("option", { value: account.name, text: accountLabel(account) })),
    );
  }

  const field = (id: string, label: string, control: HTMLElement) =>
    h("div", { class: "field add-field" }, h("label", { for: id, text: label }), control);

  const intro = (text: string) => h("span", { class: "soft", text });

  // ------------------------------------------------------ publish a folder

  function publishTab(): TabView {
    let plan: FolderPlan | null = null;
    let isPrivate = true;
    let mirror = false;

    const where = h("span", { class: "col grow" });
    const account = accountSelect("add-publish-account");
    const name = h("input", { id: "add-publish-name", class: "input", type: "text", autocomplete: "off", spellcheck: "false", oninput: () => sync() });
    const privateRow = checkbox({
      checked: true,
      label: ["Private ", h("span", { class: "muted", text: "· only you can see it" })],
      onchange: (next) => (isPrivate = next),
    });
    const mirrorRow = app.tether
      ? checkbox({
          checked: false,
          blue: true,
          label: ["Also mirror it to GitHub ", h("span", { class: "add-via", text: "· via Tether" })],
          onchange: (next) => (mirror = next),
        })
      : null;
    const note = h("span", { class: "add-note", "aria-live": "polite" });
    const foot = h("span", { class: "add-foot-note", text: "Your folder stays where it is." });
    const primary = h("button", { type: "button", class: "btn lg primary", onclick: () => void act() });

    const choose = h("button", {
      type: "button",
      class: "btn",
      text: "Choose…",
      onclick: () =>
        void ctx.busy(choose, async () => {
          const picked = await api.chooseFolder();
          if (!picked || disposed) return;
          plan = picked;
          name.value = picked.name;
          sync();
        }),
    });

    /** Brings the folder box, the note and the button in line with the plan. */
    function sync(): void {
      const step = publishStep(plan);
      mount(
        where,
        plan
          ? [h("span", { class: "add-path mono clip selectable", text: plan.display }), h("span", { class: "muted", text: plan.summary })]
          : h("span", { class: "muted", text: "No folder chosen yet" }),
      );
      note.textContent = step.note ?? "";
      note.classList.toggle("hidden", !step.note);
      // The account and name only matter when there is something to publish.
      const idle = step.kind === "init" || step.kind === "open";
      for (const control of [account, name, ...[privateRow, mirrorRow].map((row) => row?.querySelector("input"))]) {
        if (control) control.disabled = idle;
      }
      primary.textContent = step.label;
      primary.disabled = step.kind === "none" || (step.kind === "publish" && (!account.value || !nameOk(name.value)));
    }

    async function publish(folder: FolderPlan): Promise<Added | null> {
      const send = (replace: boolean) => api.publish(folder.path, account.value, name.value.trim(), isPrivate, replace);
      try {
        return await send(false);
      } catch (e) {
        const err = asError(e);
        if (err.kind !== "origin_exists") throw e;
        const sure = await ctx.confirm({
          title: err.message,
          body: err.hint ?? "Publishing points this folder at your studio instead.",
          confirm: "Publish anyway",
        });
        return sure ? send(true) : null;
      }
    }

    function act(): Promise<void | undefined> {
      const folder = plan;
      const step = publishStep(folder);
      if (!folder || step.kind === "none") return Promise.resolve();
      return ctx.busy(primary, async () => {
        if (step.kind === "open") {
          ctx.go({ screen: "repo", path: folder.path, tab: "changes" });
          return;
        }
        if (step.kind === "init") {
          await api.folderInit(folder.path);
          await ctx.refresh();
          ctx.say("It's a git folder now.", "Commit something first, then publish it.");
          ctx.go({ screen: "repo", path: folder.path, tab: "changes" });
          return;
        }
        const added = await publish(folder);
        if (!added) return;
        await ctx.refresh();
        if (mirror) {
          // The app can't set the mirror up itself; the studio's page can.
          ctx.say("Published.", "Finish the GitHub mirror in your studio.");
          try {
            await api.openWeb(`${added.web_path}/settings/tether`);
          } catch (e) {
            ctx.fail(e);
          }
        } else {
          ctx.say("Published.", added.note ?? `${added.full_name} is in your studio now.`);
        }
        ctx.go({ screen: "repo", path: added.path, tab: "changes" });
      });
    }

    const panel = h(
      "div",
      { class: "add-panel" },
      intro(
        `Pick a folder on this ${machine}. Atelier creates the repository in your studio and pushes everything, so there is nothing to set up in the browser first.`,
      ),
      h("div", { class: "add-folder" }, where, choose),
      note,
      h("div", { class: "add-fields" }, field("add-publish-account", "Account", account), field("add-publish-name", "Name", name)),
      privateRow,
      mirrorRow,
    );
    account.addEventListener("change", sync);
    sync();
    return { panel, foot, primary };
  }

  // ----------------------------------------------------------------- clone

  function cloneTab(): TabView {
    // Answers can come back out of order; only the newest question counts.
    let asked = 0;
    const results = h("div", { class: "add-hits", "aria-live": "polite" });

    const search = async (q: string) => {
      const mine = ++asked;
      try {
        const view = await api.searchRepos(q);
        if (!disposed && mine === asked) mount(results, drawHits(view, q));
      } catch (e) {
        if (!disposed && mine === asked) mount(results, h("span", { class: "add-problem", text: asError(e).message || "The search didn't work." }));
      }
    };

    const find = h("input", {
      id: "add-find",
      class: "input",
      type: "search",
      autocomplete: "off",
      spellcheck: "false",
      oninput: () => {
        clearTimeout(searchTimer);
        searchTimer = setTimeout(() => void search(find.value.trim()), 250);
      },
    });

    function drawHits(view: SearchView, q: string): Child[] {
      return [
        view.hits.length > 0
          ? view.hits.map(hitRow)
          : h("div", {
              class: "empty",
              text: q ? "Nothing by that name in your studio or on GitHub." : "There is nothing in your studio to clone yet.",
            }),
        view.github_note ? h("span", { class: "muted small", text: view.github_note }) : null,
      ];
    }

    function hitRow(hit: Hit): HTMLElement {
      const button = h("button", { type: "button", class: "btn", text: hit.label, onclick: () => void take(hit, button) });
      return h(
        "div",
        { class: ["add-hit", hit.source === "github" && "tone-github"] },
        h(
          "span",
          { class: "col grow" },
          h("span", { class: "add-hit-title clip", text: hit.title }),
          h("span", { class: "add-hit-line", text: hit.line }),
        ),
        button,
      );
    }

    function take(hit: Hit, button: HTMLButtonElement): Promise<void | undefined> {
      if (hit.action === "open") {
        if (hit.local_path) ctx.go({ screen: "repo", path: hit.local_path, tab: "changes" });
        return Promise.resolve();
      }
      return ctx.busy(button, async () => {
        let added: Added;
        if (hit.action === "mirror") {
          // Mirroring copies the whole repository into the studio first.
          button.textContent = "Mirroring…";
          try {
            added = await api.mirrorAndClone(hit.full_name);
          } finally {
            button.textContent = hit.label;
          }
        } else {
          added = await api.cloneRepo(hit.full_name);
        }
        await ctx.refresh();
        if (added.already) ctx.say("It was already here.", added.note ?? added.full_name);
        else ctx.say(`Cloned ${added.full_name}.`, added.note ?? "");
        ctx.go({ screen: "repo", path: added.path, tab: "changes" });
      });
    }

    const panel = h(
      "div",
      { class: "add-panel tight" },
      h("label", { for: "add-find", class: "label", text: "Find a repository in your studio or on GitHub" }),
      find,
      results,
      h(
        "span",
        { class: "muted small" },
        "Clones go to ",
        h("span", { class: "mono", text: `${app.clone_root}/owner/name` }),
        ". A GitHub repository is mirrored into your studio first.",
      ),
    );
    mount(results, h("span", { class: "muted", text: "Looking in your studio…" }));
    void search("");

    return {
      panel,
      foot: h("span", { class: "add-foot-note", text: `Uses this ${machine}’s SSH key.` }),
      primary: h("button", { type: "button", class: "btn lg primary", text: "Done", onclick: () => ctx.go({ screen: "today" }) }),
    };
  }

  // ------------------------------------------------------------- start new

  function newTab(): TabView {
    let readme = true;
    let openEditor = true;
    const account = accountSelect("add-new-account");
    const name = h("input", {
      id: "add-new-name",
      class: "input",
      type: "text",
      placeholder: "my-new-thing",
      autocomplete: "off",
      spellcheck: "false",
      oninput: () => sync(),
    });
    const primary = h("button", {
      type: "button",
      class: "btn lg primary",
      text: "Create repository",
      onclick: () =>
        void ctx.busy(primary, async () => {
          const added = await api.createNew(account.value, name.value.trim(), true, readme, openEditor);
          await ctx.refresh();
          if (added.note) ctx.say(added.note);
          ctx.go({ screen: "repo", path: added.path });
        }),
    });
    const sync = () => {
      primary.disabled = !account.value || !nameOk(name.value);
    };
    sync();

    const panel = h(
      "form",
      {
        class: "add-panel",
        onsubmit: (e) => {
          e.preventDefault();
          primary.click();
        },
      },
      intro(`Start from nothing. Atelier makes the repository in your studio and an empty working copy on this ${machine}.`),
      h("div", { class: "add-fields" }, field("add-new-account", "Account", account), field("add-new-name", "Name", name)),
      checkbox({ checked: true, label: "Add a README and a .gitignore", onchange: (next) => (readme = next) }),
      checkbox({ checked: true, label: `Open it in ${app.editor.label} when it's ready`, onchange: (next) => (openEditor = next) }),
    );
    return { panel, foot: h("span", { class: "add-foot-note", text: "Private by default." }), primary };
  }

  draw();

  return {
    el,
    dispose() {
      disposed = true;
      clearTimeout(searchTimer);
    },
  };
}
