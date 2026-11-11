// Add a repository: a card with four ways in. Publish a folder that is
// already on this machine, clone one from the studio (or from GitHub, by
// mirroring it into the studio first), start a new one from nothing, or
// open a folder that is already a working copy.

import "../styles/add.css";
import { api, asError, type Account, type Added, type FolderPlan, type Hit, type OpenedFolder, type SearchView } from "../api";
import type { Ctx, Screen } from "../ctx";
import { checkbox, h, mount, type Child } from "../dom";
import type { AddTab, Route } from "../routes";

// ------------------------------------------------------------------ logic

/** How an account reads in the account menu. */
export function accountLabel(account: Account): string {
  return account.kind === "user" ? `${account.name} (personal)` : `${account.display} (company)`;
}

/** `personal account` or `company`, under an owner's name. */
export function ownerKind(account: Pick<Account, "kind">): string {
  return account.kind === "user" ? "personal account" : "company";
}

/** An owner's name as shown: the display name, or the account name for the personal one. */
export function ownerName(account: Pick<Account, "name" | "display" | "kind">): string {
  return account.kind === "user" || !account.display ? account.name : account.display;
}

/**
 * Who a new repository belongs to unless the person says otherwise: the
 * owner chosen last time while the studio still reports it, else the first
 * (the personal account).
 */
export function defaultOwner(accounts: readonly Pick<Account, "name">[], remembered: string | null): string {
  const kept = remembered ? accounts.find((a) => a.name.toLowerCase() === remembered.toLowerCase()) : undefined;
  return (kept ?? accounts[0])?.name ?? "";
}

export interface OwnerChooser {
  el: HTMLElement;
  /** The chosen account's name: what the studio takes as `owner`. */
  value(): string;
  set(name: string): void;
  disable(off: boolean): void;
}

