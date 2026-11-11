//! Markdown as a small typed tree for the window to draw.
//!
//! What people and bots write in issues and comments is untrusted. It is
//! parsed here (CommonMark with tables, strikethrough and task lists) into
//! the node kinds below and nothing else: there is no "raw HTML" node, so
//! HTML in the source can only ever come out as text. The window builds
//! elements for these kinds with `createElement` and `textContent`; it has
//! no way to make any other element, attribute or handler from content.
//!
//! Links are kept only when they are `http`, `https` or `mailto` addresses
//! without credentials; anything else (`javascript:`, `data:`, `file:`,
//! relative paths) is drawn as its text. Images are never loaded: an image
//! becomes a link to its address with the alt text as its words.

use pulldown_cmark::{Alignment, CodeBlockKind, Event, LinkType, Options, Parser, Tag, TagEnd};
use serde::Serialize;

/// Deeper nesting than this is shown as plain text instead of being walked.
const MAX_DEPTH: usize = 40;

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(tag = "t", rename_all = "snake_case")]
pub enum Block {
    Heading {
        level: u8,
        inlines: Vec<Inline>,
    },
    Paragraph {
        inlines: Vec<Inline>,
    },
    /// A fenced or indented code block; `lang` is the fence's first word.
    Code {
        lang: String,
        text: String,
    },
    Quote {
        blocks: Vec<Block>,
    },
    List {
        ordered: bool,
        start: u64,
        items: Vec<Item>,
    },
    Table {
        /// `left`, `center`, `right` or empty, per column.
        aligns: Vec<&'static str>,
        head: Vec<Vec<Inline>>,
        rows: Vec<Vec<Vec<Inline>>>,
    },
    Rule,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct Item {
    /// `Some(done)` for a task list item.
    pub task: Option<bool>,
    pub blocks: Vec<Block>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(tag = "t", rename_all = "snake_case")]
pub enum Inline {
    Text {
        text: String,
    },
    Code {
        text: String,
    },
    Em {
        inlines: Vec<Inline>,
    },
    Strong {
        inlines: Vec<Inline>,
    },
    Strike {
        inlines: Vec<Inline>,
    },
    /// `url` is always an `http`, `https` or `mailto` address. `image` marks
    /// a link that stands in for an image (which is never loaded).
    Link {
        url: String,
        image: bool,
        inlines: Vec<Inline>,
    },
    /// `#123`: an issue or pull request of the same repository.
    Ref {
        number: u64,
    },
    /// `@name`, as styled text only.
    Mention {
        name: String,
    },
    Break,
}

fn text(text: impl Into<String>) -> Inline {
    Inline::Text { text: text.into() }
}

/// The address as it may be opened: `http`/`https` with a host and no
/// credentials, or `mailto`. `None` for everything else.
pub fn safe_url(raw: &str) -> Option<String> {
    let raw = raw.trim();
    if raw.is_empty() || raw.chars().any(|c| c.is_control() || c.is_whitespace()) {
        return None;
    }
    let parsed = url::Url::parse(raw).ok()?;
    match parsed.scheme() {
        "http" | "https" => {
            let host = parsed.host_str().unwrap_or_default();
            (!host.is_empty() && parsed.username().is_empty() && parsed.password().is_none())
                .then(|| parsed.to_string())
        }
        "mailto" => (parsed.path().contains('@')).then(|| parsed.to_string()),
        _ => None,
    }
}

/// Parses Markdown into the tree. Never fails: what can't be understood is
/// text.
pub fn render(source: &str) -> Vec<Block> {
    let options =
        Options::ENABLE_TABLES | Options::ENABLE_STRIKETHROUGH | Options::ENABLE_TASKLISTS;
    let events: Vec<Event<'_>> = Parser::new_ext(source, options).collect();
    // Absurd nesting (thousands of `>`) is shown as written, not walked.
    let (mut depth, mut deepest) = (0usize, 0usize);
    for event in &events {
        match event {
            Event::Start(_) => {
                depth += 1;
                deepest = deepest.max(depth);
            }
            Event::End(_) => depth = depth.saturating_sub(1),
            _ => {}
        }
    }
    if deepest > MAX_DEPTH {
        return vec![Block::Paragraph {
            inlines: vec![text(source)],
        }];
    }
    let mut walk = Walk {
        events,
        at: 0,
        task: None,
    };
    walk.container(None)
}

struct Walk<'a> {
    events: Vec<Event<'a>>,
    at: usize,
    /// A task marker met inside an item's paragraph, for the item to take.
    task: Option<bool>,
}

