// Confirmation, one-line prompt, log viewer, and the small anchored menu.

import { api, asError } from "./api";
import type { ConfirmOptions, MenuItem, PromptOptions } from "./ctx";
import { h, mount } from "./dom";

function modal(build: (close: () => void) => HTMLElement, onDismiss: () => void): void {
  const previous = document.activeElement as HTMLElement | null;
  const close = () => {
    document.removeEventListener("keydown", onKey, true);
    scrim.remove();
    previous?.focus?.();
  };
  const dismiss = () => {
    close();
    onDismiss();
  };
  const onKey = (e: KeyboardEvent) => {
    if (e.key === "Escape") {
      e.stopPropagation();
      dismiss();
    }
  };
  const scrim = h("div", {
    class: "scrim",
    onmousedown: (e) => {
      if (e.target === scrim) dismiss();
    },
  });
  scrim.appendChild(build(close));
  document.addEventListener("keydown", onKey, true);
  document.body.appendChild(scrim);
}

export function confirmDialog(options: ConfirmOptions): Promise<boolean> {
  return new Promise((resolve) => {
    modal(
      (close) => {
        const answer = (yes: boolean) => () => {
          close();
          resolve(yes);
        };
        const cancel = h("button", { type: "button", class: "btn lg", text: "Cancel", onclick: answer(false) });
        const dialog = h(
          "section",
          { class: "dialog", role: "alertdialog", "aria-modal": "true", "aria-label": options.title },
          h("div", { class: "dialog-head", text: options.title }),
          h("div", { class: "dialog-body" }, h("span", { text: options.body })),
          h(
            "div",
            { class: "dialog-foot" },
            cancel,
            h("button", {
              type: "button",
              class: options.danger ? "btn lg danger" : "btn lg primary",
              text: options.confirm,
              dataset: { confirm: "yes" },
              onclick: answer(true),
            }),
          ),
        );
        // The safe answer has the focus.
        queueMicrotask(() => cancel.focus());
        return dialog;
      },
      () => resolve(false),
    );
  });
}

export function promptDialog(options: PromptOptions): Promise<string | null> {
  return new Promise((resolve) => {
    modal(
      (close) => {
        const input = h("input", {
          class: ["input", options.mono && "mono"],
          id: "prompt-input",
          value: options.value ?? "",
          placeholder: options.placeholder ?? "",
          spellcheck: "false",
          autocapitalize: "off",
          autocomplete: "off",
        });
        const done = (value: string | null) => {
          close();
          resolve(value);
        };
        const form = h(
          "form",
          {
            class: "dialog",
            role: "dialog",
            "aria-modal": "true",
            "aria-label": options.title,
            onsubmit: (e) => {
              e.preventDefault();
              const value = input.value.trim();
              if (value) done(value);
            },
          },
          h("div", { class: "dialog-head", text: options.title }),
          h("div", { class: "dialog-body" }, h("div", { class: "field" }, h("label", { for: "prompt-input", text: options.label }), input)),
          h(
            "div",
            { class: "dialog-foot" },
            h("button", { type: "button", class: "btn lg", text: "Cancel", onclick: () => done(null) }),
            h("button", { type: "submit", class: "btn lg primary", text: options.confirm }),
          ),
        );
        queueMicrotask(() => {
          input.focus();
          input.select();
        });
        return form;
      },
      () => resolve(null),
    );
  });
}

/** Shows a failed run's log; loads it after the dialog is up. */
export function logDialog(repo: string, runId: number, webPath: string | null, openWeb: (path: string) => void): void {
  modal(
    (close) => {
      const title = h("div", { class: "dialog-head", text: "Reading the log…" });
      const pre = h("pre", { class: "log selectable", text: "" });
      const note = h("span", { class: "muted small" });
      const studio = h("button", {
        type: "button",
        class: "btn lg link",
        text: "Open in your studio ↗",
        onclick: () => {
          if (webPath) openWeb(webPath);
        },
      });
      if (!webPath) studio.classList.add("hidden");
      api
        .runLog(repo, runId)
        .then((log) => {
          title.textContent = log.title;
          pre.textContent = log.lines.join("\n") || "The log is empty.";
          note.textContent = log.truncated ? "Only the last lines are shown." : "";
          pre.scrollTop = pre.scrollHeight;
          if (log.web_path) {
            webPath = log.web_path;
            studio.classList.remove("hidden");
          }
        })
        .catch((e) => {
          const err = asError(e);
          title.textContent = "Couldn’t read the log";
          pre.textContent = [err.message, err.hint].filter(Boolean).join("\n");
        });
      return h(
        "section",
        { class: "dialog wide", role: "dialog", "aria-modal": "true", "aria-label": "Run log" },
        title,
        h("div", { class: "dialog-body" }, pre, note),
        h("div", { class: "dialog-foot" }, studio, h("button", { type: "button", class: "btn lg", text: "Close", onclick: close })),
      );
    },
    () => {},
  );
}

/** A small menu under `anchor`; closes on a pick, a click elsewhere or Escape. */
export function openMenu(anchor: HTMLElement, items: MenuItem[]): void {
  document.querySelector(".menu")?.remove();
  const menu = h("div", { class: "menu", role: "menu" });
  const close = () => {
    menu.remove();
    document.removeEventListener("mousedown", onDown, true);
    document.removeEventListener("keydown", onKey, true);
    window.removeEventListener("resize", close);
  };
  const onDown = (e: MouseEvent) => {
    if (!menu.contains(e.target as Node)) close();
  };
  const onKey = (e: KeyboardEvent) => {
    if (e.key === "Escape") {
      e.stopPropagation();
      close();
      anchor.focus();
    } else if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const buttons = [...menu.querySelectorAll<HTMLButtonElement>("button")];
      const at = buttons.indexOf(document.activeElement as HTMLButtonElement);
      const next = e.key === "ArrowDown" ? at + 1 : at - 1;
      buttons[(next + buttons.length) % buttons.length]?.focus();
    }
  };
  mount(
    menu,
    items.map((item) => {
      if ("separator" in item) return h("div", { class: "menu-sep", role: "separator" });
      if ("note" in item) return h("div", { class: "menu-note", text: item.note });
      return h(
        "button",
        {
          type: "button",
          role: "menuitem",
          class: ["menu-item", item.mono && "mono"],
          "aria-current": item.current ? "true" : undefined,
          onclick: () => {
            close();
            item.pick();
          },
        },
        h("span", { class: "clip", text: item.label }),
        item.hint ? h("span", { class: "hint", text: item.hint }) : null,
      );
    }),
  );
  document.body.appendChild(menu);
  const box = anchor.getBoundingClientRect();
  const width = menu.offsetWidth;
  const left = Math.max(8, Math.min(box.left, window.innerWidth - width - 8));
  menu.style.left = `${left}px`;
  menu.style.top = `${Math.min(box.bottom + 6, window.innerHeight - 80)}px`;
  document.addEventListener("mousedown", onDown, true);
  document.addEventListener("keydown", onKey, true);
  window.addEventListener("resize", close);
  menu.querySelector<HTMLButtonElement>("button")?.focus();
}
