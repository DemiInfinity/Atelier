//! `atl say`: the friendly messages.

use std::collections::BTreeMap;

use atelier_core::api::{Messages, MessagesUpdate, Tone};
use atelier_core::messages::{yours_with, yours_without};
use serde_json::json;

use crate::ctx::Ctx;
use crate::error::{CliError, CliResult};
use crate::ui::{column, count, outln, pad};
use crate::ToneArg;

fn tone_sentence(tone: Tone) -> &'static str {
    match tone {
        Tone::Friendly => "Messages are friendly.",
        Tone::Plain | Tone::Unknown => "Messages are plain.",
        Tone::Off => "Messages are off. Plain output only.",
    }
}

fn moment<'a>(
    messages: &'a Messages,
    name: &str,
) -> CliResult<&'a atelier_core::api::MomentMessages> {
    messages.moments.get(name).ok_or_else(|| {
        CliError::plain(format!("There is no “{name}” moment."))
            .hint(format!(
                "The moments are: {}.",
                messages
                    .moments
                    .keys()
                    .map(String::as_str)
                    .collect::<Vec<_>>()
                    .join(", ")
            ))
            .kind("unknown_moment")
    })
}

pub async fn overview(ctx: &Ctx) -> CliResult<()> {
    let client = ctx.client()?;
    let messages = client.messages().await.map_err(|e| ctx.err(e))?;
    if ctx.json {
        crate::print_json(&messages);
        return Ok(());
    }
    let ui = &ctx.ui;
    outln!("{}", tone_sentence(messages.tone));
    let width = column(messages.moments.keys().map(String::as_str), 16);
    for (name, moment) in &messages.moments {
        let mut line = format!(
            "{}{}",
            ui.amber(&format!("  {}  ", pad(name, width))),
            ui.muted(&count(moment.ours.len() as u64, "message", "messages"))
        );
        if !moment.yours.is_empty() {
            line.push_str(&ui.muted(&format!(" · {} yours", moment.yours.len())));
        }
        outln!("{line}");
    }
    outln!();
    outln!(
        "{}{}",
        ui.muted("Add your own: "),
        ui.amber("atl say add push \"Shipped. Go outside.\"")
    );
    Ok(())
}

pub async fn list(ctx: &Ctx, name: &str) -> CliResult<()> {
    let client = ctx.client()?;
    let messages = client.messages().await.map_err(|e| ctx.err(e))?;
    let found = moment(&messages, name)?;
    if ctx.json {
        crate::print_json(&json!({
            "moment": name, "ours": found.ours, "yours": found.yours,
            "plain": messages.plain.get(name),
        }));
        return Ok(());
    }
    let ui = &ctx.ui;
    for text in &found.ours {
        outln!("{}", ui.muted(&format!("  {text}")));
    }
    for text in &found.yours {
        outln!("{}{}", ui.amber(&format!("  {text}")), ui.muted("   yours"));
    }
    if found.ours.is_empty() && found.yours.is_empty() {
        outln!("{}", ui.muted("  Nothing yet."));
    }
    Ok(())
}

async fn put_yours(
    ctx: &Ctx,
    client: &atelier_core::Client,
    name: &str,
    yours: Vec<String>,
) -> CliResult<Messages> {
    let update = MessagesUpdate {
        yours: Some(BTreeMap::from([(name.to_string(), yours)])),
        ..MessagesUpdate::default()
    };
    client.put_messages(&update).await.map_err(|e| ctx.err(e))
}

pub async fn add(ctx: &Ctx, name: &str, text: &str) -> CliResult<()> {
    let client = ctx.client()?;
    let messages = client.messages().await.map_err(|e| ctx.err(e))?;
    moment(&messages, name)?;
    let yours = yours_with(&messages, name, text).map_err(|e| ctx.err(e))?;
    let saved = put_yours(ctx, &client, name, yours).await?;
    let now = saved.moments.get(name).cloned().unwrap_or_default();
    if ctx.json {
        crate::print_json(&json!({"moment": name, "ours": now.ours, "yours": now.yours}));
        return Ok(());
    }
    outln!(
        "Added to your {name} messages {}.",
        ctx.ui
            .muted(&format!("({} now)", now.ours.len() + now.yours.len()))
    );
    Ok(())
}

pub async fn remove(ctx: &Ctx, name: &str, number: usize) -> CliResult<()> {
    let client = ctx.client()?;
    let messages = client.messages().await.map_err(|e| ctx.err(e))?;
    moment(&messages, name)?;
    let (yours, removed) = yours_without(&messages, name, number).map_err(|e| {
        ctx.err(e)
            .hint(format!("See them with atl say list {name}."))
    })?;
    let saved = put_yours(ctx, &client, name, yours).await?;
    let now = saved.moments.get(name).cloned().unwrap_or_default();
    if ctx.json {
        crate::print_json(&json!({
            "moment": name, "removed": removed, "ours": now.ours, "yours": now.yours,
        }));
        return Ok(());
    }
    outln!(
        "Removed “{removed}” from your {name} messages {}.",
        ctx.ui
            .muted(&format!("({} now)", now.ours.len() + now.yours.len()))
    );
    Ok(())
}

pub async fn tone(ctx: &Ctx, tone: ToneArg) -> CliResult<()> {
    let client = ctx.client()?;
    let tone = match tone {
        ToneArg::Friendly => Tone::Friendly,
        ToneArg::Plain => Tone::Plain,
        ToneArg::Off => Tone::Off,
    };
    let update = MessagesUpdate {
        tone: Some(tone),
        ..MessagesUpdate::default()
    };
    let saved = client.put_messages(&update).await.map_err(|e| ctx.err(e))?;
    if ctx.json {
        crate::print_json(&json!({"tone": saved.tone}));
        return Ok(());
    }
    // Say what was asked for; the studio's answer is the same unless it is older than us.
    outln!("{}", tone_sentence(tone));
    Ok(())
}
