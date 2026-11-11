//! Finding the repositories that are already on the machine: someone who
//! signed in with `atl` has no watch folders, their checkouts use remotes
//! with other names, and some folders aren't in the studio at all.

mod common;

use atelier_core::Config;
use common::*;
use serde_json::json;

fn p(path: &std::path::Path) -> String {
    path.to_string_lossy().into_owned()
}

/// A git folder at `dir` with one commit and the given remotes.
async fn folder(world: &World, dir: &std::path::Path, remotes: &[(&str, &str)]) {
    std::fs::create_dir_all(dir).unwrap();
    world.g(dir, &["init", "--quiet", "-b", "main"]).await;
    world.commit(dir, "README.md", "hi\n").await;
    for (name, url) in remotes {
        world.g(dir, &["remote", "add", name, url]).await;
    }
}

/// Signed in the way `atl login` leaves things: no watch folders, an empty
/// clone root, 3 repositories in `~/Projects` and one in `~/Web-Projects`.
async fn atl_user() -> World {
    let world = World::signed_in().await;
    world.desk.update_config(|c| c.watch.clear()).unwrap();
    let home = world.home();
    // The studio checkout uses a remote called `atelier`, in another case.
    folder(
        &world,
        &home.join("Projects/Me/Atelier"),
        &[(
            "atelier",
            "ssh://git@studio.test:2223/demiinfinity/ATELIER.git",
        )],
    )
    .await;
    // Two remotes: GitHub as origin, the studio as `studio`.
    folder(
        &world,
        &home.join("Projects/Marbl"),
        &[
            ("origin", "git@github.com:someone/marbl.git"),
            ("studio", &studio_url("DemiInfinity/Marbl")),
        ],
    )
    .await;
    // Only on GitHub.
    folder(
        &world,
        &home.join("Projects/loopkit"),
        &[("origin", "https://github.com/maralind/loopkit.git")],
    )
    .await;
    // No remote at all.
    folder(&world, &home.join("Web-Projects/site"), &[]).await;
    world
        .mock_get(
            "/repos",
            json!({"repos": [
                repo_json("DemiInfinity/Atelier", json!({"clone_ssh": "ssh://git@studio.test:2223/DemiInfinity/Atelier.git"})),
                repo_json("DemiInfinity/Marbl", json!({})),
            ]}),
        )
        .await;
    world.mock_get("/status", json!({})).await;
    world
}

