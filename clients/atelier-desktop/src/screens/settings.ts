// Settings: six cards. The studio this app is connected to, remote access
// (the one red switch), commit identities, editors, notifications and the
// app's own preferences.

import "../styles/settings.css";
import { api, asError, type Account, type AppState, type Events, type NotifyPrefs, type RemoteSession, type RemoteView } from "../api";
import type { Ctx, Screen } from "../ctx";
import { ago, checkbox, h, mount, plural, toggle, type Child } from "../dom";
import type { Route } from "../routes";
import { sentenceList } from "./welcome";

// ------------------------------------------------------------------ logic

/** The words after the address in "Who's connected". */
export function sessionWords(session: RemoteSession, now: Date = new Date()): string {
  return [
    session.country || "unknown place",
    session.signed_in ? "signed in" : "not signed in",
    session.class,
    ago(session.since, now),
  ]
    .filter(Boolean)
    .join(" · ");
}

/** `Works once, for 10 minutes.` */
export function linkLife(expiresIn: number): string {
  return `Works once, for ${plural(Math.max(1, Math.round(expiresIn / 60)), "minute", "minutes")}.`;
}

/** The letter in an identity's round badge. */
export function initial(account: Pick<Account, "commit_name" | "name">): string {
  const first = [...(account.commit_name.trim() || account.name.trim())][0];
  return first ? first.toUpperCase() : "?";
}

const NOTIFY: { key: keyof NotifyPrefs; label: string }[] = [
  { key: "fail", label: "A job fails" },
  { key: "agent", label: "An agent is waiting for my approval" },
  { key: "security", label: "A security alert or update" },
  { key: "comment", label: "Someone comments on my pull request" },
  { key: "pass", label: "A job passes" },
];

// ----------------------------------------------------------------- screen

