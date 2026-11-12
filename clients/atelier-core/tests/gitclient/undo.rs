use atelier_core::git::integrate::{MergeMode, ResetMode};
use atelier_core::git::stage::Selection;
use atelier_core::git::undo::UndoOptions;

use crate::kit::*;

#[tokio::test]
async fn going_back_steps_through_recent_actions() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    let a = s.commit(&repo, "a.txt", "a\n", "A").await;
    let b = s.commit(&repo, "b.txt", "b\n", "B").await;
    let c = s.commit(&repo, "c.txt", "c\n", "C").await;
    s.g(&repo, &["branch", "side", &a]).await;

    let nothing = s
        .git
        .undo_last(&repo, UndoOptions::default())
        .await
        .unwrap();
    assert_eq!(nothing.refusal.unwrap().reason, "nothing");

    // Four actions.
    s.git.reset(&repo, &b, ResetMode::Hard).await.unwrap();
    s.git.branch_delete(&repo, "side", true).await.unwrap();
    write(&repo, "d.txt", "d\n");
    s.git.stage_all(&repo).await.unwrap();
    let d = s
        .git
        .commit_staged(&repo, "D", "", false)
        .await
        .unwrap()
        .sha;
    s.git.tag_create(&repo, "v1", None, None).await.unwrap();

    let journal = s.git.journal_list(&repo).await.unwrap();
    let titles: Vec<&str> = journal.iter().map(|e| e.title.as_str()).collect();
    assert_eq!(
        titles,
        [
            format!("Tagged {} as v1", &d[..7]).as_str(),
            "Committed “D”",
            "Deleted branch side",
            "Reset 1 commit (threw the changes away)"
        ]
    );
    assert!(journal
        .iter()
        .all(|e| e.finished && !e.undone && !e.how.is_empty()));
    assert!(journal[3].how.contains("main back at"));
    assert_eq!(journal[3].head.branch.as_deref(), Some("main"));
    assert_eq!(journal[3].refs[0].before.as_deref(), Some(c.as_str()));
    let reset_id = journal[3].id;
    let delete_id = journal[2].id;

    // Back to before the branch was deleted: the tag and the commit go, the
    // branch returns; the reset before it stays done.
    let out = s
        .git
        .go_back_to_before(&repo, delete_id, UndoOptions::default())
        .await
        .unwrap();
    assert!(out.done);
    assert_eq!(out.title, "Went back to before “Deleted branch side”");
    assert_eq!(s.head(&repo).await, b);
    assert_eq!(s.g(&repo, &["rev-parse", "side"]).await, a);
    assert_eq!(s.g(&repo, &["tag", "--list"]).await, "");
    // What was staged before that commit is staged again: work made between
    // two actions is never dropped by stepping back over them.
    assert_eq!(s.status(&repo).await, "A  d.txt\n");
    let journal = s.git.journal_list(&repo).await.unwrap();
    assert_eq!(journal[0].kind, "go_back");
    assert!(journal[1].undone && journal[2].undone && journal[3].undone && !journal[4].undone);
    // Undo now steps over what was already undone, to the reset; which it
    // won't do over uncommitted work.
    let refused = s
        .git
        .undo_last(&repo, UndoOptions::default())
        .await
        .unwrap();
    assert_eq!(refused.refusal.unwrap().reason, "dirty");
    s.g(&repo, &["rm", "--quiet", "--force", "d.txt"]).await;
    let out = s
        .git
        .undo_last(&repo, UndoOptions::default())
        .await
        .unwrap();
    assert_eq!(out.title, "Undid “Reset 1 commit (threw the changes away)”");
    assert_eq!(s.head(&repo).await, c);
    let nothing = s
        .git
        .undo_last(&repo, UndoOptions::default())
        .await
        .unwrap();
    assert!(!nothing.done);

    // Going back is itself in the list, so it can be gone back on: forward
    // again to where the tag existed.
    let journal = s.git.journal_list(&repo).await.unwrap();
    let went_back = journal.iter().find(|e| e.kind == "go_back").unwrap().id;
    let out = s
        .git
        .go_back_to_before(&repo, went_back, UndoOptions::default())
        .await
        .unwrap();
    assert!(out.done);
    assert_eq!(s.head(&repo).await, d);
    assert_eq!(s.g(&repo, &["tag", "--list"]).await, "v1");
    assert!(s.g(&repo, &["branch", "--list", "side"]).await.is_empty());
    // And all the way back to the very start.
    let out = s
        .git
        .go_back_to_before(&repo, reset_id, UndoOptions::default())
        .await
        .unwrap();
    assert!(out.done);
    assert_eq!(s.head(&repo).await, c);
    assert_eq!(s.g(&repo, &["rev-parse", "side"]).await, a);
    assert!(invalid(
        s.git
            .go_back_to_before(&repo, 7, UndoOptions::default())
            .await
    )
    .contains("isn’t kept"));
}

