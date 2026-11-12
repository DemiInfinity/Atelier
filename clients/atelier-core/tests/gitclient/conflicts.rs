use atelier_core::git::conflict::{Choice, ConflictKind, Region, TakeSide};
use atelier_core::git::integrate::{MergeMode, OperationKind};

use crate::kit::*;

#[tokio::test]
async fn files_are_listed_and_resolved_region_by_region() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    let base: String = (1..=12).map(|n| format!("line {n}\n")).collect();
    s.commit(&repo, "code.txt", &base, "Base").await;
    s.commit(&repo, "other.txt", "x\n", "Other").await;
    s.g(&repo, &["switch", "--quiet", "-c", "feature"]).await;
    let yours = base
        .replace("line 2\n", "line 2 yours\n")
        .replace("line 10\n", "line 10 yours\nline 10b yours\n");
    s.commit(&repo, "code.txt", &yours, "Yours").await;
    s.g(&repo, &["switch", "--quiet", "main"]).await;
    let theirs = base
        .replace("line 2\n", "line 2 theirs\n")
        .replace("line 6\n", "line 6 theirs only\n")
        .replace("line 10\n", "line 10 theirs\n");
    s.commit(&repo, "code.txt", &theirs, "Theirs").await;
    s.commit(&repo, "auto.txt", "merged by itself\n", "Auto")
        .await;
    s.g(&repo, &["switch", "--quiet", "feature"]).await;

    // Nothing under way: an empty list, and no file to look at.
    let list = s.git.conflict_list(&repo).await.unwrap();
    assert_eq!(list.state.kind, OperationKind::None);
    assert!(list.needs_you.is_empty());
    assert!(invalid(s.git.conflict_file(&repo, "code.txt").await).contains("no conflict under way"));

    s.git.merge(&repo, "main", MergeMode::Auto).await.unwrap();
    let list = s.git.conflict_list(&repo).await.unwrap();
    assert_eq!((list.done, list.total), (0, 1));
    assert_eq!(list.auto_resolved, ["auto.txt"]);
    let file = &list.needs_you[0];
    assert_eq!(file.path, "code.txt");
    assert_eq!(file.kind, ConflictKind::BothModified);
    assert_eq!((file.regions, file.chosen), (2, 0));
    assert!(!file.resolved && !file.binary && !file.whole_file_only);

    let detail = s.git.conflict_file(&repo, "code.txt").await.unwrap();
    assert_eq!((detail.total, detail.chosen), (2, 0));
    assert_eq!(
        (detail.yours_label.as_str(), detail.theirs_label.as_str()),
        ("feature", "main")
    );
    assert_eq!(detail.regions.len(), 5);
    assert_eq!(
        detail.regions[1],
        Region::Conflict {
            index: 0,
            yours: vec!["line 2 yours".into()],
            theirs: vec!["line 2 theirs".into()],
            base: Some(vec!["line 2".into()]),
            yours_start: 2,
            theirs_start: 2,
            choice: None,
        }
    );
    // The change only they made is common ground: it merged cleanly.
    match &detail.regions[2] {
        Region::Common {
            lines, yours_start, ..
        } => {
            assert_eq!(*yours_start, 3);
            assert!(lines.contains(&"line 6 theirs only".to_string()));
        }
        other => panic!("{other:?}"),
    }
    match &detail.regions[3] {
        Region::Conflict {
            yours,
            theirs,
            yours_start,
            theirs_start,
            ..
        } => {
            assert_eq!(yours.len(), 2);
            assert_eq!(theirs, &["line 10 theirs".to_string()]);
            assert_eq!((*yours_start, *theirs_start), (10, 10));
        }
        other => panic!("{other:?}"),
    }
    assert_eq!(
        detail
            .result
            .iter()
            .filter(|l| l.from == "undecided")
            .count(),
        2
    );

    // One choice: the file on disk has it, the other conflict still marked.
    let detail = s
        .git
        .conflict_choose(&repo, "code.txt", 0, Some(Choice::Theirs))
        .await
        .unwrap();
    assert_eq!((detail.chosen, detail.resolved), (1, false));
    let on_disk = read(&repo, "code.txt");
    assert!(on_disk.starts_with("line 1\nline 2 theirs\nline 3\n"));
    assert!(on_disk.contains("<<<<<<< feature\nline 10 yours\nline 10b yours\n||||||| base\nline 10\n=======\nline 10 theirs\n>>>>>>> main\n"));
    assert_eq!(
        s.git.conflict_list(&repo).await.unwrap().needs_you[0].chosen,
        1
    );
    // Looking again remembers the choice.
    let again = s.git.conflict_file(&repo, "code.txt").await.unwrap();
    assert!(matches!(
        &again.regions[1],
        Region::Conflict {
            choice: Some(Choice::Theirs),
            ..
        }
    ));
    assert_eq!(again.result[1].from, "theirs");
    // Still marked: it can't be called resolved by hand, and the merge waits.
    assert!(
        invalid(s.git.conflict_mark_resolved(&repo, "code.txt").await).contains("conflict markers")
    );
    assert!(invalid(s.git.operation_continue(&repo).await).contains("1 file has a conflict"));
    // Take the choice back, choose differently, then type the other one.
    s.git
        .conflict_choose(&repo, "code.txt", 0, None)
        .await
        .unwrap();
    assert!(read(&repo, "code.txt").contains("<<<<<<< feature\nline 2 yours\n"));
    s.git
        .conflict_choose(
            &repo,
            "code.txt",
            0,
            Some(Choice::Both { yours_first: false }),
        )
        .await
        .unwrap();
    assert!(invalid(
        s.git
            .conflict_choose(&repo, "code.txt", 9, Some(Choice::Yours))
            .await
    )
    .contains("no conflict number 10"));
    let detail = s
        .git
        .conflict_choose(
            &repo,
            "code.txt",
            1,
            Some(Choice::Custom {
                text: "line 10 typed".into(),
            }),
        )
        .await
        .unwrap();
    assert!(detail.resolved);
    assert_eq!(
        read(&repo, "code.txt"),
        base.replace("line 2\n", "line 2 theirs\nline 2 yours\n")
            .replace("line 6\n", "line 6 theirs only\n")
            .replace("line 10\n", "line 10 typed\n")
    );
    let list = s.git.conflict_list(&repo).await.unwrap();
    assert_eq!((list.done, list.total), (1, 1));
    assert_eq!(list.needs_you[0].how.as_deref(), Some("mixed"));
    assert!(list.state.can_continue);
    assert_eq!(list.auto_resolved, ["auto.txt"]);
    let resolved = s.git.conflict_file(&repo, "code.txt").await.unwrap();
    assert!(resolved.resolved && resolved.regions.is_empty());
    assert_eq!(resolved.result[1].text, "line 2 theirs");
    assert!(invalid(
        s.git
            .conflict_choose(&repo, "code.txt", 0, Some(Choice::Yours))
            .await
    )
    .contains("resolved already"));

    // Reopen it: in conflict again, to choose afresh.
    s.git.conflict_reopen(&repo, "code.txt").await.unwrap();
    let list = s.git.conflict_list(&repo).await.unwrap();
    assert_eq!((list.done, list.needs_you[0].chosen), (0, 0));
    // Edited by hand in an editor, then marked resolved.
    write(&repo, "code.txt", "resolved by hand\n");
    s.git
        .conflict_mark_resolved(&repo, "code.txt")
        .await
        .unwrap();
    let list = s.git.conflict_list(&repo).await.unwrap();
    assert_eq!(list.needs_you[0].how.as_deref(), Some("edited"));
    assert!(invalid(s.git.conflict_file(&repo, "other.txt").await).contains("isn’t in conflict"));
    assert!(s.git.operation_continue(&repo).await.unwrap().done);
    assert_eq!(
        s.g(&repo, &["show", "HEAD:code.txt"]).await,
        "resolved by hand"
    );
    // The notes are gone with the merge.
    assert!(!repo.join(".git/atelier/op.json").exists());
}

