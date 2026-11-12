use atelier_core::git::conflict::{Choice, TakeSide};
use atelier_core::git::integrate::{MergeMode, OperationKind, PlanAction, PlanStep, ResetMode};

use crate::kit::*;

fn step(id: &str, action: PlanAction, message: Option<&str>) -> PlanStep {
    PlanStep {
        id: id.to_string(),
        action,
        message: message.map(str::to_string),
    }
}

#[tokio::test]
async fn merges_fast_forward_or_make_a_commit() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    s.commit(&repo, "a.txt", "a\n", "A").await;
    s.g(&repo, &["switch", "--quiet", "-c", "side"]).await;
    let tip = s.commit(&repo, "b.txt", "b\n", "B on side").await;
    s.g(&repo, &["switch", "--quiet", "main"]).await;
    assert_eq!(
        s.git.operation_state(&repo).await.unwrap().kind,
        OperationKind::None
    );

    // A fast-forward.
    let out = s.git.merge(&repo, "side", MergeMode::FfOnly).await.unwrap();
    assert!(out.done && out.conflicts.is_empty());
    assert_eq!(out.summary, "Merged side into main");
    assert_eq!(out.head.as_deref(), Some(tip.as_str()));
    assert!(out.journal.is_some());
    // Again: nothing to do, and nothing written down.
    let again = s.git.merge(&repo, "side", MergeMode::Auto).await.unwrap();
    assert_eq!(again.summary, "Already up to date.");
    assert_eq!(again.journal, None);
    // Undo puts main back.
    assert!(
        s.git
            .undo_last(&repo, Default::default())
            .await
            .unwrap()
            .done
    );
    assert_eq!(s.subjects(&repo, "main").await, ["A"]);

    // Always a merge commit.
    let out = s.git.merge(&repo, "side", MergeMode::NoFf).await.unwrap();
    assert!(out.done);
    assert_eq!(
        s.g(&repo, &["rev-list", "--parents", "-n", "1", "HEAD"])
            .await
            .split(' ')
            .count(),
        3
    );
    s.git.undo_last(&repo, Default::default()).await.unwrap();

    // Both moved on: a fast-forward only is refused, and nothing is left behind.
    s.commit(&repo, "c.txt", "c\n", "C on main").await;
    assert!(invalid(s.git.merge(&repo, "side", MergeMode::FfOnly).await).contains("both moved on"));
    assert_eq!(
        s.git.operation_state(&repo).await.unwrap().kind,
        OperationKind::None
    );
    assert!(invalid(s.git.merge(&repo, "nope", MergeMode::Auto).await).contains("no commit"));
    // Uncommitted changes in the way.
    write(&repo, "b.txt", "mine, uncommitted\n");
    assert!(invalid(s.git.merge(&repo, "side", MergeMode::Auto).await)
        .contains("would overwrite your uncommitted changes"));
    assert_eq!(read(&repo, "b.txt"), "mine, uncommitted\n");
    let journal = s.git.journal_list(&repo).await.unwrap();
    assert!(journal.iter().all(|e| e.finished));
}

