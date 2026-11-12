//! Blame: who last changed each line of a file, grouped into runs of
//! neighbouring lines from the same commit.

use std::collections::HashMap;
use std::path::Path;

use serde::Serialize;

use super::util::{check_path, rfc3339, short, Call};
use super::Git;
use crate::error::{Error, Result};

/// The commit and name a run's lines had just before its commit changed
/// them: where "blame before this change" looks next.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct BlamePrevious {
    pub commit: String,
    pub path: String,
}

/// Neighbouring lines last changed by the same commit.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct BlameRun {
    pub commit: String,
    pub short: String,
    pub author: String,
    pub email: String,
    /// RFC 3339.
    pub time: String,
    pub summary: String,
    /// First and last line (from 1) in the file as blamed.
    pub start: u32,
    pub end: u32,
    /// The file's name in that commit (it may have been renamed since).
    pub path: String,
    pub previous: Option<BlamePrevious>,
    /// Not committed yet: changed in the working tree.
    pub uncommitted: bool,
    pub lines: Vec<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct Blame {
    pub path: String,
    /// The commit the file was blamed at; `None` for the working tree.
    pub at: Option<String>,
    pub runs: Vec<BlameRun>,
    pub lines: u32,
}

#[derive(Clone, Default)]
struct Meta {
    author: String,
    email: String,
    time: i64,
    summary: String,
    path: String,
    previous: Option<BlamePrevious>,
}

/// `git blame --porcelain`.
fn parse(out: &[u8]) -> Vec<BlameRun> {
    let mut metas: HashMap<String, Meta> = HashMap::new();
    let mut runs: Vec<BlameRun> = Vec::new();
    let mut current: Option<(String, u32)> = None;
    for raw in out.split(|b| *b == b'\n') {
        if let Some(content) = raw.strip_prefix(b"\t") {
            let Some((sha, line)) = current.take() else {
                continue;
            };
            let content = content.strip_suffix(b"\r").unwrap_or(content);
            let text = String::from_utf8_lossy(content).into_owned();
            match runs.last_mut() {
                Some(run) if run.commit == sha && run.end + 1 == line => {
                    run.end = line;
                    run.lines.push(text);
                }
                _ => {
                    let meta = metas.get(&sha).cloned().unwrap_or_default();
                    runs.push(BlameRun {
                        short: short(&sha),
                        uncommitted: sha.bytes().all(|b| b == b'0'),
                        commit: sha,
                        author: meta.author,
                        email: meta.email,
                        time: rfc3339(meta.time),
                        summary: meta.summary,
                        start: line,
                        end: line,
                        path: meta.path,
                        previous: meta.previous,
                        lines: vec![text],
                    });
                }
            }
            continue;
        }
        let line = String::from_utf8_lossy(raw);
        let (key, value) = line.split_once(' ').unwrap_or((&line, ""));
        if key.len() >= 40 && key.bytes().all(|b| b.is_ascii_hexdigit()) {
            // `<sha> <line before> <line now> [<lines in group>]`
            let now = value
                .split(' ')
                .nth(1)
                .and_then(|n| n.parse().ok())
                .unwrap_or(0);
            metas.entry(key.to_string()).or_default();
            current = Some((key.to_string(), now));
            continue;
        }
        let Some(meta) = current.as_ref().and_then(|(sha, _)| metas.get_mut(sha)) else {
            continue;
        };
        match key {
            "author" => meta.author = value.to_string(),
            "author-mail" => {
                meta.email = value
                    .trim_start_matches('<')
                    .trim_end_matches('>')
                    .to_string();
            }
            "author-time" => meta.time = value.parse().unwrap_or(0),
            "summary" => meta.summary = value.to_string(),
            "filename" => meta.path = value.to_string(),
            "previous" => {
                if let Some((commit, path)) = value.split_once(' ') {
                    meta.previous = Some(BlamePrevious {
                        commit: commit.to_string(),
                        path: path.to_string(),
                    });
                }
            }
            _ => {}
        }
    }
    // A commit's details are only printed the first time it appears, which
    // may be after its first run was started.
    for run in &mut runs {
        if let Some(meta) = metas.get(&run.commit) {
            if run.author.is_empty() {
                run.author.clone_from(&meta.author);
                run.email.clone_from(&meta.email);
                run.time = rfc3339(meta.time);
                run.summary.clone_from(&meta.summary);
            }
            if run.path.is_empty() {
                run.path.clone_from(&meta.path);
            }
            if run.previous.is_none() {
                run.previous.clone_from(&meta.previous);
            }
        }
    }
    runs
}

