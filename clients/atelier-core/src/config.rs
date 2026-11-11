//! Client configuration (`<config dir>/atelier/config.toml`), shared by `atl`
//! and the desktop app. See section 11 of `docs/app-api.md`.
//!
//! Nothing here reads the process environment directly: every lookup goes
//! through an [`Env`], so tests can inject one.

use std::collections::HashMap;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::error::{Error, Result};
use crate::token::Token;

pub const ENV_STUDIO: &str = "ATELIER_STUDIO";
pub const ENV_TOKEN: &str = "ATELIER_TOKEN";
/// Overrides the config directory (tests, portable installs).
pub const ENV_CONFIG_DIR: &str = "ATELIER_CONFIG_DIR";
/// Overrides the directory SSH keys are looked up and generated in.
pub const ENV_SSH_DIR: &str = "ATELIER_SSH_DIR";
/// Overrides the runner's control socket path.
pub const ENV_RUNNER_SOCKET: &str = "ATELIER_RUNNER_SOCKET";
/// `file` forces the token file instead of the OS keychain.
pub const ENV_TOKEN_STORE: &str = "ATELIER_TOKEN_STORE";
pub const ENV_SSL_CERT_FILE: &str = "SSL_CERT_FILE";

const FILE_NAME: &str = "config.toml";

/// Where environment variables come from: the process, or a fixed map.
#[derive(Clone, Debug, Default)]
pub struct Env {
    fixed: Option<HashMap<String, String>>,
}

impl Env {
    /// The real process environment.
    pub fn process() -> Self {
        Env { fixed: None }
    }

    /// A fixed set of variables; the process environment is never consulted.
    pub fn fixed<K: Into<String>, V: Into<String>>(vars: impl IntoIterator<Item = (K, V)>) -> Self {
        Env {
            fixed: Some(
                vars.into_iter()
                    .map(|(k, v)| (k.into(), v.into()))
                    .collect(),
            ),
        }
    }

    /// The variable's value; unset and empty are the same thing.
    pub fn get(&self, key: &str) -> Option<String> {
        let value = match &self.fixed {
            Some(map) => map.get(key).cloned(),
            None => std::env::var(key).ok(),
        };
        value.filter(|v| !v.trim().is_empty())
    }
}

/// The contents of `config.toml`. Keys this version doesn't know are kept
/// and written back untouched.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Config {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub studio: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub device_id: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub device_name: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub clone_root: Option<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub watch: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub editor: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub git_tool: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub runner_config: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ca_file: Option<String>,
    #[serde(flatten)]
    pub extra: toml::Table,
}

impl Config {
    pub const DEFAULT_CLONE_ROOT: &'static str = "~/Atelier";

    /// Reads `config.toml` from `dir`. A missing file is an empty config.
    pub fn load(dir: &Path) -> Result<Config> {
        let path = dir.join(FILE_NAME);
        let text = match std::fs::read_to_string(&path) {
            Ok(text) => text,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Config::default()),
            Err(e) => return Err(Error::io(format!("Couldn’t read {}", path.display()), e)),
        };
        toml::from_str(&text).map_err(|e| {
            Error::Config(format!(
                "{} isn’t valid: {}",
                path.display(),
                e.message().trim()
            ))
        })
    }

    /// Deletes `config.toml` in `dir`. A file that isn't there is fine.
    pub fn remove(dir: &Path) -> Result<()> {
        let path = dir.join(FILE_NAME);
        match std::fs::remove_file(&path) {
            Err(e) if e.kind() != std::io::ErrorKind::NotFound => {
                Err(Error::io(format!("Couldn’t remove {}", path.display()), e))
            }
            _ => Ok(()),
        }
    }

    /// Writes `config.toml` into `dir` (created if needed), atomically.
    pub fn save(&self, dir: &Path) -> Result<()> {
        std::fs::create_dir_all(dir)
            .map_err(|e| Error::io(format!("Couldn’t create {}", dir.display()), e))?;
        let text = toml::to_string(self).map_err(|e| Error::Config(e.to_string()))?;
        let path = dir.join(FILE_NAME);
        let tmp = dir.join(format!("{FILE_NAME}.tmp"));
        std::fs::write(&tmp, text)
            .and_then(|_| std::fs::rename(&tmp, &path))
            .map_err(|e| Error::io(format!("Couldn’t write {}", path.display()), e))
    }
}

/// Turns what a person types (`studio.example`, `https://studio.example/`)
/// into the studio's base URL. Also the keychain account name.
pub fn normalize_studio(input: &str) -> String {
    let trimmed = input.trim();
    let (scheme, rest) = trimmed.split_once("://").unwrap_or(("https", trimmed));
    format!("{scheme}://{}", rest.trim_end_matches('/'))
}

/// Expands a leading `~` or `~/` to `home`. Everything else is returned as is.
pub fn expand_tilde(path: &str, home: &Path) -> PathBuf {
    if path == "~" {
        home.to_path_buf()
    } else if let Some(rest) = path.strip_prefix("~/").or_else(|| path.strip_prefix("~\\")) {
        home.join(rest)
    } else {
        PathBuf::from(path)
    }
}

