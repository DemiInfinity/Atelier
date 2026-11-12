//! The git client's commands through the command layer, on the fixture
//! repository (`scripts/git-fixture.sh`): each area's wrappers answer with
//! what the engine found, and refuse folders Atelier doesn't look after.

mod common;

use atelier_core::git::branch::{PullMode, SwitchHow};
use atelier_core::git::conflict::{Choice, TakeSide};
use atelier_core::git::history::GraphQuery;
use atelier_core::git::integrate::{MergeMode, OperationKind, PlanAction, PlanStep, ResetMode};
use atelier_core::git::stage::{ChunkLineKind, LineRef, Selection, Side};
use atelier_core::git::undo::UndoOptions;
use atelier_core::git::worktree::WorktreeSource;
use common::*;

fn p(path: &std::path::Path) -> String {
    path.to_string_lossy().into_owned()
}

#[tokio::test]
async fn the_fixture_is_what_the_screens_are_promised() {
    let world = World::signed_in().await;
    let repo = world.git_fixture("DemiInfinity/Tunnel");
    let path = p(&repo);

    // Changes: two staged, one not, with two chunks.
    let stage = world.desk.stage_status(&path).await.unwrap();
    assert_eq!(stage.status.branch.as_deref(), Some("feature/tunnel-ui"));
    let staged: Vec<&str> = stage
        .status
        .staged
        .iter()
        .map(|e| e.path.as_str())
        .collect();
    assert_eq!(
        staged,
        [
            "cmd/atelier-desktop/remote.go",
            "cmd/atelier-desktop/tray.go"
        ]
    );
    let unstaged: Vec<&str> = stage
        .status
        .unstaged
        .iter()
        .map(|e| e.path.as_str())
        .collect();
    assert_eq!(unstaged, ["internal/tunnel/supervisor.go"]);
    assert_eq!(
        stage.committing_as.as_deref(),
        Some("Demi <demi@example.com>")
    );
    assert!(!stage.status.head.as_ref().unwrap().pushed);
    let diff = world
        .desk
        .stage_diff(&path, "internal/tunnel/supervisor.go", Side::Unstaged)
        .await
        .unwrap();
    assert_eq!(diff.chunks.len(), 2);
    assert_eq!((diff.additions, diff.deletions), (5, 0));

    // Branches: five to push, two remotes, a branch open in a worktree.
    let branches = world.desk.branches_list(&path).await.unwrap();
    assert_eq!(branches.current.as_deref(), Some("feature/tunnel-ui"));
    assert_eq!(branches.changes, 3);
    let current = &branches.local[0];
    assert_eq!((current.ahead, current.behind), (5, 0));
    assert_eq!(
        current.upstream.as_deref(),
        Some("studio/feature/tunnel-ui")
    );
    let names: Vec<&str> = branches.local.iter().map(|b| b.name.as_str()).collect();
    assert_eq!(
        names,
        [
            "feature/tunnel-ui",
            "feature/backoff-rework",
            "feature/ssh",
            "fix/setup-race",
            "main"
        ]
    );
    let ssh = &branches.local[2];
    assert_eq!((ssh.ahead, ssh.behind), (1, 1));
    assert!(branches.local[3]
        .worktree
        .as_deref()
        .unwrap()
        .ends_with("Tunnel.worktrees/fix-setup-race"));
    let groups: Vec<(&str, &str, usize)> = branches
        .remotes
        .iter()
        .map(|g| (g.remote.as_str(), g.label.as_str(), g.branches.len()))
        .collect();
    assert_eq!(
        groups,
        [("github", "On github", 2), ("studio", "On studio", 2)]
    );
    let remotes = world.desk.branches_remotes(&path).await.unwrap();
    assert_eq!(remotes.len(), 2);
    let tags = world.desk.branches_tags(&path).await.unwrap();
    let tag_names: Vec<(&str, bool)> = tags
        .iter()
        .map(|t| (t.name.as_str(), t.annotated))
        .collect();
    assert!(tag_names.contains(&("v0.9", true)) && tag_names.contains(&("v0.8", false)));
    let aside = world.desk.branches_stashes(&path).await.unwrap();
    let messages: Vec<(&str, Option<&str>)> = aside
        .iter()
        .map(|s| (s.message.as_str(), s.branch.as_deref()))
        .collect();
    assert_eq!(
        messages,
        [
            ("Try a faster setup lock", Some("fix/setup-race")),
            ("Half-done tunnel status badge", Some("feature/tunnel-ui"))
        ]
    );
    assert_eq!(aside[1].files, 2);
    let trees = world.desk.branches_worktrees(&path).await.unwrap();
    assert_eq!(trees.len(), 2);
    assert!(trees[0].tree.main && trees[0].tree.current);
    assert_eq!(trees[1].tree.branch.as_deref(), Some("fix/setup-race"));
    assert_eq!(
        trees[1].display_path,
        "~/Atelier/DemiInfinity/Tunnel.worktrees/fix-setup-race"
    );

    // History: the whole graph, with the current branch's five on top.
    let graph = world
        .desk
        .history_graph(&path, &GraphQuery::default())
        .await
        .unwrap();
    assert_eq!(graph.rows.len(), 13);
    assert_eq!(graph.rows.iter().filter(|r| r.row.unpushed).count(), 8);
    assert!(graph.columns >= 3);
    let head = graph.rows.iter().find(|r| r.row.is_head).unwrap();
    assert_eq!(
        head.row.subject,
        "Add remote access switch to the tray menu"
    );
    assert_eq!(head.pills[0].text, "feature/tunnel-ui");
    let mine = world
        .desk
        .history_graph(
            &path,
            &GraphQuery {
                branch: Some("feature/tunnel-ui".into()),
                limit: 5,
                ..Default::default()
            },
        )
        .await
        .unwrap();
    assert!(mine.more && mine.rows.iter().all(|r| r.row.unpushed));

    // Nothing is under way; the other branch conflicts in two files once
    // the uncommitted changes are out of the way.
    assert_eq!(
        world.desk.flow_state(&path).await.unwrap().kind,
        OperationKind::None
    );
    world
        .desk
        .branches_stash_save(&path, "Out of the way", true, &[])
        .await
        .unwrap();
    let merge = world
        .desk
        .flow_merge(&path, "feature/backoff-rework", MergeMode::Auto)
        .await
        .unwrap();
    assert!(!merge.done);
    assert_eq!(
        merge.conflicts,
        [
            "cmd/atelier-desktop/tray.go",
            "internal/tunnel/supervisor.go"
        ]
    );
    let stopped = world.desk.flow_abort(&path).await.unwrap();
    assert!(stopped.done);
    // The overview knows the worktree belongs to the repository.
    world
        .mock_get("/repos", serde_json::json!({"repos": []}))
        .await;
    world.mock_get("/status", status()).await;
    let overview = world.desk.overview().await.unwrap();
    let all: Vec<_> = overview
        .copies
        .iter()
        .chain(overview.others.iter())
        .collect();
    let tree = all
        .iter()
        .find(|c| c.path.ends_with("fix-setup-race"))
        .unwrap();
    assert_eq!(tree.worktree_of.as_deref(), Some(path.as_str()));
    assert_eq!(
        all.iter().find(|c| c.path == path).unwrap().worktree_of,
        None
    );
}