impl Git {
    /// Who last changed each line of `path`, as it is at `at` (a commit;
    /// the working tree when `None`).
    pub async fn blame(&self, repo: &Path, path: &str, at: Option<&str>) -> Result<Blame> {
        check_path(path)?;
        let at = match at.map(str::trim).filter(|a| !a.is_empty()) {
            Some(rev) => Some(self.resolve_commit(repo, rev).await?),
            None => None,
        };
        let mut args = vec!["-c", "core.quotePath=false", "blame", "--porcelain"];
        if let Some(at) = &at {
            args.push(at);
        }
        args.extend(["--", path]);
        let out = self.exec(repo, &args, Call::default().literal()).await?;
        if !out.ok() {
            let said = out.said();
            return Err(
                if said.contains("no such path") || said.contains("No such file") {
                    Error::Invalid(match &at {
                        Some(at) => format!("{path} doesn’t exist at {}.", short(at)),
                        None => format!("{path} isn’t a file git knows here."),
                    })
                } else {
                    out.error()
                },
            );
        }
        let runs = parse(&out.stdout);
        Ok(Blame {
            path: path.to_string(),
            at,
            lines: runs.last().map_or(0, |r| r.end),
            runs,
        })
    }

    /// The blame of `path` as it was just before `commit` changed it.
    /// (A run's own [`BlameRun::previous`] is more exact: it knows the
    /// file's earlier name.)
    pub async fn blame_before(&self, repo: &Path, path: &str, commit: &str) -> Result<Blame> {
        let commit = self.resolve_commit(repo, commit).await?;
        let parent = self
            .maybe(
                repo,
                &["rev-parse", "--verify", "--quiet", &format!("{commit}^")],
            )
            .await?
            .ok_or_else(|| {
                Error::Invalid(format!(
                    "{} is where this repository starts: there is nothing before it.",
                    short(&commit)
                ))
            })?;
        self.blame(repo, path, Some(&parent)).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn porcelain_is_grouped_into_runs() {
        let a = "a".repeat(40);
        let b = "b".repeat(40);
        let out = format!(
            "{a} 1 1 2\nauthor Demi\nauthor-mail <demi@example.com>\nauthor-time 1762850000\nsummary Start\nfilename old.go\n\tone\n{a} 2 2\n\ttwo\n{b} 3 3 1\nauthor Rhea\nauthor-mail <rhea@example.com>\nauthor-time 1762860000\nsummary Fix it\nprevious {a} old.go\nfilename new.go\n\tthree\r\n{a} 3 4 1\n\tfour\n"
        );
        let runs = parse(out.as_bytes());
        assert_eq!(runs.len(), 3);
        assert_eq!((runs[0].start, runs[0].end), (1, 2));
        assert_eq!(runs[0].lines, ["one", "two"]);
        assert_eq!(runs[0].author, "Demi");
        assert_eq!(runs[0].email, "demi@example.com");
        assert_eq!(runs[0].path, "old.go");
        assert_eq!(runs[1].summary, "Fix it");
        assert_eq!(runs[1].lines, ["three"]);
        assert_eq!(
            runs[1].previous,
            Some(BlamePrevious {
                commit: a.clone(),
                path: "old.go".into()
            })
        );
        // The same commit again, after another one: a new run.
        assert_eq!((runs[2].start, runs[2].end), (4, 4));
        assert_eq!(runs[2].author, "Demi");
        assert!(!runs[2].uncommitted);
    }
}
