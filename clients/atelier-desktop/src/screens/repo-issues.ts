// The Issues tab of a repository: the open (or closed) issues on the left,
// the picked one on the right with its text, conversation, a comment box
// and Close / Reopen.
//
// Titles, bodies and comments are written by people and bots, on GitHub
// too: they are shown as plain text (the Markdown source, line breaks
// kept), never as markup.

import "../styles/pulls.css";
import "../styles/issues.css";

import { api, asError, type Events, type IssueRow, type IssueView, type LabelView } from "../api";
import { ago, h, mount, pill, plural } from "../dom";
import { markdown, writePreview, type MarkdownHandlers } from "../markdown";
import type { RepoHost, TabView } from "./repo";

/** A label as a small chip; its colour (checked on the Rust side) only tints a dot. */
export function labelChip(label: LabelView): HTMLElement {
  return h(
    "span",
    { class: "issue-label" },
    h("span", { class: "dot xs", "aria-hidden": "true", style: label.color ? { background: label.color } : undefined }),
    h("span", { text: label.name }),
  );
}

/** `2 comments · opened 3 days ago` */
export function issueLine(row: Pick<IssueRow, "comments" | "created_at" | "by">, now: Date = new Date()): string {
  const parts = [`by ${row.by}`];
  if (row.comments > 0) parts.push(plural(row.comments, "comment", "comments"));
  if (row.created_at) parts.push(`opened ${ago(row.created_at, now)}`);
  return parts.join(" · ");
}

/** One issue in the list. */
export function issueCard(row: IssueRow, current: boolean, pick: () => void, now: Date = new Date()): HTMLElement {
  return h(
    "button",
    {
      type: "button",
      class: ["pulls-card issue-card", row.via?.tone === "github" && "tone-github from-github"],
      "aria-current": current ? "true" : "false",
      onclick: pick,
    },
    h("span", { class: "pulls-card-title" }, h("span", { class: "issue-number mono", text: `#${row.number}` }), " ", row.title),
    h("span", { class: "muted", text: issueLine(row, now) }),
    row.labels.length > 0 || row.via || !row.open
      ? h(
          "span",
          { class: "row wrap pulls-card-pills" },
          row.open ? null : pill("Closed", "quiet"),
          row.via ? pill(row.via.text, row.via.tone) : null,
          row.labels.map(labelChip),
        )
      : null,
  );
}

