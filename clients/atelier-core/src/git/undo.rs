//! Recent actions and Recently discarded: the journal that makes every
//! history- or worktree-changing action undoable.
//!
//! Before such an action the app writes one line to
//! `<git dir>/atelier/journal.jsonl`: when, what (in plain words), where HEAD
//! and the refs involved stood, and, when the working tree had uncommitted
//! work, commits that hold the working tree and the index as they were.
//! Those objects are kept alive by refs under `refs/atelier/undo/<id>/…`.
//! Work that is thrown away (a discarded file, chunk or line, a hard reset)
//! is kept as a commit under `refs/atelier/discarded/<id>`. Everything is
//! pruned after thirty days. Nothing is ever written to the working tree.

use std::collections::BTreeMap;
use std::io::Write;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use super::util::{count, now_unix, rfc3339, short, unique, Call};
use super::Git;
use crate::error::{Error, Result};

/// How long Recent actions and Recently discarded are kept.
pub const KEEP_DAYS: i64 = 30;
const JOURNAL: &str = "journal.jsonl";
const UNDO_REFS: &str = "refs/atelier/undo";
const DISCARD_REFS: &str = "refs/atelier/discarded";

/// Where HEAD was.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct HeadState {
    pub sha: Option<String>,
    /// `None` when HEAD was detached (or before the first commit).
    pub branch: Option<String>,
}

/// One ref an action may move, and where it was before.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct RefState {
    /// The full name (`refs/heads/main`).
    pub name: String,
    /// `None`: it didn't exist.
    pub before: Option<String>,
}

/// Uncommitted work that was thrown away, kept as a commit.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Discarded {
    /// Its tree is the files as they were; its parent's tree is the files
    /// without the discarded work.
    pub commit: String,
    pub files: u32,
    pub additions: u32,
    pub deletions: u32,
    pub paths: Vec<String>,
    /// It was brought back since.
    #[serde(default)]
    pub restored: bool,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct StashRef {
    pub sha: String,
    pub message: String,
}

/// One line of Recent actions.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct JournalEntry {
    pub id: u64,
    pub unix: i64,
    /// RFC 3339.
    pub time: String,
    /// `commit`, `amend`, `merge`, `rebase`, `tidy`, `cherry_pick`, `revert`,
    /// `reset`, `pull`, `switch`, `branch_create`, `branch_rename`,
    /// `branch_delete`, `tag_create`, `tag_delete`, `stash_save`,
    /// `stash_apply`, `stash_pop`, `stash_drop`, `worktree_add`,
    /// `worktree_remove`, `discard`, `undo`, `go_back`.
    pub kind: String,
    /// Plain words: `Rebased feature/x onto main`.
    pub title: String,
    /// The working tree it happened in.
    pub worktree: String,
    pub head: HeadState,
    pub refs: Vec<RefState>,
    /// A commit holding the working tree as it was (untracked files too),
    /// when there was uncommitted work.
    #[serde(default)]
    pub work: Option<String>,
    #[serde(default)]
    pub work_tree: Option<String>,
    /// A commit holding the index as it was.
    #[serde(default)]
    pub index: Option<String>,
    /// Where HEAD stood once the action was done.
    #[serde(default)]
    pub after_head: Option<HeadState>,
    /// The working tree once the action was done (`None`: clean).
    #[serde(default)]
    pub after_tree: Option<String>,
    /// False while the action is still under way (a rebase that stopped).
    #[serde(default)]
    pub finished: bool,
    #[serde(default)]
    pub stash: Option<StashRef>,
    #[serde(default)]
    pub discarded: Option<Discarded>,
    /// Small facts an undo needs (`path`, `branch`, `upstream`, …).
    #[serde(default)]
    pub detail: BTreeMap<String, String>,
    /// It was undone since.
    #[serde(default)]
    pub undone: bool,
    /// What going back to before this does, in plain words.
    #[serde(default)]
    pub how: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(tag = "r", rename_all = "snake_case")]
enum Record {
    Entry {
        entry: Box<JournalEntry>,
    },
    Finish {
        id: u64,
        head: HeadState,
        tree: Option<String>,
        title: Option<String>,
    },
    Cancel {
        id: u64,
    },
    Undone {
        id: u64,
    },
    Restored {
        id: u64,
    },
    Stash {
        id: u64,
        stash: StashRef,
    },
    Detail {
        id: u64,
        key: String,
        value: String,
    },
}

/// An action that was written down and is now being done.
#[derive(Debug)]
pub(crate) struct Pending {
    pub id: u64,
}

