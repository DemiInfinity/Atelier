// Messages: the little notes Atelier leaves when something happens. Pick a
// moment, read our lines, add your own, and see how one pops up.
//
// Every change goes to the Rust side and comes back as the whole view, so
// this screen never keeps its own copy of the lines.

import "../styles/messages.css";
import { api, asError, type MessageLine, type MessagesView, type MomentView, type Tone } from "../api";
import type { Ctx, Screen } from "../ctx";
import { checkbox, h, mark, mount } from "../dom";
import type { Route } from "../routes";

// ------------------------------------------------------------------ logic

/** A moment's lines in the order they are listed: ours, then yours. */
export function linesOf(moment: MomentView): { line: MessageLine; mine: boolean; own: number }[] {
  return [
    ...moment.ours.map((line) => ({ line, mine: false, own: 0 })),
    // `own` is the 1-based number the remove command wants.
    ...moment.yours.map((line, i) => ({ line, mine: true, own: i + 1 })),
  ];
}

export interface Preview {
  text: string;
  sub: string;
  /** null: messages are off, drawn with a neutral border. */
  tone: Tone | null;
}

/** What the preview pop-up says for a moment, a tone and the line picked. */
export function previewOf(tone: MessagesView["tone"], moment: MomentView, pick: number): Preview {
  if (tone === "off") {
    return { text: "Messages are off", sub: "Nothing pops up. The app stays quiet.", tone: null };
  }
  const lines = linesOf(moment);
  const picked = lines.length > 0 ? lines[((pick % lines.length) + lines.length) % lines.length] : undefined;
  const text = tone === "plain" || !picked ? moment.plain : picked.line.preview;
  return { text, sub: moment.sub, tone: moment.tone };
}

/** The moment to show: the one that was selected if it still exists, else the first. */
export function selected(view: MessagesView, key: string | null): MomentView | null {
  return view.moments.find((moment) => moment.key === key) ?? view.moments[0] ?? null;
}

const TONES: { id: MessagesView["tone"]; label: string }[] = [
  { id: "friendly", label: "Friendly" },
  { id: "plain", label: "Plain" },
  { id: "off", label: "Off" },
];

const RULES: { key: keyof MessagesView["rules"]; label: string }[] = [
  { key: "quiet_fullscreen", label: "Never interrupt a full-screen app" },
  { key: "terminal", label: "Say the same things in the terminal" },
  { key: "milestones", label: "Celebrate milestones" },
];

// ----------------------------------------------------------------- screen

