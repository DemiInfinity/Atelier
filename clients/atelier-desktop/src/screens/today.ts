// Today: what needs you, the working copies on this machine, what the
// runner is doing here, and what happened in the studio.

import { api, type FeedRow, type NeedRow, type Overview, type RunningRow, type WorkingCopy } from "../api";
import type { Ctx, Screen } from "../ctx";
import { ago, dayLine, h, mount, pill, plural } from "../dom";
import type { Route } from "../routes";
import "../styles/today.css";

export function todayScreen(ctx: Ctx, _route: Extract<Route, { screen: "today" }>): Screen {
  const machine = ctx.app().machine;
  const line = h("span", { class: "muted" });
  const body = h("div", { class: "screen-body today-body" });

  const head = h(
    "header",
    { class: "screen-head today-head", "data-tauri-drag-region": true },
    h("h1", { class: "h-screen", text: "Today" }),
    line,
    h(
      "div",
      { class: "row push-right today-actions" },
      h("button", { type: "button", class: "btn", text: "Clone…", onclick: () => ctx.go({ screen: "add", tab: "clone" }) }),
      h("button", {
        type: "button",
        class: "btn primary",
        text: "Publish a folder",
        onclick: () => ctx.go({ screen: "add", tab: "publish" }),
      }),
    ),
  );

  const section = (title: string, side: string | null, ...children: (Node | null)[]) =>
    h(
      "section",
      { class: "section" },
      h("div", { class: "row" }, h("h2", { class: "h-section grow", text: title }), side ? h("span", { class: "muted", text: side }) : null),
      children,
    );

  const quiet = (text: string) => h("div", { class: "empty", text });

  function need(row: NeedRow): HTMLElement {
    const button: HTMLButtonElement = h("button", {
      type: "button",
      class: "btn thread",
      text: row.label,
      onclick: () => void ctx.busy(button, () => ctx.runNeed(row.action)),
    });
    return h(
      "div",
      { class: `today-need tone-${row.tone}` },
      h("span", { class: "dot", "aria-hidden": "true" }),
      h(
        "span",
        { class: "col grow" },
        h("span", { class: "today-need-title", text: row.title }),
        h("span", { class: "muted", text: row.detail }),
      ),
      button,
    );
  }

  function copyRow(copy: WorkingCopy): HTMLElement {
    const open = () => ctx.go({ screen: "repo", path: copy.path, tab: "changes" });
    const label = copy.action === "pull" ? "Pull" : copy.action === "push" ? "Push" : "Open";
    const button: HTMLButtonElement = h("button", {
      type: "button",
      class: "btn sm",
      text: label,
      onclick: () => {
        if (copy.action === "open") {
          open();
        } else if (copy.action === "push") {
          void ctx.busy(button, () => ctx.runNeed({ type: "push", path: copy.path }));
        } else {
          void ctx.busy(button, async () => {
            const n = await api.repoPull(copy.path);
            ctx.say(n > 0 ? `Pulled ${plural(n, "commit", "commits")}.` : "Already up to date.", copy.name);
            await ctx.refresh();
          });
        }
      },
    });
    return h(
      "div",
      { class: "today-copy" },
      h(
        "button",
        { type: "button", class: "today-copy-name", onclick: open },
        h("span", { class: "today-copy-title clip", text: copy.name }),
        h("span", { class: "muted clip", text: copy.owner }),
      ),
      h("span", { class: "today-copy-branch mono clip", title: copy.branch, text: copy.branch }),
      h("span", { class: `today-copy-state is-${copy.tone}`, text: copy.state }),
      button,
    );
  }

  function runningCard(job: RunningRow): HTMLElement {
    return h(
      "button",
      { type: "button", class: "today-running", onclick: () => ctx.go({ screen: "device" }) },
      h(
        "span",
        { class: "row today-running-top" },
        h("span", { class: "today-running-title grow", text: job.title }),
        job.engine ? pill(job.engine, "agents") : null,
      ),
      job.detail ? h("span", { class: "soft", text: job.detail }) : null,
      job.progress !== null
        ? h(
            "span",
            { class: "progress", "aria-hidden": "true" },
            h("span", { style: { width: `${Math.round(Math.min(1, Math.max(0, job.progress)) * 100)}%` } }),
          )
        : null,
    );
  }

  function feedCard(row: FeedRow): HTMLElement {
    const inner = [
      h(
        "span",
        { class: "row today-feed-top" },
        h("span", { class: "today-feed-tag", text: row.tag }),
        h("span", { class: "muted small push-right", text: ago(row.at) }),
      ),
      h("span", { class: "today-feed-text", text: row.text }),
    ];
    const target = row.target;
    return target
      ? h("button", { type: "button", class: `today-feed tone-${row.tone}`, onclick: () => ctx.follow(target) }, inner)
      : h("div", { class: `today-feed tone-${row.tone}` }, inner);
  }

  function draw(view: Overview | null): void {
    if (!view) {
      line.textContent = dayLine();
      mount(body, h("div", { class: "muted", text: "Reading your studio…" }));
      return;
    }
    line.textContent = `${dayLine()} · ${view.studio_error ? "can’t reach your studio right now" : "in step with your studio"}`;

    const nothingRunning = view.runner.running
      ? "Nothing is running here right now."
      : `The runner isn’t running on this ${machine}.`;

    mount(
      body,
      view.studio_error ? h("div", { class: "banner selectable", role: "status", text: view.studio_error }) : null,
      h(
        "div",
        { class: "columns" },
        h(
          "div",
          { class: "wide" },
          section("Needs you", null, ...(view.needs.length ? view.needs.map(need) : [quiet("Nothing needs you right now.")])),
          section(
            "Working copies",
            view.roots_label,
            view.copies.length
              ? h("div", { class: "table" }, view.copies.map(copyRow))
              : quiet("No working copies on this " + machine + " yet. Clone a repository, or publish a folder you already have."),
          ),
        ),
        h(
          "div",
          { class: "narrow" },
          section(
            `Running on this ${machine}`,
            null,
            ...(view.running.length ? view.running.map(runningCard) : [h("span", { class: "muted", text: nothingRunning })]),
            ...view.elsewhere.map((text) => h("span", { class: "muted", text: `Also running: ${text}` })),
          ),
          section("From your studio", null, ...(view.feed.length ? view.feed.map(feedCard) : [h("span", { class: "muted", text: "Nothing new from your studio." })])),
        ),
      ),
    );
  }

  draw(ctx.overview());

  return {
    el: h("div", { class: "screen" }, head, body),
    onOverview: (overview) => draw(overview),
  };
}