#[tokio::test]
async fn signed_in_with_atl_nothing_is_found_until_folders_are_added() {
    let world = atl_user().await;
    let state = world.desk.app_state().await;
    assert!(state.signed_in);
    assert!(state.watch.is_empty());
    assert!(
        !state.prefs.folders_done,
        "Find your folders hasn’t been shown yet"
    );

    let o = world.desk.overview().await.unwrap();
    assert!(o.copies.is_empty() && o.others.is_empty());

    // The home folder is looked through one level deep for folders with
    // git repositories in them.
    let home = world.home();
    std::fs::create_dir_all(home.join("Library/Developer/big/.git")).unwrap();
    std::fs::create_dir_all(home.join(".hidden/thing/.git")).unwrap();
    std::fs::create_dir_all(home.join("Empty/nothing")).unwrap();
    std::fs::create_dir_all(home.join("Documents/GitHub/tool/.git")).unwrap();
    std::fs::create_dir_all(home.join("Deep/a/b/c/d/too-deep/.git")).unwrap();
    let outside = world.base().join("outside");
    std::fs::create_dir_all(outside.join("repo/.git")).unwrap();
    #[cfg(unix)]
    std::os::unix::fs::symlink(&outside, home.join("LinkOut")).unwrap();

    let suggestions = world.desk.suggest_folders().await;
    let found: Vec<(&str, usize, bool)> = suggestions
        .iter()
        .map(|s| (s.display.as_str(), s.repos, s.watched))
        .collect();
    assert_eq!(
        found,
        [
            ("~/Projects", 3, false),
            ("~/Documents/GitHub", 1, false),
            ("~/Web-Projects", 1, false),
        ]
    );

    // Only suggestions can be added this way.
    assert_eq!(
        world
            .desk
            .add_suggested(&[p(&outside)])
            .await
            .unwrap_err()
            .kind,
        "invalid"
    );
    assert_eq!(
        world
            .desk
            .add_suggested(&["/".into()])
            .await
            .unwrap_err()
            .kind,
        "invalid"
    );
    let added = world
        .desk
        .add_suggested(&[p(&home.join("Projects")), p(&home.join("Web-Projects"))])
        .await
        .unwrap();
    assert_eq!(added, ["~/Projects", "~/Web-Projects"]);
    world.desk.mark_folders_done().unwrap();

    let config = Config::load(&world.base().join("config")).unwrap();
    assert_eq!(config.watch, ["~/Projects", "~/Web-Projects"]);
    let state = world.desk.app_state().await;
    assert!(state.prefs.folders_done);
    assert_eq!(state.watch, ["~/Projects", "~/Web-Projects"]);
    assert!(world
        .desk
        .suggest_folders()
        .await
        .iter()
        .take(1)
        .all(|s| s.watched));

    // Now they are found: by a remote that isn't `origin`, by one remote
    // among several, and without regard to case.
    let o = world.desk.overview().await.unwrap();
    let copies: Vec<(&str, Option<&str>)> = o
        .copies
        .iter()
        .map(|c| (c.name.as_str(), c.full_name.as_deref()))
        .collect();
    assert_eq!(
        copies,
        [
            ("Atelier", Some("DemiInfinity/Atelier")),
            ("Marbl", Some("DemiInfinity/Marbl")),
        ]
    );
    // The rest aren't hidden: they are listed apart, with where they live.
    let others: Vec<(&str, &str)> = o
        .others
        .iter()
        .map(|c| (c.name.as_str(), c.owner.as_str()))
        .collect();
    assert_eq!(others, [("loopkit", "github.com"), ("site", "no remote")]);
    assert!(o.others.iter().all(|c| c.full_name.is_none()));

    // And they open like any other working copy.
    let view = world.desk.repo_view(&o.copies[0].path).await.unwrap();
    assert_eq!(view.full_name.as_deref(), Some("DemiInfinity/Atelier"));
    let view = world.desk.repo_view(&o.others[0].path).await.unwrap();
    assert_eq!(view.full_name, None);
    assert_eq!(view.web_path, None);
}

#[tokio::test]
async fn watch_folders_and_the_clone_folder_can_be_changed() {
    let world = atl_user().await;
    let home = world.home();
    assert_eq!(
        world.desk.add_watch_folder(&home.join("Projects")).unwrap(),
        "~/Projects"
    );
    assert_eq!(
        world.desk.add_watch_folder(&home.join("Projects")).unwrap(),
        "~/Projects"
    );
    assert_eq!(
        world.desk.settings().config.watch,
        ["~/Projects"],
        "not added twice"
    );
    assert!(world.desk.add_watch_folder(&home.join("nope")).is_err());
    assert_eq!(world.desk.overview().await.unwrap().copies.len(), 2);

    world.desk.remove_watch_folder("~/Projects").unwrap();
    assert!(world.desk.settings().config.watch.is_empty());
    assert_eq!(
        world
            .desk
            .remove_watch_folder("~/Projects")
            .unwrap_err()
            .kind,
        "invalid"
    );
    assert!(world.desk.overview().await.unwrap().copies.is_empty());
    // Nothing on disk went anywhere.
    assert!(home.join("Projects/Me/Atelier/README.md").exists());

    // A new clone folder; the old one had a clone in it, so it stays watched.
    folder(&world, &home.join("Atelier/DemiInfinity/Old"), &[]).await;
    let new_root = home.join("Code");
    std::fs::create_dir_all(&new_root).unwrap();
    assert_eq!(world.desk.set_clone_root(&new_root).unwrap(), "~/Code");
    let config = Config::load(&world.base().join("config")).unwrap();
    assert_eq!(config.clone_root.as_deref(), Some("~/Code"));
    assert_eq!(config.watch, ["~/Atelier"]);
    assert_eq!(world.desk.settings().clone_root(), new_root);
    assert_eq!(world.desk.app_state().await.clone_root, "~/Code");
    assert!(world.desk.set_clone_root(&home.join("missing")).is_err());
}

