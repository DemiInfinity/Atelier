//! `atl repo list | clone | publish | rename | delete`.

use std::path::PathBuf;
use std::time::Duration;

use atelier_core::api::{Account, Repo};
use atelier_core::git::{PublishOptions, PublishStep, Transport};
use atelier_core::messages::Vars;
use atelier_core::Error;
use serde_json::json;

use crate::cmd::auth::{account_label, needs_yes};
use crate::ctx::{find_repo, split_full_name, Ctx};
use crate::error::{CliError, CliResult};
use crate::ui::{column, count, out, outln, pad};
use crate::{CloneArgs, PublishArgs, RepoScope};

pub async fn list(ctx: &Ctx) -> CliResult<()> {
    let client = ctx.client()?;
    let repos = client.repos().await.map_err(|e| ctx.err(e))?;
    if ctx.json {
        crate::print_json(&json!({ "repos": repos }));
        return Ok(());
    }
    let ui = &ctx.ui;
    if repos.is_empty() {
        outln!("Your studio has no repositories yet.");
        outln!(
            "{}{}",
            ui.muted("Turn a folder into one with "),
            ui.amber("atl repo publish")
        );
        return Ok(());
    }
    let name_width = column(repos.iter().map(|r| r.full_name.as_str()), 48);
    let language_width = column(
        repos.iter().map(|r| r.language.as_deref().unwrap_or("")),
        16,
    );
    for repo in &repos {
        let visibility = if repo.private { "private" } else { "public " };
        let mut notes = Vec::new();
        if repo.open_pulls > 0 {
            notes.push(count(repo.open_pulls, "pull request", "pull requests"));
        }
        let mut line = format!(
            "{}  {}",
            ui.amber(&pad(&repo.full_name, name_width)),
            ui.muted(&format!(
                "{visibility}  {}  {}",
                pad(repo.language.as_deref().unwrap_or(""), language_width),
                notes.join(" · ")
            ))
        );
        if repo.github.is_some() {
            line = format!("{}  {}", line.trim_end(), ui.blue("on GitHub too"));
        }
        outln!("{}", line.trim_end());
    }
    Ok(())
}

pub async fn clone(ctx: &Ctx, args: CloneArgs) -> CliResult<()> {
    let client = ctx.client()?;
    let ui = &ctx.ui;
    let repo: Repo = match (&args.github, &args.name) {
        (Some(github), _) => {
            if split_full_name(github).is_none() {
                return Err(
                    CliError::plain(format!("“{github}” isn’t a GitHub repository name."))
                        .hint("Write it as owner/name."),
                );
            }
            let mirrored = client.mirror_repo(github).await.map_err(|e| ctx.err(e))?;
            if !ctx.json {
                out!(
                    "Mirroring {github} into your studio as {}… ",
                    mirrored.full_name
                );
            }
            // The mirror is made in the background; wait for it to show up.
            let mut found = None;
            for _ in 0..60 {
                let repos = client.repos().await.map_err(|e| ctx.err(e))?;
                found = repos
                    .into_iter()
                    .find(|r| r.full_name == mirrored.full_name);
                if found.is_some() {
                    break;
                }
                tokio::time::sleep(Duration::from_secs(1)).await;
            }
            let Some(repo) = found else {
                if !ctx.json {
                    outln!();
                }
                return Err(CliError::plain(format!(
                    "{} is still being mirrored.",
                    mirrored.full_name
                ))
                .hint(format!(
                    "Try atl repo clone {} in a minute.",
                    mirrored.full_name
                )));
            };
            if !ctx.json {
                outln!("{}", ui.green("done"));
            }
            repo
        }
        (None, Some(name)) => {
            let repos = client.repos().await.map_err(|e| ctx.err(e))?;
            find_repo(&repos, name)?.clone()
        }
        (None, None) => return Err(CliError::plain("Say which repository to clone.")),
    };

    let dest: PathBuf = match args.path {
        Some(path) => path,
        None => ctx.settings.clone_root().join(&repo.owner).join(&repo.name),
    };
    if dest.exists() && std::fs::read_dir(&dest).is_ok_and(|mut d| d.next().is_some()) {
        return Err(
            CliError::plain(format!("{} is already there.", dest.display()))
                .hint(format!("Open it with atl open {}.", repo.full_name))
                .kind("already_cloned"),
        );
    }
    let url = if args.https || repo.clone_ssh.is_empty() {
        &repo.clone_https
    } else {
        &repo.clone_ssh
    };
    if !ctx.json {
        out!("Cloning {}… ", repo.full_name);
    }
    if let Err(e) = ctx.git().clone_repo(url, &dest).await {
        if !ctx.json {
            outln!();
        }
        return Err(ctx.err(e));
    }
    if ctx.json {
        crate::print_json(&json!({"repo": repo.full_name, "path": dest, "url": url}));
        return Ok(());
    }
    outln!("{}", ui.green("done"));
    outln!(
        "{}{}",
        ui.green("✓ "),
        ui.amber(&dest.display().to_string())
    );
    Ok(())
}

