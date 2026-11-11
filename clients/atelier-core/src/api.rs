//! Typed requests and responses for `/api/app/v1` (sections 1–9 of
//! `docs/app-api.md`).
//!
//! Every response type tolerates unknown fields, missing fields and `null`
//! where a list or a string is expected, so an older client keeps working
//! against a newer studio. Times are RFC 3339 strings, kept as sent.
//!
//! "Notes on sections 1–9 as built" in the same document is followed too:
//! the fields it adds are optional here (and left out of JSON when absent),
//! and every enum has an `Unknown` fallback instead of failing.

use std::collections::BTreeMap;

use serde::{Deserialize, Deserializer, Serialize};

use crate::token::Token;

/// `null` (as Go sends for an empty list) reads as the default value.
fn nd<'de, D, T>(d: D) -> Result<T, D::Error>
where
    D: Deserializer<'de>,
    T: Default + Deserialize<'de>,
{
    Ok(Option::<T>::deserialize(d)?.unwrap_or_default())
}

// ---------------------------------------------------------------- devices

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DeviceKind {
    Desktop,
    #[default]
    Cli,
    #[serde(other)]
    Unknown,
}

/// Body of `POST /device/start`.
#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct DeviceStartRequest {
    pub name: String,
    pub kind: DeviceKind,
    pub os: String,
    pub version: String,
}

impl DeviceStartRequest {
    /// A request for this machine: its OS and this crate's version.
    pub fn new(name: impl Into<String>, kind: DeviceKind) -> Self {
        DeviceStartRequest {
            name: name.into(),
            kind,
            os: current_os().to_string(),
            version: crate::VERSION.to_string(),
        }
    }
}

/// `macos`, `windows` or `linux` (else whatever Rust calls this OS).
pub fn current_os() -> &'static str {
    std::env::consts::OS
}

/// Answer to `POST /device/start`. `device_code` is a secret.
#[derive(Clone, Default, PartialEq, Deserialize)]
#[serde(default)]
pub struct DeviceStart {
    pub device_code: String,
    pub user_code: String,
    pub verify_url: String,
    /// Seconds until the code expires.
    pub expires_in: u64,
    /// Seconds to wait between polls.
    pub interval: u64,
}

