// Markdown, drawn from the typed tree the Rust side parses (see
// src-tauri/src/markdown.rs). Content never becomes markup here: every
// element below is one of a fixed few, made with createElement, and every
// piece of content goes in through textContent. Links have no href (the
// window never navigates); a click hands the address to `openLink`, which
// lets Rust decide whether it may be opened.

import { api, asError, type MdBlock, type MdInline } from "./api";
import { h, mount, type Child } from "./dom";
import "./styles/markdown.css";

export interface MarkdownHandlers {
  /** Opens a web or mail address in the system browser (after Rust has checked it). */
  openLink(url: string): void;
  /** Goes to issue or pull request `number` of the same repository. Without it `#12` is plain text. */
  openRef?(number: number): void;
}

/** Something that acts like a link without being one the window could follow. */
function linkLike(cls: string, title: string, children: Child, activate: () => void): HTMLElement {
  return h(
    "a",
    {
      class: cls,
      role: "link",
      tabindex: "0",
      title,
      onclick: (e) => {
        e.preventDefault();
        activate();
      },
      onkeydown: (e) => {
        if ((e as KeyboardEvent).key === "Enter") {
          e.preventDefault();
          activate();
        }
      },
    },
    children,
  );
}

function inline(node: MdInline, on: MarkdownHandlers): Child {
  switch (node.t) {
    case "text":
      return document.createTextNode(node.text);
    case "code":
      return h("code", { class: "md-code-inline mono", text: node.text });
    case "em":
      return h("em", {}, inlines(node.inlines, on));
    case "strong":
      return h("strong", {}, inlines(node.inlines, on));
    case "strike":
      return h("s", {}, inlines(node.inlines, on));
    case "link": {
      const url = node.url;
      return linkLike(node.image ? "md-link md-image" : "md-link", node.image ? `Image: ${url}` : url, inlines(node.inlines, on), () => on.openLink(url));
    }
    case "ref": {
      const number = node.number;
      const open = on.openRef;
      return open
        ? linkLike("md-link md-ref", `Issue or pull request ${number}`, `#${number}`, () => open(number))
        : h("span", { class: "md-ref", text: `#${number}` });
    }
    case "mention":
      return h("span", { class: "md-mention", text: `@${node.name}` });
    case "break":
      return h("br");
    default:
      // A kind this version doesn't know: nothing is drawn for it.
      return null;
  }
}

function inlines(nodes: readonly MdInline[], on: MarkdownHandlers): Child[] {
  return nodes.map((node) => inline(node, on));
}

const HEADINGS = ["h3", "h3", "h4", "h5", "h6", "h6"] as const;
const ALIGNS = new Set(["left", "center", "right"]);

function block(node: MdBlock, on: MarkdownHandlers): Child {
  switch (node.t) {
    case "heading": {
      // The screen's own title is the h2; what people write starts below it.
      const level = Math.min(6, Math.max(1, Math.trunc(node.level) || 1));
      return h(HEADINGS[level - 1] ?? "h6", { class: `md-h md-h${level}` }, inlines(node.inlines, on));
    }
    case "paragraph":
      return h("p", {}, inlines(node.inlines, on));
    case "code":
      return h(
        "div",
        { class: "md-code" },
        node.lang ? h("span", { class: "md-code-lang", text: node.lang }) : null,
        h("pre", { class: "mono", tabindex: "0" }, h("code", { text: node.text })),
      );
    case "quote":
      return h("blockquote", {}, blocks(node.blocks, on));
    case "list": {
      const tasks = node.items.some((item) => item.task !== null);
      const items = node.items.map((item) => {
        if (item.task === null) return h("li", {}, blocks(item.blocks, on));
        return h(
          "li",
          { class: "md-task" },
          h("input", {
            type: "checkbox",
            class: "check md-task-box",
            checked: item.task,
            disabled: true,
            "aria-label": item.task ? "Done" : "Not done",
          }),
          h("div", { class: "md-task-words" }, blocks(item.blocks, on)),
        );
      });
      if (!node.ordered) return h("ul", { class: tasks ? "md-tasks" : undefined }, items);
      const start = Math.trunc(node.start);
      return h("ol", { class: tasks ? "md-tasks" : undefined, start: Number.isFinite(start) && start !== 1 ? String(start) : undefined }, items);
    }
    case "table": {
      const cell = (tag: "th" | "td", cells: readonly MdInline[], column: number) => {
        const align = node.aligns[column] ?? "";
        return h(tag, { class: ALIGNS.has(align) ? `md-align-${align}` : undefined }, inlines(cells, on));
      };
      return h(
        "div",
        { class: "md-table", tabindex: "0" },
        h(
          "table",
          {},
          h("thead", {}, h("tr", {}, node.head.map((cells, i) => cell("th", cells, i)))),
          h("tbody", {}, node.rows.map((row) => h("tr", {}, row.map((cells, i) => cell("td", cells, i))))),
        ),
      );
    }
    case "rule":
      return h("hr");
    default:
      return null;
  }
}

function blocks(nodes: readonly MdBlock[], on: MarkdownHandlers): Child[] {
  return nodes.map((node) => block(node, on));
}

/** The rendered text. `extra` adds classes to the wrapper. */
export function markdown(nodes: readonly MdBlock[], on: MarkdownHandlers, extra = ""): HTMLElement {
  return h("div", { class: `md selectable ${extra}`.trim() }, blocks(nodes, on));
}

/**
 * "Write / Preview" over a text box: Preview shows what was typed through
 * the same renderer. Returns the wrapper to put where the box would go.
 */
export function writePreview(
  box: HTMLTextAreaElement,
  on: MarkdownHandlers,
  options: { previewing?: boolean; onMode?: (previewing: boolean) => void } = {},
): HTMLElement {
  let seq = 0;
  const pane = h("div", { class: "md-preview hidden", "aria-label": "Preview", "aria-live": "polite" });
  const tab = (label: string, preview: boolean) =>
    h("button", {
      type: "button",
      class: "md-tab",
      role: "tab",
      text: label,
      "aria-selected": "false",
      onclick: () => show(preview),
    });
  const write = tab("Write", false);
  const preview = tab("Preview", true);

  function show(previewing: boolean): void {
    write.setAttribute("aria-selected", String(!previewing));
    preview.setAttribute("aria-selected", String(previewing));
    box.classList.toggle("hidden", previewing);
    pane.classList.toggle("hidden", !previewing);
    options.onMode?.(previewing);
    const mine = ++seq;
    if (!previewing) return;
    const source = box.value;
    if (source.trim() === "") {
      mount(pane, h("span", { class: "muted", text: "Nothing to preview." }));
      return;
    }
    // The pane keeps the box's height, so nothing jumps.
    api
      .markdownPreview(source)
      .then((tree) => {
        if (mine === seq) mount(pane, markdown(tree, on));
      })
      .catch((e) => {
        if (mine === seq) mount(pane, h("span", { class: "muted", text: asError(e).message || "Couldn’t make the preview." }));
      });
  }

  const el = h("div", { class: "md-write" }, h("div", { class: "md-tabs", role: "tablist" }, write, preview), box, pane);
  show(options.previewing === true);
  return el;
}
