//! The seven-and-one changes through the command layer: a repository's own
//! pull requests with their checks at a glance, renaming and deleting it in
//! the studio, pins and folded owner groups, the owner remembered when
//! publishing, runs gathered by pull request, and issues.

mod common;

use atelier_desktop::prefs::Prefs;
use common::*;
use serde_json::{json, Value};
use wiremock::matchers::{header, method, path, query_param};
use wiremock::{Mock, ResponseTemplate};

fn p(path: &std::path::Path) -> String {
    path.to_string_lossy().into_owned()
}

/// `/repos` answers `first` once, then `after` from then on.
async fn repos_then(world: &World, first: Value, after: Value) {
    Mock::given(method("GET"))
        .and(path(format!("{API}/repos")))
        .respond_with(ResponseTemplate::new(200).set_body_json(first))
        .up_to_n_times(1)
        .mount(&world.server)
        .await;
    world.mock_get("/repos", after).await;
}

fn summary(repo: &str, number: u64, head: &str, checks: Value) -> Value {
    json!({"repo": repo, "number": number, "title": format!("Pull {number}"), "state": "open",
        "head": head, "base": "main", "author": "owner:DemiInfinity",
        "merge": {"can_merge": false}, "checks": checks})
}

// ------------------------------------------------- 1 and 4: pulls, checks

#[tokio::test]
async fn a_repository_lists_its_own_pull_requests_with_checks_at_a_glance() {
    let world = World::signed_in().await;
    Mock::given(method("GET"))
        .and(path(format!("{API}/pulls")))
        .and(query_param("state", "open"))
        .and(query_param("repo", "DemiInfinity/Atelier"))
        .and(header("authorization", format!("Bearer {TOKEN}")))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({"pulls": [
            summary("DemiInfinity/Atelier", 14, "feature/tunnel-ui",
                json!({"total": 4, "passed": 3, "failed": 0, "pending": 1, "state": "pending"})),
            summary("DemiInfinity/Atelier", 15, "fix", json!({"total": 2, "passed": 1, "failed": 1, "pending": 0, "state": "failing"})),
            summary("DemiInfinity/Atelier", 16, "docs", json!({"total": 0, "passed": 0, "failed": 0, "pending": 0, "state": "none"})),
            // An older studio: counts only, and no `checks` at all.
            summary("DemiInfinity/Atelier", 17, "old", json!({"passed": 0, "failed": 0, "pending": 2, "waiting": 0})),
            summary("DemiInfinity/Atelier", 18, "older", Value::Null),
            // And one that ignored the filter.
            summary("DemiInfinity/Tray", 9, "other", json!({"total": 2, "passed": 2, "failed": 0, "pending": 0, "state": "passing"})),
        ]})))
        .expect(1)
        .mount(&world.server)
        .await;

    let rows = world
        .desk
        .pulls_list(Some("DemiInfinity/Atelier"))
        .await
        .unwrap();
    let seen: Vec<(u64, &str, &str, &str)> = rows
        .iter()
        .map(|r| {
            (
                r.number,
                r.checks.state,
                r.checks.text.as_str(),
                r.checks.tone,
            )
        })
        .collect();
    assert_eq!(
        seen,
        [
            (14, "pending", "3 of 4 passed", "yours"),
            (15, "failing", "1 failing", "fail"),
            (16, "none", "no checks", "quiet"),
            (17, "pending", "running", "yours"),
            (18, "none", "no checks", "quiet"),
        ]
    );
    assert!(rows.iter().all(|r| r.repo == "DemiInfinity/Atelier"));
    assert_eq!(rows[0].head, "feature/tunnel-ui");
    assert_eq!(
        world.desk.pulls_list(Some("nope")).await.unwrap_err().kind,
        "invalid"
    );
}