/** "This belongs to": every owner the studio reports, one of them chosen. */
export function ownerChooser(id: string, accounts: readonly Account[], initial: string, onchange: () => void = () => {}): OwnerChooser {
  let chosen = initial;
  const inputs = accounts.map((account) =>
    h("input", {
      type: "radio",
      name: id,
      value: account.name,
      checked: account.name === chosen,
      onchange: () => {
        chosen = account.name;
        onchange();
      },
    }),
  );
  const el = h(
    "fieldset",
    { class: "owner-choice", id },
    h("legend", { text: "This belongs to" }),
    accounts.length === 0
      ? h("span", { class: "muted", text: "Your studio didn't say who you can create repositories for." })
      : h(
          "div",
          { class: "owner-options" },
          accounts.map((account, i) =>
            h(
              "label",
              { class: "owner-option" },
              inputs[i],
              h(
                "span",
                { class: "col" },
                h("span", { class: "owner-name clip", text: ownerName(account) }),
                h("span", { class: "owner-kind", text: ownerKind(account) }),
              ),
            ),
          ),
        ),
  );
  return {
    el,
    value: () => chosen,
    set(name) {
      const input = inputs.find((i) => i.value === name);
      if (!input) return;
      chosen = name;
      input.checked = true;
    },
    disable(off) {
      for (const input of inputs) input.disabled = off;
    },
  };
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

/** The ways in, in the order of the tabs. */
export const TABS: readonly { id: AddTab; label: string }[] = [
  { id: "publish", label: "Publish a folder" },
  { id: "clone", label: "Clone" },
  { id: "new", label: "Start new" },
  { id: "open", label: "Open a folder" },
];

export interface OpenedWords {
  /** Why the picked folder wasn't simply opened. */
  line: string;
  /** The remote to show after the line, if there is one. */
  remote: string | null;
  /** The remote is on GitHub (the blue thread). */
  github: boolean;
  /** It is a git folder, so it can be opened as it is. */
  canOpen: boolean;
}

/** What to say about a picked folder that isn't in the studio. Null when it was opened. */
export function openedWords(opened: Pick<OpenedFolder, "kind" | "remote">): OpenedWords | null {
  switch (opened.kind) {
    case "opened":
      return null;
    case "elsewhere": {
      const remote = opened.remote ?? null;
      return {
        line: remote ? "This folder's remote isn't your studio:" : "This folder's remote isn't your studio.",
        remote,
        github: remote !== null && remote.includes("github.com"),
        canOpen: true,
      };
    }
    case "unpublished":
      return { line: "This git folder has no remote yet.", remote: null, github: false, canOpen: true };
    case "not_repo":
      return { line: "This isn't a git folder yet.", remote: null, github: false, canOpen: false };
  }
}

// ----------------------------------------------------------------- screen

interface TabView {
  panel: HTMLElement;
  foot: HTMLElement;
  primary: HTMLButtonElement;
}

export function addScreen(ctx: Ctx, route: Extract<Route, { screen: "add" }>): Screen {
  const app = ctx.app();
  const machine = app.machine;
  let disposed = false;
  let current: AddTab = route.tab ?? "publish";
  let searchTimer: ReturnType<typeof setTimeout> | undefined;

  // Each tab is built the first time it is opened and kept, so what was
  // typed survives a look at another tab.
  const built = new Map<AddTab, TabView>();
  // Set by the Publish tab once it is built: shows a folder there as if it
  // had been chosen with "Choose…".
  let loadPlan: ((plan: FolderPlan, owner?: string) => void) | null = null;
  // Who a new repository belongs to: the owner chosen last time, shared by
  // the tabs so a choice made in one is the choice in the others.
  let owner = defaultOwner(app.accounts, app.prefs.default_owner);
  const choosers: OwnerChooser[] = [];
  function chooser(id: string, onchange: () => void): OwnerChooser {
    const made: OwnerChooser = ownerChooser(id, app.accounts, owner, () => {
      owner = made.value();
      for (const other of choosers) if (other !== made) other.set(owner);
      onchange();
    });
    choosers.push(made);
    return made;
  }

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
      view = current === "publish" ? publishTab() : current === "clone" ? cloneTab() : current === "new" ? newTab() : openTab();
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

  /** Goes to the Publish tab with `plan` as the chosen folder. */
  function publishThis(plan: FolderPlan): void {
    current = "publish";
    draw();
    loadPlan?.(plan);
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
    const account = chooser("add-publish-owner", () => sync());
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
          take(picked);
        }),
    });

    function take(picked: FolderPlan): void {
      plan = picked;
      name.value = picked.name;
      sync();
    }
    loadPlan = take;

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
      for (const control of [name, ...[privateRow, mirrorRow].map((row) => row?.querySelector("input"))]) {
        if (control) control.disabled = idle;
      }
      account.disable(idle);
      primary.textContent = step.label;
      primary.disabled = step.kind === "none" || (step.kind === "publish" && (!account.value() || !nameOk(name.value)));
    }

    async function publish(folder: FolderPlan): Promise<Added | null> {
      const send = (replace: boolean) => api.publish(folder.path, account.value(), name.value.trim(), isPrivate, replace);
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
          ctx.go({ screen: "repo", path: folder.path });
          return;
        }
        if (step.kind === "init") {
          await api.folderInit(folder.path);
          await ctx.refresh();
          ctx.say("It's a git folder now.", "Commit something first, then publish it.");
          ctx.go({ screen: "repo", path: folder.path });
          return;
        }
        const added = await publish(folder);
        if (!added) return;
        // The owner just used is the one offered first next time.
        await ctx.reloadApp().catch(() => {});
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
        ctx.go({ screen: "repo", path: added.path });
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
      account.el,
      h("div", { class: "add-fields" }, field("add-publish-name", "Name", name)),
      privateRow,
      mirrorRow,
    );
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
        if (hit.local_path) ctx.go({ screen: "repo", path: hit.local_path });
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
        ctx.go({ screen: "repo", path: added.path });
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
    const account = chooser("add-new-owner", () => sync());
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
          const added = await api.createNew(account.value(), name.value.trim(), true, readme, openEditor);
          await ctx.reloadApp().catch(() => {});
          await ctx.refresh();
          if (added.note) ctx.say(added.note);
          ctx.go({ screen: "repo", path: added.path });
        }),
    });
    const sync = () => {
      primary.disabled = !account.value() || !nameOk(name.value);
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
      account.el,
      h("div", { class: "add-fields" }, field("add-new-name", "Name", name)),
      checkbox({ checked: true, label: "Add a README and a .gitignore", onchange: (next) => (readme = next) }),
      checkbox({ checked: true, label: `Open it in ${app.editor.label} when it's ready`, onchange: (next) => (openEditor = next) }),
    );
    return { panel, foot: h("span", { class: "add-foot-note", text: "Private by default." }), primary };
  }

  // --------------------------------------------------------- open a folder

  function openTab(): TabView {
    const result = h("div", { class: "add-opened hidden", "aria-live": "polite" });

    const toRepo = (path: string) => ctx.go({ screen: "repo", path });
    // Asked here already, so the choice is made before the Publish tab opens.
    const belongs = chooser("add-open-owner", () => {});

    function showResult(opened: OpenedFolder, words: OpenedWords): void {
      const plan = opened.plan;
      const publish = plan
        ? h("button", { type: "button", class: "btn hot", text: "Publish to Atelier…", onclick: () => publishThis(plan) })
        : null;
      const anyway: HTMLButtonElement | null = words.canOpen
        ? h("button", {
            type: "button",
            class: "btn",
            text: "Open it anyway",
            onclick: () =>
              void ctx.busy(anyway, async () => {
                // Picking it already put it among the folders here.
                await ctx.refresh();
                toRepo(opened.path);
              }),
          })
        : null;
      mount(
        result,
        h("span", { class: "add-path mono selectable", text: plan?.display ?? opened.path }),
        h(
          "span",
          { class: "add-opened-line" },
          words.line,
          words.remote ? [" ", h("span", { class: ["mono selectable", words.github && "add-via"], text: words.remote })] : null,
        ),
        plan ? h("span", { class: "muted", text: plan.summary }) : null,
        plan ? belongs.el : null,
        h("div", { class: "add-opened-actions" }, publish, anyway),
      );
      result.classList.remove("hidden");
    }

    const pick = h("button", {
      type: "button",
      class: "btn",
      text: `Open a folder on this ${machine}…`,
      onclick: () =>
        void ctx.busy(pick, async () => {
          const opened = await api.openFolder();
          if (!opened || disposed) return;
          const words = openedWords(opened);
          if (words) {
            showResult(opened, words);
            return;
          }
          await ctx.reloadApp();
          await ctx.refresh();
          ctx.say(opened.full_name ? `Added ${opened.full_name}.` : "Added.");
          toRepo(opened.path);
        }),
    });

    const panel = h(
      "div",
      { class: "add-panel" },
      intro(`Already have it on this ${machine}? Pick the folder and Atelier adds it to your working copies.`),
      h("div", null, pick),
      result,
    );
    return {
      panel,
      foot: h("span", { class: "add-foot-note", text: "Your folder stays where it is." }),
      primary: h("button", { type: "button", class: "btn lg primary", text: "Done", onclick: () => ctx.go({ screen: "today" }) }),
    };
  }

  draw();

  // A folder the app already knows (from the sidebar, say): show it in the
  // Publish tab without asking for it again.
  if (route.path !== undefined && current === "publish") {
    api.folderPlan(route.path).then(
      (plan) => {
        if (!disposed) loadPlan?.(plan);
      },
      (e) => {
        if (!disposed) ctx.fail(e);
      },
    );
  }

  return {
    el,
    dispose() {
      disposed = true;
      clearTimeout(searchTimer);
    },
  };
}
