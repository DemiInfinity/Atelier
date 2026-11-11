// A very small DOM toolkit. Everything from the studio and from git is
// untrusted, so nodes are built with textContent and attributes only:
// nothing in this app ever assigns innerHTML.

export type Child = Node | string | number | null | undefined | false | Child[];

type Handler = (event: Event) => void;

export interface Props {
  class?: string | (string | false | null | undefined)[];
  /** Sets textContent (instead of children). */
  text?: string | number;
  /** Inline styles through the CSSOM (allowed by the CSP, unlike style=""). */
  style?: Partial<Record<string, string>>;
  dataset?: Record<string, string>;
  /** Event handlers: onclick, oninput, onchange, onsubmit, onkeydown, … */
  [key: `on${string}`]: Handler | undefined;
  /** Anything else becomes an attribute (false/null/undefined leaves it out, true sets it empty). */
  [key: string]: unknown;
}

const PROPERTIES = new Set(["value", "checked", "disabled", "selected", "indeterminate"]);

function append(parent: Node, child: Child): void {
  if (child === null || child === undefined || child === false) return;
  if (Array.isArray(child)) {
    for (const c of child) append(parent, c);
  } else if (child instanceof Node) {
    parent.appendChild(child);
  } else {
    parent.appendChild(document.createTextNode(String(child)));
  }
}

function apply(el: Element, props: Props): void {
  for (const [key, value] of Object.entries(props)) {
    if (value === undefined || value === null || value === false) {
      if (PROPERTIES.has(key) && value === false) (el as unknown as Record<string, unknown>)[key] = false;
      continue;
    }
    if (key === "class") {
      const names = Array.isArray(value) ? value.filter(Boolean).join(" ") : String(value);
      if (names) el.setAttribute("class", names);
    } else if (key === "text") {
      el.textContent = String(value);
    } else if (key === "style") {
      const style = (el as HTMLElement).style;
      for (const [name, v] of Object.entries(value as Record<string, string>)) {
        if (name.startsWith("--")) style.setProperty(name, v);
        else (style as unknown as Record<string, string>)[name] = v;
      }
    } else if (key === "dataset") {
      Object.assign((el as HTMLElement).dataset, value);
    } else if (key.startsWith("on") && typeof value === "function") {
      el.addEventListener(key.slice(2).toLowerCase(), value as Handler);
    } else if (PROPERTIES.has(key)) {
      (el as unknown as Record<string, unknown>)[key] = value;
    } else {
      el.setAttribute(key, value === true ? "" : String(value));
    }
  }
}

/** Builds an element. `h("button", { class: "btn", onclick }, "Push")`. */
export function h<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  props?: Props | null,
  ...children: Child[]
): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  if (props) apply(el, props);
  append(el, children);
  return el;
}

const SVG_NS = "http://www.w3.org/2000/svg";

/** Builds an SVG element (attributes only). */
export function s(tag: string, attrs: Record<string, string | number> = {}, ...children: Node[]): SVGElement {
  const el = document.createElementNS(SVG_NS, tag);
  for (const [key, value] of Object.entries(attrs)) el.setAttribute(key, String(value));
  for (const child of children) el.appendChild(child);
  return el;
}

/** Empties an element and puts `children` in it. */
export function mount(el: Element, ...children: Child[]): void {
  el.replaceChildren();
  append(el, children);
}

// ------------------------------------------------------------------ icons

const ICONS = {
  today: "M4 6 H20 V20 H4 Z M4 10 H20 M8 3 V7 M16 3 V7",
  pulls:
    "M7 5 V19 M7 5 a2 2 0 1 0 0.01 0 M7 19 a2 2 0 1 0 0.01 0 M17 19 a2 2 0 1 0 0.01 0 M17 17 V11 a4 4 0 0 0-4-4 H11",
  plus: "M12 5 V19 M5 12 H19",
  branch: "M7 4 V20 M17 4 V8 a6 6 0 0 1-6 6 H7",
  chevron: "M6 9 L12 15 L18 9",
  close: "M6 6 L18 18 M18 6 L6 18",
  check: "M5 12 L10 17 L19 7",
  more: "M5 12 h0.01 M12 12 h0.01 M19 12 h0.01",
  pin: "M9 4 H15 L14 10 L17 14 H7 L10 10 Z M12 14 V20",
} as const;

export type IconName = keyof typeof ICONS;

/** A line icon from the fixed set, drawn in the current colour. */
export function icon(name: IconName, size = 16, strokeWidth = 2): SVGElement {
  return s(
    "svg",
    {
      width: size,
      height: size,
      viewBox: "0 0 24 24",
      fill: "none",
      stroke: "currentColor",
      "stroke-width": strokeWidth,
      "stroke-linecap": "round",
      "stroke-linejoin": "round",
      "aria-hidden": "true",
    },
    s("path", { d: ICONS[name] }),
  );
}

/** The settings icon (two sliders). */
export function settingsIcon(size = 16): SVGElement {
  return s(
    "svg",
    {
      width: size,
      height: size,
      viewBox: "0 0 24 24",
      fill: "none",
      stroke: "currentColor",
      "stroke-width": 2,
      "stroke-linecap": "round",
      "aria-hidden": "true",
    },
    s("path", { d: "M4 7 H14 M18 7 H20 M4 17 H6 M10 17 H20" }),
    s("circle", { cx: 16, cy: 7, r: 2 }),
    s("circle", { cx: 8, cy: 17, r: 2 }),
  );
}

