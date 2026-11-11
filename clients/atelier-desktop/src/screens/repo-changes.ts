// The Changes tab: the changed files with a tick each, the commit form, and
// the diff of the selected file.

import { api, asError, type DiffView, type FileChange, type RepoView } from "../api";
import { ago, h, mark, mount, plural } from "../dom";
import { diffRows, fileCounts, fileMark, initialTicks, tickedPaths } from "../logic/diff";
import type { RepoHost, TabView } from "./repo";

type Standing = Pick<RepoView, "branch" | "upstream" | "ahead" | "behind">;

/** `feature/x · 1 ahead of your studio · 2 behind` for the status strip. */
export function statusLine(view: Standing): string {
  const branch = view.branch ?? "detached";
  let line: string;
  if (view.upstream === null) line = `${branch} · not pushed to your studio yet`;
  else if (view.ahead > 0) line = `${branch} · ${view.ahead} ahead of your studio`;
  else line = `${branch} · in step with your studio`;
  return view.behind > 0 ? `${line} · ${view.behind} behind` : line;
}

/** The sentence under the "nothing to commit" heading. */
export function cleanLine(view: Standing & Pick<RepoView, "name">): string {
  if (view.ahead > 0) return `${plural(view.ahead, "commit is", "commits are")} waiting to be pushed.`;
  if (view.upstream === null) return `${view.name} isn’t pushed to your studio yet.`;
  if (view.behind > 0) return `${plural(view.behind, "commit is", "commits are")} waiting to be pulled.`;
  return `${view.name} is in step with your studio.`;
}

/** `Commit 3 files`, `Commit 1 file`; just `Commit` while nothing is ticked. */
export function commitLabel(ticked: number): string {
  return ticked === 0 ? "Commit" : `Commit ${plural(ticked, "file", "files")}`;
}

