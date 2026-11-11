//! Client core for Atelier: the studio's app API, live events, the runner's
//! local control socket, local git helpers, device sign-in and friendly
//! messages. Shared by the `atl` command-line tool and the desktop app.
//!
//! The contract is `docs/app-api.md` in the Atelier repository.

pub mod api;
pub mod client;
pub mod config;
pub mod diff;
pub mod error;
pub mod events;
pub mod git;
pub mod messages;
pub mod runner;
pub mod ssh;
pub mod token;

pub use client::{Client, ClientOptions, PollOptions};
pub use config::{Config, Env, Settings};
pub use error::{Error, Result};
pub use events::{Event, EventStream};
pub use git::Git;
pub use runner::RunnerClient;
pub use token::{Token, TokenStore};

/// The version of this crate, sent as the device version at sign-in.
pub const VERSION: &str = env!("CARGO_PKG_VERSION");
