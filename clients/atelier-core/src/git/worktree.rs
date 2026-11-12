//! Worktrees: the same repository open in a second folder on another
//! branch. Listing them, making one beside the repository, removing,
//! locking and pruning; and recognising a linked worktree's folder (whose
//! `.git` is a file) as part of its main repository.

use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use super::util::{count, read_trimmed, rfc3339, short, Call, FIELD};
use super::Git;
use crate::error::{Error, Result};

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct Worktree {
    pub path: String,
    /// The repository's first folder (where `.git` is a folder).
    pub main: bool,
    /// The one these questions were asked from.
    pub current: bool,
    /// `None` with a detached HEAD.
    pub branch: Option<String>,
    pub head: Option<String>,
    pub head_short: Option<String>,
    pub locked: bool,
    pub lock_reason: Option<String>,
    /// Its folder is gone; `prune` forgets it.
    pub prunable: bool,
    /// Uncommitted files (staged, changed or untracked).
    pub dirty: u32,
    pub upstream: Option<String>,
    pub ahead: u64,
    pub behind: u64,
    pub subject: Option<String>,
    /// RFC 3339, of the last commit.
    pub time: Option<String>,
}

/// What a new worktree checks out.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum WorktreeSource {
    /// A branch that exists here already.
    Branch { name: String },
    /// A new branch, starting from a branch, a fetched ref or a commit
    /// (`origin/feature/x`, `refs/pull/14/head`); HEAD when `from` is empty.
    NewBranch {
        name: String,
        #[serde(default)]
        from: Option<String>,
    },
    /// A commit, with no branch.
    Detached { rev: String },
}

/// Where a folder belongs: its own top, and the repository it is part of.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct RepoIdentity {
    pub toplevel: PathBuf,
    /// The main working tree. The same as `toplevel` unless `linked`.
    pub main: PathBuf,
    /// This folder is a linked worktree.
    pub linked: bool,
}

/// `feature/tunnel ui` → `feature-tunnel-ui`.
pub fn branch_slug(branch: &str) -> String {
    let mut slug = String::new();
    for c in branch.chars() {
        if c.is_ascii_alphanumeric() || matches!(c, '.' | '_') {
            slug.push(c);
        } else if !slug.ends_with('-') {
            slug.push('-');
        }
    }
    let slug = slug.trim_matches(['-', '.']).to_string();
    if slug.is_empty() {
        "worktree".to_string()
    } else {
        slug
    }
}

/// The main working tree a linked worktree belongs to, read from the files
/// alone (no git run): `dir/.git` is a file `gitdir: <main>/.git/worktrees/x`
/// and that folder's `commondir` leads to the main `.git`. `None` for
/// anything else, a main working tree and a submodule included.
pub fn linked_worktree_main(dir: &Path) -> Option<PathBuf> {
    let dot_git = dir.join(".git");
    if !dot_git.is_file() {
        return None;
    }
    let text = std::fs::read_to_string(&dot_git).ok()?;
    let target = text.lines().next()?.strip_prefix("gitdir:")?.trim();
    let git_dir = if Path::new(target).is_absolute() {
        PathBuf::from(target)
    } else {
        dir.join(target)
    };
    let common = read_trimmed(&git_dir.join("commondir"))?;
    let common = if Path::new(&common).is_absolute() {
        PathBuf::from(common)
    } else {
        git_dir.join(common)
    };
    let common = common.canonicalize().ok()?;
    // Only `…/.git/worktrees/<name>` is a worktree; a submodule's git
    // folder lives under `modules` and has no `commondir`.
    if git_dir.parent()?.file_name()? != "worktrees" || common.file_name()? != ".git" {
        return None;
    }
    common.parent().map(Path::to_path_buf)
}

impl Git {
    /// Where `path` belongs: the top of its working tree, and the main
    /// working tree of the repository when it is a linked worktree.
    pub async fn repo_identity(&self, path: &Path) -> Result<RepoIdentity> {
        let toplevel = self
            .toplevel(path)
            .await?
            .ok_or_else(|| Error::NotARepo(path.to_path_buf()))?;
        let git_dir = self.git_dir(&toplevel).await?;
        let common = self.common_dir(&toplevel).await?;
        let git_dir = git_dir.canonicalize().unwrap_or(git_dir);
        let linked = git_dir != common;
        let main = if linked {
            common
                .parent()
                .filter(|_| common.file_name().is_some_and(|n| n == ".git"))
                .map(Path::to_path_buf)
                .unwrap_or_else(|| toplevel.clone())
        } else {
            toplevel.clone()
        };
        Ok(RepoIdentity {
            toplevel,
            main,
            linked,
        })
    }

