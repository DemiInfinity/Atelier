//! `atl agent [list | approve ID]`.

use atelier_core::api::WaitingAgent;
use serde_json::json;

use crate::cmd::auth::needs_yes;
use crate::ctx::Ctx;
use crate::error::{CliError, CliResult};
use crate::ui::{column, count, outln, pad, short_repo, Ui};

/// `claude-code` → `Claude Code`; other engines as the studio names them.
fn engine_name(engine: &str) -> String {
    match engine {
        "claude-code" => "Claude Code".to_string(),
        "" => "not said".to_string(),
        other => other.to_string(),
    }
}

fn limits(agent: &WaitingAgent) -> String {
    let parts: Vec<String> = [
        agent.max_turns.map(|n| count(n, "turn", "turns")),
        agent.timeout_minutes.map(|n| count(n, "minute", "minutes")),
    ]
    .into_iter()
    .flatten()
    .collect();
    if parts.is_empty() {
        "none set".to_string()
    } else {
        parts.join(" · ")
    }
}

fn card(ui: &Ui, agent: &WaitingAgent) {
    let mut about = format!(" · {}", short_repo(&agent.repo));
    if !agent.reason.is_empty() {
        about.push_str(&format!(" · {}", agent.reason));
    }
    outln!("{}{}", ui.violet_bold(&agent.workflow), ui.muted(&about));
    let list = |tools: &[String]| {
        if tools.is_empty() {
            "nothing".to_string()
        } else {
            tools.join("  ")
        }
    };
    outln!("{}{}", ui.muted("  May use     "), list(&agent.tools));
    outln!("{}{}", ui.muted("  Can change  "), list(&agent.write_tools));
    outln!(
        "{}{}",
        ui.muted("  Engine      "),
        engine_name(&agent.engine)
    );
    outln!("{}{}", ui.muted("  Limits      "), limits(agent));
}

pub async fn list(ctx: &Ctx) -> CliResult<()> {
    let client = ctx.client()?;
    let jobs = client.agents_waiting().await.map_err(|e| ctx.err(e))?;
    if ctx.json {
        crate::print_json(&json!({ "jobs": jobs }));
        return Ok(());
    }
    let ui = &ctx.ui;
    if jobs.is_empty() {
        outln!("No agents are waiting for you.");
        return Ok(());
    }
    let ids: Vec<String> = jobs.iter().map(|j| j.job_id.to_string()).collect();
    let id_width = column(ids.iter().map(String::as_str), 12);
    let name_width = column(jobs.iter().map(|j| j.workflow.as_str()), 32);
    let about: Vec<String> = jobs
        .iter()
        .map(|j| {
            [short_repo(&j.repo), j.reason.as_str()]
                .into_iter()
                .filter(|s| !s.is_empty())
                .collect::<Vec<_>>()
                .join("  ")
        })
        .collect();
    let about_width = column(about.iter().map(String::as_str), 44);
    for ((job, id), about) in jobs.iter().zip(&ids).zip(&about) {
        outln!(
            "{}{}  {}  {}  {}",
            ui.violet("◆ "),
            pad(id, id_width),
            pad(&job.workflow, name_width),
            ui.muted(&pad(about, about_width)),
            ui.amber(&format!("atl agent approve {id}"))
        );
    }
    Ok(())
}

pub async fn approve(ctx: &Ctx, id: u64, yes: bool, follow: bool) -> CliResult<()> {
    let client = ctx.client()?;
    let ui = &ctx.ui;
    let waiting = client.agents_waiting().await.map_err(|e| ctx.err(e))?;
    let Some(agent) = waiting.iter().find(|j| j.job_id == id) else {
        return Err(
            CliError::plain(format!("No agent with id {id} is waiting."))
                .hint("See who is with atl agent list.")
                .kind("not_waiting"),
        );
    };
    if !ctx.json {
        card(ui, agent);
        outln!();
    }
    if !yes {
        if !ctx.interactive {
            return Err(needs_yes("letting this agent run"));
        }
        if !crate::ui::confirm("Approve and run?", ui) {
            outln!("Left waiting.");
            return Ok(());
        }
    }
    let approved = client.approve_agent(id).await.map_err(|e| ctx.err(e))?;
    if ctx.json {
        crate::print_json(&json!({"job_id": id, "approved": approved.approved, "agent": agent}));
        return Ok(());
    }
    if !approved.approved {
        return Err(
            CliError::plain("Your studio didn’t approve it.").hint(format!(
                "Look at it in the browser: {}",
                client.studio_url()
            )),
        );
    }
    outln!(
        "{}Approved. {} can run now.",
        ui.green("✓ "),
        agent.workflow
    );
    if follow {
        outln!();
        return crate::cmd::run::follow(ctx, &client, id).await;
    }
    outln!(
        "{}{}",
        ui.muted("  Watch it with "),
        ui.amber("atl run watch")
    );
    Ok(())
}