/// The files as they were before something is thrown away.
#[derive(Debug)]
pub(crate) struct DiscardKeep {
    before: String,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct UndoOptions {
    /// Put the current uncommitted changes aside first.
    #[serde(default)]
    pub put_aside: bool,
    /// Go back even though commits were made since that the journal doesn't
    /// know about (they stay reachable from Recent actions).
    #[serde(default)]
    pub force: bool,
}

/// Why an undo didn't happen, and what would let it.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct Refusal {
    /// `nothing`, `busy`, `dirty` or `moved`.
    pub reason: String,
    pub message: String,
    pub can_put_aside: bool,
    pub can_force: bool,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct UndoOutcome {
    pub done: bool,
    /// `Undid “Rebased feature/x onto main”`.
    pub title: String,
    pub refusal: Option<Refusal>,
    /// The message of the put-aside entry made first, when one was.
    pub put_aside: Option<String>,
    /// The journal entry of the undo itself.
    pub journal: Option<u64>,
}

impl UndoOutcome {
    fn refused(reason: &str, message: impl Into<String>, put_aside: bool, force: bool) -> Self {
        UndoOutcome {
            done: false,
            title: String::new(),
            refusal: Some(Refusal {
                reason: reason.to_string(),
                message: message.into(),
                can_put_aside: put_aside,
                can_force: force,
            }),
            put_aside: None,
            journal: None,
        }
    }
}

fn how_for(kind: &str, head: &HeadState, dirty: bool) -> String {
    let place = match (&head.branch, &head.sha) {
        (Some(branch), Some(sha)) => format!("{branch} back at {}", short(sha)),
        (None, Some(sha)) => format!("HEAD back at {}", short(sha)),
        _ => "things back as they were".to_string(),
    };
    let changes = if dirty {
        ", with your uncommitted changes as they were"
    } else {
        ""
    };
    match kind {
        "discard" => "Brings the discarded changes back into your files.".to_string(),
        "stash_drop" => "Puts the dropped entry back in the put-aside list.".to_string(),
        "branch_delete" => "Makes the branch again, at the commit it was on.".to_string(),
        "branch_create" => "Removes the branch again.".to_string(),
        "branch_rename" => "Gives the branch its old name back.".to_string(),
        "tag_create" => "Removes the tag again.".to_string(),
        "tag_delete" => "Makes the tag again.".to_string(),
        "worktree_add" => "Removes the worktree folder again.".to_string(),
        "worktree_remove" => "Makes the worktree folder again, on its branch.".to_string(),
        _ => format!("Puts {place}{changes}."),
    }
}

fn fold(records: Vec<Record>) -> Vec<JournalEntry> {
    let mut entries: Vec<JournalEntry> = Vec::new();
    for record in records {
        let find = |entries: &mut Vec<JournalEntry>, id: u64| -> Option<usize> {
            entries.iter().position(|e| e.id == id)
        };
        match record {
            Record::Entry { entry } => entries.push(*entry),
            Record::Finish {
                id,
                head,
                tree,
                title,
            } => {
                if let Some(at) = find(&mut entries, id) {
                    let entry = &mut entries[at];
                    entry.after_head = Some(head);
                    entry.after_tree = tree;
                    entry.finished = true;
                    if let Some(title) = title {
                        entry.title = title;
                    }
                }
            }
            Record::Cancel { id } => entries.retain(|e| e.id != id),
            Record::Undone { id } => {
                if let Some(at) = find(&mut entries, id) {
                    entries[at].undone = true;
                }
            }
            Record::Restored { id } => {
                if let Some(at) = find(&mut entries, id) {
                    entries[at].undone = true;
                    if let Some(d) = entries[at].discarded.as_mut() {
                        d.restored = true;
                    }
                }
            }
            Record::Stash { id, stash } => {
                if let Some(at) = find(&mut entries, id) {
                    entries[at].stash = Some(stash);
                }
            }
            Record::Detail { id, key, value } => {
                if let Some(at) = find(&mut entries, id) {
                    entries[at].detail.insert(key, value);
                }
            }
        }
    }
    entries
}

impl Git {
    async fn journal_path(&self, repo: &Path) -> Result<PathBuf> {
        Ok(self.app_dir(repo).await?.join(JOURNAL))
    }

    async fn worktree_key(&self, repo: &Path) -> Result<String> {
        Ok(self
            .toplevel(repo)
            .await?
            .unwrap_or_else(|| repo.to_path_buf())
            .to_string_lossy()
            .into_owned())
    }

    async fn journal_append(&self, repo: &Path, record: &Record) -> Result<()> {
        let path = self.journal_path(repo).await?;
        let line = serde_json::to_string(record)
            .map_err(|e| Error::Invalid(format!("Couldn’t write the journal: {e}")))?;
        let mut file = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(&path)
            .map_err(|e| Error::io(format!("Couldn’t open {}", path.display()), e))?;
        writeln!(file, "{line}")
            .map_err(|e| Error::io(format!("Couldn’t write {}", path.display()), e))
    }

    /// Every entry of every worktree, oldest first.
    async fn journal_read(&self, repo: &Path) -> Result<Vec<JournalEntry>> {
        let path = self.journal_path(repo).await?;
        let text = match std::fs::read_to_string(&path) {
            Ok(text) => text,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(e) => return Err(Error::io(format!("Couldn’t read {}", path.display()), e)),
        };
        // A line that can't be read (cut short by a crash) is skipped.
        let records = text
            .lines()
            .filter_map(|line| serde_json::from_str::<Record>(line).ok())
            .collect();
        Ok(fold(records))
    }

    async fn update_ref(&self, repo: &Path, name: &str, value: Option<&str>) -> Result<()> {
        let args: Vec<&str> = match value {
            Some(value) => vec!["update-ref", name, value],
            None => vec!["update-ref", "-d", name],
        };
        self.exec(repo, &args, Call::default())
            .await?
            .into_result()
            .map(|_| ())
    }

