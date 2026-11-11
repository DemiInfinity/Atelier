//! `atl status`: what needs you, what is running, where you are.

use atelier_core::api::{FeedItem, Need, NeedKind, Repo, RunningJob, Thread};
use atelier_core::git::LocalRepo;
use serde::Serialize;

use crate::ctx::{Ctx, SCAN_DEPTH};
use crate::error::CliResult;
use crate::ui::{column, count, outln, pad, short_repo, truncate, Ui};

/// A working copy with commits its studio doesn't have yet.
#[derive(Serialize)]
struct Unpushed {
    repo: String,
    path: std::path::PathBuf,
    branch: Option<String>,
    ahead: u64,
}

#[derive(Serialize)]
struct Here {
    /// The studio repository, when this folder belongs to one.
    repo: Option<String>,
    path: std::path::PathBuf,
    branch: Option<String>,
    upstream: Option<String>,
    changed_files: u64,
    ahead: Option<u64>,
    behind: Option<u64>,
}

#[derive(Serialize)]
struct Output<'a> {
    needs: &'a [Need],
    unpushed: &'a [Unpushed],
    running: &'a [RunningJob],
    here: Option<Here>,
    feed: &'a [FeedItem],
    unread: u64,
}

/// The command that deals with a need.
fn hint(need: &Need, here_repo: Option<&str>, client: &atelier_core::Client) -> String {
    let Some(action) = &need.action else {
        return String::new();
    };
    match action.kind.as_str() {
        "approve_agent" => action
            .job_id
            .map(|id| format!("atl agent approve {id}"))
            .unwrap_or_default(),
        "open_run" => action
            .run_id
            .map(|id| format!("atl run log {id}"))
            .unwrap_or_default(),
        "open_pull" => match action.number {
            Some(n) if here_repo == Some(need.repo.as_str()) || need.repo.is_empty() => {
                format!("atl pr {n}")
            }
            Some(n) => format!("atl pr {n} -R {}", need.repo),
            None => String::new(),
        },
        // Nothing to do from here but look at it in the browser.
        "open_web" if !need.web_path.is_empty() => client.web_url(&need.web_path),
        _ => String::new(),
    }
}

/// The studio doesn't know agent turns; the runner on this machine does.
/// Adds what the runner says (`turn 3 of 20`) to the jobs it is running.
async fn add_runner_details(ctx: &Ctx, running: &mut [RunningJob]) {
    if running.is_empty() {
        return;
    }
    let runner = crate::cmd::runner::client(ctx).with_timeout(std::time::Duration::from_secs(1));
    let Ok(status) = runner.status().await else {
        return;
    };
    for job in running {
        let Some(local) = status.current.iter().find(|c| c.job_id == job.job_id) else {
            continue;
        };
        if local.detail.is_empty() || job.detail.contains(&local.detail) {
            continue;
        }
        job.detail = if job.detail.is_empty() {
            local.detail.clone()
        } else {
            format!("{} · {}", job.detail, local.detail)
        };
    }
}

fn bullet(ui: &Ui, need: &Need) -> String {
    let dot = "  ● ";
    match (need.thread, need.kind) {
        (_, NeedKind::RunFailed | NeedKind::SecurityAlert) => ui.red(dot),
        (Thread::Agents | Thread::Connect, _) => ui.violet(dot),
        (Thread::Github, _) => ui.blue(dot),
        _ => ui.amber(dot),
    }
}

struct Row {
    bullet: String,
    title: String,
    repo: String,
    detail: String,
    hint: String,
}

fn print_rows(ui: &Ui, rows: &[Row]) {
    let title_width = column(rows.iter().map(|r| r.title.as_str()), 44);
    let repo_width = column(rows.iter().map(|r| r.repo.as_str()), 20);
    let detail_width = column(rows.iter().map(|r| r.detail.as_str()), 48);
    for row in rows {
        let middle = format!(
            "{}  {}",
            pad(&truncate(&row.repo, repo_width), repo_width),
            pad(&truncate(&row.detail, detail_width), detail_width)
        );
        let line = format!(
            "{}{}  {}  {}",
            row.bullet,
            pad(&truncate(&row.title, title_width), title_width),
            ui.muted(&middle),
            ui.amber(&row.hint)
        );
        outln!("{}", line.trim_end());
    }
}

