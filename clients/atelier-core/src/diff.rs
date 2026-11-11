//! A parser for git's unified diff output (`git diff`, `git diff --cached`,
//! `git diff --no-index`). The result is plain data: one [`FileDiff`] per
//! file, each a flat list of lines with their old and new line numbers, ready
//! to be drawn by a client without it having to understand the format.

use serde::Serialize;

/// What a line of a diff is.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum LineKind {
    /// The `@@ -a,b +c,d @@` header of a hunk.
    Hunk,
    /// Unchanged, shown for context.
    Context,
    Add,
    Delete,
    /// `\ No newline at end of file`, about the line before it.
    NoNewline,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct DiffLine {
    pub kind: LineKind,
    /// Line number in the old file (context and deleted lines).
    pub old: Option<u32>,
    /// Line number in the new file (context and added lines).
    pub new: Option<u32>,
    /// The line without its `+`/`-`/space marker and without the newline.
    pub text: String,
}

/// What happened to a file.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum FileStatus {
    #[default]
    Modified,
    Added,
    Deleted,
    Renamed,
    Copied,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize)]
pub struct FileDiff {
    /// `None` for a new file.
    pub old_path: Option<String>,
    /// `None` for a deleted file.
    pub new_path: Option<String>,
    pub status: FileStatus,
    /// Git wouldn't show the content ("Binary files … differ").
    pub binary: bool,
    pub additions: u32,
    pub deletions: u32,
    pub lines: Vec<DiffLine>,
    /// Lines were left out because the diff was longer than the limit.
    pub truncated: bool,
}

impl FileDiff {
    /// The path to show: the new one, or the old one of a deleted file.
    pub fn path(&self) -> &str {
        self.new_path
            .as_deref()
            .or(self.old_path.as_deref())
            .unwrap_or_default()
    }
}

/// Parses a whole diff with no limit on its length.
pub fn parse(text: &str) -> Vec<FileDiff> {
    parse_limited(text, usize::MAX)
}

