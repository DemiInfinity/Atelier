//! Local git helpers. Everything shells out to `git`; nothing here ever
//! writes inside a working tree (only `.git` changes, through git itself).

use std::ffi::{OsStr, OsString};
use std::path::{Path, PathBuf};
use std::process::Stdio;

use serde::Serialize;
use tokio::process::Command;

use crate::api::{CreateRepo, Repo};
use crate::client::Client;
use crate::error::{Error, Result};

pub mod blame;
pub mod branch;
pub mod conflict;
pub mod history;
pub mod integrate;
pub mod stage;
pub mod stash;
pub mod undo;
mod util;
mod work;
pub mod worktree;
pub use work::*;

/// How to run `git`.
#[derive(Clone, Debug)]
pub struct Git {
    program: OsString,
    envs: Vec<(OsString, OsString)>,
    ssh_identity: Option<PathBuf>,
}

impl Default for Git {
    fn default() -> Self {
        Self::new()
    }
}

/// Folders that never hold working copies worth looking into.
const SKIP_DIRS: [&str; 6] = [
    "node_modules",
    "target",
    "vendor",
    "Library",
    "build",
    "dist",
];

impl Git {
    /// `git` from the `PATH`. It never prompts on the terminal for
    /// credentials (`GIT_TERMINAL_PROMPT=0`).
    pub fn new() -> Self {
        Git {
            program: "git".into(),
            envs: vec![("GIT_TERMINAL_PROMPT".into(), "0".into())],
            ssh_identity: None,
        }
    }

    /// Another git binary.
    pub fn with_program(mut self, program: impl Into<OsString>) -> Self {
        self.program = program.into();
        self
    }

    /// An environment variable for every git call.
    pub fn env(mut self, key: impl Into<OsString>, value: impl Into<OsString>) -> Self {
        self.envs.push((key.into(), value.into()));
        self
    }

    /// Use this private key for SSH (the dedicated device key). Clones and
    /// published folders also remember it in their own `.git/config`
    /// (`core.sshCommand`), so plain `git push` keeps working afterwards.
    pub fn with_ssh_identity(mut self, private_key: impl Into<PathBuf>) -> Self {
        self.ssh_identity = Some(private_key.into());
        self
    }

    fn ssh_command(&self) -> Option<String> {
        self.ssh_identity.as_ref().map(|key| {
            let quoted = key.to_string_lossy().replace('\'', r"'\''");
            format!("ssh -i '{quoted}'")
        })
    }

    fn command(&self, dir: Option<&Path>) -> Command {
        let mut command = Command::new(&self.program);
        command
            .envs(self.envs.iter().map(|(k, v)| (k, v)))
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .kill_on_drop(true);
        if let Some(ssh) = self.ssh_command() {
            command.env("GIT_SSH_COMMAND", ssh);
        }
        if let Some(dir) = dir {
            command.arg("-C").arg(dir);
        }
        // Reading status must not take locks or rewrite the index.
        command.arg("--no-optional-locks");
        command
    }

    /// Runs git and returns its trimmed standard output.
    pub async fn run<I, S>(&self, dir: Option<&Path>, args: I) -> Result<String>
    where
        I: IntoIterator<Item = S>,
        S: AsRef<OsStr>,
    {
        let args: Vec<OsString> = args
            .into_iter()
            .map(|a| a.as_ref().to_os_string())
            .collect();
        let output = self
            .command(dir)
            .args(&args)
            .output()
            .await
            .map_err(|e| match e.kind() {
                std::io::ErrorKind::NotFound => Error::MissingProgram("git".into()),
                _ => Error::io("Couldn’t run git", e),
            })?;
        if output.status.success() {
            return Ok(String::from_utf8_lossy(&output.stdout).trim().to_string());
        }
        Err(failure(&args, &output))
    }

    async fn try_run(&self, dir: &Path, args: &[&str]) -> Result<Option<String>> {
        match self.run(Some(dir), args).await {
            Ok(out) => Ok(Some(out)),
            Err(Error::Git { .. }) => Ok(None),
            Err(e) => Err(e),
        }
    }

    /// The top of the working copy `path` is inside, if it is in one.
    pub async fn toplevel(&self, path: &Path) -> Result<Option<PathBuf>> {
        if !path.is_dir() {
            return Ok(None);
        }
        Ok(self
            .try_run(path, &["rev-parse", "--show-toplevel"])
            .await?
            .filter(|s| !s.is_empty())
            .map(PathBuf::from))
    }