export function messagesScreen(ctx: Ctx, _route: Extract<Route, { screen: "messages" }>): Screen {
  let disposed = false;
  let view: MessagesView | null = null;
  let key: string | null = null;
  let pick = 0;
  // What is being typed and what went wrong with it survive a redraw.
  let draft = "";
  let addError: string | null = null;

  const tones = h("div", { class: "messages-tones", role: "group", "aria-label": "How chatty" });
  const moments = h("nav", { class: "messages-moments", "aria-label": "Moments" });
  const editor = h("div", { class: "messages-editor" });
  const preview = h("aside", { class: "messages-preview", "aria-label": "Preview" });
  const columns = h("div", { class: "messages-columns" });

  const el = h(
    "div",
    { class: "screen" },
    h(
      "header",
      { class: "screen-head", "data-tauri-drag-region": true },
      h("button", { type: "button", class: "btn sm", text: "‹ Settings", onclick: () => ctx.go({ screen: "settings" }) }),
      h("h1", { class: "h-screen", text: "Messages" }),
      h("span", { class: "muted", text: "Little notes Atelier leaves when something happens. Keep ours, write your own, or turn them off." }),
      tones,
    ),
    columns,
  );

  /** Takes a fresh view from a command and redraws, keeping the moment. */
  function apply(next: MessagesView): void {
    if (disposed) return;
    view = next;
    key = selected(next, key)?.key ?? null;
    drawTones();
    drawMoments();
    drawEditor();
    drawPreview();
  }

  async function load(): Promise<void> {
    mount(columns, h("div", { class: "loading", text: "Reading your messages…" }));
    try {
      const first = await api.messagesView();
      if (disposed) return;
      mount(columns, moments, editor, preview);
      apply(first);
    } catch (e) {
      if (disposed) return;
      mount(
        columns,
        h(
          "div",
          { class: "loading col messages-problem" },
          h("span", { text: asError(e).message || "Couldn't read your messages." }),
          h("div", null, h("button", { type: "button", class: "btn", text: "Try again", onclick: () => void load() })),
        ),
      );
    }
  }

  function drawTones(): void {
    const tone = view?.tone;
    mount(
      tones,
      TONES.map(({ id, label }) => {
        const button = h("button", {
          type: "button",
          class: "messages-tone",
          "aria-pressed": id === tone ? "true" : "false",
          text: label,
          onclick: () => {
            if (id === view?.tone) return;
            void ctx.busy(button, async () => apply(await api.messagesSetTone(id)));
          },
        });
        return button;
      }),
    );
  }

  function drawMoments(): void {
    if (!view) return;
    mount(
      moments,
      h("span", { class: "eyebrow messages-when", text: "When" }),
      view.moments.map((moment) =>
        h(
          "button",
          {
            type: "button",
            class: "messages-moment",
            "aria-pressed": moment.key === key ? "true" : "false",
            onclick: () => {
              if (moment.key === key) return;
              key = moment.key;
              pick = 0;
              draft = "";
              addError = null;
              drawMoments();
              drawEditor();
              drawPreview();
            },
          },
          h("span", { class: `dot sm tone-${moment.tone}`, "aria-hidden": "true" }),
          h("span", { class: "grow clip", text: moment.label }),
          h("span", { class: "messages-count", text: moment.ours.length + moment.yours.length }),
        ),
      ),
    );
  }

  function drawEditor(): void {
    const moment = view ? selected(view, key) : null;
    if (!view || !moment) {
      mount(editor, h("span", { class: "muted", text: "There are no moments to write for yet." }));
      return;
    }
    const lines = linesOf(moment);

    const rows = lines.map(({ line, mine, own }, index) => {
      const remove = mine
        ? h("button", {
            type: "button",
            class: "btn sm",
            text: "Remove",
            "aria-label": `Remove “${line.text}”`,
            onclick: () =>
              void ctx.busy(remove, async () => {
                const next = await api.messagesRemove(moment.key, own);
                // Stay on the same line where there still is one.
                if (pick >= index && pick > 0) pick -= 1;
                apply(next);
              }),
          })
        : null;
      return h(
        "div",
        { class: "messages-line" },
        h("span", { class: "messages-line-text grow selectable", text: line.text }),
        mine ? h("span", { class: "messages-yours", text: "Yours" }) : null,
        remove,
        h("button", {
          type: "button",
          class: "btn sm",
          text: "Try it",
          onclick: () => {
            pick = index;
            drawPreview();
          },
        }),
      );
    });

    const input = h("input", {
      id: "messages-own",
      class: "input messages-own",
      type: "text",
      placeholder: "Shipped. Go outside.",
      maxlength: "140",
      autocomplete: "off",
      value: draft,
      "aria-describedby": "messages-own-problem",
      oninput: () => {
        draft = input.value;
      },
    });
    const add = h("button", { type: "submit", class: "btn primary messages-add", text: "Add" });
    const problem = h("span", { id: "messages-own-problem", class: ["messages-add-problem", !addError && "hidden"], role: "alert", text: addError ?? "" });
    const code = (text: string) => h("code", { class: "mono", text });

    const submit = async () => {
      const text = input.value.trim();
      if (!text || add.disabled) return;
      add.disabled = true;
      try {
        const next = await api.messagesAdd(moment.key, text);
        draft = "";
        addError = null;
        // Show the new line straight away: it is the last one.
        pick = lines.length;
        apply(next);
      } catch (e) {
        if (disposed) return;
        // Under the field: it is about what was just typed.
        const err = asError(e);
        addError = err.hint ? `${err.message} ${err.hint}` : err.message || "That one couldn't be added.";
        problem.textContent = addError;
        problem.classList.remove("hidden");
        add.disabled = false;
        input.focus();
      }
    };

    mount(
      editor,
      h(
        "div",
        { class: "col messages-heading" },
        h("h2", { class: "messages-title", text: moment.label }),
        h("span", { class: "muted", text: "Atelier picks one of these at random each time." }),
      ),
      rows.length > 0
        ? h("div", { class: "messages-lines" }, rows)
        : h("div", { class: "empty", text: "No lines for this moment yet. Write the first one." }),
      h(
        "form",
        {
          class: "messages-form",
          onsubmit: (e) => {
            e.preventDefault();
            void submit();
          },
        },
        h("label", { for: "messages-own", class: "label", text: "Write your own" }),
        h("div", { class: "messages-form-row" }, input, add),
        problem,
        h("span", { class: "muted small" }, "You can use ", code("{repo}"), ", ", code("{branch}"), ", ", code("{count}"), " and ", code("{device}"), "."),
      ),
      h(
        "div",
        { class: "messages-rules" },
        RULES.map(({ key: rule, label }) => {
          const row = checkbox({
            checked: view?.rules[rule] ?? false,
            label,
            onchange: (next) => {
              void api.messagesSetRule(rule, next).then(
                (fresh) => {
                  // Only the ticks changed; leave the form and its focus alone.
                  if (!disposed) view = fresh;
                },
                (e) => {
                  ctx.fail(e);
                  const box = row.querySelector("input");
                  if (box) box.checked = !next;
                },
              );
            },
          });
          return row;
        }),
      ),
    );
  }

  function drawPreview(): void {
    const moment = view ? selected(view, key) : null;
    if (!view || !moment) {
      mount(preview);
      return;
    }
    const shown = previewOf(view.tone, moment, pick);
    mount(
      preview,
      h("span", { class: "muted", text: "How it pops up, bottom right of your screen" }),
      // The same markup as the real pop-ups, but it leads nowhere.
      h(
        "div",
        { class: ["toast messages-toast", shown.tone ? `tone-${shown.tone}` : "off"], role: "status" },
        h("span", { class: "badge", "aria-hidden": "true" }, mark(20, { boxed: false, stroke: "var(--tone)", width: 2.4 })),
        h(
          "span",
          { class: "words" },
          h("span", { class: "text", text: shown.text }),
          shown.sub ? h("span", { class: "sub", text: shown.sub }) : null,
        ),
      ),
      h("button", {
        type: "button",
        class: "btn messages-another",
        text: "Show another",
        onclick: () => {
          pick += 1;
          drawPreview();
        },
      }),
    );
  }

  drawTones();
  void load();

  return {
    el,
    dispose() {
      disposed = true;
    },
  };
}
