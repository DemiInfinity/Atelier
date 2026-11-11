import "./styles/fonts";
import "./styles/base.css";
import "./styles/tray.css";

// The menu bar popover: a small window of its own with what needs you, what
// is running here, a way back into the working copies, and the two switches.
// It has no dialogs and no toasts; a failure shows as one red line.

import { api, asError, on, type AppState, type NeedRow, type Overview, type RemoteView, type RunningRow, type WorkingCopy } from "./api";
import { h, mark, mount, toggle } from "./dom";
import { jumpBackIn, popoverStep, shortElapsed } from "./logic/tray";

const root = document.getElementById("popover")!;

let app: AppState | null = null;
let overview: Overview | null = null;
let remote: RemoteView | null = null;
/** What went wrong last, as one sentence. Cleared by the next good reload. */
let failure: string | null = null;
/** The "Turn it on?" question is showing in place of the remote access row. */
let asking = false;
let seq = 0;
let debounce: ReturnType<typeof setTimeout> | undefined;
/** The running card's clock, retold every second. */
let ticking: { el: HTMLElement; job: RunningRow } | null = null;

const sentence = (e: unknown) => {
  const err = asError(e);
  return [err.message || "Something went wrong.", err.hint].filter(Boolean).join(" ");
};

// ------------------------------------------------------------------ loading

/** Reads everything again. Resolves false when something couldn't be read. */
async function reload(): Promise<boolean> {
  const mine = ++seq;
  let problem: string | null = null;
  let state: AppState;
  try {
    state = await api.appState();
  } catch (e) {
    if (mine !== seq) return false;
    failure = sentence(e);
    draw();
    return false;
  }
  let view: Overview | null = null;
  let access: RemoteView | null = null;
  if (state.signed_in) {
    const [a, b] = await Promise.allSettled([api.overview(), api.remoteView()]);
    if (a.status === "fulfilled") view = a.value;
    else problem = sentence(a.reason);
    if (b.status === "fulfilled") access = b.value;
    else problem ??= sentence(b.reason);
  }
  if (mine !== seq) return false;
  app = state;
  // What was read before stays on screen when a re-read fails.
  overview = state.signed_in ? (view ?? overview) : null;
  remote = state.signed_in ? (access ?? remote) : null;
  if (!remote?.available || remote.on) asking = false;
  failure = problem;
  draw();
  return problem === null;
}

function reloadSoon(): void {
  clearTimeout(debounce);
  debounce = setTimeout(() => void reload(), 200);
}

/**
 * Runs what a button does, with the button disabled meanwhile. Reads
 * everything again afterwards; a failure is shown after that read, so the
 * read doesn't wipe it.
 */
async function act(button: HTMLButtonElement | null, work: () => Promise<unknown>, thenReload = true): Promise<void> {
  if (button?.disabled) return;
  if (button) {
    button.disabled = true;
    button.classList.add("busy");
  }
  let problem: string | null = null;
  try {
    await work();
  } catch (e) {
    problem = sentence(e);
  }
  if (thenReload) await reload();
  else if (button) {
    button.disabled = false;
    button.classList.remove("busy");
  }
  if (problem !== null) {
    failure = problem;
    draw();
  }
}

/** A button that brings the main window up, or does one command. */
function button(props: { class: string; text?: string; label?: string; title?: string }, work: () => Promise<unknown>, thenReload = false, ...children: Node[]): HTMLButtonElement {
  const el: HTMLButtonElement = h(
    "button",
    {
      type: "button",
      class: props.class,
      text: children.length ? undefined : props.text,
      "aria-label": props.label,
      title: props.title,
      onclick: () => void act(el, work, thenReload),
    },
    children,
  );
  return el;
}

// ------------------------------------------------------------------- pieces

