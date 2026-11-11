// Draws the pop-up queue in the bottom right corner, styled like the
// designs: a tinted badge with the A, the words, and maybe two buttons.

import type { Target, Toast } from "./api";
import { h, mark, mount } from "./dom";
import { ToastQueue } from "./logic/toasts";

export function toastLayer(follow: (target: Target) => void): { el: HTMLElement; queue: ToastQueue } {
  const el = h("div", { class: "toasts", "aria-live": "polite" });
  const queue = new ToastQueue((visible) => mount(el, visible.map(draw)));

  function draw(toast: Toast): HTMLElement {
    const open = (target: Target) => {
      queue.dismiss(toast.id);
      follow(target);
    };
    const actions = toast.action
      ? h(
          "span",
          { class: "actions" },
          h("button", {
            type: "button",
            class: "btn sm thread",
            text: toast.action.label,
            onclick: (e) => {
              e.stopPropagation();
              open(toast.action!.target);
            },
          }),
          h("button", {
            type: "button",
            class: "btn sm later",
            text: "Later",
            onclick: (e) => {
              e.stopPropagation();
              queue.dismiss(toast.id);
            },
          }),
        )
      : null;
    return h(
      "div",
      {
        class: `toast tone-${toast.tone}`,
        role: "status",
        dataset: { toast: toast.id },
        onclick: () => open(toast.target),
        onmouseenter: () => queue.hold(toast.id),
        onmouseleave: () => queue.release(toast.id),
      },
      h("span", { class: "badge", "aria-hidden": "true" }, mark(20, { boxed: false, stroke: "var(--tone)", width: 2.4 })),
      h(
        "span",
        { class: "words" },
        h("span", { class: "text", text: toast.text }),
        toast.sub ? h("span", { class: "sub", text: toast.sub }) : null,
        actions,
      ),
    );
  }

  return { el, queue };
}
