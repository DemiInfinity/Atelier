//! What every command needs: settings, the token store, the terminal.

use std::io::IsTerminal;
use std::path::PathBuf;

use atelier_core::api::{Messages, Repo};
use atelier_core::config::Settings;
use atelier_core::git::LocalRepo;
use atelier_core::messages::Vars;
use atelier_core::{Client, Git, TokenStore};

use crate::error::{CliError, CliResult};
use crate::ui::Ui;

/// How deep under the clone root and the watch folders working copies are
/// looked for (`<root>/<owner>/<name>` is 2).
pub const SCAN_DEPTH: usize = 3;

pub struct Ctx {
    pub json: bool,
    pub ui: Ui,
    pub settings: Settings,
    pub store: Box<dyn TokenStore>,
    /// A person is at the keyboard: questions may be asked.
    pub interactive: bool,
    pub local_offset: Option<time::UtcOffset>,
}

impl Ctx {
    pub fn new(json: bool, settings: Settings, local_offset: Option<time::UtcOffset>) -> Ctx {
        let store = atelier_core::token::default_store(&settings.config_dir, settings.env());
        let interactive = std::io::stdin().is_terminal() && std::io::stdout().is_terminal();
        Ctx {
            json,
            ui: Ui::detect(),
            settings,
            store,
            interactive: interactive && !json,
            local_offset,
        }
    }

    /// The signed-in client, or the "not signed in" error.
    pub fn client(&self) -> CliResult<Client> {
        Client::from_settings(&self.settings, self.store.as_ref())
            .map_err(|e| CliError::from_core(e, self))
    }

    /// Git as `atl` runs it: with the dedicated device key when there is one.
    pub fn git(&self) -> Git {
        match atelier_core::ssh::dedicated_identity(&self.settings.ssh_dir()) {
            Some(identity) => Git::new().with_ssh_identity(identity),
            None => Git::new(),
        }
    }

    pub fn cwd(&self) -> CliResult<PathBuf> {
        std::env::current_dir()
            .map_err(|_| CliError::plain("Couldn’t tell which folder you’re in."))
    }

    /// The working copy the current folder is in, if it is in one.
    pub async fn here(&self) -> CliResult<Option<LocalRepo>> {
        let git = self.git();
        let cwd = self.cwd()?;
        let Some(top) = git.toplevel(&cwd).await.map_err(|e| self.err(e))? else {
            return Ok(None);
        };
        git.inspect(&top).await.map(Some).map_err(|e| self.err(e))
    }

    /// The studio repository to act on: `--repo owner/name`, else the one
    /// the current folder belongs to.
    pub async fn target_repo(
        &self,
        client: &Client,
        explicit: Option<&str>,
    ) -> CliResult<(String, String)> {
        if let Some(name) = explicit {
            return split_full_name(name).ok_or_else(|| {
                CliError::plain(format!("“{name}” isn’t a repository name."))
                    .hint("Write it as owner/name.")
            });
        }
        let repos = client.repos().await.map_err(|e| self.err(e))?;
        match self.here().await? {
            Some(here) => match here.studio_repo(&repos) {
                Some(repo) => Ok((repo.owner.clone(), repo.name.clone())),
                None => Err(CliError::plain("This folder isn’t in your studio yet.")
                    .hint("Run atl repo publish, or name one with --repo owner/name.")),
            },
            None => Err(CliError::plain("You’re not inside a repository.")
                .hint("Go to one, or name one with --repo owner/name.")),
        }
    }

    pub fn err(&self, e: atelier_core::Error) -> CliError {
        CliError::from_core(e, self)
    }

    /// The studio's host for sentences, or a placeholder.
    pub fn studio_for_hint(&self) -> String {
        self.settings
            .studio()
            .map(|s| {
                s.trim_start_matches("https://")
                    .trim_start_matches("http://")
                    .to_string()
            })
            .unwrap_or_else(|| "<studio>".to_string())
    }

    /// A friendly line for `moment`, when the person wants them in the
    /// terminal. Never fails: no message is fine.
    pub async fn friendly(&self, client: &Client, moment: &str, vars: Vars) -> Option<String> {
        let messages: Messages = client.messages().await.ok()?;
        if !messages.rules.terminal {
            return None;
        }
        let mut vars = vars.set_opt("device", self.settings.config.device_name.as_deref());
        if let Some(offset) = self.local_offset {
            let now = time::OffsetDateTime::now_utc().to_offset(offset);
            vars = vars.set("time", format!("{:02}:{:02}", now.hour(), now.minute()));
        }
        let seed = self
            .settings
            .env()
            .get("ATELIER_MESSAGE_SEED")
            .and_then(|s| s.parse().ok());
        atelier_core::messages::pick(&messages, moment, &vars, seed)
    }

    pub fn print_friendly(&self, message: &Option<String>) {
        if let Some(message) = message {
            crate::ui::outln!("{}{message}", self.ui.amber_bold("atelier: "));
        }
    }
}

pub fn split_full_name(name: &str) -> Option<(String, String)> {
    let (owner, repo) = name.trim().split_once('/')?;
    (!owner.is_empty() && !repo.is_empty() && !repo.contains('/'))
        .then(|| (owner.to_string(), repo.to_string()))
}

/// Finds a studio repository by `owner/name` or just `name` (when only one
/// has that name), ignoring case.
pub fn find_repo<'a>(repos: &'a [Repo], wanted: &str) -> CliResult<&'a Repo> {
    let wanted_lower = wanted.to_lowercase();
    let matches: Vec<&Repo> = repos
        .iter()
        .filter(|r| {
            r.full_name.to_lowercase() == wanted_lower || r.name.to_lowercase() == wanted_lower
        })
        .collect();
    match matches.as_slice() {
        [one] => Ok(one),
        [] => Err(CliError::plain(format!(
            "There is no repository called {wanted} in your studio."
        ))
        .hint("See them all with atl repo list.")),
        many => Err(CliError::plain(format!(
            "{wanted} could be {}.",
            many.iter()
                .map(|r| r.full_name.as_str())
                .collect::<Vec<_>>()
                .join(" or ")
        ))
        .hint("Say which with owner/name.")),
    }
}