function runningCard(job: RunningRow, machine: string): HTMLElement {
  const time = h("span", { class: "tray-running-time", text: shortElapsed(job.started_at) });
  ticking = { el: time, job };
  return button(
    { class: "tray-running" },
    () => api.showMain({ screen: "device" }),
    false,
    h("span", { class: "row tray-running-top" }, h("span", { class: "grow tray-strong", text: `Running on this ${machine}` }), time),
    h("span", { class: "soft tray-wrap", text: job.detail ? `${job.title} · ${job.detail}` : job.title }),
    ...(job.progress !== null
      ? [
          h(
            "span",
            { class: "progress", "aria-hidden": "true" },
            h("span", { style: { width: `${Math.round(Math.min(1, Math.max(0, job.progress)) * 100)}%` } }),
          ),
        ]
      : []),
  );
}

function needRow(need: NeedRow): HTMLElement {
  const step = popoverStep(need.action);
  return h(
    "div",
    { class: `tray-need tone-${need.tone}` },
    h("span", { class: "dot sm", "aria-hidden": "true" }),
    h(
      "span",
      { class: "col grow" },
      h("span", { class: "tray-need-title clip", title: need.title, text: need.title }),
      h("span", { class: "muted small clip", text: need.detail }),
    ),
    step.do === "push"
      ? button({ class: "btn sm thread", text: need.label }, () => api.repoPush(step.path), true)
      : button({ class: "btn sm thread", text: need.label }, () => api.showMain(step.target)),
  );
}

function copyRow(copy: WorkingCopy): HTMLElement {
  return button(
    { class: "tray-copy", title: `${copy.name} · ${copy.branch} · ${copy.state}` },
    () => api.showMain({ screen: "repo", path: copy.path }),
    false,
    h(
      "span",
      { class: "grow clip" },
      h("span", { class: "tray-need-title", text: copy.name }),
      " ",
      h("span", { class: "mono muted small", text: copy.branch }),
    ),
    h("span", { class: `tray-copy-state is-${copy.tone}`, text: copy.state }),
    h("span", { class: "tray-copy-open", text: "Open" }),
  );
}

function switchRow(title: string, detail: string, sw: HTMLElement): HTMLElement {
  return h(
    "div",
    { class: "row" },
    h("span", { class: "col grow" }, h("span", { class: "tray-need-title", text: title }), h("span", { class: "muted small", text: detail })),
    sw,
  );
}

function remoteRow(view: RemoteView): HTMLElement {
  if (asking && !view.on) {
    // No dialogs here: the question takes the row's place.
    const yes = button({ class: "btn sm danger", text: "Turn on" }, () => api.remoteSet(true), true);
    const no = h("button", {
      type: "button",
      class: "btn sm",
      text: "Cancel",
      onclick: () => {
        asking = false;
        draw();
        root.querySelector<HTMLElement>(".tray-remote")?.focus();
      },
    });
    const row = h(
      "div",
      { class: "row tray-ask", role: "group", "aria-label": "Turn remote access on?" },
      h("span", { class: "grow tray-need-title", text: "Reachable from outside. Turn it on?" }),
      yes,
      no,
    );
    queueMicrotask(() => no.focus());
    return row;
  }
  const sw: HTMLButtonElement = toggle({
    on: view.on,
    label: "Remote access",
    kind: "red",
    onchange: (next) => {
      if (next) {
        asking = true;
        draw();
      } else {
        // Off is always immediate.
        void act(sw, () => api.remoteSet(false));
      }
    },
  });
  sw.classList.add("tray-switch", "tray-remote");
  return switchRow("Remote access", view.switch_line, sw);
}

// --------------------------------------------------------------------- draw