#[tokio::test]
async fn a_merge_that_conflicts_can_be_stopped_or_finished() {
    let s = Sandbox::new();
    let repo = s.conflicting("r").await;
    let before = s.head(&repo).await;
    write(&repo, "untracked.txt", "left alone\n");

    let out = s.git.merge(&repo, "main", MergeMode::Auto).await.unwrap();
    assert!(!out.done);
    assert_eq!(out.conflicts, ["a.txt", "b.txt"]);
    assert_eq!(out.journal, None);
    let state = out.state;
    assert_eq!(state.kind, OperationKind::Merge);
    assert_eq!(state.cause.as_deref(), Some("merge"));
    assert_eq!(state.title, "Merging main into feature");
    assert_eq!(
        (state.yours.as_deref(), state.theirs.as_deref()),
        (Some("feature"), Some("main"))
    );
    assert_eq!(state.conflicts, 2);
    assert!(!state.can_continue && !state.can_skip);
    // Not in Recent actions while it is under way; other things wait.
    assert!(s.git.journal_list(&repo).await.unwrap().is_empty());
    assert!(invalid(s.git.operation_continue(&repo).await).contains("2 files have conflicts"));
    assert!(invalid(s.git.operation_skip(&repo).await).contains("no step to skip"));
    assert!(invalid(s.git.reset(&repo, &before, ResetMode::Hard).await).contains("still under way"));
    let refused = s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert!(!refused.done);

    // Stop and go back: exactly as it was.
    let stopped = s.git.operation_abort(&repo).await.unwrap();
    assert!(stopped.done);
    assert_eq!(stopped.state.kind, OperationKind::None);
    assert_eq!(s.head(&repo).await, before);
    assert_eq!(s.status(&repo).await, "?? untracked.txt\n");
    assert!(s.git.journal_list(&repo).await.unwrap().is_empty());
    assert!(invalid(s.git.operation_abort(&repo).await).contains("Nothing is under way"));

    // Again, and this time finish it.
    s.git.merge(&repo, "main", MergeMode::Auto).await.unwrap();
    s.git
        .conflict_take(&repo, "a.txt", TakeSide::Theirs)
        .await
        .unwrap();
    let detail = s
        .git
        .conflict_choose(&repo, "b.txt", 0, Some(Choice::Yours))
        .await
        .unwrap();
    assert!(detail.resolved);
    let state = s.git.operation_state(&repo).await.unwrap();
    assert!(state.can_continue && state.conflicts == 0);
    let done = s.git.operation_continue(&repo).await.unwrap();
    assert!(done.done);
    assert_eq!(done.summary, "Merged main into feature");
    assert!(done.journal.is_some());
    assert_eq!(read(&repo, "a.txt"), "one\ntwo theirs\nthree\nfour\nfive\n");
    assert_eq!(read(&repo, "b.txt"), "alpha\nbeta yours\ngamma\n");
    assert_eq!(read(&repo, "clean.txt"), "tidy\n");
    assert_eq!(s.status(&repo).await, "?? untracked.txt\n");
    // One entry for the whole merge; undoing it is one step.
    let journal = s.git.journal_list(&repo).await.unwrap();
    assert_eq!(journal.len(), 1);
    assert_eq!(journal[0].title, "Merged main into feature");
    assert!(
        s.git
            .undo_last(&repo, Default::default())
            .await
            .unwrap()
            .done
    );
    assert_eq!(s.head(&repo).await, before);
    assert!(!repo.join("clean.txt").exists());
    assert_eq!(read(&repo, "untracked.txt"), "left alone\n");
}