export function settingsScreen(ctx: Ctx, _route: Extract<Route, { screen: "settings" }>): Screen {
  let disposed = false;

  // Remote access has its own state: it is read from the studio, and it
  // changes under us when another window (or the studio) flips the switch.
  let remote: RemoteView | null = null;
  let remoteError: string | null = null;
  let remoteBusy = false;
  let link: { url: string; expires_in: number } | null = null;
  // A newer read wins over an older one that answers late.
  let remoteAsk = 0;

  const remoteCard = h("section", { class: "card settings-card settings-remote", "aria-labelledby": "settings-remote-title" });
  const grid = h("div", { class: "settings-grid" });

  const el = h(
    "div",
    { class: "screen" },
    h("header", { class: "screen-head", "data-tauri-drag-region": true }, h("h1", { class: "h-screen", text: "Settings" })),
    h("div", { class: "screen-body" }, grid),
  );

  const title = (text: string, id?: string) => h("h2", { class: "settings-title", id, text });
  const small = (...children: Child[]) => h("span", { class: "muted small" }, children);
  const card = (...children: Child[]) => h("section", { class: "card settings-card" }, children);
  const mono = (text: string) => h("span", { class: "mono", text });

  /** A checkbox that saves as it is ticked, and goes back if saving fails. */
  function pref(label: string, checked: boolean, save: (on: boolean) => Promise<void>, disabled = false): { row: HTMLElement; input: HTMLInputElement } {
    const row = checkbox({
      checked,
      label,
      disabled,
      onchange: (next) => {
        void save(next).catch((e) => {
          ctx.fail(e);
          input.checked = !next;
        });
      },
    });
    const input = row.querySelector("input") as HTMLInputElement;
    return { row, input };
  }

  function draw(): void {
    const app = ctx.app();
    mount(grid, studioCard(app), remoteCard, identityCard(app), toolsCard(app), notifyCard(app), appCard(app));
  }

  // ----------------------------------------------------------- your studio

  function studioCard(app: AppState): HTMLElement {
    const open = h("button", { type: "button", class: "btn", text: "Open studio", onclick: () => void ctx.busy(open, () => api.openStudio()) });
    const leave = h("button", {
      type: "button",
      class: "btn danger",
      text: `Disconnect this ${app.machine}…`,
      onclick: () =>
        void ctx.busy(leave, async () => {
          const sure = await ctx.confirm({
            title: `Disconnect this ${app.machine}?`,
            body: `This removes the app's token and its configuration from this ${app.machine}. The device stays listed in your studio until you revoke it under Settings → Devices.`,
            confirm: "Disconnect",
            danger: true,
          });
          if (!sure) return;
          await api.disconnect();
          await ctx.signedOut();
        }),
    });
    const status = app.offline
      ? [h("span", { class: "dot sm warn", "aria-hidden": "true" }), h("span", { text: app.note ?? "Can't reach your studio right now." })]
      : [
          h("span", { class: "dot sm ok", "aria-hidden": "true" }),
          h("span", {
            text: `Connected as ${app.person_name ?? app.person_login ?? "you"} · this ${app.machine} was approved with your passkey`,
          }),
        ];
    return card(
      title("Your studio"),
      h("span", { class: "settings-host mono selectable", text: app.studio_host ?? "" }),
      h("span", { class: "settings-status soft" }, status),
      small("The app holds its own device token, which you can revoke from the studio. It never stores your password."),
      h("div", { class: "settings-buttons" }, open, leave),
    );
  }

  // --------------------------------------------------------- remote access

  async function loadRemote(): Promise<void> {
    const mine = ++remoteAsk;
    try {
      const view = await api.remoteView();
      if (disposed || mine !== remoteAsk) return;
      remote = view;
      remoteError = null;
    } catch (e) {
      if (disposed || mine !== remoteAsk) return;
      remoteError = asError(e).message || "Couldn't read remote access.";
    }
    if (remote && !remote.on) link = null;
    drawRemote();
  }

  async function setRemote(on: boolean): Promise<void> {
    if (remoteBusy) return;
    remoteBusy = true;
    try {
      if (on) {
        const sure = await ctx.confirm({
          title: "Turn on remote access?",
          body: "Your studio becomes reachable from outside your home, at a temporary address. Anyone who has the address can reach the sign-in page until you turn this off.",
          confirm: "Turn on",
          danger: true,
        });
        if (!sure) return;
      }
      // Anything read before this is stale now.
      remoteAsk += 1;
      remote = await api.remoteSet(on);
      remoteError = null;
      if (!remote.on) link = null;
    } catch (e) {
      ctx.fail(e);
    } finally {
      remoteBusy = false;
      if (!disposed) drawRemote();
    }
  }

  function drawRemote(): void {
    const view = remote;
    const on = view?.on ?? false;
    remoteCard.classList.toggle("on", on);
    const head = h(
      "div",
      { class: "row" },
      h("h2", { class: "settings-title grow", id: "settings-remote-title", text: "Remote access" }),
      view ? h("span", { class: ["settings-remote-label", on && "on"], text: on ? "On" : "Off" }) : null,
      toggle({
        on,
        label: "Remote access",
        kind: "red",
        disabled: !view || !view.available,
        onchange: (next) => void setRemote(next),
      }),
    );
    const print = small("Turning it off cuts every outside connection at once. Away from home you sign in with a passkey, never a password.");

    if (!view) {
      mount(remoteCard, head, h("span", { class: remoteError ? "settings-problem" : "muted", text: remoteError ?? "Checking…" }), print);
      return;
    }
    mount(
      remoteCard,
      head,
      h("span", { class: view.available ? "soft" : "muted", text: view.line }),
      view.error ? h("span", { class: "settings-problem", text: view.error }) : null,
      remoteError ? h("span", { class: "settings-problem", text: remoteError }) : null,
      print,
      on ? remoteDetails(view) : null,
    );
  }

  function remoteDetails(view: RemoteView): Child[] {
    const make = h("button", {
      type: "button",
      class: "btn",
      text: "Make a one-time passkey link",
      onclick: () =>
        void ctx.busy(make, async () => {
          link = await api.remoteLink();
          if (!disposed) drawRemote();
        }),
    });
    return [
      view.address ? h("span", { class: "settings-address mono selectable", text: view.address }) : null,
      h("span", { class: "eyebrow", text: "Who's connected" }),
      view.sessions.length > 0
        ? h(
            "ul",
            { class: "settings-sessions" },
            view.sessions.map((session) =>
              h("li", null, h("span", { class: "mono selectable", text: session.ip }), ` · ${sessionWords(session)}`),
            ),
          )
        : h("span", { class: "muted", text: "Nobody is connected right now." }),
      h("div", { class: "settings-buttons" }, make),
      link ? linkBox(link) : null,
    ];
  }

  function linkBox(made: { url: string; expires_in: number }): HTMLElement {
    const copy = h("button", {
      type: "button",
      class: "btn sm",
      text: "Copy",
      onclick: async () => {
        try {
          await navigator.clipboard.writeText(made.url);
          ctx.say("Copied.");
        } catch {
          // No clipboard here: the link stays on screen to select by hand.
        }
      },
    });
    return h(
      "div",
      { class: "settings-link" },
      h("div", { class: "row" }, h("span", { class: "settings-link-url mono selectable grow", text: made.url }), copy),
      small(linkLife(made.expires_in)),
    );
  }

  // ----------------------------------------------------- who you commit as

  function identityCard(app: AppState): HTMLElement {
    return card(
      title("Who you commit as"),
      app.accounts.length > 0
        ? app.accounts.map((account) =>
            h(
              "div",
              { class: "settings-identity" },
              h("span", { class: ["settings-avatar", account.kind !== "user" && "company"], "aria-hidden": "true", text: initial(account) }),
              h(
                "span",
                { class: "col grow" },
                h("span", { class: "settings-identity-name clip", text: account.commit_name }),
                h("span", { class: "settings-identity-mail mono clip selectable", text: account.commit_email }),
              ),
              h("span", { class: "muted small", text: account.name }),
            ),
          )
        : h("span", { class: "muted", text: "Your studio hasn't told the app about any account yet." }),
      small(
        "The right name and email are used for each account, so company commits never carry your personal address. The identity is set in each repository's own git config when it is cloned or published, never globally.",
      ),
    );
  }

  // ----------------------------------------------------- editors and tools

  function toolsCard(app: AppState): HTMLElement {
    /** A select that saves on change and falls back to what is saved if that fails. */
    const picker = (id: string, label: string, options: { id: string; label: string }[], current: string, save: (id: string) => Promise<void>) => {
      const select: HTMLSelectElement = h(
        "select",
        {
          id,
          class: "select sm",
          onchange: () => {
            const picked = select.value;
            void (async () => {
              try {
                await save(picked);
                await ctx.reloadApp();
              } catch (e) {
                ctx.fail(e);
                select.value = current;
              }
            })();
          },
        },
        options.map((option) => h("option", { value: option.id, text: option.label, selected: option.id === current })),
      );
      return h("div", { class: "field" }, h("label", { for: id, text: label }), select);
    };
    return card(
      title("Editors and tools"),
      picker("settings-editor", "Open repositories in", app.editors, app.editor.id, api.setEditor),
      picker("settings-git-tool", "For heavier git work, hand over to", app.git_tools, app.git_tool?.id ?? "none", api.setGitTool),
      small(
        "Clones go to ",
        mono(app.clone_root),
        ".",
        app.watch.length > 0 ? [" Atelier also watches ", sentenceList(app.watch.map(mono)), " for folders you already have."] : null,
      ),
    );
  }

  // --------------------------------------------------------- notifications

  function notifyCard(app: AppState): HTMLElement {
    return card(
      title(`Notifications on this ${app.machine}`),
      NOTIFY.map(({ key, label }) => pref(label, app.prefs.notify[key], (on) => api.setNotifyPref(key, on)).row),
      h(
        "div",
        { class: "settings-messages" },
        h(
          "span",
          { class: "col grow" },
          h("span", { text: "Friendly messages" }),
          small("The little notes Atelier leaves when something happens."),
        ),
        h("button", { type: "button", class: "btn", text: "Edit messages", onclick: () => ctx.go({ screen: "messages" }) }),
      ),
    );
  }

  // --------------------------------------------------------------- the app

  function appCard(app: AppState): HTMLElement {
    const save = (key: "autostart" | "menu_bar" | "hide_dock") => async (on: boolean) => {
      await api.setAppPref(key, on);
      const next = await ctx.reloadApp();
      // Hiding the Dock icon only makes sense with the menu bar icon there.
      if (dock) {
        dock.input.disabled = !next.prefs.menu_bar;
        dock.input.checked = next.prefs.hide_dock;
      }
    };
    const dock =
      app.os === "macos" ? pref("Keep the Dock icon hidden", app.prefs.hide_dock, save("hide_dock"), !app.prefs.menu_bar) : null;
    const keys = h("button", {
      type: "button",
      class: "btn link settings-inline",
      text: "See your keys ↗",
      onclick: () => void ctx.busy(keys, () => api.openWeb("/settings/keys")),
    });
    return card(
      title("The app"),
      pref("Start when I log in", app.prefs.autostart, save("autostart")).row,
      pref("Show in the menu bar", app.prefs.menu_bar, save("menu_bar")).row,
      dock?.row,
      small(`This ${app.machine}'s SSH key was added to your studio when you connected. `, keys),
      h("span", { class: "settings-version", text: `Atelier Desktop ${app.version}` }),
    );
  }

  drawRemote();
  draw();
  void loadRemote();

  return {
    el,
    dispose() {
      disposed = true;
    },
    onEvent<K extends keyof Events>(name: K, payload: Events[K]) {
      if (name !== "remote") return;
      // A link made while it was on is no use once it is off.
      if (!(payload as Events["remote"]).on) link = null;
      void loadRemote();
    },
  };
}
