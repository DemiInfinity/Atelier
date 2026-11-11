//! The app's state and everything it can do, free of any window toolkit so
//! it can be tested against a mock studio and throwaway repositories. The
//! Tauri commands in `commands.rs` are one-line wrappers around this.

use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, RwLock};
use std::time::{Duration, Instant};

use atelier_core::api::{
    Account, Device, DeviceKind, DevicePoll, DeviceStart, DeviceStartRequest, Me, Messages, Repo,
};
use atelier_core::config::{normalize_studio, Config, Env, Settings};
use atelier_core::runner::RunnerStatus;
use atelier_core::token::{default_store, Place};
use atelier_core::{Client, ClientOptions, Git, RunnerClient, TokenStore};
use serde::Serialize;

use crate::error::{CmdError, CmdResult};
use crate::paths;
use crate::prefs::Prefs;
use crate::tools::{self, PresetInfo};

/// Set to anything to keep the browser closed: links are recorded, not opened.
pub const ENV_NO_BROWSER: &str = "ATELIER_NO_BROWSER";
/// Set to anything to record editor and tool launches instead of running them,
/// and to leave login items and native notifications alone.
pub const ENV_DRY_RUN: &str = "ATELIER_DESKTOP_DRY_RUN";
/// Makes the choice of friendly message repeatable.
pub const ENV_MESSAGE_SEED: &str = "ATELIER_MESSAGE_SEED";
/// How deep under a root the app looks for git folders.
pub const SCAN_DEPTH: usize = 4;

/// The things the app does to the world outside itself, so tests can watch
/// instead of opening browsers and editors.
pub trait Effects: Send + Sync + 'static {
    fn open_url(&self, url: &str) -> Result<(), String>;
    fn launch(&self, program: &str, args: &[String]) -> Result<(), String>;
}

/// Remembers what would have been opened or launched, and does nothing.
#[derive(Debug, Default)]
pub struct Recorder {
    pub opened: Mutex<Vec<String>>,
    pub launched: Mutex<Vec<(String, Vec<String>)>>,
}

impl Effects for Recorder {
    fn open_url(&self, url: &str) -> Result<(), String> {
        lock(&self.opened).push(url.to_string());
        Ok(())
    }

    fn launch(&self, program: &str, args: &[String]) -> Result<(), String> {
        lock(&self.launched).push((program.to_string(), args.to_vec()));
        Ok(())
    }
}

pub(crate) fn lock<T>(mutex: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(|e| e.into_inner())
}

struct Pending {
    client: Client,
    studio: String,
    start: DeviceStart,
    started: Instant,
}

/// What the app shows about itself on every screen.
#[derive(Clone, Debug, Serialize)]
pub struct AppState {
    pub signed_in: bool,
    /// The studio couldn't be reached just now; what is shown may be stale.
    pub offline: bool,
    /// Why the app is signed out or offline, when it has something to say.
    pub note: Option<String>,
    pub studio_url: Option<String>,
    pub studio_host: Option<String>,
    pub studio_name: Option<String>,
    pub device_name: String,
    pub person_name: Option<String>,
    pub person_login: Option<String>,
    pub accounts: Vec<Account>,
    /// The owner lets this device rename and delete repositories.
    pub can_admin: bool,
    pub tether: bool,
    pub connect: bool,
    pub clone_root: String,
    pub watch: Vec<String>,
    pub editor: PresetInfo,
    pub git_tool: Option<PresetInfo>,
    pub editors: Vec<PresetInfo>,
    pub git_tools: Vec<PresetInfo>,
    pub prefs: Prefs,
    /// `macos`, `windows` or `linux`.
    pub os: &'static str,
    /// "Mac", "PC" or "computer", for sentences like "On this Mac".
    pub machine: &'static str,
    pub version: &'static str,
}

#[derive(Clone, Debug, Serialize)]
pub struct ConnectStarted {
    pub studio: String,
    pub studio_host: String,
    pub user_code: String,
    pub verify_url: String,
    pub expires_in: u64,
    pub interval: u64,
    /// The approval page was opened in the browser.
    pub opened: bool,
}

