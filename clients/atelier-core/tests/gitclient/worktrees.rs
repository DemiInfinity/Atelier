use atelier_core::git::branch::SwitchHow;
use atelier_core::git::worktree::{linked_worktree_main, WorktreeSource};

use crate::kit::*;

#[tokio::test]
async fn worktrees_are_made_listed_locked_and_removed() {
    let s = Sandbox::new();
    let (repo, _) = s.repo_with_remote("Atelier").await;
    s.g(&repo, &["branch", "fix/setup-race"]).await;
    s.commit(&repo, "b.txt", "b\n", "Second").await;

    let suggested = s
        .git
        .worktree_suggest_path(&repo, "fix/setup-race")
        .await
        .unwrap();
    assert_eq!(suggested, s.path("Atelier.worktrees/fix-setup-race"));
    let list = s.git.worktree_list(&repo).await.unwrap();
    assert_eq!(list.len(), 1);
    assert!(list[0].main && list[0].current);
    assert_eq!(
        (list[0].branch.as_deref(), list[0].ahead, list[0].behind),
        (Some("main"), 1, 0)
    );

    // An existing branch in its own folder.
    let tree = s
        .git
        .worktree_add(
            &repo,
            &suggested,
            &WorktreeSource::Branch {
                name: "fix/setup-race".into(),
            },
        )
        .await
        .unwrap();
    assert_eq!(tree.branch.as_deref(), Some("fix/setup-race"));
    assert!(!tree.main && !tree.current && !tree.locked && tree.dirty == 0);
    assert_eq!(tree.subject.as_deref(), Some("Start"));
    assert!(suggested.join("README.md").is_file());
    assert!(
        suggested.join(".git").is_file(),
        "a linked worktree's .git is a file"
    );
    // A new branch from a fetched ref, and a commit with no branch.
    let second = s.path("Atelier.worktrees/review");
    s.git
        .worktree_add(
            &repo,
            &second,
            &WorktreeSource::NewBranch {
                name: "review/14".into(),
                from: Some("origin/main".into()),
            },
        )
        .await
        .unwrap();
    let third = s.path("elsewhere/detached");
    let head = s.head(&repo).await;
    let detached = s
        .git
        .worktree_add(
            &repo,
            &third,
            &WorktreeSource::Detached { rev: head.clone() },
        )
        .await
        .unwrap();
    assert_eq!((detached.branch, detached.head), (None, Some(head)));

    // Refusals: inside the repository, a taken folder, a branch that is
    // open elsewhere, a branch that isn't there, a name that exists.
    let source = WorktreeSource::Branch {
        name: "main".into(),
    };
    assert!(invalid(
        s.git
            .worktree_add(&repo, &repo.join("inside"), &source)
            .await
    )
    .contains("beside the repository"));
    assert!(
        invalid(s.git.worktree_add(&repo, &suggested, &source).await).contains("already exists")
    );
    let free = s.path("Atelier.worktrees/free");
    assert!(invalid(s.git.worktree_add(&repo, &free, &source).await).contains("already open in"));
    assert!(invalid(
        s.git
            .worktree_add(
                &repo,
                &free,
                &WorktreeSource::Branch {
                    name: "nope".into()
                }
            )
            .await
    )
    .contains("no branch called"));
    assert!(invalid(
        s.git
            .worktree_add(
                &repo,
                &free,
                &WorktreeSource::NewBranch {
                    name: "review/14".into(),
                    from: None
                }
            )
            .await
    )
    .contains("already a branch"));
    assert!(invalid(
        s.git
            .worktree_add(&repo, std::path::Path::new("relative"), &source)
            .await
    )
    .contains("full path"));
    assert!(!free.exists());

    // Branches say where they are open; switching to one is refused.
    let branches = s.git.branch_list(&repo).await.unwrap();
    let open = branches
        .local
        .iter()
        .find(|b| b.name == "fix/setup-race")
        .unwrap();
    assert_eq!(open.worktree.as_deref(), Some(suggested.to_str().unwrap()));
    assert_eq!(
        branches
            .local
            .iter()
            .find(|b| b.name == "main")
            .unwrap()
            .worktree,
        None
    );
    assert!(invalid(
        s.git
            .branch_switch(&repo, "fix/setup-race", SwitchHow::Ask)
            .await
    )
    .contains("open in the worktree at"));
    assert!(
        invalid(s.git.branch_delete(&repo, "fix/setup-race", true).await)
            .contains("Remove that worktree first")
    );

    // A linked worktree's folder is recognised and mapped to its repository,
    // by git and from the files alone.
    let identity = s.git.repo_identity(&suggested).await.unwrap();
    assert!(identity.linked);
    assert_eq!(
        (identity.toplevel, identity.main),
        (suggested.clone(), repo.clone())
    );
    let main = s.git.repo_identity(&repo).await.unwrap();
    assert!(!main.linked && main.main == repo);
    assert_eq!(linked_worktree_main(&suggested), Some(repo.clone()));
    assert_eq!(linked_worktree_main(&repo), None);
    assert_eq!(
        atelier_core::git::discover_repos(&[s.path("Atelier.worktrees")], 2).len(),
        2
    );
    // Asked from inside the worktree, "current" moves and so does the journal.
    let from_inside = s.git.worktree_list(&suggested).await.unwrap();
    assert!(from_inside[1].current && !from_inside[0].current);
    assert!(s.git.journal_list(&suggested).await.unwrap().is_empty());
    assert_eq!(s.git.journal_list(&repo).await.unwrap().len(), 3);

    // Dirty: it says why it can't be removed.
    write(&suggested, "README.md", "changed in the worktree\n");
    write(&suggested, "new.txt", "n\n");
    let list = s.git.worktree_list(&repo).await.unwrap();
    assert_eq!(list[1].dirty, 2);
    assert!(
        invalid(s.git.worktree_remove(&repo, &suggested, false).await)
            .contains("has 2 uncommitted changes — commit, put aside or discard them first")
    );
    assert!(suggested.is_dir());
    assert!(invalid(s.git.worktree_remove(&repo, &repo, true).await).contains("main folder"));
    assert!(
        invalid(s.git.worktree_remove(&suggested, &suggested, true).await).contains("You are in")
    );
    assert!(invalid(
        s.git
            .worktree_remove(&repo, &s.path("nowhere"), false)
            .await
    )
    .contains("isn’t a worktree"));

    // Locked, with a reason.
    s.git
        .worktree_lock(&repo, &second, "Locked while the agent works")
        .await
        .unwrap();
    let list = s.git.worktree_list(&repo).await.unwrap();
    let locked = list
        .iter()
        .find(|t| t.branch.as_deref() == Some("review/14"))
        .unwrap();
    assert_eq!(
        (locked.locked, locked.lock_reason.as_deref()),
        (true, Some("Locked while the agent works"))
    );
    assert!(invalid(s.git.worktree_remove(&repo, &second, false).await)
        .contains("locked: Locked while the agent works"));
    s.git.worktree_unlock(&repo, &second).await.unwrap();
    s.git.worktree_remove(&repo, &second, false).await.unwrap();
    assert!(!second.exists());
    assert_eq!(
        s.g(&repo, &["rev-parse", "--verify", "review/14"])
            .await
            .len(),
        40,
        "the branch stays"
    );
    // Undo puts the folder back on its branch.
    let undone = s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert_eq!(undone.title, "Undid “Removed the worktree of review/14”");
    assert!(second.join("README.md").is_file());

    // Forced: the folder goes, its uncommitted changes are kept.
    s.git
        .worktree_remove(&repo, &suggested, true)
        .await
        .unwrap();
    assert!(!suggested.exists());
    let kept = s.git.discarded_list(&repo).await.unwrap();
    assert_eq!(
        kept[0].title,
        "Discarded changes in the worktree of fix/setup-race"
    );
    assert_eq!(kept[0].discarded.as_ref().unwrap().files, 2);

    // A folder deleted by hand is prunable, and prune forgets it.
    std::fs::remove_dir_all(&third).unwrap();
    let list = s.git.worktree_list(&repo).await.unwrap();
    assert!(list.iter().any(|t| t.prunable));
    let pruned = s.git.worktree_prune(&repo).await.unwrap();
    assert_eq!(pruned.len(), 1);
    assert_eq!(s.git.worktree_list(&repo).await.unwrap().len(), 2);
}

#[tokio::test]
async fn a_new_worktree_can_be_undone() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    s.commit(&repo, "a.txt", "a\n", "A").await;
    let path = s.path("r.worktrees/spike");
    s.git
        .worktree_add(
            &repo,
            &path,
            &WorktreeSource::NewBranch {
                name: "spike".into(),
                from: None,
            },
        )
        .await
        .unwrap();
    let journal = s.git.journal_list(&repo).await.unwrap();
    assert_eq!(journal[0].title, "Opened spike in a new worktree");
    // With changes in it, undoing would lose them: refused, nothing removed.
    write(&path, "a.txt", "work in the worktree\n");
    let failed = s.git.undo_last(&repo, Default::default()).await;
    assert!(invalid(failed).contains("can’t be removed while it has uncommitted changes"));
    assert!(path.is_dir());
    s.g(&path, &["checkout", "--", "a.txt"]).await;
    assert!(
        s.git
            .undo_last(&repo, Default::default())
            .await
            .unwrap()
            .done
    );
    assert!(!path.exists());
    assert!(!s
        .git
        .branch_list(&repo)
        .await
        .unwrap()
        .local
        .iter()
        .any(|b| b.name == "spike"));
}
