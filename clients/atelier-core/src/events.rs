//! Live events: a Server-Sent Events client for `GET /events` that
//! reconnects by itself (section 8 of `docs/app-api.md`).

use std::collections::VecDeque;
use std::time::Duration;

use crate::api::{Notify, RemoteChanged, StatusChanged};
use crate::client::{api_error, network_error, Client};
use crate::error::{Error, Result};

/// One event from the studio.
#[derive(Clone, Debug, PartialEq)]
pub enum Event {
    /// Something to tell the person about.
    Notify(Notify),
    /// Parts of `GET /status` changed; fetch it again.
    Status(StatusChanged),
    /// Remote access changed.
    Remote(RemoteChanged),
}

/// One raw SSE frame.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct SseFrame {
    /// The `event:` name (`message` when the server gave none).
    pub event: String,
    pub data: String,
    pub id: Option<String>,
    /// A `retry:` hint in milliseconds.
    pub retry: Option<u64>,
}

/// An incremental SSE parser: feed it bytes as they arrive, get whole frames.
#[derive(Debug, Default)]
pub struct SseParser {
    buffer: Vec<u8>,
    event: String,
    data: Vec<String>,
    id: Option<String>,
    retry: Option<u64>,
}

impl SseParser {
    pub fn new() -> Self {
        Self::default()
    }

    /// Adds bytes; returns the frames they completed. Comment lines
    /// (`: ping`) and frames without data produce nothing.
    pub fn feed(&mut self, bytes: &[u8]) -> Vec<SseFrame> {
        self.buffer.extend_from_slice(bytes);
        let mut frames = Vec::new();
        while let Some(end) = self.buffer.iter().position(|&b| b == b'\n') {
            let mut line: Vec<u8> = self.buffer.drain(..=end).collect();
            line.pop();
            if line.last() == Some(&b'\r') {
                line.pop();
            }
            let line = String::from_utf8_lossy(&line);
            if let Some(frame) = self.line(&line) {
                frames.push(frame);
            }
        }
        frames
    }

    fn line(&mut self, line: &str) -> Option<SseFrame> {
        if line.is_empty() {
            let retry = self.retry.take();
            let event = std::mem::take(&mut self.event);
            let data = std::mem::take(&mut self.data);
            if data.is_empty() && retry.is_none() {
                return None;
            }
            return Some(SseFrame {
                event: if event.is_empty() {
                    "message".to_string()
                } else {
                    event
                },
                data: data.join("\n"),
                id: self.id.clone(),
                retry,
            });
        }
        if line.starts_with(':') {
            return None;
        }
        let (field, value) = line.split_once(':').unwrap_or((line, ""));
        let value = value.strip_prefix(' ').unwrap_or(value);
        match field {
            "event" => self.event = value.to_string(),
            "data" => self.data.push(value.to_string()),
            "id" if !value.contains('\0') => self.id = Some(value.to_string()),
            "retry" => self.retry = value.parse().ok(),
            _ => {}
        }
        None
    }
}

/// Turns a frame into a typed event. Unknown event names and data that
/// doesn't parse give `None` (a newer studio may send more than we know).
pub fn decode_frame(frame: &SseFrame) -> Option<Event> {
    match frame.event.as_str() {
        "notify" => serde_json::from_str(&frame.data).ok().map(Event::Notify),
        "status" => serde_json::from_str(&frame.data).ok().map(Event::Status),
        "remote" => serde_json::from_str(&frame.data).ok().map(Event::Remote),
        _ => None,
    }
}

/// How an [`EventStream`] waits between reconnects: doubling from `initial`
/// up to `max`, back to `initial` once events flow again.
#[derive(Clone, Copy, Debug)]
pub struct Backoff {
    pub initial: Duration,
    pub max: Duration,
    /// With nothing received for this long (the studio pings every 25 s) the
    /// connection is taken for dead and reopened.
    pub idle_timeout: Duration,
}

impl Default for Backoff {
    fn default() -> Self {
        Backoff {
            initial: Duration::from_secs(1),
            max: Duration::from_secs(30),
            idle_timeout: Duration::from_secs(70),
        }
    }
}