impl std::fmt::Debug for DeviceStart {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("DeviceStart")
            .field("device_code", &"REDACTED")
            .field("user_code", &self.user_code)
            .field("verify_url", &self.verify_url)
            .field("expires_in", &self.expires_in)
            .field("interval", &self.interval)
            .finish()
    }
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Person {
    #[serde(deserialize_with = "nd")]
    pub name: String,
    #[serde(deserialize_with = "nd")]
    pub login: String,
    pub avatar_url: Option<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct SshEndpoint {
    #[serde(deserialize_with = "nd")]
    pub host: String,
    pub port: u16,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Studio {
    #[serde(deserialize_with = "nd")]
    pub name: String,
    #[serde(deserialize_with = "nd")]
    pub url: String,
    #[serde(deserialize_with = "nd")]
    pub version: String,
    pub ssh: Option<SshEndpoint>,
    /// GitHub Tether is set up.
    #[serde(deserialize_with = "nd")]
    pub tether: bool,
    /// Connect (studio-to-studio) is set up.
    #[serde(deserialize_with = "nd")]
    pub connect: bool,
}

/// The `200` answer to `POST /device/poll`: the token, shown once.
#[derive(Clone, Debug, PartialEq, Deserialize)]
pub struct DeviceGrant {
    pub token: Token,
    #[serde(default)]
    pub device_id: u64,
    #[serde(default)]
    pub person: Person,
    #[serde(default)]
    pub studio: Studio,
}

/// One poll of `POST /device/poll`.
#[derive(Clone, Debug, PartialEq)]
pub enum DevicePoll {
    Pending,
    Approved(Box<DeviceGrant>),
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct SshKeyRequest {
    pub title: String,
    pub key: String,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct SshKey {
    pub id: u64,
    #[serde(deserialize_with = "nd")]
    pub fingerprint: String,
    /// True for `201` (new key), false for `200` (the studio had it already).
    #[serde(skip_deserializing)]
    pub created: bool,
}

// --------------------------------------------------------------------- me

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Account {
    #[serde(deserialize_with = "nd")]
    pub name: String,
    #[serde(deserialize_with = "nd")]
    pub display: String,
    /// `user` or `company`.
    #[serde(deserialize_with = "nd")]
    pub kind: String,
    #[serde(deserialize_with = "nd")]
    pub commit_name: String,
    #[serde(deserialize_with = "nd")]
    pub commit_email: String,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Device {
    pub id: u64,
    #[serde(deserialize_with = "nd")]
    pub name: String,
    pub kind: DeviceKind,
    /// The owner lets this device rename and delete repositories (off
    /// unless switched on in Settings → Devices). `None` from a studio
    /// that doesn't say; see [`Device::may_admin`].
    #[serde(skip_serializing_if = "Option::is_none")]
    pub can_admin: Option<bool>,
}

impl Device {
    /// Whether this device may rename and delete repositories: only when
    /// the studio says so.
    pub fn may_admin(&self) -> bool {
        self.can_admin == Some(true)
    }
}

/// Someone a new repository can belong to: the person themselves or one of
/// their companies. Newer studios send these as `me.owners`.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Owner {
    #[serde(deserialize_with = "nd")]
    pub name: String,
    #[serde(deserialize_with = "nd")]
    pub display_name: String,
    /// `personal` or `company`.
    #[serde(deserialize_with = "nd")]
    pub kind: String,
}

/// `GET /me`.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Me {
    pub person: Person,
    #[serde(deserialize_with = "nd")]
    pub accounts: Vec<Account>,
    /// Who the person can create repositories under, when the studio says
    /// so separately from `accounts` (older studios don't).
    #[serde(deserialize_with = "nd", skip_serializing_if = "Vec::is_empty")]
    pub owners: Vec<Owner>,
    pub studio: Studio,
    pub device: Option<Device>,
}

impl Me {
    /// Every owner the studio reports, as accounts: `accounts` first, then
    /// any of `owners` that isn't among them (names compared without
    /// regard to case). The personal account comes first.
    pub fn all_owners(&self) -> Vec<Account> {
        let mut all = self.accounts.clone();
        for owner in &self.owners {
            if owner.name.is_empty() || all.iter().any(|a| a.name.eq_ignore_ascii_case(&owner.name))
            {
                continue;
            }
            let identity = self.accounts.first();
            all.push(Account {
                name: owner.name.clone(),
                display: if owner.display_name.is_empty() {
                    owner.name.clone()
                } else {
                    owner.display_name.clone()
                },
                kind: match owner.kind.as_str() {
                    "personal" | "user" => "user".to_string(),
                    other => other.to_string(),
                },
                commit_name: identity.map(|a| a.commit_name.clone()).unwrap_or_default(),
                commit_email: identity.map(|a| a.commit_email.clone()).unwrap_or_default(),
            });
        }
        all.sort_by_key(|a| a.kind != "user");
        all
    }
}

// ----------------------------------------------------------------- status

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum NeedKind {
    AgentApproval,
    RunFailed,
    PullReady,
    PullReview,
    SecurityAlert,
    PreviewApproval,
    #[default]
    #[serde(other)]
    Unknown,
}

/// Which strand of the studio something belongs to (amber / blue / violet / violet).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Thread {
    #[default]
    Yours,
    Github,
    Agents,
    Connect,
    #[serde(other)]
    Unknown,
}

/// What a client can do about a need. `kind` is the contract's `type`:
/// `approve_agent` (with `job_id`), `open_run` (`run_id`, the global run
/// id), `open_pull` (`number`), or `open_web` (open the need's `web_path`
/// in the browser).
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct NeedAction {
    #[serde(deserialize_with = "nd")]
    pub label: String,
    #[serde(rename = "type", deserialize_with = "nd")]
    pub kind: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub job_id: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub run_id: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub number: Option<u64>,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Need {
    #[serde(deserialize_with = "nd")]
    pub id: String,
    pub kind: NeedKind,
    pub thread: Thread,
    #[serde(deserialize_with = "nd")]
    pub title: String,
    #[serde(deserialize_with = "nd")]
    pub detail: String,
    #[serde(deserialize_with = "nd")]
    pub repo: String,
    #[serde(deserialize_with = "nd")]
    pub web_path: String,
    pub action: Option<NeedAction>,
    /// When it came up (as built; not in the original contract).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub at: Option<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct RunningJob {
    pub job_id: u64,
    pub run_id: u64,
    #[serde(deserialize_with = "nd")]
    pub repo: String,
    #[serde(deserialize_with = "nd")]
    pub workflow: String,
    #[serde(deserialize_with = "nd")]
    pub job: String,
    /// `job` or `agent`.
    #[serde(deserialize_with = "nd")]
    pub kind: String,
    pub engine: Option<String>,
    #[serde(deserialize_with = "nd")]
    pub runner: String,
    #[serde(deserialize_with = "nd")]
    pub started_at: String,
    /// `pull request N` or `<event> on <branch>`. The studio doesn't know
    /// agent turns; the runner's control socket does.
    #[serde(deserialize_with = "nd")]
    pub detail: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub web_path: Option<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct FeedItem {
    pub thread: Thread,
    #[serde(deserialize_with = "nd")]
    pub text: String,
    #[serde(deserialize_with = "nd")]
    pub at: String,
    /// Empty for items from Connect, which carry `url` instead.
    #[serde(deserialize_with = "nd")]
    pub web_path: String,
    /// An absolute address, on items from another studio.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub url: Option<String>,
}

/// `GET /status` ("Today").
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Status {
    #[serde(deserialize_with = "nd")]
    pub needs: Vec<Need>,
    #[serde(deserialize_with = "nd")]
    pub running: Vec<RunningJob>,
    #[serde(deserialize_with = "nd")]
    pub feed: Vec<FeedItem>,
    #[serde(deserialize_with = "nd")]
    pub unread: u64,
}

// ------------------------------------------------------------------ repos

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct GithubLink {
    #[serde(deserialize_with = "nd")]
    pub repo: String,
    /// Which side is home: `atelier` or `github`.
    #[serde(deserialize_with = "nd")]
    pub home: String,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Repo {
    #[serde(deserialize_with = "nd")]
    pub full_name: String,
    #[serde(deserialize_with = "nd")]
    pub owner: String,
    #[serde(deserialize_with = "nd")]
    pub owner_kind: String,
    #[serde(deserialize_with = "nd")]
    pub name: String,
    #[serde(deserialize_with = "nd")]
    pub private: bool,
    #[serde(deserialize_with = "nd")]
    pub description: String,
    #[serde(deserialize_with = "nd")]
    pub default_branch: String,
    pub language: Option<String>,
    pub language_color: Option<String>,
    #[serde(deserialize_with = "nd")]
    pub clone_https: String,
    #[serde(deserialize_with = "nd")]
    pub clone_ssh: String,
    /// `None` when the repository isn't tethered to GitHub.
    pub github: Option<GithubLink>,
    #[serde(deserialize_with = "nd")]
    pub open_pulls: u64,
    #[serde(deserialize_with = "nd")]
    pub updated_at: String,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct GithubHit {
    #[serde(deserialize_with = "nd")]
    pub full_name: String,
    #[serde(deserialize_with = "nd")]
    pub description: String,
    /// The GitHub repository carries the Atelier badge.
    #[serde(deserialize_with = "nd")]
    pub atelier_badge: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub web_path: Option<String>,
}

/// `GET /repos` (`github_hits` only with `github=1`).
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct RepoList {
    #[serde(deserialize_with = "nd")]
    pub repos: Vec<Repo>,
    #[serde(deserialize_with = "nd")]
    pub github_hits: Vec<GithubHit>,
    /// Why there are no GitHub hits (for instance, Tether isn't set up).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub github_error: Option<String>,
}

/// Body of `POST /repos`.
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
pub struct CreateRepo {
    pub owner: String,
    pub name: String,
    pub private: bool,
    pub description: String,
}

/// Body of `PATCH /repos/{owner}/{name}`.
#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct RenameRepo {
    pub name: String,
}

/// Body of `DELETE /repos/{owner}/{name}`: the full name, typed back.
#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct DeleteRepo {
    pub confirm: String,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct MirrorRequest {
    pub github: String,
}

/// `202` answer to `POST /repos/mirror`.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Mirrored {
    #[serde(deserialize_with = "nd")]
    pub full_name: String,
}

// ------------------------------------------------------------------ pulls

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PullState {
    #[default]
    Open,
    Closed,
    Merged,
    /// Only meaningful as a filter.
    All,
    #[serde(other)]
    Unknown,
}

impl PullState {
    pub fn as_str(self) -> &'static str {
        match self {
            PullState::Open => "open",
            PullState::Closed => "closed",
            PullState::Merged => "merged",
            PullState::All => "all",
            PullState::Unknown => "unknown",
        }
    }
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CheckStatus {
    Success,
    Failure,
    #[default]
    Pending,
    Waiting,
    Skipped,
    #[serde(other)]
    Unknown,
}

/// How something arrived: through Tether (GitHub), Connect, or a bot.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Via {
    Tether,
    Connect,
    Bot,
    #[serde(other)]
    Unknown,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MergeMethod {
    #[default]
    Squash,
    Merge,
    Rebase,
    #[serde(other)]
    Unknown,
}

impl MergeMethod {
    pub fn as_str(self) -> &'static str {
        match self {
            MergeMethod::Squash => "squash",
            MergeMethod::Merge => "merge",
            MergeMethod::Rebase => "rebase",
            MergeMethod::Unknown => "unknown",
        }
    }
}

/// A merge waiting for green checks.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct AutoMerge {
    pub method: MergeMethod,
    #[serde(deserialize_with = "nd")]
    pub by: String,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct MergeInfo {
    #[serde(deserialize_with = "nd")]
    pub can_merge: bool,
    #[serde(deserialize_with = "nd")]
    pub reason: String,
    #[serde(deserialize_with = "nd")]
    pub methods: Vec<MergeMethod>,
    /// Merging happens on GitHub ("Merge on GitHub").
    #[serde(deserialize_with = "nd")]
    pub on_github: bool,
    pub auto_merge: Option<AutoMerge>,
    /// The when-green condition holds: every check has passed.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub green: Option<bool>,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Check {
    #[serde(deserialize_with = "nd")]
    pub name: String,
    pub status: CheckStatus,
    #[serde(deserialize_with = "nd")]
    pub detail: String,
    /// `None` for checks that bots report.
    pub run_id: Option<u64>,
    pub job_id: Option<u64>,
    /// `approve_agent` when the check waits for an agent approval.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub action: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub required: Option<bool>,
}

/// The one-word state of a pull request's checks.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChecksState {
    /// There are no checks.
    None,
    Pending,
    Failing,
    Passing,
    #[serde(other)]
    Unknown,
}

/// The reduced `checks` of a pull summary. `total` and `state` come from
/// newer studios; [`CheckCounts::count`] and [`CheckCounts::summary`] work
/// them out from the counts when they are missing.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct CheckCounts {
    pub passed: u64,
    pub failed: u64,
    pub pending: u64,
    pub waiting: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub total: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub state: Option<ChecksState>,
}

impl CheckCounts {
    /// How many checks there are.
    pub fn count(&self) -> u64 {
        self.total
            .unwrap_or(self.passed + self.failed + self.pending + self.waiting)
    }

    /// The state the studio sent, or the one the counts add up to.
    pub fn summary(&self) -> ChecksState {
        match self.state {
            Some(state) if state != ChecksState::Unknown => state,
            _ if self.failed > 0 => ChecksState::Failing,
            _ if self.pending + self.waiting > 0 => ChecksState::Pending,
            _ if self.count() == 0 => ChecksState::None,
            _ => ChecksState::Passing,
        }
    }
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct PullComment {
    #[serde(deserialize_with = "nd")]
    pub author: String,
    pub via: Option<Via>,
    /// `person`, `bot` or `agent`.
    #[serde(deserialize_with = "nd")]
    pub kind: String,
    #[serde(deserialize_with = "nd")]
    pub body: String,
    #[serde(deserialize_with = "nd")]
    pub at: String,
}

/// A pull request in a list: no comments, checks as counts.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct PullSummary {
    #[serde(deserialize_with = "nd")]
    pub repo: String,
    pub number: u64,
    #[serde(deserialize_with = "nd")]
    pub title: String,
    pub state: PullState,
    #[serde(deserialize_with = "nd")]
    pub head: String,
    #[serde(deserialize_with = "nd")]
    pub base: String,
    #[serde(deserialize_with = "nd")]
    pub author: String,
    #[serde(deserialize_with = "nd")]
    pub commits: u64,
    #[serde(deserialize_with = "nd")]
    pub files: u64,
    pub via: Option<Via>,
    pub merge: MergeInfo,
    #[serde(deserialize_with = "nd")]
    pub checks: CheckCounts,
    #[serde(deserialize_with = "nd")]
    pub web_path: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub updated_at: Option<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct PullList {
    #[serde(deserialize_with = "nd")]
    pub pulls: Vec<PullSummary>,
}

/// `GET /repos/{owner}/{repo}/pulls/{n}`.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Pull {
    #[serde(deserialize_with = "nd")]
    pub repo: String,
    pub number: u64,
    #[serde(deserialize_with = "nd")]
    pub title: String,
    pub state: PullState,
    #[serde(deserialize_with = "nd")]
    pub head: String,
    #[serde(deserialize_with = "nd")]
    pub base: String,
    #[serde(deserialize_with = "nd")]
    pub author: String,
    #[serde(deserialize_with = "nd")]
    pub commits: u64,
    #[serde(deserialize_with = "nd")]
    pub files: u64,
    pub via: Option<Via>,
    pub merge: MergeInfo,
    #[serde(deserialize_with = "nd")]
    pub checks: Vec<Check>,
    #[serde(deserialize_with = "nd")]
    pub comments: Vec<PullComment>,
    #[serde(deserialize_with = "nd")]
    pub web_path: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub updated_at: Option<String>,
}

/// Body of `POST …/pulls/{n}/merge`.
#[derive(Clone, Copy, Debug, Default, PartialEq, Serialize)]
pub struct MergeRequest {
    pub method: MergeMethod,
    /// Always answered with `202`; the studio waits for every check.
    pub when_green: bool,
    /// Delete the head branch once merged (the studio's default when `None`).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub delete_branch: Option<bool>,
}

/// What `POST …/merge` did.
#[derive(Clone, Debug, PartialEq, Serialize)]
#[serde(untagged)]
pub enum MergeOutcome {
    /// `200`: merged now.
    Merged { merged: bool, sha: String },
    /// `202`: will merge when every check passes.
    Scheduled { auto_merge: AutoMerge },
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub(crate) struct MergeResponse {
    #[serde(deserialize_with = "nd")]
    pub merged: bool,
    #[serde(deserialize_with = "nd")]
    pub sha: String,
    pub auto_merge: Option<AutoMerge>,
}

// ----------------------------------------------------------------- issues

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct Label {
    #[serde(deserialize_with = "nd")]
    pub name: String,
    /// A hex colour (`#d97706` or `d97706`), when the studio sends one.
    #[serde(deserialize_with = "nd")]
    pub color: String,
}

/// An issue in a list: `comments` is how many there are.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct IssueSummary {
    #[serde(deserialize_with = "nd")]
    pub repo: String,
    pub number: u64,
    #[serde(deserialize_with = "nd")]
    pub title: String,
    /// `open` or `closed`.
    #[serde(deserialize_with = "nd")]
    pub state: String,
    /// `<kind>:<name>`, like a pull request's.
    #[serde(deserialize_with = "nd")]
    pub author: String,
    pub via: Option<Via>,
    #[serde(deserialize_with = "nd")]
    pub labels: Vec<Label>,
    #[serde(deserialize_with = "nd")]
    pub comments: u64,
    #[serde(deserialize_with = "nd")]
    pub created_at: String,
    #[serde(deserialize_with = "nd")]
    pub updated_at: String,
    #[serde(deserialize_with = "nd")]
    pub web_path: String,
    /// The issue's page in the studio, as an absolute address.
    #[serde(deserialize_with = "nd")]
    pub web_url: String,
}

/// `GET /issues`.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct IssueList {
    #[serde(deserialize_with = "nd")]
    pub issues: Vec<IssueSummary>,
    #[serde(deserialize_with = "nd")]
    pub has_more: bool,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct IssueComment {
    #[serde(deserialize_with = "nd")]
    pub id: u64,
    /// A bare name, as in pull request comments.
    #[serde(deserialize_with = "nd")]
    pub author: String,
    pub via: Option<Via>,
    /// `person`, `bot` or `agent`.
    #[serde(deserialize_with = "nd")]
    pub kind: String,
    #[serde(deserialize_with = "nd")]
    pub body: String,
    #[serde(deserialize_with = "nd")]
    pub created_at: String,
}

/// A count where a list was expected (or nothing at all) reads as no comments.
fn comment_list<'de, D: Deserializer<'de>>(d: D) -> Result<Vec<IssueComment>, D::Error> {
    #[derive(Deserialize)]
    #[serde(untagged)]
    enum Either {
        List(Vec<IssueComment>),
        Other(serde::de::IgnoredAny),
    }
    Ok(match Either::deserialize(d)? {
        Either::List(list) => list,
        Either::Other(_) => Vec::new(),
    })
}

/// `GET /repos/{owner}/{name}/issues/{n}`: the issue with its body and
/// conversation.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Issue {
    #[serde(deserialize_with = "nd")]
    pub repo: String,
    pub number: u64,
    #[serde(deserialize_with = "nd")]
    pub title: String,
    #[serde(deserialize_with = "nd")]
    pub state: String,
    #[serde(deserialize_with = "nd")]
    pub author: String,
    pub via: Option<Via>,
    #[serde(deserialize_with = "nd")]
    pub labels: Vec<Label>,
    /// Markdown source.
    #[serde(deserialize_with = "nd")]
    pub body: String,
    /// Oldest first.
    #[serde(deserialize_with = "comment_list")]
    pub comments: Vec<IssueComment>,
    #[serde(deserialize_with = "nd")]
    pub created_at: String,
    #[serde(deserialize_with = "nd")]
    pub updated_at: String,
    #[serde(deserialize_with = "nd")]
    pub web_path: String,
    #[serde(deserialize_with = "nd")]
    pub web_url: String,
}

