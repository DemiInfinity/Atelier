use atelier_core::diff::FileStatus;
use atelier_core::git::conflict::{Choice, TakeSide};
use atelier_core::git::integrate::OperationKind;

use crate::kit::*;

#[tokio::test]
async fn changes_are_put_aside_listed_shown_and_brought_back() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    s.commit(&repo, "a.txt", "a\n", "A").await;
    s.commit(&repo, "b.txt", "b\n", "B").await;
    assert!(invalid(s.git.stash_save(&repo, "nothing", true, &[]).await)
        .contains("nothing to put aside"));
    assert!(s.git.stash_list(&repo).await.unwrap().is_empty());
    assert!(s.git.journal_list(&repo).await.unwrap().is_empty());

    write(&repo, "a.txt", "a\nhalf done\n");
    write(&repo, "b.txt", "b\nalso\n");
    write(&repo, "new.txt", "untracked\n");
    // Only one file.
    let one = s
        .git
        .stash_save(&repo, "Just b", false, &["b.txt".into()])
        .await
        .unwrap();
    assert_eq!(
        (one.message.as_str(), one.files, one.position),
        ("Just b", 1, 0)
    );
    assert_eq!(s.status(&repo).await, " M a.txt\n?? new.txt\n");
    // Everything, untracked files too; a message is made up when none is given.
    let all = s.git.stash_save(&repo, "  ", true, &[]).await.unwrap();
    assert_eq!(all.message, "Changes on main");
    assert_eq!(all.files, 2);
    assert_eq!(all.branch.as_deref(), Some("main"));
    assert_eq!(s.status(&repo).await, "");

    let list = s.git.stash_list(&repo).await.unwrap();
    let flat: Vec<(&str, usize)> = list
        .iter()
        .map(|e| (e.message.as_str(), e.position))
        .collect();
    assert_eq!(flat, [("Changes on main", 0), ("Just b", 1)]);
    assert!(list[0].time.starts_with("20"));
    let detail = s.git.stash_show(&repo, &all.id).await.unwrap();
    let files: Vec<(&str, FileStatus, u32)> = detail
        .files
        .iter()
        .map(|f| (f.path(), f.status, f.additions))
        .collect();
    assert!(files.contains(&("a.txt", FileStatus::Modified, 1)));
    assert!(files.contains(&("new.txt", FileStatus::Added, 1)));
    assert!(invalid(s.git.stash_show(&repo, "0000000").await).contains("isn’t there any more"));
    assert!(invalid(s.git.stash_show(&repo, "stash@{0}").await).contains("isn’t a put-aside entry"));

    // Apply keeps it in the list (and is named by id, wherever it sits now).
    let out = s.git.stash_apply(&repo, &one.id).await.unwrap();
    assert!(out.done);
    assert_eq!(out.summary, "Applied put-aside “Just b”");
    assert_eq!(read(&repo, "b.txt"), "b\nalso\n");
    assert_eq!(s.git.stash_list(&repo).await.unwrap().len(), 2);
    // Undo the apply: the file is as before, the entry still there.
    assert!(
        s.git
            .undo_last(&repo, Default::default())
            .await
            .unwrap()
            .done
    );
    assert_eq!(s.status(&repo).await, "");
    // Pop takes it off the list.
    let out = s.git.stash_pop(&repo, &all.id).await.unwrap();
    assert_eq!(out.summary, "Brought back “Changes on main”");
    assert_eq!(s.status(&repo).await, " M a.txt\n?? new.txt\n");
    assert_eq!(s.git.stash_list(&repo).await.unwrap().len(), 1);
    // Undo the pop: clean again and the entry is back in the list.
    assert!(
        s.git
            .undo_last(&repo, Default::default())
            .await
            .unwrap()
            .done
    );
    assert_eq!(s.status(&repo).await, "");
    let list = s.git.stash_list(&repo).await.unwrap();
    assert_eq!(list.len(), 2);
    assert!(list.iter().any(|e| e.id == all.id));

    // Drop, and bring it back from Recent actions.
    let journal = s.git.stash_drop(&repo, &one.id).await.unwrap();
    assert_eq!(s.git.stash_list(&repo).await.unwrap().len(), 1);
    let entries = s.git.journal_list(&repo).await.unwrap();
    assert_eq!(entries[0].id, journal);
    assert_eq!(entries[0].title, "Dropped put-aside “Just b”");
    assert!(invalid(s.git.stash_drop(&repo, &one.id).await).contains("isn’t there any more"));
    let undone = s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert!(undone.done);
    let list = s.git.stash_list(&repo).await.unwrap();
    assert_eq!(list[0].id, one.id);
    assert_eq!(list[0].message, "Just b");

    // Undoing "put aside" brings the changes straight back.
    write(&repo, "a.txt", "a\nagain\n");
    s.git.stash_save(&repo, "Third", true, &[]).await.unwrap();
    assert_eq!(s.git.stash_list(&repo).await.unwrap().len(), 3);
    s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert_eq!(read(&repo, "a.txt"), "a\nagain\n");
    assert_eq!(s.git.stash_list(&repo).await.unwrap().len(), 2);
}

