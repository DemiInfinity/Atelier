//! Turns a studio `notify` event (or something the app just did itself)
//! into a pop-up: the friendly, plain or absent words, the colour thread,
//! and where a click should lead.

use atelier_core::api::{Messages, Moment, Notify, Thread, Tone};
use atelier_core::messages::{self, Vars};
use serde::{Deserialize, Serialize};

use crate::prefs::NotifyPrefs;

/// Where a click on a pop-up, a need or a feed item leads.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "screen", rename_all = "snake_case")]
pub enum Target {
    Today,
    /// "This device": the runner, waiting agents.
    Device,
    Pulls,
    Pull {
        repo: String,
        number: u64,
    },
    /// A working copy on this machine.
    Repo {
        path: String,
    },
    /// A page of the studio, opened in the browser.
    Web {
        web_path: String,
    },
    /// "Add a repository", on `publish`, `clone` or `new`.
    Add {
        tab: String,
    },
    Settings,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct ToastAction {
    pub label: String,
    pub target: Target,
}

/// One pop-up, for the in-app toast and the native notification alike.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct Toast {
    pub id: String,
    /// The headline: a friendly message, the plain one, or the event's title.
    pub text: String,
    /// The quiet second line (`Atelier · test job`).
    pub sub: String,
    /// The colour thread: `yours`, `pass`, `fail`, `agents` or `github`.
    pub tone: &'static str,
    /// The moment it was dressed for, if any.
    pub moment: Option<String>,
    pub action: Option<ToastAction>,
    /// Where a click on the pop-up itself leads.
    pub target: Target,
}

/// What the app knows that the event doesn't say.
#[derive(Clone, Debug, Default)]
pub struct Context {
    /// This device's name, for `{device}`.
    pub device: Option<String>,
    /// Local time as `HH:MM`, for `{time}`.
    pub time: Option<String>,
    /// Fixes the choice of message (tests, `ATELIER_MESSAGE_SEED`).
    pub seed: Option<u64>,
}

/// The last part of `owner/name`.
pub fn short_repo(full_name: &str) -> &str {
    full_name.rsplit('/').next().unwrap_or(full_name)
}

/// The preference that switches a kind of event on or off; `None` for kinds
/// that always show.
fn allowed(kind: &str, prefs: &NotifyPrefs) -> bool {
    match kind {
        "run_failed" => prefs.fail,
        "agent_approval" | "job_waiting" => prefs.agent,
        "security_alert" => prefs.security,
        "pull_review" => prefs.comment,
        "run_passed" => prefs.pass,
        _ => true,
    }
}

fn tone_for(moment: Option<Moment>, kind: &str, thread: Thread) -> &'static str {
    match moment {
        Some(Moment::Pass) => "pass",
        Some(Moment::Fail) | Some(Moment::Security) => "fail",
        Some(Moment::Agent) => "agents",
        Some(Moment::Github) => "github",
        Some(Moment::Push) | Some(Moment::Merge) => "yours",
        Some(Moment::Unknown) | None => match (kind, thread) {
            ("run_failed" | "security_alert", _) => "fail",
            ("run_passed", _) => "pass",
            (_, Thread::Github) => "github",
            (_, Thread::Agents | Thread::Connect) => "agents",
            _ => "yours",
        },
    }
}

/// `/<owner>/<name>/pulls/<n>` → the pull request it names.
pub fn pull_from_web_path(web_path: &str) -> Option<(String, u64)> {
    let mut parts = web_path.trim_matches('/').split('/');
    let (owner, name) = (parts.next()?, parts.next()?);
    if parts.next()? != "pulls" {
        return None;
    }
    let number = parts.next()?.parse().ok()?;
    Some((format!("{owner}/{name}"), number))
}

fn web_or_today(web_path: &str) -> Target {
    if web_path.is_empty() {
        Target::Today
    } else {
        Target::Web {
            web_path: web_path.to_string(),
        }
    }
}

/// The values a message's placeholders can take for this event. What the
/// event doesn't say is left out, and messages that need it aren't picked.
fn vars_for(notify: &Notify, context: &Context) -> Vars {
    // "ci failed on feature/ssh", "Pushed 2 commits to main".
    let branch = notify
        .title
        .rsplit_once(" on ")
        .or_else(|| notify.title.rsplit_once(" to "))
        .map(|(_, branch)| branch.trim())
        .filter(|b| !b.is_empty() && !b.contains(' '));
    // "weekly-tidy finished", "Approve weekly-tidy before it runs".
    let workflow = notify.kind.starts_with("agent").then(|| {
        let mut words = notify.title.split_whitespace();
        match words.next() {
            Some("Approve") => words.next().unwrap_or_default(),
            Some(first) => first,
            None => "",
        }
    });
    // "Pushed 2 commits …".
    let count = notify
        .title
        .split_whitespace()
        .zip(notify.title.split_whitespace().skip(1))
        .find(|(_, next)| next.starts_with("commit"))
        .and_then(|(n, _)| n.parse::<u64>().ok());
    Vars::new()
        .set("repo", short_repo(&notify.repo))
        .set_opt("branch", branch)
        .set_opt("workflow", workflow)
        .set_opt("count", count)
        .set_opt("device", context.device.as_deref())
        .set_opt("time", context.time.as_deref())
}