#[tokio::test]
async fn the_list_across_the_studio_carries_the_checks_too() {
    let world = World::signed_in().await;
    world
        .mock_get(
            "/pulls",
            json!({"pulls": [
                summary("DemiInfinity/Atelier", 14, "a", json!({"total": 4, "passed": 4, "failed": 0, "pending": 0, "state": "passing"})),
                summary("DemiInfinity/Tray", 9, "b", json!({"passed": 0, "failed": 0, "pending": 0, "waiting": 1})),
            ]}),
        )
        .await;
    let rows = world.desk.pulls_list(None).await.unwrap();
    assert_eq!(rows.len(), 2);
    assert_eq!(
        (rows[0].checks.text.as_str(), rows[0].checks.tone),
        ("all 4 passed", "pass")
    );
    assert_eq!(
        (rows[1].checks.text.as_str(), rows[1].checks.tone),
        ("waiting for you", "agents")
    );
    // The request across the studio doesn't name a repository.
    let asked = world.server.received_requests().await.unwrap();
    let pulls: Vec<_> = asked
        .iter()
        .filter(|r| r.url.path().ends_with("/pulls"))
        .collect();
    assert_eq!(pulls.len(), 1);
    assert!(!pulls[0].url.query().unwrap_or_default().contains("repo="));
}

#[tokio::test]
async fn runs_are_gathered_by_pull_request() {
    let world = World::signed_in().await;
    Mock::given(method("GET"))
        .and(path(format!("{API}/runs")))
        .and(query_param("repo", "DemiInfinity/Atelier"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({"runs": [
            {"run_id": 209, "repo": "DemiInfinity/Atelier", "workflow": "image", "event": "push", "branch": "feature/tunnel-ui",
             "status": "running", "started_at": "2026-11-11T09:14:02Z", "web_path": "/DemiInfinity/Atelier/actions/runs/209"},
            {"run_id": 208, "repo": "DemiInfinity/Atelier", "workflow": "ci", "event": "push", "branch": "feature/ssh",
             "status": "failure", "started_at": "2026-11-11T09:00:00Z", "duration_s": 123},
            {"run_id": 207, "repo": "DemiInfinity/Atelier", "workflow": "ci", "event": "push", "branch": "feature/tunnel-ui",
             "status": "success", "started_at": "2026-11-11T08:00:00Z", "duration_s": 108},
            {"run_id": 1, "repo": "DemiInfinity/Tray", "workflow": "ci", "branch": "feature/tunnel-ui", "status": "success"}
        ]})))
        .mount(&world.server)
        .await;
    world
        .mock_get(
            "/pulls",
            json!({"pulls": [summary("DemiInfinity/Atelier", 14, "feature/tunnel-ui", Value::Null)]}),
        )
        .await;
    let view = world.desk.repo_runs("DemiInfinity/Atelier").await.unwrap();
    assert_eq!(view.web_path, "/DemiInfinity/Atelier/actions");
    assert_eq!(view.groups.len(), 2);
    assert_eq!(view.groups[0].pull, Some(14));
    assert_eq!(view.groups[0].label, "Pull request 14 · Pull 14");
    assert_eq!(
        view.groups[0]
            .runs
            .iter()
            .map(|r| (r.run_id, r.status.text.as_str()))
            .collect::<Vec<_>>(),
        [(209, "Running"), (207, "Passed")]
    );
    assert_eq!(view.groups[1].label, "Other runs");
    assert_eq!(view.groups[1].runs.len(), 1);
    assert!(view.groups[1].runs[0].failed);
    assert_eq!(view.groups[1].runs[0].duration, "2m 03s");
}

// ------------------------------------------------------ 2: rename, delete