export function issuesTab(host: RepoHost, repo: string): TabView {
  const { ctx } = host;
  let state: "open" | "closed" = "open";
  let rows: IssueRow[] | null = null;
  let hasMore = false;
  let page = 1;
  let listError: string | null = null;
  let picked: number | null = null;
  let detail: IssueView | null = null;
  let detailError: string | null = null;
  let listSeq = 0;
  let detailSeq = 0;
  let disposed = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  // What was typed in the comment box survives a re-read of the issue.
  let draft = "";
  /** The comment box is showing its preview. */
  let previewing = false;

  const tabs = h("div", { class: "issues-states", role: "tablist", "aria-label": "Which issues" });
  const list = h("div", { class: "pulls-list" });
  const pane = h("div", { class: "pulls-detail" });
  const fresh = h("button", { type: "button", class: "btn sm hot", text: "New issue", onclick: () => void newIssue() });

  const el = h(
    "div",
    { class: "screen pulls scoped issues" },
    h("div", { class: "pulls-side" }, h("header", { class: "pulls-head" }, tabs, h("span", { class: "grow" }), fresh), list),
    pane,
  );

  // ------------------------------------------------------------------ list

  function drawStates(): void {
    const one = (id: "open" | "closed", label: string) =>
      h("button", {
        type: "button",
        role: "tab",
        class: "issues-state",
        "aria-selected": state === id ? "true" : "false",
        text: label,
        onclick: () => {
          if (state === id) return;
          state = id;
          rows = null;
          picked = null;
          detail = null;
          page = 1;
          drawStates();
          drawList();
          drawDetail();
          void loadList();
        },
      });
    mount(tabs, one("open", "Open"), one("closed", "Closed"));
  }

  function drawList(): void {
    if (rows === null) {
      mount(list, h("span", { class: "muted pulls-note", text: listError ?? "Reading the issues…" }));
      return;
    }
    const more: HTMLButtonElement | null = hasMore
      ? h("button", {
          type: "button",
          class: "btn sm issues-more",
          text: "Show more",
          onclick: () => void ctx.busy(more, () => loadMore()),
        })
      : null;
    mount(
      list,
      rows.length === 0
        ? h("span", { class: "muted pulls-note", text: state === "open" ? "No open issues. Nothing is asking for you here." : "No closed issues yet." })
        : rows.map((row) => issueCard(row, row.number === picked, () => select(row.number))),
      more,
    );
  }

  async function loadList(): Promise<void> {
    const seq = ++listSeq;
    const asked = state;
    try {
      // Re-read every page that is showing, so "Show more" isn't undone.
      const pages = page;
      const all: IssueRow[] = [];
      let more = false;
      for (let p = 1; p <= pages; p += 1) {
        const view = await api.issuesList(repo, asked, p);
        all.push(...view.rows);
        more = view.has_more;
        if (!more) break;
      }
      if (disposed || seq !== listSeq) return;
      rows = all;
      hasMore = more;
      listError = null;
    } catch (e) {
      if (disposed || seq !== listSeq) return;
      if (rows === null) listError = asError(e).message || "Couldn’t read the issues.";
      else ctx.fail(e);
    }
    if (picked === null && rows && rows.length > 0) {
      picked = (rows[0] as IssueRow).number;
      void loadDetail(true);
    }
    drawList();
    if (picked === null) drawDetail();
  }

  async function loadMore(): Promise<void> {
    const view = await api.issuesList(repo, state, page + 1);
    if (disposed) return;
    page += 1;
    const known = new Set((rows ?? []).map((r) => r.number));
    rows = [...(rows ?? []), ...view.rows.filter((r) => !known.has(r.number))];
    hasMore = view.has_more;
    drawList();
  }

  function select(number: number): void {
    if (number === picked && detail) return;
    picked = number;
    draft = "";
    previewing = false;
    drawList();
    void loadDetail(true);
  }

  // ---------------------------------------------------------------- detail

  async function loadDetail(clear: boolean): Promise<void> {
    const number = picked;
    const seq = ++detailSeq;
    if (number === null) {
      detail = null;
      drawDetail();
      return;
    }
    if (clear) {
      detail = null;
      detailError = null;
      drawDetail();
      pane.scrollTop = 0;
    }
    try {
      const view = await api.issueDetail(repo, number);
      if (disposed || seq !== detailSeq) return;
      detail = view;
      detailError = null;
    } catch (e) {
      if (disposed || seq !== detailSeq) return;
      if (detail === null) detailError = asError(e).message || "Couldn’t read this issue.";
      else ctx.fail(e);
    }
    drawDetail();
  }

  async function reloadAll(): Promise<void> {
    await Promise.all([loadList(), loadDetail(false)]);
  }

  async function newIssue(): Promise<void> {
    const made = await ctx.compose({
      title: `New issue in ${host.view().name}`,
      titleLabel: "Title",
      bodyLabel: "What is it about? (optional)",
      confirm: "Open issue",
    });
    if (!made) return;
    await ctx.busy(fresh, async () => {
      const number = await api.issueCreate(repo, made.title, made.body);
      ctx.say(number ? `Opened issue ${number}.` : "Opened the issue.");
      if (state !== "open") {
        state = "open";
        rows = null;
        page = 1;
        drawStates();
      }
      if (number) {
        picked = number;
        draft = "";
        void loadDetail(true);
      }
      await loadList();
    });
  }

  /** `#12` in someone's text: that issue here; a pull request opens in the browser. */
  async function openRef(number: number): Promise<void> {
    if (rows?.some((r) => r.number === number)) {
      select(number);
      return;
    }
    try {
      await api.issueDetail(repo, number);
      select(number);
    } catch {
      await ctx.busy(null, () => api.openWeb(`/${repo}/pulls/${number}`));
    }
  }
  const links: MarkdownHandlers = { openLink: (url) => ctx.openLink(url), openRef: (number) => void openRef(number) };

  function drawDetail(): void {
    const view = detail;
    if (!view) {
      const words = picked === null ? "" : (detailError ?? "Reading this issue…");
      mount(pane, words ? h("span", { class: "muted selectable", text: words }) : null);
      return;
    }
    const github = view.via?.tone === "github";

    const box = h("textarea", {
      class: "textarea issue-box",
      rows: "3",
      placeholder: "Add a comment…",
      "aria-label": "Add a comment",
      value: draft,
      oninput: () => {
        draft = box.value;
        send.disabled = box.value.trim() === "";
      },
    });
    const send: HTMLButtonElement = h("button", {
      type: "button",
      class: "btn primary",
      text: "Comment",
      disabled: draft.trim() === "",
      onclick: () =>
        void ctx.busy(send, async () => {
          await api.issueComment(repo, view.number, box.value);
          draft = "";
          previewing = false;
          await reloadAll();
        }),
    });
    const toggle: HTMLButtonElement = h("button", {
      type: "button",
      class: "btn",
      text: view.open ? "Close issue" : "Reopen issue",
      onclick: () =>
        void ctx.busy(toggle, async () => {
          await api.issueSetOpen(repo, view.number, !view.open);
          ctx.say(view.open ? `Closed issue ${view.number}.` : `Reopened issue ${view.number}.`);
          // It leaves the list it was in; keep it on the right.
          await reloadAll();
        }),
    });
    const browser: HTMLButtonElement = h("button", {
      type: "button",
      class: "btn link",
      text: "Open in browser ↗",
      onclick: () => void ctx.busy(browser, () => api.openWeb(view.web_path)),
    });

    mount(
      pane,
      h(
        "div",
        { class: "col pulls-title-block" },
        h("h2", { class: "pulls-title selectable", text: view.title }),
        h(
          "span",
          { class: "row wrap issue-meta soft" },
          pill(view.open ? "Open" : "Closed", view.open ? "pass" : "quiet"),
          view.via ? pill(view.via.text, view.via.tone) : null,
          h("span", { text: `issue ${view.number} · by ${view.by}${view.created_at ? ` · opened ${ago(view.created_at)}` : ""}` }),
          view.labels.map(labelChip),
        ),
      ),
      h(
        "section",
        { class: ["issue-body", github && "tone-github from-github"], "aria-label": "Description" },
        view.body.trim() ? markdown(view.body_md, links) : h("span", { class: "muted", text: "No description." }),
      ),
      h(
        "section",
        { class: "section" },
        h("h3", { class: "h-section", text: "Conversation" }),
        view.comments.length
          ? view.comments.map((c) =>
              h(
                "div",
                { class: `pulls-comment tone-${c.tone}` },
                h(
                  "span",
                  { class: "row wrap pulls-comment-top" },
                  h("span", { class: "pulls-comment-who", text: c.who }),
                  c.tag ? h("span", { class: "pulls-comment-tag", text: c.tag }) : null,
                  h("span", { class: "muted push-right", text: c.at ? ago(c.at) : "" }),
                ),
                markdown(c.body_md, links, "pulls-comment-body"),
              ),
            )
          : h("span", { class: "muted", text: "No comments yet." }),
      ),
      h("div", { class: "col issue-reply" }, writePreview(box, links, { previewing, onMode: (on) => (previewing = on) }), h("div", { class: "row wrap issue-actions" }, send, toggle, h("span", { class: "grow" }), browser)),
    );
  }

  drawStates();
  drawList();
  drawDetail();
  void loadList();

  return {
    el,
    reload: reloadAll,
    dispose() {
      disposed = true;
      clearTimeout(timer);
    },
    onEvent<K extends keyof Events>(name: K, _payload: Events[K]) {
      if (name !== "status") return;
      clearTimeout(timer);
      timer = setTimeout(() => {
        if (!disposed) void reloadAll();
      }, 400);
    },
  };
}
