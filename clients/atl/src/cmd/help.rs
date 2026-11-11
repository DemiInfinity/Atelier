//! `atl` on its own: the short overview.

use serde_json::json;

use crate::ctx::Ctx;
use crate::error::CliResult;
use crate::ui::outln;

const COMMANDS: [(&str, &str); 9] = [
    ("atl status", "what needs you, across your studio"),
    ("atl repo publish", "turn this folder into a repository"),
    ("atl repo clone NAME", "clone from your studio or GitHub"),
    ("atl pr", "pull requests for this repository"),
    ("atl run", "jobs and agents, live"),
    ("atl agent approve", "let a waiting agent run"),
    ("atl open", "open this repository in your editor"),
    ("atl remote on|off", "reach your studio from outside"),
    ("atl say", "your pop-up messages"),
];

pub fn overview(ctx: &Ctx) -> CliResult<()> {
    let studio = ctx.settings.studio();
    if ctx.json {
        crate::print_json(&json!({
            "studio": studio,
            "version": env!("CARGO_PKG_VERSION"),
            "commands": COMMANDS.iter().map(|(c, d)| json!({"command": c, "about": d})).collect::<Vec<_>>(),
        }));
        return Ok(());
    }
    let ui = &ctx.ui;
    match &studio {
        Some(_) => outln!(
            "{}{}",
            ui.amber_bold("Atelier"),
            ui.muted(&format!(" · your studio at {}", ctx.studio_for_hint()))
        ),
        None => outln!(
            "{}{}",
            ui.amber_bold("Atelier"),
            ui.muted(" · not signed in yet. Start with atl login <studio>.")
        ),
    }
    outln!();
    for (command, about) in COMMANDS {
        outln!("{}{about}", ui.amber(&format!("  {command:<22}")));
    }
    outln!();
    outln!(
        "{}atl <command> --help{}--json{}",
        ui.muted("Run "),
        ui.muted(" for details. Everything also takes "),
        ui.muted(".")
    );
    Ok(())
}
