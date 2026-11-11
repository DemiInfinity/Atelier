//! `atl run [list | log ID | watch]`.

use std::time::Duration;

use atelier_core::api::{Run, RunningJob, RunsQuery};
use atelier_core::{Client, Event};
use serde_json::json;

use crate::ctx::Ctx;
use crate::error::{CliError, CliResult};
use crate::ui::{column, duration, outln, pad, short_repo, Ui};
use crate::RunListArgs;

/// A coloured mark for a run or job status, as the studio names them.
pub fn mark(ui: &Ui, status: &str) -> String {
    match status {
        "success" | "passed" | "completed" => ui.green("✓"),
        "failure" | "failed" | "error" | "timed_out" => ui.red("✗"),
        "running" | "pending" | "queued" | "in_progress" => ui.amber("◐"),
        "waiting" | "waiting_approval" => ui.violet("◆"),
        _ => ui.muted("–"),
    }
}

fn is_finished(status: &str) -> bool {
    !matches!(
        status,
        "running" | "pending" | "queued" | "in_progress" | "waiting" | "waiting_approval"
    )
}

fn failed(status: &str) -> bool {
    matches!(status, "failure" | "failed" | "error" | "timed_out")
}

pub async fn list(ctx: &Ctx, args: RunListArgs) -> CliResult<()> {
    let client = ctx.client()?;
    let runs = client
        .runs(&RunsQuery {
            limit: Some(args.limit),
            runner: args.runner,
            repo: args.repo,
        })
        .await
        .map_err(|e| ctx.err(e))?;
    if ctx.json {
        crate::print_json(&json!({ "runs": runs }));
        return Ok(());
    }
    let ui = &ctx.ui;
    if runs.is_empty() {
        outln!("No runs yet.");
        return Ok(());
    }
    let id_width = column(
        runs.iter()
            .map(|r| r.run_id.to_string())
            .collect::<Vec<_>>()
            .iter()
            .map(String::as_str),
        12,
    );
    let workflow_width = column(runs.iter().map(|r| r.workflow.as_str()), 28);
    let what: Vec<String> = runs
        .iter()
        .map(|r| {
            [r.event.as_str(), r.branch.as_str()]
                .into_iter()
                .filter(|s| !s.is_empty())
                .collect::<Vec<_>>()
                .join(" · ")
        })
        .collect();
    let what_width = column(what.iter().map(String::as_str), 40);
    let repo_width = column(runs.iter().map(|r| short_repo(&r.repo)), 20);
    for (run, what) in runs.iter().zip(&what) {
        let took = run.duration_s.map(duration).unwrap_or_default();
        let line = format!(
            "{} {}  {}  {}",
            mark(ui, &run.status),
            ui.amber(&pad(&run.run_id.to_string(), id_width)),
            pad(&run.workflow, workflow_width),
            ui.muted(&format!(
                "{}  {}  {took}",
                pad(what, what_width),
                pad(short_repo(&run.repo), repo_width)
            ))
        );
        outln!("{}", line.trim_end());
        // One job named like the run says nothing new; several do.
        if run.jobs.len() > 1 {
            let name_width = column(run.jobs.iter().map(|j| j.name.as_str()), 28);
            let runner_width = column(run.jobs.iter().map(|j| j.runner.as_str()), 28);
            for job in &run.jobs {
                let took = job.duration_s.map(duration).unwrap_or_default();
                let line = format!(
                    "    {} {}  {}",
                    mark(ui, &job.status),
                    pad(&job.name, name_width),
                    ui.muted(&format!("{}  {took}", pad(&job.runner, runner_width)))
                );
                outln!("{}", line.trim_end());
            }
        }
    }
    Ok(())
}