/// Filters for `GET /issues`.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct IssuesQuery {
    /// `owner/name`.
    pub repo: String,
    /// `open`, `closed` or `all` (the studio's default when empty).
    pub state: String,
    pub page: Option<u32>,
    pub per_page: Option<u32>,
}

/// Body of `POST /repos/{owner}/{name}/issues`.
#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct NewIssue {
    pub title: String,
    pub body: String,
}

// -------------------------------------------------------- runs and agents

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct RunJob {
    pub job_id: u64,
    #[serde(deserialize_with = "nd")]
    pub name: String,
    /// `success | failure | cancelled | queued | running | waiting | pending | skipped`.
    #[serde(deserialize_with = "nd")]
    pub status: String,
    #[serde(deserialize_with = "nd")]
    pub runner: String,
    pub duration_s: Option<u64>,
    /// `job` or `agent`.
    #[serde(deserialize_with = "nd")]
    pub kind: String,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Run {
    pub run_id: u64,
    #[serde(deserialize_with = "nd")]
    pub repo: String,
    #[serde(deserialize_with = "nd")]
    pub workflow: String,
    #[serde(deserialize_with = "nd")]
    pub event: String,
    #[serde(deserialize_with = "nd")]
    pub branch: String,
    #[serde(deserialize_with = "nd")]
    pub status: String,
    #[serde(deserialize_with = "nd")]
    pub started_at: String,
    pub duration_s: Option<u64>,
    #[serde(deserialize_with = "nd")]
    pub jobs: Vec<RunJob>,
    /// The run's number within its repository (`run_id` is global).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub number: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub web_path: Option<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct RunList {
    #[serde(deserialize_with = "nd")]
    pub runs: Vec<Run>,
}

/// Filters for `GET /runs`.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct RunsQuery {
    pub limit: Option<u32>,
    pub runner: Option<String>,
    /// `owner/name`.
    pub repo: Option<String>,
}