impl Backoff {
    /// The wait before reconnect number `attempt` (0-based).
    pub fn delay(&self, attempt: u32) -> Duration {
        let factor = 2u32.saturating_pow(attempt.min(16));
        self.initial.saturating_mul(factor).min(self.max)
    }
}

/// The studio's live events. Call [`EventStream::next`] in a loop.
pub struct EventStream {
    client: Client,
    backoff: Backoff,
    response: Option<reqwest::Response>,
    parser: SseParser,
    queue: VecDeque<Event>,
    last_id: Option<String>,
    attempt: u32,
    connects: u64,
}

impl Client {
    /// Subscribes to `GET /events`. Nothing is sent until the first
    /// [`EventStream::next`].
    pub fn events(&self) -> EventStream {
        self.events_with(Backoff::default())
    }

    pub fn events_with(&self, backoff: Backoff) -> EventStream {
        EventStream {
            client: self.clone(),
            backoff,
            response: None,
            parser: SseParser::new(),
            queue: VecDeque::new(),
            last_id: None,
            attempt: 0,
            connects: 0,
        }
    }
}

impl EventStream {
    /// The next event, waiting as long as it takes. Dropped connections,
    /// timeouts and server errors are retried with backoff and never
    /// surface here. An error means retrying is pointless: not signed in,
    /// the token was refused (`401`/`403`), or there is no such endpoint.
    pub async fn next(&mut self) -> Result<Event> {
        loop {
            if let Some(event) = self.queue.pop_front() {
                return Ok(event);
            }
            if self.response.is_none() {
                self.connect().await?;
                continue;
            }
            let response = self.response.as_mut().expect("checked above");
            let chunk = tokio::time::timeout(self.backoff.idle_timeout, response.chunk()).await;
            match chunk {
                Ok(Ok(Some(bytes))) => {
                    for frame in self.parser.feed(&bytes) {
                        if frame.id.is_some() {
                            self.last_id.clone_from(&frame.id);
                        }
                        if let Some(event) = decode_frame(&frame) {
                            self.attempt = 0;
                            self.queue.push_back(event);
                        }
                    }
                }
                // Closed, broken or silent: start over.
                Ok(Ok(None)) | Ok(Err(_)) | Err(_) => self.drop_connection(),
            }
        }
    }

    /// True while a connection to the studio is open.
    pub fn is_connected(&self) -> bool {
        self.response.is_some()
    }

    /// How many times a connection has been opened (1 = never reconnected).
    pub fn connects(&self) -> u64 {
        self.connects
    }

    fn drop_connection(&mut self) {
        self.response = None;
        self.parser = SseParser::new();
    }

    /// Opens a connection, retrying what can be retried.
    async fn connect(&mut self) -> Result<()> {
        loop {
            if self.connects > 0 {
                tokio::time::sleep(self.backoff.delay(self.attempt)).await;
                self.attempt = self.attempt.saturating_add(1);
            }
            self.connects += 1;
            let mut request = self
                .client
                .stream_request(self.client.api_url(&["events"]))?
                .header(reqwest::header::ACCEPT, "text/event-stream");
            if let Some(id) = &self.last_id {
                request = request.header("Last-Event-ID", id);
            }
            // The headers must arrive within the idle timeout too, or a
            // stalled studio would hold this forever.
            let sent = match tokio::time::timeout(self.backoff.idle_timeout, request.send()).await {
                Ok(sent) => sent.map_err(network_error),
                Err(_) => continue,
            };
            match sent {
                Ok(response) if response.status().is_success() => {
                    self.response = Some(response);
                    return Ok(());
                }
                Ok(response) => {
                    let status = response.status();
                    // 429 `too_many_streams` (more than 8) and other server
                    // trouble are retried with backoff; these are final.
                    if matches!(status.as_u16(), 401 | 403 | 404) {
                        let body = response.bytes().await.unwrap_or_default();
                        return Err(api_error(status, &body));
                    }
                }
                Err(Error::Network(_)) => {}
                Err(e) => return Err(e),
            }
        }
    }
}
