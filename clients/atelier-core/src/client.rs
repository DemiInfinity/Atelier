//! The HTTP client for `/api/app/v1`.

use std::path::PathBuf;
use std::time::Duration;

use reqwest::{Method, StatusCode, Url};
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};

use crate::api::*;
use crate::config::{normalize_studio, Settings};
use crate::error::{Error, Result};
use crate::token::{Token, TokenStore};

const API_PREFIX: [&str; 3] = ["api", "app", "v1"];

/// How to build a [`Client`].
#[derive(Clone, Debug)]
pub struct ClientOptions {
    /// PEM bundles to trust on top of the system roots.
    pub ca_files: Vec<PathBuf>,
    /// For a whole request, start to finish.
    pub timeout: Duration,
    pub connect_timeout: Duration,
    pub user_agent: String,
}

impl Default for ClientOptions {
    fn default() -> Self {
        ClientOptions {
            ca_files: Vec::new(),
            timeout: Duration::from_secs(30),
            connect_timeout: Duration::from_secs(10),
            user_agent: format!("atelier-core/{}", crate::VERSION),
        }
    }
}

/// How [`Client::wait_for_device`] polls.
#[derive(Clone, Copy, Debug)]
pub struct PollOptions {
    /// Never poll faster than this, whatever `interval` the studio sends.
    pub min_interval: Duration,
}

impl Default for PollOptions {
    fn default() -> Self {
        PollOptions {
            min_interval: Duration::from_secs(1),
        }
    }
}

/// A studio's app API. Cheap to clone.
#[derive(Clone)]
pub struct Client {
    http: reqwest::Client,
    /// For long-lived streams: no overall timeout.
    stream_http: reqwest::Client,
    base: Url,
    token: Option<Token>,
}

impl std::fmt::Debug for Client {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Client")
            .field("studio", &self.studio_url())
            .field("token", &self.token)
            .finish()
    }
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct ErrorBody {
    error: Option<String>,
    message: Option<String>,
}

impl Client {
    /// A client for `studio` (`https://studio.example` or just the host).
    /// Without a token only the sign-in endpoints work.
    pub fn new(studio: &str, token: Option<Token>, options: ClientOptions) -> Result<Client> {
        let studio = normalize_studio(studio);
        let base = Url::parse(&studio)
            .ok()
            .filter(|u| matches!(u.scheme(), "http" | "https") && u.has_host())
            .ok_or_else(|| Error::Config(format!("“{studio}” isn’t a studio address.")))?;

        let mut certs = Vec::new();
        for file in &options.ca_files {
            let pem = std::fs::read(file).map_err(|e| {
                Error::io(format!("Couldn’t read the CA file {}", file.display()), e)
            })?;
            let found = reqwest::Certificate::from_pem_bundle(&pem)
                .ok()
                .filter(|c| !c.is_empty())
                .ok_or_else(|| {
                    Error::Config(format!(
                        "The CA file {} has no PEM certificates in it.",
                        file.display()
                    ))
                })?;
            certs.extend(found);
        }

        let builder = || {
            reqwest::Client::builder()
                .user_agent(options.user_agent.clone())
                .connect_timeout(options.connect_timeout)
                .tls_certs_merge(certs.clone())
        };
        let http = builder()
            .timeout(options.timeout)
            .build()
            .map_err(network_error)?;
        let stream_http = builder().build().map_err(network_error)?;
        Ok(Client {
            http,
            stream_http,
            base,
            token,
        })
    }

    /// The client these settings describe: studio from `ATELIER_STUDIO` or the
    /// config, token from `ATELIER_TOKEN` or `store`, CA roots from the config
    /// and `SSL_CERT_FILE`. Fails with [`Error::NoStudio`] / [`Error::NotSignedIn`].
    pub fn from_settings(settings: &Settings, store: &dyn TokenStore) -> Result<Client> {
        let studio = settings.studio().ok_or(Error::NoStudio)?;
        let token = match settings.token_override() {
            Some(token) => token,
            None => store.get(&studio)?.ok_or(Error::NotSignedIn)?,
        };
        let options = ClientOptions {
            ca_files: settings.ca_files(),
            ..ClientOptions::default()
        };
        Client::new(&studio, Some(token), options)
    }

