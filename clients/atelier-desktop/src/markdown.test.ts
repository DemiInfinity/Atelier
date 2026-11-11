import { afterEach, describe, expect, it, vi } from "vitest";
import { api, type MdBlock, type MdInline } from "./api";
import { composeDialog } from "./dialogs";
import { markdown, writePreview } from "./markdown";

const text = (t: string): MdInline => ({ t: "text", text: t });
const p = (...inlines: MdInline[]): MdBlock => ({ t: "paragraph", inlines });

const draw = (blocks: MdBlock[], openRef?: (n: number) => void) => {
  const opened: string[] = [];
  const el = markdown(blocks, { openLink: (url) => opened.push(url), openRef });
  return { el, opened };
};

/** Every element the builder may ever make. */
const ALLOWED = new Set([
  "DIV", "P", "H3", "H4", "H5", "H6", "EM", "STRONG", "S", "CODE", "PRE", "SPAN", "A", "BR", "BLOCKQUOTE",
  "UL", "OL", "LI", "INPUT", "TABLE", "THEAD", "TBODY", "TR", "TH", "TD", "HR",
]);

function audit(el: HTMLElement): void {
  for (const node of [el, ...el.querySelectorAll<HTMLElement>("*")]) {
    expect(ALLOWED.has(node.tagName), node.tagName).toBe(true);
    for (const attr of node.getAttributeNames()) {
      expect(attr.startsWith("on"), `${node.tagName} ${attr}`).toBe(false);
      expect(["href", "src", "srcset", "style", "action", "formaction", "srcdoc"].includes(attr), `${node.tagName} ${attr}`).toBe(false);
    }
  }
}

