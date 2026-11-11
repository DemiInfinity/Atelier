//! Working copies through the command layer, on throwaway repositories:
//! which folders may be touched, changes and diffs, committing exactly the
//! ticked files, pushing to a local bare repository, branches, history,
//! and adding repositories.

mod common;

use atelier_core::diff::{FileStatus, LineKind};
use atelier_core::git::{ChangeKind, Segment};
use common::*;
use serde_json::json;

fn p(path: &std::path::Path) -> String {
    path.to_string_lossy().into_owned()
}

async fn world_with_copy() -> (World, std::path::PathBuf, std::path::PathBuf) {
    let world = World::signed_in().await;
    let (repo, bare) = world.copy("DemiInfinity/Atelier").await;
    world
        .mock_get(
            "/repos",
            json!({"repos": [repo_json("DemiInfinity/Atelier", json!({"open_pulls": 1}))]}),
        )
        .await;
    world.mock_get("/messages", messages("friendly")).await;
    (world, repo, bare)
}

#[tokio::test]
async fn folders_outside_the_roots_are_refused_everywhere() {
    let (world, repo, _) = world_with_copy().await;
    // A real repository, but not under the clone root or a watch folder.
    let outside = world.base().join("elsewhere/secret");
    std::fs::create_dir_all(&outside).unwrap();
    world.g(&outside, &["init", "--quiet", "-b", "main"]).await;
    world.commit(&outside, "a.txt", "a\n").await;
    write(&outside, "a.txt", "changed\n");
    let out = p(&outside);
    let sneaky = format!("{}/../../../../elsewhere/secret", p(&repo));

    for path in [out.as_str(), sneaky.as_str(), "relative/path", "", "/"] {
        assert_eq!(
            world.desk.repo_view(path).await.unwrap_err().kind,
            "outside",
            "{path:?}"
        );
        assert!(world.desk.repo_changes(path).await.is_err());
        assert!(world.desk.repo_diff(path, "a.txt").await.is_err());
        assert!(world
            .desk
            .repo_commit(path, &["a.txt".into()], "msg", "", false)
            .await
            .is_err());
        assert!(world
            .desk
            .repo_discard(path, &["a.txt".into()])
            .await
            .is_err());
        assert!(world.desk.repo_undo_commit(path).await.is_err());
        assert!(world.desk.repo_switch(path, "main").await.is_err());
        assert!(world.desk.repo_push(path).await.is_err());
        assert!(world.desk.repo_pull(path).await.is_err());
        assert!(world.desk.repo_fetch(path).await.is_err());
        assert!(world.desk.repo_history(path).await.is_err());
        assert!(world.desk.repo_open(path, None).await.is_err());
        assert!(world.desk.repo_hand_over(path).await.is_err());
        assert!(world.desk.folder_init(path).await.is_err());
    }
    // Nothing happened out there, and nothing was launched.
    assert_eq!(
        std::fs::read_to_string(outside.join("a.txt")).unwrap(),
        "changed\n"
    );
    assert_eq!(
        world.g(&outside, &["rev-list", "--count", "HEAD"]).await,
        "1"
    );
    assert!(world.effects.launched.lock().unwrap().is_empty());

    // A folder inside a root that isn't a repository's top folder.
    let plain = world.home().join("Projects/notes");
    std::fs::create_dir_all(&plain).unwrap();
    assert_eq!(
        world.desk.repo_view(&p(&plain)).await.unwrap_err().kind,
        "not_a_repo"
    );
    let sub = repo.join("docs");
    std::fs::create_dir_all(&sub).unwrap();
    let view = world.desk.repo_view(&p(&sub)).await.unwrap();
    assert_eq!(
        view.path,
        p(&repo),
        "a subfolder resolves to its repository"
    );

    // Once the person picks the outside folder, it is allowed, and stays so.
    world.desk.register_picked(&outside).unwrap();
    assert_eq!(world.desk.repo_changes(&out).await.unwrap().files.len(), 1);
    assert_eq!(world.desk.prefs().picked, std::slice::from_ref(&out));
    // Inside the clone root already: not recorded again.
    world.desk.register_picked(&repo).unwrap();
    assert_eq!(world.desk.prefs().picked.len(), 1);
}