    async fn drop_refs(&self, repo: &Path, prefix: &str) {
        let Ok(names) = self
            .text(repo, &["for-each-ref", "--format=%(refname)", prefix])
            .await
        else {
            return;
        };
        for name in names.lines().filter(|n| !n.is_empty()) {
            let _ = self.update_ref(repo, name, None).await;
        }
    }

    /// The full name of the branch that is checked out, if one is.
    pub(crate) async fn current_ref(&self, repo: &Path) -> Result<Option<String>> {
        self.maybe(repo, &["symbolic-ref", "-q", "HEAD"]).await
    }

    pub(crate) async fn head_state(&self, repo: &Path) -> Result<HeadState> {
        Ok(HeadState {
            sha: self.head_sha(repo).await?,
            branch: self.current_branch(repo).await?,
        })
    }

    /// The tree of the files as they are now: the index plus the working
    /// tree (untracked files too) of `paths`, or of everything. Made through
    /// a scratch index, so the real one is never touched.
    pub(crate) async fn snapshot_tree(
        &self,
        repo: &Path,
        paths: Option<&[String]>,
    ) -> Result<String> {
        let scratch = self.app_dir_local(repo).await?.join(unique("index"));
        let real = self
            .maybe(repo, &["rev-parse", "--git-path", "index"])
            .await?
            .map(PathBuf::from)
            .map(|p| if p.is_absolute() { p } else { repo.join(p) });
        if let Some(real) = real.filter(|p| p.exists()) {
            std::fs::copy(&real, &scratch).map_err(|e| Error::io("Couldn’t copy the index", e))?;
        }
        let call = || Call::default().literal().env("GIT_INDEX_FILE", &scratch);
        let result = async {
            match paths {
                None => {
                    self.exec(repo, &["add", "--all"], call())
                        .await?
                        .into_result()?;
                }
                Some(paths) => {
                    for path in paths {
                        // A file that is gone from both sides matches
                        // nothing, which is fine: it isn't in the tree.
                        let out = self
                            .exec(repo, &["add", "--all", "--", path.as_str()], call())
                            .await?;
                        if !out.ok() && !out.stderr.contains("did not match any files") {
                            return Err(out.error());
                        }
                    }
                }
            }
            Ok(self
                .exec(repo, &["write-tree"], call())
                .await?
                .into_result()?
                .trimmed())
        }
        .await;
        let _ = std::fs::remove_file(&scratch);
        let _ = std::fs::remove_file(scratch.with_extension("lock"));
        result
    }

    /// The working tree as a tree id, or `None` when nothing is uncommitted.
    async fn tree_now(&self, repo: &Path) -> Result<Option<String>> {
        if self.is_dirty(repo).await? {
            Ok(Some(self.snapshot_tree(repo, None).await?))
        } else {
            Ok(None)
        }
    }

    /// Wraps a tree in a commit, so a ref can keep it.
    async fn keep_tree(&self, repo: &Path, tree: &str, parent: Option<&str>) -> Result<String> {
        let mut args = vec!["commit-tree", tree, "-m", "Kept by Atelier"];
        if let Some(parent) = parent {
            args.extend(["-p", parent]);
        }
        Ok(self
            .exec(repo, &args, Call::default().bookkeeping())
            .await?
            .into_result()?
            .trimmed())
    }

    fn next_id(entries: &[JournalEntry]) -> u64 {
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_millis() as u64)
            .unwrap_or(0);
        let last = entries.iter().map(|e| e.id).max().unwrap_or(0);
        now.max(last + 1)
    }

    /// Writes down an action that is about to happen: where things stand,
    /// with the uncommitted work kept as commits when there is any.
    pub(crate) async fn journal_begin(
        &self,
        repo: &Path,
        kind: &str,
        title: &str,
        refs: &[String],
    ) -> Result<Pending> {
        self.journal_settle(repo).await?;
        let entries = self.journal_read(repo).await?;
        let id = Self::next_id(&entries);
        let head = self.head_state(repo).await?;
        let keep = format!("{UNDO_REFS}/{id}");

        let mut states = Vec::new();
        for (i, name) in refs.iter().enumerate() {
            let before = self
                .maybe(repo, &["rev-parse", "--verify", "--quiet", name])
                .await?;
            if let Some(sha) = &before {
                self.update_ref(repo, &format!("{keep}/r{i}"), Some(sha))
                    .await?;
            }
            states.push(RefState {
                name: name.clone(),
                before,
            });
        }
        if let Some(sha) = &head.sha {
            self.update_ref(repo, &format!("{keep}/head"), Some(sha))
                .await?;
        }
        let (mut work, mut work_tree, mut index) = (None, None, None);
        if self.is_dirty(repo).await? {
            let tree = self.snapshot_tree(repo, None).await?;
            let commit = self.keep_tree(repo, &tree, head.sha.as_deref()).await?;
            self.update_ref(repo, &format!("{keep}/work"), Some(&commit))
                .await?;
            work = Some(commit);
            work_tree = Some(tree);
            // The index can't be written as a tree while it has conflicts.
            let staged = self.exec(repo, &["write-tree"], Call::default()).await?;
            if staged.ok() {
                let commit = self
                    .keep_tree(repo, &staged.trimmed(), head.sha.as_deref())
                    .await?;
                self.update_ref(repo, &format!("{keep}/index"), Some(&commit))
                    .await?;
                index = Some(commit);
            }
        }
        let unix = now_unix();
        let entry = JournalEntry {
            id,
            unix,
            time: rfc3339(unix),
            kind: kind.to_string(),
            title: title.to_string(),
            worktree: self.worktree_key(repo).await?,
            how: how_for(kind, &head, work.is_some()),
            head,
            refs: states,
            work,
            work_tree,
            index,
            after_head: None,
            after_tree: None,
            finished: false,
            stash: None,
            discarded: None,
            detail: BTreeMap::new(),
            undone: false,
        };
        self.journal_append(
            repo,
            &Record::Entry {
                entry: Box::new(entry),
            },
        )
        .await?;
        Ok(Pending { id })
    }

