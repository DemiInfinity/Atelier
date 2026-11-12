// The repositories part of the sidebar: "Pinned", one group per owner that
// folds, and "Other folders". Built from plain data so it can be drawn (and
// tested) without the rest of the window.

import type { WorkingCopy } from "./api";
import { h, icon, type Child } from "./dom";
import type { SideGroup } from "./logic/sidebar";

export interface SideActions {
  /** The sidebar key of the screen that is showing (`repo:/path`). */
  currentKey: string;
  open(copy: WorkingCopy): void;
  /** The "…" menu of a row (also on a right-click). */
  more(copy: WorkingCopy, anchor: HTMLElement): void;
  toggle(group: SideGroup): void;
  publish(copy: WorkingCopy): void;
  othersOpen: boolean;
  toggleOthers(): void;
  /** Rows to draw under a repository's row (its linked worktrees). */
  under?(copy: WorkingCopy): Child;
}

function repoRow(copy: WorkingCopy, actions: SideActions): Child {
  return [repoLine(copy, actions), actions.under?.(copy) ?? null];
}

function repoLine(copy: WorkingCopy, actions: SideActions): HTMLElement {
  const known = copy.full_name !== null;
  const more: HTMLButtonElement = h(
    "button",
    {
      type: "button",
      class: "side-more",
      title: `More for ${copy.name}`,
      "aria-label": `More for ${copy.name}`,
      "aria-haspopup": "menu",
      onclick: () => actions.more(copy, more),
    },
    icon("more", 14, 3),
  );
  return h(
    "div",
    {
      class: "side-row",
      dataset: { path: copy.path },
      oncontextmenu: (e) => {
        e.preventDefault();
        actions.more(copy, more);
      },
    },
    h(
      "button",
      {
        type: "button",
        class: "side-link side-repo",
        title: known ? `${copy.name} · ${copy.branch} · ${copy.state}` : `${copy.name} · ${copy.owner} · not in your studio`,
        "aria-current": actions.currentKey === `repo:${copy.path}` ? "page" : undefined,
        onclick: () => actions.open(copy),
      },
      h("span", { class: ["sq", known && copy.tone === "yours" && "hot"], "aria-hidden": "true" }),
      h("span", { class: "grow clip", text: copy.name }),
      known && copy.short ? h("span", { class: `state tone-${copy.tone}`, text: copy.short }) : null,
    ),
    more,
    known
      ? null
      : h("button", {
          type: "button",
          class: "side-publish",
          text: "Publish",
          title: `Publish ${copy.name} to Atelier`,
          "aria-label": `Publish ${copy.name} to Atelier`,
          onclick: () => actions.publish(copy),
        }),
  );
}

function header(label: string, count: number, open: boolean, kind: string, onclick: () => void): HTMLElement {
  return h(
    "button",
    { type: "button", class: `group-name group-toggle kind-${kind}`, "aria-expanded": open ? "true" : "false", onclick },
    h("span", { class: ["twist", open && "open"], "aria-hidden": "true" }, icon("chevron", 10, 2.4)),
    kind === "pinned" ? h("span", { class: "group-pin", "aria-hidden": "true" }, icon("pin", 11, 2.2)) : null,
    h("span", { class: "grow clip", text: label }),
    h("span", { class: "count", text: count }),
  );
}

/** The groups of the sidebar's repository list. */
export function sideRepos(groups: readonly SideGroup[], others: readonly WorkingCopy[], actions: SideActions): Child[] {
  const loose = others.filter((copy) => !copy.pinned);
  return [
    groups.map((group) =>
      h(
        "div",
        { class: "group", dataset: { group: group.key } },
        header(group.label, group.copies.length, !group.collapsed, group.kind, () => actions.toggle(group)),
        group.collapsed ? null : group.copies.map((copy) => repoRow(copy, actions)),
      ),
    ),
    loose.length > 0
      ? h(
          "div",
          { class: "group", dataset: { group: ":others" } },
          header("Other folders", loose.length, actions.othersOpen, "other", () => actions.toggleOthers()),
          actions.othersOpen ? loose.map((copy) => repoRow(copy, actions)) : null,
        )
      : null,
  ];
}