/// Lets the person pick the account with the arrow keys.
fn pick_account(ctx: &Ctx, accounts: &[Account], default: usize) -> CliResult<usize> {
    let labels: Vec<String> = accounts.iter().map(account_label).collect();
    let picked = if ctx.ui.color {
        dialoguer::Select::with_theme(&dialoguer::theme::ColorfulTheme::default())
            .with_prompt("Account")
            .items(&labels)
            .default(default)
            .interact_opt()
    } else {
        dialoguer::Select::new()
            .with_prompt("Account")
            .items(&labels)
            .default(default)
            .interact_opt()
    };
    match picked {
        Ok(Some(index)) => Ok(index),
        _ => Err(CliError::plain("Nothing was published.").kind("cancelled")),
    }
}

pub async fn publish(ctx: &Ctx, args: PublishArgs) -> CliResult<()> {
    let client = ctx.client()?;
    let git = ctx.git();
    let ui = &ctx.ui;
    let cwd = ctx.cwd()?;
    let plan = git.publish_plan(&cwd).await.map_err(|e| ctx.err(e))?;
    if plan.commits == 0 {
        return Err(CliError::plain("There are no commits here yet.")
            .hint("Commit something first, then run atl repo publish again."));
    }

    let (me, repos) = tokio::join!(client.me(), client.repos());
    let me = me.map_err(|e| ctx.err(e))?;
    let repos = repos.map_err(|e| ctx.err(e))?;

    // Already in the studio? Then there is nothing to publish.
    if let Some(existing) = plan
        .origin
        .as_deref()
        .and_then(atelier_core::git::RemoteRef::parse)
        .and_then(|remote| {
            repos.iter().find(|r| {
                [&r.clone_https, &r.clone_ssh]
                    .iter()
                    .any(|u| atelier_core::git::RemoteRef::parse(u).as_ref() == Some(&remote))
            })
        })
    {
        return Err(CliError::plain(format!(
            "This folder is already in your studio as {}.",
            existing.full_name
        ))
        .hint("Use git push.")
        .kind("already_published"));
    }

    if !ctx.interactive && !args.yes {
        return Err(needs_yes("publishing this folder"));
    }
    let asking = ctx.interactive && !args.yes;

    // An origin that points elsewhere is only replaced with a yes.
    let mut replace_origin = args.replace_origin;
    if let (Some(origin), false) = (&plan.origin, replace_origin) {
        if !asking {
            return Err(ctx.err(Error::OriginExists {
                url: origin.clone(),
            }));
        }
        outln!("origin points at {} now.", ui.amber(origin));
        if !crate::ui::confirm("Point it at your studio instead?", ui) {
            return Err(CliError::plain("Nothing was published.").kind("cancelled"));
        }
        outln!();
        replace_origin = true;
    }

    if me.accounts.is_empty() {
        return Err(CliError::plain(
            "Your studio didn’t list any account to publish under.",
        ));
    }
    let personal = me
        .accounts
        .iter()
        .position(|a| a.name == me.person.login)
        .or_else(|| me.accounts.iter().position(|a| a.kind == "user"))
        .unwrap_or(0);
    let chosen = match &args.owner {
        Some(owner) => me
            .accounts
            .iter()
            .position(|a| a.name.eq_ignore_ascii_case(owner))
            .ok_or_else(|| {
                CliError::plain(format!("You have no account called {owner}.")).hint(format!(
                    "Yours are: {}.",
                    me.accounts
                        .iter()
                        .map(|a| a.name.as_str())
                        .collect::<Vec<_>>()
                        .join(", ")
                ))
            })?,
        None if asking && me.accounts.len() > 1 => {
            outln!("This folder isn’t in your studio yet.");
            outln!();
            pick_account(ctx, &me.accounts, personal)?
        }
        None => personal,
    };
    let owner = me.accounts[chosen].name.clone();
    let name = args
        .name
        .clone()
        .unwrap_or_else(|| plan.folder_name.clone());
    let private = !args.public;

    if !ctx.json {
        if args.owner.is_some() || !asking || me.accounts.len() <= 1 {
            outln!("This folder isn’t in your studio yet.");
            outln!();
        }
        for (index, account) in me.accounts.iter().enumerate() {
            let label = if index == 0 {
                "  Account  "
            } else {
                "           "
            };
            if index == chosen {
                outln!(
                    "{}{}",
                    ui.muted(label),
                    ui.amber_bold(&format!("› {}", account_label(account)))
                );
            } else {
                outln!(
                    "{}",
                    ui.muted(&format!("{label}  {}", account_label(account)))
                );
            }
        }
        outln!("{}{name}", ui.muted("  Name     "));
        outln!(
            "{}{}",
            ui.muted("  Private  "),
            if private { "yes" } else { "no" }
        );
        outln!();
    }

    let options = PublishOptions {
        path: plan.toplevel.clone(),
        owner,
        name,
        private,
        description: args.description.clone(),
        transport: if args.https {
            Transport::Https
        } else {
            Transport::Ssh
        },
        replace_origin,
    };
    let quiet = ctx.json;
    let done = ui.green("done");
    let mut mid_line = false;
    let mut progress = |step: PublishStep| {
        if quiet {
            return;
        }
        match step {
            PublishStep::Creating { full_name } => {
                out!("Creating {full_name}… ");
                mid_line = true;
            }
            PublishStep::Pushing { branch, commits } => {
                let what = match branch {
                    Some(branch) => {
                        format!("{branch} ({})", count(commits, "commit", "commits"))
                    }
                    None => "everything".to_string(),
                };
                out!("Pushing {what}… ");
                mid_line = true;
            }
            PublishStep::Created | PublishStep::Pushed => {
                outln!("{done}");
                mid_line = false;
            }
        }
    };
    let published = git.publish_folder(&client, &options, &mut progress).await;
    let published = match published {
        Ok(published) => published,
        Err(e) => {
            if mid_line {
                outln!("{}", ui.red("failed"));
            }
            let pushing = matches!(e, Error::Git { .. });
            let mut error = ctx.err(e);
            if pushing {
                error = error.hint(
                    "The repository exists in your studio and origin points at it; \
                     try git push -u origin --all once that is sorted.",
                );
            }
            return Err(error);
        }
    };

    let vars = Vars::new()
        .set("repo", &published.repo.name)
        .set_opt("branch", published.branch.as_deref())
        .set("count", published.commits);
    let message = ctx.friendly(&client, "push", vars).await;
    if ctx.json {
        crate::print_json(&json!({
            "repo": published.repo,
            "url": published.web_url,
            "path": published.path,
            "branch": published.branch,
            "commits": published.commits,
            "origin": published.origin,
            "previous_origin": published.previous_origin,
            "message": message,
        }));
        return Ok(());
    }
    outln!();
    outln!("{}{}", ui.green("✓ "), ui.amber(&published.web_url));
    outln!(
        "{}",
        ui.muted("  origin now points at your studio. Your folder stayed where it is.")
    );
    ctx.print_friendly(&message);
    Ok(())
}