    /// The action is done: writes down where things stand now.
    pub(crate) async fn journal_finish(
        &self,
        repo: &Path,
        pending: Pending,
        title: Option<String>,
    ) -> Result<u64> {
        let record = Record::Finish {
            id: pending.id,
            head: self.head_state(repo).await?,
            tree: self.tree_now(repo).await?,
            title,
        };
        self.journal_append(repo, &record).await?;
        Ok(pending.id)
    }

    /// The action didn't happen after all: forget it.
    pub(crate) async fn journal_cancel(&self, repo: &Path, pending: Pending) {
        let _ = self
            .journal_append(repo, &Record::Cancel { id: pending.id })
            .await;
        self.drop_refs(repo, &format!("{UNDO_REFS}/{}/", pending.id))
            .await;
    }

    pub(crate) async fn journal_note(
        &self,
        repo: &Path,
        pending: &Pending,
        key: &str,
        value: &str,
    ) {
        let _ = self
            .journal_append(
                repo,
                &Record::Detail {
                    id: pending.id,
                    key: key.to_string(),
                    value: value.to_string(),
                },
            )
            .await;
    }

    pub(crate) async fn journal_stash(&self, repo: &Path, pending: &Pending, stash: StashRef) {
        // The stash commit must outlive `git stash drop`.
        let _ = self
            .update_ref(
                repo,
                &format!("{UNDO_REFS}/{}/stash", pending.id),
                Some(&stash.sha),
            )
            .await;
        let _ = self
            .journal_append(
                repo,
                &Record::Stash {
                    id: pending.id,
                    stash,
                },
            )
            .await;
    }

    /// The action of this worktree that is still under way, if any.
    pub(crate) async fn journal_open(&self, repo: &Path) -> Result<Option<JournalEntry>> {
        let key = self.worktree_key(repo).await?;
        Ok(self
            .journal_read(repo)
            .await?
            .into_iter()
            .rev()
            .find(|e| e.worktree == key && !e.finished))
    }

    /// Finishes the action that was under way (a rebase that ran to its end).
    pub(crate) async fn journal_finish_open(&self, repo: &Path) -> Result<Option<u64>> {
        match self.journal_open(repo).await? {
            Some(entry) => Ok(Some(
                self.journal_finish(repo, Pending { id: entry.id }, None)
                    .await?,
            )),
            None => Ok(None),
        }
    }

    /// Forgets the action that was under way (it was stopped).
    pub(crate) async fn journal_cancel_open(&self, repo: &Path) -> Result<()> {
        if let Some(entry) = self.journal_open(repo).await? {
            self.journal_cancel(repo, Pending { id: entry.id }).await;
        }
        Ok(())
    }

    /// An action that was under way but isn't any more (finished or stopped
    /// in a terminal, say): close its entry by what actually happened.
    async fn journal_settle(&self, repo: &Path) -> Result<()> {
        let Some(open) = self.journal_open(repo).await? else {
            return Ok(());
        };
        if self.operation_in_progress(repo).await? {
            return Ok(());
        }
        let head = self.head_state(repo).await?;
        if head == open.head {
            self.journal_cancel(repo, Pending { id: open.id }).await;
        } else {
            self.journal_finish(repo, Pending { id: open.id }, None)
                .await?;
        }
        Ok(())
    }

    /// Drops entries older than thirty days before `now` (Unix seconds),
    /// with the objects they kept. Returns how many went.
    pub async fn journal_prune(&self, repo: &Path, now: i64) -> Result<usize> {
        let entries = self.journal_read(repo).await?;
        let cutoff = now - KEEP_DAYS * 86_400;
        let (old, keep): (Vec<_>, Vec<_>) = entries.into_iter().partition(|e| e.unix < cutoff);
        if old.is_empty() {
            return Ok(0);
        }
        for entry in &old {
            self.drop_refs(repo, &format!("{UNDO_REFS}/{}/", entry.id))
                .await;
            let _ = self
                .exec(
                    repo,
                    &["update-ref", "-d", &format!("{DISCARD_REFS}/{}", entry.id)],
                    Call::default(),
                )
                .await;
        }
        let path = self.journal_path(repo).await?;
        let mut text = String::new();
        for entry in keep {
            let record = Record::Entry {
                entry: Box::new(entry),
            };
            if let Ok(line) = serde_json::to_string(&record) {
                text.push_str(&line);
                text.push('\n');
            }
        }
        let scratch = path.with_extension("jsonl.new");
        std::fs::write(&scratch, text)
            .and_then(|()| std::fs::rename(&scratch, &path))
            .map_err(|e| Error::io(format!("Couldn’t write {}", path.display()), e))?;
        Ok(old.len())
    }