fn is_inline_start(tag: &Tag<'_>) -> bool {
    matches!(
        tag,
        Tag::Emphasis
            | Tag::Strong
            | Tag::Strikethrough
            | Tag::Superscript
            | Tag::Subscript
            | Tag::Link { .. }
            | Tag::Image { .. }
    )
}

impl<'a> Walk<'a> {
    fn next(&mut self) -> Option<Event<'a>> {
        let event = self.events.get(self.at).cloned();
        self.at += 1;
        event
    }

    fn peek(&self) -> Option<&Event<'a>> {
        self.events.get(self.at)
    }

    /// Blocks up to the end of the enclosing tag (or of the document).
    /// Inline content met directly (a tight list item's words) becomes a
    /// paragraph.
    fn container(&mut self, until: Option<TagEnd>) -> Vec<Block> {
        let mut blocks = Vec::new();
        while let Some(event) = self.peek().cloned() {
            match event {
                Event::End(end) => {
                    self.at += 1;
                    if Some(end) == until {
                        break;
                    }
                }
                Event::Rule => {
                    self.at += 1;
                    blocks.push(Block::Rule);
                }
                Event::TaskListMarker(done) => {
                    self.at += 1;
                    self.task = Some(done);
                }
                Event::Start(tag) if !is_inline_start(&tag) => {
                    self.at += 1;
                    self.block(tag, &mut blocks);
                }
                // Words, code, breaks, inline starts: a run of inlines.
                _ => {
                    let inlines = self.inlines(None, false);
                    if !inlines.is_empty() {
                        blocks.push(Block::Paragraph { inlines });
                    }
                }
            }
        }
        blocks
    }

    fn block(&mut self, tag: Tag<'a>, blocks: &mut Vec<Block>) {
        match tag {
            Tag::Paragraph => {
                let inlines = self.inlines(Some(TagEnd::Paragraph), false);
                if !inlines.is_empty() {
                    blocks.push(Block::Paragraph { inlines });
                }
            }
            Tag::Heading { level, .. } => {
                let inlines = self.inlines(Some(TagEnd::Heading(level)), false);
                blocks.push(Block::Heading {
                    level: level as u8,
                    inlines,
                });
            }
            Tag::BlockQuote(kind) => {
                let inner = self.container(Some(TagEnd::BlockQuote(kind)));
                blocks.push(Block::Quote { blocks: inner });
            }
            Tag::CodeBlock(kind) => {
                let lang = match &kind {
                    CodeBlockKind::Fenced(info) => info
                        .split(|c: char| c.is_whitespace() || c == ',')
                        .next()
                        .unwrap_or_default()
                        .chars()
                        .take(24)
                        .collect(),
                    CodeBlockKind::Indented => String::new(),
                };
                let mut code = self.raw_until(TagEnd::CodeBlock);
                if code.ends_with('\n') {
                    code.pop();
                }
                blocks.push(Block::Code { lang, text: code });
            }
            // Raw HTML is never interpreted: it is shown as it was written.
            Tag::HtmlBlock => {
                let raw = self.raw_until(TagEnd::HtmlBlock);
                let raw = raw.trim_end_matches('\n');
                if !raw.is_empty() {
                    blocks.push(Block::Paragraph {
                        inlines: vec![text(raw)],
                    });
                }
            }
            Tag::List(start) => {
                let ordered = start.is_some();
                let mut items = Vec::new();
                loop {
                    match self.next() {
                        Some(Event::Start(Tag::Item)) => {
                            self.task = None;
                            let inner = self.container(Some(TagEnd::Item));
                            items.push(Item {
                                task: self.task.take(),
                                blocks: inner,
                            });
                        }
                        Some(Event::End(TagEnd::List(_))) | None => break,
                        Some(_) => {}
                    }
                }
                blocks.push(Block::List {
                    ordered,
                    start: start.unwrap_or(1),
                    items,
                });
            }
            Tag::Table(aligns) => {
                let aligns = aligns
                    .iter()
                    .map(|a| match a {
                        Alignment::Left => "left",
                        Alignment::Center => "center",
                        Alignment::Right => "right",
                        Alignment::None => "",
                    })
                    .collect();
                let mut head = Vec::new();
                let mut rows = Vec::new();
                let mut row: Vec<Vec<Inline>> = Vec::new();
                loop {
                    match self.next() {
                        Some(Event::Start(Tag::TableCell)) => {
                            row.push(self.inlines(Some(TagEnd::TableCell), false));
                        }
                        Some(Event::End(TagEnd::TableHead)) => head = std::mem::take(&mut row),
                        Some(Event::End(TagEnd::TableRow)) => rows.push(std::mem::take(&mut row)),
                        Some(Event::End(TagEnd::Table)) | None => break,
                        Some(_) => {}
                    }
                }
                blocks.push(Block::Table { aligns, head, rows });
            }
            // Anything else that holds blocks (not switched on today): its
            // contents, without the wrapper.
            other => {
                let inner = self.container(Some(other.to_end()));
                blocks.extend(inner);
            }
        }
    }

    /// The text of everything up to `end`, as written.
    fn raw_until(&mut self, end: TagEnd) -> String {
        let mut raw = String::new();
        while let Some(event) = self.next() {
            match event {
                Event::End(e) if e == end => break,
                Event::Text(t) | Event::Html(t) | Event::InlineHtml(t) | Event::Code(t) => {
                    raw.push_str(&t);
                }
                Event::SoftBreak | Event::HardBreak => raw.push('\n'),
                _ => {}
            }
        }
        raw
    }

    /// Inlines up to `until`. With `None` (a run met directly in a
    /// container) it stops, without consuming, at the first block event.
    /// Inside a link nothing is turned into further links.
    fn inlines(&mut self, until: Option<TagEnd>, in_link: bool) -> Vec<Inline> {
        let mut out: Vec<Inline> = Vec::new();
        while let Some(event) = self.peek().cloned() {
            match &event {
                Event::End(end) => {
                    if until.is_none() {
                        break;
                    }
                    self.at += 1;
                    if Some(*end) == until {
                        break;
                    }
                    continue;
                }
                Event::Start(tag) if until.is_none() && !is_inline_start(tag) => break,
                Event::Rule if until.is_none() => break,
                _ => {}
            }
            self.at += 1;
            match event {
                Event::Text(t) => {
                    // The parser hands words over in pieces (around every
                    // `_` and `*`); addresses are found in the whole run.
                    let mut t = t.into_string();
                    while let Some(Event::Text(more)) = self.peek() {
                        t.push_str(more);
                        self.at += 1;
                    }
                    if in_link {
                        push_text(&mut out, &t);
                    } else {
                        linkify(&t, &mut out);
                    }
                }
                Event::Code(t) | Event::InlineMath(t) | Event::DisplayMath(t) => {
                    out.push(Inline::Code {
                        text: t.into_string(),
                    });
                }
                // Raw HTML in a line: its source, as text.
                Event::Html(t) | Event::InlineHtml(t) => push_text(&mut out, &t),
                Event::FootnoteReference(name) => push_text(&mut out, &format!("[^{name}]")),
                // As in a comment box: a new line is a new line.
                Event::SoftBreak | Event::HardBreak => out.push(Inline::Break),
                Event::TaskListMarker(done) => self.task = Some(done),
                Event::Rule => {}
                Event::Start(Tag::Emphasis) => {
                    let inlines = self.inlines(Some(TagEnd::Emphasis), in_link);
                    out.push(Inline::Em { inlines });
                }
                Event::Start(Tag::Strong) => {
                    let inlines = self.inlines(Some(TagEnd::Strong), in_link);
                    out.push(Inline::Strong { inlines });
                }
                Event::Start(Tag::Strikethrough) => {
                    let inlines = self.inlines(Some(TagEnd::Strikethrough), in_link);
                    out.push(Inline::Strike { inlines });
                }
                Event::Start(Tag::Link {
                    link_type,
                    dest_url,
                    ..
                }) => {
                    let inlines = self.inlines(Some(TagEnd::Link), true);
                    let dest = if link_type == LinkType::Email && !dest_url.starts_with("mailto:") {
                        format!("mailto:{dest_url}")
                    } else {
                        dest_url.to_string()
                    };
                    match safe_url(&dest) {
                        Some(url) if !in_link => out.push(Inline::Link {
                            url,
                            image: false,
                            inlines,
                        }),
                        // Not an address this app opens: just its words.
                        _ => out.extend(inlines),
                    }
                }
                Event::Start(Tag::Image { dest_url, .. }) => {
                    let alt = plain(&self.inlines(Some(TagEnd::Image), true));
                    let url = safe_url(&dest_url).filter(|u| !u.starts_with("mailto:"));
                    match url {
                        Some(url) if !in_link => {
                            let words = if alt.trim().is_empty() {
                                url.clone()
                            } else {
                                alt
                            };
                            out.push(Inline::Link {
                                url,
                                image: true,
                                inlines: vec![text(words)],
                            });
                        }
                        _ => push_text(&mut out, &alt),
                    }
                }
                // Superscript and the like: their words.
                Event::Start(tag) => {
                    let inner = self.inlines(Some(tag.to_end()), in_link);
                    out.extend(inner);
                }
                Event::End(_) => {}
            }
        }
        out
    }
}