#[tokio::test]
async fn undo_refuses_to_lose_work_and_offers_a_way() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    s.commit(&repo, "a.txt", "a\n", "A").await;
    let b = s.commit(&repo, "b.txt", "b\n", "B").await;
    s.g(&repo, &["switch", "--quiet", "-c", "side", "HEAD~1"])
        .await;
    s.commit(&repo, "s.txt", "s\n", "S").await;
    s.g(&repo, &["switch", "--quiet", "main"]).await;
    s.git.merge(&repo, "side", MergeMode::NoFf).await.unwrap();
    let merged = s.head(&repo).await;

    // New uncommitted work since the merge.
    write(&repo, "b.txt", "b\nwork since\n");
    write(&repo, "fresh.txt", "new since\n");
    let refused = s
        .git
        .undo_last(&repo, UndoOptions::default())
        .await
        .unwrap();
    assert!(!refused.done);
    let refusal = refused.refusal.unwrap();
    assert_eq!(refusal.reason, "dirty");
    assert!(refusal.can_put_aside && !refusal.can_force);
    assert!(refusal.message.contains("Put them aside first"));
    assert_eq!(s.head(&repo).await, merged, "nothing moved");
    assert_eq!(read(&repo, "b.txt"), "b\nwork since\n");

    // Put aside first, then undo.
    let out = s
        .git
        .undo_last(
            &repo,
            UndoOptions {
                put_aside: true,
                force: false,
            },
        )
        .await
        .unwrap();
    assert!(out.done);
    assert_eq!(
        out.put_aside.as_deref(),
        Some("Before going back: Merged side into main")
    );
    assert_eq!(s.head(&repo).await, b);
    assert_eq!(s.status(&repo).await, "");
    let aside = s.git.stash_list(&repo).await.unwrap();
    assert_eq!(aside[0].message, "Before going back: Merged side into main");
    assert_eq!(aside[0].files, 2);

    // A commit made outside the app since the action: going back would take
    // it off the branch, so it asks.
    s.git.merge(&repo, "side", MergeMode::NoFf).await.unwrap();
    let outside = s
        .commit(&repo, "outside.txt", "o\n", "Made in a terminal")
        .await;
    let refused = s
        .git
        .undo_last(&repo, UndoOptions::default())
        .await
        .unwrap();
    let refusal = refused.refusal.unwrap();
    assert_eq!(refusal.reason, "moved");
    assert!(refusal.can_force);
    assert_eq!(s.head(&repo).await, outside);
    let out = s
        .git
        .undo_last(
            &repo,
            UndoOptions {
                put_aside: false,
                force: true,
            },
        )
        .await
        .unwrap();
    assert!(out.done);
    assert_eq!(s.head(&repo).await, b);
    // The outside commit is not lost: the undo's own entry leads back to it.
    let journal = s.git.journal_list(&repo).await.unwrap();
    assert_eq!(journal[0].kind, "undo");
    s.git
        .go_back_to_before(&repo, journal[0].id, UndoOptions::default())
        .await
        .unwrap();
    assert_eq!(s.head(&repo).await, outside);
}