    /// Recent actions in this working tree, newest first. Actions still
    /// under way aren't listed.
    pub async fn journal_list(&self, repo: &Path) -> Result<Vec<JournalEntry>> {
        self.journal_settle(repo).await?;
        self.journal_prune(repo, now_unix()).await?;
        let key = self.worktree_key(repo).await?;
        let mut entries: Vec<JournalEntry> = self
            .journal_read(repo)
            .await?
            .into_iter()
            .filter(|e| e.worktree == key && e.finished)
            .collect();
        entries.reverse();
        Ok(entries)
    }

    /// What was thrown away in the last thirty days and not brought back.
    pub async fn discarded_list(&self, repo: &Path) -> Result<Vec<JournalEntry>> {
        Ok(self
            .journal_list(repo)
            .await?
            .into_iter()
            .filter(|e| e.discarded.as_ref().is_some_and(|d| !d.restored))
            .collect())
    }

    // ------------------------------------------------------------ discards

    /// Call before throwing uncommitted work away: remembers the files
    /// (`paths`, or everything) as they are.
    pub(crate) async fn discard_begin(
        &self,
        repo: &Path,
        paths: Option<&[String]>,
    ) -> Result<DiscardKeep> {
        Ok(DiscardKeep {
            before: self.snapshot_tree(repo, paths).await?,
        })
    }

    /// Call after: keeps what went missing as a Recently discarded entry.
    /// `None` when nothing changed.
    pub(crate) async fn discard_finish(
        &self,
        repo: &Path,
        keep: DiscardKeep,
        title: &str,
        paths: Option<&[String]>,
    ) -> Result<Option<u64>> {
        let base = self.snapshot_tree(repo, paths).await?;
        self.discard_record(repo, title, &keep.before, &base, false)
            .await
    }

    /// Keeps the difference between two trees (`base` → `before`) as
    /// discarded work. A `companion` belongs to a bigger action written down
    /// right after it (a hard reset): undoing that action brings the work
    /// back, so Undo steps over this entry.
    pub(crate) async fn discard_record(
        &self,
        repo: &Path,
        title: &str,
        before: &str,
        base: &str,
        companion: bool,
    ) -> Result<Option<u64>> {
        if before == base {
            return Ok(None);
        }
        let head = self.head_state(repo).await?;
        let without = self.keep_tree(repo, base, head.sha.as_deref()).await?;
        let commit = self.keep_tree(repo, before, Some(&without)).await?;
        let numstat = self
            .text(
                repo,
                &["diff", "--numstat", "-z", "--no-renames", &without, &commit],
            )
            .await?;
        let (mut files, mut additions, mut deletions) = (0u32, 0u32, 0u32);
        let mut paths = Vec::new();
        for item in numstat.split('\0') {
            let mut parts = item.trim_start_matches('\n').splitn(3, '\t');
            let (Some(a), Some(d), Some(path)) = (parts.next(), parts.next(), parts.next()) else {
                continue;
            };
            files += 1;
            additions += a.parse::<u32>().unwrap_or(0);
            deletions += d.parse::<u32>().unwrap_or(0);
            paths.push(path.to_string());
        }
        let entries = self.journal_read(repo).await?;
        let id = Self::next_id(&entries);
        self.update_ref(repo, &format!("{DISCARD_REFS}/{id}"), Some(&commit))
            .await?;
        let unix = now_unix();
        let tree = self.tree_now(repo).await?;
        let entry = JournalEntry {
            id,
            unix,
            time: rfc3339(unix),
            kind: "discard".to_string(),
            title: title.to_string(),
            worktree: self.worktree_key(repo).await?,
            how: how_for("discard", &head, false),
            head: head.clone(),
            refs: Vec::new(),
            work: None,
            work_tree: None,
            index: None,
            after_head: Some(head),
            after_tree: tree,
            finished: true,
            stash: None,
            discarded: Some(Discarded {
                commit,
                files,
                additions,
                deletions,
                paths,
                restored: false,
            }),
            detail: if companion {
                BTreeMap::from([("with".to_string(), "next".to_string())])
            } else {
                BTreeMap::new()
            },
            undone: false,
        };
        self.journal_append(
            repo,
            &Record::Entry {
                entry: Box::new(entry),
            },
        )
        .await?;
        Ok(Some(id))
    }

