//! Friendly messages: picking what to say for a moment (section 9 of
//! `docs/app-api.md`).

use std::collections::BTreeMap;

use crate::api::{Messages, Tone, MAX_MESSAGE_CHARS, MAX_YOURS_PER_MOMENT};
use crate::error::{Error, Result};

/// The placeholders a message may use.
pub const PLACEHOLDERS: [&str; 7] = [
    "repo", "branch", "base", "count", "device", "time", "workflow",
];

/// Values for the placeholders. Anything not set is "not known right now".
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Vars {
    values: BTreeMap<String, String>,
}

impl Vars {
    pub fn new() -> Self {
        Self::default()
    }

    /// Sets a placeholder (`repo`, `branch`, `base`, `count`, `device`,
    /// `time`, `workflow`). Empty values count as not set.
    pub fn set(mut self, name: &str, value: impl ToString) -> Self {
        let value = value.to_string();
        if !value.is_empty() {
            self.values.insert(name.to_string(), value);
        }
        self
    }

    pub fn set_opt(self, name: &str, value: Option<impl ToString>) -> Self {
        match value {
            Some(value) => self.set(name, value),
            None => self,
        }
    }
}

/// Fills `{name}` placeholders. `None` when the text needs a known
/// placeholder there is no value for. Braces around anything that isn't a
/// placeholder name are left alone.
pub fn fill(template: &str, vars: &Vars) -> Option<String> {
    let mut out = String::with_capacity(template.len());
    let mut rest = template;
    while let Some(open) = rest.find('{') {
        out.push_str(&rest[..open]);
        let after = &rest[open + 1..];
        match after.find('}') {
            Some(close) if PLACEHOLDERS.contains(&&after[..close]) => {
                out.push_str(vars.values.get(&after[..close])?);
                rest = &after[close + 1..];
            }
            _ => {
                out.push('{');
                rest = after;
            }
        }
    }
    out.push_str(rest);
    Some(out)
}

/// What to say for `moment` (`push`, `clean`, `pass`, `fail`, `merge`,
/// `first`, `late`, `mile`, `agent`).
///
/// - tone `off`: nothing.
/// - tone `plain`: the moment's plain sentence.
/// - tone `friendly`: one of `ours` + `yours` whose placeholders can all be
///   filled; when there is none, the plain sentence.
///
/// With a `seed` the choice is deterministic (`seed % candidates`, `ours`
/// first, then `yours`); without one it varies.
pub fn pick(messages: &Messages, moment: &str, vars: &Vars, seed: Option<u64>) -> Option<String> {
    let plain = || messages.plain.get(moment).and_then(|text| fill(text, vars));
    match messages.tone {
        Tone::Off => None,
        Tone::Plain | Tone::Unknown => plain(),
        Tone::Friendly => {
            let candidates: Vec<String> = messages
                .moments
                .get(moment)
                .map(|m| m.ours.iter().chain(m.yours.iter()))
                .into_iter()
                .flatten()
                .filter_map(|text| fill(text, vars))
                .collect();
            if candidates.is_empty() {
                return plain();
            }
            let index = seed.unwrap_or_else(wobble) % candidates.len() as u64;
            candidates.into_iter().nth(index as usize)
        }
    }
}

/// Something that differs from call to call; no need for real randomness.
fn wobble() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos() as u64 / 1000)
        .unwrap_or(0)
}

/// Checks one of the person's own messages against the server's limits
/// (plain text, at most 140 characters) and tidies it.
pub fn validate_yours(text: &str) -> Result<String> {
    let text = text.split_whitespace().collect::<Vec<_>>().join(" ");
    if text.is_empty() {
        return Err(Error::Invalid("A message needs some words.".into()));
    }
    let length = text.chars().count();
    if length > MAX_MESSAGE_CHARS {
        return Err(Error::Invalid(format!(
            "That message is {length} characters; the most is {MAX_MESSAGE_CHARS}."
        )));
    }
    if text.chars().any(char::is_control) {
        return Err(Error::Invalid("Messages are plain text only.".into()));
    }
    Ok(text)
}

/// `yours` for `moment` with `text` added, ready for `PUT /messages`.
pub fn yours_with(messages: &Messages, moment: &str, text: &str) -> Result<Vec<String>> {
    let text = validate_yours(text)?;
    let Some(existing) = messages.moments.get(moment) else {
        return Err(unknown_moment(messages, moment));
    };
    if existing.yours.len() >= MAX_YOURS_PER_MOMENT {
        return Err(Error::Invalid(format!(
            "You already have {MAX_YOURS_PER_MOMENT} messages for {moment}; remove one first."
        )));
    }
    let mut yours = existing.yours.clone();
    if !yours.contains(&text) {
        yours.push(text);
    }
    Ok(yours)
}

/// `yours` for `moment` without its `number`th message (1-based), and the
/// message that was removed.
pub fn yours_without(
    messages: &Messages,
    moment: &str,
    number: usize,
) -> Result<(Vec<String>, String)> {
    let Some(existing) = messages.moments.get(moment) else {
        return Err(unknown_moment(messages, moment));
    };
    if number == 0 || number > existing.yours.len() {
        return Err(Error::Invalid(match existing.yours.len() {
            0 => format!("You have no messages of your own for {moment}."),
            1 => format!("You have 1 message of your own for {moment}."),
            n => format!("You have {n} messages of your own for {moment}; pick 1 to {n}."),
        }));
    }
    let mut yours = existing.yours.clone();
    let removed = yours.remove(number - 1);
    Ok((yours, removed))
}

fn unknown_moment(messages: &Messages, moment: &str) -> Error {
    let known: Vec<&str> = messages.moments.keys().map(String::as_str).collect();
    Error::Invalid(format!(
        "There is no “{moment}” moment. The moments are: {}.",
        known.join(", ")
    ))
}