    /// The URL of `remote`, if it exists.
    pub async fn remote_url(&self, repo: &Path, remote: &str) -> Result<Option<String>> {
        Ok(self
            .try_run(repo, &["remote", "get-url", remote])
            .await?
            .filter(|s| !s.is_empty()))
    }

    /// Branch, upstream, ahead/behind, changed files and remote of a working copy.
    pub async fn inspect(&self, repo: &Path) -> Result<LocalRepo> {
        let branch = self
            .try_run(repo, &["symbolic-ref", "--short", "-q", "HEAD"])
            .await?
            .filter(|b| !b.is_empty());
        let upstream = self
            .try_run(
                repo,
                &[
                    "rev-parse",
                    "--abbrev-ref",
                    "--symbolic-full-name",
                    "@{upstream}",
                ],
            )
            .await?
            .filter(|u| !u.is_empty());
        let (mut ahead, mut behind) = (None, None);
        if upstream.is_some() {
            let counts = self
                .try_run(
                    repo,
                    &["rev-list", "--left-right", "--count", "@{upstream}...HEAD"],
                )
                .await?
                .unwrap_or_default();
            let mut parts = counts.split_whitespace().map(|n| n.parse::<u64>().ok());
            behind = parts.next().flatten();
            ahead = parts.next().flatten();
        }
        let status = self
            .run(Some(repo), ["status", "--porcelain"])
            .await
            .map_err(|e| match e {
                Error::Git { .. } => Error::NotARepo(repo.to_path_buf()),
                other => other,
            })?;
        let changed_files = status.lines().filter(|l| !l.trim().is_empty()).count() as u64;

        let mut remote_name = None;
        let mut remote_url = self.remote_url(repo, "origin").await?;
        if remote_url.is_some() {
            remote_name = Some("origin".to_string());
        } else if let Some(first) = self
            .try_run(repo, &["remote"])
            .await?
            .and_then(|r| r.lines().next().map(str::to_string))
        {
            remote_url = self.remote_url(repo, &first).await?;
            remote_name = Some(first);
        }
        let remote = remote_url.as_deref().and_then(RemoteRef::parse);

        // Every remote, so a working copy is recognised whatever its
        // studio remote is called.
        let mut remotes = Vec::new();
        let names = self.try_run(repo, &["remote"]).await?.unwrap_or_default();
        for name in names.lines().map(str::trim).filter(|n| !n.is_empty()) {
            if let Some(url) = self.remote_url(repo, name).await? {
                remotes.push(RemoteInfo {
                    name: name.to_string(),
                    remote: RemoteRef::parse(&url),
                    url,
                });
            }
        }
        // `atelier/feature/x` follows the remote `atelier`; remote names
        // may contain slashes themselves, so the longest match wins.
        let upstream_remote = upstream.as_deref().and_then(|upstream| {
            remotes
                .iter()
                .filter(|r| {
                    upstream
                        .strip_prefix(r.name.as_str())
                        .is_some_and(|rest| rest.starts_with('/'))
                })
                .max_by_key(|r| r.name.len())
                .map(|r| r.name.clone())
        });
        Ok(LocalRepo {
            path: repo.to_path_buf(),
            branch,
            upstream,
            ahead,
            behind,
            changed_files,
            remote_name,
            remote_url,
            remote,
            remotes,
            upstream_remote,
        })
    }

    /// Finds and inspects every working copy under `roots`, down to
    /// `max_depth` folders deep. Copies that can't be read are skipped.
    pub async fn scan(&self, roots: &[PathBuf], max_depth: usize) -> Vec<LocalRepo> {
        let paths = discover_repos(roots, max_depth);
        let mut found = Vec::with_capacity(paths.len());
        for batch in paths.chunks(8) {
            let mut set = tokio::task::JoinSet::new();
            for (index, path) in batch.iter().cloned().enumerate() {
                let git = self.clone();
                set.spawn(async move { (index, git.inspect(&path).await) });
            }
            let mut results: Vec<(usize, LocalRepo)> = Vec::new();
            while let Some(joined) = set.join_next().await {
                if let Ok((index, Ok(repo))) = joined {
                    results.push((index, repo));
                }
            }
            results.sort_by_key(|(index, _)| *index);
            found.extend(results.into_iter().map(|(_, repo)| repo));
        }
        found
    }

