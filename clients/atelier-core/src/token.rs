//! Device tokens and where they are kept: the OS keychain (service
//! `atelier`, account = studio URL), with a `0600` file only when no keychain
//! is available.

use std::collections::BTreeMap;
use std::fmt;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use crate::config::{Env, ENV_TOKEN_STORE};
use crate::error::{Error, Result};

pub const KEYCHAIN_SERVICE: &str = "atelier";
const TOKEN_FILE: &str = "tokens.json";

/// A device or access token. `Debug` and `Display` never show it.
#[derive(Clone, PartialEq, Eq)]
pub struct Token(String);

impl Token {
    pub fn new(value: impl Into<String>) -> Self {
        Token(value.into().trim().to_string())
    }

    /// The secret itself, for the `Authorization` header and for storing.
    pub fn expose(&self) -> &str {
        &self.0
    }
}

impl fmt::Debug for Token {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("Token(REDACTED)")
    }
}

impl fmt::Display for Token {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("REDACTED")
    }
}

impl<'de> serde::Deserialize<'de> for Token {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> std::result::Result<Self, D::Error> {
        String::deserialize(d).map(Token::new)
    }
}

/// Where a token ended up (or came from).
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Place {
    Keychain,
    /// The fallback file, used because no keychain is available.
    File(PathBuf),
    Memory,
}

/// Somewhere tokens are kept, one per studio URL.
pub trait TokenStore: Send + Sync {
    fn get(&self, studio: &str) -> Result<Option<Token>>;
    /// Stores the token and says where it went.
    fn set(&self, studio: &str, token: &Token) -> Result<Place>;
    /// Removes the token. Removing one that isn't there is fine.
    fn delete(&self, studio: &str) -> Result<()>;
}

/// The OS keychain: Keychain on macOS, Credential Manager on Windows, the
/// Secret Service on Linux.
#[derive(Debug, Default)]
pub struct KeychainStore;

impl KeychainStore {
    fn entry(studio: &str) -> std::result::Result<keyring::Entry, keyring::Error> {
        keyring::Entry::new(KEYCHAIN_SERVICE, studio)
    }
}

/// True when the error means "there is no usable keychain here" rather than
/// "the keychain refused this one thing".
fn unavailable(e: &keyring::Error) -> bool {
    matches!(
        e,
        keyring::Error::NoStorageAccess(_) | keyring::Error::PlatformFailure(_)
    )
}

/// `Error::Keychain` means "no usable keychain here" (the file fallback may
/// step in); anything else is a real refusal. keyring's messages never
/// include the secret.
fn keychain_error(e: keyring::Error) -> Error {
    if unavailable(&e) {
        Error::Keychain(e.to_string())
    } else {
        Error::Invalid(format!("The keychain refused: {e}"))
    }
}

impl TokenStore for KeychainStore {
    fn get(&self, studio: &str) -> Result<Option<Token>> {
        match Self::entry(studio).and_then(|e| e.get_password()) {
            Ok(secret) => Ok(Some(Token::new(secret))),
            Err(keyring::Error::NoEntry) => Ok(None),
            Err(e) => Err(keychain_error(e)),
        }
    }

    fn set(&self, studio: &str, token: &Token) -> Result<Place> {
        Self::entry(studio)
            .and_then(|e| e.set_password(token.expose()))
            .map(|_| Place::Keychain)
            .map_err(keychain_error)
    }

    fn delete(&self, studio: &str) -> Result<()> {
        match Self::entry(studio).and_then(|e| e.delete_credential()) {
            Ok(()) | Err(keyring::Error::NoEntry) => Ok(()),
            Err(e) => Err(keychain_error(e)),
        }
    }
}

/// A JSON file of `{studio: token}`, readable only by its owner.
#[derive(Debug)]
pub struct FileStore {
    path: PathBuf,
}

impl FileStore {
    /// `tokens.json` inside the config directory.
    pub fn in_dir(config_dir: &Path) -> Self {
        FileStore {
            path: config_dir.join(TOKEN_FILE),
        }
    }

    pub fn path(&self) -> &Path {
        &self.path
    }

    fn read(&self) -> Result<BTreeMap<String, String>> {
        match std::fs::read_to_string(&self.path) {
            Ok(text) => serde_json::from_str(&text).map_err(|_| {
                Error::Config(format!("{} isn’t a valid token file.", self.path.display()))
            }),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(BTreeMap::new()),
            Err(e) => Err(Error::io(
                format!("Couldn’t read {}", self.path.display()),
                e,
            )),
        }
    }

