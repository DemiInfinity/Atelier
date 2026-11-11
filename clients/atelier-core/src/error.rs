//! One error type for everything the core does.

use std::path::PathBuf;

pub type Result<T> = std::result::Result<T, Error>;

#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// The studio answered with `{"error": "<code>", "message": "<sentence>"}`.
    #[error("{message}")]
    Api {
        status: u16,
        code: String,
        message: String,
    },
    /// No device token for this studio (keychain, file or `ATELIER_TOKEN`).
    #[error("You’re not signed in.")]
    NotSignedIn,
    /// No studio configured (`config.toml` or `ATELIER_STUDIO`).
    #[error("No studio is set up yet.")]
    NoStudio,
    /// The studio could not be reached (DNS, TLS, timeout, connection).
    #[error("Couldn’t reach your studio: {0}")]
    Network(String),
    /// The studio answered with something this client could not read.
    #[error("Your studio sent an answer this version doesn’t understand: {0}")]
    Decode(String),
    #[error("The sign-in was denied in the browser.")]
    DeviceDenied,
    #[error("The sign-in code expired before it was approved.")]
    DeviceExpired,
    #[error("{0}")]
    Config(String),
    #[error("{context}: {source}")]
    Io {
        context: String,
        #[source]
        source: std::io::Error,
    },
    /// `git` ran and failed.
    #[error("git {command} failed: {message}")]
    Git { command: String, message: String },
    /// A helper program (`git`, `ssh-keygen`) is not installed.
    #[error("{0} isn’t installed or isn’t on your PATH.")]
    MissingProgram(String),
    #[error("{0} isn’t a git repository.")]
    NotARepo(PathBuf),
    /// `origin` already points somewhere and the caller did not agree to replace it.
    #[error("origin already points at {url}.")]
    OriginExists { url: String },
    /// The runner is not running (no control socket).
    #[error("The runner isn’t running on this machine.")]
    RunnerNotRunning,
    /// The runner answered `{"ok": false, "error": "…"}` or something unreadable.
    #[error("The runner said: {0}")]
    Runner(String),
    #[error("Couldn’t use the keychain: {0}")]
    Keychain(String),
    #[error("{0}")]
    Ssh(String),
    #[error("{0}")]
    Invalid(String),
}

impl Error {
    pub(crate) fn io(context: impl Into<String>, source: std::io::Error) -> Self {
        Error::Io {
            context: context.into(),
            source,
        }
    }

    /// The server's machine code for API errors (`name_taken`, `revoked`, …).
    pub fn code(&self) -> Option<&str> {
        match self {
            Error::Api { code, .. } => Some(code),
            _ => None,
        }
    }

    /// The HTTP status for API errors.
    pub fn status(&self) -> Option<u16> {
        match self {
            Error::Api { status, .. } => Some(*status),
            _ => None,
        }
    }

    /// True when signing in (again) is the fix: no token, or the studio
    /// refused the one we have (`401`, e.g. `revoked`).
    pub fn needs_sign_in(&self) -> bool {
        matches!(self, Error::NotSignedIn | Error::Api { status: 401, .. })
    }

    /// A stable machine code for every error, for `--json` output and UIs.
    pub fn kind(&self) -> &str {
        match self {
            Error::Api { code, .. } => code,
            Error::NotSignedIn => "not_signed_in",
            Error::NoStudio => "no_studio",
            Error::Network(_) => "network",
            Error::Decode(_) => "decode",
            Error::DeviceDenied => "denied",
            Error::DeviceExpired => "expired",
            Error::Config(_) => "config",
            Error::Io { .. } => "io",
            Error::Git { .. } => "git",
            Error::MissingProgram(_) => "missing_program",
            Error::NotARepo(_) => "not_a_repo",
            Error::OriginExists { .. } => "origin_exists",
            Error::RunnerNotRunning => "runner_not_running",
            Error::Runner(_) => "runner",
            Error::Keychain(_) => "keychain",
            Error::Ssh(_) => "ssh",
            Error::Invalid(_) => "invalid",
        }
    }
}
