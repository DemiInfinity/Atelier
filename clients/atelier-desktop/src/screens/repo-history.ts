// The History tab: one row per commit with a two-lane graph (amber: shared
// with the studio; grey: only here), and the selected commit on the right.

import { api, asError, type CommitView, type HistoryRow, type HistoryView, type Segment } from "../api";
import { agoShort, h, mount, pill, plural, s, when } from "../dom";
import type { RepoHost, TabView } from "./repo";

const LANE_X = [14, 36] as const;

export interface Graph {
  /** Lane 0's line through this row, or null for none. */
  l0: string | null;
  /** Lane 1's line through this row, or null for none. */
  l1: string | null;
  /** Where the commit's dot sits. */
  cx: number;
  fill: string;
}

function segment(lane: 0 | 1, seg: Segment): string | null {
  const x = LANE_X[lane];
  switch (seg) {
    case "full":
      return `M${x} 0 V40`;
    case "start":
      return `M${x} 20 V40`;
    case "join":
      // Only the local lane ever bends: it leaves lane 0 at this row's dot.
      return lane === 1 ? "M36 0 V8 Q36 20 14 20" : null;
    case "none":
      return null;
  }
}

/** What to draw in a row's 52×40 graph cell. */
export function graph(row: Pick<HistoryRow, "lane" | "line0" | "line1" | "incoming">): Graph {
  return {
    l0: segment(0, row.line0),
    l1: segment(1, row.line1),
    cx: LANE_X[row.lane],
    fill: row.incoming ? "var(--blue2)" : row.lane === 0 ? "var(--amber)" : "#c9c3ba",
  };
}

function graphCell(row: HistoryRow): SVGElement {
  const g = graph(row);
  const parts: Node[] = [];
  if (g.l0) parts.push(s("path", { d: g.l0, stroke: "var(--amber)", "stroke-width": 2 }));
  if (g.l1) parts.push(s("path", { d: g.l1, stroke: "#8f8a82", "stroke-width": 2, fill: "none" }));
  parts.push(s("circle", { cx: g.cx, cy: 20, r: 5, fill: g.fill, stroke: "var(--ground)", "stroke-width": 2 }));
  return s("svg", { width: 52, height: 40, viewBox: "0 0 52 40", fill: "none", "aria-hidden": "true", class: "hist-graph" }, ...parts);
}