    fn write(&self, map: &BTreeMap<String, String>) -> Result<()> {
        let context = || format!("Couldn’t write {}", self.path.display());
        if let Some(dir) = self.path.parent() {
            std::fs::create_dir_all(dir).map_err(|e| Error::io(context(), e))?;
        }
        if map.is_empty() {
            return match std::fs::remove_file(&self.path) {
                Err(e) if e.kind() != std::io::ErrorKind::NotFound => Err(Error::io(context(), e)),
                _ => Ok(()),
            };
        }
        let text = serde_json::to_string_pretty(map).map_err(|e| Error::Config(e.to_string()))?;
        let tmp = self.path.with_extension("json.tmp");
        write_private(&tmp, text.as_bytes())
            .and_then(|_| std::fs::rename(&tmp, &self.path))
            .map_err(|e| Error::io(context(), e))
    }
}

/// Creates (or truncates) a file that only its owner can read, and writes to it.
fn write_private(path: &Path, bytes: &[u8]) -> std::io::Result<()> {
    use std::io::Write;
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let mut file = options.open(path)?;
    #[cfg(unix)]
    {
        // An older file may have been created with wider permissions.
        use std::os::unix::fs::PermissionsExt;
        file.set_permissions(std::fs::Permissions::from_mode(0o600))?;
    }
    file.write_all(bytes)?;
    file.sync_all()
}

impl TokenStore for FileStore {
    fn get(&self, studio: &str) -> Result<Option<Token>> {
        Ok(self.read()?.get(studio).map(Token::new))
    }

    fn set(&self, studio: &str, token: &Token) -> Result<Place> {
        let mut map = self.read()?;
        map.insert(studio.to_string(), token.expose().to_string());
        self.write(&map)?;
        Ok(Place::File(self.path.clone()))
    }

    fn delete(&self, studio: &str) -> Result<()> {
        let mut map = self.read()?;
        if map.remove(studio).is_some() {
            self.write(&map)?;
        }
        Ok(())
    }
}

/// Tokens kept in memory only (tests, previews).
#[derive(Debug, Default)]
pub struct MemoryStore {
    tokens: Mutex<BTreeMap<String, String>>,
}

impl MemoryStore {
    fn lock(&self) -> std::sync::MutexGuard<'_, BTreeMap<String, String>> {
        self.tokens.lock().unwrap_or_else(|e| e.into_inner())
    }
}

impl TokenStore for MemoryStore {
    fn get(&self, studio: &str) -> Result<Option<Token>> {
        Ok(self.lock().get(studio).map(Token::new))
    }

    fn set(&self, studio: &str, token: &Token) -> Result<Place> {
        self.lock()
            .insert(studio.to_string(), token.expose().to_string());
        Ok(Place::Memory)
    }

    fn delete(&self, studio: &str) -> Result<()> {
        self.lock().remove(studio);
        Ok(())
    }
}

/// The keychain first; the file only when the keychain isn't available.
pub struct FallbackStore {
    primary: Box<dyn TokenStore>,
    primary_unavailable: fn(&Error) -> bool,
    file: FileStore,
}

impl FallbackStore {
    pub fn new(primary: Box<dyn TokenStore>, file: FileStore) -> Self {
        FallbackStore {
            primary,
            primary_unavailable: |e| matches!(e, Error::Keychain(_)),
            file,
        }
    }
}

impl TokenStore for FallbackStore {
    fn get(&self, studio: &str) -> Result<Option<Token>> {
        match self.primary.get(studio) {
            Ok(Some(token)) => Ok(Some(token)),
            Ok(None) => self.file.get(studio),
            Err(e) if (self.primary_unavailable)(&e) => self.file.get(studio),
            Err(e) => Err(e),
        }
    }

    fn set(&self, studio: &str, token: &Token) -> Result<Place> {
        match self.primary.set(studio, token) {
            Ok(place) => {
                // Don't leave an older copy lying in the file.
                let _ = self.file.delete(studio);
                Ok(place)
            }
            Err(e) if (self.primary_unavailable)(&e) => self.file.set(studio, token),
            Err(e) => Err(e),
        }
    }

    fn delete(&self, studio: &str) -> Result<()> {
        let primary = self.primary.delete(studio);
        self.file.delete(studio)?;
        match primary {
            Err(e) if !(self.primary_unavailable)(&e) => Err(e),
            _ => Ok(()),
        }
    }
}

/// The store to use on this machine: the OS keychain with the file as a
/// fallback, or just the file when `ATELIER_TOKEN_STORE=file`.
pub fn default_store(config_dir: &Path, env: &Env) -> Box<dyn TokenStore> {
    let file = FileStore::in_dir(config_dir);
    match env.get(ENV_TOKEN_STORE).as_deref() {
        Some("file") => Box::new(file),
        _ => Box::new(FallbackStore::new(Box::new(KeychainStore), file)),
    }
}
