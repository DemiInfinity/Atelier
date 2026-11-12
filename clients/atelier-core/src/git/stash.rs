//! Putting changes aside (git's stash) and bringing them back. Entries are
//! named by their commit id, which stays the same while the list shifts.

use std::path::Path;

use serde::Serialize;

use super::integrate::{Marker, OpOutcome};
use super::undo::StashRef;
use super::util::{check_path, is_sha, rfc3339, short, Call, FIELD};
use super::Git;
use crate::diff::{self, FileDiff};
use crate::error::{Error, Result};

/// Lines of one file's diff sent along; longer ones are cut.
const MAX_LINES: usize = 2000;

/// One entry of the put-aside list.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct StashEntry {
    /// The stash commit: what every action takes.
    pub id: String,
    pub short: String,
    /// Its place in the list right now (`stash@{0}` is 0).
    pub position: usize,
    pub message: String,
    /// The branch it was put aside on.
    pub branch: Option<String>,
    /// RFC 3339.
    pub time: String,
    pub files: u32,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct StashDetail {
    pub entry: StashEntry,
    pub files: Vec<FileDiff>,
}

/// `On main: message` / `WIP on main: 1a2b3c4 subject`.
fn parse_subject(subject: &str) -> (Option<String>, String) {
    for prefix in ["WIP on ", "On "] {
        if let Some(rest) = subject.strip_prefix(prefix) {
            if let Some((branch, message)) = rest.split_once(": ") {
                let branch = Some(branch.to_string()).filter(|b| b != "(no branch)");
                return (branch, message.to_string());
            }
        }
    }
    (None, subject.to_string())
}

impl Git {
    /// The put-aside list, newest first.
    pub async fn stash_list(&self, repo: &Path) -> Result<Vec<StashEntry>> {
        let format = format!("--format=%H{FIELD}%ct{FIELD}%gs");
        let out = self
            .exec(repo, &["stash", "list", &format], Call::default())
            .await?;
        if !out.ok() {
            return Ok(Vec::new());
        }
        let mut entries = Vec::new();
        for (position, line) in out.text().lines().enumerate() {
            let mut parts = line.split(FIELD);
            let (Some(id), Some(time), Some(subject)) = (parts.next(), parts.next(), parts.next())
            else {
                continue;
            };
            let (branch, message) = parse_subject(subject);
            let names = self
                .text(
                    repo,
                    &[
                        "stash",
                        "show",
                        "--include-untracked",
                        "--name-only",
                        "-z",
                        id,
                    ],
                )
                .await
                .unwrap_or_default();
            entries.push(StashEntry {
                id: id.to_string(),
                short: short(id),
                position,
                message,
                branch,
                time: rfc3339(time.parse().unwrap_or(0)),
                files: names.split('\0').filter(|n| !n.is_empty()).count() as u32,
            });
        }
        Ok(entries)
    }

    /// Where the stash with this commit id is in the list now.
    pub(crate) async fn stash_position(&self, repo: &Path, id: &str) -> Result<Option<usize>> {
        let out = self
            .exec(repo, &["stash", "list", "--format=%H"], Call::default())
            .await?;
        Ok(out.text().lines().position(|line| line == id))
    }

    async fn stash_find(&self, repo: &Path, id: &str) -> Result<StashEntry> {
        if !is_sha(id) {
            return Err(Error::Invalid("That isn’t a put-aside entry.".into()));
        }
        self.stash_list(repo)
            .await?
            .into_iter()
            .find(|e| e.id == id || e.id.starts_with(id))
            .ok_or_else(|| Error::Invalid("That put-aside entry isn’t there any more.".into()))
    }

    /// Puts uncommitted changes aside: everything, or only `paths`.
    pub async fn stash_save(
        &self,
        repo: &Path,
        message: &str,
        include_untracked: bool,
        paths: &[String],
    ) -> Result<StashEntry> {
        for path in paths {
            check_path(path)?;
        }
        let branch = self.current_branch(repo).await?;
        let message = match message.trim() {
            "" => format!(
                "Changes on {}",
                branch.as_deref().unwrap_or("a detached HEAD")
            ),
            typed => typed.to_string(),
        };
        let title = format!("Put aside “{message}”");
        let pending = self.journal_begin(repo, "stash_save", &title, &[]).await?;
        let before = self.stash_list(repo).await?.first().map(|e| e.id.clone());
        let mut args = vec!["stash", "push", "--quiet", "-m", message.as_str()];
        if include_untracked {
            args.push("--include-untracked");
        }
        if !paths.is_empty() {
            args.push("--");
            args.extend(paths.iter().map(String::as_str));
        }
        // Literal pathspecs only when there are paths: without any, git's own
        // clean-up of untracked files relies on pathspec magic.
        let call = if paths.is_empty() {
            Call::default()
        } else {
            Call::default().literal()
        };
        let out = self.exec(repo, &args, call).await?;
        let top = self.stash_list(repo).await?.into_iter().next();
        let made = top.filter(|e| Some(&e.id) != before.as_ref());
        match (out.ok(), made) {
            (true, Some(entry)) => {
                self.journal_stash(
                    repo,
                    &pending,
                    StashRef {
                        sha: entry.id.clone(),
                        message: entry.message.clone(),
                    },
                )
                .await;
                self.journal_finish(repo, pending, None).await?;
                Ok(entry)
            }
            (true, None) => {
                self.journal_cancel(repo, pending).await;
                Err(Error::Invalid("There is nothing to put aside.".into()))
            }
            (false, _) => {
                self.journal_cancel(repo, pending).await;
                Err(out.error())
            }
        }
    }