#[tokio::test]
async fn folders_atelier_does_not_look_after_are_refused_by_every_area() {
    let world = World::signed_in().await;
    let outside = world.base().join("elsewhere/secret");
    std::fs::create_dir_all(&outside).unwrap();
    world.g(&outside, &["init", "--quiet", "-b", "main"]).await;
    world.commit(&outside, "a.txt", "a\n").await;
    write(&outside, "a.txt", "changed\n");
    let tip = world.g(&outside, &["rev-parse", "HEAD"]).await;
    let d = &world.desk;
    for path in [p(&outside).as_str(), "relative", ""] {
        assert_eq!(
            d.stage_status(path).await.unwrap_err().kind,
            "outside",
            "{path:?}"
        );
        assert!(d.stage_diff(path, "a.txt", Side::Unstaged).await.is_err());
        assert!(d
            .stage_add(path, "a.txt", &Selection::File, None)
            .await
            .is_err());
        assert!(d
            .stage_discard(path, "a.txt", &Selection::File, None)
            .await
            .is_err());
        assert!(d.stage_discard_all(path).await.is_err());
        assert!(d.stage_commit(path, "x", "", false).await.is_err());
        assert!(d.history_graph(path, &GraphQuery::default()).await.is_err());
        assert!(d.history_commit(path, &tip).await.is_err());
        assert!(d.history_blame(path, "a.txt", None).await.is_err());
        assert!(d.branches_list(path).await.is_err());
        assert!(d.branches_create(path, "x", None).await.is_err());
        assert!(d
            .branches_switch(path, "main", SwitchHow::Carry)
            .await
            .is_err());
        assert!(d.branches_pull(path, PullMode::Merge).await.is_err());
        assert!(d.branches_stash_save(path, "x", true, &[]).await.is_err());
        assert!(d.branches_worktrees(path).await.is_err());
        assert!(d
            .branches_remote_add(path, "x", "https://x.test/a.git")
            .await
            .is_err());
        assert!(d.flow_state(path).await.is_err());
        assert!(d.flow_reset(path, &tip, ResetMode::Hard).await.is_err());
        assert!(d.flow_undo(path, UndoOptions::default()).await.is_err());
        assert!(d.flow_journal(path).await.is_err());
    }
    assert_eq!(
        std::fs::read_to_string(outside.join("a.txt")).unwrap(),
        "changed\n"
    );
    assert!(!outside.join(".git/atelier").exists());
}

