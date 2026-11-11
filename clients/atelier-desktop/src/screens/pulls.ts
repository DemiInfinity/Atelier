// Pull requests: the open ones across the studio on the left, the picked
// one on the right with its merge status, checks and conversation.

import { api, asError, type BranchUpdate, type CheckRow, type ChecksBrief, type CommentRow, type Events, type PullRow, type PullView } from "../api";
import type { Ctx, MenuItem, Screen } from "../ctx";
import { conflictGuide } from "../dialogs";
import { ago, h, icon, mount, pill, plural } from "../dom";
import { markdown, type MarkdownHandlers } from "../markdown";
import { behindLabel, pickMethod, pickPull, shortRepo, updateLabel, visiblePulls, type PullKey } from "../logic/pulls";
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

/** A pull request's checks at a glance: a dot in the state's colour and a few words. */
export function checksChip(checks: ChecksBrief): HTMLElement {
  return h(
    "span",
    { class: `checks-chip tone-${checks.tone} is-${checks.state}`, title: checks.title, "aria-label": `Checks: ${checks.text}` },
    h("span", { class: "dot xs", "aria-hidden": "true" }),
    h("span", { text: checks.text }),
  );
}

/** One pull request in a list. `showRepo` is false inside a repository, where it would only repeat. */
export function pullCard(row: PullRow, current: boolean, showRepo: boolean, pick: () => void): HTMLElement {
  const behind = behindLabel(row.behind, row.base);
  return h(
    "button",
    { type: "button", class: "pulls-card", "aria-current": current ? "true" : "false", onclick: pick },
    h("span", { class: "pulls-card-title", text: row.title }),
    h("span", { class: "muted", text: showRepo ? `${row.repo_short} · ${row.by}` : `pull request ${row.number} · ${row.by}` }),
    h(
      "span",
      { class: "row wrap pulls-card-pills" },
      pill(row.status.text, row.status.tone),
      row.via ? pill(row.via.text, row.via.tone) : null,
      row.conflicts ? pill("Conflicts", "fail", "pulls-conflicts") : null,
      checksChip(row.checks),
      behind ? h("span", { class: "pulls-behind", title: `${row.head} doesn’t have the latest ${row.base} yet.`, text: behind }) : null,
    ),
  );
}

/** The global "Pull requests" screen: every repository's. */
export function pullsScreen(ctx: Ctx, route: Extract<Route, { screen: "pulls" }>): Screen {
  return pullsView(ctx, route, null);
}

/**
 * The list and the picked pull request. With `scope`, only that
 * repository's (the Pull requests tab of a repository).
 */