    /// One put-aside entry: its files and their diffs.
    pub async fn stash_show(&self, repo: &Path, id: &str) -> Result<StashDetail> {
        let entry = self.stash_find(repo, id).await?;
        let text = self
            .text(
                repo,
                &[
                    "-c",
                    "core.quotePath=false",
                    "stash",
                    "show",
                    "--include-untracked",
                    "--patch",
                    "--no-color",
                    "--no-ext-diff",
                    &entry.id,
                ],
            )
            .await?;
        Ok(StashDetail {
            entry,
            files: diff::parse_limited(&text, MAX_LINES),
        })
    }

    async fn stash_bring(&self, repo: &Path, id: &str, pop: bool) -> Result<OpOutcome> {
        let entry = self.stash_find(repo, id).await?;
        if self.operation_in_progress(repo).await? {
            return Err(Error::Invalid(
                "Something is still under way here. Finish it or stop it first.".into(),
            ));
        }
        let name = format!("stash@{{{}}}", entry.position);
        let (kind, verb, summary) = if pop {
            (
                "stash_pop",
                "pop",
                format!("Brought back “{}”", entry.message),
            )
        } else {
            (
                "stash_apply",
                "apply",
                format!("Applied put-aside “{}”", entry.message),
            )
        };
        let stash = StashRef {
            sha: entry.id.clone(),
            message: entry.message.clone(),
        };
        let pending = self.journal_begin(repo, kind, &summary, &[]).await?;
        self.journal_stash(repo, &pending, stash.clone()).await;
        let out = self
            .exec(repo, &["stash", verb, "--quiet", &name], Call::default())
            .await?;
        if out.ok() {
            let journal = self
                .journal_finish(repo, pending, Some(summary.clone()))
                .await?;
            return Ok(OpOutcome {
                done: true,
                summary,
                conflicts: Vec::new(),
                head: self.head_sha(repo).await?,
                journal: Some(journal),
                state: self.operation_state(repo).await?,
            });
        }
        let conflicts = self.unmerged_paths(repo).await?;
        if conflicts.is_empty() {
            // Git refused before touching anything.
            self.journal_cancel(repo, pending).await;
            let said = out.said();
            return Err(
                if said.contains("would be overwritten") || said.contains("already exists") {
                    Error::Invalid(format!(
                        "“{}” touches files you have changed since. Commit or put aside those changes first.",
                        entry.message
                    ))
                } else {
                    out.error()
                },
            );
        }
        // It went in with conflicts; a popped entry stays in the list until
        // they are resolved.
        self.marker_write(
            repo,
            &Marker {
                cause: kind.to_string(),
                theirs: Some(format!("put aside “{}”", entry.message)),
                stash: Some(stash),
                pop,
                stop: "apply".to_string(),
                paths: conflicts.clone(),
                ..Marker::default()
            },
        )
        .await?;
        let state = self.operation_state(repo).await?;
        Ok(OpOutcome {
            done: false,
            summary: state.title.clone(),
            conflicts,
            head: self.head_sha(repo).await?,
            journal: None,
            state,
        })
    }

    /// Applies a put-aside entry and keeps it in the list.
    pub async fn stash_apply(&self, repo: &Path, id: &str) -> Result<OpOutcome> {
        self.stash_bring(repo, id, false).await
    }

    /// Applies a put-aside entry and takes it off the list.
    pub async fn stash_pop(&self, repo: &Path, id: &str) -> Result<OpOutcome> {
        self.stash_bring(repo, id, true).await
    }

    /// Drops a put-aside entry. It can be brought back from Recent actions
    /// for thirty days. Returns the journal entry.
    pub async fn stash_drop(&self, repo: &Path, id: &str) -> Result<u64> {
        let entry = self.stash_find(repo, id).await?;
        let title = format!("Dropped put-aside “{}”", entry.message);
        let pending = self.journal_begin(repo, "stash_drop", &title, &[]).await?;
        self.journal_stash(
            repo,
            &pending,
            StashRef {
                sha: entry.id.clone(),
                message: entry.message.clone(),
            },
        )
        .await;
        let name = format!("stash@{{{}}}", entry.position);
        let out = self
            .exec(repo, &["stash", "drop", "--quiet", &name], Call::default())
            .await?;
        if !out.ok() {
            self.journal_cancel(repo, pending).await;
            return Err(out.error());
        }
        self.journal_finish(repo, pending, None).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stash_subjects_give_branch_and_message() {
        assert_eq!(
            parse_subject("On feature/x: Half-done badge"),
            (Some("feature/x".into()), "Half-done badge".into())
        );
        assert_eq!(
            parse_subject("WIP on main: 1a2b3c4 Fix: the thing"),
            (Some("main".into()), "1a2b3c4 Fix: the thing".into())
        );
        assert_eq!(
            parse_subject("On (no branch): detached work"),
            (None, "detached work".into())
        );
        assert_eq!(parse_subject("custom"), (None, "custom".into()));
    }
}
