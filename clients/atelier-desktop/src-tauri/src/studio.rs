//! Things that live in the studio and are only shown or switched here:
//! remote access, and the friendly messages. Plus the app's own settings.

use std::collections::BTreeMap;
use std::time::Duration;

use atelier_core::api::{MessageRules, Messages, MessagesUpdate, Remote, RemoteSession, Tone};
use atelier_core::messages::{self, Vars};
use serde::Serialize;

use crate::desk::{local_now, Desk};
use crate::error::{CmdError, CmdResult};
use crate::tools;

#[derive(Clone, Debug, Serialize)]
pub struct RemoteView {
    /// False when the studio doesn't offer remote access to this person.
    pub available: bool,
    pub on: bool,
    pub address: Option<String>,
    pub connected: u64,
    pub exposed: Vec<String>,
    /// The sentence under the switch.
    pub line: String,
    /// The short line for the menu bar popover.
    pub switch_line: String,
    pub sessions: Vec<RemoteSession>,
    pub error: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct PasskeyLink {
    pub url: String,
    pub expires_in: u64,
}

#[derive(Clone, Debug, Serialize)]
pub struct MessageLine {
    /// As written, with its placeholders.
    pub text: String,
    /// With the placeholders filled in, as it would pop up.
    pub preview: String,
}

#[derive(Clone, Debug, Serialize)]
pub struct MomentView {
    pub key: String,
    /// `After a push`.
    pub label: String,
    /// `yours`, `pass`, `fail` or `agents`.
    pub tone: &'static str,
    /// The quiet second line of its pop-up (`Atelier · main`).
    pub sub: String,
    pub ours: Vec<MessageLine>,
    pub yours: Vec<MessageLine>,
    pub plain: String,
}

#[derive(Clone, Debug, Serialize)]
pub struct MessagesView {
    /// `friendly`, `plain` or `off`.
    pub tone: &'static str,
    pub moments: Vec<MomentView>,
    pub rules: MessageRules,
}

/// The moments in the order they are listed, with their labels.
const MOMENTS: [(&str, &str, &str); 9] = [
    ("push", "After a push", "yours"),
    ("clean", "Nothing to commit", "yours"),
    ("pass", "Checks pass", "pass"),
    ("fail", "Checks fail", "fail"),
    ("merge", "A merge", "yours"),
    ("first", "First commit of the day", "yours"),
    ("late", "A very late push", "yours"),
    ("mile", "A milestone", "yours"),
    ("agent", "An agent finishes", "agents"),
];

pub fn remote_view(remote: &Remote, sessions: Vec<RemoteSession>) -> RemoteView {
    let people = match remote.connected {
        1 => "1 person is connected".to_string(),
        n => format!("{n} people are connected"),
    };
    let line = match (remote.on, remote.address.as_deref()) {
        (true, Some(address)) if !address.is_empty() => {
            format!("Your studio is reachable from outside at {address}. {people}.")
        }
        (true, _) => {
            format!("Your studio is reachable from outside; its address is on the way. {people}.")
        }
        (false, _) => {
            "Your studio can only be reached at home. Turn this on to reach it from anywhere."
                .to_string()
        }
    };
    RemoteView {
        available: true,
        on: remote.on,
        address: remote.address.clone().filter(|a| !a.is_empty()),
        connected: remote.connected,
        exposed: remote.exposed.clone(),
        line,
        switch_line: if remote.on {
            "On · reachable from outside".to_string()
        } else {
            "Off · your studio is home only".to_string()
        },
        sessions,
        error: None,
    }
}

fn unavailable(message: String) -> RemoteView {
    RemoteView {
        available: false,
        on: false,
        address: None,
        connected: 0,
        exposed: Vec::new(),
        line: message.clone(),
        switch_line: "Not available".to_string(),
        sessions: Vec::new(),
        error: Some(message),
    }
}

pub fn messages_view(
    messages: &Messages,
    vars: &Vars,
    sub_for: &dyn Fn(&str) -> String,
) -> MessagesView {
    let line = |text: &String| MessageLine {
        preview: messages::fill(text, vars).unwrap_or_else(|| text.clone()),
        text: text.clone(),
    };
    let mut keys: Vec<(String, String, &'static str)> = MOMENTS
        .iter()
        .filter(|(key, _, _)| messages.moments.contains_key(*key))
        .map(|(key, label, tone)| (key.to_string(), label.to_string(), *tone))
        .collect();
    // Moments a newer studio knows and this version doesn't.
    for key in messages.moments.keys() {
        if !keys.iter().any(|(k, _, _)| k == key) {
            keys.push((key.clone(), key.clone(), "yours"));
        }
    }
    MessagesView {
        tone: messages.tone.as_str(),
        moments: keys
            .into_iter()
            .map(|(key, label, tone)| {
                let moment = &messages.moments[&key];
                MomentView {
                    sub: sub_for(&key),
                    ours: moment.ours.iter().map(line).collect(),
                    yours: moment.yours.iter().map(line).collect(),
                    plain: messages
                        .plain
                        .get(&key)
                        .map(|p| messages::fill(p, vars).unwrap_or_else(|| p.clone()))
                        .unwrap_or_default(),
                    key,
                    label,
                    tone,
                }
            })
            .collect(),
        rules: messages.rules,
    }
}

impl Desk {
    // ------------------------------------------------------------- remote

    pub async fn remote_view(&self) -> RemoteView {
        let client = match self.client() {
            Ok(client) => client,
            Err(e) => return unavailable(e.message),
        };
        match self.watch(client.remote().await) {
            Ok(remote) => {
                let sessions = if remote.on {
                    client.remote_who().await.unwrap_or_default()
                } else {
                    Vec::new()
                };
                remote_view(&remote, sessions)
            }
            Err(e) => unavailable(e.message),
        }
    }

    /// Turns remote access on or off. The window asks before turning it on;
    /// off is immediate and cuts every outside connection.
    pub async fn remote_set(&self, on: bool) -> CmdResult<RemoteView> {
        let client = self.client()?;
        let remote = self.watch(client.set_remote(on).await)?;
        let sessions = if remote.on {
            client.remote_who().await.unwrap_or_default()
        } else {
            Vec::new()
        };
        Ok(remote_view(&remote, sessions))
    }

    /// A one-time link for signing in with a passkey from outside.
    pub async fn remote_link(&self) -> CmdResult<PasskeyLink> {
        let link = self.watch(self.client()?.remote_link().await)?;
        Ok(PasskeyLink {
            url: link.url,
            expires_in: link.expires_in,
        })
    }

    // ----------------------------------------------------------- settings

    pub fn set_editor(&self, id: &str) -> CmdResult<()> {
        let preset = tools::find(id)
            .filter(|p| matches!(p.role, tools::Role::Editor | tools::Role::Either))
            .ok_or_else(|| CmdError::invalid(format!("“{id}” isn’t an editor Atelier knows.")))?;
        self.update_config(|config| config.editor = Some(preset.id.to_string()))
    }

    pub fn set_git_tool(&self, id: &str) -> CmdResult<()> {
        let id = if id == tools::NO_TOOL {
            tools::NO_TOOL
        } else {
            tools::find(id)
                .filter(|p| p.role == tools::Role::GitTool)
                .ok_or_else(|| {
                    CmdError::invalid(format!("“{id}” isn’t a git tool Atelier knows."))
                })?
                .id
        };
        self.update_config(|config| config.git_tool = Some(id.to_string()))
    }

    /// One of the "Notifications on this Mac" switches.
    pub fn set_notify_pref(&self, key: &str, on: bool) -> CmdResult<()> {
        self.update_prefs(|prefs| match key {
            "fail" => prefs.notify.fail = on,
            "agent" => prefs.notify.agent = on,
            "security" => prefs.notify.security = on,
            "comment" => prefs.notify.comment = on,
            "pass" => prefs.notify.pass = on,
            _ => {}
        })
        .and_then(|_| match key {
            "fail" | "agent" | "security" | "comment" | "pass" => Ok(()),
            other => Err(CmdError::invalid(format!(
                "“{other}” isn’t a notification."
            ))),
        })
    }

    /// One of "The app" switches. Applying it (login item, menu bar icon,
    /// Dock icon) is up to the shell; this only remembers it.
    pub fn set_app_pref(&self, key: &str, on: bool) -> CmdResult<()> {
        if !matches!(key, "autostart" | "menu_bar" | "hide_dock") {
            return Err(CmdError::invalid(format!("“{key}” isn’t an app option.")));
        }
        self.update_prefs(|prefs| match key {
            "autostart" => prefs.autostart = on,
            "menu_bar" => prefs.menu_bar = on,
            _ => prefs.hide_dock = on,
        })
        .map(|_| ())
    }

    // ----------------------------------------------------------- messages

    /// Sample values for previews: a real repository of yours where there
    /// is one.
    async fn sample_vars(&self) -> (Vars, String, String) {
        let copies = self.working_copies(self.repos_cached().as_deref()).await;
        let copy = copies
            .iter()
            .find(|c| c.changes > 0 || c.ahead > 0)
            .or(copies.first());
        let repo = copy.map_or("Atelier".to_string(), |c| c.name.clone());
        let branch = copy.map_or("main".to_string(), |c| c.branch.clone());
        let now = local_now();
        let vars = Vars::new()
            .set("repo", &repo)
            .set("branch", &branch)
            .set("base", "main")
            .set("count", 2)
            .set("device", format!("your {}", self.device_name()))
            .set("time", format!("{:02}:{:02}", now.hour(), now.minute()))
            .set("workflow", "review-helper");
        (vars, repo, branch)
    }

    async fn view_of(&self, messages: &Messages) -> MessagesView {
        let (vars, repo, branch) = self.sample_vars().await;
        let now = local_now();
        let sub = move |key: &str| match key {
            "push" => format!("{repo} · {branch}"),
            "clean" => format!("{repo} · working copy"),
            "pass" => "ci · test and lint".to_string(),
            "fail" => "ci · test".to_string(),
            "merge" => format!("{repo} · main"),
            "late" => format!("{repo} · {:02}:{:02}", now.hour(), now.minute()),
            "agent" => "review-helper · pull request 14".to_string(),
            _ => repo.clone(),
        };
        messages_view(messages, &vars, &sub)
    }

    pub async fn messages_view(&self) -> CmdResult<MessagesView> {
        let messages = self.messages(Duration::ZERO).await?;
        Ok(self.view_of(&messages).await)
    }

    async fn put_messages(&self, update: MessagesUpdate) -> CmdResult<MessagesView> {
        let messages = self.watch(self.client()?.put_messages(&update).await)?;
        self.remember_messages(&messages);
        Ok(self.view_of(&messages).await)
    }

    pub async fn messages_set_tone(&self, tone: &str) -> CmdResult<MessagesView> {
        let tone = match tone {
            "friendly" => Tone::Friendly,
            "plain" => Tone::Plain,
            "off" => Tone::Off,
            other => return Err(CmdError::invalid(format!("“{other}” isn’t a tone."))),
        };
        self.put_messages(MessagesUpdate {
            tone: Some(tone),
            ..MessagesUpdate::default()
        })
        .await
    }

    /// Adds one of your own messages to a moment.
    pub async fn messages_add(&self, moment: &str, text: &str) -> CmdResult<MessagesView> {
        let current = self.messages(Duration::ZERO).await?;
        let yours = messages::yours_with(&current, moment, text)?;
        self.put_messages(MessagesUpdate {
            yours: Some(BTreeMap::from([(moment.to_string(), yours)])),
            ..MessagesUpdate::default()
        })
        .await
    }

    /// Removes your message number `number` (1-based) from a moment.
    pub async fn messages_remove(&self, moment: &str, number: usize) -> CmdResult<MessagesView> {
        let current = self.messages(Duration::ZERO).await?;
        let (yours, _) = messages::yours_without(&current, moment, number)?;
        self.put_messages(MessagesUpdate {
            yours: Some(BTreeMap::from([(moment.to_string(), yours)])),
            ..MessagesUpdate::default()
        })
        .await
    }

    pub async fn messages_set_rule(&self, rule: &str, on: bool) -> CmdResult<MessagesView> {
        let mut rules = self.messages(Duration::ZERO).await?.rules;
        match rule {
            "quiet_fullscreen" => rules.quiet_fullscreen = on,
            "terminal" => rules.terminal = on,
            "milestones" => rules.milestones = on,
            other => return Err(CmdError::invalid(format!("“{other}” isn’t a rule."))),
        }
        self.put_messages(MessagesUpdate {
            rules: Some(rules),
            ..MessagesUpdate::default()
        })
        .await
    }
}