export function pullsView(
  ctx: Ctx,
  route: { repo?: string; number?: number },
  scope: { repo: string; name: string } | null,
): Screen & { reload(): Promise<void> } {
  const machine = ctx.app().machine;

  let rows: PullRow[] | null = null;
  let listError: string | null = null;
  let onlyRepo: string | null = !scope && route.repo !== undefined && route.number === undefined ? route.repo : null;
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
    { class: ["screen pulls", scope && "scoped"] },
    h(
      "div",
      { class: "pulls-side" },
      h(
        "header",
        { class: "pulls-head", "data-tauri-drag-region": true },
        scope ? null : h("h1", { class: "h-screen grow", text: "Pull requests" }),
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
    count.textContent = scope ? `${rows.length} open in ${scope.name}` : `${rows.length} open across your studio`;
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
        ? h("span", {
            class: "muted pulls-note",
            text: scope ? `No open pull requests in ${scope.name}. Quiet is good.` : "No open pull requests. Quiet is good.",
          })
        : visible.map((row) => {
            const key = { repo: row.repo, number: row.number };
            return pullCard(row, same(key, picked), !scope, () => select(key));
          }),
    );
  }

  async function loadList(): Promise<void> {
    const seq = ++listSeq;
    try {
      const next = await api.pullsList(scope?.repo);
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

  /** "Update from <base>": the studio merges the base into the branch. */
  async function updateBranch(view: PullView): Promise<void> {
    let done: BranchUpdate;
    for (;;) {
      try {
        done = await api.pullUpdate(view.repo, view.number);
        break;
      } catch (e) {
        // A push landed on the branch meanwhile: nothing was done; offer to try again.
        const err = asError(e);
        if (err.kind !== "branch_moved") throw e;
        const again = await ctx.confirm({
          title: "The branch changed just now",
          body: `${err.message || `Someone pushed to ${view.head} meanwhile.`} Nothing was updated.`,
          confirm: "Try again",
        });
        if (!again) {
          await reloadAll();
          return;
        }
      }
    }
    const base = done.base || view.base;
    const head = done.head || view.head;
    if (done.outcome === "conflicts") {
      ctx.showConflicts({ head, base, message: done.message, files: done.files, localPath: done.local_path });
      await reloadAll();
      return;
    }
    if (done.outcome === "up_to_date") {
      ctx.say(`Already up to date with ${base}.`);
      await reloadAll();
      return;
    }
    ctx.say(`Updated from ${base}.`, `${head} has the latest ${base} now. Its checks run again.`);
    // The pull request and its checks, as they are after the update.
    await reloadAll();
    const local = done.local_path;
    if (!done.local_on_branch || local === null) return;
    // The folder here is on that branch and now behind the studio. Never
    // pulled without asking.
    const pull = await ctx.confirm({
      title: "Your local branch is now behind",
      body: `${head} in your studio has the new commits from ${base}; the folder on this ${machine} doesn’t yet. Pull them in?`,
      confirm: "Pull",
    });
    if (!pull) return;
    const n = await api.repoPull(local);
    ctx.say(n > 0 ? `Pulled ${plural(n, "commit", "commits")}.` : "Already up to date.");
    await ctx.refresh();
  }

  function mergeCard(view: PullView): HTMLElement {
    const behind = view.state === "open" ? behindLabel(view.behind, view.base) : null;
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

    // Behind its base: one click merges the base in, in the studio. It
    // works while a merge waits for green checks too.
    if (view.can_update) {
      const update: HTMLButtonElement = h("button", {
        type: "button",
        class: "btn lg pulls-update",
        text: updateLabel(view.base),
        title: `Merge ${view.base} into ${view.head} in your studio (${plural(view.behind, "commit", "commits")} behind).`,
        onclick: () => void ctx.busy(update, () => updateBranch(view)),
      });
      buttons.push(update);
    } else if (view.update_on_github !== null) {
      // At home on GitHub: the branch is brought up to date there.
      const name = view.update_on_github;
      const github: HTMLButtonElement = h("button", {
        type: "button",
        class: "btn lg pulls-update",
        text: "Open on GitHub ↗",
        title: `This repository’s home is GitHub. Update ${view.head} from ${view.base} there.`,
        onclick: () => void ctx.busy(github, () => api.openGithub(name)),
      });
      buttons.push(github);
    }

    let conflicts: HTMLElement | null = null;
    if (view.conflicts) {
      const local = view.local_path;
      const options = { head: view.head, base: view.base, files: view.conflict_files, localPath: local };
      const open: HTMLButtonElement | null =
        local === null
          ? null
          : h("button", {
              type: "button",
              class: "btn pulls-conflicts-open",
              text: `Open in ${ctx.app().editor.label}`,
              onclick: () =>
                void ctx.busy(open, async () => {
                  const label = await api.repoOpen(local);
                  ctx.say(`Opened in ${label}.`);
                }),
            });
      conflicts = h(
        "div",
        { class: "col pulls-conflicts-box", role: "group", "aria-label": `Conflicts with ${view.base}` },
        conflictGuide(options),
        h(
          "div",
          { class: "row wrap pulls-conflicts-foot" },
          open,
          local === null ? h("span", { class: "muted small", text: `This repository isn’t on this ${machine} yet. Check out the branch first.` }) : null,
        ),
      );
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

    const look = view.state !== "open" ? "is-done" : view.conflicts ? "is-conflicting" : view.can_merge ? "is-ready" : "is-waiting";
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
          behind ? h("span", { class: "pulls-behind", text: behind }) : null,
        ),
        buttons,
      ),
      conflicts,
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
      markdown(row.body_md, links, "pulls-comment-body"),
    );
  }

  /** `#12` in a comment: that pull request here; an issue opens in the browser. */
  async function openRef(number: number): Promise<void> {
    const repo = detail?.repo;
    if (!repo) return;
    try {
      if (!rows?.some((r) => r.repo === repo && r.number === number)) await api.pullDetail(repo, number);
      select({ repo, number });
    } catch {
      await ctx.busy(null, () => api.openWeb(`/${repo}/issues/${number}`));
    }
  }
  const links: MarkdownHandlers = { openLink: (url) => ctx.openLink(url), openRef: (number) => void openRef(number) };

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
          ctx.go({ screen: "repo", path: out.path });
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
    async reload() {
      await Promise.all([loadList(), loadDetail(false)]);
    },
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