    /// `git clone url dest`. Parent folders are created; `dest` must not
    /// exist yet (or be empty).
    pub async fn clone_repo(&self, url: &str, dest: &Path) -> Result<()> {
        if let Some(parent) = dest.parent() {
            std::fs::create_dir_all(parent)
                .map_err(|e| Error::io(format!("Couldn’t create {}", parent.display()), e))?;
        }
        let mut args: Vec<OsString> = vec!["clone".into(), "--quiet".into()];
        if let (Some(ssh), true) = (self.ssh_command(), is_ssh_url(url)) {
            args.push("--config".into());
            args.push(format!("core.sshCommand={ssh}").into());
        }
        args.push("--".into());
        args.push(url.into());
        args.push(dest.into());
        self.run(None, args).await.map(|_| ())
    }

    /// `git fetch --prune [remote]`.
    pub async fn fetch(&self, repo: &Path, remote: Option<&str>) -> Result<()> {
        let mut args = vec!["fetch", "--quiet", "--prune"];
        args.extend(remote);
        self.run(Some(repo), args).await.map(|_| ())
    }

    /// `git push`: the current branch to its upstream, or with `set_upstream`
    /// `git push -u <remote> <current branch>`.
    pub async fn push(&self, repo: &Path, remote: &str, set_upstream: bool) -> Result<()> {
        if !set_upstream {
            return self.run(Some(repo), ["push", "--quiet"]).await.map(|_| ());
        }
        let branch = self
            .try_run(repo, &["symbolic-ref", "--short", "-q", "HEAD"])
            .await?
            .filter(|b| !b.is_empty())
            .ok_or_else(|| Error::Invalid("You’re not on a branch.".into()))?;
        self.run(Some(repo), ["push", "--quiet", "-u", remote, &branch])
            .await
            .map(|_| ())
    }

    /// Points `remote` at `url`, adding it if needed. Returns the URL it had.
    pub async fn set_remote(&self, repo: &Path, remote: &str, url: &str) -> Result<Option<String>> {
        let previous = self.remote_url(repo, remote).await?;
        let verb = if previous.is_some() { "set-url" } else { "add" };
        self.run(Some(repo), ["remote", verb, remote, url]).await?;
        Ok(previous)
    }

    /// After a studio repository was renamed: points every remote of the
    /// working copy that still names `old` at `new` instead, keeping the
    /// rest of each address (scheme, user, port) as it was. Returns the
    /// names of the remotes that changed.
    pub async fn retarget_remotes(
        &self,
        repo: &Path,
        old: &Repo,
        new: &Repo,
    ) -> Result<Vec<String>> {
        let local = self.inspect(repo).await?;
        let old_refs: Vec<RemoteRef> = [&old.clone_https, &old.clone_ssh]
            .into_iter()
            .filter_map(|url| RemoteRef::parse(url))
            .collect();
        let mut changed = Vec::new();
        for info in &local.remotes {
            let points_at_old = info
                .remote
                .as_ref()
                .is_some_and(|remote| old_refs.contains(remote));
            if !points_at_old {
                continue;
            }
            let Some(url) = renamed_url(&info.url, &old.name, &new.name) else {
                continue;
            };
            self.run(Some(repo), ["remote", "set-url", &info.name, &url])
                .await?;
            changed.push(info.name.clone());
        }
        Ok(changed)
    }

    /// What publishing `path` would involve, without changing anything.
    pub async fn publish_plan(&self, path: &Path) -> Result<PublishPlan> {
        let toplevel = self
            .toplevel(path)
            .await?
            .ok_or_else(|| Error::NotARepo(path.to_path_buf()))?;
        let commits = self
            .try_run(&toplevel, &["rev-list", "--count", "HEAD"])
            .await?
            .and_then(|n| n.parse().ok())
            .unwrap_or(0);
        let branch = self
            .try_run(&toplevel, &["symbolic-ref", "--short", "-q", "HEAD"])
            .await?
            .filter(|b| !b.is_empty());
        let origin = self.remote_url(&toplevel, "origin").await?;
        let folder_name = toplevel
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_default();
        Ok(PublishPlan {
            toplevel,
            folder_name,
            branch,
            commits,
            origin,
        })
    }

