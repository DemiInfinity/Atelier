// The Branches tab: the local branches, where each stands against the
// studio, and a way to switch or start a new one.

import { api, asError, type Branch } from "../api";
import { h, mount, pill } from "../dom";
import type { RepoHost, TabView } from "./repo";

/** `↑2 ↓1`, leaving out the zero side; empty when the branch is in step. */
export function aheadBehind(ahead: number, behind: number): string {
  const parts: string[] = [];
  if (ahead > 0) parts.push(`↑${ahead}`);
  if (behind > 0) parts.push(`↓${behind}`);
  return parts.join(" ");
}

export function branchesTab(host: RepoHost): TabView {
  const { ctx, path } = host;
  let disposed = false;
  let seq = 0;

  const list = h("div", { class: "branches-list" }, h("p", { class: "repo-note", text: "Reading the branches…" }));
  const el = h(
    "div",
    { class: "branches" },
    h(
      "div",
      { class: "branches-head" },
      h("h2", { class: "h-section grow", text: "Branches on this " + ctx.app().machine }),
      h("button", { type: "button", class: "btn", text: "New branch…", onclick: () => void host.newBranch(null) }),
    ),
    list,
  );

  function row(b: Branch): HTMLElement {
    return h(
      "div",
      { class: "branch-row" },
      h(
        "span",
        { class: "branch-name" },
        h("span", { class: "mono clip selectable", title: b.name, text: b.name }),
        b.current ? pill("current", "yours") : null,
      ),
      h("span", { class: "muted clip", title: b.upstream ?? undefined, text: b.upstream ?? "not in your studio yet" }),
      h("span", { class: "mono small muted", text: aheadBehind(b.ahead, b.behind) }),
      h("span", { class: "soft clip", title: b.subject, text: b.subject }),
      h(
        "span",
        { class: "branch-do" },
        b.current
          ? null
          : h("button", {
              type: "button",
              class: "btn sm",
              text: "Switch",
              "aria-label": `Switch to ${b.name}`,
              onclick: (e) => void host.switchBranch(b.name, e.currentTarget as HTMLButtonElement),
            }),
      ),
    );
  }

  async function reload(): Promise<void> {
    const mine = ++seq;
    try {
      const branches = await api.repoBranches(path);
      if (disposed || mine !== seq) return;
      if (branches.length === 0) {
        mount(list, h("p", { class: "empty", text: "No branches yet. The first commit makes one." }));
      } else {
        mount(list, h("div", { class: "table" }, branches.map(row)));
      }
    } catch (e) {
      if (disposed || mine !== seq) return;
      mount(list, h("p", { class: "repo-note", text: asError(e).message || "The branches couldn’t be read." }));
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