#[derive(Clone, Debug, Serialize)]
pub struct SshOutcome {
    pub fingerprint: String,
    /// A new key was made for this device (otherwise an existing one is used).
    pub generated: bool,
    pub key_path: String,
}

#[derive(Clone, Debug, Serialize)]
pub struct Connected {
    pub person_name: String,
    pub studio_name: String,
    pub device_name: String,
    /// Where the token went: `keychain` or `file`.
    pub token_place: &'static str,
    pub ssh: Option<SshOutcome>,
    /// Why the SSH key couldn't be registered, when it couldn't.
    pub ssh_error: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
#[serde(tag = "status", rename_all = "snake_case")]
pub enum ConnectPoll {
    Pending { seconds_left: u64 },
    Approved(Connected),
}

pub struct Desk {
    settings: RwLock<Settings>,
    store: Box<dyn TokenStore>,
    base_git: Git,
    effects: Arc<dyn Effects>,
    client: Mutex<Option<Client>>,
    pending: tokio::sync::Mutex<Option<Pending>>,
    me: Mutex<Option<(Instant, Me)>>,
    messages: Mutex<Option<(Instant, Messages)>>,
    repos: Mutex<Option<(Instant, Vec<Repo>)>>,
    note: Mutex<Option<String>>,
    pub(crate) os: &'static str,
}

impl Desk {
    /// Loads the configuration the way `atl` does: `ATELIER_CONFIG_DIR`,
    /// `ATELIER_TOKEN_STORE=file` and friends are honoured.
    pub fn load(env: Env, effects: Arc<dyn Effects>) -> CmdResult<Desk> {
        let settings = Settings::load(env)?;
        Ok(Desk::with(settings, Git::new(), effects))
    }

    /// For tests: explicit settings and a git that carries its own
    /// environment.
    pub fn with(settings: Settings, git: Git, effects: Arc<dyn Effects>) -> Desk {
        let store = default_store(&settings.config_dir, settings.env());
        Desk {
            settings: RwLock::new(settings),
            store,
            base_git: git,
            effects,
            client: Mutex::new(None),
            pending: tokio::sync::Mutex::new(None),
            me: Mutex::new(None),
            messages: Mutex::new(None),
            repos: Mutex::new(None),
            note: Mutex::new(None),
            os: std::env::consts::OS,
        }
    }

    // ------------------------------------------------------------ settings

    pub fn settings(&self) -> Settings {
        self.settings
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }

    /// Changes the configuration and writes it to disk.
    pub fn update_config(&self, change: impl FnOnce(&mut Config)) -> CmdResult<()> {
        let mut settings = self.settings.write().unwrap_or_else(|e| e.into_inner());
        change(&mut settings.config);
        settings.save()?;
        Ok(())
    }

    pub fn prefs(&self) -> Prefs {
        Prefs::read(&self.settings().config)
    }

    pub fn update_prefs(&self, change: impl FnOnce(&mut Prefs)) -> CmdResult<Prefs> {
        let mut prefs = self.prefs();
        change(&mut prefs);
        self.update_config(|config| prefs.write(config))?;
        Ok(prefs)
    }

    pub fn env_flag(&self, name: &str) -> bool {
        self.settings().env().get(name).is_some()
    }

    pub fn dry_run(&self) -> bool {
        self.env_flag(ENV_DRY_RUN)
    }

    pub fn seed(&self) -> Option<u64> {
        self.settings()
            .env()
            .get(ENV_MESSAGE_SEED)
            .and_then(|s| s.parse().ok())
    }

    pub fn device_name(&self) -> String {
        self.settings()
            .config
            .device_name
            .filter(|n| !n.is_empty())
            .unwrap_or_else(host_name)
    }

    /// "Mac", "PC" or "computer".
    pub fn machine(&self) -> &'static str {
        match self.os {
            "macos" => "Mac",
            "windows" => "PC",
            _ => "computer",
        }
    }

