// The Actions tab of a repository: its recent runs, gathered under the pull
// request they belong to. The jobs and logs stay a click away: a failed
// run opens its log here, anything else opens the run in the studio.

import "../styles/issues.css";

import { api, asError, type Events, type RepoRunRow, type RunGroup, type RunsView } from "../api";
import { ago, h, mount, pill, type Child } from "../dom";
import type { RepoHost, TabView } from "./repo";

/** One group of runs: a pull request's, or the rest. */
export function runGroup(
  group: RunGroup,
  actions: { open(run: RepoRunRow): void; pull(number: number): void },
  now: Date = new Date(),
): HTMLElement {
  const pull = group.pull;
  return h(
    "section",
    { class: ["runs-group", pull !== null && "for-pull"], "aria-label": group.label },
    h(
      "div",
      { class: "runs-group-head" },
      h("h3", { class: "h-section grow clip", title: group.label, text: group.label }),
      pull !== null ? h("button", { type: "button", class: "btn sm", text: "Open pull request", onclick: () => actions.pull(pull) }) : null,
    ),
    group.runs.map((run) =>
      h(
        "button",
        {
          type: "button",
          class: `runs-row tone-${run.status.tone}`,
          title: run.failed ? "Open the log" : "Open the run in your studio",
          onclick: () => actions.open(run),
        },
        pill(run.status.text, run.status.tone, "status"),
        h("span", { class: "runs-name", text: run.name }),
        h("span", { class: "muted clip grow", text: run.detail }),
        h("span", { class: "mono small muted", text: run.duration }),
        h("span", { class: "muted small runs-when", text: run.started_at ? ago(run.started_at, now) : "" }),
      ),
    ),
  );
}

export function runsTab(host: RepoHost, repo: string): TabView {
  const { ctx } = host;
  let disposed = false;
  let seq = 0;
  let view: RunsView | null = null;
  let timer: ReturnType<typeof setTimeout> | undefined;

  const body = h("div", { class: "runs-body" }, h("p", { class: "repo-note", text: "Reading the runs…" }));
  const studio: HTMLButtonElement = h("button", {
    type: "button",
    class: "btn link",
    text: "All runs in your studio ↗",
    onclick: () => void ctx.busy(studio, () => api.openWeb(view?.web_path ?? `/${repo}/actions`)),
  });
  const el = h("div", { class: "runs" }, h("div", { class: "runs-head" }, h("h2", { class: "h-section grow", text: "Recent runs" }), studio), body);

  const actions = {
    open(run: RepoRunRow): void {
      if (run.failed) ctx.showLog(repo, run.run_id, run.web_path);
      else if (run.web_path) void ctx.busy(null, () => api.openWeb(run.web_path as string));
    },
    pull(number: number): void {
      ctx.go({ screen: "pulls", repo, number });
    },
  };

  async function reload(): Promise<void> {
    const mine = ++seq;
    let content: Child;
    try {
      const next = await api.repoRuns(repo);
      if (disposed || mine !== seq) return;
      view = next;
      content =
        next.groups.length === 0
          ? h("p", { class: "repo-note", text: "Nothing has run in this repository yet." })
          : next.groups.map((group) => runGroup(group, actions));
    } catch (e) {
      if (disposed || mine !== seq) return;
      if (view) {
        ctx.fail(e);
        return;
      }
      content = h("p", { class: "repo-note", text: asError(e).message || "Couldn’t read the runs." });
    }
    mount(body, content);
  }

  void reload();

  return {
    el,
    reload,
    dispose() {
      disposed = true;
      clearTimeout(timer);
    },
    onEvent<K extends keyof Events>(name: K, _payload: Events[K]) {
      if (name !== "status") return;
      clearTimeout(timer);
      timer = setTimeout(() => {
        if (!disposed) void reload();
      }, 400);
    },
  };
}