#[tokio::test]
async fn a_rebase_stops_at_each_conflict_and_can_skip() {
    let s = Sandbox::new();
    let repo = s.conflicting("r").await;
    let before = s.head(&repo).await;

    let out = s.git.rebase_onto(&repo, "main").await.unwrap();
    assert!(!out.done);
    assert_eq!(out.conflicts, ["a.txt"]);
    let state = out.state;
    assert_eq!(state.kind, OperationKind::Rebase);
    assert_eq!((state.step, state.total), (Some(1), Some(3)));
    assert_eq!(
        state.title,
        "Rebasing feature onto main — stopped at 1 of 3"
    );
    assert_eq!(state.yours.as_deref(), Some("feature"));
    assert!(state.can_skip && !state.can_continue);
    let plan: Vec<(&str, bool, bool)> = state
        .plan
        .iter()
        .map(|p| (p.subject.as_str(), p.done, p.current))
        .collect();
    assert_eq!(
        plan,
        [
            ("Yours: a", true, true),
            ("Yours: b", false, false),
            ("Yours: own", false, false)
        ]
    );
    // In a rebase "yours" is the commit being replayed.
    let detail = s.git.conflict_file(&repo, "a.txt").await.unwrap();
    assert_eq!(
        (detail.yours_label.as_str(), detail.theirs_label.as_str()),
        ("feature", "main")
    );
    let detail = s
        .git
        .conflict_choose(&repo, "a.txt", 0, Some(Choice::Yours))
        .await
        .unwrap();
    assert!(detail.resolved);
    assert_eq!(read(&repo, "a.txt"), "one\ntwo yours\nthree\nfour\nfive\n");

    // On to the next stop.
    let next = s.git.operation_continue(&repo).await.unwrap();
    assert!(!next.done);
    assert_eq!(next.conflicts, ["b.txt"]);
    assert_eq!(next.state.step, Some(2));
    // Leave that commit out altogether.
    let done = s.git.operation_skip(&repo).await.unwrap();
    assert!(done.done, "{done:?}");
    assert_eq!(done.summary, "Rebased feature onto main");
    assert_eq!(
        s.subjects(&repo, "main..feature").await,
        ["Yours: own", "Yours: a"]
    );
    assert_eq!(read(&repo, "b.txt"), "alpha\nbeta theirs\ngamma\n");
    assert_eq!(
        s.git.operation_state(&repo).await.unwrap().kind,
        OperationKind::None
    );

    // Undo: the branch is exactly where it was.
    let undone = s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert_eq!(undone.title, "Undid “Rebased feature onto main”");
    assert_eq!(s.head(&repo).await, before);
    assert_eq!(s.status(&repo).await, "");

    // Stopping halfway also returns to the start.
    s.git.rebase_onto(&repo, "main").await.unwrap();
    s.git.operation_abort(&repo).await.unwrap();
    assert_eq!(s.head(&repo).await, before);
    assert_eq!(
        s.g(&repo, &["symbolic-ref", "--short", "HEAD"]).await,
        "feature"
    );
    // Uncommitted changes: refused before anything starts.
    write(&repo, "own.txt", "dirty\n");
    assert!(invalid(s.git.rebase_onto(&repo, "main").await).contains("uncommitted changes"));
    assert_eq!(s.head(&repo).await, before);
}

/// Five commits on `feature` over `main`, as on the board.
async fn five(s: &Sandbox) -> (std::path::PathBuf, Vec<String>) {
    let repo = s.repo("r").await;
    s.commit(&repo, "README.md", "hello\n", "Start").await;
    s.g(&repo, &["switch", "--quiet", "-c", "feature"]).await;
    let mut ids = Vec::new();
    for (file, text, message) in [
        ("debug.txt", "noise\n", "debug: log every packet"),
        ("sup.txt", "reconnect\n", "Supervisor: reconnect"),
        ("sup.txt", "reconnect\ncap\n", "Supervisor: cap the backoff"),
        ("tray.txt", "who\n", "Tray: show who's connected"),
        ("menu.txt", "switch\n", "add remote access switch"),
    ] {
        ids.push(s.commit(&repo, file, text, message).await);
    }
    (repo, ids)
}

