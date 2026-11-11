//! Live events: SSE parsing, typed events, reconnecting.

mod common;

use std::time::Duration;

use atelier_core::api::{Moment, Thread};
use atelier_core::events::{decode_frame, Backoff, SseFrame, SseParser};
use atelier_core::{Error, Event};
use common::{bearer, client};
use wiremock::matchers::{header, method, path};
use wiremock::{Mock, MockServer, ResponseTemplate};

/// The contract's example stream, verbatim, with the ping comment it describes.
const STREAM: &str = r#": ping

event: notify
data: {"id":"n:5512","kind":"run_failed","thread":"yours","title":"ci failed on feature/ssh","detail":"Atelier · test","repo":"DemiInfinity/Atelier","web_path":"/…","moment":"fail","at":"…"}

event: status
data: {"changed":["needs","running"]}

event: remote
data: {"on":true,"address":"https://quiet-fox-lamp.trycloudflare.com","connected":0}

"#;

const FAST: Backoff = Backoff {
    initial: Duration::from_millis(10),
    max: Duration::from_millis(40),
    idle_timeout: Duration::from_secs(5),
};

fn sse(body: &str) -> ResponseTemplate {
    ResponseTemplate::new(200).set_body_raw(body.to_owned(), "text/event-stream")
}

#[test]
fn parser_reads_the_contract_example() {
    let frames = SseParser::new().feed(STREAM.as_bytes());
    let names: Vec<&str> = frames.iter().map(|f| f.event.as_str()).collect();
    assert_eq!(names, ["notify", "status", "remote"]);

    let Some(Event::Notify(notify)) = decode_frame(&frames[0]) else {
        panic!("not a notify");
    };
    assert_eq!(notify.id, "n:5512");
    assert_eq!(notify.kind, "run_failed");
    assert_eq!(notify.thread, Thread::Yours);
    assert_eq!(notify.title, "ci failed on feature/ssh");
    assert_eq!(notify.detail, "Atelier · test");
    assert_eq!(notify.repo, "DemiInfinity/Atelier");
    assert_eq!(notify.moment, Some(Moment::Fail));

    let Some(Event::Status(status)) = decode_frame(&frames[1]) else {
        panic!("not a status");
    };
    assert_eq!(status.changed, ["needs", "running"]);

    let Some(Event::Remote(remote)) = decode_frame(&frames[2]) else {
        panic!("not a remote");
    };
    assert!(remote.on);
    assert_eq!(
        remote.address.as_deref(),
        Some("https://quiet-fox-lamp.trycloudflare.com")
    );
    assert_eq!(remote.connected, 0);
}

#[test]
fn parser_handles_any_chunking_and_crlf() {
    let whole = SseParser::new().feed(STREAM.as_bytes());
    // Byte by byte (which also splits the multi-byte "·" and "…").
    let mut parser = SseParser::new();
    let mut frames = Vec::new();
    for byte in STREAM.as_bytes() {
        frames.extend(parser.feed(std::slice::from_ref(byte)));
    }
    assert_eq!(frames, whole);

    let crlf = STREAM.replace('\n', "\r\n");
    assert_eq!(SseParser::new().feed(crlf.as_bytes()), whole);
}

#[test]
fn parser_details() {
    let mut parser = SseParser::new();
    // Nothing until the blank line arrives.
    assert!(parser
        .feed(b"event: status\ndata: {\"changed\":[]}\n")
        .is_empty());
    assert_eq!(parser.feed(b"\n").len(), 1);
    // Comments and keep-alives produce nothing.
    assert!(parser.feed(b": ping\n\n: ping\n\n").is_empty());
    // No event name means "message"; multi-line data is joined; ids stick;
    // "data:x" without the space is fine.
    let frames = parser.feed(b"id: 7\ndata:a\ndata: b\n\ndata: c\n\n");
    assert_eq!(
        frames,
        [
            SseFrame {
                event: "message".into(),
                data: "a\nb".into(),
                id: Some("7".into()),
                retry: None
            },
            SseFrame {
                event: "message".into(),
                data: "c".into(),
                id: Some("7".into()),
                retry: None
            }
        ]
    );
}