#[tokio::test]
async fn deletions_binaries_and_new_files_are_whole_file_choices() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    s.commit(&repo, "gone-here.txt", "a\n", "one").await;
    s.commit(&repo, "gone-there.txt", "b\n", "two").await;
    std::fs::write(repo.join("pic.bin"), [0u8, 1]).unwrap();
    s.g(&repo, &["add", "pic.bin"]).await;
    s.g(&repo, &["commit", "--quiet", "-m", "pic"]).await;
    s.g(&repo, &["switch", "--quiet", "-c", "feature"]).await;
    s.g(&repo, &["rm", "--quiet", "gone-here.txt"]).await;
    write(&repo, "gone-there.txt", "b\nyours\n");
    std::fs::write(repo.join("pic.bin"), [0u8, 2, 2]).unwrap();
    write(&repo, "both-new.txt", "yours\n");
    s.g(&repo, &["add", "-A"]).await;
    s.g(&repo, &["commit", "--quiet", "-m", "yours"]).await;
    s.g(&repo, &["switch", "--quiet", "main"]).await;
    write(&repo, "gone-here.txt", "a\ntheirs\n");
    s.g(&repo, &["rm", "--quiet", "gone-there.txt"]).await;
    std::fs::write(repo.join("pic.bin"), [0u8, 3, 3, 3]).unwrap();
    write(&repo, "both-new.txt", "theirs\n");
    s.g(&repo, &["add", "-A"]).await;
    s.g(&repo, &["commit", "--quiet", "-m", "theirs"]).await;
    s.g(&repo, &["switch", "--quiet", "feature"]).await;

    let out = s.git.merge(&repo, "main", MergeMode::Auto).await.unwrap();
    assert_eq!(out.conflicts.len(), 4);
    let list = s.git.conflict_list(&repo).await.unwrap();
    let kinds: Vec<(&str, ConflictKind, bool, bool)> = list
        .needs_you
        .iter()
        .map(|f| (f.path.as_str(), f.kind, f.binary, f.whole_file_only))
        .collect();
    assert_eq!(
        kinds,
        [
            ("both-new.txt", ConflictKind::BothAdded, false, false),
            ("gone-here.txt", ConflictKind::DeletedByYou, false, true),
            ("gone-there.txt", ConflictKind::DeletedByThem, false, true),
            ("pic.bin", ConflictKind::BothModified, true, true),
        ]
    );
    // A file new on both sides has no base.
    let detail = s.git.conflict_file(&repo, "both-new.txt").await.unwrap();
    assert!(matches!(
        &detail.regions[0],
        Region::Conflict { base: None, .. }
    ));
    // No region to choose in the others.
    let detail = s.git.conflict_file(&repo, "pic.bin").await.unwrap();
    assert!(detail.binary && detail.whole_file_only && detail.regions.is_empty());
    assert!(invalid(
        s.git
            .conflict_choose(&repo, "pic.bin", 0, Some(Choice::Yours))
            .await
    )
    .contains("taken whole"));
    assert!(invalid(
        s.git
            .conflict_choose(&repo, "gone-here.txt", 0, Some(Choice::Yours))
            .await
    )
    .contains("taken whole"));

    // Yours deleted it: taking yours removes the file; taking theirs keeps theirs.
    s.git
        .conflict_take(&repo, "gone-here.txt", TakeSide::Yours)
        .await
        .unwrap();
    assert!(!repo.join("gone-here.txt").exists());
    s.git
        .conflict_take(&repo, "gone-there.txt", TakeSide::Yours)
        .await
        .unwrap();
    assert_eq!(read(&repo, "gone-there.txt"), "b\nyours\n");
    s.git
        .conflict_take(&repo, "pic.bin", TakeSide::Theirs)
        .await
        .unwrap();
    assert_eq!(std::fs::read(repo.join("pic.bin")).unwrap(), [0u8, 3, 3, 3]);
    s.git
        .conflict_take(&repo, "both-new.txt", TakeSide::Theirs)
        .await
        .unwrap();
    assert_eq!(read(&repo, "both-new.txt"), "theirs\n");
    assert!(
        invalid(s.git.conflict_take(&repo, "pic.bin", TakeSide::Yours).await)
            .contains("resolved already")
    );
    let list = s.git.conflict_list(&repo).await.unwrap();
    assert_eq!((list.done, list.total), (4, 4));
    let how: Vec<&str> = list
        .needs_you
        .iter()
        .map(|f| f.how.as_deref().unwrap())
        .collect();
    assert_eq!(how, ["theirs", "yours", "yours", "theirs"]);
    assert_eq!(list.needs_you[1].kind, ConflictKind::DeletedByYou);
    assert!(s.git.operation_continue(&repo).await.unwrap().done);
    assert_eq!(s.status(&repo).await, "");
}