export function changesTab(host: RepoHost): TabView {
  const { ctx, path } = host;

  let files: FileChange[] = [];
  let clean: string | null = null;
  let ticks = new Map<string, boolean>();
  let selected: string | null = null;
  /** The file whose diff is drawn, so a reload of the same file keeps its scroll. */
  let shown: string | null = null;
  let diffSeq = 0;
  let loading: Promise<void> | null = null;
  let again = false;
  let disposed = false;
  const rows = new Map<string, HTMLElement>();

  // -------------------------------------------------------------- the form

  const count = h("span", { class: "muted" });
  const list = h("div", { class: "files-rows" });
  const message = h("input", { id: "repo-msg", class: "input sm", autocomplete: "off", spellcheck: "true" });
  const description = h("textarea", { id: "repo-desc", class: "textarea", rows: 3 });
  const who = h("span", { class: "muted small" });
  const commitButton = h("button", { type: "button", class: "btn lg primary grow", onclick: () => void commit(false) });
  const pushButton = h("button", {
    type: "button",
    class: "btn lg grow",
    text: "Commit and push",
    onclick: () => void commit(true),
  });

  const onKey = (event: Event) => {
    const e = event as KeyboardEvent;
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      void commit(false);
    }
  };
  message.addEventListener("keydown", onKey);
  description.addEventListener("keydown", onKey);
  message.addEventListener("input", updateForm);

  const form = h(
    "form",
    // Enter in the message box must not submit (and reload) the page.
    { class: "commit-form", onsubmit: (e) => e.preventDefault() },
    h("label", { for: "repo-msg", class: "label", text: "Commit message" }),
    message,
    h("label", { for: "repo-desc", class: "label", text: "Description (optional)" }),
    description,
    who,
    h("div", { class: "commit-buttons" }, commitButton, pushButton),
  );

  // -------------------------------------------------------------- the diff

  const diffPath = h("span", { class: "diff-path selectable" });
  const diffLang = h("span", { class: "muted" });
  const discardButton = h("button", {
    type: "button",
    class: "btn sm",
    text: "Discard changes…",
    onclick: () => void discard(),
  });
  const diffBody = h("div", { class: "diff-body", tabindex: 0, "aria-label": "Changes in the selected file" });
  const stripLine = h("span");
  const stripFetched = h("span");
  const stripPath = h("span", { class: "mono push-right selectable" });

  const cols = h(
    "div",
    { class: "changes-cols" },
    h(
      "div",
      { class: "files" },
      h(
        "div",
        { class: "files-list" },
        h("div", { class: "files-head" }, h("span", { class: "eyebrow grow", text: "Changed files" }), count),
        list,
      ),
      form,
    ),
    h(
      "div",
      { class: "diff" },
      h("div", { class: "diff-head" }, diffPath, diffLang, discardButton),
      diffBody,
      h("div", { class: "status-strip" }, stripLine, stripFetched, stripPath),
    ),
  );

  const el = h("div", { class: "changes" }, h("p", { class: "repo-note", text: "Looking for changes…" }));

  // ------------------------------------------------------------------ draw

  function tickedCount(): number {
    return tickedPaths(files, ticks).length;
  }

  function updateForm(): void {
    const n = tickedCount();
    count.textContent = `${n} of ${files.length} in this commit`;
    commitButton.textContent = commitLabel(n);
    const blocked = n === 0 || message.value.trim() === "";
    commitButton.disabled = blocked;
    pushButton.disabled = blocked;
  }

  function drawStrip(): void {
    const view = host.view();
    stripLine.textContent = statusLine(view);
    stripFetched.textContent = view.fetched_at ? `Fetched ${ago(view.fetched_at)}` : "Never fetched";
    stripPath.textContent = view.display_path;
    who.textContent = view.committing_as
      ? `Committing as ${view.committing_as}. Pushes are checked for secrets before they land.`
      : "Git doesn’t know who you are in this repository yet.";
  }

  function markSelected(): void {
    for (const [file, row] of rows) {
      const on = file === selected;
      row.classList.toggle("sel", on);
      const pick = row.querySelector("button");
      if (on) pick?.setAttribute("aria-current", "true");
      else pick?.removeAttribute("aria-current");
    }
  }

  function fileRow(file: FileChange): HTMLElement {
    const letter = fileMark(file);
    const counts = fileCounts(file);
    return h(
      "div",
      { class: "file-row" },
      h("input", {
        type: "checkbox",
        class: "check",
        checked: ticks.get(file.path) === true,
        "aria-label": `Include ${file.path} in this commit`,
        onchange: (e) => {
          ticks.set(file.path, (e.currentTarget as HTMLInputElement).checked);
          updateForm();
        },
      }),
      h(
        "button",
        { type: "button", class: "file-pick", onclick: () => select(file.path) },
        h("span", { class: `file-mark tone-${letter.tone}`, text: letter.letter }),
        // Right-to-left so the ellipsis eats the folders, not the file name;
        // the <bdi> keeps the path itself reading left to right.
        h(
          "span",
          { class: "file-path", title: file.old_path ? `was ${file.old_path}` : file.path },
          h("bdi", { text: file.path }),
        ),
        counts.add ? h("span", { class: "plus", text: counts.add }) : null,
        counts.del ? h("span", { class: "minus", text: counts.del }) : null,
      ),
    );
  }

  function cleanBanner(): HTMLElement {
    const view = host.view();
    return h(
      "div",
      { class: "changes-clean" },
      h(
        "div",
        { class: "clean-card" },
        mark(64, { width: 1.6 }),
        h(
          "div",
          { class: "clean-words" },
          h("span", { class: "muted", text: "Nothing to commit" }),
          h("span", { class: "clean-head", text: clean ?? "Nothing to commit." }),
          h(
            "span",
            { class: "clean-sub" },
            h("span", { text: cleanLine(view) }),
            view.ahead > 0
              ? h("button", {
                  type: "button",
                  class: "btn hot",
                  text: `Push ↑${view.ahead}`,
                  onclick: (e) => void host.push(e.currentTarget as HTMLButtonElement),
                })
              : null,
          ),
        ),
      ),
    );
  }

  function draw(): void {
    if (files.length === 0) {
      shown = null;
      diffSeq += 1;
      mount(el, cleanBanner());
      return;
    }
    if (cols.parentNode !== el) mount(el, cols);
    rows.clear();
    for (const file of files) rows.set(file.path, fileRow(file));
    mount(list, [...rows.values()]);
    markSelected();
    updateForm();
    drawStrip();
    void loadDiff();
  }

  function select(file: string): void {
    if (selected === file) return;
    selected = file;
    markSelected();
    void loadDiff();
  }

  function drawDiff(diff: DiffView): Node[] {
    if (diff.binary) {
      return [h("p", { class: "repo-note", text: "This is a binary file, so there is nothing to show line by line." })];
    }
    if (diff.lines.length === 0) {
      return [h("p", { class: "repo-note", text: "Nothing changed inside this file that can be shown line by line." })];
    }
    const out: Node[] = diffRows(diff.lines).map((row) =>
      h(
        "div",
        { class: `diff-row k-${row.kind}` },
        h("span", { class: "num", "aria-hidden": "true", text: row.a }),
        h("span", { class: "num", "aria-hidden": "true", text: row.b }),
        h("span", { class: "sign", text: row.mark }),
        h("span", { class: "code selectable", text: row.text }),
      ),
    );
    if (diff.truncated) {
      out.push(
        h("div", {
          class: "diff-more",
          text: "The rest of this file isn’t shown here. Open it in the editor to see all of it.",
        }),
      );
    }
    return out;
  }

  async function loadDiff(): Promise<void> {
    const file = selected;
    const seq = ++diffSeq;
    if (file === null) return;
    const same = shown === file;
    diffPath.textContent = file;
    if (!same) {
      diffLang.textContent = "";
      mount(diffBody, h("p", { class: "repo-note", text: "Reading the changes…" }));
    }
    try {
      const diff = await api.repoDiff(path, file);
      // A newer selection (or reload) has asked since: this answer is stale.
      if (disposed || seq !== diffSeq) return;
      const top = diffBody.scrollTop;
      const left = diffBody.scrollLeft;
      diffLang.textContent = diff.lang;
      mount(diffBody, drawDiff(diff));
      diffBody.scrollTop = same ? top : 0;
      diffBody.scrollLeft = same ? left : 0;
      shown = file;
    } catch (e) {
      if (disposed || seq !== diffSeq) return;
      shown = null;
      mount(diffBody, h("p", { class: "repo-note", text: asError(e).message || "The changes couldn’t be read." }));
    }
  }

  // --------------------------------------------------------------- actions

  async function commit(push: boolean): Promise<void> {
    const paths = tickedPaths(files, ticks);
    const subject = message.value.trim();
    if (paths.length === 0 || subject === "") return;
    await ctx.busy(push ? pushButton : commitButton, async () => {
      const result = await api.repoCommit(path, paths, subject, description.value.trim(), push);
      message.value = "";
      description.value = "";
      if (result.toast) ctx.toast(result.toast);
      else ctx.say(`Committed ${result.short}.`);
      if (result.push_error) ctx.say(result.push_error, "The commit was made; the push wasn’t.", "fail");
      await host.reloadAll();
    });
    // `busy` switches its button back on; put both back where they belong.
    if (!disposed) updateForm();
  }

  async function discard(): Promise<void> {
    const file = files.find((f) => f.path === selected);
    if (!file) return;
    const isNew = file.kind === "untracked" || file.kind === "added";
    const ok = await ctx.confirm({
      title: "Discard changes?",
      body: isNew
        ? `${file.path} is a new file, so discarding it deletes it. This can’t be undone.`
        : `Your changes to ${file.path} will be thrown away. This can’t be undone.`,
      confirm: "Discard changes",
      danger: true,
    });
    if (!ok) return;
    await ctx.busy(discardButton, async () => {
      await api.repoDiscard(path, [file.path]);
      await host.reloadAll();
    });
  }

  // ---------------------------------------------------------------- reload

  async function read(): Promise<void> {
    try {
      const view = await api.repoChanges(path);
      if (disposed) return;
      files = view.files;
      clean = view.clean;
      ticks = initialTicks(files, ticks);
      if (selected === null || !files.some((f) => f.path === selected)) selected = files[0]?.path ?? null;
      draw();
    } catch (e) {
      if (disposed) return;
      shown = null;
      mount(el, h("p", { class: "repo-note", text: asError(e).message || "The changes couldn’t be read." }));
    }
  }

  /** One read at a time; a request that arrives meanwhile runs once after it. */
  function reload(): Promise<void> {
    if (loading) {
      again = true;
      return loading;
    }
    loading = (async () => {
      try {
        do {
          again = false;
          await read();
        } while (again && !disposed);
      } finally {
        loading = null;
      }
    })();
    return loading;
  }

  return {
    el,
    reload,
    onView() {
      if (files.length > 0) drawStrip();
      else if (el.querySelector(".changes-clean")) mount(el, cleanBanner());
    },
    dispose() {
      disposed = true;
    },
  };
}
