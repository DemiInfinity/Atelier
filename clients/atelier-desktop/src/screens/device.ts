// This device: what the runner on this machine is doing, which agents wait
// for a yes, what ran earlier, and when it may work.

import { api, type DeviceView, type Events, type LiveJob, type RuleRow, type RunRow, type WaitingRow } from "../api";
import type { Ctx, Screen } from "../ctx";
import { clock, elapsed, h, mount, pill, toggle } from "../dom";
import { approvalBody, atBottom, workflowName } from "../logic/device";
import type { Route } from "../routes";
import "../styles/device.css";

/** A job's transcript, kept across redraws so its scroll position survives them. */
interface Transcript {
  pre: HTMLPreElement;
  text: string;
  /** Follow new lines (the person hasn't scrolled up). */
  follow: boolean;
  top: number;
  reading: boolean;
}

export function deviceScreen(ctx: Ctx, route: Extract<Route, { screen: "device" }>): Screen {
  const machine = ctx.app().machine;

  let view: DeviceView | null = null;
  let error: string | null = null;
  let disposed = false;
  let seq = 0;
  let debounce: ReturnType<typeof setTimeout> | undefined;
  // The row named by the route is scrolled to once, not on every redraw.
  let shown = false;

  const transcripts = new Map<number, Transcript>();
  /** The "48 seconds" spans on screen, retold every second. */
  let clocks: { el: HTMLElement; job: LiveJob }[] = [];

  const head = h("header", { class: "screen-head", "data-tauri-drag-region": true });
  const body = h("div", { class: "screen-body device-body" });

  // -------------------------------------------------------------- loading

  async function load(): Promise<void> {
    const mine = ++seq;
    try {
      const next = await api.deviceView();
      if (disposed || mine !== seq) return;
      view = next;
      error = null;
    } catch (e) {
      if (disposed || mine !== seq) return;
      if (view === null) error = "Couldn’t read this device’s runner.";
      ctx.fail(e);
    }
    draw();
  }

  const reload = async () => {
    await load();
    await ctx.refresh();
  };

  // ---------------------------------------------------------- transcripts

  function transcript(job: LiveJob): Transcript {
    let t = transcripts.get(job.job_id);
    if (!t) {
      const pre = h("pre", { class: "device-tail", tabindex: "0", "aria-label": `What ${job.title} is doing` });
      const made: Transcript = { pre, text: "", follow: true, top: 0, reading: false };
      pre.addEventListener("scroll", () => {
        made.top = pre.scrollTop;
        made.follow = atBottom(pre.scrollTop, pre.clientHeight, pre.scrollHeight);
      });
      transcripts.set(job.job_id, made);
      t = made;
    }
    return t;
  }

  function setTail(t: Transcript, lines: string[]): void {
    const text = lines.join("\n");
    if (text === t.text) return;
    // Decided before the text changes: afterwards the end has moved.
    const follow = t.follow;
    t.text = text;
    t.pre.textContent = text;
    if (follow) t.pre.scrollTop = t.pre.scrollHeight;
  }

  /** After a redraw the elements were taken out and put back, which resets their scroll. */
  function restoreScroll(): void {
    for (const t of transcripts.values()) {
      if (!t.pre.isConnected) continue;
      const follow = t.follow;
      t.pre.scrollTop = follow ? t.pre.scrollHeight : t.top;
      t.follow = follow;
    }
  }

  function readTails(): void {
    for (const job of view?.jobs ?? []) {
      const t = transcripts.get(job.job_id);
      if (!t || t.reading) continue;
      t.reading = true;
      api
        .runnerTail(job.job_id)
        .then((lines) => {
          if (!disposed) setTail(t, lines);
        })
        // The job may just have ended; the next reload tidies up.
        .catch(() => undefined)
        .finally(() => {
          t.reading = false;
        });
    }
  }

  const subLine = (job: LiveJob) => `${job.engine ? `Agent on ${job.engine}` : "Job"} · ${elapsed(job.started_at)}`;

  // -------------------------------------------------------------- pieces

  function jobSection(job: LiveJob): HTMLElement {
    const t = transcript(job);
    setTail(t, job.tail);
    const sub = h("span", { class: "soft", text: subLine(job) });
    clocks.push({ el: sub, job });
    const stop: HTMLButtonElement = h("button", {
      type: "button",
      class: "btn",
      text: "Stop",
      onclick: () =>
        void ctx.busy(stop, async () => {
          const sure = await ctx.confirm({
            title: `Stop ${job.title}?`,
            body: "This ends it now. It can’t be picked up where it left off.",
            confirm: "Stop",
            danger: true,
          });
          if (!sure) return;
          await api.jobStop(job.job_id);
          await reload();
        }),
    });
    return h(
      "section",
      { class: "device-job", "aria-label": job.title },
      h(
        "div",
        { class: "row wrap device-job-top" },
        h(
          "span",
          { class: "col device-job-words" },
          h("span", { class: "device-job-title", text: job.detail ? `${job.title} · ${job.detail}` : job.title }),
          sub,
        ),
        stop,
      ),
      t.pre,
      job.engine
        ? h("div", { class: "device-job-foot", text: "Agents only get the Atelier tools their workflow lists." })
        : null,
    );
  }


  function waitingRow(row: WaitingRow): HTMLElement {
    const name = workflowName(row.title);
    const read: HTMLButtonElement | null = row.web_path
      ? h("button", {
          type: "button",
          class: "btn",
          text: "Read the workflow",
          onclick: () => void ctx.busy(read, () => api.openWeb(row.web_path ?? "")),
        })
      : null;
    const approve: HTMLButtonElement = h("button", {
      type: "button",
      class: "btn violet device-approve",
      text: "Approve and run",
      onclick: () =>
        void ctx.busy(approve, async () => {
          const sure = await ctx.confirm({
            title: `Let ${name} run?`,
            body: approvalBody(row.detail, row.limits),
            confirm: "Approve and run",
          });
          if (!sure) return;
          await api.agentApprove(row.job_id);
          await reload();
        }),
    });
    const hot = route.approve === row.job_id;
    return h(
      "div",
      { class: ["row wrap device-waiting", hot && "is-asked"] },
      h(
        "span",
        { class: "col device-waiting-words" },
        h("span", { class: "device-waiting-title", text: row.title }),
        h("span", { class: "muted", text: row.detail }),
        row.limits ? h("span", { class: "muted small", text: row.limits }) : null,
      ),
      read,
      approve,
    );
  }

  function runRow(run: RunRow): HTMLElement {
    const cells = [
      pill(run.status.text, run.status.tone, "device-run-status"),
      h(
        "span",
        { class: "grow clip" },
        h("span", { class: "device-run-name", text: run.name }),
        run.wher ? h("span", { class: "muted", text: ` · ${run.wher}` }) : null,
      ),
      h("span", { class: "muted", text: run.duration }),
      h("span", { class: "muted device-run-at", text: clock(run.started_at) }),
    ];
    const path = run.web_path;
    if (!path) return h("div", { class: "device-run" }, cells);
    const row: HTMLButtonElement = h(
      "button",
      {
        type: "button",
        class: "device-run is-link",
        title: "Open this run in your studio",
        onclick: () => void ctx.busy(row, () => api.openWeb(path)),
      },
      cells,
    );
    return row;
  }

  function ruleRow(rule: RuleRow, running: boolean): HTMLElement {
    const sw: HTMLButtonElement = toggle({
      on: rule.on,
      label: rule.title,
      size: "sm",
      disabled: !running,
      onchange: (next) =>
        void ctx.busy(sw, async () => {
          await api.runnerSetRule(rule.id, next);
          await load();
        }),
    });
    return h(
      "div",
      { class: "row" },
      h("span", { class: "col grow" }, h("span", { text: rule.title }), h("span", { class: "muted small", text: rule.detail })),
      sw,
    );
  }

  const title = (text: string) => h("h2", { class: "h-section", text });

  // ---------------------------------------------------------------- draw

  function drawHead(): void {
    const v = view;
    const label = `Take jobs on this ${machine}`;
    let note: string | null = null;
    if (v && !v.online && v.running) note = "Can’t reach your studio";
    else if (v?.paused === "battery") note = "Paused while on battery";

    const sw: HTMLButtonElement = toggle({
      on: v?.taking_jobs ?? false,
      label,
      disabled: !v?.running,
      onchange: (next) =>
        void ctx.busy(sw, async () => {
          await api.runnerSetTaking(next);
          await reload();
        }),
    });

    mount(
      head,
      h("h1", { class: "h-screen", text: v?.name ?? ctx.app().device_name }),
      h(
        "span",
        { class: "row wrap device-caps" },
        (v?.capabilities ?? []).map((cap) => pill(cap, "agents", "device-cap")),
      ),
      h(
        "div",
        { class: "row push-right" },
        note ? h("span", { class: "muted", text: note }) : null,
        h("span", { class: "soft", text: label }),
        sw,
      ),
    );
  }

  function draw(): void {
    drawHead();
    clocks = [];
    const v = view;
    if (!v) {
      mount(body, h("span", { class: "muted", text: error ?? "Reading this device’s runner…" }));
      return;
    }

    // Transcripts of jobs that ended are let go.
    const live = new Set(v.jobs.map((job) => job.job_id));
    for (const id of [...transcripts.keys()]) if (!live.has(id)) transcripts.delete(id);

    const how: HTMLButtonElement = h("button", {
      type: "button",
      class: "btn",
      text: "How to set one up ↗",
      onclick: () => void ctx.busy(how, () => api.openWeb(v.how_web_path)),
    });
    const absent = v.running
      ? null
      : h(
          "section",
          { class: "card device-absent" },
          h("span", {
            class: "device-absent-title",
            text: v.installed
              ? `The runner is installed on this ${machine} but isn’t running.`
              : `There is no runner on this ${machine} yet.`,
          }),
          h("span", {
            class: "soft",
            text: `A runner lets this ${machine} take jobs and agents from your studio, and lets your studio open repositories here.`,
          }),
          h("div", { class: "row" }, how),
          h("span", { class: "muted small", text: "This version of the app doesn’t install or change the runner." }),
        );

    const workspaces = v.workspaces;
    const editors: HTMLButtonElement = h("button", {
      type: "button",
      class: "btn link device-link",
      text: "Change editors",
      onclick: () => ctx.go({ screen: "settings" }),
    });

    mount(
      body,
      v.studio_error ? h("div", { class: "banner selectable", role: "status", text: v.studio_error }) : null,
      absent,
      h(
        "div",
        { class: "columns" },
        h(
          "div",
          { class: "wide" },
          v.jobs.length
            ? v.jobs.map(jobSection)
            : v.running
              ? h("span", { class: "muted", text: "Nothing is running here right now." })
              : null,
          h(
            "section",
            { class: "section" },
            title("Waiting for you"),
            v.waiting.length ? v.waiting.map(waitingRow) : h("span", { class: "muted", text: "No agents are waiting." }),
          ),
          h(
            "section",
            { class: "section" },
            title(`Earlier on this ${machine}`),
            v.runs.length
              ? h("div", { class: "table" }, v.runs.map(runRow))
              : h("span", { class: "muted", text: `Nothing has run on this ${machine} lately.` }),
          ),
        ),
        h(
          "div",
          { class: "narrow device-side" },
          h("section", { class: "card" }, title("When to work"), v.rules.map((rule) => ruleRow(rule, v.running))),
          h(
            "section",
            { class: "card device-card-tight" },
            title("Claude Code"),
            v.claude_code
              ? [
                  h("span", {
                    class: "soft",
                    text: "Signed in with your Claude account. Agents that ask for Claude Code run here.",
                  }),
                  h("span", {
                    class: "muted small",
                    text: "Their own shell and file tools stay off. They only get the Atelier tools a workflow lists.",
                  }),
                ]
              : h("span", { class: "soft", text: "Claude Code isn’t set up on this runner." }),
          ),
          workspaces
            ? h(
                "section",
                { class: "card device-card-tight" },
                title("Workspaces"),
                h("span", { class: "mono small soft selectable", text: workspaces.root }),
                h("span", {
                  class: "muted small",
                  text: `“Open on ${v.name}” in your studio clones here, then opens one of:`,
                }),
                h(
                  "span",
                  { class: "row wrap device-chips" },
                  workspaces.presets.map((preset) => h("span", { class: "device-chip", text: preset })),
                ),
                h("div", { class: "row" }, editors),
              )
            : null,
        ),
      ),
    );

    restoreScroll();
    const asked = shown ? null : body.querySelector(".device-waiting.is-asked");
    if (asked) {
      shown = true;
      asked.scrollIntoView({ block: "center" });
    }
  }

  draw();
  void load();

  const second = setInterval(() => {
    for (const { el, job } of clocks) el.textContent = subLine(job);
  }, 1000);
  const tails = setInterval(readTails, 2000);

  return {
    el: h("div", { class: "screen" }, head, body),
    dispose() {
      disposed = true;
      clearInterval(second);
      clearInterval(tails);
      clearTimeout(debounce);
    },
    onEvent<K extends keyof Events>(name: K, _payload: Events[K]) {
      if (name !== "runner" && name !== "status") return;
      clearTimeout(debounce);
      debounce = setTimeout(() => {
        if (!disposed) void load();
      }, 300);
    },
  };
}