#[tokio::test]
async fn tidying_up_follows_the_plan_without_an_editor() {
    let s = Sandbox::new();
    let (repo, ids) = five(&s).await;
    let plan = s.git.rebase_plan(&repo, None).await.unwrap();
    assert_eq!(plan.branch.as_deref(), Some("feature"));
    assert_eq!(plan.base_name.as_deref(), Some("main"));
    assert_eq!(plan.commits.len(), 5);
    assert_eq!(plan.commits[0].subject, "debug: log every packet");
    assert!(plan.commits.iter().all(|c| !c.pushed));
    assert!(!plan.has_merges);

    // Drop the debug commit, fold the two supervisor commits, move the menu
    // commit before the tray one and give it a proper message.
    let steps = [
        step(&ids[0], PlanAction::Drop, None),
        step(&ids[1], PlanAction::Keep, None),
        step(&ids[2][..10], PlanAction::Squash, None),
        step(
            &ids[4],
            PlanAction::Reword,
            Some("Tray: add the remote access switch\n\nUnder Open studio."),
        ),
        step(&ids[3], PlanAction::Keep, None),
    ];
    let out = s
        .git
        .rebase_interactive(&repo, "main", &steps)
        .await
        .unwrap();
    assert!(out.done, "{out:?}");
    assert_eq!(out.summary, "Tidied up 5 commits into 3");
    assert_eq!(
        s.subjects(&repo, "main..feature").await,
        [
            "Tray: show who's connected",
            "Tray: add the remote access switch",
            "Supervisor: reconnect"
        ]
    );
    // Both messages of the squashed pair are kept; the dropped work is gone.
    let folded = s.g(&repo, &["log", "-1", "--format=%B", "HEAD~2"]).await;
    assert!(
        folded.contains("Supervisor: reconnect") && folded.contains("Supervisor: cap the backoff")
    );
    assert_eq!(
        s.g(&repo, &["log", "-1", "--format=%b", "HEAD~1"]).await,
        "Under Open studio."
    );
    assert!(!repo.join("debug.txt").exists());
    assert_eq!(read(&repo, "sup.txt"), "reconnect\ncap\n");
    assert_eq!(s.status(&repo).await, "");
    assert_eq!(
        s.git.operation_state(&repo).await.unwrap().kind,
        OperationKind::None
    );
    // The scratch files are gone from the git folder, and never were in the tree.
    assert!(!repo.join(".git/atelier/rebase").exists());

    // Undo: five commits again, the very same ones.
    let undone = s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert_eq!(undone.title, "Undid “Tidied up 5 commits into 3”");
    assert_eq!(s.head(&repo).await, ids[4]);

    // Fixup keeps only the first message; a squash can carry a new one.
    let steps = [
        step(&ids[0], PlanAction::Keep, None),
        step(&ids[1], PlanAction::Fixup, None),
        step(&ids[2], PlanAction::Keep, None),
        step(
            &ids[3],
            PlanAction::Squash,
            Some("Supervisor and tray together"),
        ),
        step(&ids[4], PlanAction::Keep, None),
    ];
    let out = s
        .git
        .rebase_interactive(&repo, "main", &steps)
        .await
        .unwrap();
    assert_eq!(out.summary, "Tidied up 5 commits into 3");
    assert_eq!(
        s.subjects(&repo, "main..feature").await,
        [
            "add remote access switch",
            "Supervisor and tray together",
            "debug: log every packet"
        ]
    );
    assert_eq!(
        s.g(&repo, &["log", "-1", "--format=%b", "HEAD~2"]).await,
        ""
    );
    s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert_eq!(s.head(&repo).await, ids[4]);
}