    /// Turns a folder into a studio repository: creates the repository
    /// through the API, points `origin` at it and pushes every branch and
    /// tag. The folder's files are never touched.
    ///
    /// An existing `origin` is only replaced when `options.replace_origin`
    /// is true (the person's say-so); otherwise this fails with
    /// [`Error::OriginExists`] before anything is created.
    pub async fn publish_folder(
        &self,
        client: &Client,
        options: &PublishOptions,
        progress: &mut (dyn FnMut(PublishStep) + Send),
    ) -> Result<Published> {
        let plan = self.publish_plan(&options.path).await?;
        if plan.commits == 0 {
            return Err(Error::Invalid(
                "There are no commits here yet. Commit something first.".into(),
            ));
        }
        if let (Some(url), false) = (&plan.origin, options.replace_origin) {
            return Err(Error::OriginExists { url: url.clone() });
        }
        let repo_dir = plan.toplevel.as_path();

        let full_name = format!("{}/{}", options.owner, options.name);
        progress(PublishStep::Creating {
            full_name: full_name.clone(),
        });
        let repo = client
            .create_repo(&CreateRepo {
                owner: options.owner.clone(),
                name: options.name.clone(),
                private: options.private,
                description: options.description.clone(),
            })
            .await?;
        progress(PublishStep::Created);

        let url = match options.transport {
            Transport::Ssh if !repo.clone_ssh.is_empty() => repo.clone_ssh.clone(),
            _ if !repo.clone_https.is_empty() => repo.clone_https.clone(),
            _ => repo.clone_ssh.clone(),
        };
        if url.is_empty() {
            return Err(Error::Decode(
                "the new repository has no clone address".into(),
            ));
        }
        let previous_origin = self.set_remote(repo_dir, "origin", &url).await?;
        if let (Some(ssh), true) = (self.ssh_command(), is_ssh_url(&url)) {
            self.run(Some(repo_dir), ["config", "core.sshCommand", &ssh])
                .await?;
        }

        progress(PublishStep::Pushing {
            branch: plan.branch.clone(),
            commits: plan.commits,
        });
        self.run(Some(repo_dir), ["push", "--quiet", "origin", "--all"])
            .await?;
        self.run(Some(repo_dir), ["push", "--quiet", "origin", "--tags"])
            .await?;
        // Track the new remote from the current branch, unless it already
        // tracks something the person chose.
        if let Some(branch) = &plan.branch {
            let has_upstream = self
                .try_run(repo_dir, &["rev-parse", "--abbrev-ref", "@{upstream}"])
                .await?
                .is_some();
            if !has_upstream || previous_origin.is_some() {
                let upstream = format!("--set-upstream-to=origin/{branch}");
                self.run(Some(repo_dir), ["branch", "--quiet", &upstream])
                    .await?;
            }
        }
        progress(PublishStep::Pushed);

        Ok(Published {
            web_url: client.web_url(&repo.full_name),
            repo,
            path: plan.toplevel,
            branch: plan.branch,
            commits: plan.commits,
            origin: url,
            previous_origin,
        })
    }
}

/// Turns a failed git run into [`Error::Git`]: the first word of the command
/// and what git said, on one line.
fn failure(args: &[OsString], output: &std::process::Output) -> Error {
    let stderr = String::from_utf8_lossy(&output.stderr);
    let message = stderr
        .lines()
        .map(str::trim)
        .filter(|l| !l.is_empty())
        .map(|l| {
            l.trim_start_matches("fatal: ")
                .trim_start_matches("error: ")
        })
        .collect::<Vec<_>>()
        .join(" · ");
    Error::Git {
        command: command_word(args),
        message: if message.is_empty() {
            format!("exit {}", output.status.code().unwrap_or(-1))
        } else {
            message
        },
    }
}

/// The git subcommand in `args`, skipping options and `-c key=value` pairs.
fn command_word(args: &[OsString]) -> String {
    let mut skip = false;
    for arg in args {
        let arg = arg.to_string_lossy();
        if skip {
            skip = false;
        } else if arg == "-c" {
            skip = true;
        } else if !arg.starts_with('-') {
            return arg.into_owned();
        }
    }
    String::new()
}