#[tokio::test]
async fn staging_committing_and_undoing_through_the_commands() {
    let world = World::signed_in().await;
    let repo = world.git_fixture("DemiInfinity/Tunnel");
    let path = p(&repo);
    let file = "internal/tunnel/supervisor.go";
    let diff = world
        .desk
        .stage_diff(&path, file, Side::Unstaged)
        .await
        .unwrap();
    // Stage only the first chunk's added line.
    let line = diff.chunks[0]
        .lines
        .iter()
        .find(|l| l.kind == ChunkLineKind::Add)
        .unwrap()
        .index;
    let pick = Selection::Lines {
        lines: vec![LineRef { chunk: 0, line }],
    };
    world
        .desk
        .stage_add(&path, file, &pick, Some(&diff.token))
        .await
        .unwrap();
    let stage = world.desk.stage_status(&path).await.unwrap();
    assert_eq!(stage.status.staged.len(), 3);
    assert_eq!(stage.status.staged[2].additions, Some(1));
    assert_eq!(stage.status.unstaged[0].additions, Some(4));
    // A stale token is refused in plain words.
    let stale = world
        .desk
        .stage_add(
            &path,
            file,
            &Selection::Chunks { chunks: vec![0] },
            Some(&diff.token),
        )
        .await
        .unwrap_err();
    assert_eq!(stale.kind, "invalid");
    assert!(stale.message.contains("changed since you looked"));
    // Take it back out, then commit the two files that were staged.
    let staged = world
        .desk
        .stage_diff(&path, file, Side::Staged)
        .await
        .unwrap();
    world
        .desk
        .stage_remove(&path, file, &Selection::File, Some(&staged.token))
        .await
        .unwrap();
    let before = world.g(&repo, &["rev-parse", "HEAD"]).await;
    let done = world
        .desk
        .stage_commit(&path, "Tray: remote sessions", "", false)
        .await
        .unwrap();
    assert!(done.journal.is_some());
    assert_eq!(
        world.g(&repo, &["status", "--porcelain"]).await,
        "M internal/tunnel/supervisor.go"
    );
    // Discard the rest, and bring it back from Recently discarded.
    let kept = world
        .desk
        .stage_discard(&path, file, &Selection::File, None)
        .await
        .unwrap()
        .unwrap();
    assert_eq!(world.g(&repo, &["status", "--porcelain"]).await, "");
    let listed = world.desk.flow_discarded(&path).await.unwrap();
    assert_eq!(listed[0].id, kept);
    world
        .desk
        .flow_restore_discarded(&path, kept, false)
        .await
        .unwrap();
    assert_eq!(
        world.g(&repo, &["status", "--porcelain"]).await,
        "M internal/tunnel/supervisor.go"
    );
    // Undo the commit: refused over uncommitted work that came since?
    // No: the tree is as the commit left it, so it steps back cleanly.
    let undone = world
        .desk
        .flow_undo(&path, UndoOptions::default())
        .await
        .unwrap();
    assert!(undone.done, "{undone:?}");
    assert_eq!(undone.title, "Undid “Committed “Tray: remote sessions””");
    assert_eq!(world.g(&repo, &["rev-parse", "HEAD"]).await, before);
    let stage = world.desk.stage_status(&path).await.unwrap();
    assert_eq!(
        (stage.status.staged.len(), stage.status.unstaged.len()),
        (2, 1)
    );
    let journal = world.desk.flow_journal(&path).await.unwrap();
    assert_eq!(journal[0].kind, "undo");
}

