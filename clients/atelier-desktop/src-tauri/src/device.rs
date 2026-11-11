//! "This device": the runner on this machine through its control socket,
//! plus what the studio knows about it (waiting agents, earlier runs).

use std::time::Duration;

use atelier_core::api::{Run, RunsQuery, WaitingAgent};
use atelier_core::runner::{RunnerRulesUpdate, RunnerStatus};
use serde::Serialize;

use crate::desk::Desk;
use crate::error::{CmdError, CmdResult};
use crate::notify::short_repo;
use crate::pulls::Badge;
use crate::today::{engine_label, running_rows, RunningRow};
use crate::tools;

#[derive(Clone, Debug, Serialize)]
pub struct LiveJob {
    #[serde(flatten)]
    pub row: RunningRow,
    /// The last lines of its transcript or log.
    pub tail: Vec<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct WaitingRow {
    pub job_id: u64,
    /// `weekly-tidy wants to run for the first time`.
    pub title: String,
    /// `Atelier · it would use issues.list, issues.comment and issues.close`.
    pub detail: String,
    /// `Claude Code · up to 20 turns · 10 minutes`.
    pub limits: String,
    pub web_path: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct RunRow {
    pub status: Badge,
    /// `ci · test`.
    pub name: String,
    /// `Atelier · feature/tunnel-ui`.
    pub wher: String,
    pub duration: String,
    pub started_at: String,
    pub web_path: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct RuleRow {
    pub id: &'static str,
    pub title: &'static str,
    pub detail: &'static str,
    pub on: bool,
}

#[derive(Clone, Debug, Serialize)]
pub struct WorkspacesCard {
    pub root: String,
    pub presets: Vec<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct DeviceView {
    pub name: String,
    /// The runner answers on its control socket.
    pub running: bool,
    /// A runner binary or config was found on this machine.
    pub installed: bool,
    pub online: bool,
    pub taking_jobs: bool,
    /// Why it isn't taking jobs: `user`, `battery`, `jobs_off`.
    pub paused: Option<&'static str>,
    pub capabilities: Vec<String>,
    pub jobs: Vec<LiveJob>,
    pub waiting: Vec<WaitingRow>,
    pub runs: Vec<RunRow>,
    pub rules: Vec<RuleRow>,
    pub claude_code: bool,
    pub workspaces: Option<WorkspacesCard>,
    /// Where the studio explains how to set a runner up.
    pub how_web_path: &'static str,
    pub studio_error: Option<String>,
}

/// What first run shows under "Lend a hand".
#[derive(Clone, Debug, Serialize)]
pub struct RunnerProbe {
    pub installed: bool,
    pub binary: Option<String>,
    pub config: Option<String>,
    pub running: bool,
    pub name: Option<String>,
    pub taking_jobs: bool,
    pub capabilities: Vec<String>,
    pub how_web_path: &'static str,
}

#[derive(Clone, Debug, Serialize)]
pub struct LogView {
    pub title: String,
    pub lines: Vec<String>,
    pub truncated: bool,
    pub status: String,
    pub web_path: Option<String>,
}

pub const RUNNERS_PAGE: &str = "/settings/runners";

/// `1m 48s`, `22s`, `2h 03m`.
pub fn duration_label(seconds: u64) -> String {
    match seconds {
        s if s < 60 => format!("{s}s"),
        s if s < 3600 => format!("{}m {:02}s", s / 60, s % 60),
        s => format!("{}h {:02}m", s / 3600, (s % 3600) / 60),
    }
}

pub fn status_badge(status: &str) -> Badge {
    let (text, tone) = match status {
        "success" => ("Passed", "pass"),
        "failure" => ("Failed", "fail"),
        "cancelled" => ("Stopped", "quiet"),
        "running" | "pending" => ("Running", "yours"),
        "queued" => ("Queued", "quiet"),
        "waiting" => ("Waiting", "agents"),
        "skipped" => ("Skipped", "quiet"),
        _ => ("Unknown", "quiet"),
    };
    Badge {
        text: text.to_string(),
        tone,
    }
}

/// `a`, `a and b`, `a, b and c`.
pub fn list_words(items: &[String]) -> String {
    match items {
        [] => String::new(),
        [one] => one.clone(),
        [first @ .., last] => format!("{} and {last}", first.join(", ")),
    }
}

pub fn waiting_row(agent: &WaitingAgent) -> WaitingRow {
    let why = match agent.reason.as_str() {
        "first run" => "wants to run for the first time",
        "tools changed" => "wants to run with different tools",
        "outside author" => "was started by someone outside your studio",
        "environment" => "wants to use a protected environment",
        _ => "needs your approval to run",
    };
    let repo = short_repo(&agent.repo);
    let uses = if agent.tools.is_empty() {
        "it uses no tools".to_string()
    } else {
        format!("it would use {}", list_words(&agent.tools))
    };
    let mut limits = vec![engine_label(&agent.engine)];
    if let Some(turns) = agent.max_turns {
        limits.push(format!("up to {turns} turns"));
    }
    if let Some(minutes) = agent.timeout_minutes {
        limits.push(if minutes == 1 {
            "1 minute".to_string()
        } else {
            format!("{minutes} minutes")
        });
    }
    WaitingRow {
        job_id: agent.job_id,
        title: format!("{} {why}", agent.workflow),
        detail: format!("{repo} · {uses}"),
        limits: limits.join(" · "),
        web_path: agent.web_path.clone(),
    }
}

/// One row per job of each run that ran on `runner`.
pub fn run_rows(runs: &[Run], runner: &str) -> Vec<RunRow> {
    let mut rows = Vec::new();
    for run in runs {
        for job in run
            .jobs
            .iter()
            .filter(|j| j.runner == runner || runner.is_empty())
        {
            if matches!(
                job.status.as_str(),
                "running" | "queued" | "waiting" | "pending"
            ) {
                continue;
            }
            rows.push(RunRow {
                status: status_badge(&job.status),
                name: if job.name.is_empty() || job.name == run.workflow {
                    run.workflow.clone()
                } else {
                    format!("{} · {}", run.workflow, job.name)
                },
                wher: format!("{} · {}", short_repo(&run.repo), run.branch),
                duration: job
                    .duration_s
                    .or(run.duration_s)
                    .map(duration_label)
                    .unwrap_or_default(),
                started_at: run.started_at.clone(),
                web_path: run.web_path.clone(),
            });
        }
    }
    rows
}

fn rule_rows(status: Option<&RunnerStatus>) -> Vec<RuleRow> {
    let rules = status.map(|s| s.rules.clone()).unwrap_or_default();
    vec![
        RuleRow {
            id: "pause_on_battery",
            title: "Pause on battery",
            detail: "Jobs wait until you plug in",
            on: rules.pause_on_battery,
        },
        RuleRow {
            id: "one_at_a_time",
            title: "One job at a time",
            detail: "Keeps this machine usable while you work",
            on: rules.max_parallel <= 1,
        },
        RuleRow {
            id: "container_jobs",
            title: "Run container jobs",
            detail: "Needs Docker running on this machine",
            on: rules.container_jobs,
        },
    ]
}

impl Desk {
    /// Looks for a runner without touching it: its binary in the usual
    /// place, its config, and whether it answers on its socket.
    pub async fn runner_probe(&self) -> RunnerProbe {
        let settings = self.settings();
        let binary_name = if cfg!(windows) {
            "atelier-runner.exe"
        } else {
            "atelier-runner"
        };
        let binary = [
            settings.home.join(".atelier/bin").join(binary_name),
            settings.home.join(".local/bin").join(binary_name),
        ]
        .into_iter()
        .find(|p| p.is_file());
        let config = Some(settings.runner_config_path()).filter(|p| p.is_file());
        let status = self.runner_status().await;
        RunnerProbe {
            installed: binary.is_some() || config.is_some() || status.is_some(),
            binary: binary.map(|p| self.display_path(&p)),
            config: config.map(|p| self.display_path(&p)),
            running: status.is_some(),
            name: status.as_ref().map(|s| s.name.clone()),
            taking_jobs: status.as_ref().is_some_and(|s| s.taking_jobs),
            capabilities: status.map(|s| s.capabilities).unwrap_or_default(),
            how_web_path: RUNNERS_PAGE,
        }
    }

    pub async fn device_view(&self) -> CmdResult<DeviceView> {
        let runner = self.runner_client();
        let status = runner.status().await.ok();
        let probe_installed = status.is_some() || self.runner_probe().await.installed;
        let name = status
            .as_ref()
            .map(|s| s.name.clone())
            .filter(|n| !n.is_empty())
            .unwrap_or_else(|| self.device_name());

        let mut studio_error = None;
        let (waiting, runs, studio_status) = match self.client() {
            Ok(client) => {
                let query = RunsQuery {
                    limit: Some(20),
                    runner: Some(name.clone()),
                    repo: None,
                };
                let (waiting, runs, studio_status) = tokio::join!(
                    client.agents_waiting(),
                    client.runs(&query),
                    client.status()
                );
                let waiting = self.watch(waiting).unwrap_or_else(|e| {
                    studio_error = Some(e.message);
                    Vec::new()
                });
                let runs = self.watch(runs).unwrap_or_else(|e| {
                    studio_error.get_or_insert(e.message);
                    Vec::new()
                });
                (waiting, runs, studio_status.ok())
            }
            Err(e) => {
                studio_error = Some(e.message);
                (Vec::new(), Vec::new(), None)
            }
        };

        let rows = running_rows(
            status.as_ref(),
            studio_status.as_ref().map_or(&[], |s| s.running.as_slice()),
        );
        let mut jobs = Vec::with_capacity(rows.len());
        for row in rows {
            let tail = runner.tail(row.job_id, Some(60)).await.unwrap_or_default();
            jobs.push(LiveJob { row, tail });
        }
        let workspaces = match &status {
            Some(_) => runner.workspaces().await.ok().map(|w| WorkspacesCard {
                root: self.display_path(std::path::Path::new(&w.root)),
                presets: w
                    .presets
                    .iter()
                    .map(|id| tools::find(id).map_or_else(|| id.clone(), |p| p.label.to_string()))
                    .collect(),
            }),
            None => None,
        };
        Ok(DeviceView {
            running: status.is_some(),
            installed: probe_installed,
            online: status.as_ref().is_some_and(|s| s.online),
            taking_jobs: status.as_ref().is_some_and(|s| s.taking_jobs),
            paused: status.as_ref().and_then(|s| s.paused_reason).map(|r| {
                use atelier_core::runner::PausedReason as P;
                match r {
                    P::User => "user",
                    P::Battery => "battery",
                    P::JobsOff => "jobs_off",
                    P::Unknown => "other",
                }
            }),
            capabilities: status
                .as_ref()
                .map(|s| s.capabilities.clone())
                .unwrap_or_default(),
            claude_code: status
                .as_ref()
                .is_some_and(|s| s.capabilities.iter().any(|c| c == "claude-code")),
            jobs,
            waiting: waiting.iter().map(waiting_row).collect(),
            runs: run_rows(&runs, &name),
            rules: rule_rows(status.as_ref()),
            workspaces,
            how_web_path: RUNNERS_PAGE,
            studio_error,
            name,
        })
    }

    /// The runner switch: take jobs on this machine, or don't.
    pub async fn runner_set_taking(&self, on: bool) -> CmdResult<bool> {
        Ok(self.runner_client().set_taking_jobs(on).await?.taking_jobs)
    }

    /// One of the three rule switches.
    pub async fn runner_set_rule(&self, rule: &str, on: bool) -> CmdResult<()> {
        let update = match rule {
            "pause_on_battery" => RunnerRulesUpdate {
                pause_on_battery: Some(on),
                ..RunnerRulesUpdate::default()
            },
            "one_at_a_time" => RunnerRulesUpdate {
                max_parallel: Some(if on { 1 } else { 2 }),
                ..RunnerRulesUpdate::default()
            },
            "container_jobs" => RunnerRulesUpdate {
                container_jobs: Some(on),
                ..RunnerRulesUpdate::default()
            },
            other => return Err(CmdError::invalid(format!("“{other}” isn’t a runner rule."))),
        };
        self.runner_client().set_rules(&update).await?;
        Ok(())
    }

    /// The last lines of a job running on this machine.
    pub async fn runner_tail(&self, job_id: u64) -> CmdResult<Vec<String>> {
        Ok(self.runner_client().tail(job_id, Some(60)).await?)
    }

    /// Stops a job: through the runner when it runs here, else the studio.
    pub async fn job_stop(&self, job_id: u64) -> CmdResult<()> {
        let here = self
            .runner_status()
            .await
            .is_some_and(|s| s.current.iter().any(|j| j.job_id == job_id));
        if here {
            self.runner_client().cancel(job_id).await?;
        } else {
            self.watch(self.client()?.cancel_job(job_id).await)?;
        }
        Ok(())
    }

    /// The log of a run's failed jobs (or of all its jobs when none failed).
    pub async fn run_log(&self, repo: &str, run_id: u64) -> CmdResult<LogView> {
        let client = self.client()?;
        let query = RunsQuery {
            limit: Some(50),
            runner: None,
            repo: Some(repo.to_string()).filter(|r| !r.is_empty()),
        };
        let runs = self.watch(client.runs(&query).await)?;
        let run = runs
            .into_iter()
            .find(|r| r.run_id == run_id)
            .ok_or_else(|| CmdError::new("not_found", "That run isn’t among the recent ones."))?;
        let failed: Vec<_> = run.jobs.iter().filter(|j| j.status == "failure").collect();
        let jobs = if failed.is_empty() {
            run.jobs.iter().collect()
        } else {
            failed
        };
        let mut view = LogView {
            title: format!(
                "{} on {} · {}",
                run.workflow,
                run.branch,
                short_repo(&run.repo)
            ),
            lines: Vec::new(),
            truncated: false,
            status: run.status.clone(),
            web_path: run.web_path.clone(),
        };
        let many = jobs.len() > 1;
        for job in jobs {
            let log = self.watch(client.job_log(job.job_id, Some(200)).await)?;
            if many {
                view.lines.push(format!("── {} ──", job.name));
            }
            view.lines.extend(log.lines);
            view.truncated |= log.truncated;
        }
        Ok(view)
    }

    /// Keeps the cached status fresh for a moment; used by the poller.
    pub async fn runner_snapshot(&self) -> Option<RunnerStatus> {
        tokio::time::timeout(Duration::from_secs(2), self.runner_status())
            .await
            .ok()
            .flatten()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn words() {
        assert_eq!(duration_label(22), "22s");
        assert_eq!(duration_label(108), "1m 48s");
        assert_eq!(duration_label(123), "2m 03s");
        assert_eq!(duration_label(7380), "2h 03m");
        let tools: Vec<String> = ["issues.list", "issues.comment", "issues.close"]
            .iter()
            .map(|s| s.to_string())
            .collect();
        assert_eq!(
            list_words(&tools),
            "issues.list, issues.comment and issues.close"
        );
        assert_eq!(list_words(&tools[..2]), "issues.list and issues.comment");
        assert_eq!(status_badge("failure").text, "Failed");
        assert_eq!(status_badge("???").tone, "quiet");
    }

    #[test]
    fn a_waiting_agent_reads_like_a_sentence() {
        let agent = WaitingAgent {
            job_id: 31,
            repo: "DemiInfinity/Atelier".into(),
            workflow: "weekly-tidy".into(),
            reason: "first run".into(),
            engine: "claude-code".into(),
            tools: vec![
                "issues.list".into(),
                "issues.comment".into(),
                "issues.close".into(),
            ],
            max_turns: Some(20),
            timeout_minutes: Some(10),
            ..WaitingAgent::default()
        };
        let row = waiting_row(&agent);
        assert_eq!(row.title, "weekly-tidy wants to run for the first time");
        assert_eq!(
            row.detail,
            "Atelier · it would use issues.list, issues.comment and issues.close"
        );
        assert_eq!(row.limits, "Claude Code · up to 20 turns · 10 minutes");
    }
}