/// The studio repository named by `--repo`, or the one this folder belongs
/// to, with the working copy when it is this folder's.
async fn target(
    ctx: &Ctx,
    client: &atelier_core::Client,
    scope: &RepoScope,
) -> CliResult<(Repo, Option<PathBuf>)> {
    let repos = client.repos().await.map_err(|e| ctx.err(e))?;
    if let Some(name) = scope.repo.as_deref() {
        return Ok((find_repo(&repos, name)?.clone(), None));
    }
    match ctx.here().await? {
        Some(here) => match here.studio_repo(&repos) {
            Some(repo) => Ok((repo.clone(), Some(here.path.clone()))),
            None => Err(CliError::plain("This folder isn’t in your studio yet.")
                .hint("Name a repository with --repo owner/name.")),
        },
        None => Err(CliError::plain("You’re not inside a repository.")
            .hint("Go to one, or name one with --repo owner/name.")),
    }
}

/// `atl repo rename NEW`: renames in the studio, then points this folder's
/// remotes that named the old address at the new one.
pub async fn rename(ctx: &Ctx, new: &str, scope: RepoScope) -> CliResult<()> {
    let client = ctx.client()?;
    let new = new.trim();
    if new.is_empty() || new.contains(char::is_whitespace) || new.contains(['/', '\\']) {
        return Err(
            CliError::plain(format!("“{new}” isn’t a name a repository can have."))
                .hint("Use one word, like my-new-name."),
        );
    }
    let (old, folder) = target(ctx, &client, &scope).await?;
    if old.name == new {
        return Err(CliError::plain(format!(
            "{} is called that already.",
            old.full_name
        )));
    }
    let renamed = client
        .rename_repo(&old.owner, &old.name, new)
        .await
        .map_err(|e| ctx.err(e))?;
    let full_name = if renamed.full_name.is_empty() {
        format!("{}/{new}", old.owner)
    } else {
        renamed.full_name.clone()
    };
    let remotes = match (&folder, renamed.name.is_empty()) {
        (Some(folder), false) => ctx
            .git()
            .retarget_remotes(folder, &old, &renamed)
            .await
            .map_err(|e| ctx.err(e))?,
        _ => Vec::new(),
    };
    if ctx.json {
        crate::print_json(&json!({
            "renamed": true, "from": old.full_name, "full_name": full_name, "remotes": remotes,
        }));
        return Ok(());
    }
    let ui = &ctx.ui;
    outln!(
        "Renamed {} to {}.",
        old.full_name,
        ui.amber_bold(&full_name)
    );
    match remotes.as_slice() {
        [] if folder.is_some() => outln!("{}", ui.muted("This folder’s remotes needed no change.")),
        [] => {}
        [one] => outln!(
            "{}",
            ui.muted(&format!(
                "This folder’s remote {one} points at the new address."
            ))
        ),
        many => outln!(
            "{}",
            ui.muted(&format!(
                "This folder’s remotes {} point at the new address.",
                many.join(", ")
            ))
        ),
    }
    Ok(())
}