    /// Brings discarded work back into the working tree. When the files
    /// changed since in a way the old changes no longer fit, it refuses
    /// unless `overwrite` is set; then the files as they are now go to
    /// Recently discarded first and the old versions are put in their place.
    pub async fn restore_discarded(&self, repo: &Path, id: u64, overwrite: bool) -> Result<()> {
        let entry = self
            .journal_read(repo)
            .await?
            .into_iter()
            .find(|e| e.id == id)
            .ok_or_else(|| Error::Invalid("That entry isn’t kept any more.".into()))?;
        let discarded = entry
            .discarded
            .ok_or_else(|| Error::Invalid("That entry isn’t something discarded.".into()))?;
        if discarded.restored {
            return Err(Error::Invalid("That was already brought back.".into()));
        }
        let commit = discarded.commit.as_str();
        let without = format!("{commit}^");
        let patch = self
            .exec(
                repo,
                &[
                    "diff",
                    "--binary",
                    "--full-index",
                    "--no-renames",
                    "--no-ext-diff",
                    "--no-textconv",
                    "--no-color",
                    "--src-prefix=a/",
                    "--dst-prefix=b/",
                    &without,
                    commit,
                ],
                Call::default(),
            )
            .await?
            .into_result()?
            .stdout;
        let applied = self
            .exec(
                repo,
                &["apply", "--whitespace=nowarn", "-"],
                Call::default().stdin(&patch),
            )
            .await?;
        if !applied.ok() {
            if !overwrite {
                return Err(Error::Invalid(
                    "Those files have changed since, so the discarded changes no longer fit on top. You can replace the files with how they were instead; what is there now goes to Recently discarded."
                        .into(),
                ));
            }
            let keep = self.discard_begin(repo, Some(&discarded.paths)).await?;
            for path in &discarded.paths {
                let spec = format!("{commit}:{path}");
                let exists = self
                    .exec(repo, &["cat-file", "-e", &spec], Call::default())
                    .await?
                    .ok();
                if exists {
                    self.exec(
                        repo,
                        &["restore", "--source", commit, "--worktree", "--", path],
                        Call::default().literal(),
                    )
                    .await?
                    .into_result()?;
                } else {
                    let _ = std::fs::remove_file(repo.join(path));
                }
            }
            self.discard_finish(
                repo,
                keep,
                &format!(
                    "Replaced {} by an earlier version",
                    count(discarded.paths.len() as u64, "file", "files")
                ),
                Some(&discarded.paths),
            )
            .await?;
        }
        self.journal_append(repo, &Record::Restored { id }).await
    }

    /// Throws away everything uncommitted (staged, changed, untracked),
    /// keeping it in Recently discarded first.
    pub async fn discard_all(&self, repo: &Path) -> Result<Option<u64>> {
        if !self.is_dirty(repo).await? {
            return Ok(None);
        }
        let Some(head) = self.head_sha(repo).await? else {
            return Err(Error::Invalid(
                "There is no commit to go back to yet.".into(),
            ));
        };
        let before = self.snapshot_tree(repo, None).await?;
        let base = self
            .line(repo, &["rev-parse", &format!("{head}^{{tree}}")])
            .await?;
        self.exec(repo, &["reset", "--quiet", "--hard"], Call::default())
            .await?
            .into_result()?;
        self.exec(
            repo,
            &["clean", "--quiet", "--force", "-d"],
            Call::default(),
        )
        .await?
        .into_result()?;
        self.discard_record(
            repo,
            "Discarded all uncommitted changes",
            &before,
            &base,
            false,
        )
        .await
    }

    // ---------------------------------------------------------------- undo