/// The config file plus the environment and the directories it lives in:
/// everything resolved, ready to use.
#[derive(Clone, Debug)]
pub struct Settings {
    pub config: Config,
    /// The directory holding `config.toml` (`…/atelier`).
    pub config_dir: PathBuf,
    pub home: PathBuf,
    env: Env,
}

impl Settings {
    /// Loads the configuration for this environment.
    pub fn load(env: Env) -> Result<Settings> {
        let home = home_dir(&env)?;
        let config_dir = config_dir(&env)?;
        let config = Config::load(&config_dir)?;
        Ok(Settings {
            config,
            config_dir,
            home,
            env,
        })
    }

    /// Settings with explicit directories (tests, embedding).
    pub fn with_dirs(config: Config, config_dir: PathBuf, home: PathBuf, env: Env) -> Settings {
        Settings {
            config,
            config_dir,
            home,
            env,
        }
    }

    pub fn save(&self) -> Result<()> {
        self.config.save(&self.config_dir)
    }

    pub fn env(&self) -> &Env {
        &self.env
    }

    /// The studio's base URL: `ATELIER_STUDIO`, else `studio` in the file.
    pub fn studio(&self) -> Option<String> {
        self.env
            .get(ENV_STUDIO)
            .or_else(|| self.config.studio.clone())
            .map(|s| normalize_studio(&s))
    }

    /// `ATELIER_TOKEN`, when set. It wins over the keychain.
    pub fn token_override(&self) -> Option<Token> {
        self.env.get(ENV_TOKEN).map(Token::new)
    }

    /// Extra CA bundles to trust: `ca_file`, then `SSL_CERT_FILE`.
    pub fn ca_files(&self) -> Vec<PathBuf> {
        let mut files = Vec::new();
        if let Some(file) = self.config.ca_file.as_deref().filter(|f| !f.is_empty()) {
            files.push(self.expand(file));
        }
        if let Some(file) = self.env.get(ENV_SSL_CERT_FILE) {
            let path = self.expand(&file);
            if !files.contains(&path) {
                files.push(path);
            }
        }
        files
    }

    /// Where clones go (`clone_root`, default `~/Atelier`), `~` expanded.
    pub fn clone_root(&self) -> PathBuf {
        self.expand(
            self.config
                .clone_root
                .as_deref()
                .unwrap_or(Config::DEFAULT_CLONE_ROOT),
        )
    }

    /// The `watch` folders, `~` expanded.
    pub fn watch_dirs(&self) -> Vec<PathBuf> {
        self.config.watch.iter().map(|w| self.expand(w)).collect()
    }

    /// The clone root and the watch folders, without duplicates.
    pub fn repo_roots(&self) -> Vec<PathBuf> {
        let mut roots = vec![self.clone_root()];
        for dir in self.watch_dirs() {
            if !roots.contains(&dir) {
                roots.push(dir);
            }
        }
        roots
    }

    /// The runner's config file: `runner_config`, else
    /// `<config base>/atelier-runner/config.json`.
    pub fn runner_config_path(&self) -> PathBuf {
        match self.config.runner_config.as_deref() {
            Some(path) if !path.is_empty() => self.expand(path),
            _ => self
                .config_dir
                .parent()
                .unwrap_or(&self.config_dir)
                .join("atelier-runner")
                .join("config.json"),
        }
    }

    /// The runner's control socket: `ATELIER_RUNNER_SOCKET`, else
    /// `control.sock` next to the runner's config.
    pub fn runner_socket(&self) -> PathBuf {
        if let Some(path) = self.env.get(ENV_RUNNER_SOCKET) {
            return self.expand(&path);
        }
        let config = self.runner_config_path();
        config
            .parent()
            .map(|dir| dir.join("control.sock"))
            .unwrap_or_else(|| PathBuf::from("control.sock"))
    }

    /// Where SSH keys live: `ATELIER_SSH_DIR`, else `~/.ssh`.
    pub fn ssh_dir(&self) -> PathBuf {
        match self.env.get(ENV_SSH_DIR) {
            Some(dir) => self.expand(&dir),
            None => self.home.join(".ssh"),
        }
    }

    pub fn expand(&self, path: &str) -> PathBuf {
        expand_tilde(path, &self.home)
    }
}

/// The person's home directory (`HOME`/`USERPROFILE` from `env` first, so it
/// can be injected).
pub fn home_dir(env: &Env) -> Result<PathBuf> {
    if let Some(home) = env.get("HOME").or_else(|| env.get("USERPROFILE")) {
        return Ok(PathBuf::from(home));
    }
    directories::BaseDirs::new()
        .map(|dirs| dirs.home_dir().to_path_buf())
        .ok_or_else(|| Error::Config("Couldn’t find your home folder.".into()))
}

/// `<config dir>/atelier`: `ATELIER_CONFIG_DIR` when set, else the per-OS
/// place (`~/Library/Application Support/atelier`, `%APPDATA%\atelier`,
/// `$XDG_CONFIG_HOME/atelier`).
pub fn config_dir(env: &Env) -> Result<PathBuf> {
    if let Some(dir) = env.get(ENV_CONFIG_DIR) {
        return Ok(PathBuf::from(dir));
    }
    directories::BaseDirs::new()
        .map(|dirs| dirs.config_dir().join("atelier"))
        .ok_or_else(|| Error::Config("Couldn’t find your configuration folder.".into()))
}