    // --------------------------------------------------------------- git

    /// Git as this device: with the dedicated SSH key when there is one.
    pub fn git(&self) -> Git {
        match atelier_core::ssh::dedicated_identity(&self.settings().ssh_dir()) {
            Some(identity) => self.base_git.clone().with_ssh_identity(identity),
            None => self.base_git.clone(),
        }
    }

    /// The folders repositories may live in: the clone root, the watch
    /// folders, and folders picked with the chooser.
    pub fn roots(&self) -> Vec<PathBuf> {
        let settings = self.settings();
        let mut roots = settings.repo_roots();
        for picked in Prefs::read(&settings.config).picked {
            let path = PathBuf::from(picked);
            if !roots.contains(&path) {
                roots.push(path);
            }
        }
        roots
    }

    /// A working copy the app may touch: inside one of the roots, and the
    /// top folder of a git repository.
    pub async fn repo_path(&self, path: &str) -> CmdResult<PathBuf> {
        let roots = self.roots();
        let real = paths::validate(&roots, path)?;
        let top = self
            .git()
            .toplevel(&real)
            .await?
            .ok_or_else(|| CmdError::from(atelier_core::Error::NotARepo(real.clone())))?;
        // A folder inside a root whose repository starts above the root.
        let top = paths::validate(&roots, &top.to_string_lossy())?;
        Ok(top)
    }

    pub fn display_path(&self, path: &Path) -> String {
        paths::display(path, &self.settings().home)
    }

    // ------------------------------------------------------------- studio

    pub fn signed_in(&self) -> bool {
        let settings = self.settings();
        match settings.studio() {
            Some(studio) => {
                settings.token_override().is_some()
                    || matches!(self.store.get(&studio), Ok(Some(_)))
            }
            None => false,
        }
    }

    pub fn client(&self) -> CmdResult<Client> {
        if let Some(client) = lock(&self.client).clone() {
            return Ok(client);
        }
        let client = Client::from_settings(&self.settings(), self.store.as_ref())?;
        *lock(&self.client) = Some(client.clone());
        Ok(client)
    }

    fn forget_studio(&self) {
        *lock(&self.client) = None;
        *lock(&self.me) = None;
        *lock(&self.messages) = None;
        *lock(&self.repos) = None;
    }

    /// Called with every studio error: a refused token signs the app out of
    /// its caches so the next screen is the welcome one.
    pub(crate) fn watch<T>(&self, result: atelier_core::Result<T>) -> CmdResult<T> {
        if let Err(e) = &result {
            if e.needs_sign_in() && matches!(e, atelier_core::Error::Api { .. }) {
                *lock(&self.note) = Some(
                    "This device was signed out of your studio. Connect it again.".to_string(),
                );
            }
        }
        result.map_err(CmdError::from)
    }

    pub async fn me(&self, max_age: Duration) -> CmdResult<Me> {
        if let Some((at, me)) = lock(&self.me).clone() {
            if at.elapsed() <= max_age {
                return Ok(me);
            }
        }
        let me = self.watch(self.client()?.me().await)?;
        *lock(&self.me) = Some((Instant::now(), me.clone()));
        Ok(me)
    }

    /// `/me` from memory, however old.
    pub fn me_cached(&self) -> Option<Me> {
        lock(&self.me).as_ref().map(|(_, me)| me.clone())
    }

    pub async fn messages(&self, max_age: Duration) -> CmdResult<Messages> {
        if let Some((at, messages)) = lock(&self.messages).clone() {
            if at.elapsed() <= max_age {
                return Ok(messages);
            }
        }
        let messages = self.watch(self.client()?.messages().await)?;
        self.remember_messages(&messages);
        Ok(messages)
    }

    pub(crate) fn remember_messages(&self, messages: &Messages) {
        *lock(&self.messages) = Some((Instant::now(), messages.clone()));
    }