#[tokio::test]
async fn put_aside_changes_that_no_longer_fit_go_through_the_conflict_view() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    s.commit(&repo, "a.txt", "one\ntwo\nthree\n", "A").await;
    s.commit(&repo, "b.txt", "b\n", "B").await;
    write(&repo, "a.txt", "one\ntwo aside\nthree\n");
    write(&repo, "b.txt", "b\naside\n");
    let entry = s
        .git
        .stash_save(&repo, "Half-done badge", true, &[])
        .await
        .unwrap();
    s.commit(&repo, "a.txt", "one\ntwo committed\nthree\n", "Meanwhile")
        .await;
    write(&repo, "other.txt", "uncommitted, unrelated\n");

    // A changed file in the way: refused before anything is touched.
    write(&repo, "b.txt", "b\nin the way\n");
    assert!(
        invalid(s.git.stash_pop(&repo, &entry.id).await).contains("touches files you have changed")
    );
    assert_eq!(read(&repo, "b.txt"), "b\nin the way\n");
    s.g(&repo, &["checkout", "--", "b.txt"]).await;

    let out = s.git.stash_pop(&repo, &entry.id).await.unwrap();
    assert!(!out.done);
    assert_eq!(out.conflicts, ["a.txt"]);
    let state = out.state;
    assert_eq!(state.kind, OperationKind::StashApply);
    assert_eq!(state.cause.as_deref(), Some("stash_pop"));
    assert_eq!(state.title, "Applying put-aside changes “Half-done badge”");
    assert!(!state.can_skip);
    let list = s.git.conflict_list(&repo).await.unwrap();
    assert_eq!(list.needs_you[0].path, "a.txt");
    assert_eq!(list.auto_resolved, ["b.txt"]);
    // The entry stays in the list until it is finished.
    assert_eq!(s.git.stash_list(&repo).await.unwrap().len(), 1);

    // Stop and go back: as it was, the unrelated file included.
    s.git.operation_abort(&repo).await.unwrap();
    assert_eq!(s.status(&repo).await, "?? other.txt\n");
    assert_eq!(read(&repo, "a.txt"), "one\ntwo committed\nthree\n");
    assert_eq!(s.git.stash_list(&repo).await.unwrap().len(), 1);
    assert_eq!(
        s.git.operation_state(&repo).await.unwrap().kind,
        OperationKind::None
    );

    // Again, resolve, finish: the changes wait as plain changes and the
    // entry is gone from the list.
    s.git.stash_pop(&repo, &entry.id).await.unwrap();
    let detail = s
        .git
        .conflict_choose(&repo, "a.txt", 0, Some(Choice::Theirs))
        .await
        .unwrap();
    assert!(detail.resolved);
    assert_eq!(detail.theirs_label, "put aside “Half-done badge”");
    let state = s.git.operation_state(&repo).await.unwrap();
    assert_eq!(state.kind, OperationKind::StashApply);
    assert!(state.can_continue);
    let done = s.git.operation_continue(&repo).await.unwrap();
    assert!(done.done);
    assert_eq!(done.summary, "Applied put-aside “Half-done badge”");
    assert_eq!(read(&repo, "a.txt"), "one\ntwo aside\nthree\n");
    assert_eq!(s.status(&repo).await, " M a.txt\n M b.txt\n?? other.txt\n");
    assert!(s.git.stash_list(&repo).await.unwrap().is_empty());
    assert_eq!(
        s.git.operation_state(&repo).await.unwrap().kind,
        OperationKind::None
    );

    // Apply (not pop) with a conflict keeps the entry afterwards.
    s.g(&repo, &["checkout", "--", "."]).await;
    write(&repo, "a.txt", "one\ntwo second\nthree\n");
    let second = s.git.stash_save(&repo, "Second", false, &[]).await.unwrap();
    s.commit(&repo, "a.txt", "one\ntwo third\nthree\n", "Later")
        .await;
    assert!(!s.git.stash_apply(&repo, &second.id).await.unwrap().done);
    s.git
        .conflict_take(&repo, "a.txt", TakeSide::Yours)
        .await
        .unwrap();
    assert!(s.git.operation_continue(&repo).await.unwrap().done);
    assert_eq!(read(&repo, "a.txt"), "one\ntwo third\nthree\n");
    assert_eq!(s.git.stash_list(&repo).await.unwrap().len(), 1);
}