export function historyTab(host: RepoHost): TabView {
  const { ctx, path } = host;
  let history: HistoryView | null = null;
  let selected: string | null = null;
  let seq = 0;
  let detailSeq = 0;
  let disposed = false;
  const rows = new Map<string, HTMLButtonElement>();

  const list = h("div", { class: "hist-list" }, h("p", { class: "repo-note", text: "Reading the history…" }));
  const aside = h("aside", { class: "hist-aside", "aria-label": "Selected commit" });
  const el = h("div", { class: "history" }, list, aside);

  // Up and down walk the list, the way a list of commits should.
  list.addEventListener("keydown", (event) => {
    const e = event as KeyboardEvent;
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    const shas = [...rows.keys()];
    const at = selected === null ? -1 : shas.indexOf(selected);
    const next = shas[at + (e.key === "ArrowDown" ? 1 : -1)];
    if (next === undefined) return;
    e.preventDefault();
    select(next);
    rows.get(next)?.focus();
  });

  function markSelected(): void {
    for (const [sha, row] of rows) {
      if (sha === selected) row.setAttribute("aria-current", "true");
      else row.removeAttribute("aria-current");
    }
  }

  function select(sha: string): void {
    if (selected === sha) return;
    selected = sha;
    markSelected();
    void loadDetail();
  }

  function commitRow(row: HistoryRow): HTMLButtonElement {
    return h(
      "button",
      { type: "button", class: "hist-row", onclick: () => select(row.sha) },
      graphCell(row),
      h(
        "span",
        { class: "hist-main" },
        row.pills.map((p) => pill(p.text, p.tone, "mono")),
        h("span", { class: "hist-subject", title: row.subject, text: row.subject }),
      ),
      h("span", { class: "hist-who", text: row.author }),
      h("span", { class: "hist-when", text: agoShort(row.date) }),
      h("span", { class: "hist-sha", text: row.short }),
    );
  }

  function draw(): void {
    rows.clear();
    if (!history) return;
    if (history.rows.length === 0) {
      mount(list, h("p", { class: "repo-note", text: "No commits yet." }));
      mount(aside);
      aside.classList.add("hidden");
      return;
    }
    aside.classList.remove("hidden");
    for (const row of history.rows) rows.set(row.sha, commitRow(row));
    const gitTool = host.view().git_tool;
    mount(
      list,
      [...rows.values()],
      history.more
        ? h("p", {
            class: "hist-more",
            text: gitTool
              ? `Older commits aren’t shown here. Hand over to ${gitTool.label} for the full history.`
              : "Older commits aren’t shown here. A git tool has the full history.",
          })
        : null,
    );
    markSelected();
  }

  // ---------------------------------------------------------------- detail

  function statusCard(row: HistoryRow): HTMLElement {
    let head = "In your studio";
    let sub = "";
    if (row.incoming) {
      head = `Not on this ${ctx.app().machine} yet`;
      sub = "Pull to bring it in.";
    } else if (row.unpushed) {
      head = "Not pushed yet";
      sub = "Checks run on your runners after you push.";
    }
    return h(
      "div",
      { class: "hist-status" },
      h("span", { class: "head", text: head }),
      sub ? h("span", { class: "muted", text: sub }) : null,
    );
  }

  function drawDetail(row: HistoryRow, commit: CommitView): void {
    const undo = h("button", {
      type: "button",
      class: "btn",
      text: "Undo this commit…",
      disabled: !commit.can_undo,
      title: commit.can_undo
        ? "Takes the commit back and keeps its changes in your files"
        : "Only the last commit can be undone, and only while it isn’t pushed",
      onclick: () => void undoCommit(undo, commit),
    });
    mount(
      aside,
      h(
        "div",
        { class: "hist-words" },
        h("span", { class: "hist-title selectable", text: commit.subject }),
        commit.body.trim() ? h("span", { class: "hist-body selectable", text: commit.body.trim() }) : null,
        h("span", { class: "hist-meta selectable", text: `${commit.short} · ${commit.author} · ${when(commit.date)}` }),
      ),
      statusCard(row),
      h(
        "div",
        { class: "hist-files" },
        h("span", { class: "eyebrow", text: `${plural(commit.files.length, "file", "files")} changed` }),
        commit.files.map((f) =>
          h(
            "div",
            { class: "hist-file" },
            h("span", { class: "grow clip selectable", title: f.path, text: f.path }),
            f.additions === null ? null : h("span", { class: "plus", text: `+${f.additions}` }),
            f.deletions === null ? null : h("span", { class: "minus", text: `−${f.deletions}` }),
          ),
        ),
      ),
      h(
        "div",
        { class: "hist-buttons" },
        h("button", {
          type: "button",
          class: "btn",
          text: "New branch from here",
          onclick: () => void host.newBranch(commit.sha),
        }),
        undo,
        h("button", {
          type: "button",
          class: "btn",
          text: "Copy ID",
          onclick: async () => {
            try {
              await navigator.clipboard.writeText(commit.sha);
              ctx.say("Copied.");
            } catch (e) {
              ctx.fail(e);
            }
          },
        }),
      ),
    );
  }

  async function loadDetail(): Promise<void> {
    const mine = ++detailSeq;
    const row = history?.rows.find((r) => r.sha === selected);
    if (!row) {
      mount(aside);
      return;
    }
    mount(aside, h("p", { class: "muted", text: "Reading the commit…" }));
    try {
      const commit = await api.repoCommitDetail(path, row.sha);
      if (disposed || mine !== detailSeq) return;
      drawDetail(row, commit);
    } catch (e) {
      if (disposed || mine !== detailSeq) return;
      mount(aside, h("p", { class: "muted", text: asError(e).message || "The commit couldn’t be read." }));
    }
  }

  async function undoCommit(button: HTMLButtonElement, commit: CommitView): Promise<void> {
    const ok = await ctx.confirm({
      title: "Undo this commit?",
      body: `“${commit.subject}” is taken back and its changes stay in your files, staged. Nothing you wrote is lost.`,
      confirm: "Undo commit",
    });
    if (!ok) return;
    await ctx.busy(button, async () => {
      const subject = await api.repoUndoCommit(path);
      ctx.say(`Undid “${subject}”. Its changes are back in Changes.`);
      try {
        await host.reloadBar();
      } catch (e) {
        ctx.fail(e);
      }
      host.showTab("changes");
      void ctx.refresh();
    });
  }

  async function reload(): Promise<void> {
    const mine = ++seq;
    try {
      const next = await api.repoHistory(path);
      if (disposed || mine !== seq) return;
      history = next;
      if (selected === null || !next.rows.some((r) => r.sha === selected)) selected = next.rows[0]?.sha ?? null;
      draw();
      void loadDetail();
    } catch (e) {
      if (disposed || mine !== seq) return;
      mount(list, h("p", { class: "repo-note", text: asError(e).message || "The history couldn’t be read." }));
    }
  }

  return {
    el,
    reload,
    dispose() {
      disposed = true;
    },
  };
}
