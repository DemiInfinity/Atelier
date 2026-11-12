//! Staging: what is staged and what isn't, one file's diff as chunks, and
//! staging, unstaging or discarding a whole file, a chunk or a handful of
//! lines. Partial changes are made the way `git add -p` makes them: a patch
//! is built from the chosen lines and handed to `git apply`.
//!
//! Patches are handled as bytes from end to end, so a file that isn't UTF-8,
//! has CRLF line ends or no newline at its end comes through unharmed.

use std::collections::{HashMap, HashSet};
use std::hash::{Hash, Hasher};
use std::path::Path;

use serde::{Deserialize, Serialize};

use super::util::{check_path, short, Call};
use super::{ChangeKind, Git};
use crate::diff::FileStatus;
use crate::error::{Error, Result};

/// Which half of a file's changes is meant.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Side {
    /// Already in the index: what the next commit takes.
    Staged,
    /// In the working tree only.
    Unstaged,
}

/// One file on one side of the index.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct StageEntry {
    pub path: String,
    /// Where a renamed file used to be.
    pub old_path: Option<String>,
    pub kind: ChangeKind,
    pub additions: Option<u32>,
    pub deletions: Option<u32>,
    pub binary: bool,
}

/// The commit an amend would rewrite.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct HeadCommit {
    pub sha: String,
    pub short: String,
    pub subject: String,
    pub body: String,
    /// Some remote already has it, so amending rewrites shared history.
    pub pushed: bool,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize)]