    /// Puts things back to how they were before `entry`: refs, HEAD, index
    /// and working tree. The caller has checked that nothing is lost.
    async fn restore_before(&self, repo: &Path, entry: &JournalEntry) -> Result<()> {
        let run = |args: Vec<String>| async move {
            self.exec(repo, &args, Call::default().no_editor())
                .await?
                .into_result()
                .map(|_| ())
        };
        let s = |parts: &[&str]| parts.iter().map(|p| p.to_string()).collect::<Vec<_>>();
        match entry.kind.as_str() {
            "discard" => return self.restore_discarded(repo, entry.id, false).await,
            "stash_drop" => {
                if let Some(stash) = &entry.stash {
                    run(s(&["stash", "store", "-m", &stash.message, &stash.sha])).await?;
                }
                return Ok(());
            }
            "branch_rename" => {
                if let (Some(old), Some(new)) = (entry.detail.get("old"), entry.detail.get("new")) {
                    run(s(&["branch", "-m", new, old])).await?;
                }
                return Ok(());
            }
            "worktree_add" => {
                if let Some(path) = entry.detail.get("path") {
                    run(s(&["worktree", "remove", path])).await.map_err(|e| {
                        Error::Invalid(format!(
                            "The worktree at {path} can’t be removed while it has uncommitted changes ({e})."
                        ))
                    })?;
                }
                if let Some(branch) = entry.detail.get("new_branch") {
                    run(s(&["branch", "-D", branch])).await?;
                }
                return Ok(());
            }
            "worktree_remove" => {
                if let Some(path) = entry.detail.get("path") {
                    match (entry.detail.get("branch"), entry.detail.get("head")) {
                        (Some(branch), _) => run(s(&["worktree", "add", path, branch])).await?,
                        (None, Some(head)) => {
                            run(s(&["worktree", "add", "--detach", path, head])).await?
                        }
                        _ => {}
                    }
                }
                return Ok(());
            }
            _ => {}
        }

        let work_tree_before = entry.work_tree.clone();
        let moves_tree =
            entry.after_head.as_ref() != Some(&entry.head) || entry.after_tree != work_tree_before;
        let now = self.head_state(repo).await?;
        let current_ref = now.branch.as_ref().map(|b| format!("refs/heads/{b}"));

        if moves_tree {
            let Some(target) = entry.head.sha.as_deref() else {
                return Err(Error::Invalid(
                    "Going back to before the first commit isn’t possible here.".into(),
                ));
            };
            if self.is_dirty(repo).await? {
                // Everything becomes known to git, so the reset below
                // leaves nothing of the later state behind.
                run(s(&["add", "--all"])).await?;
            }
            let same_branch = entry.head.branch.is_some() && entry.head.branch == now.branch;
            for state in &entry.refs {
                let Some(before) = state.before.as_deref() else {
                    continue;
                };
                if same_branch && Some(&state.name) == current_ref.as_ref() {
                    continue;
                }
                self.update_ref(repo, &state.name, Some(before)).await?;
            }
            match (&entry.head.branch, same_branch) {
                (Some(_), true) => {}
                (Some(branch), false) => {
                    let full = format!("refs/heads/{branch}");
                    let exists = self
                        .maybe(repo, &["rev-parse", "--verify", "--quiet", &full])
                        .await?
                        .is_some();
                    if !exists {
                        self.update_ref(repo, &full, Some(target)).await?;
                    }
                    run(s(&["checkout", "--quiet", "--force", branch])).await?;
                }
                (None, _) => {
                    run(s(&["checkout", "--quiet", "--force", "--detach", target])).await?
                }
            }
            run(s(&["reset", "--quiet", "--hard", target])).await?;
            if let Some(work) = entry.work.as_deref() {
                run(s(&["read-tree", "--reset", "-u", work])).await?;
                let index = entry.index.as_deref().unwrap_or("HEAD");
                run(s(&["read-tree", index])).await?;
                let _ = self
                    .exec(repo, &["update-index", "-q", "--refresh"], Call::default())
                    .await;
            }
        } else {
            for state in &entry.refs {
                if let Some(before) = state.before.as_deref() {
                    self.update_ref(repo, &state.name, Some(before)).await?;
                }
            }
        }
        // Refs the action made go away again, now that HEAD is elsewhere.
        for state in &entry.refs {
            if state.before.is_none() {
                let _ = self.update_ref(repo, &state.name, None).await;
            }
        }
        match entry.kind.as_str() {
            "branch_delete" => {
                if let (Some(branch), Some(upstream)) =
                    (entry.detail.get("branch"), entry.detail.get("upstream"))
                {
                    let to = format!("--set-upstream-to={upstream}");
                    let _ = self
                        .exec(repo, &["branch", "--quiet", &to, branch], Call::default())
                        .await;
                }
            }
            "stash_save" => {
                if let Some(stash) = &entry.stash {
                    if let Some(at) = self.stash_position(repo, &stash.sha).await? {
                        run(s(&["stash", "drop", "--quiet", &format!("stash@{{{at}}}")])).await?;
                    }
                }
            }
            "stash_pop" => {
                if let Some(stash) = &entry.stash {
                    if self.stash_position(repo, &stash.sha).await?.is_none() {
                        run(s(&["stash", "store", "-m", &stash.message, &stash.sha])).await?;
                    }
                }
            }
            _ => {}
        }
        Ok(())
    }

    /// Restores the state before the action that is under way, exactly, and
    /// forgets the action. For stopping something git has no abort for.
    pub(crate) async fn restore_open(&self, repo: &Path) -> Result<bool> {
        let Some(mut entry) = self.journal_open(repo).await? else {
            return Ok(false);
        };
        // Whatever is in the tree now is the half-done action's.
        entry.after_head = None;
        self.restore_before(repo, &entry).await?;
        self.journal_cancel(repo, Pending { id: entry.id }).await;
        Ok(true)
    }