/// Parses a diff, keeping at most `max_lines` lines per file. The counts of
/// added and deleted lines still cover the whole file.
pub fn parse_limited(text: &str, max_lines: usize) -> Vec<FileDiff> {
    let mut files: Vec<FileDiff> = Vec::new();
    let mut current: Option<FileDiff> = None;
    // Lines of the old and new side still expected in the current hunk, and
    // the next line number on each side.
    let (mut old_left, mut new_left) = (0u32, 0u32);
    let (mut old_no, mut new_no) = (0u32, 0u32);

    for raw in text.split('\n') {
        let line = raw.strip_suffix('\r').unwrap_or(raw);
        let in_hunk = old_left > 0 || new_left > 0;

        if in_hunk {
            if let Some(file) = current.as_mut() {
                let (kind, old, new) = match line.as_bytes().first() {
                    Some(b'+') => {
                        new_left = new_left.saturating_sub(1);
                        new_no += 1;
                        file.additions += 1;
                        (LineKind::Add, None, Some(new_no - 1))
                    }
                    Some(b'-') => {
                        old_left = old_left.saturating_sub(1);
                        old_no += 1;
                        file.deletions += 1;
                        (LineKind::Delete, Some(old_no - 1), None)
                    }
                    Some(b'\\') => (LineKind::NoNewline, None, None),
                    // A context line; an empty line is a context line whose
                    // single space was stripped by something on the way.
                    _ => {
                        old_left = old_left.saturating_sub(1);
                        new_left = new_left.saturating_sub(1);
                        old_no += 1;
                        new_no += 1;
                        (LineKind::Context, Some(old_no - 1), Some(new_no - 1))
                    }
                };
                let text = match kind {
                    LineKind::NoNewline => line.trim_start_matches(['\\', ' ']).to_string(),
                    _ => line.get(1..).unwrap_or_default().to_string(),
                };
                push_line(
                    file,
                    DiffLine {
                        kind,
                        old,
                        new,
                        text,
                    },
                    max_lines,
                );
                continue;
            }
        }

        if let Some(rest) = line.strip_prefix("diff --git ") {
            files.extend(current.take());
            let (old, new) = split_header(rest);
            current = Some(FileDiff {
                old_path: old,
                new_path: new,
                ..FileDiff::default()
            });
            continue;
        }
        let Some(file) = current.as_mut() else {
            continue;
        };
        if line.starts_with("\\ ") {
            // Follows the last line of a hunk whose counts have run out.
            push_line(
                file,
                DiffLine {
                    kind: LineKind::NoNewline,
                    old: None,
                    new: None,
                    text: line.trim_start_matches(['\\', ' ']).to_string(),
                },
                max_lines,
            );
        } else if let Some(rest) = line.strip_prefix("@@ ") {
            if let Some((o, ol, n, nl)) = parse_hunk_header(rest) {
                old_no = o;
                new_no = n;
                old_left = ol;
                new_left = nl;
                push_line(
                    file,
                    DiffLine {
                        kind: LineKind::Hunk,
                        old: None,
                        new: None,
                        text: line.to_string(),
                    },
                    max_lines,
                );
            }
        } else if line.starts_with("new file mode ") {
            file.status = FileStatus::Added;
            file.old_path = None;
        } else if line.starts_with("deleted file mode ") {
            file.status = FileStatus::Deleted;
            file.new_path = None;
        } else if let Some(path) = line.strip_prefix("rename from ") {
            file.status = FileStatus::Renamed;
            file.old_path = Some(unquote(path));
        } else if let Some(path) = line.strip_prefix("rename to ") {
            file.status = FileStatus::Renamed;
            file.new_path = Some(unquote(path));
        } else if let Some(path) = line.strip_prefix("copy from ") {
            file.status = FileStatus::Copied;
            file.old_path = Some(unquote(path));
        } else if let Some(path) = line.strip_prefix("copy to ") {
            file.status = FileStatus::Copied;
            file.new_path = Some(unquote(path));
        } else if let Some(path) = line.strip_prefix("--- ") {
            match side_path(path, "a/") {
                Some(path) => file.old_path = Some(path),
                None => {
                    file.old_path = None;
                    file.status = FileStatus::Added;
                }
            }
        } else if let Some(path) = line.strip_prefix("+++ ") {
            match side_path(path, "b/") {
                Some(path) => file.new_path = Some(path),
                None => {
                    file.new_path = None;
                    file.status = FileStatus::Deleted;
                }
            }
        } else if line.starts_with("Binary files ") || line == "GIT binary patch" {
            file.binary = true;
        }
    }
    files.extend(current);
    files
}

fn push_line(file: &mut FileDiff, line: DiffLine, max_lines: usize) {
    if file.lines.len() < max_lines {
        file.lines.push(line);
    } else {
        file.truncated = true;
    }
}

/// `-41,9 +41,16 @@ fn x()` → (old start, old count, new start, new count).
fn parse_hunk_header(rest: &str) -> Option<(u32, u32, u32, u32)> {
    let mut parts = rest.split(' ');
    let old = parts.next()?.strip_prefix('-')?;
    let new = parts.next()?.strip_prefix('+')?;
    let range = |s: &str| -> Option<(u32, u32)> {
        match s.split_once(',') {
            Some((start, count)) => Some((start.parse().ok()?, count.parse().ok()?)),
            None => Some((s.parse().ok()?, 1)),
        }
    };
    let (old_start, old_count) = range(old)?;
    let (new_start, new_count) = range(new)?;
    Some((old_start, old_count, new_start, new_count))
}

/// The path of a `---`/`+++` line, or `None` for `/dev/null`.
fn side_path(raw: &str, prefix: &str) -> Option<String> {
    // Git appends a tab after a path with spaces in it.
    let raw = raw.strip_suffix('\t').unwrap_or(raw);
    let path = unquote(raw);
    if path == "/dev/null" {
        return None;
    }
    Some(path.strip_prefix(prefix).unwrap_or(&path).to_string())
}

