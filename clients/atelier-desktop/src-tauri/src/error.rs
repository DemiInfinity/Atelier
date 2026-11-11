//! What a command says when it can't do what was asked: one plain sentence
//! and, when there is one, the next step.

use serde::Serialize;

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct CmdError {
    /// A machine word (`network`, `not_signed_in`, `invalid`, `git`, …).
    pub kind: String,
    /// One plain sentence.
    pub message: String,
    /// What to do about it.
    pub hint: Option<String>,
}

pub type CmdResult<T> = Result<T, CmdError>;

impl CmdError {
    pub fn new(kind: &str, message: impl Into<String>) -> CmdError {
        CmdError {
            kind: kind.to_string(),
            message: message.into(),
            hint: None,
        }
    }

    /// Something about the request itself was wrong.
    pub fn invalid(message: impl Into<String>) -> CmdError {
        CmdError::new("invalid", message)
    }

    pub fn hint(mut self, hint: impl Into<String>) -> CmdError {
        self.hint = Some(hint.into());
        self
    }
}

impl std::fmt::Display for CmdError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.message)
    }
}

impl std::error::Error for CmdError {}

impl From<atelier_core::Error> for CmdError {
    fn from(e: atelier_core::Error) -> Self {
        use atelier_core::Error as E;
        let hint = match &e {
            E::NotSignedIn | E::NoStudio => Some("Connect this machine to your studio first."),
            E::Api { status: 401, .. } => {
                Some("This machine was signed out. Connect it again from Settings.")
            }
            E::Network(_) => Some("Check that this machine can reach your studio."),
            E::MissingProgram(_) => Some("Install it, then try again."),
            E::RunnerNotRunning => Some("Start atelier-runner on this machine."),
            E::DeviceDenied | E::DeviceExpired => Some("Start again to get a fresh code."),
            _ => None,
        };
        CmdError {
            kind: e.kind().to_string(),
            message: tidy(e.to_string()),
            hint: hint.map(str::to_string),
        }
    }
}

/// Ends a message with a full stop when it has none.
fn tidy(message: String) -> String {
    let trimmed = message.trim_end();
    if trimmed.ends_with(['.', '!', '?', '…']) {
        trimmed.to_string()
    } else {
        format!("{trimmed}.")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn core_errors_become_a_sentence_and_a_hint() {
        let e: CmdError = atelier_core::Error::NotSignedIn.into();
        assert_eq!(e.kind, "not_signed_in");
        assert_eq!(e.message, "You’re not signed in.");
        assert!(e.hint.unwrap().contains("Connect"));

        let e: CmdError = atelier_core::Error::Git {
            command: "push".into(),
            message: "rejected".into(),
        }
        .into();
        assert_eq!(e.kind, "git");
        assert_eq!(e.message, "git push failed: rejected.");
        assert_eq!(e.hint, None);
    }
}