describe("markdown", () => {
  it("draws the everyday things", () => {
    const { el } = draw([
      { t: "heading", level: 1, inlines: [text("Title")] },
      p(text("Some "), { t: "em", inlines: [text("soft")] }, text(" "), { t: "strong", inlines: [text("strong")] }, text(" "), { t: "strike", inlines: [text("gone")] }, { t: "break" }, { t: "code", text: "x < 1" }),
      { t: "quote", blocks: [p(text("quoted"))] },
      { t: "rule" },
      { t: "heading", level: 6, inlines: [text("small")] },
      { t: "heading", level: 99, inlines: [text("odd")] },
    ]);
    expect(el.className).toBe("md selectable");
    // The screen's title is the h2: what people write starts at h3.
    expect(el.querySelector("h3.md-h1")?.textContent).toBe("Title");
    expect(el.querySelector(".md-h6")?.tagName).toBe("H6");
    expect(el.querySelectorAll("h1, h2").length).toBe(0);
    expect(el.querySelector("p em")?.textContent).toBe("soft");
    expect(el.querySelector("p strong")?.textContent).toBe("strong");
    expect(el.querySelector("p s")?.textContent).toBe("gone");
    expect(el.querySelector("p br")).not.toBeNull();
    expect(el.querySelector("p code.md-code-inline")?.textContent).toBe("x < 1");
    expect(el.querySelector("blockquote p")?.textContent).toBe("quoted");
    expect(el.querySelector("hr")).not.toBeNull();
    audit(el);
  });

  it("draws code blocks in monospace with their language", () => {
    const { el } = draw([
      { t: "code", lang: "rust", text: "fn main() {\n  <b>not markup</b>\n}" },
      { t: "code", lang: "", text: "plain" },
    ]);
    const first = el.querySelectorAll(".md-code")[0] as HTMLElement;
    expect(first.querySelector(".md-code-lang")?.textContent).toBe("rust");
    expect(first.querySelector("pre.mono code")?.textContent).toBe("fn main() {\n  <b>not markup</b>\n}");
    expect(first.querySelector("b")).toBeNull();
    expect(el.querySelectorAll(".md-code")[1]?.querySelector(".md-code-lang")).toBeNull();
    audit(el);
  });

  it("draws lists, read-only task boxes and tables", () => {
    const { el } = draw([
      { t: "list", ordered: true, start: 3, items: [{ task: null, blocks: [p(text("three"))] }, { task: null, blocks: [p(text("four")), { t: "list", ordered: false, start: 1, items: [{ task: null, blocks: [p(text("inner"))] }] }] }] },
      { t: "list", ordered: false, start: 1, items: [{ task: true, blocks: [p(text("done"))] }, { task: false, blocks: [p(text("todo"))] }] },
      { t: "table", aligns: ["left", "right", "sideways"], head: [[text("a")], [text("b")], [text("c")]], rows: [[[text("1")], [{ t: "strong", inlines: [text("2")] }], [text("3")]]] },
    ]);
    const ol = el.querySelector("ol") as HTMLOListElement;
    expect(ol.getAttribute("start")).toBe("3");
    expect([...ol.querySelectorAll(":scope > li")].length).toBe(2);
    expect(ol.querySelector("li ul li")?.textContent).toBe("inner");
    const boxes = [...el.querySelectorAll<HTMLInputElement>("ul.md-tasks input[type=checkbox]")];
    expect(boxes.map((b) => [b.checked, b.disabled, b.getAttribute("aria-label")])).toEqual([
      [true, true, "Done"],
      [false, true, "Not done"],
    ]);
    expect([...el.querySelectorAll("th")].map((th) => [th.textContent, th.className])).toEqual([
      ["a", "md-align-left"],
      ["b", "md-align-right"],
      ["c", ""],
    ]);
    expect(el.querySelector("td strong")?.textContent).toBe("2");
    expect(el.querySelector(".md-table table")).not.toBeNull();
    audit(el);
  });

  it("hands links to the opener and never gives the window something to follow", () => {
    const refs: number[] = [];
    const { el, opened } = draw(
      [
        p(
          { t: "link", url: "https://example.com/docs", image: false, inlines: [text("docs")] },
          text(" "),
          { t: "link", url: "https://example.com/cat.png", image: true, inlines: [text("a cat")] },
          text(" "),
          { t: "ref", number: 12 },
          text(" "),
          { t: "mention", name: "mara" },
        ),
      ],
      (n) => refs.push(n),
    );
    const [docs, cat, ref] = [...el.querySelectorAll<HTMLElement>("a")];
    expect(docs?.textContent).toBe("docs");
    expect(docs?.title).toBe("https://example.com/docs");
    expect(docs?.getAttribute("role")).toBe("link");
    expect(docs?.hasAttribute("href")).toBe(false);
    docs?.click();
    docs?.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter" }));
    // An image is its alt text, linking to the picture; nothing is loaded.
    expect(cat?.className).toBe("md-link md-image");
    expect(cat?.textContent).toBe("a cat");
    expect(el.querySelector("img")).toBeNull();
    cat?.click();
    expect(opened).toEqual(["https://example.com/docs", "https://example.com/docs", "https://example.com/cat.png"]);
    expect(ref?.textContent).toBe("#12");
    ref?.click();
    expect(refs).toEqual([12]);
    expect(el.querySelector(".md-mention")?.textContent).toBe("@mara");
    expect(el.querySelector(".md-mention")?.tagName).toBe("SPAN");
    audit(el);
    // Without somewhere to go, a reference is plain styled text.
    const plain = draw([p({ t: "ref", number: 7 })]).el;
    expect(plain.querySelector("a")).toBeNull();
    expect(plain.querySelector("span.md-ref")?.textContent).toBe("#7");
  });

  it("never turns content into markup, whatever the tree says", () => {
    const nasty = '<script>alert(1)</script><img src=x onerror=alert(1)><a href="javascript:alert(1)">x</a>';
    const { el, opened } = draw([
      p(text(nasty), { t: "code", text: nasty }, { t: "em", inlines: [text(nasty)] }),
      { t: "heading", level: 2, inlines: [text(nasty)] },
      { t: "code", lang: nasty, text: nasty },
      { t: "quote", blocks: [p(text(nasty))] },
      { t: "list", ordered: false, start: 1, items: [{ task: false, blocks: [p(text(nasty))] }] },
      { t: "table", aligns: [nasty], head: [[text(nasty)]], rows: [[[text(nasty)]]] },
      p({ t: "link", url: "javascript:alert(1)", image: false, inlines: [text(nasty)] }, { t: "mention", name: nasty }),
      // Kinds this version doesn't know draw nothing at all.
      { t: "html", html: nasty } as unknown as MdBlock,
      p({ t: "html", html: nasty } as unknown as MdInline, { t: "image", url: "https://example.com/x.png" } as unknown as MdInline),
    ]);
    document.body.appendChild(el);
    expect(el.querySelectorAll("script, img, iframe, object, embed, style, link, form, svg").length).toBe(0);
    expect(el.querySelectorAll("[href], [src]").length).toBe(0);
    expect(el.querySelector("p")?.textContent).toContain("<script>alert(1)</script>");
    expect(el.querySelector(".md-code-lang")?.textContent).toBe(nasty);
    expect(el.querySelector("th")?.className).toBe("");
    audit(el);
    // Even a bad address only ever reaches the opener, which is Rust's to refuse.
    el.querySelector<HTMLElement>("a")?.click();
    expect(opened).toEqual(["javascript:alert(1)"]);
    el.remove();
  });
});