/// The two paths of `a/old b/new` from a `diff --git` line. Later lines
/// (`rename from`, `---`, `+++`) are more reliable and overwrite these; this
/// is all there is for binary files and mode changes.
fn split_header(rest: &str) -> (Option<String>, Option<String>) {
    let strip = |side: String, prefix: &str| -> String {
        side.strip_prefix(prefix).unwrap_or(&side).to_string()
    };
    if rest.starts_with('"') {
        // "a/one" "b/two", or "a/one" b/two
        if let Some(end) = closing_quote(rest) {
            let old = unquote(&rest[..=end]);
            let new = unquote(rest[end + 1..].trim_start());
            return (Some(strip(old, "a/")), Some(strip(new, "b/")));
        }
    }
    // Same path on both sides: `a/P b/P` splits exactly in the middle.
    if rest.len() >= 5 && (rest.len() - 5) % 2 == 0 {
        let half = (rest.len() - 5) / 2;
        let (old, new) = (rest.get(2..2 + half), rest.get(half + 5..));
        if rest.starts_with("a/") && old.is_some() && old == new {
            if let Some(" b/") = rest.get(2 + half..half + 5) {
                return (old.map(str::to_string), new.map(str::to_string));
            }
        }
    }
    match rest.find(" b/").or_else(|| rest.find(" \"b/")) {
        Some(at) => (
            Some(strip(unquote(&rest[..at]), "a/")),
            Some(strip(unquote(&rest[at + 1..]), "b/")),
        ),
        None => (Some(strip(rest.to_string(), "a/")), None),
    }
}

fn closing_quote(s: &str) -> Option<usize> {
    let bytes = s.as_bytes();
    let mut i = 1;
    while i < bytes.len() {
        match bytes[i] {
            b'\\' => i += 2,
            b'"' => return Some(i),
            _ => i += 1,
        }
    }
    None
}