    /// The same client with a (new) token.
    pub fn with_token(mut self, token: Token) -> Client {
        self.token = Some(token);
        self
    }

    /// The studio's base URL, without a trailing slash.
    pub fn studio_url(&self) -> String {
        self.base.as_str().trim_end_matches('/').to_string()
    }

    /// The studio's host name, for display.
    pub fn studio_host(&self) -> String {
        self.base.host_str().unwrap_or_default().to_string()
    }

    /// A full URL for a `web_path` from the API (`/Owner/Repo/pulls/14`).
    pub fn web_url(&self, web_path: &str) -> String {
        format!("{}/{}", self.studio_url(), web_path.trim_start_matches('/'))
    }

    pub(crate) fn api_url(&self, segments: &[&str]) -> Url {
        let mut url = self.base.clone();
        if let Ok(mut path) = url.path_segments_mut() {
            path.pop_if_empty().extend(API_PREFIX).extend(segments);
        }
        url
    }

    pub(crate) fn stream_request(&self, url: Url) -> Result<reqwest::RequestBuilder> {
        let token = self.token.as_ref().ok_or(Error::NotSignedIn)?;
        Ok(self.stream_http.get(url).bearer_auth(token.expose()))
    }

    async fn call<B: Serialize + ?Sized>(
        &self,
        method: Method,
        segments: &[&str],
        query: &[(&str, String)],
        body: Option<&B>,
        auth: bool,
    ) -> Result<(StatusCode, Vec<u8>)> {
        let mut request = self.http.request(method, self.api_url(segments));
        if !query.is_empty() {
            request = request.query(query);
        }
        if auth {
            let token = self.token.as_ref().ok_or(Error::NotSignedIn)?;
            request = request.bearer_auth(token.expose());
        }
        if let Some(body) = body {
            request = request.json(body);
        }
        let response = request
            .header(reqwest::header::ACCEPT, "application/json")
            .send()
            .await
            .map_err(network_error)?;
        let status = response.status();
        let bytes = response.bytes().await.map_err(network_error)?.to_vec();
        if status.is_success() {
            Ok((status, bytes))
        } else {
            Err(api_error(status, &bytes))
        }
    }

    async fn get<T: DeserializeOwned>(
        &self,
        segments: &[&str],
        query: &[(&str, String)],
    ) -> Result<T> {
        let (_, bytes) = self
            .call::<()>(Method::GET, segments, query, None, true)
            .await?;
        decode(&bytes)
    }

    async fn post<B: Serialize + ?Sized, T: DeserializeOwned>(
        &self,
        segments: &[&str],
        body: Option<&B>,
    ) -> Result<(StatusCode, T)> {
        let (status, bytes) = self.call(Method::POST, segments, &[], body, true).await?;
        Ok((status, decode(&bytes)?))
    }

    // ------------------------------------------------------------ devices

    /// `POST /device/start`. No token needed.
    pub async fn device_start(&self, request: &DeviceStartRequest) -> Result<DeviceStart> {
        let (_, bytes) = self
            .call(
                Method::POST,
                &["device", "start"],
                &[],
                Some(request),
                false,
            )
            .await?;
        decode(&bytes)
    }

