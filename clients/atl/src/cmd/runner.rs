//! `atl runner [status | pause | resume]`: the runner on this machine,
//! through its local control socket.

use atelier_core::runner::{PausedReason, RunnerStatus};
use atelier_core::{Error, RunnerClient};
use serde_json::json;

use crate::ctx::Ctx;
use crate::error::CliResult;
use crate::ui::{column, count, outln, pad, short_repo, this_machine, Ui};

pub fn client(ctx: &Ctx) -> RunnerClient {
    // An explicit socket wins; otherwise the per-OS default for the config.
    if ctx
        .settings
        .env()
        .get(atelier_core::config::ENV_RUNNER_SOCKET)
        .is_some()
    {
        RunnerClient::new(ctx.settings.runner_socket())
    } else {
        RunnerClient::for_config(&ctx.settings.runner_config_path())
    }
}

fn state(status: &RunnerStatus) -> &'static str {
    match (status.taking_jobs, status.paused_reason) {
        (true, _) => "taking jobs",
        (false, Some(PausedReason::User)) => "paused by you",
        (false, Some(PausedReason::Battery)) => "paused while on battery",
        (false, Some(PausedReason::JobsOff)) => "jobs are switched off",
        (false, _) => "not taking jobs",
    }
}

fn print(ui: &Ui, status: &RunnerStatus) {
    let online = if status.online { "online" } else { "offline" };
    let state = state(status);
    outln!(
        "{}{}{}",
        ui.bold(&status.name),
        ui.muted(&format!(" · {online} · ")),
        if status.taking_jobs {
            ui.green(state)
        } else {
            ui.amber(state)
        }
    );
    if !status.server.is_empty() {
        outln!("{}{}", ui.muted("  Studio   "), status.server);
    }
    let rules = &status.rules;
    let rules = [
        if rules.pause_on_battery {
            "pauses on battery"
        } else {
            "runs on battery"
        }
        .to_string(),
        format!("{} at a time", rules.max_parallel),
        if rules.container_jobs {
            "container jobs"
        } else {
            "no container jobs"
        }
        .to_string(),
    ];
    outln!("{}{}", ui.muted("  Rules    "), rules.join(" · "));
    if status.current.is_empty() {
        outln!("{}{}", ui.muted("  Running  "), "nothing");
        return;
    }
    let width = column(status.current.iter().map(|j| j.workflow.as_str()), 32);
    for (index, job) in status.current.iter().enumerate() {
        let label = if index == 0 {
            "  Running  "
        } else {
            "           "
        };
        let diamond = if job.kind == "agent" {
            ui.violet("◆ ")
        } else {
            ui.amber("◆ ")
        };
        let rest: Vec<&str> = [short_repo(&job.repo), job.detail.as_str()]
            .into_iter()
            .filter(|s| !s.is_empty())
            .collect();
        outln!(
            "{}{diamond}{}  {}",
            ui.muted(label),
            pad(&job.workflow, width),
            ui.muted(&rest.join(" · "))
        );
    }
}

/// "Not running" is an answer here, not a failure: exit 0, said plainly.
pub async fn status(ctx: &Ctx) -> CliResult<()> {
    match client(ctx).status().await {
        Ok(status) => {
            if ctx.json {
                let mut value = serde_json::to_value(&status).unwrap_or_default();
                value["ok"] = true.into();
                crate::print_json(&value);
            } else {
                print(&ctx.ui, &status);
            }
            Ok(())
        }
        Err(Error::RunnerNotRunning) => {
            if ctx.json {
                // The same object `atelier-runner status --json` prints.
                crate::print_json(&json!({"ok": false, "error": "not running"}));
            } else {
                outln!("The runner isn’t running on {}.", this_machine());
                outln!(
                    "{}{}",
                    ctx.ui.muted("Start it with "),
                    ctx.ui.amber("atelier-runner run")
                );
            }
            Ok(())
        }
        Err(e) => Err(ctx.err(e)),
    }
}

pub async fn set(ctx: &Ctx, taking_jobs: bool) -> CliResult<()> {
    let status = client(ctx)
        .set_taking_jobs(taking_jobs)
        .await
        .map_err(|e| ctx.err(e))?;
    if ctx.json {
        let mut value = serde_json::to_value(&status).unwrap_or_default();
        value["ok"] = true.into();
        crate::print_json(&value);
        return Ok(());
    }
    let ui = &ctx.ui;
    match (taking_jobs, status.taking_jobs) {
        (false, _) => {
            outln!("Paused. {} won’t take new jobs.", status.name);
            if !status.current.is_empty() {
                outln!(
                    "{}",
                    ui.muted(&format!(
                        "  {} already running will finish.",
                        count(status.current.len() as u64, "job", "jobs")
                    ))
                );
            }
        }
        (true, true) => outln!("{} is taking jobs again.", status.name),
        // Resumed by you, but something else still holds it back.
        (true, false) => outln!(
            "Resumed, but {} still isn’t taking jobs: {}.",
            status.name,
            state(&status)
        ),
    }
    Ok(())
}