#[tokio::test]
async fn the_bar_knows_the_repository_and_who_commits() {
    let (world, repo, _) = world_with_copy().await;
    world.g(&repo, &["config", "user.name", "Demi"]).await;
    write(&repo, "new.txt", "x\n");
    world.commit(&repo, "ahead.txt", "a\n").await;
    world.desk.repo_fetch(&p(&repo)).await.unwrap();
    let view = world.desk.repo_view(&p(&repo)).await.unwrap();
    assert_eq!(view.name, "Atelier");
    assert_eq!(view.owner, "DemiInfinity");
    assert_eq!(view.full_name.as_deref(), Some("DemiInfinity/Atelier"));
    assert_eq!(view.display_path, "~/Atelier/DemiInfinity/Atelier");
    assert_eq!(view.branch.as_deref(), Some("main"));
    assert_eq!(
        (
            view.ahead,
            view.behind,
            view.changes,
            view.branches,
            view.open_pulls
        ),
        (1, 0, 1, 1, 1)
    );
    assert_eq!(view.web_path.as_deref(), Some("/DemiInfinity/Atelier"));
    assert_eq!(
        view.committing_as.as_deref(),
        Some("Demi · personal identity")
    );
    assert_eq!(view.editor.label, "VS Code");
    assert_eq!(view.git_tool.unwrap().label, "Fork");
    assert!(view.fetched_at.is_some());
}

#[tokio::test]
async fn exactly_the_ticked_files_are_committed_and_pushed_to_the_bare_repo() {
    let (world, repo, bare) = world_with_copy().await;
    let path = p(&repo);
    write(&repo, "README.md", "hello\nworld\n");
    write(&repo, "tray.go", "package main\n");
    write(&repo, "secret.env", "TOKEN=1\n");
    write(&repo, "notes.md", "later\n");
    world.g(&repo, &["add", "notes.md"]).await; // staged by hand, not ticked

    let changes = world.desk.repo_changes(&path).await.unwrap();
    let files: Vec<(&str, ChangeKind)> = changes
        .files
        .iter()
        .map(|f| (f.path.as_str(), f.kind))
        .collect();
    assert_eq!(
        files,
        [
            ("README.md", ChangeKind::Modified),
            ("notes.md", ChangeKind::Added),
            ("secret.env", ChangeKind::Untracked),
            ("tray.go", ChangeKind::Untracked),
        ]
    );
    assert_eq!(changes.clean, None);

    let ticked = vec!["README.md".to_string(), "tray.go".to_string()];
    let result = world
        .desk
        .repo_commit(&path, &ticked, "Add the tray", "It has a switch.", true)
        .await
        .unwrap();
    assert!(result.pushed, "{:?}", result.push_error);
    assert_eq!(result.short.len(), 7);

    // In the commit: the two ticked files, nothing else.
    let committed = world
        .g(&repo, &["show", "--name-only", "--format=", "HEAD"])
        .await;
    let mut names: Vec<&str> = committed.lines().collect();
    names.sort_unstable();
    assert_eq!(names, ["README.md", "tray.go"]);
    assert_eq!(
        world.g(&repo, &["log", "-1", "--format=%s|%b"]).await,
        "Add the tray|It has a switch."
    );
    // Left exactly as they were: one still staged, one still untracked.
    let left = world.desk.repo_changes(&path).await.unwrap();
    let left: Vec<(&str, ChangeKind, bool)> = left
        .files
        .iter()
        .map(|f| (f.path.as_str(), f.kind, f.staged))
        .collect();
    assert_eq!(
        left,
        [
            ("notes.md", ChangeKind::Added, true),
            ("secret.env", ChangeKind::Untracked, false)
        ]
    );
    // The bare repository has the commit, and not the unticked file.
    assert_eq!(
        world.g(&bare, &["rev-parse", "main"]).await,
        world.g(&repo, &["rev-parse", "HEAD"]).await
    );
    let in_studio = world
        .g(&bare, &["ls-tree", "-r", "--name-only", "main"])
        .await;
    assert!(!in_studio.contains("secret.env") && !in_studio.contains("notes.md"));

    // The app says the `push` moment itself, with this repository's words.
    let toast = result.toast.expect("a push pop-up");
    assert_eq!(toast.moment.as_deref(), Some("push"));
    assert_eq!(toast.text, "Home and dry. main is safe in your studio.");
    assert_eq!(toast.sub, "Atelier · 1 commit");
    assert_eq!(toast.tone, "yours");

    // Nonsense is refused and commits nothing.
    let head = world.g(&repo, &["rev-parse", "HEAD"]).await;
    assert!(world
        .desk
        .repo_commit(&path, &[], "msg", "", false)
        .await
        .is_err());
    assert!(world
        .desk
        .repo_commit(&path, &["secret.env".into()], " ", "", false)
        .await
        .is_err());
    assert!(world
        .desk
        .repo_commit(&path, &["../x".into()], "msg", "", false)
        .await
        .is_err());
    assert!(world
        .desk
        .repo_commit(&path, &["README.md".into()], "msg", "", false)
        .await
        .is_err());
    assert_eq!(world.g(&repo, &["rev-parse", "HEAD"]).await, head);
}

