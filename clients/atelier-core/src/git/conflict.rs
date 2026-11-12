//! Resolving conflicts: which files need a person, each text file as
//! aligned regions (common, or a conflict with your side, their side and
//! the base), and choosing per region or per file.
//!
//! The three sides come from the index (stages 1, 2 and 3), never from
//! whatever is on disk; `git merge-file` lines them up. "Yours" is always
//! the work of the person at the keyboard: during a rebase that is stage 3
//! (the commit being replayed), in every other case stage 2.

use std::collections::{BTreeMap, BTreeSet};
use std::path::Path;

use serde::{Deserialize, Serialize};

use super::integrate::{OperationKind, OperationState};
use super::util::{check_path, unique, Call};
use super::Git;
use crate::error::{Error, Result};

/// The length of the markers asked of `git merge-file`: long enough that
/// no real line of code starts like one.
const MARK: usize = 23;

/// What to put where a conflict was.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Choice {
    Yours,
    Theirs,
    /// Both sides, one after the other.
    Both {
        yours_first: bool,
    },
    /// Whatever was typed.
    Custom {
        text: String,
    },
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TakeSide {
    Yours,
    Theirs,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConflictKind {
    BothModified,
    BothAdded,
    /// You deleted the file, they changed it.
    DeletedByYou,
    /// They deleted the file, you changed it.
    DeletedByThem,
    AddedByYou,
    AddedByThem,
    BothDeleted,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct ConflictFile {
    pub path: String,
    pub kind: ConflictKind,
    pub binary: bool,
    pub resolved: bool,
    /// Only a whole-file choice is possible (binary, or one side deleted it).
    pub whole_file_only: bool,
    /// Conflicts in the file, and how many of them are chosen.
    pub regions: u32,
    pub chosen: u32,
    /// How a resolved file was resolved: `yours`, `theirs`, `mixed`, `edited`.
    pub how: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct ConflictList {
    pub state: OperationState,
    pub needs_you: Vec<ConflictFile>,
    /// Files the other side changed that merged without help.
    pub auto_resolved: Vec<String>,
    pub done: u32,
    pub total: u32,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Region {
    /// The same on both sides.
    Common {
        lines: Vec<String>,
        /// The first line's number on each side.
        yours_start: u32,
        theirs_start: u32,
    },
    Conflict {
        /// Counts conflicts only, from 0.
        index: u32,
        yours: Vec<String>,
        theirs: Vec<String>,
        /// What both started from; `None` when the file is new on both sides.
        base: Option<Vec<String>>,
        yours_start: u32,
        theirs_start: u32,
        choice: Option<Choice>,
    },
}

/// One line of the file as it will be.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct ResultLine {
    pub text: String,
    /// `common`, `yours`, `theirs`, `custom`, or `undecided` (a placeholder
    /// for a conflict that isn't chosen yet).
    pub from: String,
    pub region: Option<u32>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct ConflictDetail {
    pub path: String,
    pub kind: ConflictKind,
    pub binary: bool,
    pub resolved: bool,
    pub whole_file_only: bool,
    pub yours_label: String,
    pub theirs_label: String,
    pub regions: Vec<Region>,
    pub total: u32,
    pub chosen: u32,
    pub result: Vec<ResultLine>,
}

/// The blob ids of a conflicted file's stages 1, 2 and 3.
type Stages = [Option<String>; 3];

#[derive(Debug, PartialEq, Eq)]
enum Segment {
    Common(Vec<Vec<u8>>),
    Conflict {
        yours: Vec<Vec<u8>>,
        base: Vec<Vec<u8>>,
        theirs: Vec<Vec<u8>>,
    },
}

fn is_marker(line: &[u8], ch: u8) -> bool {
    line.len() >= MARK
        && line[..MARK].iter().all(|b| *b == ch)
        && matches!(line.get(MARK), None | Some(b' ' | b'\n' | b'\r'))
}

/// Cuts `git merge-file --diff3` output into common runs and conflicts.
fn segments(merged: &[u8]) -> Vec<Segment> {
    #[derive(PartialEq)]
    enum At {
        Common,
        Yours,
        Base,
        Theirs,
    }
    let mut out: Vec<Segment> = Vec::new();
    let mut at = At::Common;
    let mut common: Vec<Vec<u8>> = Vec::new();
    let (mut yours, mut base, mut theirs) = (Vec::new(), Vec::new(), Vec::new());
    for line in merged.split_inclusive(|b| *b == b'\n') {
        match at {
            At::Common if is_marker(line, b'<') => {
                if !common.is_empty() {
                    out.push(Segment::Common(std::mem::take(&mut common)));
                }
                at = At::Yours;
            }
            At::Common => common.push(line.to_vec()),
            At::Yours if is_marker(line, b'|') => at = At::Base,
            At::Yours if is_marker(line, b'=') => at = At::Theirs,
            At::Yours => yours.push(line.to_vec()),
            At::Base if is_marker(line, b'=') => at = At::Theirs,
            At::Base => base.push(line.to_vec()),
            At::Theirs if is_marker(line, b'>') => {
                out.push(Segment::Conflict {
                    yours: std::mem::take(&mut yours),
                    base: std::mem::take(&mut base),
                    theirs: std::mem::take(&mut theirs),
                });
                at = At::Common;
            }
            At::Theirs => theirs.push(line.to_vec()),
        }
    }
    if !common.is_empty() {
        out.push(Segment::Common(common));
    }
    out
}

fn shown(lines: &[Vec<u8>]) -> Vec<String> {
    lines
        .iter()
        .map(|line| {
            let line = line.strip_suffix(b"\n").unwrap_or(line);
            let line = line.strip_suffix(b"\r").unwrap_or(line);
            String::from_utf8_lossy(line).into_owned()
        })
        .collect()
}

fn push_lines(out: &mut Vec<u8>, lines: &[Vec<u8>]) {
    for line in lines {
        if out.last().is_some_and(|b| *b != b'\n') {
            out.push(b'\n');
        }
        out.extend_from_slice(line);
    }
}

fn custom_bytes(text: &str, crlf: bool) -> Vec<u8> {
    let mut text = text.replace("\r\n", "\n");
    if !text.is_empty() && !text.ends_with('\n') {
        text.push('\n');
    }
    if crlf {
        text = text.replace('\n', "\r\n");
    }
    text.into_bytes()
}

/// The file with every chosen conflict replaced by its choice. Conflicts
/// not chosen yet are written the way git writes them.
fn render(
    segments: &[Segment],
    choices: &[Option<Choice>],
    yours_label: &str,
    theirs_label: &str,
) -> Vec<u8> {
    let crlf = segments.iter().any(|s| match s {
        Segment::Common(lines) => lines.iter().any(|l| l.ends_with(b"\r\n")),
        Segment::Conflict { yours, .. } => yours.iter().any(|l| l.ends_with(b"\r\n")),
    });
    let eol: &[u8] = if crlf { b"\r\n" } else { b"\n" };
    let mut out: Vec<u8> = Vec::new();
    let mut n = 0;
    for segment in segments {
        match segment {
            Segment::Common(lines) => push_lines(&mut out, lines),
            Segment::Conflict {
                yours,
                base,
                theirs,
            } => {
                match choices.get(n).and_then(Option::as_ref) {
                    Some(Choice::Yours) => push_lines(&mut out, yours),
                    Some(Choice::Theirs) => push_lines(&mut out, theirs),
                    Some(Choice::Both { yours_first: true }) => {
                        push_lines(&mut out, yours);
                        push_lines(&mut out, theirs);
                    }
                    Some(Choice::Both { yours_first: false }) => {
                        push_lines(&mut out, theirs);
                        push_lines(&mut out, yours);
                    }
                    Some(Choice::Custom { text }) => {
                        push_lines(&mut out, &[custom_bytes(text, crlf)]);
                    }
                    None => {
                        let mark = |out: &mut Vec<u8>, ch: u8, label: &str| {
                            if out.last().is_some_and(|b| *b != b'\n') {
                                out.extend_from_slice(eol);
                            }
                            out.extend(std::iter::repeat_n(ch, 7));
                            if !label.is_empty() {
                                out.push(b' ');
                                out.extend_from_slice(label.as_bytes());
                            }
                            out.extend_from_slice(eol);
                        };
                        mark(&mut out, b'<', yours_label);
                        push_lines(&mut out, yours);
                        mark(&mut out, b'|', "base");
                        push_lines(&mut out, base);
                        mark(&mut out, b'=', "");
                        push_lines(&mut out, theirs);
                        mark(&mut out, b'>', theirs_label);
                    }
                }
                n += 1;
            }
        }
    }
    out
}

fn regions_of(segments: &[Segment], choices: &[Option<Choice>], has_base: bool) -> Vec<Region> {
    let (mut yours_at, mut theirs_at) = (1u32, 1u32);
    let mut n = 0u32;
    segments
        .iter()
        .map(|segment| match segment {
            Segment::Common(lines) => {
                let region = Region::Common {
                    lines: shown(lines),
                    yours_start: yours_at,
                    theirs_start: theirs_at,
                };
                yours_at += lines.len() as u32;
                theirs_at += lines.len() as u32;
                region
            }
            Segment::Conflict {
                yours,
                base,
                theirs,
            } => {
                let region = Region::Conflict {
                    index: n,
                    yours: shown(yours),
                    theirs: shown(theirs),
                    base: has_base.then(|| shown(base)),
                    yours_start: yours_at,
                    theirs_start: theirs_at,
                    choice: choices.get(n as usize).cloned().flatten(),
                };
                yours_at += yours.len() as u32;
                theirs_at += theirs.len() as u32;
                n += 1;
                region
            }
        })
        .collect()
}

fn result_of(segments: &[Segment], choices: &[Option<Choice>]) -> Vec<ResultLine> {
    let mut out = Vec::new();
    let mut n = 0u32;
    let add = |out: &mut Vec<ResultLine>, lines: Vec<String>, from: &str, region| {
        out.extend(lines.into_iter().map(|text| ResultLine {
            text,
            from: from.to_string(),
            region,
        }));
    };
    for segment in segments {
        match segment {
            Segment::Common(lines) => add(&mut out, shown(lines), "common", None),
            Segment::Conflict { yours, theirs, .. } => {
                let region = Some(n);
                match choices.get(n as usize).and_then(Option::as_ref) {
                    Some(Choice::Yours) => add(&mut out, shown(yours), "yours", region),
                    Some(Choice::Theirs) => add(&mut out, shown(theirs), "theirs", region),
                    Some(Choice::Both { yours_first }) => {
                        let (first, second) = if *yours_first {
                            ((yours, "yours"), (theirs, "theirs"))
                        } else {
                            ((theirs, "theirs"), (yours, "yours"))
                        };
                        add(&mut out, shown(first.0), first.1, region);
                        add(&mut out, shown(second.0), second.1, region);
                    }
                    Some(Choice::Custom { text }) => add(
                        &mut out,
                        text.lines().map(str::to_string).collect(),
                        "custom",
                        region,
                    ),
                    None => add(&mut out, vec![String::new()], "undecided", region),
                }
                n += 1;
            }
        }
    }
    out
}

fn how_of(choices: &[Option<Choice>]) -> &'static str {
    if choices.iter().all(|c| c == &Some(Choice::Yours)) {
        "yours"
    } else if choices.iter().all(|c| c == &Some(Choice::Theirs)) {
        "theirs"
    } else {
        "mixed"
    }
}

fn kind_of(stages: &Stages, swapped: bool) -> ConflictKind {
    // "You" are stage 2, except in a rebase.
    let (base, mut you, mut they) = (
        stages[0].is_some(),
        stages[1].is_some(),
        stages[2].is_some(),
    );
    if swapped {
        std::mem::swap(&mut you, &mut they);
    }
    match (base, you, they) {
        (true, true, true) => ConflictKind::BothModified,
        (false, true, true) => ConflictKind::BothAdded,
        (true, false, true) => ConflictKind::DeletedByYou,
        (true, true, false) => ConflictKind::DeletedByThem,
        (false, true, false) => ConflictKind::AddedByYou,
        (false, false, true) => ConflictKind::AddedByThem,
        _ => ConflictKind::BothDeleted,
    }
}

impl Git {
    /// The stages of every conflicted file.
    async fn unmerged_stages(&self, repo: &Path) -> Result<BTreeMap<String, Stages>> {
        let out = self.text(repo, &["ls-files", "--unmerged", "-z"]).await?;
        let mut map: BTreeMap<String, Stages> = BTreeMap::new();
        for record in out.split('\0').filter(|r| !r.is_empty()) {
            let Some((meta, path)) = record.split_once('\t') else {
                continue;
            };
            let mut parts = meta.split(' ');
            let (_, Some(sha), Some(stage)) = (parts.next(), parts.next(), parts.next()) else {
                continue;
            };
            if let Some(slot) = stage.parse::<usize>().ok().filter(|n| (1..=3).contains(n)) {
                map.entry(path.to_string()).or_default()[slot - 1] = Some(sha.to_string());
            }
        }
        Ok(map)
    }

    async fn blob(&self, repo: &Path, sha: &str) -> Result<Vec<u8>> {
        Ok(self
            .exec(repo, &["cat-file", "blob", sha], Call::default())
            .await?
            .into_result()?
            .stdout)
    }

    /// The three sides of a text conflict, lined up. `None` when the file
    /// can only be taken whole (a side is missing, or it is binary).
    async fn line_up(
        &self,
        repo: &Path,
        stages: &Stages,
        swapped: bool,
    ) -> Result<Option<Vec<Segment>>> {
        let (Some(two), Some(three)) = (&stages[1], &stages[2]) else {
            return Ok(None);
        };
        let (yours, theirs) = if swapped { (three, two) } else { (two, three) };
        let yours = self.blob(repo, yours).await?;
        let theirs = self.blob(repo, theirs).await?;
        let base = match &stages[0] {
            Some(sha) => self.blob(repo, sha).await?,
            None => Vec::new(),
        };
        if [&yours, &theirs, &base].iter().any(|b| b.contains(&0)) {
            return Ok(None);
        }
        let dir = self.app_dir_local(repo).await?.join(unique("merge"));
        std::fs::create_dir_all(&dir)
            .map_err(|e| Error::io(format!("Couldn’t create {}", dir.display()), e))?;
        let result = async {
            for (name, bytes) in [("yours", &yours), ("base", &base), ("theirs", &theirs)] {
                std::fs::write(dir.join(name), bytes)
                    .map_err(|e| Error::io("Couldn’t write a scratch file", e))?;
            }
            let size = format!("--marker-size={MARK}");
            let (a, b, c) = (dir.join("yours"), dir.join("base"), dir.join("theirs"));
            let args: Vec<&std::ffi::OsStr> = vec![
                "merge-file".as_ref(),
                "-p".as_ref(),
                "--diff3".as_ref(),
                size.as_ref(),
                a.as_os_str(),
                b.as_os_str(),
                c.as_os_str(),
            ];
            let out = self.exec(repo, &args, Call::default()).await?;
            // The exit code is the number of conflicts; 255 is a failure.
            if !(0..=127).contains(&out.code) {
                return Err(out.error());
            }
            Ok(segments(&out.stdout))
        }
        .await;
        let _ = std::fs::remove_dir_all(&dir);
        result.map(Some)
    }

    /// The files of the conflict that is under way: those that need a
    /// person (resolved or not) and those that merged by themselves.
    pub async fn conflict_list(&self, repo: &Path) -> Result<ConflictList> {
        let state = self.operation_state(repo).await?;
        if state.kind == OperationKind::None {
            return Ok(ConflictList {
                state,
                needs_you: Vec::new(),
                auto_resolved: Vec::new(),
                done: 0,
                total: 0,
            });
        }
        let swapped = state.kind == OperationKind::Rebase;
        let mut marker = self.marker_current(repo, state.kind).await?;
        let stages = self.unmerged_stages(repo).await?;
        let paths: BTreeSet<String> = marker
            .paths
            .iter()
            .cloned()
            .chain(stages.keys().cloned())
            .collect();
        let mut needs_you = Vec::new();
        let mut changed = false;
        for path in &paths {
            let file = match stages.get(path) {
                Some(found) => {
                    let kind = kind_of(found, swapped);
                    if marker.kinds.get(path) != Some(&kind) {
                        marker.kinds.insert(path.clone(), kind);
                        changed = true;
                    }
                    let lined = self.line_up(repo, found, swapped).await?;
                    let binary = lined.is_none() && found[1].is_some() && found[2].is_some();
                    let regions = lined
                        .as_ref()
                        .map(|s| {
                            s.iter()
                                .filter(|s| matches!(s, Segment::Conflict { .. }))
                                .count() as u32
                        })
                        .unwrap_or(0);
                    let chosen = marker
                        .choices
                        .get(path)
                        .filter(|c| c.len() as u32 == regions)
                        .map(|c| c.iter().filter(|c| c.is_some()).count() as u32)
                        .unwrap_or(0);
                    ConflictFile {
                        path: path.clone(),
                        kind,
                        binary,
                        resolved: false,
                        whole_file_only: lined.is_none(),
                        regions,
                        chosen,
                        how: None,
                    }
                }
                None => {
                    let regions = marker.choices.get(path).map_or(0, |c| c.len() as u32);
                    ConflictFile {
                        path: path.clone(),
                        kind: marker
                            .kinds
                            .get(path)
                            .copied()
                            .unwrap_or(ConflictKind::BothModified),
                        binary: false,
                        resolved: true,
                        whole_file_only: false,
                        regions,
                        chosen: regions,
                        how: Some(
                            marker
                                .resolved
                                .get(path)
                                .cloned()
                                .unwrap_or_else(|| "edited".to_string()),
                        ),
                    }
                }
            };
            needs_you.push(file);
        }
        if changed || marker.paths.len() != paths.len() {
            marker.paths = paths.iter().cloned().collect();
            self.marker_write(repo, &marker).await?;
        }
        let staged = if self.head_sha(repo).await?.is_some() {
            self.text(
                repo,
                &[
                    "diff",
                    "--cached",
                    "--name-only",
                    "-z",
                    "--no-renames",
                    "HEAD",
                ],
            )
            .await?
        } else {
            String::new()
        };
        let auto_resolved: Vec<String> = staged
            .split('\0')
            .filter(|p| !p.is_empty() && !paths.contains(*p))
            .map(str::to_string)
            .collect();
        let done = needs_you.iter().filter(|f| f.resolved).count() as u32;
        Ok(ConflictList {
            total: needs_you.len() as u32,
            done,
            state,
            needs_you,
            auto_resolved,
        })
    }

    async fn conflict_context(
        &self,
        repo: &Path,
        path: &str,
    ) -> Result<(OperationState, super::integrate::Marker, Option<Stages>)> {
        check_path(path)?;
        let state = self.operation_state(repo).await?;
        if state.kind == OperationKind::None {
            return Err(Error::Invalid(
                "There is no conflict under way here.".into(),
            ));
        }
        let marker = self.marker_current(repo, state.kind).await?;
        let stages = self.unmerged_stages(repo).await?.remove(path);
        if stages.is_none() && !marker.paths.iter().any(|p| p == path) {
            return Err(Error::Invalid(format!("{path} isn’t in conflict.")));
        }
        Ok((state, marker, stages))
    }

    fn labels(state: &OperationState) -> (String, String) {
        (
            state.yours.clone().unwrap_or_else(|| "yours".to_string()),
            state.theirs.clone().unwrap_or_else(|| "theirs".to_string()),
        )
    }

    /// One conflicted file: the two sides lined up, what was chosen so far
    /// and what the file will be.
    pub async fn conflict_file(&self, repo: &Path, path: &str) -> Result<ConflictDetail> {
        let (state, marker, stages) = self.conflict_context(repo, path).await?;
        let swapped = state.kind == OperationKind::Rebase;
        let (yours_label, theirs_label) = Self::labels(&state);
        let Some(stages) = stages else {
            // Resolved already: show the file as it is now.
            let bytes = std::fs::read(repo.join(path)).unwrap_or_default();
            let lines: Vec<Vec<u8>> = bytes
                .split_inclusive(|b| *b == b'\n')
                .map(<[u8]>::to_vec)
                .collect();
            let total = marker.choices.get(path).map_or(0, |c| c.len() as u32);
            return Ok(ConflictDetail {
                path: path.to_string(),
                kind: marker
                    .kinds
                    .get(path)
                    .copied()
                    .unwrap_or(ConflictKind::BothModified),
                binary: bytes.contains(&0),
                resolved: true,
                whole_file_only: false,
                yours_label,
                theirs_label,
                regions: Vec::new(),
                total,
                chosen: total,
                result: if bytes.contains(&0) {
                    Vec::new()
                } else {
                    shown(&lines)
                        .into_iter()
                        .map(|text| ResultLine {
                            text,
                            from: "common".to_string(),
                            region: None,
                        })
                        .collect()
                },
            });
        };
        let kind = kind_of(&stages, swapped);
        let Some(lined) = self.line_up(repo, &stages, swapped).await? else {
            return Ok(ConflictDetail {
                path: path.to_string(),
                kind,
                binary: stages[1].is_some() && stages[2].is_some(),
                resolved: false,
                whole_file_only: true,
                yours_label,
                theirs_label,
                regions: Vec::new(),
                total: 0,
                chosen: 0,
                result: Vec::new(),
            });
        };
        let total = lined
            .iter()
            .filter(|s| matches!(s, Segment::Conflict { .. }))
            .count();
        let choices = marker
            .choices
            .get(path)
            .filter(|c| c.len() == total)
            .cloned()
            .unwrap_or_else(|| vec![None; total]);
        Ok(ConflictDetail {
            path: path.to_string(),
            kind,
            binary: false,
            resolved: false,
            whole_file_only: false,
            yours_label,
            theirs_label,
            regions: regions_of(&lined, &choices, stages[0].is_some()),
            total: total as u32,
            chosen: choices.iter().filter(|c| c.is_some()).count() as u32,
            result: result_of(&lined, &choices),
        })
    }

    /// Chooses what one conflict of a file becomes (`None` takes a choice
    /// back). The file on disk is rewritten each time; once every conflict
    /// in it is chosen it is marked resolved.
    pub async fn conflict_choose(
        &self,
        repo: &Path,
        path: &str,
        region: u32,
        choice: Option<Choice>,
    ) -> Result<ConflictDetail> {
        let (state, mut marker, stages) = self.conflict_context(repo, path).await?;
        let swapped = state.kind == OperationKind::Rebase;
        let Some(stages) = stages else {
            return Err(Error::Invalid(format!(
                "{path} is resolved already. Reopen it to choose again."
            )));
        };
        let Some(lined) = self.line_up(repo, &stages, swapped).await? else {
            return Err(Error::Invalid(format!(
                "{path} can only be taken whole: yours or theirs."
            )));
        };
        let total = lined
            .iter()
            .filter(|s| matches!(s, Segment::Conflict { .. }))
            .count();
        let mut choices = marker
            .choices
            .get(path)
            .filter(|c| c.len() == total)
            .cloned()
            .unwrap_or_else(|| vec![None; total]);
        let Some(slot) = choices.get_mut(region as usize) else {
            return Err(Error::Invalid(format!(
                "{path} has no conflict number {}.",
                region + 1
            )));
        };
        *slot = choice;
        let (yours_label, theirs_label) = Self::labels(&state);
        let bytes = render(&lined, &choices, &yours_label, &theirs_label);
        let file = repo.join(path);
        std::fs::write(&file, bytes)
            .map_err(|e| Error::io(format!("Couldn’t write {}", file.display()), e))?;
        let resolved = choices.iter().all(Option::is_some);
        let mut detail = ConflictDetail {
            path: path.to_string(),
            kind: kind_of(&stages, swapped),
            binary: false,
            resolved,
            whole_file_only: false,
            yours_label,
            theirs_label,
            regions: regions_of(&lined, &choices, stages[0].is_some()),
            total: total as u32,
            chosen: choices.iter().filter(|c| c.is_some()).count() as u32,
            result: result_of(&lined, &choices),
        };
        if resolved {
            self.exec(repo, &["add", "--", path], Call::default().literal())
                .await?
                .into_result()?;
            marker
                .resolved
                .insert(path.to_string(), how_of(&choices).to_string());
            detail.resolved = true;
        }
        marker.kinds.insert(path.to_string(), detail.kind);
        marker.choices.insert(path.to_string(), choices);
        self.marker_write(repo, &marker).await?;
        Ok(detail)
    }

    /// Takes one side's whole file (or its deletion) and marks it resolved.
    /// The only choice there is for a binary file or a delete/modify conflict.
    pub async fn conflict_take(&self, repo: &Path, path: &str, side: TakeSide) -> Result<()> {
        let (state, mut marker, stages) = self.conflict_context(repo, path).await?;
        let Some(stages) = stages else {
            return Err(Error::Invalid(format!(
                "{path} is resolved already. Reopen it to choose again."
            )));
        };
        let swapped = state.kind == OperationKind::Rebase;
        // Stage 2 is "ours" to git; in a rebase that is the other side.
        let ours = (side == TakeSide::Yours) != swapped;
        let has = if ours { &stages[1] } else { &stages[2] };
        let call = || Call::default().literal();
        if has.is_some() {
            let flag = if ours { "--ours" } else { "--theirs" };
            self.exec(repo, &["checkout", flag, "--", path], call())
                .await?
                .into_result()?;
            self.exec(repo, &["add", "--", path], call())
                .await?
                .into_result()?;
        } else {
            // That side deleted the file.
            self.exec(repo, &["rm", "--quiet", "--force", "--", path], call())
                .await?
                .into_result()?;
        }
        marker
            .kinds
            .insert(path.to_string(), kind_of(&stages, swapped));
        marker.choices.remove(path);
        marker.resolved.insert(
            path.to_string(),
            match side {
                TakeSide::Yours => "yours",
                TakeSide::Theirs => "theirs",
            }
            .to_string(),
        );
        self.marker_write(repo, &marker).await
    }

    /// Marks a file resolved as it is on disk (after editing it by hand).
    /// Refused while it still has conflict markers in it.
    pub async fn conflict_mark_resolved(&self, repo: &Path, path: &str) -> Result<()> {
        let (_, mut marker, stages) = self.conflict_context(repo, path).await?;
        if stages.is_none() {
            return Ok(());
        }
        let bytes = std::fs::read(repo.join(path)).unwrap_or_default();
        let marked = bytes.split(|b| *b == b'\n').any(|line| {
            line.starts_with(b"<<<<<<< ") || line.starts_with(b">>>>>>> ") || line == b"======="
        });
        if marked && !bytes.contains(&0) {
            return Err(Error::Invalid(format!(
                "{path} still has conflict markers in it."
            )));
        }
        self.exec(repo, &["add", "--", path], Call::default().literal())
            .await?
            .into_result()?;
        marker.choices.remove(path);
        marker
            .resolved
            .insert(path.to_string(), "edited".to_string());
        self.marker_write(repo, &marker).await
    }

    /// Puts a resolved file back in conflict, to choose again.
    pub async fn conflict_reopen(&self, repo: &Path, path: &str) -> Result<()> {
        let (_, mut marker, stages) = self.conflict_context(repo, path).await?;
        if stages.is_some() {
            return Ok(());
        }
        self.exec(
            repo,
            &["checkout", "--merge", "--", path],
            Call::default().literal(),
        )
        .await?
        .into_result()
        .map_err(|_| {
            Error::Invalid(format!(
                "{path} can’t be reopened any more. Stop and go back to start over."
            ))
        })?;
        marker.choices.remove(path);
        marker.resolved.remove(path);
        self.marker_write(repo, &marker).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn mark(ch: char, label: &str) -> String {
        let run: String = std::iter::repeat_n(ch, MARK).collect();
        if label.is_empty() {
            format!("{run}\n")
        } else {
            format!("{run} {label}\n")
        }
    }

    fn sample() -> Vec<Segment> {
        let text = format!(
            "one\n{}mine\n{}old\n{}theirs a\ntheirs b\n{}two\n<<<<<<< not a marker\n",
            mark('<', "yours"),
            mark('|', "base"),
            mark('=', ""),
            mark('>', "theirs"),
        );
        segments(text.as_bytes())
    }

    #[test]
    fn merged_output_is_cut_into_regions() {
        let lined = sample();
        assert_eq!(lined.len(), 3);
        let regions = regions_of(&lined, &[None], true);
        assert_eq!(
            regions[1],
            Region::Conflict {
                index: 0,
                yours: vec!["mine".into()],
                theirs: vec!["theirs a".into(), "theirs b".into()],
                base: Some(vec!["old".into()]),
                yours_start: 2,
                theirs_start: 2,
                choice: None,
            }
        );
        // A short run of `<` is content, and line numbers carry on per side.
        assert_eq!(
            regions[2],
            Region::Common {
                lines: vec!["two".into(), "<<<<<<< not a marker".into()],
                yours_start: 3,
                theirs_start: 4,
            }
        );
    }

    #[test]
    fn choices_decide_what_the_file_becomes() {
        let lined = sample();
        let text = |choice: Option<Choice>| {
            String::from_utf8(render(&lined, &[choice], "feature", "main")).unwrap()
        };
        assert_eq!(
            text(Some(Choice::Yours)),
            "one\nmine\ntwo\n<<<<<<< not a marker\n"
        );
        assert!(text(Some(Choice::Theirs)).starts_with("one\ntheirs a\ntheirs b\ntwo\n"));
        assert!(text(Some(Choice::Both { yours_first: false }))
            .starts_with("one\ntheirs a\ntheirs b\nmine\ntwo\n"));
        assert!(text(Some(Choice::Custom {
            text: "typed".into()
        }))
        .starts_with("one\ntyped\ntwo\n"));
        assert!(text(None).starts_with(
            "one\n<<<<<<< feature\nmine\n||||||| base\nold\n=======\ntheirs a\ntheirs b\n>>>>>>> main\ntwo\n"
        ));
        let result = result_of(&lined, &[Some(Choice::Both { yours_first: true })]);
        let from: Vec<&str> = result.iter().map(|l| l.from.as_str()).collect();
        assert_eq!(
            from,
            ["common", "yours", "theirs", "theirs", "common", "common"]
        );
        assert_eq!(result_of(&lined, &[None])[1].from, "undecided");
    }

    #[test]
    fn crlf_files_keep_their_line_ends() {
        let text = format!(
            "one\r\n{}mine\r\n{}{}theirs\r\n{}",
            mark('<', "a"),
            mark('|', "b"),
            mark('=', ""),
            mark('>', "c")
        );
        let lined = segments(text.as_bytes());
        let out = render(
            &lined,
            &[Some(Choice::Custom {
                text: "typed\nmore".into(),
            })],
            "a",
            "c",
        );
        assert_eq!(out, b"one\r\ntyped\r\nmore\r\n");
    }

    #[test]
    fn sides_swap_in_a_rebase() {
        let stages: Stages = [Some("b".into()), None, Some("t".into())];
        assert_eq!(kind_of(&stages, false), ConflictKind::DeletedByYou);
        assert_eq!(kind_of(&stages, true), ConflictKind::DeletedByThem);
        let both: Stages = [None, Some("o".into()), Some("t".into())];
        assert_eq!(kind_of(&both, false), ConflictKind::BothAdded);
    }
}