/// Undoes git's C-style path quoting (`"sp\303\244ce.txt"`).
pub fn unquote(path: &str) -> String {
    let Some(inner) = path
        .strip_prefix('"')
        .and_then(|rest| rest.strip_suffix('"'))
    else {
        return path.to_string();
    };
    let bytes = inner.as_bytes();
    let mut out: Vec<u8> = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] != b'\\' || i + 1 >= bytes.len() {
            out.push(bytes[i]);
            i += 1;
            continue;
        }
        i += 1;
        match bytes[i] {
            b'n' => out.push(b'\n'),
            b't' => out.push(b'\t'),
            b'r' => out.push(b'\r'),
            b'a' => out.push(7),
            b'b' => out.push(8),
            b'f' => out.push(12),
            b'v' => out.push(11),
            digit @ b'0'..=b'7' => {
                let mut value = u32::from(digit - b'0');
                let mut taken = 1;
                while taken < 3 && i + 1 < bytes.len() && (b'0'..=b'7').contains(&bytes[i + 1]) {
                    i += 1;
                    taken += 1;
                    value = value * 8 + u32::from(bytes[i] - b'0');
                }
                out.push(value as u8);
            }
            other => out.push(other),
        }
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn kinds(file: &FileDiff) -> Vec<LineKind> {
        file.lines.iter().map(|l| l.kind).collect()
    }

    #[test]
    fn a_modified_file_has_numbered_lines() {
        let text = "diff --git a/src/tray.go b/src/tray.go\n\
index 1a2b3c4..5d6e7f8 100644\n\
--- a/src/tray.go\n\
+++ b/src/tray.go\n\
@@ -41,4 +41,5 @@ func (t *Tray) build() *Menu {\n \tm := NewMenu()\n-\tm.Add(Item{Label: \"Quit\"})\n+\tm.AddSeparator()\n+\tm.Add(Item{Label: \"Quit\", Do: t.quit})\n \treturn m\n }\n";
        let files = parse(text);
        assert_eq!(files.len(), 1);
        let f = &files[0];
        assert_eq!(f.status, FileStatus::Modified);
        assert_eq!(f.path(), "src/tray.go");
        assert_eq!((f.additions, f.deletions), (2, 1));
        assert!(!f.binary);
        assert_eq!(
            kinds(f),
            [
                LineKind::Hunk,
                LineKind::Context,
                LineKind::Delete,
                LineKind::Add,
                LineKind::Add,
                LineKind::Context,
                LineKind::Context
            ]
        );
        assert_eq!(
            f.lines[0].text,
            "@@ -41,4 +41,5 @@ func (t *Tray) build() *Menu {"
        );
        assert_eq!((f.lines[1].old, f.lines[1].new), (Some(41), Some(41)));
        assert_eq!((f.lines[2].old, f.lines[2].new), (Some(42), None));
        assert_eq!((f.lines[3].old, f.lines[3].new), (None, Some(42)));
        assert_eq!((f.lines[4].old, f.lines[4].new), (None, Some(43)));
        assert_eq!((f.lines[5].old, f.lines[5].new), (Some(43), Some(44)));
        assert_eq!(f.lines[5].text, "\treturn m");
        assert_eq!((f.lines[6].old, f.lines[6].new), (Some(44), Some(45)));
    }

    #[test]
    fn added_and_deleted_files() {
        let text = "diff --git a/new.txt b/new.txt\n\
new file mode 100644\n\
index 0000000..3b18e51\n\
--- /dev/null\n\
+++ b/new.txt\n\
@@ -0,0 +1,2 @@\n\
+hello\n\
+world\n\
diff --git a/gone.txt b/gone.txt\n\
deleted file mode 100644\n\
index 3b18e51..0000000\n\
--- a/gone.txt\n\
+++ /dev/null\n\
@@ -1 +0,0 @@\n\
-bye\n";
        let files = parse(text);
        assert_eq!(files.len(), 2);
        assert_eq!(files[0].status, FileStatus::Added);
        assert_eq!(files[0].old_path, None);
        assert_eq!(files[0].new_path.as_deref(), Some("new.txt"));
        assert_eq!((files[0].additions, files[0].deletions), (2, 0));
        assert_eq!(files[0].lines[2].new, Some(2));
        assert_eq!(files[1].status, FileStatus::Deleted);
        assert_eq!(files[1].new_path, None);
        assert_eq!(files[1].path(), "gone.txt");
        assert_eq!((files[1].additions, files[1].deletions), (0, 1));
        assert_eq!(files[1].lines[1].old, Some(1));
    }

    #[test]
    fn a_rename_with_and_without_changes() {
        let text = "diff --git a/old name.txt b/new name.txt\n\
similarity index 100%\n\
rename from old name.txt\n\
rename to new name.txt\n\
diff --git a/a.rs b/b.rs\n\
similarity index 80%\n\
rename from a.rs\n\
rename to b.rs\n\
index 111..222 100644\n\
--- a/a.rs\n\
+++ b/b.rs\n\
@@ -1,2 +1,2 @@\n one\n-two\n+three\n";
        let files = parse(text);
        assert_eq!(files.len(), 2);
        assert_eq!(files[0].status, FileStatus::Renamed);
        assert_eq!(files[0].old_path.as_deref(), Some("old name.txt"));
        assert_eq!(files[0].new_path.as_deref(), Some("new name.txt"));
        assert!(files[0].lines.is_empty());
        assert_eq!(files[1].status, FileStatus::Renamed);
        assert_eq!(files[1].old_path.as_deref(), Some("a.rs"));
        assert_eq!(files[1].new_path.as_deref(), Some("b.rs"));
        assert_eq!((files[1].additions, files[1].deletions), (1, 1));
    }

    #[test]
    fn a_binary_file() {
        let text = "diff --git a/img/logo.png b/img/logo.png\n\
index 1111111..2222222 100644\n\
Binary files a/img/logo.png and b/img/logo.png differ\n\
diff --git a/new.bin b/new.bin\n\
new file mode 100644\n\
index 0000000..2222222\n\
Binary files /dev/null and b/new.bin differ\n";
        let files = parse(text);
        assert_eq!(files.len(), 2);
        assert!(files[0].binary);
        assert_eq!(files[0].path(), "img/logo.png");
        assert_eq!(files[0].status, FileStatus::Modified);
        assert!(files[0].lines.is_empty());
        assert!(files[1].binary);
        assert_eq!(files[1].status, FileStatus::Added);
        assert_eq!(files[1].path(), "new.bin");
    }

    #[test]
    fn no_newline_at_end_of_file() {
        let text = "diff --git a/x b/x\n\
--- a/x\n\
+++ b/x\n\
@@ -1 +1 @@\n\
-old\n\
\\ No newline at end of file\n\
+new\n\
\\ No newline at end of file\n";
        let files = parse(text);
        let f = &files[0];
        assert_eq!(
            kinds(f),
            [
                LineKind::Hunk,
                LineKind::Delete,
                LineKind::NoNewline,
                LineKind::Add,
                LineKind::NoNewline
            ]
        );
        assert_eq!(f.lines[2].text, "No newline at end of file");
        assert_eq!((f.lines[2].old, f.lines[2].new), (None, None));
        assert_eq!((f.additions, f.deletions), (1, 1));
    }

    #[test]
    fn lines_that_look_like_headers_inside_a_hunk_are_content() {
        let text = "diff --git a/x b/x\n\
--- a/x\n\
+++ b/x\n\
@@ -1,2 +1,2 @@\n\
--- a/not-a-header\n\
+++ b/not-a-header\n\
 diff --git a/y b/y\n";
        let files = parse(text);
        assert_eq!(files.len(), 1);
        assert_eq!(files[0].path(), "x");
        assert_eq!(files[0].lines[1].text, "-- a/not-a-header");
        assert_eq!(files[0].lines[2].text, "++ b/not-a-header");
        assert_eq!(files[0].lines[3].kind, LineKind::Context);
    }

    #[test]
    fn several_hunks_and_a_limit() {
        let text = "diff --git a/x b/x\n\
--- a/x\n\
+++ b/x\n\
@@ -1,2 +1,2 @@\n a\n-b\n+B\n@@ -10 +10,2 @@ fn later()\n j\n+k\n";
        let files = parse(text);
        assert_eq!(files[0].lines.len(), 7);
        assert_eq!(files[0].lines[4].kind, LineKind::Hunk);
        assert_eq!(files[0].lines[5].old, Some(10));
        assert_eq!(files[0].lines[6].new, Some(11));

        let limited = parse_limited(text, 3);
        assert_eq!(limited[0].lines.len(), 3);
        assert!(limited[0].truncated);
        assert_eq!((limited[0].additions, limited[0].deletions), (2, 1));
    }

    #[test]
    fn quoted_paths_and_windows_line_ends() {
        assert_eq!(
            unquote("\"sp\\303\\244ce \\\"x\\\".txt\""),
            "späce \"x\".txt"
        );
        assert_eq!(unquote("plain.txt"), "plain.txt");
        let text = "diff --git \"a/t\\tab.txt\" \"b/t\\tab.txt\"\r\n\
--- \"a/t\\tab.txt\"\r\n\
+++ \"b/t\\tab.txt\"\r\n\
@@ -1 +1 @@\r\n\
-a\r\n\
+b\r\n";
        let files = parse(text);
        assert_eq!(files[0].path(), "t\tab.txt");
        assert_eq!(files[0].lines[1].text, "a");
    }

    #[test]
    fn nothing_in_nothing_out() {
        assert!(parse("").is_empty());
        assert!(parse("warning: something unrelated\n").is_empty());
    }
}