#[tokio::test]
async fn a_commit_without_a_push_and_a_push_that_is_refused() {
    let (world, repo, bare) = world_with_copy().await;
    let path = p(&repo);
    write(&repo, "a.txt", "a\n");
    let result = world
        .desk
        .repo_commit(&path, &["a.txt".into()], "Add a", "", false)
        .await
        .unwrap();
    assert!(!result.pushed && result.push_error.is_none());
    // The second commit of the repository today is neither the first of the
    // day nor a milestone: nothing pops up.
    assert_eq!(result.toast, None);
    assert_ne!(world.g(&bare, &["rev-parse", "main"]).await, result.sha);

    // The studio moves on; the push is refused with a plain sentence, and
    // "commit and push" still keeps the commit.
    let other = world.base().join("other");
    world
        .g(
            &world.base(),
            &[
                "clone",
                "--quiet",
                bare.to_str().unwrap(),
                other.to_str().unwrap(),
            ],
        )
        .await;
    world.commit(&other, "theirs.txt", "t\n").await;
    world.g(&other, &["push", "--quiet"]).await;
    let err = world.desk.repo_push(&path).await.unwrap_err();
    assert_eq!(err.kind, "push_rejected");
    assert!(err.hint.unwrap().contains("Pull first"));
    write(&repo, "b.txt", "b\n");
    let result = world
        .desk
        .repo_commit(&path, &["b.txt".into()], "Add b", "", true)
        .await
        .unwrap();
    assert!(!result.pushed);
    assert!(result.push_error.unwrap().contains("refused"));
    assert_eq!(world.g(&repo, &["log", "-1", "--format=%s"]).await, "Add b");

    // Pull is fast-forward only, and says so when it can't be.
    let err = world.desk.repo_pull(&path).await.unwrap_err();
    assert!(err.message.contains("both moved on"), "{}", err.message);
}

