//! "Add a repository": publish a folder, clone from the studio (mirroring
//! from GitHub first when asked), or start a new one.

use std::path::{Path, PathBuf};
use std::time::Duration;

use atelier_core::api::{Account, CreateRepo, Repo};
use atelier_core::git::{PublishOptions, Transport};
use serde::Serialize;

use crate::desk::Desk;
use crate::error::{CmdError, CmdResult};
use crate::paths;

/// What the app found in a folder the person picked.
#[derive(Clone, Debug, Serialize)]
pub struct FolderPlan {
    pub path: String,
    pub display: String,
    pub name: String,
    pub is_repo: bool,
    pub branch: Option<String>,
    pub commits: u64,
    /// `origin`, when the folder already has one.
    pub origin: Option<String>,
    /// The studio repository it already belongs to.
    pub in_studio: Option<String>,
    /// `A git folder · 42 commits on main · not in your studio yet`.
    pub summary: String,
}

#[derive(Clone, Debug, Serialize)]
pub struct Added {
    pub path: String,
    pub full_name: String,
    pub web_path: String,
    /// The folder was already there; nothing was cloned.
    pub already: bool,
    /// Something worth saying that didn't stop it (`Opened in VS Code`).
    pub note: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct Hit {
    /// `deminimmo / loop-notes`.
    pub title: String,
    pub full_name: String,
    /// `In your studio · private`.
    pub line: String,
    /// `studio` or `github`.
    pub source: &'static str,
    /// `clone`, `mirror` or `open` (already on this machine).
    pub action: &'static str,
    pub label: &'static str,
    pub local_path: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct SearchView {
    pub hits: Vec<Hit>,
    /// Why GitHub wasn't searched, when it wasn't.
    pub github_note: Option<String>,
}

/// A found folder in first run's "Find your folders".
#[derive(Clone, Debug, Serialize)]
pub struct FoundFolder {
    pub path: String,
    pub display: String,
    pub name: String,
    pub branch: Option<String>,
    /// `owner/name` in the studio when its remote points there.
    pub repo: Option<String>,
    /// Where its remote points otherwise (`github.com`), or nothing.
    pub remote_host: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct Folders {
    /// The folders looked through, for showing.
    pub roots: Vec<String>,
    pub folders: Vec<FoundFolder>,
    pub matched: usize,
    pub studio_repos: usize,
}

fn spaced(full_name: &str) -> String {
    full_name.replacen('/', " / ", 1)
}

impl Desk {
    /// Remembers a folder the person picked so its repository may be used.
    pub fn register_picked(&self, folder: &Path) -> CmdResult<PathBuf> {
        let real = folder
            .canonicalize()
            .map_err(|_| CmdError::new("missing", "That folder isn’t there."))?;
        if !real.is_dir() {
            return Err(CmdError::invalid("Pick a folder, not a file."));
        }
        let text = real.to_string_lossy().into_owned();
        let inside = self.roots().iter().any(|root| {
            root.canonicalize()
                .is_ok_and(|root| real.starts_with(&root))
        });
        if !inside {
            self.update_prefs(|prefs| {
                if !prefs.picked.contains(&text) {
                    prefs.picked.push(text.clone());
                }
            })?;
        }
        Ok(real)
    }

    /// Adds a folder to the ones looked through for working copies.
    pub fn add_watch_folder(&self, folder: &Path) -> CmdResult<String> {
        let real = folder
            .canonicalize()
            .map_err(|_| CmdError::new("missing", "That folder isn’t there."))?;
        if !real.is_dir() {
            return Err(CmdError::invalid("Pick a folder, not a file."));
        }
        let settings = self.settings();
        let shown = paths::display(&real, &settings.home);
        if !settings.repo_roots().iter().any(|r| r == &real) {
            self.update_config(|config| config.watch.push(shown.clone()))?;
        }
        Ok(shown)
    }

    /// First run, step three: the git folders already on this machine and
    /// the studio repositories they belong to.
    pub async fn find_folders(&self) -> CmdResult<Folders> {
        let repos = self.repos(Duration::from_secs(30)).await?;
        let settings = self.settings();
        let found = self
            .git()
            .scan(&self.roots(), crate::desk::SCAN_DEPTH)
            .await;
        let folders: Vec<FoundFolder> = found
            .iter()
            .map(|local| FoundFolder {
                path: local.path.to_string_lossy().into_owned(),
                display: paths::display(&local.path, &settings.home),
                name: local.folder_name(),
                branch: local.branch.clone(),
                repo: local.studio_repo(&repos).map(|r| r.full_name.clone()),
                remote_host: local.remote.as_ref().map(|r| r.host.clone()),
            })
            .collect();
        Ok(Folders {
            roots: self
                .roots()
                .iter()
                .map(|r| paths::display(r, &settings.home))
                .collect(),
            matched: folders.iter().filter(|f| f.repo.is_some()).count(),
            studio_repos: repos.len(),
            folders,
        })
    }

    /// What publishing a picked folder would do.
    pub async fn folder_plan(&self, folder: &Path) -> CmdResult<FolderPlan> {
        let real = self.register_picked(folder)?;
        let git = self.git();
        let display = self.display_path(&real);
        let name = real
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_default();
        let is_top = git.toplevel(&real).await?.is_some_and(|top| top == real);
        if !is_top {
            return Ok(FolderPlan {
                path: real.to_string_lossy().into_owned(),
                display,
                name,
                is_repo: false,
                branch: None,
                commits: 0,
                origin: None,
                in_studio: None,
                summary: "Not a git folder yet · make it one, then commit something to publish"
                    .to_string(),
            });
        }
        let plan = git.publish_plan(&real).await?;
        let local = git.inspect(&real).await?;
        let in_studio = match self.repos(Duration::from_secs(60)).await {
            Ok(repos) => local.studio_repo(&repos).map(|r| r.full_name.clone()),
            Err(_) => None,
        };
        let commits = match plan.commits {
            0 => "no commits yet".to_string(),
            1 => "1 commit".to_string(),
            n => format!("{n} commits"),
        };
        let on = plan
            .branch
            .as_ref()
            .filter(|_| plan.commits > 0)
            .map(|b| format!(" on {b}"))
            .unwrap_or_default();
        let place = match (&in_studio, &plan.origin) {
            (Some(full), _) => format!("already in your studio as {full}"),
            (None, Some(_)) => "its origin points somewhere else".to_string(),
            (None, None) => "not in your studio yet".to_string(),
        };
        Ok(FolderPlan {
            path: real.to_string_lossy().into_owned(),
            display,
            name: plan.folder_name,
            is_repo: true,
            branch: plan.branch,
            commits: plan.commits,
            origin: plan.origin,
            in_studio,
            summary: format!("A git folder · {commits}{on} · {place}"),
        })
    }

    /// Makes a picked folder a git repository (`git init`), nothing more.
    pub async fn folder_init(&self, path: &str) -> CmdResult<()> {
        let real = paths::validate(&self.roots(), path)?;
        if self
            .git()
            .toplevel(&real)
            .await?
            .is_some_and(|top| top == real)
        {
            return Ok(());
        }
        Ok(self.git().init(&real, "main").await?)
    }

    async fn account(&self, owner: &str) -> Option<Account> {
        let me = self.me(Duration::from_secs(300)).await.ok()?;
        me.accounts
            .into_iter()
            .find(|a| a.name.eq_ignore_ascii_case(owner))
    }

    /// Sets the account's commit identity in the repository's own config.
    async fn apply_identity(&self, repo: &Path, owner: &str) {
        if let Some(account) = self.account(owner).await {
            let _ = self
                .git()
                .set_identity(repo, &account.commit_name, &account.commit_email)
                .await;
        }
    }

    fn transport(&self, repo: &Repo) -> Transport {
        let has_key = atelier_core::ssh::find_public_key(&self.settings().ssh_dir()).is_some();
        if has_key && !repo.clone_ssh.is_empty() {
            Transport::Ssh
        } else {
            Transport::Https
        }
    }

    fn clone_url(&self, repo: &Repo) -> CmdResult<String> {
        let url = match self.transport(repo) {
            Transport::Ssh => &repo.clone_ssh,
            Transport::Https if !repo.clone_https.is_empty() => &repo.clone_https,
            Transport::Https => &repo.clone_ssh,
        };
        if url.is_empty() {
            Err(CmdError::new(
                "decode",
                "Your studio didn’t say where to clone that from.",
            ))
        } else {
            Ok(url.clone())
        }
    }

    /// Creates the repository in the studio and pushes the folder to it.
    pub async fn publish(
        &self,
        path: &str,
        owner: &str,
        name: &str,
        private: bool,
        replace_origin: bool,
    ) -> CmdResult<Added> {
        let repo = self.repo_path(path).await?;
        let owner = paths::safe_segment(owner.trim())?;
        let name = paths::safe_segment(name.trim())?;
        let client = self.client()?;
        let has_key = atelier_core::ssh::find_public_key(&self.settings().ssh_dir()).is_some();
        let options = PublishOptions {
            path: repo.clone(),
            owner: owner.to_string(),
            name: name.to_string(),
            private,
            description: String::new(),
            transport: if has_key {
                Transport::Ssh
            } else {
                Transport::Https
            },
            replace_origin,
        };
        let published = self
            .git()
            .publish_folder(&client, &options, &mut |_| {})
            .await
            .map_err(|e| match e {
                atelier_core::Error::OriginExists { url } => CmdError::new(
                    "origin_exists",
                    format!("This folder’s origin already points at {url}."),
                )
                .hint("Publish anyway to point origin at your studio instead."),
                other => CmdError::from(other),
            })?;
        self.forget_repos();
        self.apply_identity(&repo, owner).await;
        Ok(Added {
            path: repo.to_string_lossy().into_owned(),
            web_path: format!("/{}", published.repo.full_name),
            full_name: published.repo.full_name,
            already: false,
            note: None,
        })
    }

    /// Repositories in the studio matching `q`, and on GitHub through
    /// Tether.
    pub async fn search(&self, q: &str) -> CmdResult<SearchView> {
        let client = self.client()?;
        let q = q.trim();
        let list = if q.is_empty() {
            atelier_core::api::RepoList {
                repos: self.repos(Duration::from_secs(30)).await?,
                ..Default::default()
            }
        } else {
            self.watch(client.search_repos(q, true).await)?
        };
        let all = self
            .repos(Duration::from_secs(60))
            .await
            .unwrap_or_default();
        let copies = self.working_copies(Some(&all)).await;
        let local = |full: &str| {
            copies
                .iter()
                .find(|c| c.full_name.as_deref() == Some(full))
                .map(|c| c.path.clone())
        };
        let machine = self.machine();
        let mut hits: Vec<Hit> = list
            .repos
            .iter()
            .map(|r| {
                let local_path = local(&r.full_name);
                let mut line = vec!["In your studio".to_string()];
                line.push(if r.private { "private" } else { "public" }.to_string());
                if local_path.is_some() {
                    line.push(format!("already on this {machine}"));
                }
                Hit {
                    title: spaced(&r.full_name),
                    full_name: r.full_name.clone(),
                    line: line.join(" · "),
                    source: "studio",
                    action: if local_path.is_some() {
                        "open"
                    } else {
                        "clone"
                    },
                    label: if local_path.is_some() {
                        "Open"
                    } else {
                        "Clone"
                    },
                    local_path,
                }
            })
            .collect();
        hits.extend(list.github_hits.iter().map(|h| Hit {
            title: spaced(&h.full_name),
            full_name: h.full_name.clone(),
            line: if h.atelier_badge {
                "On GitHub · via Tether · uses Atelier".to_string()
            } else {
                "On GitHub · via Tether".to_string()
            },
            source: "github",
            action: "mirror",
            label: "Mirror and clone",
            local_path: None,
        }));
        Ok(SearchView {
            hits,
            github_note: list.github_error.filter(|e| !e.is_empty()),
        })
    }

    fn clone_dest(&self, owner: &str, name: &str) -> CmdResult<PathBuf> {
        let owner = paths::safe_segment(owner)?;
        let name = paths::safe_segment(name)?;
        Ok(self.settings().clone_root().join(owner).join(name))
    }

    /// Clones a studio repository to `<clone root>/<owner>/<name>` and sets
    /// the account's commit identity in it.
    pub async fn clone_studio(&self, full_name: &str) -> CmdResult<Added> {
        paths::split_full_name(full_name)?;
        self.forget_repos();
        let repos = self.repos(Duration::ZERO).await?;
        let repo = repos
            .iter()
            .find(|r| r.full_name.eq_ignore_ascii_case(full_name))
            .ok_or_else(|| {
                CmdError::new(
                    "not_found",
                    format!("There is no repository called {full_name} in your studio."),
                )
            })?;
        let dest = self.clone_dest(&repo.owner, &repo.name)?;
        let added = |already| Added {
            path: dest.to_string_lossy().into_owned(),
            full_name: repo.full_name.clone(),
            web_path: format!("/{}", repo.full_name),
            already,
            note: None,
        };
        let git = self.git();
        if dest.exists() {
            let same = match git.inspect(&dest).await {
                Ok(local) => {
                    local
                        .studio_repo(&repos)
                        .is_some_and(|r| r.full_name == repo.full_name)
                        || local
                            .remote_url
                            .as_deref()
                            .is_some_and(|url| url == repo.clone_https || url == repo.clone_ssh)
                }
                Err(_) => false,
            };
            return if same {
                Ok(added(true))
            } else {
                Err(CmdError::new(
                    "exists",
                    format!(
                        "{} is already there and is something else.",
                        self.display_path(&dest)
                    ),
                )
                .hint("Move it away, or publish that folder instead."))
            };
        }
        git.clone_repo(&self.clone_url(repo)?, &dest).await?;
        self.apply_identity(&dest, &repo.owner).await;
        Ok(added(false))
    }

    /// Mirrors a GitHub repository into the studio, waits for it to appear,
    /// then clones it.
    pub async fn mirror_and_clone(&self, github: &str) -> CmdResult<Added> {
        paths::split_full_name(github)?;
        let client = self.client()?;
        let mirrored = self.watch(client.mirror_repo(github).await)?;
        let full_name = mirrored.full_name;
        // The mirror is made in the background; give it a little while.
        for attempt in 0..30u32 {
            if attempt > 0 {
                tokio::time::sleep(Duration::from_millis(if attempt < 5 { 400 } else { 2000 }))
                    .await;
            }
            self.forget_repos();
            let repos = self.repos(Duration::ZERO).await?;
            if repos
                .iter()
                .any(|r| r.full_name.eq_ignore_ascii_case(&full_name))
            {
                return self.clone_studio(&full_name).await;
            }
        }
        Err(CmdError::new(
            "mirror_slow",
            format!("{full_name} is still being mirrored into your studio."),
        )
        .hint("Give it a minute, then clone it from the list."))
    }

    /// Makes a new repository in the studio and an empty working copy here,
    /// optionally with a README and a .gitignore as its first commit.
    pub async fn create_new(
        &self,
        owner: &str,
        name: &str,
        private: bool,
        readme: bool,
        open_editor: bool,
    ) -> CmdResult<Added> {
        let owner = paths::safe_segment(owner.trim())?.to_string();
        let name = paths::safe_segment(name.trim())?.to_string();
        let dest = self.clone_dest(&owner, &name)?;
        if dest.exists() {
            return Err(CmdError::new(
                "exists",
                format!("{} is already there.", self.display_path(&dest)),
            ));
        }
        let client = self.client()?;
        let repo = self.watch(
            client
                .create_repo(&CreateRepo {
                    owner: owner.clone(),
                    name: name.clone(),
                    private,
                    description: String::new(),
                })
                .await,
        )?;
        self.forget_repos();
        let git = self.git();
        git.clone_repo(&self.clone_url(&repo)?, &dest).await?;
        let branch = if repo.default_branch.is_empty() {
            "main".to_string()
        } else {
            repo.default_branch.clone()
        };
        // An empty clone starts on whatever git's default is; use the studio's.
        git.run(
            Some(&dest),
            ["symbolic-ref", "HEAD", &format!("refs/heads/{branch}")],
        )
        .await?;
        self.apply_identity(&dest, &repo.owner).await;
        if readme {
            let io = |e: std::io::Error| {
                CmdError::new("io", format!("Couldn’t write the first files: {e}"))
            };
            std::fs::write(dest.join("README.md"), format!("# {name}\n")).map_err(io)?;
            std::fs::write(dest.join(".gitignore"), ".DS_Store\n").map_err(io)?;
            git.commit_paths(
                &dest,
                &["README.md".to_string(), ".gitignore".to_string()],
                &format!("Start {name}"),
                "",
            )
            .await?;
            git.push(&dest, "origin", true).await?;
        }
        let mut note = None;
        if open_editor {
            let path = dest.to_string_lossy().into_owned();
            note = match self.repo_open(&path, None).await {
                Ok(label) => Some(format!("Opened in {label}.")),
                Err(e) => Some(e.message),
            };
        }
        Ok(Added {
            path: dest.to_string_lossy().into_owned(),
            web_path: format!("/{}", repo.full_name),
            full_name: repo.full_name,
            already: false,
            note,
        })
    }
}
