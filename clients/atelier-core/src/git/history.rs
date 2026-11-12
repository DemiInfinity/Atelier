//! History: the commit graph of every branch (or one), laid out into lanes
//! here so a window only has to draw it; search by message, author, path
//! and id; one commit with its files; one file's history across renames.

use std::collections::HashSet;
use std::path::Path;

use serde::{Deserialize, Serialize};

use super::util::{check_path, is_sha, short, Call, FIELD, RECORD};
use super::{Git, RefLabel};
use crate::diff::{self, FileDiff, FileStatus};
use crate::error::{Error, Result};

/// Lines of one file's diff sent along; longer ones are cut.
const MAX_LINES: usize = 4000;
/// The most commits ever laid out for one page.
const MAX_WALK: usize = 50_000;
/// A file bigger than this isn't sent as text.
const MAX_FILE: usize = 1024 * 1024;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum EdgeKind {
    /// Straight through the row, top to bottom, in column `from`.
    Pass,
    /// From the top of column `from` into this row's commit.
    In,
    /// From this row's commit down to the bottom of column `to`.
    Out,
}

/// One line segment of the graph in one row.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
pub struct Edge {
    pub kind: EdgeKind,
    pub from: u32,
    pub to: u32,
    /// Which line of work it belongs to: the same number all the way down.
    pub color: u32,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct GraphRow {
    pub id: String,
    pub short: String,
    pub parents: Vec<String>,
    pub refs: Vec<RefLabel>,
    pub author: String,
    pub email: String,
    /// Author date, RFC 3339.
    pub time: String,
    pub subject: String,
    pub body: String,
    /// The column of this commit's dot.
    pub column: u32,
    pub color: u32,
    pub edges: Vec<Edge>,
    /// No remote has it yet.
    pub unpushed: bool,
    pub is_head: bool,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct GraphQuery {
    /// One branch (or any ref); every branch, remote and tag when `None`.
    #[serde(default)]
    pub branch: Option<String>,
    /// Found in the message, in the author, or the start of an id.
    #[serde(default)]
    pub text: Option<String>,
    #[serde(default)]
    pub author: Option<String>,
    /// Only commits that touched this file or folder.
    #[serde(default)]
    pub path: Option<String>,
    #[serde(default)]
    pub skip: usize,
    /// 0 means 200.
    #[serde(default)]
    pub limit: usize,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize)]
pub struct GraphPage {
    pub rows: Vec<GraphRow>,
    /// There are older commits after these.
    pub more: bool,
    /// The widest the graph gets on this page, in columns.
    pub columns: u32,
    /// A search or filter is on: the rows are a flat list, not a graph.
    pub filtered: bool,
    pub branch: Option<String>,
    pub head: Option<String>,
}

/// One file a commit touched.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct CommitFile {
    pub path: String,
    pub old_path: Option<String>,
    pub status: FileStatus,
    pub additions: Option<u32>,
    pub deletions: Option<u32>,
    pub binary: bool,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct CommitInfo {
    pub id: String,
    pub short: String,
    pub parents: Vec<String>,
    pub author: String,
    pub email: String,
    pub time: String,
    pub committer: String,
    pub subject: String,
    pub body: String,
    pub refs: Vec<RefLabel>,
    /// Against the first parent, for a merge.
    pub files: Vec<CommitFile>,
    /// Some remote has it.
    pub pushed: bool,
    pub is_head: bool,
    /// It is part of the branch that is checked out.
    pub on_current: bool,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct FileHistoryEntry {
    pub id: String,
    pub short: String,
    pub author: String,
    pub time: String,
    pub subject: String,
    /// The file's name in this commit.
    pub path: String,
    /// What it was called before this commit renamed it.
    pub old_path: Option<String>,
    pub status: FileStatus,
    pub additions: Option<u32>,
    pub deletions: Option<u32>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct FileHistory {
    pub path: String,
    pub entries: Vec<FileHistoryEntry>,
    pub more: bool,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct FileContent {
    pub path: String,
    pub rev: String,
    pub text: String,
    pub binary: bool,
    pub truncated: bool,
}

struct Lane {
    want: String,
    color: u32,
}

/// Gives every commit (newest first, parents after children) a column and
/// the line segments that cross its row. A branch keeps its column for as
/// long as it runs; columns freed by a merge are reused.
fn lay_out<'a>(
    commits: impl Iterator<Item = (&'a str, &'a [String])>,
) -> Vec<(u32, u32, Vec<Edge>)> {
    let mut lanes: Vec<Option<Lane>> = Vec::new();
    let mut next_color = 0u32;
    let mut rows = Vec::new();
    let free = |lanes: &mut Vec<Option<Lane>>| -> usize {
        match lanes.iter().position(Option::is_none) {
            Some(i) => i,
            None => {
                lanes.push(None);
                lanes.len() - 1
            }
        }
    };
    for (id, parents) in commits {
        let mine: Vec<usize> = lanes
            .iter()
            .enumerate()
            .filter(|(_, lane)| lane.as_ref().is_some_and(|l| l.want == id))
            .map(|(i, _)| i)
            .collect();
        let (column, color) = match mine.first() {
            Some(&i) => (i, lanes[i].as_ref().map_or(0, |l| l.color)),
            None => {
                let i = free(&mut lanes);
                next_color += 1;
                (i, next_color - 1)
            }
        };
        let mut edges = Vec::new();
        for (i, lane) in lanes.iter().enumerate() {
            let Some(lane) = lane else { continue };
            edges.push(Edge {
                kind: if lane.want == id {
                    EdgeKind::In
                } else {
                    EdgeKind::Pass
                },
                from: i as u32,
                to: if lane.want == id {
                    column as u32
                } else {
                    i as u32
                },
                color: lane.color,
            });
        }
        for i in mine {
            lanes[i] = None;
        }
        for (n, parent) in parents.iter().enumerate() {
            let waiting = lanes
                .iter()
                .position(|lane| lane.as_ref().is_some_and(|l| &l.want == parent));
            let (to, edge_color) = match waiting {
                // Another line already leads to this parent: join it.
                Some(j) => (j, lanes[j].as_ref().map_or(color, |l| l.color)),
                None if n == 0 => {
                    if lanes.len() <= column {
                        lanes.resize_with(column + 1, || None);
                    }
                    lanes[column] = Some(Lane {
                        want: parent.clone(),
                        color,
                    });
                    (column, color)
                }
                None => {
                    let j = free(&mut lanes);
                    next_color += 1;
                    lanes[j] = Some(Lane {
                        want: parent.clone(),
                        color: next_color - 1,
                    });
                    (j, next_color - 1)
                }
            };
            edges.push(Edge {
                kind: EdgeKind::Out,
                from: column as u32,
                to: to as u32,
                color: edge_color,
            });
        }
        while lanes.last().is_some_and(Option::is_none) {
            lanes.pop();
        }
        rows.push((column as u32, color, edges));
    }
    rows
}

struct Logged {
    id: String,
    parents: Vec<String>,
    author: String,
    email: String,
    time: String,
    committed: i64,
    subject: String,
    body: String,
    refs: Vec<RefLabel>,
}

fn log_format() -> String {
    format!(
        "--format=%H{FIELD}%P{FIELD}%an{FIELD}%ae{FIELD}%aI{FIELD}%ct{FIELD}%s{FIELD}%b{FIELD}%D{RECORD}"
    )
}

fn parse_log(out: &str) -> Vec<Logged> {
    out.split(RECORD)
        .filter_map(|record| {
            let mut parts = record.trim_start_matches(['\n', '\r']).split(FIELD);
            let id = parts.next().filter(|s| is_sha(s))?.to_string();
            Some(Logged {
                id,
                parents: parts
                    .next()?
                    .split_whitespace()
                    .map(str::to_string)
                    .collect(),
                author: parts.next()?.to_string(),
                email: parts.next()?.to_string(),
                time: parts.next()?.to_string(),
                committed: parts.next()?.parse().unwrap_or(0),
                subject: parts.next()?.to_string(),
                body: parts.next()?.trim().to_string(),
                refs: super::parse_decoration(parts.next().unwrap_or_default()),
            })
        })
        .collect()
}

/// `status\0path\0` and, for renames and copies, `R100\0old\0new\0`.
fn parse_name_status(out: &str) -> Vec<(FileStatus, String, Option<String>)> {
    let mut files = Vec::new();
    let mut parts = out.split('\0').map(|p| p.trim_start_matches('\n'));
    while let Some(status) = parts.next() {
        let status = match status.as_bytes().first() {
            Some(b'A') => FileStatus::Added,
            Some(b'D') => FileStatus::Deleted,
            Some(b'R') => FileStatus::Renamed,
            Some(b'C') => FileStatus::Copied,
            Some(_) => FileStatus::Modified,
            None => continue,
        };
        let Some(first) = parts.next() else { break };
        if matches!(status, FileStatus::Renamed | FileStatus::Copied) {
            let Some(new) = parts.next() else { break };
            files.push((status, new.to_string(), Some(first.to_string())));
        } else {
            files.push((status, first.to_string(), None));
        }
    }
    files
}

/// `a\td\tpath\0` and, for renames, `a\td\t\0old\0new\0`. Keyed by new path.
fn parse_numstat_renames(out: &str) -> Vec<(String, Option<u32>, Option<u32>)> {
    let mut counts = Vec::new();
    let mut parts = out.split('\0').map(|p| p.trim_start_matches('\n'));
    while let Some(entry) = parts.next() {
        let mut fields = entry.splitn(3, '\t');
        let (Some(a), Some(d), Some(path)) = (fields.next(), fields.next(), fields.next()) else {
            continue;
        };
        let path = if path.is_empty() {
            let _old = parts.next();
            match parts.next() {
                Some(new) => new.to_string(),
                None => break,
            }
        } else {
            path.to_string()
        };
        counts.push((path, a.parse().ok(), d.parse().ok()));
    }
    counts
}

impl Git {
    async fn graph_revs(&self, repo: &Path, branch: Option<&str>) -> Result<Vec<String>> {
        Ok(match branch.map(str::trim).filter(|b| !b.is_empty()) {
            Some(branch) => {
                self.resolve_commit(repo, branch).await?;
                vec![branch.to_string()]
            }
            None => vec![
                "--branches".to_string(),
                "--remotes".to_string(),
                "--tags".to_string(),
                "HEAD".to_string(),
            ],
        })
    }

    async fn log_rows(
        &self,
        repo: &Path,
        options: &[String],
        revs: &[String],
        path: Option<&str>,
    ) -> Result<Vec<Logged>> {
        let mut args: Vec<String> = vec![
            "-c".into(),
            "core.quotePath=false".into(),
            "log".into(),
            "--no-color".into(),
            "--decorate=full".into(),
            "--decorate-refs-exclude=refs/atelier".into(),
            log_format(),
        ];
        args.extend(options.iter().cloned());
        args.extend(revs.iter().cloned());
        args.push("--".into());
        args.extend(path.map(str::to_string));
        let out = self.exec(repo, &args, Call::default().literal()).await?;
        Ok(parse_log(&out.into_result()?.text()))
    }

    /// One page of history, newest first. Without a search it is a graph:
    /// each row has its column and the segments that cross it. With a
    /// search (`text`, `author` or `path`) it is a flat list of matches.
    pub async fn graph(&self, repo: &Path, query: &GraphQuery) -> Result<GraphPage> {
        let head = self.head_sha(repo).await?;
        let mut page = GraphPage {
            branch: self.current_branch(repo).await?,
            head: head.clone(),
            ..GraphPage::default()
        };
        if head.is_none() {
            return Ok(page);
        }
        let limit = if query.limit == 0 { 200 } else { query.limit }.min(2000);
        let walk = (query.skip + limit + 1).min(MAX_WALK);
        let revs = self.graph_revs(repo, query.branch.as_deref()).await?;
        let text = query
            .text
            .as_deref()
            .map(str::trim)
            .filter(|t| !t.is_empty());
        let author = query
            .author
            .as_deref()
            .map(str::trim)
            .filter(|a| !a.is_empty());
        let path = match query
            .path
            .as_deref()
            .map(str::trim)
            .filter(|p| !p.is_empty())
        {
            Some(path) => Some(check_path(path.trim_end_matches('/'))?),
            None => None,
        };
        page.filtered = text.is_some() || author.is_some() || path.is_some();
        let count = format!("--max-count={walk}");

        let mut logged = if page.filtered {
            let search = |extra: Vec<String>| {
                let mut options = vec![
                    count.clone(),
                    "--date-order".to_string(),
                    "--regexp-ignore-case".to_string(),
                    "--fixed-strings".to_string(),
                ];
                options.extend(extra);
                options
            };
            let mut by_message = Vec::new();
            by_message.extend(author.map(|a| format!("--author={a}")));
            by_message.extend(text.map(|t| format!("--grep={t}")));
            let mut found = self
                .log_rows(repo, &search(by_message), &revs, path)
                .await?;
            if let (Some(text), None) = (text, author) {
                // The same words may be someone's name, or the start of an id.
                found.extend(
                    self.log_rows(repo, &search(vec![format!("--author={text}")]), &revs, path)
                        .await?,
                );
                if is_sha(text) {
                    if let Ok(id) = self.resolve_commit(repo, text).await {
                        let one = vec!["--max-count=1".to_string()];
                        found.extend(self.log_rows(repo, &one, &[id], None).await?);
                    }
                }
            }
            let mut seen = HashSet::new();
            found.retain(|c| seen.insert(c.id.clone()));
            found.sort_by_key(|c| std::cmp::Reverse(c.committed));
            found
        } else {
            let options = vec![count, "--topo-order".to_string()];
            self.log_rows(repo, &options, &revs, None).await?
        };

        let layout = if page.filtered {
            Vec::new()
        } else {
            lay_out(logged.iter().map(|c| (c.id.as_str(), c.parents.as_slice())))
        };
        page.more = logged.len() > query.skip + limit;
        let mut unpushed_args: Vec<String> = vec!["rev-list".into(), "--max-count=20000".into()];
        unpushed_args.extend(revs.iter().cloned());
        unpushed_args.extend(["--not".to_string(), "--remotes".to_string()]);
        let unpushed = self.text(repo, &unpushed_args).await.unwrap_or_default();
        let unpushed: HashSet<&str> = unpushed.lines().collect();

        let rows: Vec<GraphRow> = logged
            .drain(..)
            .enumerate()
            .skip(query.skip)
            .take(limit)
            .map(|(i, c)| {
                let (column, color, edges) = layout.get(i).cloned().unwrap_or((0, 0, Vec::new()));
                GraphRow {
                    short: short(&c.id),
                    unpushed: unpushed.contains(c.id.as_str()),
                    is_head: head.as_deref() == Some(c.id.as_str()),
                    id: c.id,
                    parents: c.parents,
                    refs: c.refs,
                    author: c.author,
                    email: c.email,
                    time: c.time,
                    subject: c.subject,
                    body: c.body,
                    column,
                    color,
                    edges,
                }
            })
            .collect();
        page.columns = rows
            .iter()
            .flat_map(|r| {
                r.edges
                    .iter()
                    .flat_map(|e| [e.from, e.to])
                    .chain([r.column])
            })
            .max()
            .map_or(0, |c| c + 1);
        page.rows = rows;
        Ok(page)
    }

    /// One commit: who, when, the message, and the files it touched.
    pub async fn commit_info(&self, repo: &Path, rev: &str) -> Result<CommitInfo> {
        let id = self.resolve_commit(repo, rev).await?;
        let format = format!(
            "--format=%P{FIELD}%an{FIELD}%ae{FIELD}%aI{FIELD}%cn{FIELD}%s{FIELD}%b{FIELD}%D"
        );
        let out = self
            .text(
                repo,
                &[
                    "log",
                    "-1",
                    "--no-color",
                    "--decorate=full",
                    "--decorate-refs-exclude=refs/atelier",
                    &format,
                    &id,
                ],
            )
            .await?;
        let mut parts = out.trim_end().split(FIELD);
        let mut next = || parts.next().unwrap_or_default().to_string();
        let (parents, author, email, time, committer, subject, body, refs) = (
            next(),
            next(),
            next(),
            next(),
            next(),
            next(),
            next(),
            next(),
        );
        let show = |what: &'static str| {
            let id = id.clone();
            async move {
                self.text(
                    repo,
                    &[
                        "-c",
                        "core.quotePath=false",
                        "show",
                        "--no-color",
                        "--format=",
                        what,
                        "-z",
                        "-M",
                        "--diff-merges=first-parent",
                        &id,
                    ],
                )
                .await
            }
        };
        let names = parse_name_status(&show("--name-status").await?);
        let counts = parse_numstat_renames(&show("--numstat").await?);
        let files = names
            .into_iter()
            .map(|(status, path, old_path)| {
                let found = counts.iter().find(|(p, _, _)| p == &path);
                let (additions, deletions) = found.map_or((None, None), |(_, a, d)| (*a, *d));
                CommitFile {
                    binary: found.is_some() && additions.is_none(),
                    path,
                    old_path,
                    status,
                    additions,
                    deletions,
                }
            })
            .collect();
        let head = self.head_sha(repo).await?;
        let on_current = self
            .exec(
                repo,
                &["merge-base", "--is-ancestor", &id, "HEAD"],
                Call::default(),
            )
            .await?
            .ok();
        Ok(CommitInfo {
            short: short(&id),
            parents: parents.split_whitespace().map(str::to_string).collect(),
            author,
            email,
            time,
            committer,
            subject,
            body: body.trim().to_string(),
            refs: super::parse_decoration(&refs),
            files,
            pushed: self.is_pushed(repo, &id).await?,
            is_head: head.as_deref() == Some(id.as_str()),
            on_current,
            id,
        })
    }

    /// What one commit did to one file (against its first parent).
    /// `old_path` is the file's earlier name when the commit renamed it.
    pub async fn commit_file_diff(
        &self,
        repo: &Path,
        rev: &str,
        path: &str,
        old_path: Option<&str>,
    ) -> Result<FileDiff> {
        let id = self.resolve_commit(repo, rev).await?;
        check_path(path)?;
        let mut args = vec![
            "-c",
            "core.quotePath=false",
            "show",
            "--no-color",
            "--no-ext-diff",
            "--format=",
            "-M",
            "--diff-merges=first-parent",
            id.as_str(),
            "--",
        ];
        if let Some(old) = old_path.filter(|o| !o.is_empty()) {
            args.push(check_path(old)?);
        }
        args.push(path);
        let out = self.exec(repo, &args, Call::default().literal()).await?;
        diff::parse_limited(&out.into_result()?.text(), MAX_LINES)
            .into_iter()
            .next()
            .ok_or_else(|| Error::Invalid(format!("{} didn’t change {path}.", short(&id))))
    }

    /// The commits that changed one file, newest first, following it
    /// through renames. `from` is where to start looking (HEAD by default).
    pub async fn file_history(
        &self,
        repo: &Path,
        path: &str,
        from: Option<&str>,
        limit: usize,
    ) -> Result<FileHistory> {
        check_path(path)?;
        let start = match from.map(str::trim).filter(|f| !f.is_empty()) {
            Some(rev) => self.resolve_commit(repo, rev).await?,
            None => "HEAD".to_string(),
        };
        let limit = if limit == 0 { 200 } else { limit }.min(2000);
        let format = format!("--format={RECORD}%H{FIELD}%an{FIELD}%aI{FIELD}%s{FIELD}");
        let count = format!("--max-count={}", limit + 1);
        let out = self
            .exec(
                repo,
                &[
                    "-c",
                    "core.quotePath=false",
                    "log",
                    "--follow",
                    "--no-color",
                    "-M",
                    "--raw",
                    "--numstat",
                    "--no-abbrev",
                    &format,
                    &count,
                    &start,
                    "--",
                    path,
                ],
                Call::default().literal(),
            )
            .await?
            .into_result()?
            .text();
        let mut entries: Vec<FileHistoryEntry> = Vec::new();
        for record in out.split(RECORD).filter(|r| !r.trim().is_empty()) {
            let mut lines = record.lines();
            let mut head = lines.next().unwrap_or_default().split(FIELD);
            let Some(id) = head.next().filter(|s| is_sha(s)) else {
                continue;
            };
            let mut entry = FileHistoryEntry {
                id: id.to_string(),
                short: short(id),
                author: head.next().unwrap_or_default().to_string(),
                time: head.next().unwrap_or_default().to_string(),
                subject: head.next().unwrap_or_default().to_string(),
                path: path.to_string(),
                old_path: None,
                status: FileStatus::Modified,
                additions: None,
                deletions: None,
            };
            for line in lines {
                if let Some(raw) = line.strip_prefix(':') {
                    // `mode mode sha sha R100<TAB>old<TAB>new`
                    let mut fields = raw.split('\t');
                    let status = fields
                        .next()
                        .and_then(|meta| meta.split(' ').nth(4))
                        .unwrap_or_default();
                    let first = fields.next().map(diff::unquote);
                    let second = fields.next().map(diff::unquote);
                    entry.status = match status.as_bytes().first() {
                        Some(b'A') => FileStatus::Added,
                        Some(b'D') => FileStatus::Deleted,
                        Some(b'R') => FileStatus::Renamed,
                        Some(b'C') => FileStatus::Copied,
                        _ => FileStatus::Modified,
                    };
                    match (first, second) {
                        (Some(old), Some(new)) => {
                            entry.path = new;
                            entry.old_path = Some(old);
                        }
                        (Some(only), None) => entry.path = only,
                        _ => {}
                    }
                } else {
                    let mut fields = line.split('\t');
                    if let (Some(a), Some(d), Some(_)) =
                        (fields.next(), fields.next(), fields.next())
                    {
                        if a == "-" || a.parse::<u32>().is_ok() {
                            entry.additions = a.parse().ok();
                            entry.deletions = d.parse().ok();
                        }
                    }
                }
            }
            entries.push(entry);
        }
        let more = entries.len() > limit;
        entries.truncate(limit);
        Ok(FileHistory {
            path: path.to_string(),
            entries,
            more,
        })
    }

    /// One file as it was at a commit.
    pub async fn file_at(&self, repo: &Path, rev: &str, path: &str) -> Result<FileContent> {
        let id = self.resolve_commit(repo, rev).await?;
        check_path(path)?;
        let spec = format!("{id}:{path}");
        let out = self.exec(repo, &["show", &spec], Call::default()).await?;
        if !out.ok() {
            return Err(Error::Invalid(format!(
                "{path} doesn’t exist at {}.",
                short(&id)
            )));
        }
        let binary = out.stdout.contains(&0);
        let truncated = out.stdout.len() > MAX_FILE;
        let bytes = &out.stdout[..out.stdout.len().min(MAX_FILE)];
        Ok(FileContent {
            path: path.to_string(),
            rev: id,
            text: if binary {
                String::new()
            } else {
                String::from_utf8_lossy(bytes).into_owned()
            },
            binary,
            truncated,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn rows(commits: &[(&str, &[&str])]) -> Vec<(u32, u32, Vec<Edge>)> {
        let owned: Vec<(String, Vec<String>)> = commits
            .iter()
            .map(|(id, parents)| {
                (
                    id.to_string(),
                    parents.iter().map(|p| p.to_string()).collect(),
                )
            })
            .collect();
        lay_out(owned.iter().map(|(id, p)| (id.as_str(), p.as_slice())))
    }

    #[test]
    fn a_straight_line_stays_in_one_column() {
        let out = rows(&[("c", &["b"]), ("b", &["a"]), ("a", &[])]);
        assert!(out
            .iter()
            .all(|(column, color, _)| (*column, *color) == (0, 0)));
        // The tip only runs down, the middle comes in and goes out, the root only comes in.
        let kinds = |i: usize| -> Vec<EdgeKind> { out[i].2.iter().map(|e| e.kind).collect() };
        assert_eq!(kinds(0), [EdgeKind::Out]);
        assert_eq!(kinds(1), [EdgeKind::In, EdgeKind::Out]);
        assert_eq!(kinds(2), [EdgeKind::In]);
    }

    #[test]
    fn a_merge_opens_a_lane_and_the_fork_point_closes_it() {
        // m merges side branch s into main line: m(a2,s1) a2(a1) s1(a1) a1()
        let out = rows(&[
            ("m", &["a2", "s1"]),
            ("a2", &["a1"]),
            ("s1", &["a1"]),
            ("a1", &[]),
        ]);
        let columns: Vec<u32> = out.iter().map(|r| r.0).collect();
        assert_eq!(columns, [0, 0, 1, 0]);
        // The merge sends one line down its own column and one into column 1.
        assert_eq!(
            out[0].2,
            [
                Edge {
                    kind: EdgeKind::Out,
                    from: 0,
                    to: 0,
                    color: 0
                },
                Edge {
                    kind: EdgeKind::Out,
                    from: 0,
                    to: 1,
                    color: 1
                },
            ]
        );
        // a2's row: its own line in and out, the side lane passing by.
        assert!(out[1].2.contains(&Edge {
            kind: EdgeKind::Pass,
            from: 1,
            to: 1,
            color: 1
        }));
        // s1 joins the lane that already leads to a1.
        assert!(out[2].2.contains(&Edge {
            kind: EdgeKind::Out,
            from: 1,
            to: 0,
            color: 0
        }));
        // a1 takes one line in, from column 0.
        assert_eq!(
            out[3].2,
            [Edge {
                kind: EdgeKind::In,
                from: 0,
                to: 0,
                color: 0
            }]
        );
        // Colours follow the line of work.
        assert_eq!(out[2].1, 1);
    }

    #[test]
    fn a_second_tip_gets_its_own_column_and_joins_the_base() {
        let out = rows(&[
            ("x2", &["x1"]),
            ("x1", &["base"]),
            ("y1", &["base"]),
            ("base", &[]),
        ]);
        let columns: Vec<u32> = out.iter().map(|r| r.0).collect();
        assert_eq!(columns, [0, 0, 1, 0]);
        assert!(out[2].2.contains(&Edge {
            kind: EdgeKind::Pass,
            from: 0,
            to: 0,
            color: 0
        }));
        assert!(out[2].2.contains(&Edge {
            kind: EdgeKind::Out,
            from: 1,
            to: 0,
            color: 0
        }));
        assert_eq!(out[2].1, 1);
    }

    #[test]
    fn name_status_and_numstat_with_renames() {
        let names = parse_name_status("M\0a.txt\0R096\0old name.go\0new name.go\0A\0c.bin\0");
        assert_eq!(names[0], (FileStatus::Modified, "a.txt".to_string(), None));
        assert_eq!(
            names[1],
            (
                FileStatus::Renamed,
                "new name.go".to_string(),
                Some("old name.go".to_string())
            )
        );
        let counts =
            parse_numstat_renames("3\t1\ta.txt\x002\t0\t\0old name.go\0new name.go\0-\t-\tc.bin\0");
        assert_eq!(counts[0], ("a.txt".to_string(), Some(3), Some(1)));
        assert_eq!(counts[1], ("new name.go".to_string(), Some(2), Some(0)));
        assert_eq!(counts[2], ("c.bin".to_string(), None, None));
    }
}