#[tokio::test]
async fn pull_fast_forwards_and_nothing_to_commit_gets_a_friendly_line() {
    let (world, repo, bare) = world_with_copy().await;
    let path = p(&repo);
    let other = world.base().join("other");
    world
        .g(
            &world.base(),
            &[
                "clone",
                "--quiet",
                bare.to_str().unwrap(),
                other.to_str().unwrap(),
            ],
        )
        .await;
    world.commit(&other, "theirs.txt", "t\n").await;
    world.g(&other, &["push", "--quiet"]).await;
    world.desk.repo_fetch(&path).await.unwrap();
    assert_eq!(world.desk.repo_view(&path).await.unwrap().behind, 1);
    assert_eq!(world.desk.repo_pull(&path).await.unwrap(), 1);
    assert_eq!(world.desk.repo_pull(&path).await.unwrap(), 0);
    assert!(repo.join("theirs.txt").exists());

    let changes = world.desk.repo_changes(&path).await.unwrap();
    assert!(changes.files.is_empty());
    assert_eq!(
        changes.clean.as_deref(),
        Some("All tidy. The kettle is free.")
    );
}

#[tokio::test]
async fn switching_branches_refuses_to_lose_uncommitted_work() {
    let (world, repo, _) = world_with_copy().await;
    let path = p(&repo);
    world
        .desk
        .repo_new_branch(&path, "feature/x", None)
        .await
        .unwrap();
    world.commit(&repo, "README.md", "from the feature\n").await;
    let names: Vec<(String, bool)> = world
        .desk
        .repo_branches(&path)
        .await
        .unwrap()
        .into_iter()
        .map(|b| (b.name, b.current))
        .collect();
    assert_eq!(
        names,
        [("feature/x".to_string(), true), ("main".to_string(), false)]
    );

    write(&repo, "README.md", "uncommitted\n");
    let err = world.desk.repo_switch(&path, "main").await.unwrap_err();
    assert_eq!(err.kind, "invalid");
    assert_eq!(
        err.message,
        "Switching to main would lose your uncommitted changes to README.md. Commit or discard them first."
    );
    assert_eq!(
        world.g(&repo, &["branch", "--show-current"]).await,
        "feature/x"
    );
    assert_eq!(
        std::fs::read_to_string(repo.join("README.md")).unwrap(),
        "uncommitted\n"
    );

    // Discarding (the window asks first) clears the way.
    world
        .desk
        .repo_discard(&path, &["README.md".into()])
        .await
        .unwrap();
    world.desk.repo_switch(&path, "main").await.unwrap();
    assert_eq!(world.g(&repo, &["branch", "--show-current"]).await, "main");

    // Only branches that are there; never an option.
    assert!(world.desk.repo_switch(&path, "origin/main").await.is_err());
    assert!(world.desk.repo_switch(&path, "--orphan").await.is_err());
    assert!(world.desk.repo_new_branch(&path, "-D", None).await.is_err());
    assert!(world
        .desk
        .repo_new_branch(&path, "ok", Some("HEAD; rm -rf /"))
        .await
        .is_err());
}

