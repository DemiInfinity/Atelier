//! The desktop app's own preferences. They live in the shared `config.toml`
//! under `[desktop]`, a table `atl` doesn't know and therefore keeps.

use atelier_core::Config;
use serde::{Deserialize, Serialize};

const TABLE: &str = "desktop";

/// Which studio events may pop up on this machine.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct NotifyPrefs {
    /// A job fails.
    pub fail: bool,
    /// An agent is waiting for approval.
    pub agent: bool,
    /// A security alert or update.
    pub security: bool,
    /// Someone comments on or reviews a pull request.
    pub comment: bool,
    /// A job passes.
    pub pass: bool,
}

impl Default for NotifyPrefs {
    fn default() -> Self {
        NotifyPrefs {
            fail: true,
            agent: true,
            security: true,
            comment: true,
            pass: false,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct Prefs {
    pub notify: NotifyPrefs,
    /// Start the app when the person logs in.
    pub autostart: bool,
    /// Show the menu bar icon.
    pub menu_bar: bool,
    /// Keep the Dock icon hidden (macOS).
    pub hide_dock: bool,
    /// Folders the person picked with the folder chooser; repositories in
    /// them may be opened like the ones in the clone root.
    pub picked: Vec<String>,
    /// "Find your folders" was shown once (in first run, or later for
    /// someone who signed in with `atl`).
    pub folders_done: bool,
}

impl Default for Prefs {
    fn default() -> Self {
        Prefs {
            notify: NotifyPrefs::default(),
            autostart: false,
            menu_bar: true,
            hide_dock: false,
            picked: Vec::new(),
            folders_done: false,
        }
    }
}

impl Prefs {
    /// Reads `[desktop]`; anything missing or malformed falls back to the
    /// defaults.
    pub fn read(config: &Config) -> Prefs {
        config
            .extra
            .get(TABLE)
            .cloned()
            .and_then(|value| value.try_into().ok())
            .unwrap_or_default()
    }

    pub fn write(&self, config: &mut Config) {
        if let Ok(value) = toml::Value::try_from(self) {
            config.extra.insert(TABLE.to_string(), value);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn prefs_round_trip_next_to_keys_they_do_not_know() {
        let mut config: Config = toml::from_str(
            "studio = \"https://s.example\"\nfuture = 1\n[desktop]\nmenu_bar = false\nlater = \"kept?\"\n[desktop.notify]\npass = true\n",
        )
        .unwrap();
        let mut prefs = Prefs::read(&config);
        assert!(!prefs.menu_bar);
        assert!(prefs.notify.pass);
        assert!(prefs.notify.fail, "missing keys take the default");
        prefs.picked.push("/tmp/x".into());
        prefs.write(&mut config);
        let text = toml::to_string(&config).unwrap();
        assert!(text.contains("future = 1"));
        assert!(text.contains("picked = [\"/tmp/x\"]"));
        assert_eq!(Prefs::read(&toml::from_str(&text).unwrap()), prefs);
        assert_eq!(Prefs::read(&Config::default()), Prefs::default());
    }
}
