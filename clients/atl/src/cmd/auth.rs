//! `atl login`, `atl logout`, `atl whoami`.

use atelier_core::api::{DeviceKind, DeviceStartRequest};
use atelier_core::config::normalize_studio;
use atelier_core::token::Place;
use atelier_core::{Client, ClientOptions, PollOptions};
use serde_json::json;

use crate::ctx::Ctx;
use crate::error::{CliError, CliResult};
use crate::ui::{outln, this_machine};
use crate::LoginArgs;

/// A name for this device: the host name without its domain.
async fn device_name() -> String {
    let from_env = std::env::var("COMPUTERNAME").ok();
    let name = match from_env {
        Some(name) => name,
        None => tokio::process::Command::new("hostname")
            .output()
            .await
            .map(|o| String::from_utf8_lossy(&o.stdout).into_owned())
            .unwrap_or_default(),
    };
    let name = name
        .trim()
        .split('.')
        .next()
        .unwrap_or_default()
        .to_string();
    if name.is_empty() {
        "atl".to_string()
    } else {
        name
    }
}

pub async fn login(ctx: &Ctx, args: LoginArgs) -> CliResult<()> {
    let ui = &ctx.ui;
    let studio = normalize_studio(&args.studio);
    let options = ClientOptions {
        ca_files: ctx.settings.ca_files(),
        ..ClientOptions::default()
    };
    let client = Client::new(&studio, None, options).map_err(|e| ctx.err(e))?;
    let name = match args.name {
        Some(name) => name,
        None => device_name().await,
    };

    let start = client
        .device_start(&DeviceStartRequest::new(&name, DeviceKind::Cli))
        .await
        .map_err(|e| ctx.err(e))?;
    let say = |line: String| {
        // With --json the conversation goes to standard error, the result to standard output.
        if ctx.json {
            eprintln!("{line}");
        } else {
            outln!("{line}");
        }
    };
    say(ui.muted(&format!("Found Atelier at {studio}")));

    let no_browser = args.no_browser
        || ctx.settings.env().get("ATELIER_NO_BROWSER").is_some()
        || !ctx.interactive;
    let mut opened = false;
    if !no_browser {
        say(format!(
            "Opening your browser to approve {}…",
            this_machine()
        ));
        opened = open::that_detached(&start.verify_url).is_ok();
    }
    if !opened {
        say(format!(
            "Open this in your browser to approve {}:",
            this_machine()
        ));
        say(format!("  {}", ui.amber(&start.verify_url)));
    }
    say(format!(
        "{}{}",
        ui.muted("  Approve with your passkey. Check the code matches: "),
        ui.amber_bold(&start.user_code)
    ));
    say(String::new());

    let grant = client
        .wait_for_device(&start, PollOptions::default())
        .await
        .map_err(|e| ctx.err(e))?;
    let tick = ui.green("✓ ");
    let person = if grant.person.name.is_empty() {
        grant.person.login.clone()
    } else {
        grant.person.name.clone()
    };
    say(format!("{tick}Approved as {person}"));

    // The token first: if anything after this fails, the sign-in still holds.
    let place = ctx
        .store
        .set(&studio, &grant.token)
        .map_err(|e| ctx.err(e))?;
    let client = client.with_token(grant.token.clone());
    // Device names are unique per person, so the studio may have made this
    // one "MacBook 2"; keep the name it knows us by.
    let name = match client.me().await {
        Ok(me) => me
            .device
            .map(|d| d.name)
            .filter(|n| !n.is_empty())
            .unwrap_or(name),
        Err(_) => name,
    };
    let mut settings = ctx.settings.clone();
    settings.config.studio = Some(studio.clone());
    settings.config.device_id = Some(grant.device_id);
    settings.config.device_name = Some(name.clone());
    settings.save().map_err(|e| ctx.err(e))?;

    let title = format!("{name} (atl)");
    let key =
        atelier_core::ssh::register_device_key(&client, &ctx.settings.ssh_dir(), &title).await;
    let (ssh_key, ssh_error) = match key {
        Ok(key) => {
            say(format!(
                "{tick}Added {}’s SSH key to your studio",
                this_machine()
            ));
            (Some(key), None)
        }
        Err(e) => {
            say(format!(
                "{}Couldn’t add an SSH key: {e} Cloning over HTTPS still works (--https).",
                ui.red("✗ ")
            ));
            (None, Some(e.to_string()))
        }
    };
    let stored = match &place {
        Place::Keychain => "in your keychain".to_string(),
        Place::File(path) => format!(
            "in {}, a file only you can read (no keychain in use)",
            path.display()
        ),
        Place::Memory => "in memory".to_string(),
    };
    say(format!("{tick}Saved a device token {stored}"));
    say(String::new());

    if ctx.json {
        crate::print_json(&json!({
            "studio": studio,
            "person": grant.person,
            "device": {"id": grant.device_id, "name": name, "kind": "cli"},
            "token_stored": match &place {
                Place::Keychain => "keychain",
                Place::File(_) => "file",
                Place::Memory => "memory",
            },
            "ssh_key": ssh_key.as_ref().map(|k| json!({
                "id": k.registered.id,
                "fingerprint": k.registered.fingerprint,
                "path": k.key.path,
                "generated": k.generated,
            })),
            "ssh_error": ssh_error,
        }));
        return Ok(());
    }
    outln!("You’re in. Try {}.", ui.amber("atl status"));
    outln!("{}", ui.muted("Your password was never typed here."));
    Ok(())
}