#[tokio::test]
async fn diffs_arrive_as_structured_lines() {
    let (world, repo, _) = world_with_copy().await;
    let path = p(&repo);
    world
        .commit(&repo, "old.go", "package a\n\nfunc a() {}\nfunc b() {}\n")
        .await;
    world.commit(&repo, "logo.bin", "x").await;
    world.commit(&repo, "tail.txt", "end").await;
    world.commit(&repo, "gone.md", "bye\n").await;

    write(&repo, "README.md", "hello\n<script>alert(1)</script>\n");
    world.g(&repo, &["mv", "old.go", "new.go"]).await;
    std::fs::write(repo.join("logo.bin"), [0u8, 1, 2]).unwrap();
    write(&repo, "tail.txt", "END");
    std::fs::remove_file(repo.join("gone.md")).unwrap();
    write(&repo, "fresh.md", "# new\nfile\n");

    let d = world.desk.repo_diff(&path, "README.md").await.unwrap();
    assert_eq!(
        (d.status, d.lang.as_str(), d.additions, d.deletions),
        (FileStatus::Modified, "Markdown", 1, 0)
    );
    let add = d.lines.iter().find(|l| l.kind == LineKind::Add).unwrap();
    assert_eq!(
        (add.old, add.new, add.text.as_str()),
        (None, Some(2), "<script>alert(1)</script>")
    );
    assert_eq!(d.lines[0].kind, LineKind::Hunk);

    let d = world.desk.repo_diff(&path, "new.go").await.unwrap();
    assert_eq!(d.status, FileStatus::Renamed);
    assert_eq!(d.old_path.as_deref(), Some("old.go"));
    assert_eq!(d.lang, "Go · renamed");
    assert!(d.lines.is_empty());

    let d = world.desk.repo_diff(&path, "logo.bin").await.unwrap();
    assert!(d.binary && d.lines.is_empty());
    assert_eq!(d.lang, "Binary");

    let d = world.desk.repo_diff(&path, "tail.txt").await.unwrap();
    let kinds: Vec<LineKind> = d.lines.iter().map(|l| l.kind).collect();
    assert_eq!(
        kinds,
        [
            LineKind::Hunk,
            LineKind::Delete,
            LineKind::NoNewline,
            LineKind::Add,
            LineKind::NoNewline
        ]
    );

    let d = world.desk.repo_diff(&path, "gone.md").await.unwrap();
    assert_eq!(d.status, FileStatus::Deleted);
    assert_eq!(d.lang, "Markdown · deleted");
    assert_eq!(
        (d.lines[1].kind, d.lines[1].old),
        (LineKind::Delete, Some(1))
    );

    let d = world.desk.repo_diff(&path, "fresh.md").await.unwrap();
    assert_eq!((d.status, d.additions), (FileStatus::Added, 2));
    assert_eq!(d.lang, "Markdown · new file");
    assert_eq!(d.lines[2].new, Some(2));

    // Only files that changed can be read this way.
    assert!(world.desk.repo_diff(&path, ".git/config").await.is_err());
    assert!(world
        .desk
        .repo_diff(&path, "../../../../etc/hosts")
        .await
        .is_err());
    assert!(world
        .desk
        .repo_diff(&path, "unchanged-or-missing.txt")
        .await
        .is_err());
}

#[tokio::test]
async fn history_is_graph_rows_with_pills_and_undo_only_for_the_unpushed_tip() {
    let (world, repo, _) = world_with_copy().await;
    let path = p(&repo);
    world.commit(&repo, "one.txt", "1\n").await;
    world.g(&repo, &["push", "--quiet"]).await;
    world.g(&repo, &["tag", "v0.9"]).await;
    world.commit(&repo, "two.txt", "2\n").await;
    world.commit(&repo, "three.txt", "3\n").await;

    let h = world.desk.repo_history(&path).await.unwrap();
    assert_eq!((h.ahead, h.behind), (2, 0));
    assert_eq!(h.upstream.as_deref(), Some("local/main"));
    let rows: Vec<(&str, u8, Segment, Segment, bool)> = h
        .rows
        .iter()
        .map(|r| (r.subject.as_str(), r.lane, r.line0, r.line1, r.unpushed))
        .collect();
    assert_eq!(
        rows,
        [
            ("change three.txt", 1, Segment::None, Segment::Start, true),
            ("change two.txt", 1, Segment::None, Segment::Full, true),
            ("change one.txt", 0, Segment::Start, Segment::Join, false),
            ("change README.md", 0, Segment::Full, Segment::None, false),
        ]
    );
    let pills = |i: usize| -> Vec<(String, &'static str)> {
        h.rows[i]
            .pills
            .iter()
            .map(|p| (p.text.clone(), p.tone))
            .collect()
    };
    assert_eq!(pills(0), [("main".to_string(), "yours")]);
    assert_eq!(
        pills(2),
        [
            ("v0.9".to_string(), "quiet"),
            ("local/main".to_string(), "quiet")
        ]
    );
    assert!(h.rows[0].date.contains('T'));

    let tip = world
        .desk
        .repo_commit_detail(&path, &h.rows[0].sha)
        .await
        .unwrap();
    assert!(tip.can_undo && tip.detail.is_head && !tip.detail.pushed);
    assert_eq!(tip.detail.files[0].path, "three.txt");
    let pushed = world
        .desk
        .repo_commit_detail(&path, &h.rows[2].short)
        .await
        .unwrap();
    assert!(!pushed.can_undo && pushed.detail.pushed);
    let json = serde_json::to_value(&tip).unwrap();
    assert_eq!(
        json["subject"], "change three.txt",
        "the detail is flattened for the window"
    );
    assert_eq!(json["can_undo"], true);

    // Undo twice takes back both unpushed commits; the third is pushed.
    assert_eq!(
        world.desk.repo_undo_commit(&path).await.unwrap(),
        "change three.txt"
    );
    assert_eq!(
        world.desk.repo_undo_commit(&path).await.unwrap(),
        "change two.txt"
    );
    let err = world.desk.repo_undo_commit(&path).await.unwrap_err();
    assert!(err.message.contains("already pushed"));
    let staged = world.desk.repo_changes(&path).await.unwrap();
    assert_eq!(staged.files.len(), 2, "the undone work is still there");
    assert!(staged.files.iter().all(|f| f.staged));
}

