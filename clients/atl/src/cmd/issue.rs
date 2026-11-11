//! `atl issue list | view N | new | close N`.

use atelier_core::api::{Issue, IssuesQuery, Via};
use serde_json::json;

use crate::ctx::Ctx;
use crate::error::CliResult;
use crate::ui::{column, count, outln, pad, since, Ui};
use crate::{IssueStateArg, RepoScope};

const INDENT: &str = "     ";

/// `owner:DemiInfinity` → `DemiInfinity`; `token:device:MacBook` → `MacBook`.
fn author(author: &str) -> &str {
    author.rsplit(':').next().unwrap_or(author)
}

fn via(via: Option<Via>) -> &'static str {
    match via {
        Some(Via::Tether) => " · via Tether",
        Some(Via::Connect) => " · via Connect",
        _ => "",
    }
}

fn now(ctx: &Ctx) -> time::OffsetDateTime {
    let now = time::OffsetDateTime::now_utc();
    match ctx.local_offset {
        Some(offset) => now.to_offset(offset),
        None => now,
    }
}

pub async fn list(ctx: &Ctx, state: IssueStateArg, scope: RepoScope) -> CliResult<()> {
    let client = ctx.client()?;
    let (owner, repo) = ctx.target_repo(&client, scope.repo.as_deref()).await?;
    let full_name = format!("{owner}/{repo}");
    let word = match state {
        IssueStateArg::Open => "open",
        IssueStateArg::Closed => "closed",
        IssueStateArg::All => "all",
    };
    let list = client
        .issues(&IssuesQuery {
            repo: full_name.clone(),
            state: word.to_string(),
            page: None,
            per_page: Some(100),
        })
        .await
        .map_err(|e| ctx.err(e))?;
    if ctx.json {
        crate::print_json(
            &json!({"repo": full_name, "issues": list.issues, "has_more": list.has_more}),
        );
        return Ok(());
    }
    let ui = &ctx.ui;
    if list.issues.is_empty() {
        match state {
            IssueStateArg::All => outln!("No issues in {repo}."),
            _ => outln!("No {word} issues in {repo}."),
        }
        return Ok(());
    }
    let width = column(list.issues.iter().map(|i| i.title.as_str()), 60);
    let at = now(ctx);
    for issue in &list.issues {
        let mut notes: Vec<String> = Vec::new();
        if issue.state == "closed" {
            notes.push("closed".into());
        }
        if !issue.labels.is_empty() {
            notes.push(
                issue
                    .labels
                    .iter()
                    .map(|l| l.name.as_str())
                    .collect::<Vec<_>>()
                    .join(", "),
            );
        }
        if issue.comments > 0 {
            notes.push(count(issue.comments, "comment", "comments"));
        }
        if !issue.created_at.is_empty() {
            notes.push(since(&issue.created_at, at));
        }
        let number = pad(&format!("#{}", issue.number), 5);
        let mut notes = ui.muted(&notes.join(" · "));
        if issue.via.is_some_and(|v| !via(Some(v)).is_empty()) {
            notes.push_str(&ui.blue(via(issue.via)));
        }
        let line = format!(
            "{}{}   {}",
            if issue.via == Some(Via::Tether) {
                ui.blue_bold(&number)
            } else {
                ui.amber_bold(&number)
            },
            pad(&issue.title, width),
            notes,
        );
        outln!("{}", line.trim_end());
    }
    if list.has_more {
        outln!("{}", ui.muted("There are more; these are the newest."));
    }
    Ok(())
}

fn print_issue(ui: &Ui, issue: &Issue, at: time::OffsetDateTime) {
    outln!(
        "{}{}{}",
        ui.amber_bold(&pad(&format!("#{}", issue.number), 5)),
        ui.bold(&issue.title),
        ui.muted(&format!(
            "   {} · by {}{}",
            if issue.state == "closed" {
                "closed"
            } else {
                "open"
            },
            author(&issue.author),
            via(issue.via)
        ))
    );
    if !issue.labels.is_empty() {
        let names: Vec<&str> = issue.labels.iter().map(|l| l.name.as_str()).collect();
        outln!("{INDENT}{}", ui.muted(&names.join(", ")));
    }
    if !issue.body.trim().is_empty() {
        outln!();
        for line in issue.body.lines() {
            outln!("{}", format!("{INDENT}{line}").trim_end());
        }
    }
    for comment in &issue.comments {
        outln!();
        let mut who = format!("{INDENT}{}{}", author(&comment.author), via(comment.via));
        if !comment.created_at.is_empty() {
            who.push_str(&format!(" · {}", since(&comment.created_at, at)));
        }
        outln!(
            "{}",
            match comment.kind.as_str() {
                "bot" => ui.blue(&who),
                "agent" => ui.violet(&who),
                _ => ui.bold(&who),
            }
        );
        for line in comment.body.lines() {
            outln!("{}", ui.muted(&format!("{INDENT}{line}")).trim_end());
        }
    }
}

pub async fn view(ctx: &Ctx, number: u64, scope: RepoScope) -> CliResult<()> {
    let client = ctx.client()?;
    let (owner, repo) = ctx.target_repo(&client, scope.repo.as_deref()).await?;
    let issue = client
        .issue(&owner, &repo, number)
        .await
        .map_err(|e| ctx.err(e))?;
    if ctx.json {
        crate::print_json(&issue);
    } else {
        print_issue(&ctx.ui, &issue, now(ctx));
    }
    Ok(())
}

pub async fn new(ctx: &Ctx, title: &str, body: &str, scope: RepoScope) -> CliResult<()> {
    let client = ctx.client()?;
    let (owner, repo) = ctx.target_repo(&client, scope.repo.as_deref()).await?;
    let issue = client
        .create_issue(&owner, &repo, title.trim(), body)
        .await
        .map_err(|e| ctx.err(e))?;
    if ctx.json {
        crate::print_json(&issue);
    } else if issue.number > 0 {
        outln!(
            "Opened issue {} in {repo}.",
            ctx.ui.amber_bold(&format!("#{}", issue.number))
        );
    } else {
        outln!("Opened the issue in {repo}.");
    }
    Ok(())
}

pub async fn close(ctx: &Ctx, number: u64, scope: RepoScope) -> CliResult<()> {
    let client = ctx.client()?;
    let (owner, repo) = ctx.target_repo(&client, scope.repo.as_deref()).await?;
    client
        .set_issue_open(&owner, &repo, number, false)
        .await
        .map_err(|e| ctx.err(e))?;
    if ctx.json {
        crate::print_json(
            &json!({"closed": true, "repo": format!("{owner}/{repo}"), "number": number}),
        );
    } else {
        outln!("Closed issue #{number} in {repo}.");
    }
    Ok(())
}