#[tokio::test]
async fn renaming_updates_the_studio_and_the_remotes_that_pointed_there() {
    let world = World::signed_in().await;
    let (repo, bare) = world.copy("DemiInfinity/Atelier").await;
    world
        .g(
            &repo,
            &[
                "remote",
                "add",
                "github",
                "https://github.com/DemiInfinity/Atelier.git",
            ],
        )
        .await;
    repos_then(
        &world,
        json!({"repos": [repo_json("DemiInfinity/Atelier", json!({})), repo_json("DemiInfinity/Tray", json!({}))]}),
        json!({"repos": [repo_json("DemiInfinity/Studio", json!({})), repo_json("DemiInfinity/Tray", json!({}))]}),
    )
    .await;
    world
        .mock(
            "PATCH",
            "/repos/DemiInfinity/Atelier",
            200,
            repo_json("DemiInfinity/Studio", json!({})),
        )
        .await;

    // Not a name: refused before the studio is asked.
    for bad in ["", "two words", "a/b", ".."] {
        assert!(
            world.desk.repo_rename(&p(&repo), bad).await.is_err(),
            "{bad:?}"
        );
    }
    assert!(world
        .received("PATCH", "/repos/DemiInfinity/Atelier")
        .await
        .is_empty());

    let renamed = world.desk.repo_rename(&p(&repo), " Studio ").await.unwrap();
    assert_eq!(renamed.full_name, "DemiInfinity/Studio");
    assert_eq!(renamed.name, "Studio");
    assert_eq!(renamed.remotes, ["origin"]);
    assert_eq!(renamed.remote_error, None);
    assert_eq!(
        world.received("PATCH", "/repos/DemiInfinity/Atelier").await,
        [json!({"name": "Studio"})]
    );
    let url = |name: &'static str| {
        let (world, repo) = (&world, &repo);
        async move {
            world
                .g(repo, &["remote", "get-url", name])
                .await
                .trim()
                .to_string()
        }
    };
    assert_eq!(url("origin").await, studio_url("DemiInfinity/Studio"));
    assert_eq!(
        url("github").await,
        "https://github.com/DemiInfinity/Atelier.git"
    );
    assert_eq!(url("local").await, p(&bare));

    // The folder is the same one, now known by the new name.
    let view = world.desk.repo_view(&p(&repo)).await.unwrap();
    assert_eq!(view.full_name.as_deref(), Some("DemiInfinity/Studio"));
    assert_eq!(view.name, "Studio");
}

#[tokio::test]
async fn a_refused_rename_changes_nothing_here() {
    let world = World::signed_in().await;
    let (repo, _) = world.copy("DemiInfinity/Atelier").await;
    world
        .mock_get(
            "/repos",
            json!({"repos": [repo_json("DemiInfinity/Atelier", json!({}))]}),
        )
        .await;
    world
        .mock(
            "PATCH",
            "/repos/DemiInfinity/Atelier",
            409,
            json!({"error": "name_taken", "message": "There is already a repository called Tray."}),
        )
        .await;
    let err = world.desk.repo_rename(&p(&repo), "Tray").await.unwrap_err();
    assert_eq!(err.kind, "name_taken");
    assert_eq!(err.message, "There is already a repository called Tray.");
    assert_eq!(
        world
            .g(&repo, &["remote", "get-url", "origin"])
            .await
            .trim(),
        studio_url("DemiInfinity/Atelier")
    );
    assert_eq!(
        world
            .desk
            .repo_rename(&p(&repo), "Atelier")
            .await
            .unwrap_err()
            .kind,
        "invalid"
    );

    // A folder the studio doesn't know can't be renamed there.
    let other = world.home().join("Projects/loose");
    std::fs::create_dir_all(&other).unwrap();
    world.g(&other, &["init", "--quiet", "-b", "main"]).await;
    world.commit(&other, "a.txt", "a\n").await;
    assert_eq!(
        world
            .desk
            .repo_rename(&p(&other), "New")
            .await
            .unwrap_err()
            .kind,
        "not_in_studio"
    );
    assert_eq!(
        world
            .desk
            .repo_delete(&p(&other), "loose")
            .await
            .unwrap_err()
            .kind,
        "not_in_studio"
    );
}