#[tokio::test]
async fn opening_a_picked_folder_makes_it_a_working_copy_or_says_what_it_is() {
    let world = atl_user().await;
    let home = world.home();

    // In the studio (through its `atelier` remote), outside every root:
    // its parent becomes a watch folder and it opens.
    let opened = world
        .desk
        .open_folder(&home.join("Projects/Me/Atelier"))
        .await
        .unwrap();
    assert_eq!(opened.kind, "opened");
    assert_eq!(opened.full_name.as_deref(), Some("DemiInfinity/Atelier"));
    assert_eq!(world.desk.settings().config.watch, ["~/Projects/Me"]);
    assert!(world.desk.repo_view(&opened.path).await.is_ok());
    // A folder inside it opens the repository; nothing is added twice.
    std::fs::create_dir_all(home.join("Projects/Me/Atelier/docs")).unwrap();
    let again = world
        .desk
        .open_folder(&home.join("Projects/Me/Atelier/docs"))
        .await
        .unwrap();
    assert_eq!(
        (again.kind, again.path.as_str()),
        ("opened", opened.path.as_str())
    );
    assert_eq!(world.desk.settings().config.watch.len(), 1);

    // Directly in the home folder: only the folder itself is allowed, not
    // the whole home folder.
    folder(
        &world,
        &home.join("Solo"),
        &[("origin", &studio_url("DemiInfinity/Marbl"))],
    )
    .await;
    let solo = world.desk.open_folder(&home.join("Solo")).await.unwrap();
    assert_eq!(solo.kind, "opened");
    assert_eq!(world.desk.settings().config.watch.len(), 1);
    assert_eq!(world.desk.prefs().picked, std::slice::from_ref(&solo.path));

    // A remote somewhere else: says which, and offers the publish plan.
    let elsewhere = world
        .desk
        .open_folder(&home.join("Projects/loopkit"))
        .await
        .unwrap();
    assert_eq!(elsewhere.kind, "elsewhere");
    assert_eq!(
        elsewhere.remote.as_deref(),
        Some("origin → github.com/maralind/loopkit")
    );
    let plan = elsewhere.plan.unwrap();
    assert!(plan.is_repo);
    assert_eq!(
        plan.summary,
        "A git folder · 1 commit on main · its origin points somewhere else"
    );
    assert!(
        world.desk.repo_view(&elsewhere.path).await.is_ok(),
        "picked, so it may be opened"
    );

    // No remote, and not a git folder at all.
    let unpublished = world
        .desk
        .open_folder(&home.join("Web-Projects/site"))
        .await
        .unwrap();
    assert_eq!(
        (unpublished.kind, unpublished.remote.as_deref()),
        ("unpublished", None)
    );
    std::fs::create_dir_all(home.join("Notes")).unwrap();
    let plain = world.desk.open_folder(&home.join("Notes")).await.unwrap();
    assert_eq!(plain.kind, "not_repo");
    assert!(!plain.plan.unwrap().is_repo);
    assert_eq!(
        world
            .desk
            .open_folder(&home.join("gone"))
            .await
            .unwrap_err()
            .kind,
        "missing"
    );
}

#[tokio::test]
async fn a_push_without_an_upstream_goes_to_the_studios_remote() {
    let world = World::signed_in().await;
    let bare = world.bare("Atelier").await;
    let repo = world.home().join("Projects/Atelier");
    // `origin` is somewhere unreachable; the studio is the remote `atelier`.
    folder(
        &world,
        &repo,
        &[("origin", "https://github.invalid/someone/atelier.git")],
    )
    .await;
    world
        .g(
            &repo,
            &[
                "remote",
                "add",
                "atelier",
                &studio_url("DemiInfinity/Atelier"),
            ],
        )
        .await;
    // Reach the "studio" without a network: pushes to it go to the bare folder.
    world
        .g(
            &repo,
            &["config", "remote.atelier.pushurl", bare.to_str().unwrap()],
        )
        .await;
    world
        .mock_get(
            "/repos",
            json!({"repos": [repo_json("DemiInfinity/Atelier", json!({}))]}),
        )
        .await;
    world.mock_get("/messages", messages("plain")).await;

    let pushed = world.desk.repo_push(&p(&repo)).await.unwrap();
    assert_eq!((pushed.commits, pushed.branch.as_str()), (1, "main"));
    assert_eq!(pushed.toast.unwrap().text, "Pushed main.");
    assert_eq!(
        world.g(&bare, &["rev-parse", "main"]).await,
        world.g(&repo, &["rev-parse", "HEAD"]).await
    );
    assert_eq!(
        world
            .g(&repo, &["rev-parse", "--abbrev-ref", "@{upstream}"])
            .await,
        "atelier/main"
    );
}