    /// Every worktree of the repository, the main one first.
    pub async fn worktree_list(&self, repo: &Path) -> Result<Vec<Worktree>> {
        let here = self
            .toplevel(repo)
            .await?
            .map(|p| p.canonicalize().unwrap_or(p));
        let out = self
            .text(repo, &["worktree", "list", "--porcelain", "-z"])
            .await?;
        let mut trees: Vec<Worktree> = Vec::new();
        // Records are separated by an empty field.
        for record in out.split("\0\0") {
            let mut tree: Option<Worktree> = None;
            let mut bare = false;
            for field in record.split('\0').filter(|f| !f.is_empty()) {
                let (key, value) = field.split_once(' ').unwrap_or((field, ""));
                match (key, tree.as_mut()) {
                    ("worktree", _) => {
                        tree = Some(Worktree {
                            path: value.to_string(),
                            main: trees.is_empty(),
                            current: false,
                            branch: None,
                            head: None,
                            head_short: None,
                            locked: false,
                            lock_reason: None,
                            prunable: false,
                            dirty: 0,
                            upstream: None,
                            ahead: 0,
                            behind: 0,
                            subject: None,
                            time: None,
                        });
                    }
                    ("HEAD", Some(t)) => {
                        t.head_short = Some(short(value));
                        t.head = Some(value.to_string());
                    }
                    ("branch", Some(t)) => {
                        t.branch = Some(value.trim_start_matches("refs/heads/").to_string());
                    }
                    ("locked", Some(t)) => {
                        t.locked = true;
                        t.lock_reason = Some(value.to_string()).filter(|r| !r.is_empty());
                    }
                    ("prunable", Some(t)) => t.prunable = true,
                    ("bare", _) => bare = true,
                    _ => {}
                }
            }
            if let (Some(tree), false) = (tree, bare) {
                trees.push(tree);
            }
        }
        for tree in &mut trees {
            let path = PathBuf::from(&tree.path);
            let real = path.canonicalize().unwrap_or_else(|_| path.clone());
            tree.current = here.as_ref() == Some(&real);
            if tree.prunable || !path.is_dir() {
                tree.prunable = true;
                continue;
            }
            tree.dirty = self
                .text(&path, &["status", "--porcelain"])
                .await
                .map(|s| s.lines().filter(|l| !l.is_empty()).count() as u32)
                .unwrap_or(0);
            let format = format!("--format=%s{FIELD}%ct");
            if let Ok(Some(line)) = self.maybe(&path, &["log", "-1", &format]).await {
                if let Some((subject, time)) = line.split_once(FIELD) {
                    tree.subject = Some(subject.to_string());
                    tree.time = time.parse().ok().map(rfc3339);
                }
            }
            tree.upstream = self
                .maybe(
                    &path,
                    &[
                        "rev-parse",
                        "--abbrev-ref",
                        "--symbolic-full-name",
                        "@{upstream}",
                    ],
                )
                .await?;
            if tree.upstream.is_some() {
                let counts = self
                    .maybe(
                        &path,
                        &["rev-list", "--left-right", "--count", "@{upstream}...HEAD"],
                    )
                    .await?
                    .unwrap_or_default();
                let mut parts = counts.split_whitespace().map(|n| n.parse().unwrap_or(0));
                tree.behind = parts.next().unwrap_or(0);
                tree.ahead = parts.next().unwrap_or(0);
            }
        }
        Ok(trees)
    }

    /// Where a worktree for `branch` would go by default:
    /// `<repo>.worktrees/<branch-slug>`, beside the repository and never
    /// inside it.
    pub async fn worktree_suggest_path(&self, repo: &Path, branch: &str) -> Result<PathBuf> {
        let main = self.repo_identity(repo).await?.main;
        let name = main
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_else(|| "repo".to_string());
        let parent = main.parent().unwrap_or(&main);
        Ok(parent
            .join(format!("{name}.worktrees"))
            .join(branch_slug(branch)))
    }