#[tokio::test]
async fn deleting_needs_the_name_typed_and_keeps_the_folder() {
    let world = World::signed_in().await;
    let (repo, _) = world.copy("DemiInfinity/Atelier").await;
    write(&repo, "notes.md", "not committed\n");
    let both = json!({"repos": [repo_json("DemiInfinity/Atelier", json!({}))]});
    // Three wrong tries and the right one read the list; afterwards it is gone.
    for _ in 0..4 {
        Mock::given(method("GET"))
            .and(path(format!("{API}/repos")))
            .respond_with(ResponseTemplate::new(200).set_body_json(both.clone()))
            .up_to_n_times(1)
            .mount(&world.server)
            .await;
    }
    world.mock_get("/repos", json!({"repos": []})).await;
    Mock::given(method("DELETE"))
        .and(path(format!("{API}/repos/DemiInfinity/Atelier")))
        .and(header("authorization", format!("Bearer {TOKEN}")))
        .respond_with(ResponseTemplate::new(204))
        .mount(&world.server)
        .await;

    for wrong in ["", "atelier", "Atelier2"] {
        let err = world.desk.repo_delete(&p(&repo), wrong).await.unwrap_err();
        assert_eq!(err.kind, "confirm_mismatch", "{wrong:?}");
        assert_eq!(err.message, "Type Atelier to delete it.");
    }
    assert!(world
        .received("DELETE", "/repos/DemiInfinity/Atelier")
        .await
        .is_empty());

    let deleted = world.desk.repo_delete(&p(&repo), "Atelier").await.unwrap();
    assert_eq!(deleted, "DemiInfinity/Atelier");
    assert_eq!(
        world
            .received("DELETE", "/repos/DemiInfinity/Atelier")
            .await,
        [json!({"confirm": "DemiInfinity/Atelier"})]
    );

    // Everything on this machine is as it was, uncommitted work included.
    assert!(repo.join(".git").is_dir());
    assert_eq!(
        std::fs::read_to_string(repo.join("notes.md")).unwrap(),
        "not committed\n"
    );
    assert_eq!(
        world
            .g(&repo, &["remote", "get-url", "origin"])
            .await
            .trim(),
        studio_url("DemiInfinity/Atelier")
    );
    // And it now shows under "Other folders".
    world.mock_get("/status", status()).await;
    let overview = world.desk.overview().await.unwrap();
    assert!(overview.copies.is_empty());
    assert_eq!(overview.others.len(), 1);
    assert_eq!(overview.others[0].path, p(&repo));
    assert_eq!(overview.others[0].full_name, None);
}

#[tokio::test]
async fn a_device_that_may_not_administer_is_told_where_to_go_instead() {
    let world = World::signed_in().await;
    let (repo, _) = world.copy("DemiInfinity/Atelier").await;
    world
        .mock_get(
            "/repos",
            json!({"repos": [repo_json("DemiInfinity/Atelier", json!({}))]}),
        )
        .await;
    // The switch for this device is off (the default): /me says so too.
    assert!(!world.desk.app_state().await.can_admin);
    world
        .mock(
            "PATCH",
            "/repos/DemiInfinity/Atelier",
            403,
            json!({"error": "device_not_allowed", "message": "This device isn't allowed to rename or delete repositories. Turn it on in Settings → Devices."}),
        )
        .await;
    // Any other 403: the person may not administer this repository.
    world
        .mock(
            "DELETE",
            "/repos/DemiInfinity/Atelier",
            403,
            json!({"error": "forbidden", "message": "You can’t do that."}),
        )
        .await;
    let err = world
        .desk
        .repo_rename(&p(&repo), "Studio")
        .await
        .unwrap_err();
    assert_eq!(err.kind, "device_not_allowed");
    assert!(
        err.message
            .contains("isn’t allowed to rename or delete repositories yet"),
        "{}",
        err.message
    );
    assert!(err.hint.unwrap().contains("Settings → Devices"));
    let err = world
        .desk
        .repo_delete(&p(&repo), "Atelier")
        .await
        .unwrap_err();
    assert_eq!(err.kind, "not_allowed");
    assert_eq!(err.message, "You can’t rename or delete Atelier from here.");
    assert!(err.hint.unwrap().contains("Atelier’s settings page"));
    assert_eq!(
        world
            .g(&repo, &["remote", "get-url", "origin"])
            .await
            .trim(),
        studio_url("DemiInfinity/Atelier")
    );
}