#[tokio::test]
async fn plans_that_make_no_sense_are_refused_before_anything_moves() {
    let s = Sandbox::new();
    let (repo, ids) = five(&s).await;
    let keep_all: Vec<PlanStep> = ids
        .iter()
        .map(|id| step(id, PlanAction::Keep, None))
        .collect();
    let run = |steps: Vec<PlanStep>| {
        let (git, repo) = (s.git.clone(), repo.clone());
        async move { git.rebase_interactive(&repo, "main", &steps).await }
    };
    // Nothing to do.
    let out = run(keep_all.clone()).await.unwrap();
    assert!(out.done && out.journal.is_none());
    assert_eq!(out.summary, "Nothing to tidy up.");
    // A commit left out, one twice, one from elsewhere.
    assert!(invalid(run(keep_all[1..].to_vec()).await).contains("has changed since"));
    let mut twice = keep_all.clone();
    twice[1] = keep_all[0].clone();
    assert!(invalid(run(twice).await).contains("twice"));
    let main = s.g(&repo, &["rev-parse", "main"]).await;
    let mut foreign = keep_all.clone();
    foreign[0] = step(&main, PlanAction::Keep, None);
    assert!(invalid(run(foreign).await).contains("isn’t one of the commits"));
    // Nothing above to fold into; a reword without words.
    let mut first = keep_all.clone();
    first[0].action = PlanAction::Squash;
    assert!(invalid(run(first).await).contains("nothing above it"));
    let mut first = keep_all.clone();
    first[0].action = PlanAction::Drop;
    first[1].action = PlanAction::Fixup;
    assert!(invalid(run(first).await).contains("nothing above it"));
    let mut silent = keep_all.clone();
    silent[2].action = PlanAction::Reword;
    assert!(invalid(run(silent).await).contains("new message"));
    // Uncommitted changes.
    write(&repo, "tray.txt", "dirty\n");
    let mut drop = keep_all.clone();
    drop[0].action = PlanAction::Drop;
    assert!(invalid(run(drop.clone()).await).contains("put them aside"));
    s.g(&repo, &["checkout", "--", "tray.txt"]).await;
    // A merge among the commits.
    s.g(&repo, &["switch", "--quiet", "-c", "side", "main"])
        .await;
    s.commit(&repo, "side.txt", "s\n", "Side").await;
    s.g(&repo, &["switch", "--quiet", "feature"]).await;
    s.g(
        &repo,
        &["merge", "--quiet", "--no-ff", "-m", "Merge side", "side"],
    )
    .await;
    assert!(
        s.git
            .rebase_plan(&repo, Some("main"))
            .await
            .unwrap()
            .has_merges
    );
    assert!(invalid(run(drop).await).contains("merge among"));
    // Through all of that the branch never moved and nothing was written down.
    assert_eq!(s.subjects(&repo, "main..feature").await.len(), 7);
    assert!(s.git.journal_list(&repo).await.unwrap().is_empty());
}

#[tokio::test]
async fn a_tidy_up_that_stops_halfway_carries_on_or_goes_back() {
    let s = Sandbox::new();
    let (repo, ids) = five(&s).await;
    // Swapping the two supervisor commits makes the second one conflict.
    let steps = [
        step(&ids[0], PlanAction::Keep, None),
        step(&ids[2], PlanAction::Keep, None),
        step(&ids[1], PlanAction::Keep, None),
        step(&ids[3], PlanAction::Keep, None),
        step(&ids[4], PlanAction::Reword, Some("Menu: remote access")),
    ];
    let out = s
        .git
        .rebase_interactive(&repo, "main", &steps)
        .await
        .unwrap();
    assert!(!out.done);
    assert_eq!(out.conflicts, ["sup.txt"]);
    let state = out.state;
    assert_eq!(state.kind, OperationKind::Rebase);
    assert_eq!(state.cause.as_deref(), Some("tidy"));
    assert_eq!(state.step, Some(2));
    assert!(state
        .title
        .starts_with("Tidying up commits — stopped at 2 of"));
    let plan: Vec<(&str, &str, bool, bool)> = state
        .plan
        .iter()
        .map(|p| (p.short.as_str(), p.action.as_str(), p.done, p.current))
        .collect();
    assert_eq!(plan.len(), 5);
    assert_eq!(plan[1], (&ids[2][..7], "keep", true, true));
    assert_eq!(plan[4], (&ids[4][..7], "reword", false, false));

    // Stop and go back.
    s.git.operation_abort(&repo).await.unwrap();
    assert_eq!(s.head(&repo).await, ids[4]);
    assert_eq!(s.status(&repo).await, "");
    assert!(s.git.journal_list(&repo).await.unwrap().is_empty());
    assert!(!repo.join(".git/atelier/rebase").exists());

    // Again; resolve each stop and carry on to the end.
    let out = s
        .git
        .rebase_interactive(&repo, "main", &steps)
        .await
        .unwrap();
    assert!(!out.done);
    let mut stops = 0;
    loop {
        let list = s.git.conflict_list(&repo).await.unwrap();
        for file in list.needs_you.iter().filter(|f| !f.resolved) {
            s.git
                .conflict_take(&repo, &file.path, TakeSide::Yours)
                .await
                .unwrap();
        }
        stops += 1;
        let out = s.git.operation_continue(&repo).await.unwrap();
        if out.done {
            assert_eq!(out.summary, "Tidied up 5 commits into 5");
            break;
        }
        assert!(stops < 5, "{out:?}");
    }
    assert_eq!(
        s.subjects(&repo, "main..feature").await,
        [
            "Menu: remote access",
            "Tray: show who's connected",
            "Supervisor: reconnect",
            "Supervisor: cap the backoff",
            "debug: log every packet"
        ]
    );
    assert_eq!(s.git.journal_list(&repo).await.unwrap().len(), 1);
    s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert_eq!(s.head(&repo).await, ids[4]);
}