#[test]
fn unknown_events_and_bad_data_are_skipped() {
    let frame = |event: &str, data: &str| SseFrame {
        event: event.into(),
        data: data.into(),
        id: None,
        retry: None,
    };
    assert_eq!(decode_frame(&frame("weather", r#"{"rain":true}"#)), None);
    assert_eq!(decode_frame(&frame("status", "not json")), None);
    // Unknown fields and values inside a known event are fine.
    let Some(Event::Notify(notify)) = decode_frame(&frame(
        "notify",
        r#"{"id":"n:1","kind":"brand_new","moment":"confetti","thread":"elsewhere","extra":[1]}"#,
    )) else {
        panic!("should parse");
    };
    assert_eq!(notify.kind, "brand_new");
    assert_eq!(notify.moment, Some(Moment::Unknown));
    assert_eq!(notify.thread, Thread::Unknown);
    // moment may be null.
    let Some(Event::Notify(notify)) = decode_frame(&frame("notify", r#"{"moment":null}"#)) else {
        panic!("should parse");
    };
    assert_eq!(notify.moment, None);
}

#[test]
fn backoff_doubles_up_to_the_cap() {
    let backoff = Backoff {
        initial: Duration::from_secs(1),
        max: Duration::from_secs(30),
        idle_timeout: Duration::from_secs(70),
    };
    let delays: Vec<u64> = (0..7).map(|n| backoff.delay(n).as_secs()).collect();
    assert_eq!(delays, [1, 2, 4, 8, 16, 30, 30]);
    assert_eq!(backoff.delay(u32::MAX).as_secs(), 30);
}

#[tokio::test]
async fn stream_yields_typed_events_and_reconnects_when_the_connection_ends() {
    let server = MockServer::start().await;
    // First connection: the example stream, then the server hangs up.
    Mock::given(method("GET"))
        .and(path("/api/app/v1/events"))
        .and(header("authorization", bearer().as_str()))
        .and(header("accept", "text/event-stream"))
        .respond_with(sse(STREAM))
        .up_to_n_times(1)
        .expect(1)
        .mount(&server)
        .await;
    // Second connection: one more event.
    Mock::given(method("GET"))
        .and(path("/api/app/v1/events"))
        .respond_with(sse(
            "id: 9\nevent: status\ndata: {\"changed\":[\"feed\"]}\n\n",
        ))
        .mount(&server)
        .await;

    let mut events = client(&server).events_with(FAST);
    assert!(!events.is_connected());
    assert!(matches!(events.next().await.unwrap(), Event::Notify(_)));
    assert!(events.is_connected());
    assert!(matches!(events.next().await.unwrap(), Event::Status(_)));
    assert!(matches!(events.next().await.unwrap(), Event::Remote(_)));
    assert_eq!(events.connects(), 1);

    let Event::Status(after) = events.next().await.unwrap() else {
        panic!("expected the event from the second connection");
    };
    assert_eq!(after.changed, ["feed"]);
    assert_eq!(events.connects(), 2);

    // The third connection resumes from the last id it saw.
    events.next().await.unwrap();
    let requests = server.received_requests().await.unwrap();
    assert!(requests[0].headers.get("last-event-id").is_none());
    assert_eq!(requests[2].headers.get("last-event-id").unwrap(), "9");
}

#[tokio::test]
async fn stream_retries_server_errors() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/events"))
        .respond_with(ResponseTemplate::new(503))
        .up_to_n_times(3)
        .expect(3)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/events"))
        .respond_with(sse("event: status\ndata: {\"changed\":[\"needs\"]}\n\n"))
        .mount(&server)
        .await;
    let mut events = client(&server).events_with(FAST);
    assert!(matches!(events.next().await.unwrap(), Event::Status(_)));
    assert_eq!(events.connects(), 4);
}

#[tokio::test]
async fn stream_retries_while_the_studio_is_down() {
    // Reserve a port, start with nothing listening on it.
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    drop(listener);
    let client = atelier_core::Client::new(
        &format!("http://{address}"),
        Some(atelier_core::Token::new(common::TOKEN)),
        atelier_core::ClientOptions::default(),
    )
    .unwrap();
    let mut events = client.events_with(FAST);
    let next = tokio::spawn(async move {
        let event = events.next().await;
        (event, events.connects())
    });
    tokio::time::sleep(Duration::from_millis(150)).await;

    // The studio comes back on the same port.
    let listener = std::net::TcpListener::bind(address).unwrap();
    let server = MockServer::builder().listener(listener).start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/events"))
        .respond_with(sse("event: remote\ndata: {\"on\":false}\n\n"))
        .mount(&server)
        .await;
    let (event, connects) = tokio::time::timeout(Duration::from_secs(10), next)
        .await
        .expect("reconnected in time")
        .unwrap();
    assert!(matches!(event.unwrap(), Event::Remote(remote) if !remote.on));
    assert!(connects > 1, "{connects} connects");
}

#[tokio::test]
async fn stream_reopens_a_silent_connection() {
    let server = MockServer::start().await;
    // The first answer stalls far longer than the idle timeout.
    Mock::given(method("GET"))
        .and(path("/api/app/v1/events"))
        .respond_with(sse("event: status\ndata: {}\n\n").set_delay(Duration::from_secs(30)))
        .up_to_n_times(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/events"))
        .respond_with(sse("event: status\ndata: {\"changed\":[\"running\"]}\n\n"))
        .mount(&server)
        .await;
    // wiremock stalls before the headers; a stall mid-body takes the same path.
    let client = atelier_core::Client::new(
        &server.uri(),
        Some(atelier_core::Token::new(common::TOKEN)),
        atelier_core::ClientOptions::default(),
    )
    .unwrap();
    let mut events = client.events_with(Backoff {
        idle_timeout: Duration::from_millis(200),
        ..FAST
    });
    let event = tokio::time::timeout(Duration::from_secs(10), events.next())
        .await
        .expect("did not hang on the silent connection")
        .unwrap();
    assert!(matches!(event, Event::Status(s) if s.changed == ["running"]));
}

#[tokio::test]
async fn stream_stops_when_the_token_is_refused() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/app/v1/events"))
        .respond_with(
            ResponseTemplate::new(401).set_body_raw(r#"{"error":"revoked"}"#, "application/json"),
        )
        .expect(1)
        .mount(&server)
        .await;
    let err = client(&server).events_with(FAST).next().await.unwrap_err();
    assert_eq!(err.code(), Some("revoked"));
    assert!(err.needs_sign_in());
}

#[tokio::test]
async fn stream_needs_a_token() {
    let server = MockServer::start().await;
    let err = common::anonymous(&server)
        .events()
        .next()
        .await
        .unwrap_err();
    assert!(matches!(err, Error::NotSignedIn));
}