    pub async fn repos(&self, max_age: Duration) -> CmdResult<Vec<Repo>> {
        if let Some((at, repos)) = lock(&self.repos).clone() {
            if at.elapsed() <= max_age {
                return Ok(repos);
            }
        }
        let repos = self.watch(self.client()?.repos().await)?;
        *lock(&self.repos) = Some((Instant::now(), repos.clone()));
        Ok(repos)
    }

    pub(crate) fn repos_cached(&self) -> Option<Vec<Repo>> {
        lock(&self.repos).as_ref().map(|(_, r)| r.clone())
    }

    pub(crate) fn forget_repos(&self) {
        *lock(&self.repos) = None;
    }

    /// The account's display name (`Elyndra Studios`) for an owner name.
    pub(crate) fn account_label(&self, owner: &str) -> String {
        self.me_cached()
            .and_then(|me| {
                me.all_owners()
                    .into_iter()
                    .find(|a| a.name.eq_ignore_ascii_case(owner))
                    .map(|a| {
                        if a.kind == "user" || a.display.is_empty() {
                            a.name
                        } else {
                            a.display
                        }
                    })
            })
            .unwrap_or_else(|| owner.to_string())
    }

    // -------------------------------------------------------------- links

    /// True for the studio's own origin and for `https://github.com`.
    pub fn url_allowed(&self, url: &str) -> bool {
        let Ok(parsed) = url::Url::parse(url) else {
            return false;
        };
        if parsed.scheme() == "https"
            && parsed.host_str() == Some("github.com")
            && parsed.port().is_none()
            && parsed.username().is_empty()
        {
            return true;
        }
        if !matches!(parsed.scheme(), "http" | "https") || !parsed.username().is_empty() {
            return false;
        }
        let pending_or_configured = self.settings().studio();
        pending_or_configured
            .and_then(|studio| url::Url::parse(&studio).ok())
            .is_some_and(|studio| studio.origin() == parsed.origin())
    }

    /// Opens a link in the browser, if it leads to the studio or GitHub.
    pub fn open_url(&self, url: &str) -> CmdResult<()> {
        if !self.url_allowed(url) {
            return Err(CmdError::new(
                "link_refused",
                "That link doesn’t lead to your studio or to GitHub, so it wasn’t opened.",
            ));
        }
        self.effects
            .open_url(url)
            .map_err(|e| CmdError::new("open_failed", format!("Couldn’t open the browser: {e}")))
    }

    /// Opens a link someone wrote in an issue or a comment. Only web and
    /// mail addresses are ever opened (see [`crate::markdown::safe_url`]).
    /// One that leads outside the studio and GitHub is refused with
    /// `link_outside` until the person has seen it and said yes (`sure`).
    pub fn open_link(&self, url: &str, sure: bool) -> CmdResult<()> {
        let Some(url) = crate::markdown::safe_url(url) else {
            return Err(CmdError::new(
                "link_refused",
                "That isn’t a web or mail address, so it wasn’t opened.",
            ));
        };
        if !sure && !self.url_allowed(&url) {
            return Err(CmdError::new(
                "link_outside",
                "That link leads outside your studio.",
            ));
        }
        self.effects
            .open_url(&url)
            .map_err(|e| CmdError::new("open_failed", format!("Couldn’t open the browser: {e}")))
    }

    /// Opens a page of the studio (`/owner/name/pulls/3`) in the browser.
    pub fn open_web(&self, web_path: &str) -> CmdResult<()> {
        let studio = self
            .settings()
            .studio()
            .ok_or_else(|| CmdError::from(atelier_core::Error::NoStudio))?;
        // Only a path: nothing that could change the host.
        if !web_path.starts_with('/') || web_path.starts_with("//") || web_path.contains('\\') {
            return Err(CmdError::new(
                "link_refused",
                "That isn’t a page of your studio.",
            ));
        }
        self.open_url(&format!("{studio}{web_path}"))
    }

    /// Opens `https://github.com/<owner>/<name>`.
    pub fn open_github(&self, full_name: &str) -> CmdResult<()> {
        let (owner, name) = paths::split_full_name(full_name)?;
        self.open_url(&format!("https://github.com/{owner}/{name}"))
    }