#[tokio::test]
async fn cherry_pick_and_revert() {
    let s = Sandbox::new();
    let repo = s.conflicting("r").await;
    let clean = s.g(&repo, &["rev-parse", "main"]).await;
    let theirs_a = s.g(&repo, &["rev-parse", "main~2"]).await;

    let out = s.git.cherry_pick(&repo, &clean).await.unwrap();
    assert!(out.done);
    assert_eq!(
        out.summary,
        format!("Cherry-picked {} onto feature", &clean[..7])
    );
    assert_eq!(read(&repo, "clean.txt"), "tidy\n");
    assert_eq!(s.subjects(&repo, "-1").await, ["Theirs: clean"]);
    // Its changes are here now: picking it again has nothing to do.
    assert!(invalid(s.git.cherry_pick(&repo, &clean).await).contains("already here"));
    assert_eq!(
        s.git.operation_state(&repo).await.unwrap().kind,
        OperationKind::None
    );

    // One that conflicts: stop, then again and resolve.
    let out = s.git.cherry_pick(&repo, &theirs_a).await.unwrap();
    assert!(!out.done);
    assert_eq!(out.state.kind, OperationKind::CherryPick);
    assert!(out.state.title.starts_with("Cherry-picking"));
    s.git.operation_abort(&repo).await.unwrap();
    assert_eq!(read(&repo, "a.txt"), "one\ntwo yours\nthree\nfour\nfive\n");
    s.git.cherry_pick(&repo, &theirs_a).await.unwrap();
    s.git
        .conflict_choose(&repo, "a.txt", 0, Some(Choice::Both { yours_first: true }))
        .await
        .unwrap();
    let done = s.git.operation_continue(&repo).await.unwrap();
    assert!(done.done);
    assert_eq!(
        read(&repo, "a.txt"),
        "one\ntwo yours\ntwo theirs\nthree\nfour\nfive\n"
    );

    // Revert adds a commit that undoes one.
    let own = s.g(&repo, &["rev-parse", "HEAD~2"]).await;
    let out = s.git.revert(&repo, &own).await.unwrap();
    assert!(out.done);
    assert_eq!(out.summary, "Reverted “Yours: own”");
    assert!(!repo.join("own.txt").exists());
    assert_eq!(s.subjects(&repo, "-1").await, ["Revert \"Yours: own\""]);
    // A merge can't be picked or reverted here.
    s.g(
        &repo,
        &["merge", "--quiet", "-s", "ours", "-m", "Merge main", "main"],
    )
    .await;
    let merge = s.head(&repo).await;
    assert!(invalid(s.git.revert(&repo, &merge).await).contains("is a merge"));
    assert!(invalid(s.git.cherry_pick(&repo, &merge).await).contains("is a merge"));

    let titles: Vec<String> = s
        .git
        .journal_list(&repo)
        .await
        .unwrap()
        .into_iter()
        .map(|e| e.kind)
        .collect();
    assert_eq!(titles, ["revert", "cherry_pick", "cherry_pick"]);
}