#[tokio::test]
async fn what_the_device_may_do_is_read_again_when_asked_fresh() {
    let world = World::build_with_me(json!({})).await;
    let me = |can_admin: bool| {
        let mut me = me();
        me["device"]["can_admin"] = json!(can_admin);
        me
    };
    world.server.reset().await;
    Mock::given(method("GET"))
        .and(path(format!("{API}/me")))
        .respond_with(ResponseTemplate::new(200).set_body_json(me(false)))
        .up_to_n_times(1)
        .mount(&world.server)
        .await;
    world.mock_get("/me", me(true)).await;
    assert!(!world.desk.app_state().await.can_admin);
    // Within a minute the remembered answer is used…
    assert!(!world.desk.app_state().await.can_admin);
    // …unless it is asked for fresh (the window came back).
    assert!(world.desk.app_state_fresh().await.can_admin);
    assert!(world.desk.app_state().await.can_admin);
}

// ------------------------------------------------ 3 and 6: groups and pins

#[tokio::test]
async fn copies_say_whose_they_are_and_pins_and_folds_are_remembered() {
    let world = World::signed_in().await;
    let (atelier, _) = world.copy("DemiInfinity/Atelier").await;
    let (sparky, _) = world.copy("elyndrastudios/sparky-ventures").await;
    world
        .mock_get(
            "/repos",
            json!({"repos": [
                repo_json("DemiInfinity/Atelier", json!({})),
                repo_json("elyndrastudios/sparky-ventures", json!({"owner_kind": "company"})),
            ]}),
        )
        .await;
    world.mock_get("/status", status()).await;

    let overview = world.desk.overview().await.unwrap();
    let seen: Vec<_> = overview
        .copies
        .iter()
        .map(|c| {
            (
                c.name.as_str(),
                c.owner.as_str(),
                c.owner_name.as_str(),
                c.owner_kind.as_str(),
                c.pinned,
            )
        })
        .collect();
    assert_eq!(
        seen,
        [
            ("Atelier", "DemiInfinity", "DemiInfinity", "user", false),
            (
                "sparky-ventures",
                "Elyndra Studios LTD",
                "elyndrastudios",
                "company",
                false
            ),
        ]
    );

    world.desk.set_pinned(&p(&sparky), true).await.unwrap();
    world.desk.set_pinned(&p(&sparky), true).await.unwrap();
    assert_eq!(world.desk.prefs().pinned, [p(&sparky)]);
    let overview = world.desk.overview().await.unwrap();
    assert_eq!(
        overview.copies.iter().map(|c| c.pinned).collect::<Vec<_>>(),
        [false, true]
    );
    // Only folders the app may touch can be pinned.
    assert_eq!(
        world.desk.set_pinned("/etc", true).await.unwrap_err().kind,
        "outside"
    );
    world.desk.set_pinned(&p(&atelier), true).await.unwrap();
    world.desk.set_pinned(&p(&sparky), false).await.unwrap();
    assert_eq!(world.desk.prefs().pinned, [p(&atelier)]);

    world
        .desk
        .set_group_collapsed("ElyndraStudios", true)
        .unwrap();
    world
        .desk
        .set_group_collapsed("elyndrastudios", true)
        .unwrap();
    assert_eq!(world.desk.prefs().collapsed, ["elyndrastudios"]);
    assert!(world.desk.set_group_collapsed("  ", true).is_err());

    // It is all in config.toml under [desktop], and read back by a new start.
    let text = std::fs::read_to_string(world.base().join("config/config.toml")).unwrap();
    assert!(text.contains("[desktop]"), "{text}");
    assert!(text.contains("collapsed = [\"elyndrastudios\"]"), "{text}");
    let config: atelier_core::Config = toml::from_str(&text).unwrap();
    let prefs = Prefs::read(&config);
    assert_eq!(prefs.pinned, [p(&atelier)]);
    assert_eq!(world.desk.app_state().await.prefs, prefs);

    world
        .desk
        .set_group_collapsed("elyndrastudios", false)
        .unwrap();
    assert!(world.desk.prefs().collapsed.is_empty());
}

// ------------------------------------------------------- 7: who it is for

