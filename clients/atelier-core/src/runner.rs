//! A client for `atelier-runner`'s local control channel (section 10 of
//! `docs/app-api.md`): a Unix socket next to the runner's config, a named
//! pipe on Windows. One JSON object per line, request then response.
//!
//! Follows "Notes on section 10 as built" in the same document. The runner
//! may run without a control channel at all (for instance when the socket
//! path is too long for the OS), so a missing or refused socket always means
//! [`Error::RunnerNotRunning`], never an I/O error.

use std::path::{Path, PathBuf};
use std::time::Duration;

use serde::{Deserialize, Deserializer, Serialize};
use tokio::io::{AsyncBufReadExt, AsyncRead, AsyncWrite, AsyncWriteExt, BufReader};

use crate::error::{Error, Result};

/// The Windows named pipe the runner listens on.
pub const WINDOWS_PIPE: &str = r"\\.\pipe\atelier-runner";

fn nd<'de, D, T>(d: D) -> std::result::Result<T, D::Error>
where
    D: Deserializer<'de>,
    T: Default + Deserialize<'de>,
{
    Ok(Option::<T>::deserialize(d)?.unwrap_or_default())
}

/// The server's bounds on `max_parallel`.
pub const MAX_PARALLEL_RANGE: std::ops::RangeInclusive<u32> = 1..=64;
/// What `tail` returns when no line count is given, and the most it returns.
pub const TAIL_DEFAULT_LINES: u32 = 50;
pub const TAIL_MAX_LINES: u32 = 500;