describe("Write / Preview", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    document.body.replaceChildren();
  });

  const settle = async () => {
    for (let i = 0; i < 10; i++) await Promise.resolve();
  };
  const tab = (el: HTMLElement, label: string) => [...el.querySelectorAll<HTMLButtonElement>(".md-tab")].find((b) => b.textContent === label) as HTMLButtonElement;

  it("shows what was typed through the same renderer, and goes back", async () => {
    const preview = vi.spyOn(api, "markdownPreview").mockResolvedValue([p({ t: "strong", inlines: [text("bold")] }), p(text("<script>x</script>"))]);
    const box = document.createElement("textarea");
    const modes: boolean[] = [];
    const el = writePreview(box, { openLink: () => {} }, { onMode: (on) => modes.push(on) });
    expect(tab(el, "Write").getAttribute("aria-selected")).toBe("true");
    expect(el.querySelector(".md-preview")?.classList.contains("hidden")).toBe(true);

    box.value = "**bold**\n\n<script>x</script>";
    tab(el, "Preview").click();
    await settle();
    expect(preview).toHaveBeenCalledExactlyOnceWith("**bold**\n\n<script>x</script>");
    expect(box.classList.contains("hidden")).toBe(true);
    const pane = el.querySelector(".md-preview") as HTMLElement;
    expect(pane.classList.contains("hidden")).toBe(false);
    expect(pane.querySelector(".md strong")?.textContent).toBe("bold");
    expect(pane.querySelector("script")).toBeNull();
    expect(tab(el, "Preview").getAttribute("aria-selected")).toBe("true");

    tab(el, "Write").click();
    expect(box.classList.contains("hidden")).toBe(false);
    expect(pane.classList.contains("hidden")).toBe(true);
    expect(box.value).toBe("**bold**\n\n<script>x</script>");
    expect(modes).toEqual([false, true, false]);
  });

  it("says so when there is nothing to preview or it can't be made", async () => {
    const preview = vi.spyOn(api, "markdownPreview").mockRejectedValue({ kind: "invalid", message: "That text is too long to preview.", hint: null });
    const box = document.createElement("textarea");
    const el = writePreview(box, { openLink: () => {} }, { previewing: true });
    expect(el.querySelector(".md-preview")?.textContent).toBe("Nothing to preview.");
    expect(preview).not.toHaveBeenCalled();
    box.value = "x";
    tab(el, "Preview").click();
    await settle();
    expect(el.querySelector(".md-preview")?.textContent).toBe("That text is too long to preview.");
  });

  it("is on the new issue box too, and what is sent is what was typed", async () => {
    vi.spyOn(api, "markdownPreview").mockResolvedValue([p(text("hello"))]);
    const made = composeDialog({ title: "New issue", titleLabel: "Title", bodyLabel: "About", confirm: "Open issue" });
    const dialog = document.querySelector("form.dialog") as HTMLFormElement;
    const title = dialog.querySelector<HTMLInputElement>("#compose-title") as HTMLInputElement;
    const body = dialog.querySelector<HTMLTextAreaElement>("#compose-body") as HTMLTextAreaElement;
    title.value = "A thing";
    title.dispatchEvent(new Event("input"));
    body.value = "hello";
    tab(dialog, "Preview").click();
    await settle();
    expect(dialog.querySelector(".md-preview .md p")?.textContent).toBe("hello");
    // Looking at the preview doesn't submit anything.
    expect(document.querySelector("form.dialog")).not.toBeNull();
    dialog.dispatchEvent(new Event("submit", { cancelable: true }));
    expect(await made).toEqual({ title: "A thing", body: "hello" });
  });
});
