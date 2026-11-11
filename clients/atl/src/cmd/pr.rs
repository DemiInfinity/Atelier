//! `atl pr`, `atl pr N`, `atl pr merge N`.

use atelier_core::api::{
    Check, CheckCounts, CheckStatus, ChecksState, MergeMethod, MergeOutcome, MergeRequest, Pull,
    PullState, PullSummary, Via,
};
use atelier_core::messages::Vars;
use atelier_core::Client;
use serde_json::json;

use crate::ctx::Ctx;
use crate::error::CliResult;
use crate::ui::{column, outln, pad, short_repo, Ui};
use crate::{MethodArg, RepoScope, StateArg};

const INDENT: &str = "     ";

fn header(ui: &Ui, number: u64, title: &str, head: &str, base: &str, repo: Option<&str>) {
    let mut line = format!(
        "{}{}{}",
        ui.amber_bold(&pad(&format!("#{number}"), 5)),
        ui.bold(title),
        ui.muted(&format!("   {head} → {base}"))
    );
    if let Some(repo) = repo {
        line.push_str(&ui.muted(&format!("   {}", short_repo(repo))));
    }
    outln!("{line}");
}

fn symbol(ui: &Ui, status: CheckStatus) -> String {
    match status {
        CheckStatus::Success => ui.green("✓ "),
        CheckStatus::Failure => ui.red("✗ "),
        CheckStatus::Pending => ui.amber("◐ "),
        CheckStatus::Waiting => ui.violet("◆ "),
        CheckStatus::Skipped | CheckStatus::Unknown => ui.muted("– "),
    }
}

/// `3 of 4 passed`, `1 failing`, `running`, `all 4 passed`: the checks in
/// a few words. `None` when there are no checks.
fn checks_summary(ui: &Ui, counts: &CheckCounts) -> Option<String> {
    let total = counts.count();
    Some(match counts.summary() {
        ChecksState::None => return None,
        ChecksState::Failing => ui.red(&format!("{} failing", counts.failed.max(1))),
        ChecksState::Pending if counts.passed > 0 => {
            ui.amber(&format!("{} of {total} passed", counts.passed))
        }
        ChecksState::Pending if counts.pending == 0 => ui.violet("waiting for you"),
        ChecksState::Pending => ui.amber("running"),
        ChecksState::Passing | ChecksState::Unknown if total > 1 => {
            ui.green(&format!("all {total} passed"))
        }
        ChecksState::Passing | ChecksState::Unknown => ui.green("passed"),
    })
}

/// The summary, then the counts behind it when they say more.
fn counts_line(ui: &Ui, counts: &CheckCounts) -> String {
    let Some(summary) = checks_summary(ui, counts) else {
        return ui.muted("no checks");
    };
    let mut parts = vec![summary];
    let kinds = [counts.failed, counts.passed, counts.pending, counts.waiting]
        .iter()
        .filter(|n| **n > 0)
        .count();
    if kinds < 2 {
        // One kind of check only: the summary has said it all.
        return parts.remove(0);
    }
    if counts.failed > 0 {
        parts.push(format!("{}{} failed", ui.red("✗ "), counts.failed));
    }
    if counts.passed > 0 {
        parts.push(format!("{}{} passed", ui.green("✓ "), counts.passed));
    }
    if counts.pending > 0 {
        parts.push(format!("{}{} running", ui.amber("◐ "), counts.pending));
    }
    if counts.waiting > 0 {
        parts.push(format!(
            "{}{} waiting for you",
            ui.violet("◆ "),
            counts.waiting
        ));
    }
    parts.join("   ")
}

fn check_line(ui: &Ui, check: &Check, width: usize) -> String {
    let name = pad(&check.name, width);
    let line = if check.status == CheckStatus::Waiting {
        let mut line = format!(
            "{INDENT}{}{name}   {}",
            symbol(ui, check.status),
            ui.violet(&check.detail)
        );
        if let (Some("approve_agent"), Some(job)) = (check.action.as_deref(), check.job_id) {
            line.push_str(&format!(
                "   {}",
                ui.amber(&format!("atl agent approve {job}"))
            ));
        }
        line
    } else {
        format!(
            "{INDENT}{}{name}   {}",
            symbol(ui, check.status),
            ui.muted(&check.detail)
        )
    };
    line.trim_end().to_string()
}