/// Why the runner isn't taking jobs (`user` wins over `battery`).
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PausedReason {
    /// Paused by the person.
    User,
    /// On battery, with `pause_on_battery` set.
    Battery,
    /// The runner's jobs capability is off.
    JobsOff,
    #[serde(other)]
    Unknown,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct RunnerRules {
    pub pause_on_battery: bool,
    pub max_parallel: u32,
    pub container_jobs: bool,
}

/// A change to the rules: only what is set is sent.
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
pub struct RunnerRulesUpdate {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub pause_on_battery: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub max_parallel: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub container_jobs: Option<bool>,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct RunnerJob {
    pub job_id: u64,
    #[serde(deserialize_with = "nd")]
    pub repo: String,
    #[serde(deserialize_with = "nd")]
    /// The agent's workflow name, or the workflow file's base name.
    pub workflow: String,
    /// `agent` or `job`.
    #[serde(deserialize_with = "nd")]
    pub kind: String,
    #[serde(deserialize_with = "nd")]
    pub started_at: String,
    #[serde(deserialize_with = "nd")]
    pub detail: String,
}

/// The answer to `status` and `set`.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct RunnerStatus {
    #[serde(deserialize_with = "nd")]
    pub name: String,
    #[serde(deserialize_with = "nd")]
    pub version: String,
    #[serde(deserialize_with = "nd")]
    pub server: String,
    /// False until the runner's first poll of the studio succeeds.
    #[serde(deserialize_with = "nd")]
    pub online: bool,
    /// The effective state: false when paused by the person, by battery, or
    /// when the jobs capability is off.
    #[serde(deserialize_with = "nd")]
    pub taking_jobs: bool,
    /// `None` while taking jobs.
    pub paused_reason: Option<PausedReason>,
    #[serde(deserialize_with = "nd")]
    pub capabilities: Vec<String>,
    #[serde(deserialize_with = "nd")]
    pub rules: RunnerRules,
    #[serde(deserialize_with = "nd")]
    pub current: Vec<RunnerJob>,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Workspace {
    #[serde(deserialize_with = "nd")]
    pub repo: String,
    #[serde(deserialize_with = "nd")]
    pub path: String,
    #[serde(deserialize_with = "nd")]
    pub branch: String,
    #[serde(deserialize_with = "nd")]
    pub dirty: bool,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Workspaces {
    #[serde(deserialize_with = "nd")]
    pub root: String,
    /// Editors and git tools the runner can open (`fork`, `vscode`, `cursor`).
    #[serde(deserialize_with = "nd")]
    pub presets: Vec<String>,
    #[serde(deserialize_with = "nd")]
    pub items: Vec<Workspace>,
}

#[derive(Serialize)]
#[serde(tag = "op", rename_all = "snake_case")]
enum Request<'a> {
    Status,
    Set {
        #[serde(skip_serializing_if = "Option::is_none")]
        taking_jobs: Option<bool>,
        #[serde(skip_serializing_if = "Option::is_none")]
        rules: Option<&'a RunnerRulesUpdate>,
    },
    Tail {
        job_id: u64,
        #[serde(skip_serializing_if = "Option::is_none")]
        lines: Option<u32>,
    },
    Cancel {
        job_id: u64,
    },
    Workspaces,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct Envelope {
    ok: bool,
    error: Option<String>,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct Tail {
    #[serde(deserialize_with = "nd")]
    lines: Vec<String>,
}

/// Talks to the runner on this machine.
#[derive(Clone, Debug)]
pub struct RunnerClient {
    /// The Unix socket path (the pipe name on Windows).
    path: PathBuf,
    timeout: Duration,
}

impl RunnerClient {
    /// A client for the control socket at `path` (see
    /// [`crate::Settings::runner_socket`]).
    pub fn new(path: impl Into<PathBuf>) -> Self {
        RunnerClient {
            path: path.into(),
            timeout: Duration::from_secs(5),
        }
    }

    /// A client for the place the runner listens by default on this OS when
    /// its config file is `runner_config`: `control.sock` next to it, or the
    /// fixed named pipe on Windows.
    pub fn for_config(runner_config: &Path) -> Self {
        if cfg!(windows) {
            return Self::new(WINDOWS_PIPE);
        }
        Self::new(
            runner_config
                .parent()
                .unwrap_or(Path::new("."))
                .join("control.sock"),
        )
    }

    pub fn with_timeout(mut self, timeout: Duration) -> Self {
        self.timeout = timeout;
        self
    }

    pub fn path(&self) -> &Path {
        &self.path
    }

    /// `{"op":"status"}`. [`Error::RunnerNotRunning`] when nothing listens.
    pub async fn status(&self) -> Result<RunnerStatus> {
        self.request(&Request::Status).await
    }

    /// Pause (`false`) or resume (`true`) taking jobs. Persisted by the runner.
    pub async fn set_taking_jobs(&self, taking_jobs: bool) -> Result<RunnerStatus> {
        self.request(&Request::Set {
            taking_jobs: Some(taking_jobs),
            rules: None,
        })
        .await
    }

    /// Change the runner's rules. Persisted by the runner. An update that
    /// changes nothing, or a `max_parallel` outside 1–64, is refused here
    /// before it is sent (the runner refuses both too).
    pub async fn set_rules(&self, rules: &RunnerRulesUpdate) -> Result<RunnerStatus> {
        if *rules == RunnerRulesUpdate::default() {
            return Err(Error::Invalid("There is nothing to change.".into()));
        }
        if let Some(n) = rules.max_parallel {
            if !MAX_PARALLEL_RANGE.contains(&n) {
                return Err(Error::Invalid(format!(
                    "A runner can do 1 to 64 jobs at a time, not {n}."
                )));
            }
        }
        self.request(&Request::Set {
            taking_jobs: None,
            rules: Some(rules),
        })
        .await
    }

    /// The last lines of the output of a job that is running now (the
    /// runner refuses jobs that aren't). `None` asks for the runner's
    /// default of 50; more than 500 is cut to 500, the runner's cap.
    pub async fn tail(&self, job_id: u64, lines: Option<u32>) -> Result<Vec<String>> {
        let lines = lines.map(|n| n.min(TAIL_MAX_LINES));
        let tail: Tail = self.request(&Request::Tail { job_id, lines }).await?;
        Ok(tail.lines)
    }

    pub async fn cancel(&self, job_id: u64) -> Result<()> {
        self.request::<serde::de::IgnoredAny>(&Request::Cancel { job_id })
            .await
            .map(|_| ())
    }

    pub async fn workspaces(&self) -> Result<Workspaces> {
        self.request(&Request::Workspaces).await
    }

    async fn request<T: serde::de::DeserializeOwned>(&self, request: &Request<'_>) -> Result<T> {
        let line = serde_json::to_string(request).map_err(|e| Error::Runner(e.to_string()))?;
        let answer = tokio::time::timeout(self.timeout, self.exchange(line))
            .await
            .map_err(|_| Error::Runner("no answer in time".into()))??;
        let envelope: Envelope = serde_json::from_str(&answer)
            .map_err(|_| Error::Runner("an answer that isn’t JSON".into()))?;
        if !envelope.ok {
            return Err(Error::Runner(
                envelope.error.unwrap_or_else(|| "unknown error".into()),
            ));
        }
        serde_json::from_str(&answer).map_err(|e| Error::Runner(e.to_string()))
    }

    #[cfg(unix)]
    async fn exchange(&self, line: String) -> Result<String> {
        use std::io::ErrorKind;
        match tokio::net::UnixStream::connect(&self.path).await {
            Ok(stream) => round_trip(stream, line).await,
            // No socket, a stale one, or a path too long for a socket (the
            // runner then runs without a control channel): not running, as
            // far as anyone can tell from here.
            Err(e)
                if matches!(
                    e.kind(),
                    ErrorKind::NotFound | ErrorKind::ConnectionRefused | ErrorKind::InvalidInput
                ) =>
            {
                Err(Error::RunnerNotRunning)
            }
            Err(e) => Err(Error::io(
                format!("Couldn’t open {}", self.path.display()),
                e,
            )),
        }
    }

    /// Untested: written against the contract, never run on Windows.
    #[cfg(windows)]
    async fn exchange(&self, line: String) -> Result<String> {
        use std::io::ErrorKind;
        use tokio::net::windows::named_pipe::ClientOptions;
        // ERROR_PIPE_BUSY: every instance is taken; the runner serves one
        // request per connection, so a short wait is enough.
        const PIPE_BUSY: i32 = 231;
        let mut tries = 0;
        let pipe = loop {
            match ClientOptions::new().open(&self.path) {
                Ok(pipe) => break pipe,
                Err(e) if e.raw_os_error() == Some(PIPE_BUSY) && tries < 20 => {
                    tries += 1;
                    tokio::time::sleep(Duration::from_millis(50)).await;
                }
                Err(e) if e.kind() == ErrorKind::NotFound => return Err(Error::RunnerNotRunning),
                Err(e) => {
                    return Err(Error::io(
                        format!("Couldn’t open {}", self.path.display()),
                        e,
                    ))
                }
            }
        };
        round_trip(pipe, line).await
    }

    #[cfg(not(any(unix, windows)))]
    async fn exchange(&self, _line: String) -> Result<String> {
        Err(Error::RunnerNotRunning)
    }
}

/// Writes one line, reads one line.
#[cfg_attr(not(any(unix, windows)), allow(dead_code))]
async fn round_trip<S: AsyncRead + AsyncWrite + Unpin>(
    stream: S,
    mut line: String,
) -> Result<String> {
    let io = |e| Error::io("Couldn’t talk to the runner", e);
    line.push('\n');
    let mut stream = BufReader::new(stream);
    stream
        .get_mut()
        .write_all(line.as_bytes())
        .await
        .map_err(io)?;
    stream.get_mut().flush().await.map_err(io)?;
    let mut answer = String::new();
    let read = stream.read_line(&mut answer).await.map_err(io)?;
    if read == 0 {
        return Err(Error::Runner("it hung up without answering".into()));
    }
    Ok(answer)
}
