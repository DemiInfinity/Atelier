// Confirmation, one-line prompt, log viewer, and the small anchored menu.

import { api, asError } from "./api";
import type { ComposeOptions, ConfirmOptions, ConflictsOptions, MenuItem, PromptOptions, TypedConfirmOptions } from "./ctx";
import { h, mount, plural } from "./dom";
import { resolveSteps } from "./logic/pulls";
import { writePreview, type MarkdownHandlers } from "./markdown";

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

/**
 * Asks before something that can't be undone: the red button only works
 * once `expect` has been typed exactly.
 */
export function typedConfirmDialog(options: TypedConfirmOptions): Promise<string | null> {
  return new Promise((resolve) => {
    modal(
      (close) => {
        const done = (value: string | null) => {
          close();
          resolve(value);
        };
        const confirm = h("button", { type: "submit", class: "btn lg danger", text: options.confirm, disabled: true });
        const input = h("input", {
          class: "input mono",
          id: "typed-confirm-input",
          spellcheck: "false",
          autocapitalize: "off",
          autocomplete: "off",
          oninput: () => {
            confirm.disabled = input.value.trim() !== options.expect;
          },
        });
        const form = h(
          "form",
          {
            class: "dialog",
            role: "alertdialog",
            "aria-modal": "true",
            "aria-label": options.title,
            onsubmit: (e) => {
              e.preventDefault();
              if (input.value.trim() === options.expect) done(input.value.trim());
            },
          },
          h("div", { class: "dialog-head", text: options.title }),
          h(
            "div",
            { class: "dialog-body" },
            options.body.map((line) => h("span", { text: line })),
            h(
              "div",
              { class: "field" },
              h("label", { for: "typed-confirm-input" }, "Type ", h("span", { class: "mono selectable", text: options.expect }), " to confirm"),
              input,
            ),
          ),
          h(
            "div",
            { class: "dialog-foot" },
            h("button", { type: "button", class: "btn lg", text: "Cancel", onclick: () => done(null) }),
            confirm,
          ),
        );
        queueMicrotask(() => input.focus());
        return form;
      },
      () => resolve(null),
    );
  });
}

/** A title and a longer text (a new issue). */
export function composeDialog(
  options: ComposeOptions,
  links: MarkdownHandlers = { openLink: () => {} },
): Promise<{ title: string; body: string } | null> {
  return new Promise((resolve) => {
    modal(
      (close) => {
        const done = (value: { title: string; body: string } | null) => {
          close();
          resolve(value);
        };
        const submit = h("button", { type: "submit", class: "btn lg primary", text: options.confirm, disabled: true });
        const title = h("input", {
          class: "input",
          id: "compose-title",
          autocomplete: "off",
          oninput: () => {
            submit.disabled = title.value.trim() === "";
          },
        });
        const body = h("textarea", { class: "textarea compose-body", id: "compose-body", rows: "8" });
        const form = h(
          "form",
          {
            class: "dialog wide",
            role: "dialog",
            "aria-modal": "true",
            "aria-label": options.title,
            onsubmit: (e) => {
              e.preventDefault();
              if (title.value.trim()) done({ title: title.value.trim(), body: body.value });
            },
          },
          h("div", { class: "dialog-head", text: options.title }),
          h(
            "div",
            { class: "dialog-body" },
            h("div", { class: "field" }, h("label", { for: "compose-title", text: options.titleLabel }), title),
            h("div", { class: "field" }, h("label", { for: "compose-body", text: options.bodyLabel }), writePreview(body, links)),
          ),
          h(
            "div",
            { class: "dialog-foot" },
            h("button", { type: "button", class: "btn lg", text: "Cancel", onclick: () => done(null) }),
            submit,
          ),
        );
        queueMicrotask(() => title.focus());
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

/**
 * The files that conflict and the steps to resolve them, shared by the
 * dialog and the pull request's own card. Paths and branch names are only
 * ever put in as text.
 */
export function conflictGuide(options: ConflictsOptions): HTMLElement {
  const { files } = options;
  return h(
    "div",
    { class: "conflicts-guide" },
    files.length
      ? h(
          "div",
          { class: "col conflicts-block" },
          h("span", { class: "conflicts-label", text: `${plural(files.length, "file conflicts", "files conflict")} with ${options.base}` }),
          h("ul", { class: "conflicts-files selectable", "aria-label": "Conflicting files" }, files.map((file) => h("li", { class: "mono", text: file }))),
        )
      : null,
    h(
      "div",
      { class: "col conflicts-block" },
      h("span", { class: "conflicts-label", text: "To resolve it on this machine" }),
      h(
        "ol",
        { class: "conflicts-steps" },
        resolveSteps(options.head, options.base).map((step) =>
          h("li", {}, h("span", { text: step.text }), step.command ? h("code", { class: "mono selectable", text: step.command }) : null),
        ),
      ),
    ),
  );
}

/** "Update from <base>" hit conflicts: nothing changed; here is what to do. */
export function conflictsDialog(options: ConflictsOptions, editorLabel: string, openEditor: (path: string) => void): void {
  modal(
    (close) => {
      const local = options.localPath;
      const done = h("button", { type: "button", class: "btn lg", text: "Close", onclick: close });
      queueMicrotask(() => done.focus());
      return h(
        "section",
        { class: "dialog conflicts-dialog", role: "dialog", "aria-modal": "true", "aria-label": `Conflicts with ${options.base}` },
        h("div", { class: "dialog-head", text: `${options.head} conflicts with ${options.base}` }),
        h(
          "div",
          { class: "dialog-body" },
          h("span", {
            text: options.message || `Your studio couldn’t merge ${options.base} into ${options.head} by itself. Nothing was changed.`,
          }),
          conflictGuide(options),
          local === null ? h("span", { class: "muted small", text: "This repository isn’t on this machine yet. Check out the branch first." }) : null,
        ),
        h(
          "div",
          { class: "dialog-foot" },
          local !== null
            ? h("button", {
                type: "button",
                class: "btn lg primary",
                text: `Open in ${editorLabel}`,
                dataset: { conflicts: "open" },
                onclick: () => {
                  close();
                  openEditor(local);
                },
              })
            : null,
          done,
        ),
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