fn here_line(here: &LocalRepo, in_studio: bool) -> String {
    let mut parts = vec![match here.changed_files {
        0 => "no changes".to_string(),
        n => count(n, "changed file", "changed files"),
    }];
    let upstream = if in_studio {
        "your studio".to_string()
    } else {
        here.upstream.clone().unwrap_or_default()
    };
    match (here.ahead, here.behind) {
        (Some(0), Some(0)) => parts.push(format!("up to date with {upstream}")),
        (Some(ahead), Some(behind)) => {
            if ahead > 0 {
                parts.push(format!(
                    "{} ahead of {upstream}",
                    count(ahead, "commit", "commits")
                ));
            }
            if behind > 0 {
                parts.push(if ahead > 0 {
                    format!("{behind} behind")
                } else {
                    format!("{} behind {upstream}", count(behind, "commit", "commits"))
                });
            }
        }
        _ if here.branch.is_none() => parts.push("not on a branch".to_string()),
        _ => parts.push("this branch isn’t pushed yet".to_string()),
    }
    parts.join(" · ")
}

pub async fn status(ctx: &Ctx) -> CliResult<()> {
    let client = ctx.client()?;
    let (status, repos) = tokio::join!(client.status(), client.repos());
    let mut status = status.map_err(|e| ctx.err(e))?;
    add_runner_details(ctx, &mut status.running).await;
    // The repository list only dresses the local part; do without it if need be.
    let repos: Vec<Repo> = repos.unwrap_or_default();

    let here = ctx.here().await.unwrap_or(None);
    let here_repo = here.as_ref().and_then(|h| h.studio_repo(&repos));

    // "Commits not pushed" is local: the studio can't know.
    let mut locals = ctx.git().scan(&ctx.settings.repo_roots(), SCAN_DEPTH).await;
    if let Some(here) = &here {
        if !locals.iter().any(|l| l.path == here.path) {
            locals.push(here.clone());
        }
    }
    let unpushed: Vec<Unpushed> = locals
        .iter()
        .filter_map(|local| {
            let repo = local.studio_repo(&repos)?;
            let ahead = local.ahead.filter(|&a| a > 0)?;
            Some(Unpushed {
                repo: repo.full_name.clone(),
                path: local.path.clone(),
                branch: local.branch.clone(),
                ahead,
            })
        })
        .collect();

    if ctx.json {
        crate::print_json(&Output {
            needs: &status.needs,
            unpushed: &unpushed,
            running: &status.running,
            here: here.as_ref().map(|h| Here {
                repo: here_repo.map(|r| r.full_name.clone()),
                path: h.path.clone(),
                branch: h.branch.clone(),
                upstream: h.upstream.clone(),
                changed_files: h.changed_files,
                ahead: h.ahead,
                behind: h.behind,
            }),
            feed: &status.feed,
            unread: status.unread,
        });
        return Ok(());
    }

    let ui = &ctx.ui;
    let here_name = here_repo.map(|r| r.full_name.as_str());
    let mut rows: Vec<Row> = status
        .needs
        .iter()
        .map(|need| Row {
            bullet: bullet(ui, need),
            title: need.title.clone(),
            repo: short_repo(&need.repo).to_string(),
            detail: need.detail.clone(),
            hint: hint(need, here_name, &client),
        })
        .collect();
    // The folder you're in gets its own "Here" section below.
    let elsewhere = unpushed
        .iter()
        .filter(|u| here.as_ref().is_none_or(|h| h.path != u.path));
    rows.extend(elsewhere.map(|u| Row {
        bullet: ui.amber("  ● "),
        title: format!("{} not pushed", count(u.ahead, "commit", "commits")),
        repo: short_repo(&u.repo).to_string(),
        detail: u.branch.clone().unwrap_or_default(),
        hint: "git push".to_string(),
    }));

    if rows.is_empty() {
        outln!("{}", ui.bold("Nothing needs you."));
    } else {
        outln!("{}", ui.bold("Needs you"));
        print_rows(ui, &rows);
    }

    if !status.running.is_empty() {
        outln!();
        outln!("{}", ui.bold("Running"));
        let width = column(status.running.iter().map(|r| r.workflow.as_str()), 44);
        for job in &status.running {
            let diamond = if job.kind == "agent" {
                ui.violet("  ◆ ")
            } else {
                ui.amber("  ◆ ")
            };
            let mut detail = vec![short_repo(&job.repo).to_string()];
            let mut rest: Vec<&str> = Vec::new();
            rest.extend(
                [job.detail.as_str(), job.runner.as_str()]
                    .iter()
                    .filter(|s| !s.is_empty()),
            );
            if !rest.is_empty() {
                detail.push(rest.join(" · "));
            }
            outln!(
                "{diamond}{}  {}",
                pad(&truncate(&job.workflow, width), width),
                ui.muted(&detail.join("  "))
            );
        }
    }

    if let Some(here) = &here {
        outln!();
        let name = here_repo
            .map(|r| r.name.clone())
            .unwrap_or_else(|| here.folder_name());
        let branch = here.branch.as_deref().unwrap_or("detached");
        outln!(
            "{}{}",
            ui.bold("Here"),
            ui.muted(&format!("  {name} · {branch}"))
        );
        outln!("  {}", here_line(here, here_repo.is_some()));
        if here_repo.is_none() {
            outln!(
                "  {}{}",
                ui.muted("Not in your studio yet: "),
                ui.amber("atl repo publish")
            );
        }
    }

    for (thread, title, via) in [
        (Thread::Github, "From GitHub", "via Tether"),
        (Thread::Connect, "From Connect", "via Connect"),
    ] {
        let items: Vec<&FeedItem> = status.feed.iter().filter(|f| f.thread == thread).collect();
        if items.is_empty() {
            continue;
        }
        let paint = |text: &str, bold: bool| match (thread, bold) {
            (Thread::Github, true) => ui.blue_bold(text),
            (Thread::Github, false) => ui.blue(text),
            (_, true) => ui.violet_bold(text),
            (_, false) => ui.violet(text),
        };
        outln!();
        outln!("{}", paint(title, true));
        for item in items {
            outln!("{}{}  {}", paint("  ● ", false), item.text, ui.muted(via));
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn local(ahead: Option<u64>, behind: Option<u64>, changed: u64) -> LocalRepo {
        LocalRepo {
            path: "/x/Atelier".into(),
            branch: Some("main".into()),
            upstream: ahead.map(|_| "origin/main".to_string()),
            ahead,
            behind,
            changed_files: changed,
            remote_name: None,
            remote_url: None,
            remote: None,
        }
    }

    #[test]
    fn the_here_line_reads_like_a_sentence() {
        assert_eq!(
            here_line(&local(Some(1), Some(0), 3), true),
            "3 changed files · 1 commit ahead of your studio"
        );
        assert_eq!(
            here_line(&local(Some(0), Some(0), 0), true),
            "no changes · up to date with your studio"
        );
        assert_eq!(
            here_line(&local(Some(2), Some(1), 1), true),
            "1 changed file · 2 commits ahead of your studio · 1 behind"
        );
        assert_eq!(
            here_line(&local(Some(0), Some(4), 0), false),
            "no changes · 4 commits behind origin/main"
        );
        assert_eq!(
            here_line(&local(None, None, 0), true),
            "no changes · this branch isn’t pushed yet"
        );
    }
}
