//! Atelier Desktop: the studio's companion on this machine.
//!
//! [`desk::Desk`] holds the state and does the work through `atelier-core`;
//! it knows nothing about windows. [`shell`] is the Tauri part: commands,
//! the menu bar icon and its popover, live events, notifications.

pub mod add;
pub mod desk;
pub mod device;
pub mod error;
pub mod issues;
pub mod manage;
pub mod notify;
pub mod paths;
pub mod prefs;
pub mod pulls;
pub mod repo;
pub mod studio;
pub mod today;
pub mod tools;

mod commands;
mod shell;

pub use desk::Desk;
pub use shell::run;