#[tokio::test]
async fn cloning_puts_it_under_the_clone_root_with_the_accounts_identity() {
    let world = World::signed_in().await;
    // The studio's copy is a local bare repository reached over file://.
    let bare = world.bare("nova").await;
    let seed = world.base().join("seed");
    world
        .g(
            &world.base(),
            &[
                "clone",
                "--quiet",
                bare.to_str().unwrap(),
                seed.to_str().unwrap(),
            ],
        )
        .await;
    world.commit(&seed, "README.md", "# nova\n").await;
    world
        .g(&seed, &["push", "--quiet", "origin", "HEAD:main"])
        .await;
    let url = format!("file://{}", bare.display());
    world
        .mock_get(
            "/repos",
            json!({"repos": [repo_json("elyndrastudios/nova", json!({"owner_kind": "company", "clone_https": url}))]}),
        )
        .await;

    let added = world
        .desk
        .clone_studio("elyndrastudios/nova")
        .await
        .unwrap();
    let dest = world.home().join("Atelier/elyndrastudios/nova");
    assert_eq!(added.path, p(&dest));
    assert!(!added.already);
    assert!(dest.join("README.md").exists());
    // The company identity, in this repository's own config only.
    let local = std::fs::read_to_string(dest.join(".git/config")).unwrap();
    assert!(
        local.contains("name = Demi at Elyndra") && local.contains("email = demi@company.example")
    );
    assert!(!world.home().join(".gitconfig").exists());

    assert_eq!(
        world
            .desk
            .clone_studio("elyndrastudios/missing")
            .await
            .unwrap_err()
            .kind,
        "not_found"
    );
    assert_eq!(
        world.desk.clone_studio("../../etc").await.unwrap_err().kind,
        "invalid"
    );
    // Something else already in the way.
    std::fs::create_dir_all(world.home().join("Atelier/DemiInfinity/Ingle")).unwrap();
    world.server.reset().await;
    world.mock_get("/me", me()).await;
    world
        .mock_get(
            "/repos",
            json!({"repos": [repo_json("DemiInfinity/Ingle", json!({"clone_https": url}))]}),
        )
        .await;
    assert_eq!(
        world
            .desk
            .clone_studio("DemiInfinity/Ingle")
            .await
            .unwrap_err()
            .kind,
        "exists"
    );
}