/// `atl repo delete`: deletes from the studio only, after the name was
/// typed (or given to `--yes`). Nothing on this machine is touched.
pub async fn delete(ctx: &Ctx, yes: Option<String>, scope: RepoScope) -> CliResult<()> {
    let client = ctx.client()?;
    let ui = &ctx.ui;
    let (repo, _) = target(ctx, &client, &scope).await?;
    let matches = |typed: &str| typed.trim() == repo.full_name || typed.trim() == repo.name;
    match yes {
        Some(given) if given.trim() == repo.full_name => {}
        Some(given) => {
            return Err(
                CliError::plain(format!("“{given}” isn’t this repository’s full name."))
                    .hint(format!("To delete it, pass --yes {}.", repo.full_name))
                    .kind("confirm_mismatch"),
            )
        }
        None if ctx.json => return Err(needs_yes("deleting it")),
        None => {
            outln!(
                "This deletes {} from your studio for good: its code there, pull requests, issues and runs.",
                ui.bold(&repo.full_name)
            );
            outln!(
                "{}",
                ui.muted(&format!(
                    "The folder on {} is kept exactly as it is.",
                    crate::ui::this_machine()
                ))
            );
            out!("Type {} to delete it: ", ui.amber(&repo.name));
            let mut typed = String::new();
            let read = std::io::stdin().read_line(&mut typed).unwrap_or(0);
            if read == 0 || !matches(&typed) {
                if read == 0 {
                    outln!();
                }
                return Err(CliError::plain("Nothing was deleted.")
                    .hint(format!(
                        "Type the name exactly, or pass --yes {}.",
                        repo.full_name
                    ))
                    .kind("confirm_mismatch"));
            }
        }
    }
    client
        .delete_repo(&repo.owner, &repo.name)
        .await
        .map_err(|e| ctx.err(e))?;
    if ctx.json {
        crate::print_json(&json!({"deleted": true, "full_name": repo.full_name}));
        return Ok(());
    }
    outln!("Deleted {} from your studio.", repo.full_name);
    outln!(
        "{}",
        ui.muted("Any folder with it on this machine is still there.")
    );
    Ok(())
}
