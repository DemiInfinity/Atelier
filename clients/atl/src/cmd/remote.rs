//! `atl remote [status | on | off | who | link]`.

use std::time::Duration;

use atelier_core::api::Remote;
use serde_json::json;

use crate::ctx::Ctx;
use crate::error::{CliError, CliResult};
use crate::ui::{column, count, duration, outln, pad, since, Ui};

/// `["web","git"]` → `Web and git only.`
fn exposed(remote: &Remote) -> String {
    let mut words = remote.exposed.join(" and ");
    if words.is_empty() {
        return String::new();
    }
    if let Some(first) = words.get(..1) {
        words = format!("{}{}", first.to_uppercase(), &words[1..]);
    }
    format!(" {words} only.")
}

fn print_on(ui: &Ui, remote: &Remote) {
    outln!("{}", ui.red_bold("Remote access is on."));
    match &remote.address {
        Some(address) => {
            outln!("{}", ui.amber(&format!("  {address}")));
            outln!(
                "{}",
                ui.muted(&format!(
                    "  Sign in there with a passkey.{}",
                    exposed(remote)
                ))
            );
            outln!(
                "{}{}",
                ui.muted("  Need a passkey for that address? "),
                ui.amber("atl remote link")
            );
        }
        None => outln!(
            "{}{}",
            ui.muted("  The address is still being set up. In a moment: "),
            ui.amber("atl remote status")
        ),
    }
}

pub async fn status(ctx: &Ctx) -> CliResult<()> {
    let client = ctx.client()?;
    let remote = client.remote().await.map_err(|e| ctx.err(e))?;
    if ctx.json {
        crate::print_json(&remote);
        return Ok(());
    }
    let ui = &ctx.ui;
    if remote.on {
        print_on(ui, &remote);
        outln!(
            "{}",
            ui.muted(&format!(
                "  {} connected from outside.",
                count(remote.connected, "session", "sessions")
            ))
        );
    } else {
        outln!("{}", ui.green_bold("Remote access is off."));
        outln!("{}", ui.muted("Your studio is home only."));
    }
    Ok(())
}

pub async fn on(ctx: &Ctx) -> CliResult<()> {
    let client = ctx.client()?;
    let mut remote = client.set_remote(true).await.map_err(|e| ctx.err(e))?;
    // The address can take a few seconds to exist.
    for _ in 0..10 {
        if remote.address.is_some() || !remote.on {
            break;
        }
        tokio::time::sleep(Duration::from_secs(1)).await;
        remote = client.remote().await.map_err(|e| ctx.err(e))?;
    }
    if ctx.json {
        crate::print_json(&remote);
        return Ok(());
    }
    if !remote.on {
        return Err(CliError::plain("Remote access didn’t come on.")
            .hint("Look at Settings → Remote access in your studio."));
    }
    print_on(&ctx.ui, &remote);
    Ok(())
}

pub async fn off(ctx: &Ctx) -> CliResult<()> {
    let client = ctx.client()?;
    // How many are about to be signed out; fine if we can't tell.
    let before = client.remote().await.ok();
    let remote = client.set_remote(false).await.map_err(|e| ctx.err(e))?;
    let signed_out = before.filter(|b| b.on).map(|b| b.connected).unwrap_or(0);
    if ctx.json {
        crate::print_json(&json!({
            "on": remote.on, "mode": remote.mode, "address": remote.address,
            "exposed": remote.exposed, "connected": remote.connected, "signed_out": signed_out,
        }));
        return Ok(());
    }
    let ui = &ctx.ui;
    let tail = match signed_out {
        0 => String::new(),
        n => format!(" {} signed out.", count(n, "session", "sessions")),
    };
    outln!("{}{tail}", ui.green_bold("Remote access is off."));
    outln!("{}", ui.muted("Your studio is home only again."));
    Ok(())
}

pub async fn who(ctx: &Ctx) -> CliResult<()> {
    let client = ctx.client()?;
    let sessions = client.remote_who().await.map_err(|e| ctx.err(e))?;
    if ctx.json {
        crate::print_json(&json!({ "sessions": sessions }));
        return Ok(());
    }
    if sessions.is_empty() {
        outln!("Nobody is connected from outside.");
        return Ok(());
    }
    let now = time::OffsetDateTime::now_utc();
    let ip_width = column(sessions.iter().map(|s| s.ip.as_str()), 40);
    let signed = |session: &atelier_core::api::RemoteSession| {
        if session.signed_in {
            "signed in"
        } else {
            "not signed in"
        }
    };
    let signed_width = column(sessions.iter().map(signed), 13);
    let class_width = column(sessions.iter().map(|s| s.class.as_str()), 8);
    for session in &sessions {
        let line = format!(
            "  {}   {}   {}   {}   {}",
            pad(&session.ip, ip_width),
            pad(&session.country, 2),
            pad(signed(session), signed_width),
            pad(&session.class, class_width),
            since(&session.since, now)
        );
        outln!("{}", line.trim_end());
    }
    Ok(())
}

pub async fn link(ctx: &Ctx) -> CliResult<()> {
    let client = ctx.client()?;
    let link = client.remote_link().await.map_err(|e| {
        let error = ctx.err(e);
        if error.hint.is_none() {
            error.hint("Links are only made at home, while remote access is on (atl remote on).")
        } else {
            error
        }
    })?;
    if ctx.json {
        crate::print_json(&link);
        return Ok(());
    }
    let ui = &ctx.ui;
    outln!("{}", ui.amber(&link.url));
    outln!(
        "{}",
        ui.muted(&format!(
            "Works once, for the next {}. Open it on the device that needs a passkey.",
            duration(link.expires_in).replace(" 00s", "")
        ))
    );
    Ok(())
}
