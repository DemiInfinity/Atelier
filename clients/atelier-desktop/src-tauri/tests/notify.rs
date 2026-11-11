//! What pops up for each moment, in each tone, and what the switches in
//! Settings and Messages do to it.

mod common;

use atelier_core::api::{Messages, Notify};
use atelier_core::messages::Vars;
use atelier_desktop::notify::{dress, local, Context, Target};
use atelier_desktop::prefs::NotifyPrefs;
use common::messages;
use serde_json::json;

fn event(value: serde_json::Value) -> Notify {
    serde_json::from_value(value).unwrap()
}

fn tone(tone: &str) -> Messages {
    serde_json::from_value(messages(tone)).unwrap()
}

fn context() -> Context {
    Context {
        device: Some("your MacBook".into()),
        time: Some("01:12".into()),
        seed: Some(0),
    }
}

fn all_on() -> NotifyPrefs {
    NotifyPrefs {
        pass: true,
        ..NotifyPrefs::default()
    }
}

/// One event per moment the studio sends, as in the contract.
fn events() -> Vec<(&'static str, Notify)> {
    vec![
        (
            "push",
            event(
                json!({"id": "n:1", "kind": "push", "thread": "yours", "title": "Pushed 2 commits to feature/tunnel-ui",
            "detail": "", "repo": "DemiInfinity/Atelier", "web_path": "/DemiInfinity/Atelier", "moment": "push"}),
            ),
        ),
        (
            "pass",
            event(
                json!({"id": "n:2", "kind": "run_passed", "thread": "yours", "title": "ci passed on feature/tunnel-ui",
            "detail": "Atelier · test and lint", "repo": "DemiInfinity/Atelier", "web_path": "/DemiInfinity/Atelier/actions/runs/207", "moment": "pass"}),
            ),
        ),
        (
            "fail",
            event(
                json!({"id": "n:3", "kind": "run_failed", "thread": "yours", "title": "ci failed on feature/ssh",
            "detail": "Atelier · test", "repo": "DemiInfinity/Atelier", "web_path": "/DemiInfinity/Atelier/actions/runs/208", "moment": "fail"}),
            ),
        ),
        (
            "merge",
            event(
                json!({"id": "n:4", "kind": "pull_merged", "thread": "yours", "title": "Merged pull request 13",
            "detail": "Pages custom domains", "repo": "DemiInfinity/Atelier", "web_path": "/DemiInfinity/Atelier/pulls/13", "moment": "merge"}),
            ),
        ),
        (
            "agent",
            event(
                json!({"id": "n:5", "kind": "agent_finished", "thread": "agents", "title": "review-helper finished",
            "detail": "pull request 14", "repo": "DemiInfinity/Atelier", "web_path": "/DemiInfinity/Atelier/pulls/14", "moment": "agent"}),
            ),
        ),
        (
            "github",
            event(
                json!({"id": "n:6", "kind": "pull_review", "thread": "github", "title": "A reviewer approved your pull request",
            "detail": "via Tether", "repo": "elyndrastudios/sparky-ventures", "web_path": "/elyndrastudios/sparky-ventures/pulls/63", "moment": "github"}),
            ),
        ),
        (
            "security",
            event(
                json!({"id": "n:7", "kind": "security_alert", "thread": "yours", "title": "okhttp has 1 advisory",
            "detail": "an update is ready", "repo": "DemiInfinity/Tray", "web_path": "/DemiInfinity/Tray/pulls/9", "moment": "security"}),
            ),
        ),
        (
            "none",
            event(
                json!({"id": "n:8", "kind": "agent_approval", "thread": "agents", "title": "Approve weekly-tidy before it runs",
            "detail": "first run · wants to close issues", "repo": "DemiInfinity/Atelier", "web_path": "/DemiInfinity/Atelier/actions/runs/31", "moment": null}),
            ),
        ),
    ]
}

#[test]
fn friendly_dresses_each_moment_and_keeps_the_facts_underneath() {
    let messages = tone("friendly");
    let got: Vec<(&str, String, String, &str)> = events()
        .iter()
        .map(|(name, e)| {
            let t = dress(e, Some(&messages), &all_on(), &context()).unwrap();
            (*name, t.text, t.sub, t.tone)
        })
        .collect();
    let expect = [
        (
            "push",
            "Home and dry. feature/tunnel-ui is safe in your studio.",
            "Atelier · Pushed 2 commits to feature/tunnel-ui",
            "yours",
        ),
        (
            "pass",
            "Green across the board on your MacBook.",
            "Atelier · test and lint",
            "pass",
        ),
        (
            "fail",
            "One test disagrees with you on feature/ssh.",
            "Atelier · test",
            "fail",
        ),
        (
            "merge",
            "Merged into main. Nicely done.",
            "Atelier · Pages custom domains",
            "yours",
        ),
        (
            "agent",
            "review-helper left you a note.",
            "Atelier · pull request 14",
            "agents",
        ),
        // No friendly lines exist for these: the studio's own title is shown.
        (
            "github",
            "A reviewer approved your pull request",
            "sparky-ventures · via Tether",
            "github",
        ),
        (
            "security",
            "okhttp has 1 advisory",
            "Tray · an update is ready",
            "fail",
        ),
        (
            "none",
            "Approve weekly-tidy before it runs",
            "Atelier · first run · wants to close issues",
            "agents",
        ),
    ];
    for (got, want) in got.iter().zip(expect) {
        assert_eq!((got.0, got.1.as_str(), got.2.as_str(), got.3), want);
    }
}