    async fn go_back(
        &self,
        repo: &Path,
        chain: Vec<JournalEntry>,
        options: UndoOptions,
        kind: &str,
        title: String,
    ) -> Result<UndoOutcome> {
        let Some(top) = chain.first() else {
            return Ok(UndoOutcome::refused(
                "nothing",
                "There is nothing to undo.",
                false,
                false,
            ));
        };
        if self.operation_in_progress(repo).await? {
            return Ok(UndoOutcome::refused(
                "busy",
                "Something is still under way here. Finish it or stop it first.",
                false,
                false,
            ));
        }
        let touches_tree = chain.iter().any(|e| {
            !matches!(
                e.kind.as_str(),
                "discard" | "stash_drop" | "branch_rename" | "worktree_add" | "worktree_remove"
            ) && (e.after_head.as_ref() != Some(&e.head) || e.after_tree != e.work_tree)
        });
        let mut put_aside = None;
        if touches_tree {
            let head = self.head_state(repo).await?;
            let moved = top
                .after_head
                .as_ref()
                .is_some_and(|after| after.sha != head.sha);
            if moved && !options.force {
                return Ok(UndoOutcome::refused(
                    "moved",
                    "There are commits here that were made after that action and outside Atelier. Going back would take them off the branch; they would stay reachable from Recent actions.",
                    false,
                    true,
                ));
            }
            let tree = self.tree_now(repo).await?;
            if tree.is_some() && tree != top.after_tree {
                if !options.put_aside {
                    return Ok(UndoOutcome::refused(
                        "dirty",
                        "You have uncommitted changes that going back would overwrite. Put them aside first, or commit them.",
                        true,
                        false,
                    ));
                }
                let message = format!("Before going back: {}", top.title);
                self.exec(
                    repo,
                    &[
                        "stash",
                        "push",
                        "--quiet",
                        "--include-untracked",
                        "-m",
                        &message,
                    ],
                    Call::default().bookkeeping(),
                )
                .await?
                .into_result()?;
                put_aside = Some(message);
            }
        }

        let mut refs: Vec<String> = Vec::new();
        refs.extend(self.current_ref(repo).await?);
        for entry in &chain {
            for state in &entry.refs {
                if !refs.contains(&state.name) {
                    refs.push(state.name.clone());
                }
            }
        }
        let pending = self.journal_begin(repo, kind, &title, &refs).await?;
        let mut failed = None;
        for entry in &chain {
            if let Err(e) = self.restore_before(repo, entry).await {
                failed = Some(e);
                break;
            }
            if entry.kind != "discard" {
                let _ = self
                    .journal_append(repo, &Record::Undone { id: entry.id })
                    .await;
            }
        }
        let journal = self.journal_finish(repo, pending, None).await?;
        if let Some(e) = failed {
            return Err(e);
        }
        Ok(UndoOutcome {
            done: true,
            title,
            refusal: None,
            put_aside,
            journal: Some(journal),
        })
    }

    /// Undoes the most recent action in this working tree that hasn't been
    /// undone yet. Nothing is lost by it: the undo is itself in the journal.
    pub async fn undo_last(&self, repo: &Path, options: UndoOptions) -> Result<UndoOutcome> {
        let last = self.journal_list(repo).await?.into_iter().find(|e| {
            !e.undone
                && !matches!(e.kind.as_str(), "undo" | "go_back")
                && !e.detail.contains_key("with")
        });
        match last {
            Some(entry) => {
                let title = format!("Undid “{}”", entry.title);
                self.go_back(repo, vec![entry], options, "undo", title)
                    .await
            }
            None => Ok(UndoOutcome::refused(
                "nothing",
                "There is nothing to undo.",
                false,
                false,
            )),
        }
    }

    /// Steps back through Recent actions to how things were just before
    /// the entry `id`.
    pub async fn go_back_to_before(
        &self,
        repo: &Path,
        id: u64,
        options: UndoOptions,
    ) -> Result<UndoOutcome> {
        let entries = self.journal_list(repo).await?;
        let Some(target) = entries.iter().find(|e| e.id == id) else {
            return Err(Error::Invalid("That entry isn’t kept any more.".into()));
        };
        let title = format!("Went back to before “{}”", target.title);
        // Newest first, down to and including the target. Discards that
        // were brought back already have nothing left to restore.
        let chain: Vec<JournalEntry> = entries
            .iter()
            .filter(|e| e.id >= id)
            .filter(|e| !e.discarded.as_ref().is_some_and(|d| d.restored))
            .filter(|e| !e.detail.contains_key("with"))
            .cloned()
            .collect();
        self.go_back(repo, chain, options, "go_back", title).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn entry(id: u64) -> JournalEntry {
        JournalEntry {
            id,
            unix: 0,
            time: String::new(),
            kind: "reset".into(),
            title: format!("action {id}"),
            worktree: "/w".into(),
            head: HeadState::default(),
            refs: Vec::new(),
            work: None,
            work_tree: None,
            index: None,
            after_head: None,
            after_tree: None,
            finished: false,
            stash: None,
            discarded: None,
            detail: BTreeMap::new(),
            undone: false,
            how: String::new(),
        }
    }

    #[test]
    fn records_fold_into_entries() {
        let records = vec![
            Record::Entry {
                entry: Box::new(entry(1)),
            },
            Record::Entry {
                entry: Box::new(entry(2)),
            },
            Record::Finish {
                id: 1,
                head: HeadState {
                    sha: Some("abc".into()),
                    branch: Some("main".into()),
                },
                tree: None,
                title: Some("renamed".into()),
            },
            Record::Cancel { id: 2 },
            Record::Undone { id: 1 },
            Record::Detail {
                id: 1,
                key: "path".into(),
                value: "/x".into(),
            },
        ];
        let entries = fold(records);
        assert_eq!(entries.len(), 1);
        assert!(entries[0].finished && entries[0].undone);
        assert_eq!(entries[0].title, "renamed");
        assert_eq!(entries[0].detail["path"], "/x");
    }

    #[test]
    fn a_record_survives_a_round_trip() {
        let line = serde_json::to_string(&Record::Entry {
            entry: Box::new(entry(7)),
        })
        .unwrap();
        assert!(line.starts_with("{\"r\":\"entry\""));
        let back: Record = serde_json::from_str(&line).unwrap();
        assert!(matches!(back, Record::Entry { entry } if entry.id == 7));
    }
}