#[tokio::test]
async fn a_studio_cannot_steer_a_clone_out_of_the_clone_root() {
    let world = World::signed_in().await;
    world
        .mock_get(
            "/repos",
            json!({"repos": [{"full_name": "a/b", "owner": "..", "name": "..", "clone_https": "file:///nowhere"}]}),
        )
        .await;
    assert_eq!(
        world.desk.clone_studio("a/b").await.unwrap_err().kind,
        "invalid"
    );
    assert!(std::fs::read_dir(world.home().join("Atelier"))
        .unwrap()
        .next()
        .is_none());
}

#[tokio::test]
async fn publishing_a_picked_folder_creates_the_repository_and_pushes() {
    let world = World::signed_in().await;
    let bare = world.bare("Ingle").await;
    let url = format!("file://{}", bare.display());
    world
        .mock(
            "POST",
            "/repos",
            201,
            repo_json("DemiInfinity/Ingle", json!({"clone_https": url})),
        )
        .await;
    world.mock_get("/repos", json!({"repos": []})).await;

    // A folder somewhere the app doesn't look, picked by the person.
    let folder = world.base().join("Code/Ingle");
    std::fs::create_dir_all(&folder).unwrap();
    let plan = world.desk.folder_plan(&folder).await.unwrap();
    assert!(!plan.is_repo);
    world.desk.folder_init(&plan.path).await.unwrap();
    let plan = world.desk.folder_plan(&folder).await.unwrap();
    assert!(plan.is_repo);
    assert_eq!(
        plan.summary,
        "A git folder · no commits yet · not in your studio yet"
    );
    let err = world
        .desk
        .publish(&plan.path, "DemiInfinity", "Ingle", true, false)
        .await
        .unwrap_err();
    assert!(err.message.contains("no commits"), "{}", err.message);

    world.commit(&folder, "main.go", "package main\n").await;
    write(&folder, "untracked.txt", "stays here\n");
    let plan = world.desk.folder_plan(&folder).await.unwrap();
    assert_eq!(
        plan.summary,
        "A git folder · 1 commit on main · not in your studio yet"
    );
    assert_eq!(plan.name, "Ingle");

    let added = world
        .desk
        .publish(&plan.path, "DemiInfinity", "Ingle", true, false)
        .await
        .unwrap();
    assert_eq!(added.full_name, "DemiInfinity/Ingle");
    assert_eq!(added.web_path, "/DemiInfinity/Ingle");
    assert_eq!(
        world.received("POST", "/repos").await,
        [json!({"owner": "DemiInfinity", "name": "Ingle", "private": true, "description": ""})]
    );
    assert_eq!(
        world.g(&bare, &["rev-parse", "main"]).await,
        world.g(&folder, &["rev-parse", "HEAD"]).await
    );
    // The folder stays where it is, with its untracked file untouched, and
    // gets the personal identity locally.
    assert!(folder.join("untracked.txt").exists());
    assert_eq!(
        world.g(&folder, &["config", "--local", "user.email"]).await,
        "demi@example.com"
    );

    // Publishing again: origin is already somewhere.
    let err = world
        .desk
        .publish(&plan.path, "DemiInfinity", "Ingle", true, false)
        .await
        .unwrap_err();
    assert_eq!(err.kind, "origin_exists");
    assert!(world
        .desk
        .publish(&plan.path, "Demi/../x", "Ingle", true, false)
        .await
        .is_err());
}