#[tokio::test]
async fn every_owner_is_offered_and_the_last_one_is_remembered() {
    let world = World::build_with_me(json!({
        "person": {"name": "Demi", "login": "DemiInfinity"},
        "accounts": [
            {"name": "DemiInfinity", "display": "Demi", "kind": "user", "commit_name": "Demi", "commit_email": "demi@example.com"},
            {"name": "elyndrastudios", "display": "Elyndra Studios LTD", "kind": "company", "commit_name": "Demi", "commit_email": "demi@company.example"}
        ],
        "owners": [
            {"name": "DemiInfinity", "display_name": "Demi", "kind": "personal"},
            {"name": "sidequest", "display_name": "Side Quest", "kind": "company"}
        ],
        "studio": {"name": "Atelier"}
    }))
    .await;
    let state = world.desk.app_state().await;
    assert_eq!(
        state
            .accounts
            .iter()
            .map(|a| (a.name.as_str(), a.kind.as_str()))
            .collect::<Vec<_>>(),
        [
            ("DemiInfinity", "user"),
            ("elyndrastudios", "company"),
            ("sidequest", "company")
        ]
    );
    assert_eq!(state.prefs.default_owner, None);

    let bare = world.bare("Ingle").await;
    world
        .mock(
            "POST",
            "/repos",
            201,
            repo_json(
                "elyndrastudios/Ingle",
                json!({"clone_https": format!("file://{}", bare.display()), "owner_kind": "company"}),
            ),
        )
        .await;
    world.mock_get("/repos", json!({"repos": []})).await;
    let folder = world.base().join("Code/Ingle");
    std::fs::create_dir_all(&folder).unwrap();
    world.g(&folder, &["init", "--quiet", "-b", "main"]).await;
    world.commit(&folder, "main.go", "package main\n").await;
    let plan = world.desk.folder_plan(&folder).await.unwrap();
    let added = world
        .desk
        .publish(&plan.path, "elyndrastudios", "Ingle", true, false)
        .await
        .unwrap();
    assert_eq!(added.full_name, "elyndrastudios/Ingle");
    assert_eq!(
        world.received("POST", "/repos").await[0]["owner"],
        json!("elyndrastudios")
    );
    assert_eq!(
        world.desk.app_state().await.prefs.default_owner.as_deref(),
        Some("elyndrastudios")
    );
    // The company's commit identity went into the folder.
    assert_eq!(
        world.g(&folder, &["config", "user.email"]).await.trim(),
        "demi@company.example"
    );
}

// ---------------------------------------------------------------- 8: issues