/// `url` with its last path segment (`Old` or `Old.git`, compared without
/// regard to case) replaced by `new`. `None` when it doesn't end that way.
pub fn renamed_url(url: &str, old: &str, new: &str) -> Option<String> {
    let trimmed = url.trim_end_matches('/');
    let (stem, suffix) = match trimmed.len().checked_sub(4) {
        Some(at) if trimmed.is_char_boundary(at) && trimmed[at..].eq_ignore_ascii_case(".git") => {
            (&trimmed[..at], &trimmed[at..])
        }
        _ => (trimmed, ""),
    };
    let at = stem.len().checked_sub(old.len())?;
    if old.is_empty() || !stem.is_char_boundary(at) || !stem[at..].eq_ignore_ascii_case(old) {
        return None;
    }
    if !stem[..at].ends_with(['/', ':']) {
        return None;
    }
    Some(format!("{}{new}{suffix}", &stem[..at]))
}

fn is_ssh_url(url: &str) -> bool {
    url.starts_with("ssh://") || (!url.contains("://") && RemoteRef::parse(url).is_some())
}

/// A working copy on this machine.
#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct LocalRepo {
    pub path: PathBuf,
    /// `None` when HEAD is detached.
    pub branch: Option<String>,
    /// The branch's upstream (`origin/main`), if it has one.
    pub upstream: Option<String>,
    /// Commits here that the upstream doesn't have. `None` without an upstream.
    pub ahead: Option<u64>,
    /// Commits on the upstream that aren't here (as of the last fetch).
    pub behind: Option<u64>,
    /// Files changed, staged or untracked.
    pub changed_files: u64,
    pub remote_name: Option<String>,
    pub remote_url: Option<String>,
    pub remote: Option<RemoteRef>,
    /// Every remote of the working copy, in git's order.
    pub remotes: Vec<RemoteInfo>,
    /// The remote the current branch follows, when it follows one.
    pub upstream_remote: Option<String>,
}

/// One remote of a working copy.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct RemoteInfo {
    pub name: String,
    pub url: String,
    /// `None` for a local path or an address this crate doesn't understand.
    pub remote: Option<RemoteRef>,
}

impl LocalRepo {
    /// The studio repository this working copy belongs to, if any: any of
    /// its remotes may point at it, whatever the remote is called. Owner
    /// and name are compared without regard to case.
    pub fn studio_repo<'a>(&self, repos: &'a [Repo]) -> Option<&'a Repo> {
        self.studio_remote(repos)
            .map(|(_, repo)| repo)
            .or_else(|| self.remote.as_ref().and_then(|r| r.match_repo(repos)))
    }

    /// The remote that points at a studio repository, and that repository.
    /// With several candidates the one the current branch follows wins,
    /// then `origin`, then the first in git's order.
    pub fn studio_remote<'a, 'b>(
        &'b self,
        repos: &'a [Repo],
    ) -> Option<(&'b RemoteInfo, &'a Repo)> {
        let rank = |info: &RemoteInfo| {
            if self.upstream_remote.as_deref() == Some(info.name.as_str()) {
                0
            } else if info.name == "origin" {
                1
            } else {
                2
            }
        };
        let mut candidates: Vec<&RemoteInfo> = self.remotes.iter().collect();
        candidates.sort_by_key(|info| rank(info));
        candidates.into_iter().find_map(|info| {
            let repo = info.remote.as_ref()?.match_repo(repos)?;
            Some((info, repo))
        })
    }

    /// The folder's own name.
    pub fn folder_name(&self) -> String {
        self.path
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_default()
    }
}

/// A git remote reduced to what identifies it: host and path, lower-cased,
/// without user, port, `.git` or slashes around it.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct RemoteRef {
    pub host: String,
    pub path: String,
}

impl RemoteRef {
    /// Understands `https://host[:port]/Owner/Name[.git]`,
    /// `ssh://[user@]host[:port]/Owner/Name[.git]`, `git://…` and the scp
    /// form `[user@]host:Owner/Name[.git]`. Local paths give `None`.
    pub fn parse(url: &str) -> Option<RemoteRef> {
        let url = url.trim();
        let (authority, path) = if let Some((scheme, rest)) = url.split_once("://") {
            if !matches!(
                scheme.to_ascii_lowercase().as_str(),
                "http" | "https" | "ssh" | "git" | "git+ssh" | "ssh+git"
            ) {
                return None;
            }
            let (authority, path) = rest.split_once('/')?;
            let host = authority.rsplit_once('@').map_or(authority, |(_, h)| h);
            // Strip a port, minding IPv6 literals.
            let host = match host.rsplit_once(':') {
                Some((h, port)) if port.chars().all(|c| c.is_ascii_digit()) => h,
                _ => host,
            };
            (host, path)
        } else {
            // scp form; a slash before the colon, a drive letter or no colon
            // at all means a local path.
            let (authority, path) = url.split_once(':')?;
            if authority.contains('/') || authority.contains('\\') || authority.len() < 2 {
                return None;
            }
            (
                authority.rsplit_once('@').map_or(authority, |(_, h)| h),
                path,
            )
        };
        let host = authority
            .trim_matches(|c| c == '[' || c == ']')
            .to_lowercase();
        let path = path.split(['?', '#']).next().unwrap_or(path);
        let path = path.trim_matches('/');
        let path = path
            .strip_suffix(".git")
            .or_else(|| path.strip_suffix(".GIT"))
            .unwrap_or(path)
            .trim_matches('/')
            .to_lowercase();
        if host.is_empty() || path.is_empty() {
            return None;
        }
        Some(RemoteRef { host, path })
    }