/// The pop-up for a studio event, or `None` when nothing should pop up:
/// messages are off, or this kind of event is switched off in Settings.
pub fn dress(
    notify: &Notify,
    messages: Option<&Messages>,
    prefs: &NotifyPrefs,
    context: &Context,
) -> Option<Toast> {
    if !allowed(&notify.kind, prefs) {
        return None;
    }
    let tone_setting = messages.map_or(Tone::Plain, |m| m.tone);
    if tone_setting == Tone::Off {
        return None;
    }
    let key = notify.moment.and_then(Moment::message_key);
    // An agent asking for approval isn't the "agent finished" moment.
    let key = key.filter(|_| notify.kind != "agent_approval" && notify.kind != "job_waiting");
    let dressed = match (key, messages) {
        (Some(key), Some(messages)) => {
            messages::pick(messages, key, &vars_for(notify, context), context.seed)
        }
        _ => None,
    };
    let text = dressed
        .clone()
        .filter(|t| !t.trim().is_empty())
        .unwrap_or_else(|| notify.title.clone());
    if text.trim().is_empty() {
        return None;
    }
    // When the headline is a friendly line, the facts go underneath.
    let mut sub_parts: Vec<&str> = Vec::new();
    let repo = short_repo(&notify.repo);
    if !repo.is_empty() {
        sub_parts.push(repo);
    }
    if dressed.is_some() && !notify.title.is_empty() && notify.detail.is_empty() {
        sub_parts.push(&notify.title);
    }
    let detail = notify
        .detail
        .strip_prefix(repo)
        .map(|rest| rest.trim_start_matches([' ', '·']))
        .unwrap_or(&notify.detail);
    if !detail.is_empty() {
        sub_parts.push(detail);
    }

    let pull =
        pull_from_web_path(&notify.web_path).map(|(repo, number)| Target::Pull { repo, number });
    let (target, action) = match notify.kind.as_str() {
        "run_failed" => {
            let target = web_or_today(&notify.web_path);
            let action = ToastAction {
                label: "Open the log".into(),
                target: target.clone(),
            };
            (target, Some(action))
        }
        "agent_approval" | "job_waiting" => (
            Target::Device,
            Some(ToastAction {
                label: "Review".into(),
                target: Target::Device,
            }),
        ),
        "pull_review" | "pull_merged" | "auto_merge_cancelled" => {
            (pull.unwrap_or(Target::Pulls), None)
        }
        "security_alert" => {
            let target = pull.unwrap_or_else(|| web_or_today(&notify.web_path));
            let action = ToastAction {
                label: "Review".into(),
                target: target.clone(),
            };
            (target, Some(action))
        }
        _ => (pull.unwrap_or_else(|| web_or_today(&notify.web_path)), None),
    };

    Some(Toast {
        id: if notify.id.is_empty() {
            format!("n:{}", notify.at)
        } else {
            notify.id.clone()
        },
        text,
        sub: sub_parts.join(" · "),
        tone: tone_for(notify.moment, &notify.kind, notify.thread),
        moment: key.map(str::to_string),
        action,
        target,
    })
}

/// The pop-up for something the app did itself (`push`, `first`, `late`,
/// `mile`, `clean`, `merge`). `None` when messages are off, or the moment is
/// a milestone and milestones are switched off.
pub fn local(
    moment: &str,
    messages: Option<&Messages>,
    vars: &Vars,
    sub: &str,
    target: Target,
    seed: Option<u64>,
) -> Option<Toast> {
    let messages = messages?;
    if matches!(moment, "first" | "late" | "mile") && !messages.rules.milestones {
        return None;
    }
    let text = messages::pick(messages, moment, vars, seed)?;
    Some(Toast {
        id: format!("local:{moment}:{}", seed.unwrap_or_default()),
        text,
        sub: sub.to_string(),
        tone: match moment {
            "pass" => "pass",
            "fail" => "fail",
            "agent" => "agents",
            _ => "yours",
        },
        moment: Some(moment.to_string()),
        action: None,
        target,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn web_paths_name_pull_requests() {
        assert_eq!(
            pull_from_web_path("/DemiInfinity/Tray/pulls/9"),
            Some(("DemiInfinity/Tray".into(), 9))
        );
        assert_eq!(
            pull_from_web_path("/DemiInfinity/Tray/pulls/9/files")
                .unwrap()
                .1,
            9
        );
        assert_eq!(
            pull_from_web_path("/DemiInfinity/Tray/actions/runs/9"),
            None
        );
        assert_eq!(pull_from_web_path(""), None);
        assert_eq!(short_repo("DemiInfinity/Atelier"), "Atelier");
        assert_eq!(short_repo("Atelier"), "Atelier");
    }
}