    /// Makes a worktree at `path`. Returns it as listed.
    pub async fn worktree_add(
        &self,
        repo: &Path,
        path: &Path,
        source: &WorktreeSource,
    ) -> Result<Worktree> {
        if !path.is_absolute() {
            return Err(Error::Invalid(
                "Choose a full path for the new folder.".into(),
            ));
        }
        let main = self.repo_identity(repo).await?.main;
        if path.starts_with(&main) {
            return Err(Error::Invalid(
                "A worktree is kept beside the repository, not inside it.".into(),
            ));
        }
        let taken = std::fs::read_dir(path)
            .map(|mut entries| entries.next().is_some())
            .unwrap_or(false);
        if taken || path.is_file() {
            return Err(Error::Invalid(format!(
                "{} already exists and isn’t empty.",
                path.display()
            )));
        }
        let listed = self.worktree_list(repo).await?;
        let in_use = |branch: &str| {
            listed
                .iter()
                .find(|t| t.branch.as_deref() == Some(branch))
                .map(|t| t.path.clone())
        };
        let target = path.to_string_lossy().into_owned();
        let mut args: Vec<String> = vec!["worktree".into(), "add".into(), "--quiet".into()];
        let (label, new_branch) = match source {
            WorktreeSource::Branch { name } => {
                self.check_name(repo, name, "branch").await?;
                if let Some(at) = in_use(name) {
                    return Err(Error::Invalid(format!("{name} is already open in {at}.")));
                }
                let full = format!("refs/heads/{name}");
                if self
                    .maybe(repo, &["rev-parse", "--verify", "--quiet", &full])
                    .await?
                    .is_none()
                {
                    return Err(Error::Invalid(format!(
                        "There is no branch called {name} here."
                    )));
                }
                args.extend([target.clone(), name.clone()]);
                (name.clone(), None)
            }
            WorktreeSource::NewBranch { name, from } => {
                self.check_name(repo, name, "branch").await?;
                let from = match from.as_deref().map(str::trim).filter(|f| !f.is_empty()) {
                    Some(from) => self.resolve_commit(repo, from).await.map(|_| from)?,
                    None => "HEAD",
                };
                args.extend(["-b".into(), name.clone(), target.clone(), from.to_string()]);
                (name.clone(), Some(name.clone()))
            }
            WorktreeSource::Detached { rev } => {
                let sha = self.resolve_commit(repo, rev).await?;
                args.extend(["--detach".into(), target.clone(), sha.clone()]);
                (short(&sha), None)
            }
        };
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent)
                .map_err(|e| Error::io(format!("Couldn’t create {}", parent.display()), e))?;
        }
        let title = format!("Opened {label} in a new worktree");
        let pending = self
            .journal_begin(repo, "worktree_add", &title, &[])
            .await?;
        let out = self.exec(repo, &args, Call::default()).await?;
        if !out.ok() {
            self.journal_cancel(repo, pending).await;
            let said = out.said();
            return Err(if said.contains("already exists") {
                Error::Invalid(format!("There is already a branch called {label}."))
            } else {
                out.error()
            });
        }
        let real = path.canonicalize().unwrap_or_else(|_| path.to_path_buf());
        self.journal_note(repo, &pending, "path", &real.to_string_lossy())
            .await;
        if let Some(branch) = &new_branch {
            self.journal_note(repo, &pending, "new_branch", branch)
                .await;
        }
        self.journal_finish(repo, pending, None).await?;
        self.worktree_list(repo)
            .await?
            .into_iter()
            .find(|t| Path::new(&t.path).canonicalize().ok().as_ref() == Some(&real))
            .ok_or_else(|| Error::Invalid("The worktree was made but can’t be found.".into()))
    }

    async fn worktree_find(&self, repo: &Path, path: &Path) -> Result<Worktree> {
        let real = path.canonicalize().unwrap_or_else(|_| path.to_path_buf());
        self.worktree_list(repo)
            .await?
            .into_iter()
            .find(|t| {
                let listed = PathBuf::from(&t.path);
                listed == path || listed.canonicalize().unwrap_or(listed) == real
            })
            .ok_or_else(|| {
                Error::Invalid(format!(
                    "{} isn’t a worktree of this repository.",
                    path.display()
                ))
            })
    }

    /// Removes a worktree's folder; its branch and commits stay. Refused
    /// while it has uncommitted changes or is locked, unless `force`: then
    /// the uncommitted changes go to Recently discarded first.
    pub async fn worktree_remove(&self, repo: &Path, path: &Path, force: bool) -> Result<()> {
        let tree = self.worktree_find(repo, path).await?;
        if tree.main {
            return Err(Error::Invalid(
                "This is the repository’s main folder. It can’t be removed as a worktree.".into(),
            ));
        }
        if tree.current {
            return Err(Error::Invalid(
                "You are in this worktree. Open the main folder to remove it.".into(),
            ));
        }
        if tree.locked && !force {
            return Err(Error::Invalid(match &tree.lock_reason {
                Some(reason) => format!("This worktree is locked: {reason}."),
                None => "This worktree is locked.".to_string(),
            }));
        }
        if tree.dirty > 0 && !force {
            return Err(Error::Invalid(format!(
                "This worktree has {} — commit, put aside or discard them first.",
                count(
                    u64::from(tree.dirty),
                    "uncommitted change",
                    "uncommitted changes"
                )
            )));
        }
        let folder = PathBuf::from(&tree.path);
        if tree.dirty > 0 && folder.is_dir() {
            if let Some(head) = &tree.head {
                let before = self.snapshot_tree(&folder, None).await?;
                let base = self
                    .line(&folder, &["rev-parse", &format!("{head}^{{tree}}")])
                    .await?;
                let name = tree.branch.clone().unwrap_or_else(|| short(head));
                self.discard_record(
                    repo,
                    &format!("Discarded changes in the worktree of {name}"),
                    &before,
                    &base,
                    true,
                )
                .await?;
            }
        }
        let label = tree
            .branch
            .clone()
            .or(tree.head_short.clone())
            .unwrap_or_default();
        let title = format!("Removed the worktree of {label}");
        let pending = self
            .journal_begin(repo, "worktree_remove", &title, &[])
            .await?;
        let mut args = vec!["worktree", "remove"];
        if force {
            args.extend(["--force", "--force"]);
        }
        args.push(&tree.path);
        let out = self.exec(repo, &args, Call::default()).await?;
        if !out.ok() {
            self.journal_cancel(repo, pending).await;
            return Err(out.error());
        }
        self.journal_note(repo, &pending, "path", &tree.path).await;
        if let Some(branch) = &tree.branch {
            self.journal_note(repo, &pending, "branch", branch).await;
        }
        if let Some(head) = &tree.head {
            self.journal_note(repo, &pending, "head", head).await;
        }
        self.journal_finish(repo, pending, None).await?;
        Ok(())
    }

    /// Locks a worktree so it isn't removed or pruned (`reason` is shown).
    pub async fn worktree_lock(&self, repo: &Path, path: &Path, reason: &str) -> Result<()> {
        let tree = self.worktree_find(repo, path).await?;
        if tree.main {
            return Err(Error::Invalid(
                "The main folder doesn’t need a lock.".into(),
            ));
        }
        if tree.locked {
            return Ok(());
        }
        let mut args = vec!["worktree", "lock"];
        let reason = reason.trim();
        if !reason.is_empty() {
            args.extend(["--reason", reason]);
        }
        args.push(&tree.path);
        self.exec(repo, &args, Call::default())
            .await?
            .into_result()
            .map(|_| ())
    }

    pub async fn worktree_unlock(&self, repo: &Path, path: &Path) -> Result<()> {
        let tree = self.worktree_find(repo, path).await?;
        if !tree.locked {
            return Ok(());
        }
        self.exec(repo, &["worktree", "unlock", &tree.path], Call::default())
            .await?
            .into_result()
            .map(|_| ())
    }

    /// Forgets worktrees whose folders are gone. Returns their paths.
    pub async fn worktree_prune(&self, repo: &Path) -> Result<Vec<String>> {
        let gone: Vec<String> = self
            .worktree_list(repo)
            .await?
            .into_iter()
            .filter(|t| t.prunable && !t.locked)
            .map(|t| t.path)
            .collect();
        self.exec(repo, &["worktree", "prune"], Call::default())
            .await?
            .into_result()?;
        Ok(gone)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn slugs_are_safe_folder_names() {
        assert_eq!(branch_slug("feature/tunnel-ui"), "feature-tunnel-ui");
        assert_eq!(branch_slug("fix/setup race"), "fix-setup-race");
        assert_eq!(branch_slug("../../etc"), "etc");
        assert_eq!(branch_slug("v1.2_x"), "v1.2_x");
        assert_eq!(branch_slug("///"), "worktree");
    }

    #[test]
    fn a_linked_worktree_points_at_its_main_folder() {
        let dir = tempfile::tempdir().unwrap();
        let base = dir.path().canonicalize().unwrap();
        let main = base.join("repo");
        let admin = main.join(".git/worktrees/side");
        let side = base.join("repo.worktrees/side");
        std::fs::create_dir_all(&admin).unwrap();
        std::fs::create_dir_all(&side).unwrap();
        std::fs::write(admin.join("commondir"), "../..\n").unwrap();
        std::fs::write(side.join(".git"), format!("gitdir: {}\n", admin.display())).unwrap();
        assert_eq!(linked_worktree_main(&side), Some(main.clone()));
        // The main folder and a plain folder are not linked worktrees.
        assert_eq!(linked_worktree_main(&main), None);
        assert_eq!(linked_worktree_main(&base), None);
        // A submodule's `.git` file leads to `modules`, not `worktrees`.
        let module = main.join(".git/modules/dep");
        let sub = main.join("dep");
        std::fs::create_dir_all(&module).unwrap();
        std::fs::create_dir_all(&sub).unwrap();
        std::fs::write(sub.join(".git"), "gitdir: ../.git/modules/dep\n").unwrap();
        assert_eq!(linked_worktree_main(&sub), None);
    }
}
