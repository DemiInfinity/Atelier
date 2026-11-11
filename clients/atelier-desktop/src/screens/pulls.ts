// Pull requests: the open ones across the studio on the left, the picked
// one on the right with its merge status, checks and conversation.

import { api, asError, type CheckRow, type CommentRow, type Events, type PullRow, type PullView } from "../api";
import type { Ctx, MenuItem, Screen } from "../ctx";
import { ago, h, icon, mount, pill, plural } from "../dom";
import { pickMethod, pickPull, shortRepo, visiblePulls, type PullKey } from "../logic/pulls";
import type { Route } from "../routes";
import "../styles/pulls.css";

const METHOD_KEY = "atelier.merge-method";

/** The merge method picked last, kept across visits (and restarts when storage is there). */
let rememberedMethod: string | null = null;
try {
  rememberedMethod = localStorage.getItem(METHOD_KEY);
} catch {
  // No storage: remember for this run only.
}

function rememberMethod(id: string): void {
  rememberedMethod = id;
  try {
    localStorage.setItem(METHOD_KEY, id);
  } catch {
    // Fine; see above.
  }
}

export function pullsScreen(ctx: Ctx, route: Extract<Route, { screen: "pulls" }>): Screen {
  const machine = ctx.app().machine;

  let rows: PullRow[] | null = null;
  let listError: string | null = null;
  let onlyRepo: string | null = route.repo !== undefined && route.number === undefined ? route.repo : null;
  let picked: PullKey | null =
    route.repo !== undefined && route.number !== undefined ? { repo: route.repo, number: route.number } : null;
  let detail: PullView | null = null;
  let detailError: string | null = null;
  // Answers to old questions are dropped: each load takes a number.
  let listSeq = 0;
  let detailSeq = 0;
  let disposed = false;
  let timer: ReturnType<typeof setTimeout> | undefined;

  const count = h("span", { class: "muted" });
  const list = h("div", { class: "pulls-list" });
  const pane = h("div", { class: "pulls-detail" });

  const el = h(
    "div",
    { class: "screen pulls" },
    h(
      "div",
      { class: "pulls-side" },
      h(
        "header",
        { class: "pulls-head", "data-tauri-drag-region": true },
        h("h1", { class: "h-screen grow", text: "Pull requests" }),
        count,
      ),
      list,
    ),
    pane,
  );

  const same = (a: PullKey | null, b: PullKey | null) => !!a && !!b && a.repo === b.repo && a.number === b.number;

  // ---------------------------------------------------------------- list

  function drawList(): void {
    if (rows === null) {
      count.textContent = "";
      mount(list, h("span", { class: "muted pulls-note", text: listError ?? "Reading your pull requests…" }));
      return;
    }
    count.textContent = `${rows.length} open across your studio`;
    const visible = visiblePulls(rows, onlyRepo);
    const only = onlyRepo;
    mount(
      list,
      only
        ? h(
            "div",
            { class: "pulls-filter muted small" },
            `Showing ${rows.find((r) => r.repo === only)?.repo_short ?? shortRepo(only)} only · `,
            h("button", {
              type: "button",
              class: "pulls-show-all",
              text: "Show all",
              onclick: () => {
                onlyRepo = null;
                drawList();
              },
            }),
          )
        : null,
      visible.length === 0
        ? h("span", { class: "muted pulls-note", text: "No open pull requests. Quiet is good." })
        : visible.map((row) => {
            const key = { repo: row.repo, number: row.number };
            return h(
              "button",
              {
                type: "button",
                class: "pulls-card",
                "aria-current": same(key, picked) ? "true" : "false",
                onclick: () => select(key),
              },
              h("span", { class: "pulls-card-title", text: row.title }),
              h("span", { class: "muted", text: `${row.repo_short} · ${row.by}` }),
              h(
                "span",
                { class: "row wrap pulls-card-pills" },
                pill(row.status.text, row.status.tone),
                row.via ? pill(row.via.text, row.via.tone) : null,
              ),
            );
          }),
    );
  }

  async function loadList(): Promise<void> {
    const seq = ++listSeq;
    try {
      const next = await api.pullsList();
      if (disposed || seq !== listSeq) return;
      rows = next;
      listError = null;
    } catch (e) {
      if (disposed || seq !== listSeq) return;
      // Keep what is on screen when a re-read fails; say so only when there is nothing.
      if (rows === null) listError = asError(e).message || "Couldn’t read your pull requests.";
      else ctx.fail(e);
    }
    const next = pickPull(visiblePulls(rows ?? [], onlyRepo), picked);
    const changed = !same(next, picked);
    picked = next;
    drawList();
    if (changed) void loadDetail(true);
    else if (!picked) drawDetail();
  }

  function select(key: PullKey): void {
    if (same(key, picked) && detail) return;
    picked = key;
    drawList();
    void loadDetail(true);
  }

  // -------------------------------------------------------------- detail

  /** `fresh`: another pull request, so clear the pane while it loads. */
  async function loadDetail(fresh: boolean): Promise<void> {
    const key = picked;
    const seq = ++detailSeq;
    if (!key) {
      detail = null;
      drawDetail();
      return;
    }
    if (fresh) {
      detail = null;
      detailError = null;
      drawDetail();
      pane.scrollTop = 0;
    }
    try {
      const view = await api.pullDetail(key.repo, key.number);
      if (disposed || seq !== detailSeq) return;
      detail = view;
      detailError = null;
    } catch (e) {
      if (disposed || seq !== detailSeq) return;
      if (detail === null) detailError = asError(e).message || "Couldn’t read this pull request.";
      else ctx.fail(e);
    }
    drawDetail();
  }

  /** After merging, approving and the like: everything may have moved. */
  async function reloadAll(): Promise<void> {
    await Promise.all([loadList(), loadDetail(false)]);
    await ctx.refresh();
  }

  function mergeCard(view: PullView): HTMLElement {
    const method = pickMethod(view.methods, rememberedMethod);
    const buttons: (HTMLElement | null)[] = [];

    if (view.state === "open" && method) {
      const merge: HTMLButtonElement = h("button", {
        type: "button",
        class: "btn lg primary",
        text: method.label,
        disabled: !view.can_merge,
        onclick: () =>
          void ctx.busy(merge, async () => {
            const sure = await ctx.confirm({
              title: "Merge this pull request?",
              body:
                `${method.label}: pull request ${view.number} into ${view.base}?` +
                (view.on_github ? " The merge happens on GitHub." : ""),
              confirm: method.label,
            });
            if (!sure) return;
            const result = await api.pullMerge(view.repo, view.number, method.id, false);
            if (result.toast) ctx.toast(result.toast);
            else ctx.say("Merged.");
            await reloadAll();
          }),
      });
      buttons.push(merge);

      if (view.methods.length > 1) {
        const more: HTMLButtonElement = h(
          "button",
          {
            type: "button",
            class: "btn lg pulls-method",
            "aria-label": "Choose how to merge",
            title: "Choose how to merge",
            "aria-haspopup": "menu",
            onclick: () => {
              const items: MenuItem[] = view.methods.map((m) => ({
                label: m.label,
                current: m.id === method.id,
                pick: () => {
                  rememberMethod(m.id);
                  drawDetail();
                },
              }));
              ctx.menu(more, items);
            },
          },
          icon("chevron", 14),
        );
        buttons.push(more);
      }

      if (!view.can_merge && view.auto_merge === null) {
        const later: HTMLButtonElement = h("button", {
          type: "button",
          class: "btn lg thread",
          text: "Merge when checks pass",
          onclick: () =>
            void ctx.busy(later, async () => {
              await api.pullMerge(view.repo, view.number, method.id, true);
              ctx.say("It will merge by itself when the checks pass.");
              await reloadAll();
            }),
        });
        buttons.push(later);
      }
    }

    let waiting: HTMLElement | null = null;
    if (view.state === "open" && view.auto_merge !== null) {
      const cancel: HTMLButtonElement = h("button", {
        type: "button",
        class: "btn sm",
        text: "Cancel",
        onclick: () =>
          void ctx.busy(cancel, async () => {
            await api.pullCancelMerge(view.repo, view.number);
            await reloadAll();
          }),
      });
      waiting = h(
        "div",
        { class: "row wrap pulls-auto" },
        h("span", { class: "grow pulls-auto-line", text: `Merging when checks pass · ${view.auto_merge}` }),
        cancel,
      );
    }

    const look = view.state !== "open" ? "is-done" : view.can_merge ? "is-ready" : "is-waiting";
    return h(
      "section",
      { class: `pulls-merge tone-yours ${look}`, "aria-label": "Merge status" },
      h(
        "div",
        { class: "row wrap pulls-merge-top" },
        h(
          "span",
          { class: "col pulls-merge-words" },
          h("span", { class: "pulls-merge-headline", text: view.headline }),
          view.reason ? h("span", { class: "soft", text: view.reason }) : null,
        ),
        buttons,
      ),
      waiting,
      view.checks.length ? h("div", { class: "col" }, view.checks.map(checkRow)) : null,
    );
  }

  function checkRow(check: CheckRow): HTMLElement {
    const action = check.action;
    let button: HTMLButtonElement | null = null;
    if (action) {
      const b: HTMLButtonElement = h("button", {
        type: "button",
        class: "btn sm thread",
        text: check.action_label ?? "Open",
        onclick: () =>
          void ctx.busy(b, async () => {
            if (action.type !== "approve_agent") {
              await ctx.runNeed(action);
              return;
            }
            const sure = await ctx.confirm({
              title: "Let this agent run?",
              body: check.detail ? `${check.name} · ${check.detail}` : check.name,
              confirm: "Approve and run",
            });
            if (!sure) return;
            await api.agentApprove(action.job_id);
            await reloadAll();
          }),
      });
      button = b;
    }
    return h(
      "div",
      { class: `row wrap pulls-check tone-${check.status.tone}` },
      pill(check.status.text, check.status.tone, "status"),
      h(
        "span",
        { class: "pulls-check-words" },
        h("span", { class: "pulls-check-name", text: check.name }),
        check.detail ? h("span", { class: "muted", text: ` · ${check.detail}` }) : null,
      ),
      button,
    );
  }

  function comment(row: CommentRow): HTMLElement {
    return h(
      "div",
      { class: `pulls-comment tone-${row.tone}` },
      h(
        "span",
        { class: "row wrap pulls-comment-top" },
        h("span", { class: "pulls-comment-who", text: row.who }),
        row.tag ? h("span", { class: "pulls-comment-tag", text: row.tag }) : null,
        h("span", { class: "muted push-right", text: ago(row.at) }),
      ),
      h("span", { class: "pulls-comment-body selectable", text: row.body }),
    );
  }

  function drawDetail(): void {
    const view = detail;
    if (!view) {
      const words = !picked ? "" : (detailError ?? "Reading this pull request…");
      mount(pane, words ? h("span", { class: "muted selectable", text: words }) : null);
      return;
    }

    const checkout: HTMLButtonElement = h("button", {
      type: "button",
      class: "btn pulls-foot-btn",
      text: "Check out this branch",
      onclick: () =>
        void ctx.busy(checkout, async () => {
          const out = await api.pullCheckout(view.repo, view.number);
          ctx.say(`Switched to ${out.branch}.`);
          await ctx.refresh();
          ctx.go({ screen: "repo", path: out.path, tab: "changes" });
        }),
    });
    const local = view.local_path;
    const commits = h("button", {
      type: "button",
      class: "btn pulls-foot-btn",
      text: "See the commits",
      disabled: local === null,
      title: local === null ? `This repository isn’t on this ${machine} yet. Check out the branch first.` : undefined,
      onclick: () => {
        if (local !== null) ctx.go({ screen: "repo", path: local, tab: "history" });
      },
    });
    const review: HTMLButtonElement = h("button", {
      type: "button",
      class: "btn link pulls-foot-btn",
      text: "Review files in your studio ↗",
      onclick: () => void ctx.busy(review, () => api.openWeb(`${view.web_path}/files`)),
    });

    mount(
      pane,
      h(
        "div",
        { class: "col pulls-title-block" },
        h("h2", { class: "pulls-title selectable", text: view.title }),
        h(
          "span",
          { class: "soft" },
          `${view.repo_short} · pull request ${view.number} · `,
          h("span", { class: "mono pulls-branches", text: `${view.head} → ${view.base}` }),
          ` · ${plural(view.commits, "commit", "commits")} · ${plural(view.files, "file", "files")} · by ${view.by}`,
        ),
      ),
      mergeCard(view),
      h(
        "section",
        { class: "section" },
        h("h3", { class: "h-section", text: "Conversation" }),
        view.comments.length ? view.comments.map(comment) : h("span", { class: "muted", text: "No conversation yet." }),
      ),
      h("div", { class: "row wrap pulls-foot" }, checkout, commits, review),
    );
  }

  drawList();
  drawDetail();
  // A pull request named by the route is read straight away, without
  // waiting for the list.
  if (picked) void loadDetail(true);
  void loadList();

  return {
    el,
    dispose() {
      disposed = true;
      clearTimeout(timer);
    },
    onEvent<K extends keyof Events>(name: K, _payload: Events[K]) {
      if (name !== "status") return;
      // Studio events come in bursts; read once they settle.
      clearTimeout(timer);
      timer = setTimeout(() => {
        if (disposed) return;
        void loadList();
        void loadDetail(false);
      }, 400);
    },
  };
}