/** The Atelier mark: an A in a rounded square. `boxed: false` draws the A alone. */
export function mark(size = 28, opts: { boxed?: boolean; stroke?: string; width?: number } = {}): SVGElement {
  const stroke = opts.stroke ?? "var(--amber)";
  const width = opts.width ?? 2;
  const parts: Node[] = [];
  if (opts.boxed !== false) {
    parts.push(s("rect", { x: 2, y: 2, width: 24, height: 24, rx: 7, stroke, "stroke-width": width }));
  }
  parts.push(
    s("path", {
      d: "M9 19 L14 8 L19 19 M11 15 H17",
      stroke,
      "stroke-width": width,
      "stroke-linecap": "round",
      "stroke-linejoin": "round",
    }),
  );
  return s("svg", { width: size, height: size, viewBox: "0 0 28 28", fill: "none", "aria-hidden": "true" }, ...parts);
}

// ------------------------------------------------------------- components

/** A switch (`role="switch"`). `kind: "red"` for remote access. */
export function toggle(opts: {
  on: boolean;
  label: string;
  kind?: "amber" | "red";
  size?: "md" | "sm";
  disabled?: boolean;
  onchange: (next: boolean) => void;
}): HTMLButtonElement {
  return h(
    "button",
    {
      type: "button",
      role: "switch",
      class: ["switch", opts.kind === "red" && "red", opts.size === "sm" && "sm"],
      "aria-checked": opts.on ? "true" : "false",
      "aria-label": opts.label,
      disabled: opts.disabled ?? false,
      onclick: () => opts.onchange(!opts.on),
    },
    h("span", { class: "knob" }),
  );
}

/** A small rounded label in a colour thread. */
export function pill(text: string, tone: string, extra = ""): HTMLSpanElement {
  return h("span", { class: `pill tone-${tone} ${extra}`.trim(), text });
}

/** A checkbox with its label. */
export function checkbox(opts: {
  checked: boolean;
  label: Child;
  blue?: boolean;
  disabled?: boolean;
  onchange: (next: boolean) => void;
}): HTMLLabelElement {
  const input = h("input", {
    type: "checkbox",
    class: ["check", opts.blue && "blue"],
    checked: opts.checked,
    disabled: opts.disabled ?? false,
    onchange: (e) => opts.onchange((e.target as HTMLInputElement).checked),
  });
  return h("label", { class: "check-row" }, input, h("span", null, opts.label));
}

// ------------------------------------------------------------------ words

/** `10 min ago`, `2 hours ago`, `yesterday`, `3 days ago`, then a date. */
export function ago(iso: string, now: Date = new Date()): string {
  const then = new Date(iso);
  if (Number.isNaN(then.getTime())) return "";
  const seconds = Math.max(0, Math.round((now.getTime() - then.getTime()) / 1000));
  if (seconds < 45) return "just now";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24 && then.getDate() === now.getDate()) return hours === 1 ? "1 hour ago" : `${hours} hours ago`;
  const startOfToday = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  const days = Math.ceil((startOfToday - then.getTime()) / 86_400_000);
  if (days <= 1) return "yesterday";
  if (days < 7) return `${days} days ago`;
  return then.toLocaleDateString(undefined, { day: "numeric", month: "short", year: then.getFullYear() === now.getFullYear() ? undefined : "numeric" });
}

/** The short column form: `10 min`, `2 hours`, `today`, `yesterday`, `2 days`. */
export function agoShort(iso: string, now: Date = new Date()): string {
  const text = ago(iso, now);
  if (text === "just now") return "now";
  if (/hours? ago$/.test(text)) {
    const hours = Number.parseInt(text, 10);
    return hours > 3 ? "today" : text.replace(" ago", "");
  }
  return text.replace(" ago", "");
}

/** `09:14` in local time. */
export function clock(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return "";
  return at.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hour12: false });
}

/** `today 09:12`, `yesterday 18:40`, `3 Nov 09:12`. */
export function when(iso: string, now: Date = new Date()): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return "";
  const startOfToday = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  const days = Math.ceil((startOfToday - at.getTime()) / 86_400_000);
  const day = days <= 0 ? "today" : days === 1 ? "yesterday" : at.toLocaleDateString(undefined, { day: "numeric", month: "short" });
  return `${day} ${clock(iso)}`;
}

/** `Wednesday 11 November`. */
export function dayLine(now: Date = new Date()): string {
  return now.toLocaleDateString(undefined, { weekday: "long", day: "numeric", month: "long" });
}

/** `48 seconds`, `3 minutes`. */
export function elapsed(iso: string, now: Date = new Date()): string {
  const then = new Date(iso);
  if (Number.isNaN(then.getTime())) return "";
  const seconds = Math.max(0, Math.round((now.getTime() - then.getTime()) / 1000));
  if (seconds < 60) return seconds === 1 ? "1 second" : `${seconds} seconds`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return minutes === 1 ? "1 minute" : `${minutes} minutes`;
  const hours = Math.floor(minutes / 60);
  return hours === 1 ? "1 hour" : `${hours} hours`;
}

export function plural(n: number, one: string, many: string): string {
  return n === 1 ? `1 ${one}` : `${n} ${many}`;
}