/// `GET /jobs/{id}/log`.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct JobLog {
    #[serde(deserialize_with = "nd")]
    pub lines: Vec<String>,
    #[serde(deserialize_with = "nd")]
    pub truncated: bool,
    #[serde(deserialize_with = "nd")]
    pub status: String,
}

/// An agent job waiting for the owner's approval.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct WaitingAgent {
    pub job_id: u64,
    pub run_id: u64,
    #[serde(deserialize_with = "nd")]
    pub repo: String,
    #[serde(deserialize_with = "nd")]
    pub workflow: String,
    #[serde(deserialize_with = "nd")]
    pub reason: String,
    #[serde(deserialize_with = "nd")]
    pub engine: String,
    #[serde(deserialize_with = "nd")]
    pub tools: Vec<String>,
    #[serde(deserialize_with = "nd")]
    pub write_tools: Vec<String>,
    pub max_turns: Option<u64>,
    pub timeout_minutes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub detail: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub web_path: Option<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct WaitingAgents {
    #[serde(deserialize_with = "nd")]
    pub jobs: Vec<WaitingAgent>,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Approved {
    pub approved: bool,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Cancelled {
    pub cancelled: bool,
}

// ----------------------------------------------------------------- remote

/// `GET`/`POST /remote`.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Remote {
    #[serde(deserialize_with = "nd")]
    pub on: bool,
    #[serde(deserialize_with = "nd")]
    pub mode: String,
    /// `None` while off, and for a few seconds after turning on.
    pub address: Option<String>,
    #[serde(deserialize_with = "nd")]
    pub exposed: Vec<String>,
    #[serde(deserialize_with = "nd")]
    pub connected: u64,
}

#[derive(Clone, Copy, Debug, PartialEq, Serialize)]
pub struct SetRemote {
    pub on: bool,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct RemoteSession {
    #[serde(deserialize_with = "nd")]
    pub ip: String,
    #[serde(deserialize_with = "nd")]
    pub country: String,
    #[serde(deserialize_with = "nd")]
    pub signed_in: bool,
    /// `web` or `git`.
    #[serde(deserialize_with = "nd")]
    pub class: String,
    #[serde(deserialize_with = "nd")]
    pub since: String,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct RemoteWho {
    #[serde(deserialize_with = "nd")]
    pub sessions: Vec<RemoteSession>,
}

/// A one-time passkey link for the remote address.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct RemoteLink {
    #[serde(deserialize_with = "nd")]
    pub url: String,
    pub expires_in: u64,
}

// ----------------------------------------------------------------- events

/// The pop-up "moment" of a notification.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Moment {
    Push,
    Pass,
    Fail,
    Merge,
    Agent,
    Github,
    Security,
    #[serde(other)]
    Unknown,
}

impl Moment {
    /// The key into [`Messages::moments`], when there is one.
    pub fn message_key(self) -> Option<&'static str> {
        match self {
            Moment::Push => Some("push"),
            Moment::Pass => Some("pass"),
            Moment::Fail => Some("fail"),
            Moment::Merge => Some("merge"),
            Moment::Agent => Some("agent"),
            Moment::Github | Moment::Security | Moment::Unknown => None,
        }
    }
}

