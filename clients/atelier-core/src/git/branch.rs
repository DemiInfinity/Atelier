//! Branches, tags and remotes: listing them with where each stands,
//! creating, renaming, deleting, switching (with a say about uncommitted
//! changes), fetch, pull, push and the careful force push.

use std::path::Path;

use serde::{Deserialize, Serialize};

use super::integrate::{Marker, OpOutcome};
use super::util::{check_rev, count, is_sha, plain_refusal, rfc3339, short, Call, FIELD};
use super::Git;
use crate::error::{Error, Result};

/// One branch, local or on a remote.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct BranchInfo {
    /// `main`; for a remote branch the name without the remote.
    pub name: String,
    /// `refs/heads/main`, `refs/remotes/origin/main`.
    pub full_ref: String,
    /// The remote's name, for a remote branch.
    pub remote: Option<String>,
    pub current: bool,
    /// What it follows (`origin/main`).
    pub upstream: Option<String>,
    /// The upstream was deleted on the remote.
    pub upstream_gone: bool,
    pub ahead: u64,
    pub behind: u64,
    pub sha: String,
    pub short: String,
    pub subject: String,
    pub author: String,
    /// RFC 3339, of the last commit.
    pub time: String,
    /// The folder of the worktree that has it checked out, if one has.
    pub worktree: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct RemoteBranches {
    pub remote: String,
    pub branches: Vec<BranchInfo>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct BranchList {
    /// `None` with a detached HEAD.
    pub current: Option<String>,
    pub head: Option<String>,
    /// The current one first, then by name.
    pub local: Vec<BranchInfo>,
    pub remotes: Vec<RemoteBranches>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct TagInfo {
    pub name: String,
    /// The commit it points at.
    pub sha: String,
    pub short: String,
    pub annotated: bool,
    /// The tag's own message, or the commit's subject for a lightweight tag.
    pub subject: String,
    /// RFC 3339: when it was tagged, or the commit's date.
    pub time: String,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct RemoteEntry {
    pub name: String,
    pub url: String,
    /// Where pushes go, when that differs from `url`.
    pub push_url: Option<String>,
}

/// What to do with uncommitted changes when switching.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SwitchHow {
    /// Switch only when nothing is uncommitted; otherwise say so and wait.
    Ask,
    /// Bring the changes along (a three-way merge when they overlap).
    Carry,
    /// Put them aside first.
    PutAside,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct SwitchOutcome {
    pub switched: bool,
    /// There are uncommitted changes and no choice was made: nothing changed.
    pub needs_choice: bool,
    /// How many uncommitted files there were.
    pub changes: u32,
    /// The message of the put-aside entry, when the changes were put aside.
    pub put_aside: Option<String>,
    /// Carried changes that collided with the other branch.
    pub conflicts: Vec<String>,
    pub branch: String,
    pub journal: Option<u64>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PullMode {
    Merge,
    Rebase,
    FfOnly,
}

/// What deleting a branch would cost.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct DeleteCheck {
    pub branch: String,
    /// Nothing is lost: every commit is on its upstream or on HEAD.
    pub safe: bool,
    /// Commits only this branch has.
    pub unmerged: u64,
    pub current: bool,
    /// Open in a worktree: it can't be deleted while that is so.
    pub worktree: Option<String>,
}

/// What a force push would replace, for the dialog that asks.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct ForcePushPreview {
    pub branch: String,
    pub remote: String,
    pub remote_branch: String,
    /// Where the remote branch stood at the last fetch: the lease.
    pub expected: Option<String>,
    /// Commits on the remote that would go.
    pub replaced: u64,
    /// Commits from here that would arrive.
    pub new: u64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct Pushed {
    pub branch: String,
    pub remote: String,
    pub forced: bool,
}

fn parse_track(track: &str) -> (u64, u64, bool) {
    let (mut ahead, mut behind) = (0, 0);
    for part in track.split(',') {
        let mut words = part.split_whitespace();
        match (words.next(), words.next().and_then(|n| n.parse().ok())) {
            (Some("ahead"), Some(n)) => ahead = n,
            (Some("behind"), Some(n)) => behind = n,
            _ => {}
        }
    }
    (ahead, behind, track.trim() == "gone")
}

impl Git {
    async fn remote_names(&self, repo: &Path) -> Result<Vec<String>> {
        Ok(self
            .text(repo, &["remote"])
            .await?
            .lines()
            .map(str::trim)
            .filter(|n| !n.is_empty())
            .map(str::to_string)
            .collect())
    }

    /// Local branches and every remote's branches, with upstream,
    /// ahead/behind and the last commit.
    pub async fn branch_list(&self, repo: &Path) -> Result<BranchList> {
        let format = [
            "%(HEAD)",
            "%(refname)",
            "%(upstream:short)",
            "%(upstream:track,nobracket)",
            "%(objectname)",
            "%(contents:subject)",
            "%(authorname)",
            "%(committerdate:unix)",
            "%(worktreepath)",
        ]
        .join("%1f");
        let out = self
            .text(
                repo,
                &[
                    "for-each-ref",
                    &format!("--format={format}"),
                    "refs/heads",
                    "refs/remotes",
                ],
            )
            .await?;
        let mut remote_names = self.remote_names(repo).await?;
        // Remote names may contain slashes: the longest match wins.
        remote_names.sort_by_key(|n| std::cmp::Reverse(n.len()));
        let here = self
            .toplevel(repo)
            .await?
            .map(|p| p.to_string_lossy().into_owned());
        let mut local = Vec::new();
        let mut remotes: Vec<RemoteBranches> = Vec::new();
        for line in out.lines() {
            let parts: Vec<&str> = line.split(FIELD).collect();
            let [head, full_ref, upstream, track, sha, subject, author, time, worktree] = parts[..]
            else {
                continue;
            };
            let (ahead, behind, gone) = parse_track(track);
            let mut info = BranchInfo {
                name: String::new(),
                full_ref: full_ref.to_string(),
                remote: None,
                current: head == "*",
                upstream: Some(upstream.to_string()).filter(|u| !u.is_empty()),
                upstream_gone: gone,
                ahead,
                behind,
                sha: sha.to_string(),
                short: short(sha),
                subject: subject.to_string(),
                author: author.to_string(),
                time: rfc3339(time.parse().unwrap_or(0)),
                worktree: Some(worktree.to_string())
                    .filter(|w| !w.is_empty() && Some(w) != here.as_ref()),
            };
            if let Some(name) = full_ref.strip_prefix("refs/heads/") {
                info.name = name.to_string();
                local.push(info);
            } else if let Some(rest) = full_ref.strip_prefix("refs/remotes/") {
                let Some(remote) = remote_names.iter().find(|r| {
                    rest.strip_prefix(r.as_str())
                        .is_some_and(|b| b.starts_with('/'))
                }) else {
                    continue;
                };
                let name = &rest[remote.len() + 1..];
                if name == "HEAD" {
                    continue;
                }
                info.name = name.to_string();
                info.remote = Some(remote.clone());
                info.worktree = None;
                match remotes.iter_mut().find(|r| &r.remote == remote) {
                    Some(group) => group.branches.push(info),
                    None => remotes.push(RemoteBranches {
                        remote: remote.clone(),
                        branches: vec![info],
                    }),
                }
            }
        }
        local.sort_by(|a, b| b.current.cmp(&a.current).then(a.name.cmp(&b.name)));
        remotes.sort_by(|a, b| a.remote.cmp(&b.remote));
        Ok(BranchList {
            current: self.current_branch(repo).await?,
            head: self.head_sha(repo).await?,
            local,
            remotes,
        })
    }

    async fn local_branch(&self, repo: &Path, name: &str) -> Result<BranchInfo> {
        self.check_name(repo, name, "branch").await?;
        self.branch_list(repo)
            .await?
            .local
            .into_iter()
            .find(|b| b.name == name)
            .ok_or_else(|| Error::Invalid(format!("There is no branch called {name} here.")))
    }

    /// Makes a branch at `from` (a branch, a tag, a remote branch or a
    /// commit; HEAD when `None`). It isn't switched to.
    pub async fn branch_create(&self, repo: &Path, name: &str, from: Option<&str>) -> Result<()> {
        self.check_name(repo, name, "branch").await?;
        let start = match from.map(str::trim).filter(|f| !f.is_empty()) {
            Some(from) => {
                self.resolve_commit(repo, from).await?;
                from.to_string()
            }
            None => "HEAD".to_string(),
        };
        let full = format!("refs/heads/{name}");
        let title = format!("Made the branch {name}");
        let pending = self
            .journal_begin(repo, "branch_create", &title, &[full])
            .await?;
        let out = self
            .exec(repo, &["branch", "--quiet", name, &start], Call::default())
            .await?;
        if !out.ok() {
            self.journal_cancel(repo, pending).await;
            return Err(if out.said().contains("already exists") {
                Error::Invalid(format!("There is already a branch called {name}."))
            } else {
                out.error()
            });
        }
        self.journal_finish(repo, pending, None).await?;
        Ok(())
    }

    pub async fn branch_rename(&self, repo: &Path, old: &str, new: &str) -> Result<()> {
        self.local_branch(repo, old).await?;
        self.check_name(repo, new, "branch").await?;
        let title = format!("Renamed {old} to {new}");
        let pending = self
            .journal_begin(repo, "branch_rename", &title, &[])
            .await?;
        let out = self
            .exec(repo, &["branch", "-m", old, new], Call::default())
            .await?;
        if !out.ok() {
            self.journal_cancel(repo, pending).await;
            return Err(if out.said().contains("already exists") {
                Error::Invalid(format!("There is already a branch called {new}."))
            } else {
                out.error()
            });
        }
        self.journal_note(repo, &pending, "old", old).await;
        self.journal_note(repo, &pending, "new", new).await;
        self.journal_finish(repo, pending, None).await?;
        Ok(())
    }

    /// What deleting `name` would cost.
    pub async fn branch_delete_check(&self, repo: &Path, name: &str) -> Result<DeleteCheck> {
        let branch = self.local_branch(repo, name).await?;
        // Commits that neither HEAD nor any remote has.
        let mut args = vec![
            "rev-list".to_string(),
            "--count".to_string(),
            branch.full_ref.clone(),
            "--not".to_string(),
            "--remotes".to_string(),
        ];
        if !branch.current && self.head_sha(repo).await?.is_some() {
            args.push("HEAD".to_string());
        }
        let unmerged = self.line(repo, &args).await?.parse().unwrap_or(0);
        Ok(DeleteCheck {
            branch: name.to_string(),
            safe: unmerged == 0,
            unmerged,
            current: branch.current,
            worktree: branch.worktree,
        })
    }

    /// Deletes a local branch (never one on a remote). Without `force` it
    /// is refused when the branch has commits nothing else has. Either way
    /// it can be brought back from Recent actions.
    pub async fn branch_delete(&self, repo: &Path, name: &str, force: bool) -> Result<()> {
        let check = self.branch_delete_check(repo, name).await?;
        if check.current {
            return Err(Error::Invalid(format!(
                "You are on {name}. Switch to another branch first."
            )));
        }
        if let Some(at) = &check.worktree {
            return Err(Error::Invalid(format!(
                "{name} is open in the worktree at {at}. Remove that worktree first."
            )));
        }
        if !check.safe && !force {
            return Err(Error::Invalid(format!(
                "{name} has {} that {} nowhere else. Deleting it would leave {} reachable only from Recent actions.",
                count(check.unmerged, "commit", "commits"),
                if check.unmerged == 1 { "is" } else { "are" },
                if check.unmerged == 1 { "it" } else { "them" },
            )));
        }
        let branch = self.local_branch(repo, name).await?;
        let title = format!("Deleted branch {name}");
        let pending = self
            .journal_begin(
                repo,
                "branch_delete",
                &title,
                std::slice::from_ref(&branch.full_ref),
            )
            .await?;
        let out = self
            .exec(repo, &["branch", "--quiet", "-D", name], Call::default())
            .await?;
        if !out.ok() {
            self.journal_cancel(repo, pending).await;
            return Err(out.error());
        }
        self.journal_note(repo, &pending, "branch", name).await;
        if let Some(upstream) = branch.upstream.filter(|_| !branch.upstream_gone) {
            self.journal_note(repo, &pending, "upstream", &upstream)
                .await;
        }
        self.journal_finish(repo, pending, None).await?;
        Ok(())
    }

    /// Switches to `target`: a local branch, or a remote branch
    /// (`origin/feature/x`), which gets a local branch that follows it.
    /// With uncommitted changes `how` decides: ask (nothing happens), carry
    /// them along, or put them aside first.
    pub async fn branch_switch(
        &self,
        repo: &Path,
        target: &str,
        how: SwitchHow,
    ) -> Result<SwitchOutcome> {
        check_rev(target)?;
        if self.operation_in_progress(repo).await? {
            return Err(Error::Invalid(
                "Something is still under way here. Finish it or stop it first.".into(),
            ));
        }
        let list = self.branch_list(repo).await?;
        // A local name wins; otherwise `remote/branch` makes a follower.
        let local = list.local.iter().find(|b| b.name == target);
        let remote = list
            .remotes
            .iter()
            .flat_map(|r| r.branches.iter())
            .find(|b| b.full_ref == format!("refs/remotes/{target}"));
        let (name, track): (String, Option<String>) = match (local, remote) {
            (Some(branch), _) => (branch.name.clone(), None),
            (None, Some(branch)) => match list.local.iter().find(|b| b.name == branch.name) {
                Some(existing) => (existing.name.clone(), None),
                None => (branch.name.clone(), Some(target.to_string())),
            },
            (None, None) => {
                return Err(Error::Invalid(format!(
                    "There is no branch called {target} here."
                )))
            }
        };
        let mut outcome = SwitchOutcome {
            switched: false,
            needs_choice: false,
            changes: self
                .text(repo, &["status", "--porcelain"])
                .await?
                .lines()
                .count() as u32,
            put_aside: None,
            conflicts: Vec::new(),
            branch: name.clone(),
            journal: None,
        };
        if list.current.as_deref() == Some(name.as_str()) {
            outcome.switched = true;
            return Ok(outcome);
        }
        if let Some(at) = list
            .local
            .iter()
            .find(|b| b.name == name)
            .and_then(|b| b.worktree.clone())
        {
            return Err(Error::Invalid(format!(
                "{name} is open in the worktree at {at}. Open that folder instead."
            )));
        }
        let tracked = self.tracked_changes(repo).await?;
        if tracked > 0 && how == SwitchHow::Ask {
            outcome.needs_choice = true;
            return Ok(outcome);
        }
        if outcome.changes > 0 && how == SwitchHow::PutAside {
            let message = format!("Before switching to {name}");
            self.stash_save(repo, &message, true, &[]).await?;
            outcome.put_aside = Some(message);
        }
        let title = format!("Switched to {name}");
        let mut refs = Vec::new();
        if track.is_some() {
            refs.push(format!("refs/heads/{name}"));
        }
        let pending = self.journal_begin(repo, "switch", &title, &refs).await?;
        let mut args: Vec<String> = vec!["switch".into(), "--quiet".into()];
        if tracked > 0 && how == SwitchHow::Carry {
            args.push("--merge".into());
        }
        match &track {
            Some(remote) => args.extend(["--track".into(), remote.clone()]),
            None => args.push(name.clone()),
        }
        let out = self.exec(repo, &args, Call::default()).await?;
        let conflicts = self.unmerged_paths(repo).await?;
        if !conflicts.is_empty() {
            // The carried changes collided: the same view as any conflict.
            self.marker_write(
                repo,
                &Marker {
                    cause: "switch".to_string(),
                    theirs: Some("your uncommitted changes".to_string()),
                    stop: "apply".to_string(),
                    paths: conflicts.clone(),
                    ..Marker::default()
                },
            )
            .await?;
            outcome.switched = true;
            outcome.conflicts = conflicts;
            return Ok(outcome);
        }
        if !out.ok() {
            self.journal_cancel(repo, pending).await;
            return Err(plain_refusal(out.error(), &format!("Switching to {name}")));
        }
        outcome.switched = true;
        outcome.journal = Some(self.journal_finish(repo, pending, None).await?);
        Ok(outcome)
    }

    /// Sets what `branch` follows (`origin/main`), or with `None` makes it
    /// follow nothing.
    pub async fn branch_set_upstream(
        &self,
        repo: &Path,
        branch: &str,
        upstream: Option<&str>,
    ) -> Result<()> {
        self.local_branch(repo, branch).await?;
        let args: Vec<String> = match upstream.map(str::trim).filter(|u| !u.is_empty()) {
            Some(upstream) => {
                check_rev(upstream)?;
                vec![
                    "branch".into(),
                    "--quiet".into(),
                    format!("--set-upstream-to={upstream}"),
                    branch.to_string(),
                ]
            }
            None => vec![
                "branch".into(),
                "--quiet".into(),
                "--unset-upstream".into(),
                branch.to_string(),
            ],
        };
        let out = self.exec(repo, &args, Call::default()).await?;
        if out.ok() {
            return Ok(());
        }
        Err(match upstream {
            Some(upstream) if out.said().contains("does not exist") => Error::Invalid(format!(
                "There is no {upstream} here. Fetch first, or push the branch."
            )),
            _ => out.error(),
        })
    }

    async fn check_remote(&self, repo: &Path, remote: &str) -> Result<()> {
        if self.remote_names(repo).await?.iter().any(|r| r == remote) {
            Ok(())
        } else {
            Err(Error::Invalid(format!(
                "There is no remote called {remote} here."
            )))
        }
    }

    /// Fetches one remote, or all of them.
    pub async fn fetch_remotes(
        &self,
        repo: &Path,
        remote: Option<&str>,
        prune: bool,
    ) -> Result<()> {
        let mut args = vec!["fetch", "--quiet"];
        if prune {
            args.push("--prune");
        }
        match remote {
            Some(remote) => {
                self.check_remote(repo, remote).await?;
                args.push(remote);
            }
            None => args.push("--all"),
        }
        self.exec(repo, &args, Call::default())
            .await?
            .into_result()
            .map(|_| ())
    }

    /// Pulls the current branch from what it follows. Conflicts come back
    /// as an outcome that isn't done, to be resolved like any other.
    pub async fn pull(&self, repo: &Path, mode: PullMode) -> Result<OpOutcome> {
        let Some(branch) = self.current_branch(repo).await? else {
            return Err(Error::Invalid(
                "You’re not on a branch, so there is nothing to pull into.".into(),
            ));
        };
        let upstream = self
            .maybe(
                repo,
                &[
                    "rev-parse",
                    "--abbrev-ref",
                    "--symbolic-full-name",
                    "@{upstream}",
                ],
            )
            .await?
            .ok_or_else(|| {
                Error::Invalid(format!(
                    "{branch} doesn’t follow a branch on a remote yet. Push it, or set what it follows."
                ))
            })?;
        let summary = format!("Pulled {{commits}} into {branch}");
        let refs: Vec<String> = self.current_ref(repo).await?.into_iter().collect();
        let pending = self
            .journal_begin(repo, "pull", &format!("Pulled into {branch}"), &refs)
            .await?;
        let flag = match mode {
            PullMode::Merge => "--no-rebase",
            PullMode::Rebase => "--rebase",
            PullMode::FfOnly => "--ff-only",
        };
        let args: Vec<String> = ["pull", "--quiet", "--no-edit", flag]
            .iter()
            .map(|a| a.to_string())
            .collect();
        let marker = Marker {
            cause: "pull".into(),
            theirs: Some(upstream.clone()),
            ..Marker::default()
        };
        self.run_op(
            repo,
            pending,
            &args,
            Call::default().no_editor(),
            marker,
            &summary,
            "Pulling",
        )
        .await
        .map_err(|e| match e {
            Error::Git { message, .. }
                if message.contains("Not possible to fast-forward")
                    || message.contains("divergent")
                    || message.contains("diverging") =>
            {
                Error::Invalid(format!(
                    "{branch} and {upstream} have both moved on. Pull with a merge or a rebase instead."
                ))
            }
            other => other,
        })
    }

    /// Pushes a local branch to a remote (its upstream's remote by
    /// default, else the only remote), setting the upstream when it has none.
    pub async fn push_branch(
        &self,
        repo: &Path,
        branch: Option<&str>,
        remote: Option<&str>,
    ) -> Result<Pushed> {
        let (branch, remote, remote_branch, has_upstream) =
            self.push_target(repo, branch, remote).await?;
        let spec = format!("refs/heads/{branch}:refs/heads/{remote_branch}");
        let mut args = vec!["push", "--quiet"];
        if !has_upstream {
            args.push("--set-upstream");
        }
        args.extend([remote.as_str(), spec.as_str()]);
        let out = self.exec(repo, &args, Call::default()).await?;
        if !out.ok() {
            let said = out.said();
            return Err(
                if said.contains("rejected")
                    || said.contains("fetch first")
                    || said.contains("non-fast-forward")
                {
                    Error::Invalid(format!(
                        "{remote} has commits on {remote_branch} that aren’t here yet, so the push was refused. Pull first, or force push safely if you rewrote the branch."
                    ))
                } else {
                    out.error()
                },
            );
        }
        Ok(Pushed {
            branch,
            remote,
            forced: false,
        })
    }

    /// The branch, the remote, the branch's name there, and whether it
    /// follows something already.
    async fn push_target(
        &self,
        repo: &Path,
        branch: Option<&str>,
        remote: Option<&str>,
    ) -> Result<(String, String, String, bool)> {
        let name = match branch {
            Some(name) => name.to_string(),
            None => self.current_branch(repo).await?.ok_or_else(|| {
                Error::Invalid("You’re not on a branch, so there is nothing to push.".into())
            })?,
        };
        let info = self.local_branch(repo, &name).await?;
        let remotes = self.remote_names(repo).await?;
        let followed = info.upstream.as_deref().and_then(|upstream| {
            remotes
                .iter()
                .filter(|r| {
                    upstream
                        .strip_prefix(r.as_str())
                        .is_some_and(|rest| rest.starts_with('/'))
                })
                .max_by_key(|r| r.len())
                .map(|r| (r.clone(), upstream[r.len() + 1..].to_string()))
        });
        let (remote, remote_branch, has_upstream) = match (remote, followed) {
            (Some(remote), Some((followed, there))) if followed == remote => {
                (remote.to_string(), there, true)
            }
            (Some(remote), _) => {
                self.check_remote(repo, remote).await?;
                (remote.to_string(), name.clone(), info.upstream.is_some())
            }
            (None, Some((followed, there))) => (followed, there, true),
            (None, None) => match remotes.as_slice() {
                [only] => (only.clone(), name.clone(), false),
                [] => {
                    return Err(Error::Invalid(
                        "This repository has no remote to push to yet.".into(),
                    ))
                }
                _ if remotes.iter().any(|r| r == "origin") => {
                    ("origin".to_string(), name.clone(), false)
                }
                _ => {
                    return Err(Error::Invalid(format!(
                        "Choose which remote {name} goes to."
                    )))
                }
            },
        };
        Ok((name, remote, remote_branch, has_upstream))
    }

    /// What a force push of `branch` would replace on its remote, as of
    /// the last fetch.
    pub async fn force_push_preview(
        &self,
        repo: &Path,
        branch: Option<&str>,
        remote: Option<&str>,
    ) -> Result<ForcePushPreview> {
        let (branch, remote, remote_branch, _) = self.push_target(repo, branch, remote).await?;
        let tracking = format!("refs/remotes/{remote}/{remote_branch}");
        let expected = self
            .maybe(repo, &["rev-parse", "--verify", "--quiet", &tracking])
            .await?;
        let local = format!("refs/heads/{branch}");
        let (replaced, new) = match &expected {
            Some(expected) => (
                self.count_commits(repo, &[&format!("{local}..{expected}")])
                    .await?,
                self.count_commits(repo, &[&format!("{expected}..{local}")])
                    .await?,
            ),
            None => (0, self.count_commits(repo, &[&local]).await?),
        };
        Ok(ForcePushPreview {
            branch,
            remote,
            remote_branch,
            expected,
            replaced,
            new,
        })
    }

    /// Replaces the remote branch with the local one, but only if the
    /// remote still stands at `expected` (the commit the person was shown).
    /// If someone pushed in between, nothing happens.
    pub async fn push_force_with_lease(
        &self,
        repo: &Path,
        branch: Option<&str>,
        remote: Option<&str>,
        expected: &str,
    ) -> Result<Pushed> {
        if !is_sha(expected) || expected.len() < 40 {
            return Err(Error::Invalid(
                "A force push needs the full id of the commit it replaces.".into(),
            ));
        }
        let (branch, remote, remote_branch, _) = self.push_target(repo, branch, remote).await?;
        let lease = format!("--force-with-lease=refs/heads/{remote_branch}:{expected}");
        let spec = format!("refs/heads/{branch}:refs/heads/{remote_branch}");
        let out = self
            .exec(
                repo,
                &["push", "--quiet", &lease, &remote, &spec],
                Call::default(),
            )
            .await?;
        if !out.ok() {
            let said = out.said();
            return Err(
                if said.contains("stale info") || said.contains("rejected") {
                    Error::Invalid(format!(
                    "Someone pushed to {remote_branch} on {remote} in the meantime, so nothing was replaced. Fetch and look again."
                ))
                } else {
                    out.error()
                },
            );
        }
        Ok(Pushed {
            branch,
            remote,
            forced: true,
        })
    }

    // ------------------------------------------------------------------ tags

    /// Tags, newest first.
    pub async fn tag_list(&self, repo: &Path) -> Result<Vec<TagInfo>> {
        let format = [
            "%(refname:short)",
            "%(objecttype)",
            "%(objectname)",
            "%(*objectname)",
            "%(contents:subject)",
            "%(creatordate:unix)",
        ]
        .join("%1f");
        let out = self
            .text(
                repo,
                &[
                    "for-each-ref",
                    "--sort=-creatordate",
                    &format!("--format={format}"),
                    "refs/tags",
                ],
            )
            .await?;
        Ok(out
            .lines()
            .filter_map(|line| {
                let parts: Vec<&str> = line.split(FIELD).collect();
                let [name, kind, object, peeled, subject, time] = parts[..] else {
                    return None;
                };
                let annotated = kind == "tag";
                let sha = if annotated && !peeled.is_empty() {
                    peeled
                } else {
                    object
                };
                Some(TagInfo {
                    name: name.to_string(),
                    sha: sha.to_string(),
                    short: short(sha),
                    annotated,
                    subject: subject.to_string(),
                    time: rfc3339(time.parse().unwrap_or(0)),
                })
            })
            .collect())
    }

    /// Tags a commit (HEAD when `rev` is `None`): annotated when there is a
    /// message, lightweight otherwise.
    pub async fn tag_create(
        &self,
        repo: &Path,
        name: &str,
        rev: Option<&str>,
        message: Option<&str>,
    ) -> Result<()> {
        self.check_name(repo, name, "tag").await?;
        let target = self.resolve_commit(repo, rev.unwrap_or("HEAD")).await?;
        let full = format!("refs/tags/{name}");
        let title = format!("Tagged {} as {name}", short(&target));
        let pending = self
            .journal_begin(repo, "tag_create", &title, &[full])
            .await?;
        let mut args = vec!["tag"];
        let message = message.map(str::trim).filter(|m| !m.is_empty());
        if let Some(message) = message {
            args.extend(["--annotate", "-m", message]);
        }
        args.extend([name, target.as_str()]);
        let out = self.exec(repo, &args, Call::default().no_editor()).await?;
        if !out.ok() {
            self.journal_cancel(repo, pending).await;
            return Err(if out.said().contains("already exists") {
                Error::Invalid(format!("There is already a tag called {name}."))
            } else {
                out.error()
            });
        }
        self.journal_finish(repo, pending, None).await?;
        Ok(())
    }

    /// Deletes a tag here (never on a remote).
    pub async fn tag_delete(&self, repo: &Path, name: &str) -> Result<()> {
        self.check_name(repo, name, "tag").await?;
        let full = format!("refs/tags/{name}");
        if self
            .maybe(repo, &["rev-parse", "--verify", "--quiet", &full])
            .await?
            .is_none()
        {
            return Err(Error::Invalid(format!(
                "There is no tag called {name} here."
            )));
        }
        let title = format!("Deleted tag {name}");
        let pending = self
            .journal_begin(repo, "tag_delete", &title, &[full])
            .await?;
        let out = self
            .exec(repo, &["tag", "--delete", name], Call::default())
            .await?;
        if !out.ok() {
            self.journal_cancel(repo, pending).await;
            return Err(out.error());
        }
        self.journal_finish(repo, pending, None).await?;
        Ok(())
    }

    /// Pushes one tag to a remote.
    pub async fn tag_push(&self, repo: &Path, name: &str, remote: &str) -> Result<()> {
        self.check_name(repo, name, "tag").await?;
        self.check_remote(repo, remote).await?;
        let spec = format!("refs/tags/{name}:refs/tags/{name}");
        self.exec(repo, &["push", "--quiet", remote, &spec], Call::default())
            .await?
            .into_result()
            .map(|_| ())
    }

    // --------------------------------------------------------------- remotes

    pub async fn remote_list(&self, repo: &Path) -> Result<Vec<RemoteEntry>> {
        let mut remotes = Vec::new();
        for name in self.remote_names(repo).await? {
            let url = self
                .maybe(repo, &["remote", "get-url", &name])
                .await?
                .unwrap_or_default();
            let push = self
                .maybe(repo, &["remote", "get-url", "--push", &name])
                .await?;
            remotes.push(RemoteEntry {
                push_url: push.filter(|p| p != &url),
                name,
                url,
            });
        }
        Ok(remotes)
    }

    fn check_url(url: &str) -> Result<&str> {
        let url = url.trim();
        // `ext::` and friends run commands; an option is never an address.
        let bad = url.is_empty()
            || url.starts_with('-')
            || url.contains("::")
            || url.chars().any(|c| c.is_control() || c.is_whitespace());
        if bad {
            Err(Error::Invalid("That isn’t an address git can use.".into()))
        } else {
            Ok(url)
        }
    }

    pub async fn remote_add(&self, repo: &Path, name: &str, url: &str) -> Result<()> {
        self.check_name(repo, name, "remote").await?;
        let url = Self::check_url(url)?;
        let out = self
            .exec(repo, &["remote", "add", name, url], Call::default())
            .await?;
        if out.ok() {
            Ok(())
        } else if out.said().contains("already exists") {
            Err(Error::Invalid(format!(
                "There is already a remote called {name}."
            )))
        } else {
            Err(out.error())
        }
    }

    pub async fn remote_rename(&self, repo: &Path, old: &str, new: &str) -> Result<()> {
        self.check_remote(repo, old).await?;
        self.check_name(repo, new, "remote").await?;
        let out = self
            .exec(repo, &["remote", "rename", old, new], Call::default())
            .await?;
        if out.ok() {
            Ok(())
        } else if out.said().contains("already exists") {
            Err(Error::Invalid(format!(
                "There is already a remote called {new}."
            )))
        } else {
            Err(out.error())
        }
    }

    /// Forgets a remote and its branches here. Nothing on the remote changes.
    pub async fn remote_remove(&self, repo: &Path, name: &str) -> Result<()> {
        self.check_remote(repo, name).await?;
        self.exec(repo, &["remote", "remove", name], Call::default())
            .await?
            .into_result()
            .map(|_| ())
    }

    pub async fn remote_set_url(&self, repo: &Path, name: &str, url: &str) -> Result<()> {
        self.check_remote(repo, name).await?;
        let url = Self::check_url(url)?;
        self.exec(repo, &["remote", "set-url", name, url], Call::default())
            .await?
            .into_result()
            .map(|_| ())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tracking_words_are_read() {
        assert_eq!(parse_track("ahead 2, behind 1"), (2, 1, false));
        assert_eq!(parse_track("behind 4"), (0, 4, false));
        assert_eq!(parse_track("gone"), (0, 0, true));
        assert_eq!(parse_track(""), (0, 0, false));
    }

    #[test]
    fn addresses_that_run_commands_are_refused() {
        assert!(Git::check_url("git@host:me/repo.git").is_ok());
        assert!(Git::check_url("https://host/me/repo.git").is_ok());
        assert!(Git::check_url("/a/local/path.git").is_ok());
        assert!(Git::check_url("ext::sh -c evil").is_err());
        assert!(Git::check_url("--upload-pack=evil").is_err());
        assert!(Git::check_url("").is_err());
    }
}