fn via(via: Option<Via>) -> &'static str {
    match via {
        Some(Via::Tether) => " · via Tether",
        Some(Via::Connect) => " · via Connect",
        _ => "",
    }
}

fn merge_line(ui: &Ui, pull: &Pull) -> Option<String> {
    if pull.state != PullState::Open {
        return Some(ui.muted(&format!("This pull request is {}.", pull.state.as_str())));
    }
    if let Some(auto) = &pull.merge.auto_merge {
        return Some(format!(
            "Will {} when every check passes.",
            method_words(auto.method)
        ));
    }
    if pull.merge.can_merge {
        let command = format!("atl pr merge {}", pull.number);
        return Some(format!(
            "{}{}",
            ui.green("Ready to merge: "),
            ui.amber(&command)
        ));
    }
    (!pull.merge.reason.is_empty())
        .then(|| ui.muted(&format!("Can’t merge yet: {}", pull.merge.reason)))
}

fn print_pull(ui: &Ui, pull: &Pull) {
    header(ui, pull.number, &pull.title, &pull.head, &pull.base, None);
    let width = column(pull.checks.iter().map(|c| c.name.as_str()), 40);
    for check in &pull.checks {
        outln!("{}", check_line(ui, check, width));
    }
    for comment in &pull.comments {
        outln!();
        let who = format!("{INDENT}{}{}", comment.author, via(comment.via));
        outln!(
            "{}",
            match comment.kind.as_str() {
                "bot" => ui.blue(&who),
                "agent" => ui.violet(&who),
                _ => ui.bold(&who),
            }
        );
        let lines: Vec<&str> = comment.body.lines().collect();
        for line in lines.iter().take(6) {
            outln!("{}", ui.muted(&format!("{INDENT}{line}")).trim_end());
        }
        if lines.len() > 6 {
            outln!("{}", ui.muted(&format!("{INDENT}…")));
        }
    }
    if let Some(line) = merge_line(ui, pull) {
        outln!();
        outln!("{INDENT}{line}");
    }
}

fn method_words(method: MergeMethod) -> &'static str {
    match method {
        MergeMethod::Squash => "squash and merge",
        MergeMethod::Rebase => "rebase and merge",
        MergeMethod::Merge | MergeMethod::Unknown => "merge",
    }
}

pub async fn list(ctx: &Ctx, scope: RepoScope, all: bool, state: StateArg) -> CliResult<()> {
    let client = ctx.client()?;
    let ui = &ctx.ui;
    let state = match state {
        StateArg::Open => PullState::Open,
        StateArg::Closed => PullState::Closed,
        StateArg::Merged => PullState::Merged,
        StateArg::All => PullState::All,
    };
    // Across the studio when asked, or when this folder isn't a studio repository.
    let target = if all {
        None
    } else {
        match ctx.target_repo(&client, scope.repo.as_deref()).await {
            Ok(target) => Some(target),
            Err(e) if scope.repo.is_some() || e.exit != crate::error::EXIT_ERROR => return Err(e),
            Err(_) => None,
        }
    };
    let pulls: Vec<PullSummary> = match &target {
        Some((owner, repo)) => client.repo_pulls(owner, repo, state).await,
        None => client.pulls().await,
    }
    .map_err(|e| ctx.err(e))?;

    if ctx.json {
        crate::print_json(&json!({
            "repo": target.as_ref().map(|(o, r)| format!("{o}/{r}")),
            "pulls": pulls,
        }));
        return Ok(());
    }
    if pulls.is_empty() {
        match &target {
            Some((_, repo)) => outln!("No {} pull requests in {repo}.", state_word(state)),
            None => outln!("No open pull requests in your studio."),
        }
        return Ok(());
    }
    // A single pull request is shown in full, as `atl pr N` would.
    if let ([only], Some((owner, repo))) = (pulls.as_slice(), &target) {
        if let Ok(pull) = client.pull(owner, repo, only.number).await {
            print_pull(ui, &pull);
            return Ok(());
        }
    }
    for (index, pull) in pulls.iter().enumerate() {
        if index > 0 {
            outln!();
        }
        let repo = target.is_none().then_some(pull.repo.as_str());
        header(ui, pull.number, &pull.title, &pull.head, &pull.base, repo);
        outln!("{INDENT}{}", counts_line(ui, &pull.checks));
    }
    Ok(())
}