/// `event: notify`.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Notify {
    #[serde(deserialize_with = "nd")]
    pub id: String,
    /// An open set (`push`, `run_passed`, `run_failed`, `run_cancelled`,
    /// `agent_finished`, `agent_approval`, `job_waiting`, `pull_merged`,
    /// `pull_review`, `security_alert`, `auto_merge_cancelled`, or an event
    /// name); treat kinds you don't know as a plain notification.
    #[serde(deserialize_with = "nd")]
    pub kind: String,
    pub thread: Thread,
    #[serde(deserialize_with = "nd")]
    pub title: String,
    #[serde(deserialize_with = "nd")]
    pub detail: String,
    #[serde(deserialize_with = "nd")]
    pub repo: String,
    #[serde(deserialize_with = "nd")]
    pub web_path: String,
    pub moment: Option<Moment>,
    #[serde(deserialize_with = "nd")]
    pub at: String,
}

/// `event: status`: which parts of `GET /status` changed.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct StatusChanged {
    #[serde(deserialize_with = "nd")]
    pub changed: Vec<String>,
}

/// `event: remote`.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct RemoteChanged {
    #[serde(deserialize_with = "nd")]
    pub on: bool,
    pub address: Option<String>,
    #[serde(deserialize_with = "nd")]
    pub connected: u64,
}