    /// One `POST /device/poll`. `410` becomes [`Error::DeviceDenied`] or
    /// [`Error::DeviceExpired`].
    pub async fn device_poll(&self, device_code: &str) -> Result<DevicePoll> {
        #[derive(Serialize)]
        struct Body<'a> {
            device_code: &'a str,
        }
        let result = self
            .call(
                Method::POST,
                &["device", "poll"],
                &[],
                Some(&Body { device_code }),
                false,
            )
            .await;
        match result {
            Ok((StatusCode::ACCEPTED, _)) => Ok(DevicePoll::Pending),
            Ok((_, bytes)) => Ok(DevicePoll::Approved(Box::new(decode(&bytes)?))),
            Err(Error::Api {
                status: 410, code, ..
            }) if code == "denied" => Err(Error::DeviceDenied),
            Err(Error::Api { status: 410, .. }) => Err(Error::DeviceExpired),
            Err(e) => Err(e),
        }
    }

    /// Polls until the owner decides in the browser, waiting `interval`
    /// seconds between polls and giving up after `expires_in`. Network
    /// hiccups while waiting are ridden out.
    pub async fn wait_for_device(
        &self,
        start: &DeviceStart,
        options: PollOptions,
    ) -> Result<DeviceGrant> {
        let interval = Duration::from_secs(start.interval).max(options.min_interval);
        let deadline = tokio::time::Instant::now() + Duration::from_secs(start.expires_in);
        loop {
            if tokio::time::Instant::now() + interval > deadline {
                return Err(Error::DeviceExpired);
            }
            tokio::time::sleep(interval).await;
            match self.device_poll(&start.device_code).await {
                Ok(DevicePoll::Approved(grant)) => return Ok(*grant),
                Ok(DevicePoll::Pending) | Err(Error::Network(_)) => {}
                Err(e) => return Err(e),
            }
        }
    }

    /// `POST /device/ssh-key`: registers this device's public key.
    pub async fn register_ssh_key(&self, title: &str, key: &str) -> Result<SshKey> {
        let body = SshKeyRequest {
            title: title.to_string(),
            key: key.trim().to_string(),
        };
        let (status, mut key): (_, SshKey) = self.post(&["device", "ssh-key"], Some(&body)).await?;
        key.created = status == StatusCode::CREATED;
        Ok(key)
    }

    // ------------------------------------------------------ who and where

    /// `GET /me`.
    pub async fn me(&self) -> Result<Me> {
        self.get(&["me"], &[]).await
    }

    /// `GET /status`.
    pub async fn status(&self) -> Result<Status> {
        self.get(&["status"], &[]).await
    }

    // -------------------------------------------------------------- repos

    /// `GET /repos`: every repository the caller can read.
    pub async fn repos(&self) -> Result<Vec<Repo>> {
        Ok(self.get::<RepoList>(&["repos"], &[]).await?.repos)
    }

    /// `GET /repos?q=…[&github=1]`: filter by name, optionally with GitHub hits.
    pub async fn search_repos(&self, q: &str, github: bool) -> Result<RepoList> {
        let mut query = vec![("q", q.to_string())];
        if github {
            query.push(("github", "1".to_string()));
        }
        self.get(&["repos"], &query).await
    }

    /// `POST /repos`. `409 name_taken` and `403` come back as [`Error::Api`].
    pub async fn create_repo(&self, request: &CreateRepo) -> Result<Repo> {
        Ok(self.post(&["repos"], Some(request)).await?.1)
    }

    /// `PATCH /repos/{owner}/{name}`: gives the repository a new name and
    /// returns it as it is now. `409 name_taken`, `422` (not a name) and
    /// `403` come back as [`Error::Api`].
    pub async fn rename_repo(&self, owner: &str, name: &str, new_name: &str) -> Result<Repo> {
        let body = RenameRepo {
            name: new_name.to_string(),
        };
        let (_, bytes) = self
            .call(
                Method::PATCH,
                &["repos", owner, name],
                &[],
                Some(&body),
                true,
            )
            .await?;
        decode(&bytes)
    }

    /// `DELETE /repos/{owner}/{name}`: removes the repository from the
    /// studio. The body repeats `owner/name`, as the studio insists.
    pub async fn delete_repo(&self, owner: &str, name: &str) -> Result<()> {
        let body = DeleteRepo {
            confirm: format!("{owner}/{name}"),
        };
        self.call(
            Method::DELETE,
            &["repos", owner, name],
            &[],
            Some(&body),
            true,
        )
        .await
        .map(|_| ())
    }

    /// `POST /repos/mirror`: mirror a GitHub repository (`owner/name`) into the studio.
    pub async fn mirror_repo(&self, github: &str) -> Result<Mirrored> {
        let body = MirrorRequest {
            github: github.to_string(),
        };
        Ok(self.post(&["repos", "mirror"], Some(&body)).await?.1)
    }

    // -------------------------------------------------------------- pulls

    /// `GET /pulls?state=open`: open pull requests across every repository.
    pub async fn pulls(&self) -> Result<Vec<PullSummary>> {
        let query = [("state", "open".to_string())];
        Ok(self.get::<PullList>(&["pulls"], &query).await?.pulls)
    }

    /// `GET /pulls?state=open&repo=owner/name`: one repository's open pull
    /// requests. A studio that doesn't know the `repo` filter answers with
    /// all of them, so the answer is narrowed here as well.
    pub async fn pulls_in(&self, full_name: &str) -> Result<Vec<PullSummary>> {
        let query = [
            ("state", "open".to_string()),
            ("repo", full_name.to_string()),
        ];
        let mut pulls = self.get::<PullList>(&["pulls"], &query).await?.pulls;
        pulls.retain(|p| p.repo.eq_ignore_ascii_case(full_name));
        Ok(pulls)
    }

    /// `GET /repos/{owner}/{repo}/pulls?state=…`.
    pub async fn repo_pulls(
        &self,
        owner: &str,
        repo: &str,
        state: PullState,
    ) -> Result<Vec<PullSummary>> {
        let query = [("state", state.as_str().to_string())];
        let list: PullList = self.get(&["repos", owner, repo, "pulls"], &query).await?;
        Ok(list.pulls)
    }

    /// `GET /repos/{owner}/{repo}/pulls/{n}`.
    pub async fn pull(&self, owner: &str, repo: &str, number: u64) -> Result<Pull> {
        let n = number.to_string();
        self.get(&["repos", owner, repo, "pulls", &n], &[]).await
    }

    /// `POST …/pulls/{n}/merge`. Not mergeable is `Error::Api { status: 409, .. }`
    /// with the reason as its message.
    pub async fn merge_pull(
        &self,
        owner: &str,
        repo: &str,
        number: u64,
        request: MergeRequest,
    ) -> Result<MergeOutcome> {
        let n = number.to_string();
        let (_, response): (_, MergeResponse) = self
            .post(
                &["repos", owner, repo, "pulls", &n, "merge"],
                Some(&request),
            )
            .await?;
        Ok(match response.auto_merge {
            Some(auto_merge) if !response.merged => MergeOutcome::Scheduled { auto_merge },
            _ => MergeOutcome::Merged {
                merged: response.merged,
                sha: response.sha,
            },
        })
    }

    /// `DELETE …/pulls/{n}/merge`: cancel a merge that waits for green
    /// checks. When none was waiting the studio answers `404 no_auto_merge`,
    /// which comes back as [`Error::Api`] with that code.
    pub async fn cancel_auto_merge(&self, owner: &str, repo: &str, number: u64) -> Result<()> {
        let n = number.to_string();
        self.call::<()>(
            Method::DELETE,
            &["repos", owner, repo, "pulls", &n, "merge"],
            &[],
            None,
            true,
        )
        .await
        .map(|_| ())
    }

    // ------------------------------------------------------------- issues

    /// `GET /issues?repo=owner/name&state=…&page=…&per_page=…`.
    pub async fn issues(&self, query: &IssuesQuery) -> Result<IssueList> {
        let mut q = vec![("repo", query.repo.clone())];
        if !query.state.is_empty() {
            q.push(("state", query.state.clone()));
        }
        if let Some(page) = query.page {
            q.push(("page", page.to_string()));
        }
        if let Some(per_page) = query.per_page {
            q.push(("per_page", per_page.to_string()));
        }
        self.get(&["issues"], &q).await
    }

    /// `GET /repos/{owner}/{name}/issues/{n}`.
    pub async fn issue(&self, owner: &str, repo: &str, number: u64) -> Result<Issue> {
        let n = number.to_string();
        self.get(&["repos", owner, repo, "issues", &n], &[]).await
    }

    /// `POST /repos/{owner}/{name}/issues`: opens an issue and returns what
    /// the studio said about it (its number, when it says).
    pub async fn create_issue(
        &self,
        owner: &str,
        repo: &str,
        title: &str,
        body: &str,
    ) -> Result<Issue> {
        let new = NewIssue {
            title: title.to_string(),
            body: body.to_string(),
        };
        Ok(self
            .post(&["repos", owner, repo, "issues"], Some(&new))
            .await?
            .1)
    }

    /// `POST …/issues/{n}/comments`.
    pub async fn comment_on_issue(
        &self,
        owner: &str,
        repo: &str,
        number: u64,
        body: &str,
    ) -> Result<()> {
        #[derive(Serialize)]
        struct Body<'a> {
            body: &'a str,
        }
        let n = number.to_string();
        self.call(
            Method::POST,
            &["repos", owner, repo, "issues", &n, "comments"],
            &[],
            Some(&Body { body }),
            true,
        )
        .await
        .map(|_| ())
    }

    /// `PATCH …/issues/{n}` `{"state": "open" | "closed"}`.
    pub async fn set_issue_open(
        &self,
        owner: &str,
        repo: &str,
        number: u64,
        open: bool,
    ) -> Result<()> {
        #[derive(Serialize)]
        struct Body {
            state: &'static str,
        }
        let n = number.to_string();
        let body = Body {
            state: if open { "open" } else { "closed" },
        };
        self.call(
            Method::PATCH,
            &["repos", owner, repo, "issues", &n],
            &[],
            Some(&body),
            true,
        )
        .await
        .map(|_| ())
    }

    // ---------------------------------------------------- runs and agents

    /// `GET /runs`.
    pub async fn runs(&self, query: &RunsQuery) -> Result<Vec<Run>> {
        let mut q = Vec::new();
        if let Some(limit) = query.limit {
            q.push(("limit", limit.to_string()));
        }
        if let Some(runner) = &query.runner {
            q.push(("runner", runner.clone()));
        }
        if let Some(repo) = &query.repo {
            q.push(("repo", repo.clone()));
        }
        Ok(self.get::<RunList>(&["runs"], &q).await?.runs)
    }

    /// `GET /jobs/{id}/log?tail=N` (masked by the studio).
    pub async fn job_log(&self, job_id: u64, tail: Option<u32>) -> Result<JobLog> {
        let query: Vec<_> = tail
            .map(|t| ("tail", t.min(MAX_LOG_TAIL).to_string()))
            .into_iter()
            .collect();
        self.get(&["jobs", &job_id.to_string(), "log"], &query)
            .await
    }

    /// `GET /agents/waiting`.
    pub async fn agents_waiting(&self) -> Result<Vec<WaitingAgent>> {
        Ok(self
            .get::<WaitingAgents>(&["agents", "waiting"], &[])
            .await?
            .jobs)
    }

    /// `POST /agents/jobs/{id}/approve`.
    pub async fn approve_agent(&self, job_id: u64) -> Result<Approved> {
        let id = job_id.to_string();
        Ok(self
            .post::<(), _>(&["agents", "jobs", &id, "approve"], None)
            .await?
            .1)
    }

    /// `POST /jobs/{id}/cancel`.
    pub async fn cancel_job(&self, job_id: u64) -> Result<Cancelled> {
        let id = job_id.to_string();
        Ok(self.post::<(), _>(&["jobs", &id, "cancel"], None).await?.1)
    }

    // ------------------------------------------------------------- remote

    /// `GET /remote`.
    pub async fn remote(&self) -> Result<Remote> {
        self.get(&["remote"], &[]).await
    }

    /// `POST /remote {"on": …}`. Through the tunnel, turning it on is
    /// refused with `403 remote_only_off`.
    pub async fn set_remote(&self, on: bool) -> Result<Remote> {
        Ok(self.post(&["remote"], Some(&SetRemote { on })).await?.1)
    }

    /// `GET /remote/who`.
    pub async fn remote_who(&self) -> Result<Vec<RemoteSession>> {
        Ok(self
            .get::<RemoteWho>(&["remote", "who"], &[])
            .await?
            .sessions)
    }

    /// `POST /remote/link`: a one-time passkey link.
    pub async fn remote_link(&self) -> Result<RemoteLink> {
        Ok(self.post::<(), _>(&["remote", "link"], None).await?.1)
    }

    // ----------------------------------------------------------- messages

    /// `GET /messages`.
    pub async fn messages(&self) -> Result<Messages> {
        self.get(&["messages"], &[]).await
    }

    /// `PUT /messages`: returns the full object.
    pub async fn put_messages(&self, update: &MessagesUpdate) -> Result<Messages> {
        let (_, bytes) = self
            .call(Method::PUT, &["messages"], &[], Some(update), true)
            .await?;
        decode(&bytes)
    }
}