function draw(): void {
  ticking = null;
  const state = app;
  const machine = state?.machine ?? "computer";
  const signedIn = state?.signed_in ?? false;
  const view = overview;
  const trouble = signedIn && (state?.offline || !view || view.studio_error !== null);

  const status = !state
    ? h("span", { class: "tray-status" }, failure ? "Atelier isn’t answering" : "Starting…")
    : !signedIn
      ? h("span", { class: "tray-status" }, h("span", { class: "dot xs off", "aria-hidden": "true" }), "Not connected yet")
      : h(
          "span",
          { class: "tray-status" },
          h("span", { class: ["dot xs", trouble ? "warn" : "ok"], "aria-hidden": "true" }),
          trouble ? "Can’t reach your studio" : "Connected to your studio",
        );

  const head = h(
    "header",
    { class: "tray-head" },
    mark(24),
    h("span", { class: "col grow" }, h("span", { class: "tray-name", text: "Atelier" }), status),
    button({ class: "btn tray-open", text: "Open app" }, () => api.showMain()),
  );

  let middle: HTMLElement;
  if (!state) {
    middle = h("div", { class: "tray-middle" });
  } else if (!signedIn) {
    middle = h(
      "div",
      { class: "tray-middle tray-welcome" },
      h("span", { class: "soft", text: "Connect your studio to get started." }),
      button({ class: "btn lg primary", text: "Open Atelier" }, () => api.showMain()),
    );
  } else if (!view) {
    middle = h("div", { class: "tray-middle" }, h("span", { class: "muted tray-pad", text: failure ? "" : "Reading your studio…" }));
  } else {
    const job = view.running[0];
    const needs = view.needs.slice(0, 3);
    const more = view.needs.length - needs.length;
    const copies = jumpBackIn(view.copies);
    middle = h(
      "div",
      { class: "tray-middle" },
      job ? runningCard(job, machine) : null,
      h(
        "section",
        { class: "tray-needs", "aria-label": "Needs you" },
        h("span", { class: "eyebrow tray-eyebrow", text: "Needs you" }),
        needs.length ? needs.map(needRow) : h("span", { class: "muted tray-pad", text: "Nothing needs you." }),
        more > 0
          ? button({ class: "tray-more", text: `and ${more} more` }, () => api.showMain({ screen: "today" }))
          : null,
      ),
      copies.length
        ? h(
            "section",
            { class: "tray-copies", "aria-label": "Jump back in" },
            h("span", { class: "eyebrow tray-eyebrow", text: "Jump back in" }),
            copies.map(copyRow),
          )
        : null,
      h(
        "div",
        { class: "tray-pair" },
        button({ class: "btn tray-big", text: "Publish a folder" }, () => api.showMain({ screen: "add", tab: "publish" })),
        button({ class: "btn tray-big", text: "Clone…" }, () => api.showMain({ screen: "add", tab: "clone" })),
      ),
    );
  }

  let runnerSwitch: HTMLElement | null = null;
  if (signedIn && view) {
    const label = `Take jobs on this ${machine}`;
    const sw: HTMLButtonElement = toggle({
      on: view.runner.taking_jobs,
      label,
      disabled: !view.runner.running,
      onchange: (next) => void act(sw, () => api.runnerSetTaking(next)),
    });
    sw.classList.add("tray-switch");
    runnerSwitch = switchRow(label, view.runner.switch_line, sw);
  }

  const foot = h(
    "footer",
    { class: "tray-foot" },
    runnerSwitch,
    signedIn && remote?.available ? remoteRow(remote) : null,
    h(
      "div",
      { class: "tray-links" },
      signedIn ? button({ class: "tray-link", text: "Settings" }, () => api.showMain({ screen: "settings" })) : null,
      signedIn
        ? button({ class: "tray-link", text: "Open studio in browser ↗" }, async () => {
            await api.openStudio();
            await api.hidePopover();
          })
        : null,
      button({ class: "tray-link tray-quit", text: "Quit" }, () => api.quit()),
    ),
  );

  mount(
    root,
    head,
    middle,
    failure ? h("div", { class: "tray-failure selectable", role: "alert", text: failure }) : null,
    foot,
  );
}

// ------------------------------------------------------------------- wiring

setInterval(() => {
  if (ticking?.el.isConnected) ticking.el.textContent = shortElapsed(ticking.job.started_at);
}, 1000);

document.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") return;
  // A question left open would be a surprise next time.
  if (asking) {
    asking = false;
    draw();
  }
  void api.hidePopover().catch(() => undefined);
});

void on("popover-shown", () => {
  asking = false;
  reloadSoon();
});
for (const name of ["status", "runner", "remote", "notify"] as const) void on(name, reloadSoon);

draw();
void reload();