    /// Opens a folder in a built-in editor or tool. Returns the tool's label.
    pub fn launch(&self, preset_id: &str, folder: &Path) -> CmdResult<String> {
        let preset = tools::find(preset_id).ok_or_else(|| {
            CmdError::invalid(format!("“{preset_id}” isn’t an editor Atelier knows."))
        })?;
        let (program, args) = tools::launch_plan(preset.id, folder, self.os).ok_or_else(|| {
            CmdError::invalid(format!("{} isn’t available on this system.", preset.label))
        })?;
        self.effects.launch(&program, &args).map_err(|e| {
            CmdError::new(
                "launch_failed",
                format!("Couldn’t open {}: {e}", preset.label),
            )
            .hint(format!("Is {} installed?", preset.label))
        })?;
        Ok(preset.label.to_string())
    }

    // ---------------------------------------------------------- app state

    pub async fn app_state(&self) -> AppState {
        self.app_state_within(Duration::from_secs(60)).await
    }

    /// The app's state with `/me` asked again now (what this device may do
    /// can change at any time in the studio's settings).
    pub async fn app_state_fresh(&self) -> AppState {
        self.app_state_within(Duration::ZERO).await
    }

    async fn app_state_within(&self, max_age: Duration) -> AppState {
        let settings = self.settings();
        let studio = settings.studio();
        let mut signed_in = self.signed_in();
        let mut offline = false;
        let mut note = lock(&self.note).clone();
        let mut me = None;
        if signed_in {
            match self.me(max_age).await {
                Ok(found) => {
                    me = Some(found);
                    note = None;
                    *lock(&self.note) = None;
                }
                Err(e) if e.kind == "network" => {
                    offline = true;
                    me = self.me_cached();
                    note = Some(e.message);
                }
                Err(e) if matches!(e.kind.as_str(), "revoked" | "unauthorized" | "http_401") => {
                    signed_in = false;
                    note = Some(
                        "This device was signed out of your studio. Connect it again.".to_string(),
                    );
                }
                Err(e) => {
                    offline = true;
                    me = self.me_cached();
                    note = Some(e.message);
                }
            }
        }
        let editor = tools::editor_or_default(settings.config.editor.as_deref());
        let git_tool = tools::git_tool_or_default(settings.config.git_tool.as_deref());
        let info = |p: &tools::Preset| PresetInfo {
            id: p.id.to_string(),
            label: p.label.to_string(),
        };
        AppState {
            signed_in,
            offline,
            note,
            studio_host: studio
                .as_deref()
                .and_then(|s| url::Url::parse(s).ok())
                .and_then(|u| u.host_str().map(str::to_string)),
            studio_url: studio,
            studio_name: me.as_ref().map(|m| m.studio.name.clone()),
            device_name: me
                .as_ref()
                .and_then(|m| m.device.as_ref().map(|d| d.name.clone()))
                .filter(|n| !n.is_empty())
                .unwrap_or_else(|| self.device_name()),
            person_name: me.as_ref().map(|m| m.person.name.clone()),
            person_login: me.as_ref().map(|m| m.person.login.clone()),
            accounts: me.as_ref().map(Me::all_owners).unwrap_or_default(),
            can_admin: me
                .as_ref()
                .and_then(|m| m.device.as_ref())
                .is_some_and(Device::may_admin),
            tether: me.as_ref().is_some_and(|m| m.studio.tether),
            connect: me.as_ref().is_some_and(|m| m.studio.connect),
            clone_root: paths::display(&settings.clone_root(), &settings.home),
            watch: settings
                .watch_dirs()
                .iter()
                .map(|w| paths::display(w, &settings.home))
                .collect(),
            editor: info(editor),
            git_tool: git_tool.map(info),
            editors: tools::editors(self.os),
            git_tools: tools::git_tools(self.os),
            prefs: Prefs::read(&settings.config),
            os: self.os,
            machine: self.machine(),
            version: env!("CARGO_PKG_VERSION"),
        }
    }