/// `atl run log ID`: ID is a run id (what `atl status` and `atl run list`
/// show); the logs of its failed jobs are printed, or of all its jobs when
/// none failed. With `--job`, or when no run has that id, ID is a job id.
pub async fn log(ctx: &Ctx, id: u64, tail: u32, as_job: bool) -> CliResult<()> {
    let client = ctx.client()?;
    let ui = &ctx.ui;
    let run: Option<Run> = if as_job {
        None
    } else {
        client
            .runs(&RunsQuery {
                limit: Some(100),
                ..RunsQuery::default()
            })
            .await
            .map_err(|e| ctx.err(e))?
            .into_iter()
            .find(|r| r.run_id == id)
    };
    // (job id, heading)
    let jobs: Vec<(u64, Option<String>)> = match &run {
        Some(run) if !run.jobs.is_empty() => {
            let any_failed = run.jobs.iter().any(|j| failed(&j.status));
            run.jobs
                .iter()
                .filter(|j| !any_failed || failed(&j.status))
                .map(|j| {
                    let heading = format!("{} · {}", run.workflow, j.name);
                    (j.job_id, Some(heading))
                })
                .collect()
        }
        _ => vec![(id, None)],
    };

    let mut collected = Vec::new();
    for (job_id, heading) in &jobs {
        let log = client.job_log(*job_id, Some(tail)).await.map_err(|e| {
            let missing = e.status() == Some(404);
            let error = ctx.err(e);
            if missing {
                CliError::plain(format!("There is no run or job with id {id}."))
                    .hint("See recent ones with atl run list.")
                    .kind("not_found")
            } else {
                error
            }
        })?;
        if ctx.json {
            collected.push(json!({
                "job_id": job_id, "status": log.status, "truncated": log.truncated, "lines": log.lines,
            }));
            continue;
        }
        if jobs.len() > 1 || heading.is_some() {
            let heading = heading.clone().unwrap_or_else(|| format!("job {job_id}"));
            outln!(
                "{} {}{}",
                mark(ui, &log.status),
                ui.bold(&heading),
                ui.muted(&format!("  job {job_id} · {}", log.status))
            );
        }
        if log.truncated {
            outln!(
                "{}",
                ui.muted(&format!(
                    "  … earlier lines left out (--tail N for more than {tail})"
                ))
            );
        }
        for line in &log.lines {
            outln!("{line}");
        }
    }
    if ctx.json {
        crate::print_json(&json!({"run_id": run.map(|r| r.run_id), "jobs": collected}));
    }
    Ok(())
}

fn print_running(ui: &Ui, running: &[RunningJob]) {
    if running.is_empty() {
        outln!("{}", ui.muted("Nothing is running."));
        return;
    }
    let width = column(running.iter().map(|r| r.workflow.as_str()), 40);
    for job in running {
        let diamond = if job.kind == "agent" {
            ui.violet("◆ ")
        } else {
            ui.amber("◆ ")
        };
        let rest: Vec<&str> = [
            short_repo(&job.repo),
            job.detail.as_str(),
            job.runner.as_str(),
        ]
        .into_iter()
        .filter(|s| !s.is_empty())
        .collect();
        outln!(
            "{diamond}{}  {}",
            pad(&job.workflow, width),
            ui.muted(&rest.join(" · "))
        );
    }
}

/// Shows what is running and again whenever the studio says it changed.
/// With `--json`, one JSON object per line.
pub async fn watch(ctx: &Ctx, limit: Option<u64>) -> CliResult<()> {
    let client = ctx.client()?;
    let ui = &ctx.ui;
    let show = |client: Client| async move { client.status().await.map(|s| s.running) };
    let print = |running: &[RunningJob]| {
        if ctx.json {
            crate::ui::outln!("{}", json!({"running": running}));
        } else {
            print_running(ui, running);
        }
    };
    print(&show(client.clone()).await.map_err(|e| ctx.err(e))?);

    let mut events = client.events();
    let mut seen = 0u64;
    while limit.is_none_or(|n| seen < n) {
        let event = tokio::select! {
            event = events.next() => event.map_err(|e| ctx.err(e))?,
            _ = tokio::signal::ctrl_c() => break,
        };
        match event {
            Event::Status(changed) if changed.changed.iter().any(|c| c == "running") => {
                seen += 1;
                if !ctx.json {
                    outln!();
                }
                // The change and the event can race; a moment's grace is cheap.
                tokio::time::sleep(Duration::from_millis(100)).await;
                print(&show(client.clone()).await.map_err(|e| ctx.err(e))?);
            }
            Event::Notify(notify) => {
                seen += 1;
                if ctx.json {
                    crate::ui::outln!("{}", json!({"notify": notify}));
                } else {
                    let mark = match notify.kind.as_str() {
                        "run_failed" => ui.red("✗ "),
                        _ => ui.green("● "),
                    };
                    outln!(
                        "{mark}{}  {}",
                        notify.title,
                        ui.muted(&[short_repo(&notify.repo), notify.detail.as_str()].join("  "))
                    );
                }
            }
            _ => {}
        }
    }
    Ok(())
}

/// Prints a job's log as it grows, until the job finishes.
pub async fn follow(ctx: &Ctx, client: &Client, job_id: u64) -> CliResult<()> {
    let mut printed: Vec<String> = Vec::new();
    loop {
        let log = client
            .job_log(job_id, Some(500))
            .await
            .map_err(|e| ctx.err(e))?;
        // Print what is new: the lines after the longest overlap with what
        // was already shown (the log is a sliding tail).
        let overlap = (0..=printed.len().min(log.lines.len()))
            .rev()
            .find(|&n| printed[printed.len() - n..] == log.lines[..n])
            .unwrap_or(0);
        for line in &log.lines[overlap..] {
            outln!("{line}");
        }
        printed = log.lines;
        if is_finished(&log.status) {
            let ui = &ctx.ui;
            outln!("{} {}", mark(ui, &log.status), log.status);
            return Ok(());
        }
        tokio::time::sleep(Duration::from_secs(2)).await;
    }
}