#[tokio::test]
async fn reset_keeps_or_throws_away_and_says_so() {
    let s = Sandbox::new();
    let (repo, ids) = five(&s).await;
    let main = s.g(&repo, &["rev-parse", "main"]).await;
    write(&repo, "tray.txt", "who\nuncommitted\n");
    write(&repo, "scratch.txt", "untracked\n");

    let preview = s.git.reset_preview(&repo, &ids[2]).await.unwrap();
    assert_eq!(
        (preview.commits, preview.pushed, preview.uncommitted),
        (2, 0, 2)
    );
    assert_eq!(preview.branch.as_deref(), Some("feature"));

    // Mixed: the commits' work stays in the files, unstaged.
    let out = s.git.reset(&repo, &ids[2], ResetMode::Mixed).await.unwrap();
    assert_eq!(out.summary, "Reset 2 commits (kept changes)");
    assert_eq!(s.head(&repo).await, ids[2]);
    assert_eq!(
        s.status(&repo).await,
        "?? menu.txt\n?? scratch.txt\n?? tray.txt\n"
    );
    assert_eq!(read(&repo, "tray.txt"), "who\nuncommitted\n");
    // Undo: the commits are back and so is the uncommitted edit, as it was.
    assert!(
        s.git
            .undo_last(&repo, Default::default())
            .await
            .unwrap()
            .done
    );
    assert_eq!(s.head(&repo).await, ids[4]);
    assert_eq!(s.status(&repo).await, " M tray.txt\n?? scratch.txt\n");
    assert_eq!(read(&repo, "tray.txt"), "who\nuncommitted\n");

    // Soft: staged.
    let out = s.git.reset(&repo, &ids[3], ResetMode::Soft).await.unwrap();
    assert_eq!(out.summary, "Reset 1 commit (kept changes staged)");
    assert_eq!(
        s.status(&repo).await,
        "A  menu.txt\n M tray.txt\n?? scratch.txt\n"
    );
    s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert_eq!(s.status(&repo).await, " M tray.txt\n?? scratch.txt\n");

    // Hard: everything goes, and the uncommitted part is kept on its own.
    let out = s.git.reset(&repo, &main, ResetMode::Hard).await.unwrap();
    assert_eq!(out.summary, "Reset 5 commits (threw the changes away)");
    assert_eq!(s.head(&repo).await, main);
    assert_eq!(s.status(&repo).await, "?? scratch.txt\n");
    let kept = s.git.discarded_list(&repo).await.unwrap();
    assert_eq!(kept.len(), 1);
    assert_eq!(kept[0].title, "Discarded uncommitted changes (reset)");
    assert_eq!(
        kept[0].discarded.as_ref().unwrap().paths,
        ["scratch.txt", "tray.txt"]
    );
    // Undo steps over that entry: undoing the reset brings everything back.
    let undone = s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert_eq!(
        undone.title,
        "Undid “Reset 5 commits (threw the changes away)”"
    );
    assert_eq!(s.head(&repo).await, ids[4]);
    assert_eq!(s.status(&repo).await, " M tray.txt\n?? scratch.txt\n");
    assert_eq!(read(&repo, "tray.txt"), "who\nuncommitted\n");

    // Sideways (not an ancestor) is a move, not a count of commits.
    s.g(&repo, &["stash", "--quiet", "-u"]).await;
    let out = s.git.reset(&repo, "main", ResetMode::Hard).await.unwrap();
    assert!(out.summary.starts_with("Reset 5 commits"));
    s.g(&repo, &["switch", "--quiet", "main"]).await;
    let out = s.git.reset(&repo, &ids[0], ResetMode::Soft).await.unwrap();
    assert_eq!(
        out.summary,
        format!("Moved main to {} (kept changes staged)", &ids[0][..7])
    );
    assert!(invalid(s.git.reset(&repo, "--hard", ResetMode::Soft).await).contains("isn’t a commit"));
}