    // ------------------------------------------------------------ sign-in

    /// Step one of first run: asks the studio at `address` for a code and
    /// opens its approval page in the browser.
    pub async fn connect_start(&self, address: &str) -> CmdResult<ConnectStarted> {
        let address = address.trim();
        if address.is_empty() {
            return Err(CmdError::invalid("Type your studio’s address first."));
        }
        let studio = normalize_studio(address);
        let settings = self.settings();
        let options = ClientOptions {
            ca_files: settings.ca_files(),
            user_agent: format!("atelier-desktop/{}", env!("CARGO_PKG_VERSION")),
            ..ClientOptions::default()
        };
        let client = Client::new(&studio, None, options)?;
        let mut request = DeviceStartRequest::new(host_name(), DeviceKind::Desktop);
        request.version = env!("CARGO_PKG_VERSION").to_string();
        let start = client.device_start(&request).await.map_err(|e| match e {
            atelier_core::Error::Network(_) => CmdError::from(e).hint(
                "Check the address, and that this machine is on the same network as your studio.",
            ),
            atelier_core::Error::Api { status: 404, .. } | atelier_core::Error::Decode(_) => {
                CmdError::new(
                    "not_a_studio",
                    "There is no Atelier studio at that address.",
                )
                .hint("Use the address you open Atelier with in your browser.")
            }
            other => CmdError::from(other),
        })?;
        if start.user_code.is_empty() || start.device_code.is_empty() {
            return Err(CmdError::new(
                "not_a_studio",
                "There is no Atelier studio at that address.",
            ));
        }
        // The approval page must be on the studio itself.
        let studio_url = client.studio_url();
        let same_origin = match (
            url::Url::parse(&start.verify_url),
            url::Url::parse(&studio_url),
        ) {
            (Ok(verify), Ok(base)) => verify.origin() == base.origin(),
            _ => false,
        };
        let mut opened = false;
        if same_origin && !self.env_flag(ENV_NO_BROWSER) {
            opened = self.effects.open_url(&start.verify_url).is_ok();
        }
        let started = ConnectStarted {
            studio_host: client.studio_host(),
            studio: studio_url.clone(),
            user_code: start.user_code.clone(),
            verify_url: if same_origin {
                start.verify_url.clone()
            } else {
                String::new()
            },
            expires_in: start.expires_in,
            interval: start.interval.max(1),
            opened,
        };
        *self.pending.lock().await = Some(Pending {
            client,
            studio: studio_url,
            start,
            started: Instant::now(),
        });
        Ok(started)
    }

    /// Opens the approval page again.
    pub async fn connect_reopen(&self) -> CmdResult<()> {
        let pending = self.pending.lock().await;
        let pending = pending
            .as_ref()
            .ok_or_else(|| CmdError::invalid("There is no sign-in under way."))?;
        let (verify, base) = (
            url::Url::parse(&pending.start.verify_url),
            url::Url::parse(&pending.studio),
        );
        match (verify, base) {
            (Ok(verify), Ok(base)) if verify.origin() == base.origin() => {
                self.effects.open_url(verify.as_str()).map_err(|e| {
                    CmdError::new("open_failed", format!("Couldn’t open the browser: {e}"))
                })
            }
            _ => Err(CmdError::new(
                "link_refused",
                "The approval page isn’t on your studio, so it wasn’t opened.",
            )),
        }
    }

    pub async fn connect_cancel(&self) {
        *self.pending.lock().await = None;
    }