pub struct StageStatus {
    pub branch: Option<String>,
    pub head: Option<HeadCommit>,
    pub staged: Vec<StageEntry>,
    pub unstaged: Vec<StageEntry>,
    /// Files with a merge conflict; they are neither staged nor not.
    pub conflicted: Vec<StageEntry>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ChunkLineKind {
    Context,
    Add,
    Delete,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct ChunkLine {
    /// Position in the chunk; what a line selection refers to.
    pub index: usize,
    pub kind: ChunkLineKind,
    pub old: Option<u32>,
    pub new: Option<u32>,
    /// Without its marker and line end.
    pub text: String,
    /// The file ends here without a newline.
    pub no_newline: bool,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct Chunk {
    pub index: usize,
    pub old_start: u32,
    pub old_lines: u32,
    pub new_start: u32,
    pub new_lines: u32,
    /// What git printed after the `@@` (usually the enclosing function).
    pub section: String,
    pub additions: u32,
    pub deletions: u32,
    pub lines: Vec<ChunkLine>,
}

/// One file's changes on one side, cut into chunks.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct StageDiff {
    pub path: String,
    pub old_path: Option<String>,
    pub side: Side,
    pub kind: ChangeKind,
    pub status: FileStatus,
    /// Binary files can only be staged or discarded whole.
    pub binary: bool,
    pub additions: u32,
    pub deletions: u32,
    pub chunks: Vec<Chunk>,
    /// Names exactly this diff. Staging part of it must send it back, so a
    /// file that changed in between is noticed instead of mangled.
    pub token: String,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct LineRef {
    pub chunk: usize,
    pub line: usize,
}

/// How much of a file an action is about.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Selection {
    File,
    Chunks { chunks: Vec<usize> },
    Lines { lines: Vec<LineRef> },
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct Committed {
    pub sha: String,
    pub short: String,
    pub subject: String,
    pub amended: bool,
    /// The entry in Recent actions.
    pub journal: Option<u64>,
}

// ------------------------------------------------------------ raw patches

#[derive(Clone, Debug, PartialEq, Eq)]
struct RawLine {
    /// `b' '`, `b'+'` or `b'-'`.
    kind: u8,
    /// Without the marker and without the newline that ends the diff line.
    content: Vec<u8>,
    no_newline: bool,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct RawHunk {
    old_start: u32,
    old_count: u32,
    new_start: u32,
    new_count: u32,
    section: Vec<u8>,
    lines: Vec<RawLine>,
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
struct RawDiff {
    header: Vec<Vec<u8>>,
    hunks: Vec<RawHunk>,
    binary: bool,
    new_file: bool,
    deleted: bool,
    renamed: bool,
}

fn parse_range(text: &[u8]) -> Option<(u32, u32)> {
    let text = std::str::from_utf8(text).ok()?;
    match text.split_once(',') {
        Some((start, count)) => Some((start.parse().ok()?, count.parse().ok()?)),
        None => Some((text.parse().ok()?, 1)),
    }
}

/// `@@ -41,9 +41,16 @@ fn x()`.
fn parse_hunk_header(line: &[u8]) -> Option<RawHunk> {
    let rest = line.strip_prefix(b"@@ -")?;
    let end = rest.windows(3).position(|w| w == b" @@")?;
    let (ranges, after) = rest.split_at(end);
    let mut parts = ranges.split(|b| *b == b' ');
    let (old_start, old_count) = parse_range(parts.next()?)?;
    let (new_start, new_count) = parse_range(parts.next()?.strip_prefix(b"+")?)?;
    let section = after[3..].strip_prefix(b" ").unwrap_or(&after[3..]);
    Some(RawHunk {
        old_start,
        old_count,
        new_start,
        new_count,
        section: section.to_vec(),
        lines: Vec::new(),
    })
}

/// Reads the diff of one file (the first one in `bytes`).
fn parse_raw(bytes: &[u8]) -> RawDiff {
    let mut diff = RawDiff::default();
    let lines = bytes.split(|b| *b == b'\n');
    let mut started = false;
    // Lines of each side the current hunk still expects.
    let (mut old_left, mut new_left) = (0u32, 0u32);
    for line in lines {
        if old_left > 0 || new_left > 0 {
            let Some(hunk) = diff.hunks.last_mut() else {
                break;
            };
            let kind = match line.first() {
                Some(b'+') => b'+',
                Some(b'-') => b'-',
                Some(b'\\') => {
                    if let Some(last) = hunk.lines.last_mut() {
                        last.no_newline = true;
                    }
                    continue;
                }
                _ => b' ',
            };
            if kind != b'+' {
                old_left = old_left.saturating_sub(1);
            }
            if kind != b'-' {
                new_left = new_left.saturating_sub(1);
            }
            hunk.lines.push(RawLine {
                kind,
                content: line.get(1..).unwrap_or_default().to_vec(),
                no_newline: false,
            });
            continue;
        }
        if line.starts_with(b"\\ ") {
            if let Some(last) = diff.hunks.last_mut().and_then(|h| h.lines.last_mut()) {
                last.no_newline = true;
            }
            continue;
        }
        if line.starts_with(b"diff --git ") || line.starts_with(b"diff --cc ") {
            if started {
                break;
            }
            started = true;
            diff.header.push(line.to_vec());
            continue;
        }
        if let Some(hunk) = parse_hunk_header(line) {
            old_left = hunk.old_count;
            new_left = hunk.new_count;
            diff.hunks.push(hunk);
            continue;
        }
        if !diff.hunks.is_empty() {
            continue;
        }
        if line.starts_with(b"Binary files ") || line == b"GIT binary patch" {
            diff.binary = true;
        } else if line.starts_with(b"new file mode ") {
            diff.new_file = true;
        } else if line.starts_with(b"deleted file mode ") {
            diff.deleted = true;
        } else if line.starts_with(b"rename from ") || line.starts_with(b"copy from ") {
            diff.renamed = true;
        }
        if started && !line.is_empty() {
            diff.header.push(line.to_vec());
        }
    }
    diff
}

/// Which way a partial patch will be applied.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Direction {
    /// Onto the old side (staging: index → index + chosen lines).
    Forward,
    /// Backwards onto the new side (unstaging, discarding).
    Reverse,
}

/// Builds the patch that carries only the chosen change lines of `diff`.
/// `chosen(chunk, line)` says whether a change line was picked. `None` when
/// nothing changes.
fn build_patch(
    diff: &RawDiff,
    direction: Direction,
    chosen: &dyn Fn(usize, usize) -> bool,
) -> Option<Vec<u8>> {
    let partial = diff.hunks.iter().enumerate().any(|(c, hunk)| {
        hunk.lines
            .iter()
            .enumerate()
            .any(|(l, line)| line.kind != b' ' && !chosen(c, l))
    });

    // The file's own header fits unless leaving lines out changes what the
    // patch does to the file as a whole: half a deletion is an edit, and so
    // is taking half of a new file back out. A rename is never undone by a
    // partial patch: only the lines move.
    let as_edit = match direction {
        Direction::Forward => partial && diff.deleted,
        Direction::Reverse => (partial && diff.new_file) || diff.renamed,
    };
    let mut out: Vec<u8> = Vec::new();
    if as_edit {
        let side = |prefix: &[u8]| {
            diff.header
                .iter()
                .find(|l| l.starts_with(prefix) && !l[4..].starts_with(b"/dev/null"))
                .map(|l| l[4..].to_vec())
        };
        // Either side names the file; `-p1` strips `a/` and `b/` alike.
        let name = match direction {
            Direction::Forward => side(b"--- ").or_else(|| side(b"+++ ")),
            Direction::Reverse => side(b"+++ ").or_else(|| side(b"--- ")),
        }?;
        out.extend_from_slice(b"--- ");
        out.extend_from_slice(&name);
        out.extend_from_slice(b"\n+++ ");
        out.extend_from_slice(&name);
        out.push(b'\n');
    } else {
        for line in &diff.header {
            if line.starts_with(b"index ") {
                continue;
            }
            out.extend_from_slice(line);
            out.push(b'\n');
        }
    }

    let mut delta: i64 = 0;
    let mut any = false;
    for (c, hunk) in diff.hunks.iter().enumerate() {
        let mut body: Vec<&RawLine> = Vec::new();
        let mut kinds: Vec<u8> = Vec::new();
        let mut changed = false;
        for (l, line) in hunk.lines.iter().enumerate() {
            let kind = match (line.kind, chosen(c, l), direction) {
                (b' ', _, _) => Some(b' '),
                (kind, true, _) => {
                    changed = true;
                    Some(kind)
                }
                // Not chosen, applying forward: a removed line stays (it is
                // context), an added line doesn't exist yet.
                (b'-', false, Direction::Forward) => Some(b' '),
                (b'+', false, Direction::Forward) => None,
                // Not chosen, applying backwards: the mirror image.
                (b'+', false, Direction::Reverse) => Some(b' '),
                _ => None,
            };
            if let Some(kind) = kind {
                body.push(line);
                kinds.push(kind);
            }
        }
        if !changed {
            continue;
        }
        any = true;
        let old_count = kinds.iter().filter(|k| **k != b'+').count() as i64;
        let new_count = kinds.iter().filter(|k| **k != b'-').count() as i64;
        let (old_start, new_start) = match direction {
            Direction::Forward => {
                // The old side is exactly as git saw it.
                let old_pos = if hunk.old_count > 0 {
                    i64::from(hunk.old_start)
                } else {
                    i64::from(hunk.old_start) + 1
                };
                let new_pos = old_pos + delta;
                (
                    if old_count > 0 { old_pos } else { old_pos - 1 },
                    if new_count > 0 { new_pos } else { new_pos - 1 },
                )
            }
            Direction::Reverse => {
                let new_pos = if hunk.new_count > 0 {
                    i64::from(hunk.new_start)
                } else {
                    i64::from(hunk.new_start) + 1
                };
                let old_pos = new_pos - delta;
                (
                    if old_count > 0 { old_pos } else { old_pos - 1 },
                    if new_count > 0 { new_pos } else { new_pos - 1 },
                )
            }
        };
        delta += new_count - old_count;
        out.extend_from_slice(
            format!(
                "@@ -{},{} +{},{} @@\n",
                old_start.max(0),
                old_count,
                new_start.max(0),
                new_count
            )
            .as_bytes(),
        );
        for (line, kind) in body.iter().zip(&kinds) {
            out.push(*kind);
            out.extend_from_slice(&line.content);
            out.push(b'\n');
            if line.no_newline {
                out.extend_from_slice(b"\\ No newline at end of file\n");
            }
        }
    }
    any.then_some(out)
}

fn token_of(bytes: &[u8]) -> String {
    let mut hasher = std::collections::hash_map::DefaultHasher::new();
    bytes.hash(&mut hasher);
    format!("{:016x}", hasher.finish())
}

fn text_of(bytes: &[u8]) -> String {
    let bytes = bytes.strip_suffix(b"\r").unwrap_or(bytes);
    String::from_utf8_lossy(bytes).into_owned()
}

fn chunks_of(raw: &RawDiff) -> Vec<Chunk> {
    raw.hunks
        .iter()
        .enumerate()
        .map(|(index, hunk)| {
            let (mut old, mut new) = (hunk.old_start, hunk.new_start);
            let lines: Vec<ChunkLine> = hunk
                .lines
                .iter()
                .enumerate()
                .map(|(i, line)| {
                    let (kind, o, n) = match line.kind {
                        b'+' => {
                            new += 1;
                            (ChunkLineKind::Add, None, Some(new - 1))
                        }
                        b'-' => {
                            old += 1;
                            (ChunkLineKind::Delete, Some(old - 1), None)
                        }
                        _ => {
                            old += 1;
                            new += 1;
                            (ChunkLineKind::Context, Some(old - 1), Some(new - 1))
                        }
                    };
                    ChunkLine {
                        index: i,
                        kind,
                        old: o,
                        new: n,
                        text: text_of(&line.content),
                        no_newline: line.no_newline,
                    }
                })
                .collect();
            Chunk {
                index,
                old_start: hunk.old_start,
                old_lines: hunk.old_count,
                new_start: hunk.new_start,
                new_lines: hunk.new_count,
                section: text_of(&hunk.section),
                additions: lines
                    .iter()
                    .filter(|l| l.kind == ChunkLineKind::Add)
                    .count() as u32,
                deletions: lines
                    .iter()
                    .filter(|l| l.kind == ChunkLineKind::Delete)
                    .count() as u32,
                lines,
            }
        })
        .collect()
}

// ----------------------------------------------------------------- status

fn kind_of(code: u8) -> ChangeKind {
    match code {
        b'A' | b'C' => ChangeKind::Added,
        b'D' => ChangeKind::Deleted,
        b'R' => ChangeKind::Renamed,
        _ => ChangeKind::Modified,
    }
}

struct Parsed {
    staged: Vec<StageEntry>,
    unstaged: Vec<StageEntry>,
    conflicted: Vec<StageEntry>,
}

fn entry(path: &str, old_path: Option<&str>, kind: ChangeKind) -> StageEntry {
    StageEntry {
        path: path.to_string(),
        old_path: old_path.map(str::to_string),
        kind,
        additions: None,
        deletions: None,
        binary: false,
    }
}

/// `git status --porcelain=v2 -z`.
fn parse_status_v2(out: &str) -> Parsed {
    let mut parsed = Parsed {
        staged: Vec::new(),
        unstaged: Vec::new(),
        conflicted: Vec::new(),
    };
    let mut entries = out.split('\0');
    while let Some(record) = entries.next() {
        let mut both = |xy: &str, path: &str, old: Option<&str>| {
            let (x, y) = (xy.as_bytes()[0], xy.as_bytes()[1]);
            if x != b'.' {
                let kind = kind_of(x);
                let old = old.filter(|_| kind == ChangeKind::Renamed);
                parsed.staged.push(entry(path, old, kind));
            }
            if y != b'.' {
                parsed.unstaged.push(entry(path, None, kind_of(y)));
            }
        };
        match record.as_bytes().first() {
            Some(b'1') => {
                let fields: Vec<&str> = record.splitn(9, ' ').collect();
                if let [_, xy, _, _, _, _, _, _, path] = fields[..] {
                    if xy.len() == 2 {
                        both(xy, path, None);
                    }
                }
            }
            Some(b'2') => {
                let old = entries.next();
                let fields: Vec<&str> = record.splitn(10, ' ').collect();
                if let [_, xy, _, _, _, _, _, _, _, path] = fields[..] {
                    if xy.len() == 2 {
                        both(xy, path, old);
                    }
                }
            }
            Some(b'u') => {
                let fields: Vec<&str> = record.splitn(11, ' ').collect();
                if let Some(path) = fields.get(10) {
                    parsed
                        .conflicted
                        .push(entry(path, None, ChangeKind::Conflicted));
                }
            }
            Some(b'?') => {
                if let Some(path) = record.get(2..) {
                    parsed
                        .unstaged
                        .push(entry(path, None, ChangeKind::Untracked));
                }
            }
            _ => {}
        }
    }
    for list in [
        &mut parsed.staged,
        &mut parsed.unstaged,
        &mut parsed.conflicted,
    ] {
        list.sort_by(|a, b| a.path.cmp(&b.path));
    }
    parsed
}

/// `added\tdeleted\tpath\0`, `-` for binary.
fn parse_numstat(out: &str) -> HashMap<String, (Option<u32>, Option<u32>)> {
    out.split('\0')
        .filter_map(|entry| {
            let mut parts = entry.trim_start_matches('\n').splitn(3, '\t');
            let additions = parts.next()?;
            let deletions = parts.next()?;
            let path = parts.next().filter(|p| !p.is_empty())?;
            Some((
                path.to_string(),
                (additions.parse().ok(), deletions.parse().ok()),
            ))
        })
        .collect()
}

/// Lines of an untracked file, or `None` when it is binary or too big to read.
fn count_untracked(path: &Path) -> (Option<u32>, bool) {
    let Ok(meta) = std::fs::symlink_metadata(path) else {
        return (None, false);
    };
    if !meta.is_file() || meta.len() > 1024 * 1024 {
        return (None, false);
    }
    let Ok(bytes) = std::fs::read(path) else {
        return (None, false);
    };
    if bytes.contains(&0) {
        return (None, true);
    }
    let newlines = bytes.iter().filter(|&&b| b == b'\n').count();
    let unfinished = usize::from(bytes.last().is_some_and(|&b| b != b'\n'));
    (Some((newlines + unfinished) as u32), false)
}

const DIFF_OPTIONS: [&str; 11] = [
    "-c",
    "core.quotePath=false",
    "diff",
    "--no-color",
    "--no-ext-diff",
    "--no-textconv",
    "--src-prefix=a/",
    "--dst-prefix=b/",
    "--unified=3",
    "--inter-hunk-context=0",
    "--full-index",
];

impl Git {
    /// What is staged, what isn't, and what is in conflict.
    pub async fn stage_status(&self, repo: &Path) -> Result<StageStatus> {
        let out = self
            .exec(
                repo,
                &[
                    "status",
                    "--porcelain=v2",
                    "-z",
                    "--untracked-files=all",
                    "--find-renames",
                ],
                Call::default(),
            )
            .await?;
        if !out.ok() {
            return Err(Error::NotARepo(repo.to_path_buf()));
        }
        let mut parsed = parse_status_v2(&out.text());

        let staged = parse_numstat(
            &self
                .text(
                    repo,
                    &["diff", "--cached", "--numstat", "-z", "--no-renames"],
                )
                .await?,
        );
        let unstaged = parse_numstat(
            &self
                .text(repo, &["diff", "--numstat", "-z", "--no-renames"])
                .await?,
        );
        for item in &mut parsed.staged {
            if let Some(&(additions, deletions)) = staged.get(&item.path) {
                item.additions = additions;
                item.deletions = deletions;
                item.binary = additions.is_none();
                // A rename's removed lines are counted on the old path.
                if item.kind == ChangeKind::Renamed && !item.binary {
                    item.deletions = Some(0);
                }
            }
        }
        for item in &mut parsed.unstaged {
            if item.kind == ChangeKind::Untracked {
                let (lines, binary) = count_untracked(&repo.join(&item.path));
                item.additions = lines;
                item.deletions = lines.map(|_| 0);
                item.binary = binary;
            } else if let Some(&(additions, deletions)) = unstaged.get(&item.path) {
                item.additions = additions;
                item.deletions = deletions;
                item.binary = additions.is_none();
            }
        }

        let head = match self.head_sha(repo).await? {
            Some(sha) => {
                let message = self.text(repo, &["log", "-1", "--format=%s%x1f%b"]).await?;
                let (subject, body) = message.split_once('\u{1f}').unwrap_or((&message, ""));
                Some(HeadCommit {
                    pushed: self.is_pushed(repo, &sha).await?,
                    short: short(&sha),
                    sha,
                    subject: subject.trim().to_string(),
                    body: body.trim().to_string(),
                })
            }
            None => None,
        };
        Ok(StageStatus {
            branch: self.current_branch(repo).await?,
            head,
            staged: parsed.staged,
            unstaged: parsed.unstaged,
            conflicted: parsed.conflicted,
        })
    }

    /// The raw diff of one side of one file, and what the status says of it.
    async fn stage_raw(
        &self,
        repo: &Path,
        path: &str,
        side: Side,
    ) -> Result<(StageEntry, Vec<u8>)> {
        check_path(path)?;
        let status = self.stage_status(repo).await?;
        if status.conflicted.iter().any(|e| e.path == path) {
            return Err(Error::Invalid(format!(
                "{path} has a conflict. Resolve it first."
            )));
        }
        let list = match side {
            Side::Staged => status.staged,
            Side::Unstaged => status.unstaged,
        };
        let item = list.into_iter().find(|e| e.path == path).ok_or_else(|| {
            Error::Invalid(match side {
                Side::Staged => format!("{path} has nothing staged."),
                Side::Unstaged => format!("{path} has no changes that aren’t staged."),
            })
        })?;
        let mut args: Vec<&str> = DIFF_OPTIONS.to_vec();
        let mut ok_codes = vec![0];
        let null = if cfg!(windows) { "NUL" } else { "/dev/null" };
        match (side, item.kind) {
            (Side::Unstaged, ChangeKind::Untracked) => {
                args.extend(["--no-index", "--", null, path]);
                ok_codes.push(1);
            }
            (Side::Unstaged, _) => args.extend(["--no-renames", "--", path]),
            (Side::Staged, _) => {
                args.extend(["--cached", "-M", "--"]);
                if let Some(old) = item.old_path.as_deref() {
                    args.push(old);
                }
                args.push(path);
            }
        }
        let out = self.exec(repo, &args, Call::default().literal()).await?;
        if !ok_codes.contains(&out.code) {
            return Err(out.error());
        }
        Ok((item, out.stdout))
    }

    /// One side of one file's changes as chunks with line numbers.
    pub async fn stage_diff(&self, repo: &Path, path: &str, side: Side) -> Result<StageDiff> {
        let (item, bytes) = self.stage_raw(repo, path, side).await?;
        let raw = parse_raw(&bytes);
        let chunks = chunks_of(&raw);
        let status = match item.kind {
            ChangeKind::Added | ChangeKind::Untracked => FileStatus::Added,
            ChangeKind::Deleted => FileStatus::Deleted,
            ChangeKind::Renamed => FileStatus::Renamed,
            _ => FileStatus::Modified,
        };
        Ok(StageDiff {
            path: path.to_string(),
            old_path: item.old_path,
            side,
            kind: item.kind,
            status,
            binary: raw.binary || item.binary,
            additions: chunks.iter().map(|c| c.additions).sum(),
            deletions: chunks.iter().map(|c| c.deletions).sum(),
            chunks,
            token: token_of(&bytes),
        })
    }

    /// The patch for part of a file, checked against what the window saw.
    async fn partial_patch(
        &self,
        repo: &Path,
        path: &str,
        side: Side,
        selection: &Selection,
        token: Option<&str>,
        direction: Direction,
    ) -> Result<Vec<u8>> {
        let (_, bytes) = self.stage_raw(repo, path, side).await?;
        if token.is_some_and(|t| t != token_of(&bytes)) {
            return Err(Error::Invalid(format!(
                "{path} changed since you looked at it. Look again, then choose."
            )));
        }
        let raw = parse_raw(&bytes);
        if raw.binary {
            return Err(Error::Invalid(format!(
                "{path} is a binary file: it can only be taken or left whole."
            )));
        }
        let (chunks, lines): (HashSet<usize>, HashSet<(usize, usize)>) = match selection {
            Selection::File => (HashSet::new(), HashSet::new()),
            Selection::Chunks { chunks } => (chunks.iter().copied().collect(), HashSet::new()),
            Selection::Lines { lines } => (
                HashSet::new(),
                lines.iter().map(|l| (l.chunk, l.line)).collect(),
            ),
        };
        if chunks.iter().any(|c| *c >= raw.hunks.len())
            || lines.iter().any(|(c, l)| {
                raw.hunks
                    .get(*c)
                    .and_then(|h| h.lines.get(*l))
                    .is_none_or(|line| line.kind == b' ')
            })
        {
            return Err(Error::Invalid(format!(
                "{path} changed since you looked at it. Look again, then choose."
            )));
        }
        build_patch(&raw, direction, &|c, l| {
            chunks.contains(&c) || lines.contains(&(c, l))
        })
        .ok_or_else(|| Error::Invalid("Choose at least one changed line first.".into()))
    }

    async fn apply_patch(
        &self,
        repo: &Path,
        patch: &[u8],
        args: &[&str],
        path: &str,
    ) -> Result<()> {
        let mut full = vec!["apply", "--whitespace=nowarn"];
        full.extend(args);
        full.push("-");
        let out = self.exec(repo, &full, Call::default().stdin(patch)).await?;
        if out.ok() {
            Ok(())
        } else {
            Err(Error::Invalid(format!(
                "Those lines of {path} can’t be taken on their own ({}). Choose the whole chunk instead.",
                out.said()
            )))
        }
    }

    /// Stages a whole file, some of its chunks, or some of its lines.
    /// `token` is [`StageDiff::token`] of the diff the choice was made in.
    pub async fn stage(
        &self,
        repo: &Path,
        path: &str,
        selection: &Selection,
        token: Option<&str>,
    ) -> Result<()> {
        check_path(path)?;
        if *selection == Selection::File {
            let out = self
                .exec(
                    repo,
                    &["add", "--all", "--", path],
                    Call::default().literal(),
                )
                .await?;
            return out.into_result().map(|_| ());
        }
        let patch = self
            .partial_patch(
                repo,
                path,
                Side::Unstaged,
                selection,
                token,
                Direction::Forward,
            )
            .await?;
        self.apply_patch(repo, &patch, &["--cached"], path).await
    }

    /// Stages everything that isn't staged yet (conflicted files stay).
    pub async fn stage_all(&self, repo: &Path) -> Result<()> {
        let status = self.stage_status(repo).await?;
        let paths: Vec<&str> = status.unstaged.iter().map(|e| e.path.as_str()).collect();
        if paths.is_empty() {
            return Ok(());
        }
        let mut args = vec!["add", "--all", "--"];
        args.extend(paths);
        self.exec(repo, &args, Call::default().literal())
            .await?
            .into_result()
            .map(|_| ())
    }

    /// Takes a whole file, some chunks or some lines back out of the index.
    /// The working tree is never touched.
    pub async fn unstage(
        &self,
        repo: &Path,
        path: &str,
        selection: &Selection,
        token: Option<&str>,
    ) -> Result<()> {
        check_path(path)?;
        if *selection == Selection::File {
            let status = self.stage_status(repo).await?;
            let item = status
                .staged
                .iter()
                .find(|e| e.path == path)
                .ok_or_else(|| Error::Invalid(format!("{path} has nothing staged.")))?;
            let mut paths = vec![path];
            paths.extend(item.old_path.as_deref());
            return self.unstage_paths(repo, &paths).await;
        }
        let patch = self
            .partial_patch(
                repo,
                path,
                Side::Staged,
                selection,
                token,
                Direction::Reverse,
            )
            .await?;
        self.apply_patch(repo, &patch, &["--cached", "-R"], path)
            .await
    }

    async fn unstage_paths(&self, repo: &Path, paths: &[&str]) -> Result<()> {
        let mut args = if self.head_sha(repo).await?.is_some() {
            vec!["restore", "--staged", "--"]
        } else {
            // Before the first commit there is nothing to restore from.
            vec!["rm", "--quiet", "--cached", "-r", "--"]
        };
        args.extend(paths);
        self.exec(repo, &args, Call::default().literal())
            .await?
            .into_result()
            .map(|_| ())
    }

    /// Unstages everything.
    pub async fn unstage_all(&self, repo: &Path) -> Result<()> {
        let status = self.stage_status(repo).await?;
        let mut paths: Vec<&str> = Vec::new();
        for item in &status.staged {
            paths.push(&item.path);
            paths.extend(item.old_path.as_deref());
        }
        if paths.is_empty() {
            return Ok(());
        }
        self.unstage_paths(repo, &paths).await
    }

    /// Throws away changes that aren't staged: a whole file, some chunks or
    /// some lines. What is thrown away is kept in Recently discarded first.
    /// Returns the journal entry, when anything was discarded.
    pub async fn discard(
        &self,
        repo: &Path,
        path: &str,
        selection: &Selection,
        token: Option<&str>,
    ) -> Result<Option<u64>> {
        check_path(path)?;
        let status = self.stage_status(repo).await?;
        let item = status
            .unstaged
            .iter()
            .find(|e| e.path == path)
            .ok_or_else(|| Error::Invalid(format!("{path} has no changes that aren’t staged.")))?
            .clone();
        // Build (and so check) the patch before anything is recorded.
        let patch = match selection {
            Selection::File => None,
            _ => Some(
                self.partial_patch(
                    repo,
                    path,
                    Side::Unstaged,
                    selection,
                    token,
                    Direction::Reverse,
                )
                .await?,
            ),
        };
        let what = match selection {
            Selection::File => format!("Discarded changes in {path}"),
            Selection::Chunks { chunks } if chunks.len() == 1 => {
                format!("Discarded a chunk of {path}")
            }
            Selection::Chunks { chunks } => {
                format!("Discarded {} chunks of {path}", chunks.len())
            }
            Selection::Lines { lines } if lines.len() == 1 => {
                format!("Discarded a line of {path}")
            }
            Selection::Lines { lines } => format!("Discarded {} lines of {path}", lines.len()),
        };
        let paths = [path.to_string()];
        let keep = self.discard_begin(repo, Some(&paths)).await?;
        let done = match patch {
            Some(patch) => self.apply_patch(repo, &patch, &["-R"], path).await,
            None => self.discard_file(repo, &item).await,
        };
        match done {
            Ok(()) => self.discard_finish(repo, keep, &what, Some(&paths)).await,
            Err(e) => Err(e),
        }
    }

    /// Throws away the unstaged changes of several files at once, as one
    /// entry in Recently discarded.
    pub async fn discard_files(&self, repo: &Path, paths: &[String]) -> Result<Option<u64>> {
        let status = self.stage_status(repo).await?;
        let mut items: Vec<StageEntry> = Vec::new();
        for path in paths {
            check_path(path)?;
            if let Some(item) = status.unstaged.iter().find(|e| &e.path == path) {
                items.push(item.clone());
            }
        }
        if items.is_empty() {
            return Ok(None);
        }
        let touched: Vec<String> = items.iter().map(|e| e.path.clone()).collect();
        let what = match touched.as_slice() {
            [one] => format!("Discarded changes in {one}"),
            many => format!("Discarded changes in {} files", many.len()),
        };
        let keep = self.discard_begin(repo, Some(&touched)).await?;
        for item in &items {
            self.discard_file(repo, item).await?;
        }
        self.discard_finish(repo, keep, &what, Some(&touched)).await
    }

    /// Puts one file's working-tree content back to what the index has; a
    /// file git doesn't know is deleted.
    async fn discard_file(&self, repo: &Path, item: &StageEntry) -> Result<()> {
        let path = item.path.as_str();
        let args: Vec<&str> = match item.kind {
            ChangeKind::Untracked => vec!["clean", "--quiet", "--force", "--", path],
            _ => vec!["restore", "--worktree", "--", path],
        };
        self.exec(repo, &args, Call::default().literal())
            .await?
            .into_result()
            .map(|_| ())
    }

    /// Commits what is staged. With `amend` the last commit is rewritten to
    /// hold what it had plus what is staged, under the new message.
    pub async fn commit_staged(
        &self,
        repo: &Path,
        message: &str,
        description: &str,
        amend: bool,
    ) -> Result<Committed> {
        let message = message.trim();
        if message.is_empty() {
            return Err(Error::Invalid("Write a commit message first.".into()));
        }
        let status = self.stage_status(repo).await?;
        if let Some(first) = status.conflicted.first() {
            return Err(Error::Invalid(format!(
                "{} still has a conflict. Resolve it first.",
                first.path
            )));
        }
        if amend && status.head.is_none() {
            return Err(Error::Invalid("There is no commit to amend yet.".into()));
        }
        if !amend && status.staged.is_empty() {
            return Err(Error::Invalid("Stage something to commit first.".into()));
        }
        let title = if amend {
            format!("Amended “{message}”")
        } else {
            format!("Committed “{message}”")
        };
        let refs = self
            .current_ref(repo)
            .await?
            .into_iter()
            .collect::<Vec<_>>();
        let pending = self
            .journal_begin(repo, if amend { "amend" } else { "commit" }, &title, &refs)
            .await?;
        let mut args = vec!["commit", "--quiet", "-m", message];
        let description = description.trim();
        if !description.is_empty() {
            args.extend(["-m", description]);
        }
        if amend {
            args.push("--amend");
        }
        let out = self.exec(repo, &args, Call::default().no_editor()).await?;
        if !out.ok() {
            self.journal_cancel(repo, pending).await;
            return Err(out.error());
        }
        let journal = self.journal_finish(repo, pending, None).await.ok();
        let sha = self.line(repo, &["rev-parse", "HEAD"]).await?;
        Ok(Committed {
            short: short(&sha),
            sha,
            subject: message.to_string(),
            amended: amend,
            journal,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const DIFF: &[u8] = b"diff --git a/f.txt b/f.txt\nindex 1111111..2222222 100644\n--- a/f.txt\n+++ b/f.txt\n@@ -1,4 +1,5 @@ fn x()\n one\n-two\n+TWO\n+two and a half\n three\n four\n@@ -10,2 +11,3 @@\n ten\n+new\n eleven\n\\ No newline at end of file\n";

    fn text(patch: Option<Vec<u8>>) -> String {
        String::from_utf8(patch.unwrap()).unwrap()
    }

    #[test]
    fn a_diff_is_read_into_hunks() {
        let raw = parse_raw(DIFF);
        assert_eq!(raw.header.len(), 4);
        assert_eq!(raw.hunks.len(), 2);
        assert_eq!(raw.hunks[0].section, b"fn x()");
        assert_eq!(raw.hunks[0].lines.len(), 6);
        assert!(raw.hunks[1].lines[2].no_newline);
        let chunks = chunks_of(&raw);
        assert_eq!((chunks[0].additions, chunks[0].deletions), (2, 1));
        assert_eq!(chunks[0].lines[1].old, Some(2));
        assert_eq!(chunks[0].lines[2].new, Some(2));
        assert_eq!(chunks[1].lines[1].new, Some(12));
    }

    #[test]
    fn staging_one_added_line_keeps_the_rest_as_it_was() {
        let raw = parse_raw(DIFF);
        // Only "+two and a half".
        let patch = text(build_patch(&raw, Direction::Forward, &|c, l| {
            (c, l) == (0, 3)
        }));
        assert_eq!(
            patch,
            "diff --git a/f.txt b/f.txt\n--- a/f.txt\n+++ b/f.txt\n@@ -1,4 +1,5 @@\n one\n two\n+two and a half\n three\n four\n"
        );
    }

    #[test]
    fn unstaging_one_line_treats_the_others_as_already_there() {
        let raw = parse_raw(DIFF);
        // Only "+new" of the second hunk, backwards.
        let patch = text(build_patch(&raw, Direction::Reverse, &|c, l| {
            (c, l) == (1, 1)
        }));
        assert_eq!(
            patch,
            "diff --git a/f.txt b/f.txt\n--- a/f.txt\n+++ b/f.txt\n@@ -11,2 +11,3 @@\n ten\n+new\n eleven\n\\ No newline at end of file\n"
        );
        // The removed line alone, backwards: the added ones are context.
        let patch = text(build_patch(&raw, Direction::Reverse, &|c, l| {
            (c, l) == (0, 1)
        }));
        assert!(
            patch.contains("@@ -1,6 +1,5 @@\n one\n-two\n TWO\n two and a half\n three\n four\n")
        );
    }

    #[test]
    fn later_hunks_move_by_what_earlier_ones_added() {
        let raw = parse_raw(DIFF);
        let patch = text(build_patch(&raw, Direction::Forward, &|_, _| true));
        assert!(patch.contains("@@ -1,4 +1,5 @@\n"));
        assert!(patch.contains("@@ -10,2 +11,3 @@\n"));
        // Without the first hunk the second one starts where the old file has it.
        let patch = text(build_patch(&raw, Direction::Forward, &|c, _| c == 1));
        assert!(patch.contains("@@ -10,2 +10,3 @@\n"));
        assert!(!patch.contains("TWO"));
        assert_eq!(build_patch(&raw, Direction::Forward, &|_, _| false), None);
    }

    #[test]
    fn half_a_new_file_taken_back_is_an_edit() {
        let diff = b"diff --git a/n.txt b/n.txt\nnew file mode 100644\nindex 0000000..3333333\n--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1,3 @@\n+a\n+b\n+c\n";
        let raw = parse_raw(diff);
        assert!(raw.new_file);
        let forward = text(build_patch(&raw, Direction::Forward, &|_, l| l == 1));
        assert_eq!(
            forward,
            "diff --git a/n.txt b/n.txt\nnew file mode 100644\n--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1,1 @@\n+b\n"
        );
        let reverse = text(build_patch(&raw, Direction::Reverse, &|_, l| l == 1));
        assert_eq!(
            reverse,
            "--- b/n.txt\n+++ b/n.txt\n@@ -1,2 +1,3 @@\n a\n+b\n c\n"
        );
    }

    #[test]
    fn half_a_deletion_is_an_edit() {
        let diff = b"diff --git a/d.txt b/d.txt\ndeleted file mode 100644\nindex 3333333..0000000\n--- a/d.txt\n+++ /dev/null\n@@ -1,3 +0,0 @@\n-a\n-b\n-c\n";
        let raw = parse_raw(diff);
        assert!(raw.deleted);
        let forward = text(build_patch(&raw, Direction::Forward, &|_, l| l == 0));
        assert_eq!(
            forward,
            "--- a/d.txt\n+++ a/d.txt\n@@ -1,3 +1,2 @@\n-a\n b\n c\n"
        );
        let reverse = text(build_patch(&raw, Direction::Reverse, &|_, l| l == 0));
        assert!(reverse.contains("deleted file mode 100644\n"));
        assert!(reverse.ends_with("@@ -1,1 +0,0 @@\n-a\n"));
    }

    #[test]
    fn status_v2_is_split_into_sides() {
        let out = "1 M. N... 100644 100644 100644 aaa bbb staged only.txt\x001 .M N... 100644 100644 100644 aaa aaa unstaged.txt\x001 MM N... 100644 100644 100644 aaa bbb both.txt\x002 R. N... 100644 100644 100644 aaa aaa R100 new name.txt\x00old name.txt\x00u UU N... 100644 100644 100644 100644 a b c fight.txt\x00? fresh file.txt\x001 D. N... 100644 000000 000000 aaa 000 gone.txt\x00";
        let parsed = parse_status_v2(out);
        let names = |list: &[StageEntry]| -> Vec<(String, ChangeKind)> {
            list.iter().map(|e| (e.path.clone(), e.kind)).collect()
        };
        assert_eq!(
            names(&parsed.staged),
            [
                ("both.txt".to_string(), ChangeKind::Modified),
                ("gone.txt".to_string(), ChangeKind::Deleted),
                ("new name.txt".to_string(), ChangeKind::Renamed),
                ("staged only.txt".to_string(), ChangeKind::Modified),
            ]
        );
        assert_eq!(parsed.staged[2].old_path.as_deref(), Some("old name.txt"));
        assert_eq!(
            names(&parsed.unstaged),
            [
                ("both.txt".to_string(), ChangeKind::Modified),
                ("fresh file.txt".to_string(), ChangeKind::Untracked),
                ("unstaged.txt".to_string(), ChangeKind::Modified),
            ]
        );
        assert_eq!(parsed.conflicted[0].path, "fight.txt");
    }
}