#[tokio::test]
async fn history_file_history_and_blame_through_the_commands() {
    let world = World::signed_in().await;
    let repo = world.git_fixture("DemiInfinity/Tunnel");
    let path = p(&repo);
    let found = world
        .desk
        .history_graph(
            &path,
            &GraphQuery {
                text: Some("backoff".into()),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    assert!(found.filtered);
    assert_eq!(found.rows.len(), 3);
    let cap = found
        .rows
        .iter()
        .find(|r| r.row.subject == "Supervisor: cap the backoff at 30 seconds")
        .unwrap();
    let info = world.desk.history_commit(&path, &cap.row.id).await.unwrap();
    assert_eq!(info.info.files.len(), 1);
    assert_eq!(info.info.files[0].path, "internal/tunnel/supervisor.go");
    assert!(info.info.on_current && !info.info.pushed);
    let patch = world
        .desk
        .history_commit_diff(&path, &cap.row.id, "internal/tunnel/supervisor.go", None)
        .await
        .unwrap();
    assert_eq!((patch.additions, patch.deletions), (1, 1));
    let history = world
        .desk
        .history_file(&path, "internal/tunnel/supervisor.go", None, 0)
        .await
        .unwrap();
    assert_eq!(history.entries.len(), 3);
    let blame = world
        .desk
        .history_blame(&path, "internal/tunnel/supervisor.go", Some("HEAD"))
        .await
        .unwrap();
    assert!(blame.runs.len() >= 3);
    let capped = blame
        .runs
        .iter()
        .find(|r| r.summary.starts_with("Supervisor: cap"))
        .unwrap();
    let before = world
        .desk
        .history_blame_before(&path, "internal/tunnel/supervisor.go", &capped.commit)
        .await
        .unwrap();
    assert!(before
        .runs
        .iter()
        .all(|r| !r.summary.starts_with("Supervisor: cap")));
    let old = world
        .desk
        .history_file_at(&path, "v0.8", "README.md")
        .await
        .unwrap();
    assert_eq!(old.text, "# Atelier\n\nA studio of your own.\n");
    assert_eq!(
        world
            .desk
            .history_commit(&path, "nope")
            .await
            .unwrap_err()
            .kind,
        "invalid"
    );
}

#[tokio::test]
async fn branches_worktrees_and_pushing_through_the_commands() {
    let world = World::signed_in().await;
    let repo = world.git_fixture("DemiInfinity/Tunnel");
    let path = p(&repo);
    let d = &world.desk;

    // Switching with changes asks first; putting them aside goes through.
    let asked = d
        .branches_switch(&path, "main", SwitchHow::Ask)
        .await
        .unwrap();
    assert!(asked.needs_choice && !asked.switched);
    assert_eq!(asked.changes, 3);
    let switched = d
        .branches_switch(&path, "main", SwitchHow::PutAside)
        .await
        .unwrap();
    assert!(switched.switched);
    assert_eq!(d.branches_stashes(&path).await.unwrap().len(), 3);
    d.branches_switch(&path, "feature/tunnel-ui", SwitchHow::Ask)
        .await
        .unwrap();

    // Push the five commits; then a rewritten branch needs the careful push.
    let pushed = d.branches_push(&path, None, None).await.unwrap();
    assert_eq!((pushed.remote.as_str(), pushed.forced), ("studio", false));
    assert_eq!(d.branches_list(&path).await.unwrap().local[0].ahead, 0);
    world
        .g(
            &repo,
            &[
                "commit",
                "--quiet",
                "--amend",
                "-m",
                "Add the remote access switch",
            ],
        )
        .await;
    assert!(d
        .branches_push(&path, None, None)
        .await
        .unwrap_err()
        .message
        .contains("force push safely"));
    let preview = d.branches_force_preview(&path, None, None).await.unwrap();
    assert_eq!((preview.replaced, preview.new), (1, 1));
    let forced = d
        .branches_force_push(&path, None, None, preview.expected.as_deref().unwrap())
        .await
        .unwrap();
    assert!(forced.forced);
    // A branch that follows nothing, with two remotes and neither the
    // studio's: the window has to say where.
    d.branches_create(&path, "spike", Some("main"))
        .await
        .unwrap();
    let unsure = d
        .branches_push(&path, Some("spike"), None)
        .await
        .unwrap_err();
    assert!(unsure.message.contains("Choose which remote"));
    d.branches_push(&path, Some("spike"), Some("github"))
        .await
        .unwrap();
    assert!(
        !d.branches_delete_check(&path, "spike")
            .await
            .unwrap()
            .current
    );
    d.branches_delete(&path, "spike", false).await.unwrap();

    // Worktrees: the suggested folder is beside the repository.
    let suggested = d
        .branches_worktree_suggest(&path, "feature/ssh")
        .await
        .unwrap();
    assert_eq!(
        suggested.display_path,
        "~/Atelier/DemiInfinity/Tunnel.worktrees/feature-ssh"
    );
    let tree = d
        .branches_worktree_add(
            &path,
            None,
            &WorktreeSource::Branch {
                name: "feature/ssh".into(),
            },
        )
        .await
        .unwrap();
    assert_eq!(tree.tree.path, suggested.path);
    // A folder Atelier doesn't look after is refused, before git is asked.
    let outside = world.base().join("elsewhere/tree");
    let refused = d
        .branches_worktree_add(
            &path,
            Some(&p(&outside)),
            &WorktreeSource::NewBranch {
                name: "x".into(),
                from: None,
            },
        )
        .await
        .unwrap_err();
    assert_eq!(refused.kind, "outside");
    assert!(!outside.exists());
    let sneaky = format!("{}/../../../../elsewhere/tree", suggested.path);
    assert_eq!(
        d.branches_worktree_add(
            &path,
            Some(&sneaky),
            &WorktreeSource::Detached { rev: "main".into() }
        )
        .await
        .unwrap_err()
        .kind,
        "outside"
    );
    // Inside a watch folder is fine.
    let watched = world.home().join("Projects/trees/main-copy");
    let made = d
        .branches_worktree_add(
            &path,
            Some(&p(&watched)),
            &WorktreeSource::Detached { rev: "main".into() },
        )
        .await
        .unwrap();
    assert_eq!(made.tree.branch, None);

    // A linked worktree opens like any repository, wherever it is.
    let view = d.repo_view(&tree.tree.path).await.unwrap();
    assert_eq!(view.branch.as_deref(), Some("feature/ssh"));
    let inside = d.branches_worktrees(&tree.tree.path).await.unwrap();
    assert!(inside
        .iter()
        .any(|t| t.tree.current && t.tree.branch.as_deref() == Some("feature/ssh")));
    d.branches_worktree_lock(&path, &tree.tree.path, "Locked while the agent works")
        .await
        .unwrap();
    let locked = d
        .branches_worktree_remove(&path, &tree.tree.path, false)
        .await
        .unwrap_err();
    assert!(locked.message.contains("Locked while the agent works"));
    d.branches_worktree_unlock(&path, &tree.tree.path)
        .await
        .unwrap();
    d.branches_worktree_remove(&path, &tree.tree.path, false)
        .await
        .unwrap();
    assert_eq!(
        d.repo_view(&tree.tree.path).await.unwrap_err().kind,
        "missing"
    );
    assert!(d.branches_worktree_prune(&path).await.unwrap().is_empty());

    // Tags and remotes.
    d.branches_tag_create(&path, "v1.0", Some("main"), Some("One"))
        .await
        .unwrap();
    d.branches_tag_push(&path, "v1.0", "studio").await.unwrap();
    d.branches_tag_delete(&path, "v1.0").await.unwrap();
    d.branches_remote_add(&path, "mirror", "https://example.test/mirror.git")
        .await
        .unwrap();
    d.branches_remote_rename(&path, "mirror", "backup")
        .await
        .unwrap();
    d.branches_remote_set_url(&path, "backup", "https://example.test/backup.git")
        .await
        .unwrap();
    d.branches_remote_remove(&path, "backup").await.unwrap();
    d.branches_fetch(&path, None, true).await.unwrap();
    d.branches_set_upstream(&path, "feature/backoff-rework", Some("studio/main"))
        .await
        .unwrap();
    d.branches_rename(&path, "feature/backoff-rework", "feature/backoff")
        .await
        .unwrap();
    let pulled = d.branches_pull(&path, PullMode::FfOnly).await.unwrap();
    assert_eq!(pulled.summary, "Already up to date.");
}

#[tokio::test]
async fn tidying_resolving_and_going_back_through_the_commands() {
    let world = World::signed_in().await;
    let repo = world.git_fixture("DemiInfinity/Tunnel");
    let path = p(&repo);
    let d = &world.desk;
    let start = world.g(&repo, &["rev-parse", "HEAD"]).await;
    let put_aside = d
        .branches_stash_save(&path, "For later", true, &[])
        .await
        .unwrap();

    // Tidy up: drop the debug commit, fold the two supervisor commits.
    let plan = d.flow_rebase_plan(&path, None).await.unwrap();
    assert_eq!(plan.commits.len(), 5);
    assert_eq!(plan.base_name.as_deref(), Some("main"));
    let action = |i: usize| match i {
        0 => PlanAction::Drop,
        2 => PlanAction::Squash,
        _ => PlanAction::Keep,
    };
    let steps: Vec<PlanStep> = plan
        .commits
        .iter()
        .enumerate()
        .map(|(i, c)| PlanStep {
            id: c.id.clone(),
            action: action(i),
            message: None,
        })
        .collect();
    let tidied = d.flow_tidy(&path, &plan.base, &steps).await.unwrap();
    assert!(tidied.done);
    assert_eq!(tidied.summary, "Tidied up 5 commits into 3");

    // Rebase onto the branch that conflicts: one stop per commit that collides.
    let rebase = d
        .flow_rebase(&path, "feature/backoff-rework")
        .await
        .unwrap();
    assert!(!rebase.done);
    assert_eq!(rebase.state.kind, OperationKind::Rebase);
    let list = d.flow_conflicts(&path).await.unwrap();
    assert_eq!(list.total, 1);
    let file = list.needs_you[0].path.clone();
    let detail = d.flow_conflict_file(&path, &file).await.unwrap();
    assert_eq!(detail.theirs_label, "feature/backoff-rework");
    assert!(detail.total >= 1);
    let mut detail = detail;
    for region in 0..detail.total {
        detail = d
            .flow_conflict_choose(&path, &file, region, Some(Choice::Yours))
            .await
            .unwrap();
    }
    assert!(detail.resolved);
    let mut out = d.flow_continue(&path).await.unwrap();
    let mut guard = 0;
    while !out.done {
        for f in d.flow_conflicts(&path).await.unwrap().needs_you {
            if !f.resolved {
                d.flow_conflict_take(&path, &f.path, TakeSide::Yours)
                    .await
                    .unwrap();
            }
        }
        out = d.flow_continue(&path).await.unwrap();
        guard += 1;
        assert!(guard < 5);
    }
    assert_eq!(
        out.summary,
        "Rebased feature/tunnel-ui onto feature/backoff-rework"
    );
    assert_eq!(d.flow_state(&path).await.unwrap().kind, OperationKind::None);

    // A reset, previewed first.
    let preview = d
        .flow_reset_preview(&path, "feature/backoff-rework")
        .await
        .unwrap();
    assert_eq!(preview.commits, 3);
    let reset = d
        .flow_reset(&path, "feature/backoff-rework", ResetMode::Mixed)
        .await
        .unwrap();
    assert_eq!(reset.summary, "Reset 3 commits (kept changes)");

    // Recent actions, newest first; back to before the tidy-up in one go.
    let journal = d.flow_journal(&path).await.unwrap();
    let kinds: Vec<&str> = journal.iter().map(|e| e.kind.as_str()).collect();
    assert_eq!(kinds, ["reset", "rebase", "tidy", "stash_save"]);
    let tidy = journal[2].id;
    let back = d
        .flow_go_back(&path, tidy, UndoOptions::default())
        .await
        .unwrap();
    assert!(back.done, "{back:?}");
    assert_eq!(world.g(&repo, &["rev-parse", "HEAD"]).await, start);
    assert_eq!(world.g(&repo, &["status", "--porcelain"]).await, "");
    // And one more step: the put-aside changes are back in the files.
    let undone = d.flow_undo(&path, UndoOptions::default()).await.unwrap();
    assert_eq!(undone.title, "Undid “Put aside “For later””");
    assert_eq!(d.stage_status(&path).await.unwrap().status.staged.len(), 2);
    assert!(!d
        .branches_stashes(&path)
        .await
        .unwrap()
        .iter()
        .any(|s| s.id == put_aside.id));

    // Cherry-pick and revert answer the same way.
    d.stage_discard_all(&path).await.unwrap();
    let fix = world.g(&repo, &["rev-parse", "fix/setup-race"]).await;
    let picked = d.flow_cherry_pick(&path, &fix).await.unwrap();
    assert!(picked.done);
    let reverted = d.flow_revert(&path, "HEAD").await.unwrap();
    assert_eq!(
        reverted.summary,
        "Reverted “Setup: wait for the database before first run”"
    );
    assert!(d.flow_skip(&path).await.is_err());
    assert_eq!(
        d.flow_conflict_mark(&path, "README.md")
            .await
            .unwrap_err()
            .kind,
        "invalid"
    );
    assert_eq!(
        d.flow_conflict_reopen(&path, "README.md")
            .await
            .unwrap_err()
            .kind,
        "invalid"
    );
}