#[test]
fn a_different_seed_picks_a_different_line_including_your_own() {
    let messages = tone("friendly");
    let push = &events()[0].1;
    let texts: Vec<String> = (0..4)
        .map(|seed| {
            let context = Context {
                seed: Some(seed),
                ..context()
            };
            dress(push, Some(&messages), &all_on(), &context)
                .unwrap()
                .text
        })
        .collect();
    assert_eq!(
        texts,
        [
            "Home and dry. feature/tunnel-ui is safe in your studio.",
            "Off it goes. 2 commits landed on Atelier.",
            "Pushed. Checks are warming up on your MacBook.",
            "Shipped. Go outside.",
        ]
    );
}

#[test]
fn plain_says_it_plainly() {
    let messages = tone("plain");
    let got: Vec<String> = events()
        .iter()
        .map(|(_, e)| {
            dress(e, Some(&messages), &all_on(), &context())
                .unwrap()
                .text
        })
        .collect();
    assert_eq!(
        got,
        [
            "Pushed feature/tunnel-ui.",
            "Checks passed.",
            "Checks failed.",
            // The plain merge line needs {base}, which the event doesn't say.
            "Merged pull request 13",
            "review-helper finished.",
            "A reviewer approved your pull request",
            "okhttp has 1 advisory",
            "Approve weekly-tidy before it runs",
        ]
    );
}

#[test]
fn off_means_nothing_pops_up_at_all() {
    let messages = tone("off");
    for (name, e) in events() {
        assert_eq!(
            dress(&e, Some(&messages), &all_on(), &context()),
            None,
            "{name}"
        );
    }
    assert_eq!(
        local(
            "push",
            Some(&messages),
            &Vars::new().set("branch", "main"),
            "",
            Target::Today,
            Some(0)
        ),
        None
    );
}

#[test]
fn without_the_messages_the_studios_title_is_used() {
    let fail = &events()[2].1;
    let t = dress(fail, None, &all_on(), &context()).unwrap();
    assert_eq!(t.text, "ci failed on feature/ssh");
    assert_eq!(t.sub, "Atelier · test");
}

#[test]
fn the_switches_in_settings_silence_their_kind_only() {
    let messages = tone("friendly");
    let shown = |prefs: &NotifyPrefs| -> Vec<&'static str> {
        events()
            .into_iter()
            .filter(|(_, e)| dress(e, Some(&messages), prefs, &context()).is_some())
            .map(|(name, _)| name)
            .collect()
    };
    // The defaults: a passing job stays quiet.
    assert_eq!(
        shown(&NotifyPrefs::default()),
        ["push", "fail", "merge", "agent", "github", "security", "none"]
    );
    let quiet = NotifyPrefs {
        fail: false,
        agent: false,
        security: false,
        comment: false,
        pass: false,
    };
    assert_eq!(shown(&quiet), ["push", "merge", "agent"]);
}

#[test]
fn a_click_leads_to_the_right_screen() {
    let messages = tone("friendly");
    let targets: Vec<(&str, Target, Option<String>)> = events()
        .iter()
        .map(|(name, e)| {
            let t = dress(e, Some(&messages), &all_on(), &context()).unwrap();
            (*name, t.target, t.action.map(|a| a.label))
        })
        .collect();
    let web = |p: &str| Target::Web {
        web_path: p.to_string(),
    };
    let pull = |r: &str, n| Target::Pull {
        repo: r.to_string(),
        number: n,
    };
    assert_eq!(
        targets,
        [
            ("push", web("/DemiInfinity/Atelier"), None),
            ("pass", web("/DemiInfinity/Atelier/actions/runs/207"), None),
            (
                "fail",
                web("/DemiInfinity/Atelier/actions/runs/208"),
                Some("Open the log".to_string())
            ),
            ("merge", pull("DemiInfinity/Atelier", 13), None),
            ("agent", pull("DemiInfinity/Atelier", 14), None),
            ("github", pull("elyndrastudios/sparky-ventures", 63), None),
            (
                "security",
                pull("DemiInfinity/Tray", 9),
                Some("Review".to_string())
            ),
            ("none", Target::Device, Some("Review".to_string())),
        ]
    );
}

#[test]
fn the_apps_own_moments_and_the_milestones_rule() {
    let mut messages = tone("friendly");
    let vars = Vars::new()
        .set("repo", "Atelier")
        .set("branch", "main")
        .set("count", 100)
        .set("time", "01:12");
    let say = |moment: &str, messages: &Messages| {
        local(
            moment,
            Some(messages),
            &vars,
            "Atelier",
            Target::Today,
            Some(0),
        )
        .map(|t| t.text)
    };
    assert_eq!(
        say("push", &messages).as_deref(),
        Some("Home and dry. main is safe in your studio.")
    );
    assert_eq!(
        say("mile", &messages).as_deref(),
        Some("That was commit 100 on Atelier.")
    );
    assert_eq!(
        say("first", &messages).as_deref(),
        Some("Good morning. First one of the day.")
    );
    assert_eq!(
        say("late", &messages).as_deref(),
        Some("Pushed at 01:12. It will still be here tomorrow.")
    );
    messages.rules.milestones = false;
    assert_eq!(say("mile", &messages), None);
    assert_eq!(say("first", &messages), None);
    assert_eq!(say("late", &messages), None);
    assert!(
        say("push", &messages).is_some(),
        "a push still gets its word"
    );
}
