//! `atl open [REPO]`: the working copy, in the configured editor.

use std::path::{Path, PathBuf};

use serde_json::json;

use crate::ctx::{find_repo, Ctx, SCAN_DEPTH};
use crate::error::{CliError, CliResult};
use crate::ui::outln;

/// The command for an editor name from `config.toml`. Known presets map to
/// their launchers; anything else is taken as a command line of its own.
fn editor_command(editor: &str) -> Vec<String> {
    let words = |s: &str| s.split_whitespace().map(str::to_string).collect::<Vec<_>>();
    let mac_app = |app: &str| vec!["open".to_string(), "-a".to_string(), app.to_string()];
    match editor {
        "vscode" => words("code"),
        "cursor" => words("cursor"),
        "zed" => words("zed"),
        "sublime" => words("subl"),
        "fork" if cfg!(target_os = "macos") => mac_app("Fork"),
        "xcode" if cfg!(target_os = "macos") => mac_app("Xcode"),
        other => words(other),
    }
}

async fn working_copy(ctx: &Ctx, wanted: Option<&str>) -> CliResult<PathBuf> {
    let Some(wanted) = wanted else {
        return match ctx.here().await? {
            Some(here) => Ok(here.path),
            None => Err(CliError::plain("You’re not inside a repository.")
                .hint("Go to one, or say which: atl open NAME.")),
        };
    };
    let client = ctx.client()?;
    let repos = client.repos().await.map_err(|e| ctx.err(e))?;
    let repo = find_repo(&repos, wanted)?;
    // Where atl clones to, then anywhere under the clone root and watch folders.
    let usual = ctx.settings.clone_root().join(&repo.owner).join(&repo.name);
    if usual.join(".git").exists() {
        return Ok(usual);
    }
    let locals = ctx.git().scan(&ctx.settings.repo_roots(), SCAN_DEPTH).await;
    locals
        .into_iter()
        .find(|local| {
            local
                .studio_repo(&repos)
                .is_some_and(|r| r.full_name == repo.full_name)
        })
        .map(|local| local.path)
        .ok_or_else(|| {
            CliError::plain(format!(
                "{} isn’t cloned on this machine yet.",
                repo.full_name
            ))
            .hint(format!("Run atl repo clone {}.", repo.full_name))
            .kind("not_cloned")
        })
}

fn launch(command: &[String], path: &Path) -> std::io::Result<std::process::ExitStatus> {
    std::process::Command::new(&command[0])
        .args(&command[1..])
        .arg(path)
        .status()
}

pub async fn open(ctx: &Ctx, repo: Option<String>) -> CliResult<()> {
    let path = working_copy(ctx, repo.as_deref()).await?;
    let editor = ctx
        .settings
        .config
        .editor
        .clone()
        .filter(|e| !e.trim().is_empty());
    let command = editor.as_deref().map(editor_command).unwrap_or_default();

    match command.first() {
        Some(program) => match launch(&command, &path) {
            Ok(status) if status.success() => {}
            Ok(_) => {
                return Err(CliError::plain(format!(
                    "{program} couldn’t open {}.",
                    path.display()
                )))
            }
            Err(_) => {
                return Err(CliError::plain(format!(
                    "Couldn’t start {program} (editor = \"{}\").",
                    editor.as_deref().unwrap_or_default()
                ))
                .hint("Install its command-line launcher, or change editor in config.toml.")
                .kind("editor_missing"))
            }
        },
        // No editor set: let the system decide what opens a folder.
        None => open::that_detached(&path).map_err(|_| {
            CliError::plain(format!("Couldn’t open {}.", path.display()))
                .hint("Set editor = \"vscode\" (or another) in config.toml.")
        })?,
    }
    if ctx.json {
        crate::print_json(&json!({"path": path, "editor": editor}));
    } else {
        let with = editor.map(|e| format!(" in {e}")).unwrap_or_default();
        outln!(
            "Opened {}{with}.",
            ctx.ui.amber(&path.display().to_string())
        );
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn editor_presets() {
        assert_eq!(editor_command("vscode"), ["code"]);
        assert_eq!(editor_command("cursor"), ["cursor"]);
        assert_eq!(editor_command("nvim -p"), ["nvim", "-p"]);
        #[cfg(target_os = "macos")]
        assert_eq!(editor_command("fork"), ["open", "-a", "Fork"]);
    }
}