/// The words of some inlines, without any structure.
fn plain(inlines: &[Inline]) -> String {
    let mut out = String::new();
    for inline in inlines {
        match inline {
            Inline::Text { text } | Inline::Code { text } => out.push_str(text),
            Inline::Em { inlines }
            | Inline::Strong { inlines }
            | Inline::Strike { inlines }
            | Inline::Link { inlines, .. } => out.push_str(&plain(inlines)),
            Inline::Ref { number } => out.push_str(&format!("#{number}")),
            Inline::Mention { name } => out.push_str(&format!("@{name}")),
            Inline::Break => out.push(' '),
        }
    }
    out
}

/// Adds text, joined to a text node right before it.
fn push_text(out: &mut Vec<Inline>, more: &str) {
    if more.is_empty() {
        return;
    }
    if let Some(Inline::Text { text }) = out.last_mut() {
        text.push_str(more);
    } else {
        out.push(text(more));
    }
}

fn is_word(c: char) -> bool {
    c.is_alphanumeric() || c == '_'
}

/// Plain words → text, with bare `https://…` addresses as links, `#123` as
/// references and `@name` as mentions.
fn linkify(source: &str, out: &mut Vec<Inline>) {
    let mut rest = source;
    let mut before: Option<char> = match out.last() {
        Some(Inline::Text { text }) => text.chars().last(),
        Some(Inline::Break) | None => None,
        Some(_) => Some('x'),
    };
    let mut pending = String::new();
    while let Some(c) = rest.chars().next() {
        let boundary = before.is_none_or(|b| !is_word(b));
        let mut taken = 0usize;
        if boundary && (rest.starts_with("https://") || rest.starts_with("http://")) {
            let end = rest
                .find(|c: char| c.is_whitespace() || c == '<' || c == '>' || c == '"')
                .unwrap_or(rest.len());
            let mut candidate = &rest[..end];
            // Sentence punctuation after an address isn't part of it.
            while let Some(last) = candidate.chars().last() {
                let closes_nothing = last == ')' && !candidate.contains('(');
                if matches!(
                    last,
                    '.' | ',' | ';' | ':' | '!' | '?' | '\'' | ']' | '*' | '_'
                ) || closes_nothing
                {
                    candidate = &candidate[..candidate.len() - last.len_utf8()];
                } else {
                    break;
                }
            }
            if let Some(url) = safe_url(candidate) {
                push_text(out, &std::mem::take(&mut pending));
                out.push(Inline::Link {
                    url,
                    image: false,
                    inlines: vec![text(candidate)],
                });
                taken = candidate.len();
            }
        } else if boundary && c == '#' && before != Some('&') {
            let digits: String = rest[1..].chars().take_while(char::is_ascii_digit).collect();
            let after = rest[1 + digits.len()..].chars().next();
            if (1..=9).contains(&digits.len()) && after.is_none_or(|a| !is_word(a)) {
                if let Ok(number) = digits.parse::<u64>() {
                    if number > 0 {
                        push_text(out, &std::mem::take(&mut pending));
                        out.push(Inline::Ref { number });
                        taken = 1 + digits.len();
                    }
                }
            }
        } else if boundary && c == '@' {
            let name: String = rest[1..]
                .chars()
                .take_while(|c| c.is_ascii_alphanumeric() || *c == '-' || *c == '_')
                .collect();
            let name = name.trim_end_matches('-');
            let starts_well = name
                .chars()
                .next()
                .is_some_and(|c| c.is_ascii_alphanumeric());
            if starts_well && name.len() <= 39 {
                push_text(out, &std::mem::take(&mut pending));
                out.push(Inline::Mention {
                    name: name.to_string(),
                });
                taken = 1 + name.len();
            }
        }
        if taken == 0 {
            pending.push(c);
            taken = c.len_utf8();
        }
        before = rest[..taken].chars().last();
        rest = &rest[taken..];
    }
    push_text(out, &pending);
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{json, Value};

    fn para(source: &str) -> Vec<Inline> {
        match render(source).as_slice() {
            [Block::Paragraph { inlines }] => inlines.clone(),
            other => panic!("not one paragraph: {other:?}"),
        }
    }

    fn link(url: &str, words: &str) -> Inline {
        Inline::Link {
            url: url.into(),
            image: false,
            inlines: vec![text(words)],
        }
    }

    /// Every node kind in a rendered tree, and every link address.
    fn survey(value: &Value, kinds: &mut Vec<String>, urls: &mut Vec<String>) {
        match value {
            Value::Object(map) => {
                if let Some(Value::String(kind)) = map.get("t") {
                    kinds.push(kind.clone());
                }
                if let Some(Value::String(url)) = map.get("url") {
                    urls.push(url.clone());
                }
                for v in map.values() {
                    survey(v, kinds, urls);
                }
            }
            Value::Array(list) => list.iter().for_each(|v| survey(v, kinds, urls)),
            _ => {}
        }
    }

    const KINDS: [&str; 16] = [
        "heading",
        "paragraph",
        "code",
        "quote",
        "list",
        "table",
        "rule",
        "text",
        "em",
        "strong",
        "strike",
        "link",
        "ref",
        "mention",
        "break",
        "code",
    ];

    #[test]
    fn the_everyday_things() {
        let blocks = render(
            "# Title\n\nSome *soft* and **strong** and ~~gone~~ and `code`.\n\n> quoted\n\n---\n\n1. one\n2. two\n\n- [x] done\n- [ ] todo\n",
        );
        assert_eq!(
            blocks[0],
            Block::Heading {
                level: 1,
                inlines: vec![text("Title")]
            }
        );
        assert_eq!(
            blocks[1],
            Block::Paragraph {
                inlines: vec![
                    text("Some "),
                    Inline::Em {
                        inlines: vec![text("soft")]
                    },
                    text(" and "),
                    Inline::Strong {
                        inlines: vec![text("strong")]
                    },
                    text(" and "),
                    Inline::Strike {
                        inlines: vec![text("gone")]
                    },
                    text(" and "),
                    Inline::Code {
                        text: "code".into()
                    },
                    text("."),
                ]
            }
        );
        assert_eq!(
            blocks[2],
            Block::Quote {
                blocks: vec![Block::Paragraph {
                    inlines: vec![text("quoted")]
                }]
            }
        );
        assert_eq!(blocks[3], Block::Rule);
        let item = |task, words: &str| Item {
            task,
            blocks: vec![Block::Paragraph {
                inlines: vec![text(words)],
            }],
        };
        assert_eq!(
            blocks[4],
            Block::List {
                ordered: true,
                start: 1,
                items: vec![item(None, "one"), item(None, "two")]
            }
        );
        assert_eq!(
            blocks[5],
            Block::List {
                ordered: false,
                start: 1,
                items: vec![item(Some(true), "done"), item(Some(false), "todo")]
            }
        );
        assert_eq!(blocks.len(), 6);
    }

    #[test]
    fn code_blocks_tables_and_nested_lists() {
        let blocks = render("```rust,ignore\nfn main() {}\n<b>not html</b>\n```\n\n    indented\n");
        assert_eq!(
            blocks,
            [
                Block::Code {
                    lang: "rust".into(),
                    text: "fn main() {}\n<b>not html</b>".into()
                },
                Block::Code {
                    lang: String::new(),
                    text: "indented".into()
                }
            ]
        );
        let table = render("| a | b |\n|:--|--:|\n| 1 | **2** |\n");
        assert_eq!(
            table,
            [Block::Table {
                aligns: vec!["left", "right"],
                head: vec![vec![text("a")], vec![text("b")]],
                rows: vec![vec![
                    vec![text("1")],
                    vec![Inline::Strong {
                        inlines: vec![text("2")]
                    }]
                ]],
            }]
        );
        // A loose item with a task marker, and a list inside a list.
        let nested = render("- [x] first\n\n  more\n- outer\n  - inner\n");
        let Block::List { items, .. } = &nested[0] else {
            panic!("{nested:?}")
        };
        assert_eq!(items[0].task, Some(true));
        assert_eq!(items[0].blocks.len(), 2);
        assert!(matches!(items[1].blocks[1], Block::List { .. }));
        // A new line in a comment is a new line.
        assert_eq!(para("one\ntwo"), [text("one"), Inline::Break, text("two")]);
    }

    #[test]
    fn links_are_only_ever_web_or_mail_addresses() {
        assert_eq!(
            para(
                "[docs](https://example.com/a?b=1) and <https://example.org> and <me@example.com>"
            ),
            [
                link("https://example.com/a?b=1", "docs"),
                text(" and "),
                link("https://example.org/", "https://example.org"),
                text(" and "),
                link("mailto:me@example.com", "me@example.com"),
            ]
        );
        // Bare addresses, without the full stop that ends the sentence.
        assert_eq!(
            para("See https://example.com/x_(y). Or http://a.example, ok"),
            [
                text("See "),
                link("https://example.com/x_(y)", "https://example.com/x_(y)"),
                text(". Or "),
                link("http://a.example/", "http://a.example"),
                text(", ok"),
            ]
        );
        // Everything else is only its words.
        for (source, words) in [
            ("[click](javascript:alert(1))", "click"),
            ("[click](JaVaScRiPt:alert(1))", "click"),
            ("[click](data:text/html,<script>alert(1)</script>)", "click"),
            ("[click](vbscript:x)", "click"),
            ("[click](file:///etc/passwd)", "click"),
            ("[click](/relative/path)", "click"),
            ("[click](https://user:pw@example.com/)", "click"),
            ("[click](<java\tscript:alert(1)>)", "click"),
            ("[click](tauri://localhost/x)", "click"),
        ] {
            let blocks = render(source);
            let (mut kinds, mut urls) = (Vec::new(), Vec::new());
            survey(
                &serde_json::to_value(&blocks).unwrap(),
                &mut kinds,
                &mut urls,
            );
            assert!(urls.is_empty(), "{source}: {urls:?}");
            assert!(!kinds.contains(&"link".to_string()), "{source}");
            assert!(plain(&para(source)).contains(words), "{source}");
        }
        assert_eq!(
            safe_url(" https://example.com "),
            Some("https://example.com/".into())
        );
        assert_eq!(safe_url("https://exa mple.com"), None);
        assert_eq!(safe_url("mailto:nobody"), None);
        assert_eq!(safe_url("https://"), None);
    }

    #[test]
    fn images_are_never_loaded() {
        assert_eq!(
            para("![a cat](https://example.com/cat.png) ![](https://example.com/x.png)"),
            [
                Inline::Link {
                    url: "https://example.com/cat.png".into(),
                    image: true,
                    inlines: vec![text("a cat")]
                },
                text(" "),
                Inline::Link {
                    url: "https://example.com/x.png".into(),
                    image: true,
                    inlines: vec![text("https://example.com/x.png")]
                },
            ]
        );
        // An image that isn't on the web is its alt text.
        assert_eq!(para("![boom](javascript:alert(1))"), [text("boom")]);
        assert_eq!(para("![local](./shot.png)"), [text("local")]);
        // No node kind is an image at all.
        let json = serde_json::to_string(&render("![x](https://example.com/x.png)")).unwrap();
        assert!(!json.contains("\"t\":\"image\"") && !json.contains("\"t\":\"img\""));
    }

    #[test]
    fn raw_html_is_only_ever_text() {
        let nasty = [
            "<script>alert(1)</script>",
            "Hello <script>alert(1)</script> there",
            "<img src=x onerror=alert(1)>",
            "text <img src=x onerror=alert(1)> more",
            "<div onclick=\"steal()\">\n\n**inside**\n\n</div>",
            "<iframe src=\"https://evil.example\"></iframe>",
            "<a href=\"javascript:alert(1)\">x</a>",
            "<style>body{display:none}</style>",
            "<svg onload=alert(1)>",
            "<!-- comment --><b>bold</b>",
            "<details><summary>s</summary>hidden</details>",
            "```\n<script>alert(1)</script>\n```",
            "| a |\n|---|\n| <script>x</script> |",
            "- <img src=x onerror=alert(1)>",
            "> <object data=x></object>",
            "<scr<script>ipt>alert(1)</scr</script>ipt>",
            "&lt;script&gt;alert(1)&lt;/script&gt;",
        ];
        for source in nasty {
            let value = serde_json::to_value(render(source)).unwrap();
            let (mut kinds, mut urls) = (Vec::new(), Vec::new());
            survey(&value, &mut kinds, &mut urls);
            // Only the known kinds: nothing the window could mistake for an
            // element to build, and no address taken from an attribute.
            for kind in &kinds {
                assert!(KINDS.contains(&kind.as_str()), "{source}: {kind}");
            }
            assert!(urls.is_empty(), "{source}: {urls:?}");
            // The keys are the fixed ones too: no attributes, no handlers.
            fn keys(value: &Value, out: &mut Vec<String>) {
                match value {
                    Value::Object(map) => {
                        for (k, v) in map {
                            out.push(k.clone());
                            keys(v, out);
                        }
                    }
                    Value::Array(list) => list.iter().for_each(|v| keys(v, out)),
                    _ => {}
                }
            }
            let mut seen = Vec::new();
            keys(&value, &mut seen);
            for key in seen {
                assert!(
                    [
                        "t", "text", "inlines", "blocks", "items", "task", "level", "lang",
                        "ordered", "start", "aligns", "head", "rows", "url", "image", "number",
                        "name"
                    ]
                    .contains(&key.as_str()),
                    "{source}: {key}"
                );
            }
        }
        // And the source is still there to read.
        assert_eq!(
            render("<script>alert(1)</script>"),
            [Block::Paragraph {
                inlines: vec![text("<script>alert(1)</script>")]
            }]
        );
        assert_eq!(
            para("Hello <img src=x onerror=alert(1)> there"),
            [text("Hello <img src=x onerror=alert(1)> there")]
        );
        assert_eq!(
            serde_json::to_value(para("a <b>bold</b>")).unwrap(),
            json!([{"t": "text", "text": "a <b>bold</b>"}])
        );
    }

    #[test]
    fn references_and_mentions() {
        assert_eq!(
            para("Fixes #12, see (#7) and @mara-lind; not a#3, #x, issue#4 or me@example.com."),
            [
                text("Fixes "),
                Inline::Ref { number: 12 },
                text(", see ("),
                Inline::Ref { number: 7 },
                text(") and "),
                Inline::Mention {
                    name: "mara-lind".into()
                },
                text("; not a#3, #x, issue#4 or me@example.com."),
            ]
        );
        // Not inside code or a link's words.
        assert_eq!(
            para("`#12 @me` [#3 @you](https://example.com)"),
            [
                Inline::Code {
                    text: "#12 @me".into()
                },
                text(" "),
                link("https://example.com/", "#3 @you"),
            ]
        );
        assert_eq!(para("#0 #1234567890"), [text("#0 #1234567890")]);
    }

    #[test]
    fn absurd_nesting_is_shown_as_written() {
        let deep = format!("{} hello", ">".repeat(5000));
        assert_eq!(
            render(&deep),
            [Block::Paragraph {
                inlines: vec![text(deep.clone())]
            }]
        );
        let stars = format!("{}x{}", "*a ".repeat(3000), " a*".repeat(3000));
        assert!(!render(&stars).is_empty());
        assert!(render("").is_empty());
        assert!(render("   \n\n").is_empty());
    }
}