    /// Step two: asks once whether the code was approved. On approval the
    /// token goes to the keychain, the configuration is written, and this
    /// device's SSH key is registered.
    pub async fn connect_poll(&self) -> CmdResult<ConnectPoll> {
        let mut guard = self.pending.lock().await;
        let pending = guard
            .as_ref()
            .ok_or_else(|| CmdError::invalid("There is no sign-in under way."))?;
        let left = pending
            .start
            .expires_in
            .saturating_sub(pending.started.elapsed().as_secs());
        if left == 0 {
            *guard = None;
            return Err(atelier_core::Error::DeviceExpired.into());
        }
        let grant = match pending.client.device_poll(&pending.start.device_code).await {
            Ok(DevicePoll::Pending) => return Ok(ConnectPoll::Pending { seconds_left: left }),
            // A dropped connection isn't a refusal; keep asking.
            Err(atelier_core::Error::Network(_)) => {
                return Ok(ConnectPoll::Pending { seconds_left: left })
            }
            Ok(DevicePoll::Approved(grant)) => *grant,
            Err(e) => {
                *guard = None;
                return Err(e.into());
            }
        };
        let studio = pending.studio.clone();
        let client = pending.client.clone().with_token(grant.token.clone());
        *guard = None;
        drop(guard);

        let place = self.store.set(&studio, &grant.token)?;
        // `/me` knows the name the studio really gave this device.
        let me = client.me().await.ok();
        let device_name = me
            .as_ref()
            .and_then(|m| m.device.as_ref().map(|d| d.name.clone()))
            .filter(|n| !n.is_empty())
            .unwrap_or_else(host_name);
        self.update_config(|config| {
            config.studio = Some(studio.clone());
            config.device_id = Some(grant.device_id);
            config.device_name = Some(device_name.clone());
        })?;
        self.forget_studio();
        *lock(&self.note) = None;
        if let Some(me) = &me {
            *lock(&self.me) = Some((Instant::now(), me.clone()));
        }
        *lock(&self.client) = Some(client.clone());

        let title = format!("{device_name} (Atelier Desktop)");
        let ssh_dir = self.settings().ssh_dir();
        let (ssh, ssh_error) =
            match atelier_core::ssh::register_device_key(&client, &ssh_dir, &title).await {
                Ok(registered) => (
                    Some(SshOutcome {
                        fingerprint: registered.registered.fingerprint,
                        generated: registered.generated,
                        key_path: self.display_path(&registered.key.path),
                    }),
                    None,
                ),
                Err(e) => (None, Some(CmdError::from(e).message)),
            };
        Ok(ConnectPoll::Approved(Connected {
            person_name: grant.person.name,
            studio_name: grant.studio.name,
            device_name,
            token_place: match place {
                Place::Keychain => "keychain",
                Place::File(_) => "file",
                Place::Memory => "memory",
            },
            ssh,
            ssh_error,
        }))
    }

    /// Forgets the studio on this machine: the token and the configuration
    /// file go. The device stays listed in the studio until it is revoked
    /// there (Settings → Devices).
    pub fn disconnect(&self) -> CmdResult<()> {
        let settings = self.settings();
        if let Some(studio) = settings.studio() {
            self.store.delete(&studio)?;
        }
        Config::remove(&settings.config_dir)?;
        {
            let mut settings = self.settings.write().unwrap_or_else(|e| e.into_inner());
            settings.config = Config::default();
        }
        self.forget_studio();
        *lock(&self.note) = None;
        Ok(())
    }

    // -------------------------------------------------------------- runner

    pub fn runner_client(&self) -> RunnerClient {
        RunnerClient::new(self.settings().runner_socket()).with_timeout(Duration::from_secs(2))
    }

    /// The runner's status, or `None` when it isn't running here.
    pub async fn runner_status(&self) -> Option<RunnerStatus> {
        self.runner_client().status().await.ok()
    }
}

/// This machine's name, without `.local`.
pub fn host_name() -> String {
    let name = gethostname::gethostname().to_string_lossy().into_owned();
    let name = name
        .strip_suffix(".local")
        .or_else(|| name.strip_suffix(".lan"))
        .unwrap_or(&name)
        .trim()
        .to_string();
    if name.is_empty() {
        "This computer".to_string()
    } else {
        name
    }
}

/// The local time, when the system will say what the offset is.
pub fn local_now() -> time::OffsetDateTime {
    let now = time::OffsetDateTime::now_utc();
    match time::UtcOffset::current_local_offset() {
        Ok(offset) => now.to_offset(offset),
        Err(_) => now,
    }
}