#[tokio::test]
async fn in_a_rebase_yours_is_the_commit_being_replayed() {
    let s = Sandbox::new();
    let repo = s.conflicting("r").await;
    s.git.rebase_onto(&repo, "main").await.unwrap();
    let detail = s.git.conflict_file(&repo, "a.txt").await.unwrap();
    assert_eq!(
        detail.regions[1],
        Region::Conflict {
            index: 0,
            yours: vec!["two yours".into()],
            theirs: vec!["two theirs".into()],
            base: Some(vec!["two".into()]),
            yours_start: 2,
            theirs_start: 2,
            choice: None,
        }
    );
    // Whole-file choices follow the same sense.
    s.git
        .conflict_take(&repo, "a.txt", TakeSide::Yours)
        .await
        .unwrap();
    assert_eq!(read(&repo, "a.txt"), "one\ntwo yours\nthree\nfour\nfive\n");
    s.git.operation_continue(&repo).await.unwrap();
    s.git
        .conflict_take(&repo, "b.txt", TakeSide::Theirs)
        .await
        .unwrap();
    assert_eq!(read(&repo, "b.txt"), "alpha\nbeta theirs\ngamma\n");
    // Taking theirs left that commit with nothing in it.
    let out = s.git.operation_continue(&repo).await;
    match out {
        Ok(out) => assert!(out.done, "{out:?}"),
        Err(e) => {
            assert!(invalid::<()>(Err(e)).contains("Skip it"));
            assert!(s.git.operation_skip(&repo).await.unwrap().done);
        }
    }
    assert_eq!(
        s.git.operation_state(&repo).await.unwrap().kind,
        OperationKind::None
    );
    assert_eq!(read(&repo, "own.txt"), "mine\n");
}
