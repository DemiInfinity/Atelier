#![allow(dead_code)]
//! Shared by the integration tests: a client pointed at a mock studio.

use atelier_core::{Client, ClientOptions, Token};
use wiremock::MockServer;

pub const TOKEN: &str = "atd_0123456789abcdefghijABCDEFGHIJ0123456789";

pub fn client(server: &MockServer) -> Client {
    Client::new(
        &server.uri(),
        Some(Token::new(TOKEN)),
        ClientOptions::default(),
    )
    .unwrap()
}

pub fn anonymous(server: &MockServer) -> Client {
    Client::new(&server.uri(), None, ClientOptions::default()).unwrap()
}

pub fn bearer() -> String {
    format!("Bearer {TOKEN}")
}
