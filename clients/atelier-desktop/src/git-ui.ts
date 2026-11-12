// Small building blocks the git client's screens share. Like everything in
// this app they build nodes with `h()` only: text from git (file names,
// commit messages, code) is only ever put in the page as text.

import { asError } from "./api";
import { h, mount, type Child } from "./dom";

/** `e04a9d1` */
export function shortId(id: string): string {
  return id.slice(0, 7);
}

/** `+12 −3`, leaving out a side that is zero; empty for a binary file. */
export function plusMinus(additions: number | null, deletions: number | null): string {
  const parts: string[] = [];
  if (additions) parts.push(`+${additions}`);
  if (deletions) parts.push(`−${deletions}`);
  return parts.join(" ");
}

/**
 * The stand-in every area's screen starts as: a title, a line saying what
 * will be here, and a list of facts read through the area's own commands
 * (so the wiring is proven before the screen is drawn).
 */
export function placeholder(opts: { area: string; title: string; about: string; facts: HTMLElement; actions?: Child }): HTMLElement {
  return h(
    "div",
    { class: `git-placeholder git-${opts.area}`, dataset: { area: opts.area } },
    h("h2", { class: "h-section", text: opts.title }),
    h("p", { class: "muted", text: opts.about }),
    opts.facts,
    opts.actions ? h("div", { class: "git-placeholder-actions" }, opts.actions) : null,
  );
}

/** A list of plain lines, for a placeholder's facts. */
export function factList(): { el: HTMLElement; set(lines: string[]): void; fail(error: unknown): void } {
  const el = h("ul", { class: "git-facts" }, h("li", { class: "muted", text: "Reading…" }));
  return {
    el,
    set: (lines) => mount(el, lines.map((line) => h("li", { class: "selectable", text: line }))),
    fail: (error) => mount(el, h("li", { class: "muted", text: asError(error).message || "That couldn’t be read." })),
  };
}