#[tokio::test]
async fn the_journal_lives_in_the_git_folder_and_is_pruned_after_thirty_days() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    let a = s.commit(&repo, "a.txt", "a\n", "A").await;
    s.commit(&repo, "b.txt", "b\n", "B").await;
    write(&repo, "a.txt", "a\nuncommitted\n");
    s.git.reset(&repo, &a, ResetMode::Hard).await.unwrap();
    write(&repo, "a.txt", "again\n");
    s.git
        .discard(&repo, "a.txt", &Selection::File, None)
        .await
        .unwrap();

    // Refs keep everything alive, out of sight of branches and tags.
    let refs = s
        .g(
            &repo,
            &["for-each-ref", "--format=%(refname)", "refs/atelier"],
        )
        .await;
    assert!(refs
        .lines()
        .any(|r| r.starts_with("refs/atelier/undo/") && r.ends_with("/head")));
    assert!(refs
        .lines()
        .any(|r| r.starts_with("refs/atelier/undo/") && r.ends_with("/work")));
    assert_eq!(
        refs.lines()
            .filter(|r| r.starts_with("refs/atelier/discarded/"))
            .count(),
        2
    );
    s.g(&repo, &["gc", "--quiet", "--prune=now"]).await;
    assert_eq!(s.g(&repo, &["branch", "--list"]).await, "* main");
    // The working tree holds only the project's own files.
    assert_eq!(s.status(&repo).await, "");
    let files: Vec<String> = std::fs::read_dir(&repo)
        .unwrap()
        .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
        .filter(|f| f != ".git")
        .collect();
    assert_eq!(files, ["a.txt"]);
    let journal = std::fs::read_to_string(repo.join(".git/atelier/journal.jsonl")).unwrap();
    assert!(journal.lines().count() >= 3);

    // Twenty-nine days on: everything is still there, and still works.
    let now = time::OffsetDateTime::now_utc().unix_timestamp();
    assert_eq!(
        s.git.journal_prune(&repo, now + 29 * 86_400).await.unwrap(),
        0
    );
    assert_eq!(s.git.journal_list(&repo).await.unwrap().len(), 3);
    assert_eq!(s.git.discarded_list(&repo).await.unwrap().len(), 2);
    // Thirty-one days on: gone, with the objects they kept.
    assert_eq!(
        s.git.journal_prune(&repo, now + 31 * 86_400).await.unwrap(),
        3
    );
    assert!(s.git.journal_list(&repo).await.unwrap().is_empty());
    assert!(s.git.discarded_list(&repo).await.unwrap().is_empty());
    assert_eq!(s.g(&repo, &["for-each-ref", "refs/atelier"]).await, "");
    let nothing = s
        .git
        .undo_last(&repo, UndoOptions::default())
        .await
        .unwrap();
    assert_eq!(nothing.refusal.unwrap().reason, "nothing");
}

#[tokio::test]
async fn an_action_finished_outside_the_app_is_closed_by_what_happened() {
    let s = Sandbox::new();
    let repo = s.conflicting("r").await;
    let before = s.head(&repo).await;
    // A merge started here, finished in a terminal.
    s.git.merge(&repo, "main", MergeMode::Auto).await.unwrap();
    s.g(&repo, &["checkout", "--theirs", "--", "a.txt", "b.txt"])
        .await;
    s.g(&repo, &["add", "-A"]).await;
    s.g(&repo, &["commit", "--quiet", "--no-edit"]).await;
    let journal = s.git.journal_list(&repo).await.unwrap();
    assert_eq!(journal.len(), 1);
    assert_eq!(journal[0].title, "Merged main into feature");
    assert!(
        s.git
            .undo_last(&repo, UndoOptions::default())
            .await
            .unwrap()
            .done
    );
    assert_eq!(s.head(&repo).await, before);

    // A merge started here, abandoned in a terminal: forgotten.
    s.git.merge(&repo, "main", MergeMode::Auto).await.unwrap();
    s.g(&repo, &["merge", "--abort"]).await;
    let journal = s.git.journal_list(&repo).await.unwrap();
    assert!(journal.iter().all(|e| e.kind != "merge" || e.undone));
    assert_eq!(journal.iter().filter(|e| e.kind == "merge").count(), 1);
}