    /// The studio repository with this remote: one whose HTTPS or SSH clone
    /// address has the same host and path, else one tethered to this GitHub
    /// repository.
    pub fn match_repo<'a>(&self, repos: &'a [Repo]) -> Option<&'a Repo> {
        let same = |url: &str| RemoteRef::parse(url).as_ref() == Some(self);
        repos
            .iter()
            .find(|r| same(&r.clone_https) || same(&r.clone_ssh))
            .or_else(|| {
                if self.host != "github.com" {
                    return None;
                }
                repos.iter().find(|r| {
                    r.github
                        .as_ref()
                        .is_some_and(|g| g.repo.to_lowercase() == self.path)
                })
            })
    }
}

/// Finds working copies under `roots`, at most `max_depth` folders below
/// each root (the root itself is depth 0). Hidden folders, dependency and
/// build folders and symlinks are not followed; the search doesn't look
/// inside a working copy it found. Sorted, without duplicates.
pub fn discover_repos(roots: &[PathBuf], max_depth: usize) -> Vec<PathBuf> {
    fn walk(dir: &Path, depth: usize, max_depth: usize, found: &mut Vec<PathBuf>) {
        if dir.join(".git").exists() {
            found.push(dir.to_path_buf());
            return;
        }
        if depth >= max_depth {
            return;
        }
        let Ok(entries) = std::fs::read_dir(dir) else {
            return;
        };
        for entry in entries.flatten() {
            let name = entry.file_name();
            let name = name.to_string_lossy();
            if name.starts_with('.') || SKIP_DIRS.contains(&name.as_ref()) {
                continue;
            }
            // file_type() doesn't follow symlinks.
            if entry.file_type().is_ok_and(|t| t.is_dir()) {
                walk(&entry.path(), depth + 1, max_depth, found);
            }
        }
    }
    let mut found = Vec::new();
    for root in roots {
        walk(root, 0, max_depth, &mut found);
    }
    found.sort();
    found.dedup();
    found
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Transport {
    /// `clone_ssh` (the device's SSH key is registered at sign-in).
    #[default]
    Ssh,
    Https,
}

#[derive(Clone, Debug)]
pub struct PublishOptions {
    /// The folder (anywhere inside the working copy).
    pub path: PathBuf,
    /// The account to publish under (`accounts[].name` from `GET /me`).
    pub owner: String,
    pub name: String,
    pub private: bool,
    pub description: String,
    pub transport: Transport,
    /// The person agreed to point an existing `origin` at the studio.
    pub replace_origin: bool,
}

/// What [`Git::publish_plan`] found.
#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct PublishPlan {
    pub toplevel: PathBuf,
    pub folder_name: String,
    pub branch: Option<String>,
    /// Commits on the current branch.
    pub commits: u64,
    /// Where `origin` points now, if anywhere.
    pub origin: Option<String>,
}

/// Progress of [`Git::publish_folder`].
#[derive(Clone, Debug, PartialEq)]
pub enum PublishStep {
    Creating {
        full_name: String,
    },
    Created,
    Pushing {
        branch: Option<String>,
        commits: u64,
    },
    Pushed,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct Published {
    pub repo: Repo,
    /// The repository's page in the studio.
    pub web_url: String,
    pub path: PathBuf,
    pub branch: Option<String>,
    pub commits: u64,
    /// Where `origin` points now.
    pub origin: String,
    /// Where `origin` pointed before, when it was replaced.
    pub previous_origin: Option<String>,
}