#[tokio::test]
async fn starting_new_makes_the_repository_a_working_copy_and_a_first_commit() {
    let world = World::signed_in().await;
    let bare = world.bare("fresh").await;
    let url = format!("file://{}", bare.display());
    world
        .mock(
            "POST",
            "/repos",
            201,
            repo_json("DemiInfinity/my-new-thing", json!({"clone_https": url})),
        )
        .await;
    let added = world
        .desk
        .create_new("DemiInfinity", "my-new-thing", true, true, true)
        .await
        .unwrap();
    let dest = world.home().join("Atelier/DemiInfinity/my-new-thing");
    assert_eq!(added.path, p(&dest));
    assert_eq!(
        std::fs::read_to_string(dest.join("README.md")).unwrap(),
        "# my-new-thing\n"
    );
    assert_eq!(
        world.g(&dest, &["log", "--format=%s"]).await,
        "Start my-new-thing"
    );
    assert_eq!(
        world.g(&dest, &["config", "--local", "user.email"]).await,
        "demi@example.com"
    );
    assert_eq!(
        world.g(&bare, &["rev-parse", "main"]).await,
        world.g(&dest, &["rev-parse", "HEAD"]).await
    );
    assert_eq!(added.note.as_deref(), Some("Opened in VS Code."));
    assert_eq!(world.effects.launched.lock().unwrap().len(), 1);

    assert_eq!(
        world
            .desk
            .create_new("DemiInfinity", "my-new-thing", true, true, false)
            .await
            .unwrap_err()
            .kind,
        "exists"
    );
    assert_eq!(
        world
            .desk
            .create_new("DemiInfinity", "bad name", true, true, false)
            .await
            .unwrap_err()
            .kind,
        "invalid"
    );
}

#[tokio::test]
async fn search_lists_studio_and_github_hits_and_mirrors_before_cloning() {
    let world = World::signed_in().await;
    let (_repo, _) = world.copy("DemiInfinity/loop-notes").await;
    let bare = world.bare("loopkit").await;
    let seed = world.base().join("seed");
    world
        .g(
            &world.base(),
            &[
                "clone",
                "--quiet",
                bare.to_str().unwrap(),
                seed.to_str().unwrap(),
            ],
        )
        .await;
    world.commit(&seed, "README.md", "# loopkit\n").await;
    world
        .g(&seed, &["push", "--quiet", "origin", "HEAD:main"])
        .await;
    let url = format!("file://{}", bare.display());
    world
        .mock_get(
            "/repos",
            json!({
                "repos": [repo_json("DemiInfinity/loop-notes", json!({})), repo_json("DemiInfinity/loopkit", json!({"clone_https": url}))],
                "github_hits": [{"full_name": "maralind/loopkit", "description": "", "atelier_badge": true},
                                {"full_name": "maralind/loopkit-samples", "description": "", "atelier_badge": false}]
            }),
        )
        .await;
    world
        .mock(
            "POST",
            "/repos/mirror",
            202,
            json!({"full_name": "DemiInfinity/loopkit"}),
        )
        .await;

    let found = world.desk.search("loop").await.unwrap();
    let hits: Vec<(&str, &str, &str, &str)> = found
        .hits
        .iter()
        .map(|h| (h.title.as_str(), h.line.as_str(), h.action, h.label))
        .collect();
    let machine = world.desk.machine();
    assert_eq!(
        hits,
        [
            (
                "DemiInfinity / loop-notes",
                format!("In your studio · private · already on this {machine}").as_str(),
                "open",
                "Open"
            ),
            (
                "DemiInfinity / loopkit",
                "In your studio · private",
                "clone",
                "Clone"
            ),
            (
                "maralind / loopkit",
                "On GitHub · via Tether · uses Atelier",
                "mirror",
                "Mirror and clone"
            ),
            (
                "maralind / loopkit-samples",
                "On GitHub · via Tether",
                "mirror",
                "Mirror and clone"
            ),
        ]
    );

    let added = world
        .desk
        .mirror_and_clone("maralind/loopkit")
        .await
        .unwrap();
    assert_eq!(added.full_name, "DemiInfinity/loopkit");
    assert_eq!(
        world.received("POST", "/repos/mirror").await,
        [json!({"github": "maralind/loopkit"})]
    );
    assert!(world
        .home()
        .join("Atelier/DemiInfinity/loopkit/README.md")
        .exists());
    // Asking again finds it already here.
    assert!(world
        .desk
        .clone_studio("DemiInfinity/loopkit")
        .await
        .is_ok_and(|a| a.path == added.path));
}
