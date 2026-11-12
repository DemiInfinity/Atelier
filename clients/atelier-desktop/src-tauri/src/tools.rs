//! Editors the app can open a folder in. The table is fixed
//! and built in; nothing from the studio or from a config file is ever run
//! as a command, and nothing goes through a shell.

use std::path::Path;

use serde::Serialize;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Role {
    Editor,
    /// Not offered as an editor; opened only when asked for by id.
    Terminal,
    /// A file manager, offered with the editors.
    Either,
}

#[derive(Clone, Copy, Debug)]
pub struct Preset {
    pub id: &'static str,
    pub label: &'static str,
    pub role: Role,
    /// The application's name for `open -a` on macOS.
    mac_app: Option<&'static str>,
    /// The launcher on the `PATH` elsewhere.
    cli: Option<&'static str>,
}

pub const PRESETS: [Preset; 7] = [
    Preset {
        id: "vscode",
        label: "VS Code",
        role: Role::Editor,
        mac_app: Some("Visual Studio Code"),
        cli: Some("code"),
    },
    Preset {
        id: "cursor",
        label: "Cursor",
        role: Role::Editor,
        mac_app: Some("Cursor"),
        cli: Some("cursor"),
    },
    Preset {
        id: "zed",
        label: "Zed",
        role: Role::Editor,
        mac_app: Some("Zed"),
        cli: Some("zed"),
    },
    Preset {
        id: "xcode",
        label: "Xcode",
        role: Role::Editor,
        mac_app: Some("Xcode"),
        cli: None,
    },
    Preset {
        id: "android-studio",
        label: "Android Studio",
        role: Role::Editor,
        mac_app: Some("Android Studio"),
        cli: Some("studio"),
    },
    Preset {
        id: "terminal",
        label: "Terminal",
        role: Role::Terminal,
        mac_app: Some("Terminal"),
        cli: None,
    },
    Preset {
        id: "finder",
        label: "Finder",
        role: Role::Either,
        mac_app: None,
        cli: None,
    },
];

pub const DEFAULT_EDITOR: &str = "vscode";

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct PresetInfo {
    pub id: String,
    pub label: String,
}

pub fn find(id: &str) -> Option<&'static Preset> {
    PRESETS.iter().find(|p| p.id == id)
}

/// The label of the file manager on this system.
fn finder_label(os: &str) -> &'static str {
    match os {
        "macos" => "Finder",
        "windows" => "Explorer",
        _ => "Files",
    }
}

fn info(preset: &Preset, os: &str) -> PresetInfo {
    PresetInfo {
        id: preset.id.to_string(),
        label: if preset.id == "finder" {
            finder_label(os).to_string()
        } else {
            preset.label.to_string()
        },
    }
}

pub fn editors(os: &str) -> Vec<PresetInfo> {
    PRESETS
        .iter()
        .filter(|p| matches!(p.role, Role::Editor | Role::Either))
        .filter(|p| launch_plan(p.id, Path::new("/"), os).is_some())
        .map(|p| info(p, os))
        .collect()
}

/// The configured editor when it is a preset, otherwise the default. (`atl`
/// accepts any command in `editor`; the app never runs one.)
pub fn editor_or_default(configured: Option<&str>) -> &'static Preset {
    configured
        .and_then(find)
        .filter(|p| matches!(p.role, Role::Editor | Role::Either))
        .or_else(|| find(DEFAULT_EDITOR))
        .expect("the default editor is in the table")
}

/// The program and arguments that open `folder` in preset `id` on `os`
/// (`macos`, `windows`, anything else is treated as Linux). `None` when the
/// preset doesn't exist there.
pub fn launch_plan(id: &str, folder: &Path, os: &str) -> Option<(String, Vec<String>)> {
    let preset = find(id)?;
    let folder = folder.to_string_lossy().into_owned();
    let plan = |program: &str, args: Vec<String>| Some((program.to_string(), args));
    match (os, preset.id) {
        ("macos", "finder") => plan("open", vec!["--".into(), folder]),
        ("macos", _) => plan(
            "open",
            vec!["-a".into(), preset.mac_app?.into(), "--".into(), folder],
        ),
        ("windows", "finder") => plan("explorer", vec![folder]),
        ("windows", "terminal") => plan("wt", vec!["-d".into(), folder]),
        (_, "finder") => plan("xdg-open", vec![folder]),
        (_, "terminal") if os != "windows" => plan(
            "x-terminal-emulator",
            vec![format!("--working-directory={folder}")],
        ),
        (_, "xcode") => None,
        _ => plan(preset.cli?, vec![folder]),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn every_preset_opens_without_a_shell_on_macos() {
        let folder = Path::new("/Users/x/Atelier/me/my app; rm -rf ~");
        for preset in PRESETS {
            let (program, args) = launch_plan(preset.id, folder, "macos").unwrap();
            assert_eq!(program, "open", "{}", preset.id);
            // The folder is one argument, after `--`, never interpolated.
            assert_eq!(args.last().unwrap(), "/Users/x/Atelier/me/my app; rm -rf ~");
            assert_eq!(args[args.len() - 2], "--");
        }
        assert_eq!(
            launch_plan("vscode", folder, "macos").unwrap().1[..2],
            ["-a".to_string(), "Visual Studio Code".to_string()]
        );
        assert_eq!(launch_plan("finder", folder, "macos").unwrap().1.len(), 2);
    }

    #[test]
    fn unknown_ids_and_commands_are_never_run() {
        let folder = Path::new("/x");
        assert_eq!(launch_plan("sh -c 'curl evil'", folder, "macos"), None);
        assert_eq!(launch_plan("", folder, "macos"), None);
        assert_eq!(launch_plan("/bin/sh", folder, "linux"), None);
        assert_eq!(editor_or_default(Some("my-own-script")).id, "vscode");
        assert_eq!(
            editor_or_default(Some("terminal")).id,
            "vscode",
            "the terminal isn’t an editor"
        );
        assert_eq!(launch_plan("fork", folder, "macos"), None);
        assert_eq!(editor_or_default(Some("cursor")).id, "cursor");
        assert_eq!(editor_or_default(None).id, "vscode");
    }

    #[test]
    fn other_systems_use_their_own_launchers() {
        let folder = Path::new("/home/x/code");
        assert_eq!(
            launch_plan("vscode", folder, "linux").unwrap(),
            ("code".to_string(), vec!["/home/x/code".to_string()])
        );
        assert_eq!(
            launch_plan("finder", folder, "linux").unwrap().0,
            "xdg-open"
        );
        assert_eq!(
            launch_plan("finder", folder, "windows").unwrap().0,
            "explorer"
        );
        assert_eq!(launch_plan("xcode", folder, "linux"), None);
        assert!(editors("linux").iter().all(|e| e.id != "xcode"));
        assert!(editors("macos").iter().any(|e| e.id == "xcode"));
        assert!(editors("macos").iter().all(|e| e.id != "terminal"));
        assert_eq!(
            launch_plan("terminal", folder, "macos").unwrap().1[..2],
            ["-a".to_string(), "Terminal".to_string()]
        );
        assert_eq!(editors("windows").last().unwrap().label, "Explorer");
    }
}