pub async fn logout(ctx: &Ctx) -> CliResult<()> {
    let Some(studio) = ctx.settings.studio() else {
        return Err(ctx.err(atelier_core::Error::NoStudio));
    };
    let had = ctx.store.get(&studio).map_err(|e| ctx.err(e))?.is_some();
    ctx.store.delete(&studio).map_err(|e| ctx.err(e))?;
    if ctx.json {
        crate::print_json(&json!({"studio": studio, "signed_out": had}));
        return Ok(());
    }
    if had {
        outln!(
            "Signed out of {} on {}.",
            ctx.studio_for_hint(),
            this_machine()
        );
        outln!(
            "{}",
            ctx.ui
                .muted("To cut the device off for good, revoke it in Settings → Devices.")
        );
    } else {
        outln!("You weren’t signed in on {}.", this_machine());
    }
    Ok(())
}

pub async fn whoami(ctx: &Ctx) -> CliResult<()> {
    let client = ctx.client()?;
    let me = client.me().await.map_err(|e| ctx.err(e))?;
    if ctx.json {
        crate::print_json(&me);
        return Ok(());
    }
    let ui = &ctx.ui;
    let studio_name = if me.studio.name.is_empty() {
        "your studio"
    } else {
        &me.studio.name
    };
    outln!(
        "{}{}",
        ui.bold(&format!("{} ({})", me.person.name, me.person.login)),
        ui.muted(&format!(" · {studio_name} at {}", client.studio_url()))
    );
    if let Some(device) = &me.device {
        outln!("{}{}", ui.muted("  Device    "), device.name);
    }
    for (index, account) in me.accounts.iter().enumerate() {
        let label = if index == 0 {
            "  Accounts  "
        } else {
            "            "
        };
        outln!("{}{}", ui.muted(label), account_label(account));
    }
    if !me.studio.version.is_empty() {
        outln!("{}{}", ui.muted("  Version   "), me.studio.version);
    }
    Ok(())
}

/// `DemiInfinity (personal)`, `Elyndra Studios LTD (company)`.
pub fn account_label(account: &atelier_core::api::Account) -> String {
    match account.kind.as_str() {
        "user" => format!("{} (personal)", account.name),
        kind => {
            let shown = if account.display.is_empty() {
                &account.name
            } else {
                &account.display
            };
            format!(
                "{shown} ({})",
                if kind.is_empty() { "account" } else { kind }
            )
        }
    }
}

/// Used where a command can't go on without a person or a `--yes`.
pub fn needs_yes(what: &str) -> CliError {
    CliError::plain(format!("There’s nobody here to ask about {what}."))
        .hint("Add --yes to go ahead without being asked.")
        .kind("needs_confirmation")
}