#[tokio::test]
async fn issues_list_read_open_comment_close_and_reopen() {
    let world = World::signed_in().await;
    Mock::given(method("GET"))
        .and(path(format!("{API}/issues")))
        .and(query_param("repo", "DemiInfinity/Atelier"))
        .and(query_param("state", "open"))
        .and(query_param("page", "1"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({"issues": [
            {"repo": "DemiInfinity/Atelier", "number": 7, "title": "Tray icon is <b>blurry</b>", "state": "open",
             "author": "owner:DemiInfinity", "via": "tether", "labels": [{"name": "bug", "color": "D73A4A"}, {"name": "odd", "color": "url(x)"}],
             "comments": 2, "created_at": "2026-11-01T10:00:00Z", "updated_at": "2026-11-02T10:00:00Z",
             "web_url": "https://elsewhere.example/phish"},
            {"repo": "DemiInfinity/Atelier", "number": 5, "title": "Closed one", "state": "closed"},
            {"repo": "DemiInfinity/Tray", "number": 1, "title": "Other repo", "state": "open"}
        ], "has_more": true})))
        .mount(&world.server)
        .await;
    world
        .mock_get(
            "/repos/DemiInfinity/Atelier/issues/7",
            json!({"repo": "DemiInfinity/Atelier", "number": 7, "title": "Tray icon is <b>blurry</b>", "state": "open",
                "author": "owner:DemiInfinity", "labels": [{"name": "bug", "color": "#d73a4a"}],
                "body": "On a **retina** screen.\n<img src=x onerror=alert(1)>",
                "comments": [{"id": 1, "author": "Mara", "via": null, "kind": "person", "body": "Same <i>here</i>.", "created_at": "2026-11-01T11:00:00Z"},
                             {"id": 2, "author": "CodeRabbit[bot]", "via": "tether", "kind": "bot", "body": "Noted.", "created_at": "2026-11-01T12:00:00Z"}],
                "created_at": "2026-11-01T10:00:00Z", "web_url": "https://elsewhere.example/phish"}),
        )
        .await;
    world
        .mock(
            "POST",
            "/repos/DemiInfinity/Atelier/issues",
            201,
            json!({"number": 9}),
        )
        .await;
    world
        .mock(
            "POST",
            "/repos/DemiInfinity/Atelier/issues/7/comments",
            201,
            json!({}),
        )
        .await;
    world
        .mock(
            "PATCH",
            "/repos/DemiInfinity/Atelier/issues/7",
            200,
            json!({}),
        )
        .await;

    let list = world
        .desk
        .issues_list("DemiInfinity/Atelier", "open", 0)
        .await
        .unwrap();
    assert!(list.has_more);
    assert_eq!(list.page, 1);
    assert_eq!(list.rows.len(), 1, "only this repository's open issues");
    let row = &list.rows[0];
    assert_eq!((row.number, row.comments, row.open), (7, 2, true));
    assert_eq!(row.by, "you");
    let via = row.via.as_ref().unwrap();
    assert_eq!((via.text.as_str(), via.tone), ("via Tether", "github"));
    assert_eq!(row.title, "Tray icon is <b>blurry</b>");
    assert_eq!(row.labels[0].color.as_deref(), Some("#d73a4a"));
    assert_eq!(row.labels[1].color, None);
    assert!(world
        .desk
        .issues_list("DemiInfinity/Atelier", "merged", 1)
        .await
        .is_err());

    let issue = world
        .desk
        .issue_detail("DemiInfinity/Atelier", 7)
        .await
        .unwrap();
    assert!(
        issue.body.contains("<img src=x"),
        "markup is carried as text"
    );
    assert_eq!(issue.comments.len(), 2);
    assert_eq!(
        (issue.comments[0].who.as_str(), issue.comments[0].tone),
        ("Mara", "yours")
    );
    assert_eq!(
        (issue.comments[1].tag.as_str(), issue.comments[1].tone),
        ("bot · via Tether", "github")
    );
    // The page to open is worked out here, never taken from the answer.
    assert_eq!(issue.web_path, "/DemiInfinity/Atelier/issues/7");

    assert!(world
        .desk
        .issue_create("DemiInfinity/Atelier", "  ", "x")
        .await
        .is_err());
    assert_eq!(
        world
            .desk
            .issue_create("DemiInfinity/Atelier", " New thing ", " Please. ")
            .await
            .unwrap(),
        Some(9)
    );
    assert_eq!(
        world
            .received("POST", "/repos/DemiInfinity/Atelier/issues")
            .await,
        [json!({"title": "New thing", "body": "Please."})]
    );
    assert!(world
        .desk
        .issue_comment("DemiInfinity/Atelier", 7, " \n ")
        .await
        .is_err());
    world
        .desk
        .issue_comment("DemiInfinity/Atelier", 7, "Fixed in main.\n")
        .await
        .unwrap();
    assert_eq!(
        world
            .received("POST", "/repos/DemiInfinity/Atelier/issues/7/comments")
            .await,
        [json!({"body": "Fixed in main."})]
    );
    world
        .desk
        .issue_set_open("DemiInfinity/Atelier", 7, false)
        .await
        .unwrap();
    world
        .desk
        .issue_set_open("DemiInfinity/Atelier", 7, true)
        .await
        .unwrap();
    assert_eq!(
        world
            .received("PATCH", "/repos/DemiInfinity/Atelier/issues/7")
            .await,
        [json!({"state": "closed"}), json!({"state": "open"})]
    );
}