// --------------------------------------------------------------- messages

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Tone {
    #[default]
    Friendly,
    Plain,
    Off,
    /// A tone this version doesn't know; treated as plain.
    #[serde(other)]
    Unknown,
}

impl Tone {
    pub fn as_str(self) -> &'static str {
        match self {
            Tone::Friendly => "friendly",
            Tone::Plain | Tone::Unknown => "plain",
            Tone::Off => "off",
        }
    }
}

/// The messages for one moment: the studio's (`ours`) and the person's (`yours`).
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct MomentMessages {
    #[serde(deserialize_with = "nd")]
    pub ours: Vec<String>,
    #[serde(deserialize_with = "nd")]
    pub yours: Vec<String>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct MessageRules {
    /// Hold pop-ups while something is full screen.
    pub quiet_fullscreen: bool,
    /// Say things in the terminal too.
    pub terminal: bool,
    pub milestones: bool,
}

impl Default for MessageRules {
    fn default() -> Self {
        MessageRules {
            quiet_fullscreen: true,
            terminal: true,
            milestones: true,
        }
    }
}

/// `GET /messages`.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct Messages {
    pub tone: Tone,
    #[serde(deserialize_with = "nd")]
    pub moments: BTreeMap<String, MomentMessages>,
    #[serde(deserialize_with = "nd")]
    pub plain: BTreeMap<String, String>,
    #[serde(deserialize_with = "nd")]
    pub rules: MessageRules,
}

/// Body of `PUT /messages`. Only what is set is sent; `yours` lists replace
/// per moment.
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
pub struct MessagesUpdate {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tone: Option<Tone>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub yours: Option<BTreeMap<String, Vec<String>>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub rules: Option<MessageRules>,
}

/// The most lines `GET /jobs/{id}/log?tail=` returns.
pub const MAX_LOG_TAIL: u32 = 2000;

/// The server's limits on `yours` messages.
pub const MAX_YOURS_PER_MOMENT: usize = 20;
pub const MAX_MESSAGE_CHARS: usize = 140;
