//! Errors as the person sees them: one plain sentence, then the next step.

use atelier_core::Error;

use crate::ctx::Ctx;

pub type CliResult<T> = Result<T, CliError>;

pub const EXIT_ERROR: i32 = 1;
pub const EXIT_NOT_SIGNED_IN: i32 = 3;

#[derive(Debug)]
pub struct CliError {
    /// A stable machine code (`not_signed_in`, `name_taken`, …).
    pub kind: String,
    pub message: String,
    /// What to do next.
    pub hint: Option<String>,
    pub exit: i32,
}

impl CliError {
    pub fn plain(message: impl Into<String>) -> CliError {
        CliError {
            kind: "error".into(),
            message: message.into(),
            hint: None,
            exit: EXIT_ERROR,
        }
    }

    pub fn hint(mut self, hint: impl Into<String>) -> CliError {
        self.hint = Some(hint.into());
        self
    }

    pub fn kind(mut self, kind: &str) -> CliError {
        self.kind = kind.into();
        self
    }

    pub fn from_core(e: Error, ctx: &Ctx) -> CliError {
        let studio = ctx.studio_for_hint();
        let hint = match &e {
            Error::NotSignedIn | Error::NoStudio => Some(format!("Run atl login {studio}.")),
            Error::Api { status: 401, .. } => Some(format!("Run atl login {studio} again.")),
            Error::Api { code, .. } if code == "name_taken" => {
                Some("Pick another with --name.".to_string())
            }
            Error::Api { code, .. } if code == "remote_only_off" => {
                Some("Turn it on from home.".to_string())
            }
            Error::Network(_) => Some(
                "Check that you’re on your home network, or that remote access is on.".to_string(),
            ),
            Error::DeviceDenied | Error::DeviceExpired => {
                Some(format!("Run atl login {studio} to try again."))
            }
            Error::RunnerNotRunning => Some("Start it with atelier-runner run.".to_string()),
            Error::NotARepo(_) => Some("Run git init here first.".to_string()),
            Error::MissingProgram(_) => Some("Install it and try again.".to_string()),
            Error::OriginExists { .. } => {
                Some("Add --replace-origin to point it at your studio.".to_string())
            }
            _ => None,
        };
        let message = match &e {
            // "You’re not signed in" covers "no studio set up" for a person.
            Error::NoStudio => Error::NotSignedIn.to_string(),
            Error::NotARepo(_) => "This folder isn’t a git repository.".to_string(),
            other => other.to_string(),
        };
        CliError {
            kind: match &e {
                Error::NoStudio => "not_signed_in".to_string(),
                other => other.kind().to_string(),
            },
            message: sentence(&message),
            hint,
            exit: if e.needs_sign_in() || matches!(e, Error::NoStudio) {
                EXIT_NOT_SIGNED_IN
            } else {
                EXIT_ERROR
            },
        }
    }

    /// Prints the error to standard error: a sentence and the next step, or
    /// one JSON object with `--json`.
    pub fn report(&self, json: bool) {
        if json {
            let mut object = serde_json::json!({"error": self.kind, "message": self.message});
            if let Some(hint) = &self.hint {
                object["hint"] = hint.clone().into();
            }
            eprintln!("{object}");
        } else {
            match &self.hint {
                Some(hint) => eprintln!("{} {hint}", self.message),
                None => eprintln!("{}", self.message),
            }
        }
    }
}

/// Makes sure a message ends like a sentence.
fn sentence(message: &str) -> String {
    let message = message.trim();
    if message.ends_with(['.', '!', '?', '…']) {
        message.to_string()
    } else {
        format!("{message}.")
    }
}