fn state_word(state: PullState) -> &'static str {
    match state {
        PullState::All => "",
        other => other.as_str(),
    }
}

pub async fn show(ctx: &Ctx, number: u64, scope: RepoScope) -> CliResult<()> {
    let client = ctx.client()?;
    let (owner, repo) = ctx.target_repo(&client, scope.repo.as_deref()).await?;
    let pull = client
        .pull(&owner, &repo, number)
        .await
        .map_err(|e| ctx.err(e))?;
    if ctx.json {
        crate::print_json(&pull);
    } else {
        print_pull(&ctx.ui, &pull);
    }
    Ok(())
}

async fn default_method(client: &Client, owner: &str, repo: &str, number: u64) -> MergeMethod {
    client
        .pull(owner, repo, number)
        .await
        .ok()
        .and_then(|p| {
            p.merge
                .methods
                .into_iter()
                .find(|m| *m != MergeMethod::Unknown)
        })
        .unwrap_or(MergeMethod::Squash)
}

pub async fn merge(
    ctx: &Ctx,
    number: u64,
    when_green: bool,
    method: Option<MethodArg>,
    cancel: bool,
    delete_branch: bool,
    scope: RepoScope,
) -> CliResult<()> {
    let client = ctx.client()?;
    let ui = &ctx.ui;
    let (owner, repo) = ctx.target_repo(&client, scope.repo.as_deref()).await?;
    let tag = ui.amber(&format!("#{number}"));

    if cancel {
        client
            .cancel_auto_merge(&owner, &repo, number)
            .await
            .map_err(|e| ctx.err(e))?;
        if ctx.json {
            crate::print_json(&json!({"number": number, "auto_merge": null}));
        } else {
            outln!("{tag} won’t merge by itself any more.");
        }
        return Ok(());
    }

    let method = match method {
        Some(MethodArg::Squash) => MergeMethod::Squash,
        Some(MethodArg::Merge) => MergeMethod::Merge,
        Some(MethodArg::Rebase) => MergeMethod::Rebase,
        None => default_method(&client, &owner, &repo, number).await,
    };
    let outcome = client
        .merge_pull(
            &owner,
            &repo,
            number,
            MergeRequest {
                method,
                when_green,
                delete_branch: delete_branch.then_some(true),
            },
        )
        .await
        .map_err(|e| {
            let refused = e.status() == Some(409);
            let error = ctx.err(e);
            if refused && !when_green {
                error.hint(format!(
                    "To merge once everything passes: atl pr merge {number} --when-green."
                ))
            } else {
                error
            }
        })?;

    match &outcome {
        MergeOutcome::Scheduled { auto_merge } => {
            if ctx.json {
                crate::print_json(
                    &json!({"number": number, "merged": false, "auto_merge": auto_merge}),
                );
                return Ok(());
            }
            outln!(
                "Will {} {tag} when every check passes.",
                method_words(auto_merge.method)
            );
            outln!("{}", ui.muted("You’ll get a message when it lands."));
        }
        MergeOutcome::Merged { sha, .. } => {
            // The pull request's base, for "Merged into main".
            let base = client
                .pull(&owner, &repo, number)
                .await
                .ok()
                .map(|p| p.base)
                .filter(|b| !b.is_empty());
            let vars = Vars::new()
                .set("repo", &repo)
                .set_opt("base", base.as_deref());
            let message = ctx.friendly(&client, "merge", vars).await;
            if ctx.json {
                crate::print_json(&json!({
                    "number": number, "merged": true, "sha": sha, "method": method, "message": message,
                }));
                return Ok(());
            }
            let short: String = sha.chars().take(7).collect();
            let into = base.map(|b| format!(" into {b}")).unwrap_or_default();
            outln!(
                "{}Merged {tag}{into}{}",
                ui.green("✓ "),
                if short.is_empty() {
                    ".".to_string()
                } else {
                    format!(" {}", ui.muted(&format!("({short})")))
                }
            );
            ctx.print_friendly(&message);
        }
    }
    Ok(())
}