fn decode<T: DeserializeOwned>(bytes: &[u8]) -> Result<T> {
    // An empty body reads as `{}` so endpoints that answer with nothing still work.
    let bytes: &[u8] = if bytes.iter().all(u8::is_ascii_whitespace) {
        b"{}"
    } else {
        bytes
    };
    serde_json::from_slice(bytes).map_err(|e| Error::Decode(e.to_string()))
}

pub(crate) fn api_error(status: StatusCode, body: &[u8]) -> Error {
    let parsed: ErrorBody = serde_json::from_slice(body).unwrap_or_default();
    let code = parsed
        .error
        .filter(|c| !c.is_empty())
        .unwrap_or_else(|| format!("http_{}", status.as_u16()));
    let message = parsed
        .message
        .filter(|m| !m.trim().is_empty())
        .unwrap_or_else(|| default_message(status, &code));
    Error::Api {
        status: status.as_u16(),
        code,
        message,
    }
}

/// A sentence for errors the studio sent without one.
fn default_message(status: StatusCode, code: &str) -> String {
    match (status.as_u16(), code) {
        (_, "revoked") => "This device was signed out of your studio.".into(),
        (_, "name_taken") => "That name is already taken.".into(),
        (_, "invalid_name") => "That isn’t a name a repository can have.".into(),
        (_, "device_not_allowed") => {
            "This device isn’t allowed to rename or delete repositories.".into()
        }
        (_, "confirm_mismatch") => "The name typed to confirm didn’t match.".into(),
        (_, "invalid_title") => "An issue needs a title of one line.".into(),
        (_, "body_too_long") => "That text is too long.".into(),
        (_, "empty_body") => "Write something first.".into(),
        (_, "no_auto_merge") => "That pull request wasn’t waiting to merge.".into(),
        (_, "not_waiting") => "That agent isn’t waiting for approval any more.".into(),
        (_, "finished") => "That job has already finished.".into(),
        (_, "tether_off") => "GitHub Tether isn’t set up in your studio.".into(),
        (_, "remote_off") => "Remote access is off.".into(),
        (_, "too_many_streams") => "Too many live connections are open to your studio.".into(),
        (_, "remote_only_off") => "From outside, remote access can only be turned off.".into(),
        (401, _) => "Your studio didn’t accept this device’s token.".into(),
        (403, _) => "This device isn’t allowed to do that.".into(),
        (404, _) => "Your studio couldn’t find that.".into(),
        (409, _) => "Your studio couldn’t do that right now.".into(),
        (429, _) => "Too many tries. Wait a moment.".into(),
        (s, _) if s >= 500 => format!("Your studio had a problem (HTTP {s})."),
        (s, code) => format!("Your studio refused (HTTP {s}, {code})."),
    }
}

/// reqwest errors can carry the URL but never headers or bodies, so the
/// token cannot leak here; the URL is dropped anyway to keep it short.
pub(crate) fn network_error(e: reqwest::Error) -> Error {
    let e = e.without_url();
    let mut text = e.to_string();
    let mut source = std::error::Error::source(&e);
    while let Some(inner) = source {
        let inner_text = inner.to_string();
        if !text.contains(&inner_text) {
            text.push_str(": ");
            text.push_str(&inner_text);
        }
        source = inner.source();
    }
    Error::Network(text)
}
