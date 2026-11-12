use atelier_core::git::branch::{PullMode, SwitchHow};
use atelier_core::git::conflict::TakeSide;
use atelier_core::git::integrate::OperationKind;

use crate::kit::*;

#[tokio::test]
async fn branches_are_listed_with_where_they_stand() {
    let s = Sandbox::new();
    let (repo, bare) = s.repo_with_remote("r").await;
    let other = s.path("other.git");
    std::fs::create_dir_all(&other).unwrap();
    s.g(&other, &["init", "--quiet", "--bare", "-b", "main"])
        .await;
    s.g(
        &repo,
        &["remote", "add", "team/github", other.to_str().unwrap()],
    )
    .await;
    s.g(&repo, &["push", "--quiet", "team/github", "main"])
        .await;
    s.g(&repo, &["switch", "--quiet", "-c", "feature/ssh"])
        .await;
    s.commit(&repo, "ssh.txt", "1\n", "SSH one").await;
    s.g(&repo, &["push", "--quiet", "-u", "origin", "feature/ssh"])
        .await;
    s.commit(&repo, "ssh.txt", "1\n2\n", "SSH two").await;
    // Someone else pushes to it meanwhile.
    let theirs = s.clone_of(&bare, "theirs").await;
    s.g(&theirs, &["switch", "--quiet", "feature/ssh"]).await;
    s.commit(&theirs, "else.txt", "e\n", "From someone else")
        .await;
    s.g(&theirs, &["push", "--quiet"]).await;
    s.g(&repo, &["branch", "only-here"]).await;
    s.git.fetch_remotes(&repo, None, true).await.unwrap();

    let list = s.git.branch_list(&repo).await.unwrap();
    assert_eq!(list.current.as_deref(), Some("feature/ssh"));
    let local: Vec<(&str, bool, Option<&str>, u64, u64)> = list
        .local
        .iter()
        .map(|b| {
            (
                b.name.as_str(),
                b.current,
                b.upstream.as_deref(),
                b.ahead,
                b.behind,
            )
        })
        .collect();
    assert_eq!(
        local,
        [
            ("feature/ssh", true, Some("origin/feature/ssh"), 1, 1),
            ("main", false, Some("origin/main"), 0, 0),
            ("only-here", false, None, 0, 0),
        ]
    );
    assert_eq!(list.local[0].subject, "SSH two");
    assert_eq!(list.local[0].author, "Test Person");
    assert_eq!(list.local[0].full_ref, "refs/heads/feature/ssh");
    assert!(list.local[0].time.starts_with("2026-11-10"));
    // A remote whose name has a slash in it keeps its branches.
    let remotes: Vec<(&str, Vec<&str>)> = list
        .remotes
        .iter()
        .map(|r| {
            (
                r.remote.as_str(),
                r.branches.iter().map(|b| b.name.as_str()).collect(),
            )
        })
        .collect();
    assert_eq!(
        remotes,
        [
            ("origin", vec!["feature/ssh", "main"]),
            ("team/github", vec!["main"])
        ]
    );
    assert_eq!(list.remotes[0].branches[0].subject, "From someone else");
    assert_eq!(
        list.remotes[0].branches[0].remote.as_deref(),
        Some("origin")
    );

    // What it follows.
    s.git
        .branch_set_upstream(&repo, "only-here", Some("team/github/main"))
        .await
        .unwrap();
    let list = s.git.branch_list(&repo).await.unwrap();
    assert_eq!(list.local[2].upstream.as_deref(), Some("team/github/main"));
    s.git
        .branch_set_upstream(&repo, "only-here", None)
        .await
        .unwrap();
    assert_eq!(
        s.git.branch_list(&repo).await.unwrap().local[2].upstream,
        None
    );
    assert!(invalid(
        s.git
            .branch_set_upstream(&repo, "only-here", Some("origin/nope"))
            .await
    )
    .contains("no origin/nope"));
    assert!(
        invalid(s.git.branch_set_upstream(&repo, "nope", None).await).contains("no branch called")
    );

    // A branch deleted on the remote: pruned on fetch, and the follower knows.
    s.g(
        &theirs,
        &["push", "--quiet", "origin", "--delete", "feature/ssh"],
    )
    .await;
    s.git
        .fetch_remotes(&repo, Some("origin"), true)
        .await
        .unwrap();
    let list = s.git.branch_list(&repo).await.unwrap();
    assert!(list.local[0].upstream_gone);
    assert_eq!(list.remotes[0].branches.len(), 1);
    assert!(
        invalid(s.git.fetch_remotes(&repo, Some("nope"), false).await).contains("no remote called")
    );
}

#[tokio::test]
async fn create_rename_delete() {
    let s = Sandbox::new();
    let (repo, _) = s.repo_with_remote("r").await;
    let start = s.head(&repo).await;
    s.commit(&repo, "b.txt", "b\n", "Second").await;

    s.git
        .branch_create(&repo, "spike/quic", None)
        .await
        .unwrap();
    s.git
        .branch_create(&repo, "from-start", Some(&start[..8]))
        .await
        .unwrap();
    s.git
        .branch_create(&repo, "from-remote", Some("origin/main"))
        .await
        .unwrap();
    assert_eq!(s.g(&repo, &["rev-parse", "from-start"]).await, start);
    assert_eq!(
        s.g(&repo, &["symbolic-ref", "--short", "HEAD"]).await,
        "main"
    );
    assert!(
        invalid(s.git.branch_create(&repo, "spike/quic", None).await).contains("already a branch")
    );
    assert!(invalid(s.git.branch_create(&repo, "bad name", None).await)
        .contains("can’t be a branch name"));
    assert!(
        invalid(s.git.branch_create(&repo, "-D", None).await).contains("can’t be a branch name")
    );
    assert!(invalid(s.git.branch_create(&repo, "x", Some("nope")).await).contains("no commit"));

    s.git
        .branch_rename(&repo, "spike/quic", "spike/http3")
        .await
        .unwrap();
    assert!(
        invalid(s.git.branch_rename(&repo, "spike/quic", "y").await).contains("no branch called")
    );
    assert!(invalid(
        s.git
            .branch_rename(&repo, "from-start", "spike/http3")
            .await
    )
    .contains("already a branch"));
    // Undo gives the old name back.
    s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert!(s
        .git
        .branch_list(&repo)
        .await
        .unwrap()
        .local
        .iter()
        .any(|b| b.name == "spike/quic"));

    // Deleting: safe when nothing is lost.
    let check = s
        .git
        .branch_delete_check(&repo, "from-start")
        .await
        .unwrap();
    assert!(check.safe && !check.current && check.unmerged == 0);
    s.git
        .branch_delete(&repo, "from-start", false)
        .await
        .unwrap();
    // A branch with commits nothing else has is refused unless forced.
    s.g(&repo, &["switch", "--quiet", "spike/quic"]).await;
    s.g(
        &repo,
        &["branch", "--quiet", "--set-upstream-to=origin/main"],
    )
    .await;
    let tip = s.commit(&repo, "quic.txt", "q\n", "QUIC spike").await;
    assert!(invalid(s.git.branch_delete(&repo, "spike/quic", true).await).contains("You are on"));
    s.g(&repo, &["switch", "--quiet", "main"]).await;
    let check = s
        .git
        .branch_delete_check(&repo, "spike/quic")
        .await
        .unwrap();
    assert!(!check.safe && check.unmerged == 1);
    assert!(
        invalid(s.git.branch_delete(&repo, "spike/quic", false).await)
            .contains("1 commit that is nowhere else")
    );
    s.git
        .branch_delete(&repo, "spike/quic", true)
        .await
        .unwrap();
    assert!(
        invalid(s.git.branch_delete(&repo, "spike/quic", true).await).contains("no branch called")
    );
    // It comes back from Recent actions: same commit, same upstream.
    let journal = s.git.journal_list(&repo).await.unwrap();
    assert_eq!(journal[0].title, "Deleted branch spike/quic");
    let undone = s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert!(undone.done);
    assert_eq!(s.g(&repo, &["rev-parse", "spike/quic"]).await, tip);
    let list = s.git.branch_list(&repo).await.unwrap();
    let back = list.local.iter().find(|b| b.name == "spike/quic").unwrap();
    assert_eq!(back.upstream.as_deref(), Some("origin/main"));
    assert_eq!(
        list.current.as_deref(),
        Some("main"),
        "undo never moved HEAD"
    );
}

#[tokio::test]
async fn switching_with_uncommitted_changes_asks_first() {
    let s = Sandbox::new();
    let (repo, bare) = s.repo_with_remote("r").await;
    s.commit(&repo, "shared.txt", "one\ntwo\nthree\n", "Shared")
        .await;
    s.g(&repo, &["push", "--quiet"]).await;
    s.g(&repo, &["switch", "--quiet", "-c", "feature"]).await;
    s.commit(
        &repo,
        "shared.txt",
        "one\ntwo on feature\nthree\n",
        "Feature edit",
    )
    .await;
    s.g(&repo, &["switch", "--quiet", "main"]).await;

    // Clean: it just switches.
    let out = s
        .git
        .branch_switch(&repo, "feature", SwitchHow::Ask)
        .await
        .unwrap();
    assert!(out.switched && !out.needs_choice && out.journal.is_some());
    s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert_eq!(
        s.g(&repo, &["symbolic-ref", "--short", "HEAD"]).await,
        "main"
    );

    // Changes that don't collide: still a question, nothing happens yet.
    write(&repo, "README.md", "hello\nmine\n");
    write(&repo, "new.txt", "untracked\n");
    let out = s
        .git
        .branch_switch(&repo, "feature", SwitchHow::Ask)
        .await
        .unwrap();
    assert!(!out.switched && out.needs_choice);
    assert_eq!(out.changes, 2);
    assert_eq!(
        s.g(&repo, &["symbolic-ref", "--short", "HEAD"]).await,
        "main"
    );
    // Bring them along.
    let out = s
        .git
        .branch_switch(&repo, "feature", SwitchHow::Carry)
        .await
        .unwrap();
    assert!(out.switched && out.conflicts.is_empty());
    assert_eq!(read(&repo, "README.md"), "hello\nmine\n");
    assert_eq!(s.status(&repo).await, " M README.md\n?? new.txt\n");
    // Undo: back on main, with the changes.
    s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert_eq!(
        s.g(&repo, &["symbolic-ref", "--short", "HEAD"]).await,
        "main"
    );
    assert_eq!(s.status(&repo).await, " M README.md\n?? new.txt\n");

    // Put them aside first.
    let out = s
        .git
        .branch_switch(&repo, "feature", SwitchHow::PutAside)
        .await
        .unwrap();
    assert!(out.switched);
    assert_eq!(
        out.put_aside.as_deref(),
        Some("Before switching to feature")
    );
    assert_eq!(s.status(&repo).await, "");
    let aside = s.git.stash_list(&repo).await.unwrap();
    assert_eq!(aside[0].message, "Before switching to feature");
    assert_eq!(
        (aside[0].branch.as_deref(), aside[0].files),
        (Some("main"), 2)
    );

    // Changes that collide with the other branch: carried with a conflict,
    // resolved like any other.
    s.g(&repo, &["switch", "--quiet", "main"]).await;
    write(&repo, "shared.txt", "one\ntwo uncommitted\nthree\n");
    let out = s
        .git
        .branch_switch(&repo, "feature", SwitchHow::Carry)
        .await
        .unwrap();
    assert!(out.switched);
    assert_eq!(out.conflicts, ["shared.txt"]);
    let state = s.git.operation_state(&repo).await.unwrap();
    assert_eq!(state.kind, OperationKind::StashApply);
    assert_eq!(state.cause.as_deref(), Some("switch"));
    assert_eq!(state.title, "Bringing your changes along to feature");
    // Stop and go back: on main again with the edit untouched.
    s.git.operation_abort(&repo).await.unwrap();
    assert_eq!(
        s.g(&repo, &["symbolic-ref", "--short", "HEAD"]).await,
        "main"
    );
    assert_eq!(read(&repo, "shared.txt"), "one\ntwo uncommitted\nthree\n");
    assert_eq!(s.status(&repo).await, " M shared.txt\n");
    // Again, and finish it.
    s.git
        .branch_switch(&repo, "feature", SwitchHow::Carry)
        .await
        .unwrap();
    s.git
        .conflict_take(&repo, "shared.txt", TakeSide::Theirs)
        .await
        .unwrap();
    let done = s.git.operation_continue(&repo).await.unwrap();
    assert!(done.done);
    assert_eq!(done.summary, "Switched, with your changes");
    assert_eq!(
        s.g(&repo, &["symbolic-ref", "--short", "HEAD"]).await,
        "feature"
    );
    assert_eq!(s.status(&repo).await, " M shared.txt\n");
    s.g(&repo, &["checkout", "--", "shared.txt"]).await;

    // A branch that only exists on the remote gets a follower.
    let theirs = s.clone_of(&bare, "theirs").await;
    s.g(&theirs, &["switch", "--quiet", "-c", "from/elsewhere"])
        .await;
    s.commit(&theirs, "e.txt", "e\n", "Elsewhere").await;
    s.g(
        &theirs,
        &["push", "--quiet", "-u", "origin", "from/elsewhere"],
    )
    .await;
    s.git.fetch_remotes(&repo, None, false).await.unwrap();
    let out = s
        .git
        .branch_switch(&repo, "origin/from/elsewhere", SwitchHow::Ask)
        .await
        .unwrap();
    assert_eq!(out.branch, "from/elsewhere");
    let list = s.git.branch_list(&repo).await.unwrap();
    assert_eq!(
        list.local[0].upstream.as_deref(),
        Some("origin/from/elsewhere")
    );
    // Undoing that takes the new branch away again.
    s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert!(!s
        .git
        .branch_list(&repo)
        .await
        .unwrap()
        .local
        .iter()
        .any(|b| b.name == "from/elsewhere"));
    assert!(
        invalid(s.git.branch_switch(&repo, "nope", SwitchHow::Ask).await)
            .contains("no branch called")
    );
    // Already there.
    let out = s
        .git
        .branch_switch(&repo, "feature", SwitchHow::Ask)
        .await
        .unwrap();
    assert!(out.switched && out.journal.is_none());
}

#[tokio::test]
async fn pull_push_and_the_careful_force_push() {
    let s = Sandbox::new();
    let (repo, bare) = s.repo_with_remote("r").await;
    let theirs = s.clone_of(&bare, "theirs").await;

    // Nothing to pull.
    let out = s.git.pull(&repo, PullMode::Merge).await.unwrap();
    assert_eq!(
        (out.done, out.summary.as_str(), out.journal),
        (true, "Already up to date.", None)
    );
    // They push two commits; a fast-forward brings them in.
    s.commit(&theirs, "one.txt", "1\n", "Theirs one").await;
    s.commit(&theirs, "README.md", "hello\ntheirs\n", "Theirs readme")
        .await;
    s.g(&theirs, &["push", "--quiet"]).await;
    let out = s.git.pull(&repo, PullMode::FfOnly).await.unwrap();
    assert_eq!(out.summary, "Pulled 2 commits into main");
    assert_eq!(
        s.git.journal_list(&repo).await.unwrap()[0].title,
        "Pulled 2 commits into main"
    );
    s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert_eq!(s.subjects(&repo, "HEAD").await, ["Start"]);

    // Both moved on.
    s.commit(&repo, "mine.txt", "m\n", "Mine").await;
    assert!(invalid(s.git.pull(&repo, PullMode::FfOnly).await).contains("both moved on"));
    // A push is refused while the remote is ahead.
    assert!(invalid(s.git.push_branch(&repo, None, None).await).contains("Pull first"));
    // Rebase: mine goes on top.
    let out = s.git.pull(&repo, PullMode::Rebase).await.unwrap();
    assert!(out.done);
    assert_eq!(
        s.subjects(&repo, "HEAD").await,
        ["Mine", "Theirs readme", "Theirs one", "Start"]
    );
    s.git.undo_last(&repo, Default::default()).await.unwrap();
    // Merge: a merge commit.
    let out = s.git.pull(&repo, PullMode::Merge).await.unwrap();
    assert!(out.done);
    assert_eq!(
        s.g(&repo, &["rev-list", "--count", "--merges", "HEAD"])
            .await,
        "1"
    );
    let pushed = s.git.push_branch(&repo, None, None).await.unwrap();
    assert_eq!(
        (
            pushed.branch.as_str(),
            pushed.remote.as_str(),
            pushed.forced
        ),
        ("main", "origin", false)
    );
    assert_eq!(
        s.g(&bare, &["rev-parse", "main"]).await,
        s.head(&repo).await
    );

    // A pull that conflicts is resolved like any merge.
    s.g(&theirs, &["pull", "--quiet"]).await;
    s.commit(
        &theirs,
        "README.md",
        "hello\ntheirs again\n",
        "Theirs again",
    )
    .await;
    s.g(&theirs, &["push", "--quiet"]).await;
    s.commit(&repo, "README.md", "hello\nmine\n", "Mine readme")
        .await;
    let out = s.git.pull(&repo, PullMode::Merge).await.unwrap();
    assert!(!out.done);
    assert_eq!(out.conflicts, ["README.md"]);
    assert_eq!(out.state.cause.as_deref(), Some("pull"));
    assert_eq!(out.state.title, "Pulling origin/main into main");
    s.git
        .conflict_take(&repo, "README.md", TakeSide::Yours)
        .await
        .unwrap();
    let done = s.git.operation_continue(&repo).await.unwrap();
    assert!(done.done);
    assert!(done.summary.starts_with("Pulled"), "{}", done.summary);
    s.git.push_branch(&repo, None, None).await.unwrap();

    // A new branch: pushed to the only remote, and follows it afterwards.
    s.g(&repo, &["switch", "--quiet", "-c", "feature/x"]).await;
    s.commit(&repo, "x.txt", "1\n", "X one").await;
    assert!(invalid(s.git.pull(&repo, PullMode::Merge).await).contains("doesn’t follow"));
    s.git.push_branch(&repo, None, None).await.unwrap();
    assert_eq!(
        s.g(&repo, &["rev-parse", "--abbrev-ref", "@{upstream}"])
            .await,
        "origin/feature/x"
    );
    // Rewrite it here, while someone else pushes to it there.
    let pushed_tip = s.head(&repo).await;
    s.g(
        &repo,
        &["commit", "--quiet", "--amend", "-m", "X one, reworded"],
    )
    .await;
    s.commit(&repo, "x.txt", "1\n2\n", "X two").await;
    let preview = s.git.force_push_preview(&repo, None, None).await.unwrap();
    assert_eq!((preview.replaced, preview.new), (1, 2));
    assert_eq!(preview.expected.as_deref(), Some(pushed_tip.as_str()));
    assert_eq!(
        (preview.remote.as_str(), preview.remote_branch.as_str()),
        ("origin", "feature/x")
    );
    s.g(&theirs, &["fetch", "--quiet"]).await;
    s.g(&theirs, &["switch", "--quiet", "feature/x"]).await;
    let sneaked = s
        .commit(&theirs, "sneak.txt", "s\n", "Pushed in between")
        .await;
    s.g(&theirs, &["push", "--quiet"]).await;
    // The lease is the commit the person was shown: it no longer holds.
    assert!(invalid(
        s.git
            .push_force_with_lease(&repo, None, None, &pushed_tip)
            .await
    )
    .contains("in the meantime"));
    assert_eq!(s.g(&bare, &["rev-parse", "feature/x"]).await, sneaked);
    assert!(invalid(
        s.git
            .push_force_with_lease(&repo, None, None, "abc123")
            .await
    )
    .contains("full id"));
    // Fetch, look again, and then it goes through.
    s.git.fetch_remotes(&repo, None, false).await.unwrap();
    let preview = s.git.force_push_preview(&repo, None, None).await.unwrap();
    assert_eq!((preview.replaced, preview.new), (2, 2));
    let pushed = s
        .git
        .push_force_with_lease(&repo, None, None, preview.expected.as_deref().unwrap())
        .await
        .unwrap();
    assert!(pushed.forced);
    assert_eq!(
        s.g(&bare, &["rev-parse", "feature/x"]).await,
        s.head(&repo).await
    );
}

#[tokio::test]
async fn tags_and_remotes() {
    let s = Sandbox::new();
    let (repo, bare) = s.repo_with_remote("r").await;
    let first = s.head(&repo).await;
    s.commit(&repo, "b.txt", "b\n", "Second").await;

    s.git
        .tag_create(&repo, "v0.8", Some(&first), None)
        .await
        .unwrap();
    s.git
        .tag_create(&repo, "v0.9", None, Some("Remote access\n\nWith passkeys."))
        .await
        .unwrap();
    let tags = s.git.tag_list(&repo).await.unwrap();
    let flat: Vec<(&str, bool, &str)> = tags
        .iter()
        .map(|t| (t.name.as_str(), t.annotated, t.subject.as_str()))
        .collect();
    assert!(flat.contains(&("v0.8", false, "Start")));
    assert!(flat.contains(&("v0.9", true, "Remote access")));
    let annotated = tags.iter().find(|t| t.name == "v0.9").unwrap();
    assert_eq!(
        annotated.sha,
        s.head(&repo).await,
        "an annotated tag names its commit"
    );
    assert_eq!(tags.iter().find(|t| t.name == "v0.8").unwrap().sha, first);
    assert!(invalid(s.git.tag_create(&repo, "v0.9", None, None).await).contains("already a tag"));
    assert!(
        invalid(s.git.tag_create(&repo, "bad tag", None, None).await)
            .contains("can’t be a tag name")
    );

    s.git.tag_push(&repo, "v0.9", "origin").await.unwrap();
    assert_eq!(s.g(&bare, &["tag", "--list"]).await, "v0.9");
    assert!(invalid(s.git.tag_push(&repo, "v0.9", "nope").await).contains("no remote called"));
    s.git.tag_delete(&repo, "v0.9").await.unwrap();
    assert!(invalid(s.git.tag_delete(&repo, "v0.9").await).contains("no tag called"));
    assert_eq!(
        s.g(&bare, &["tag", "--list"]).await,
        "v0.9",
        "the remote keeps its tag"
    );
    // A deleted tag comes back, annotation and all.
    s.git.undo_last(&repo, Default::default()).await.unwrap();
    let tags = s.git.tag_list(&repo).await.unwrap();
    assert!(tags.iter().any(|t| t.name == "v0.9" && t.annotated));

    let remotes = s.git.remote_list(&repo).await.unwrap();
    assert_eq!(remotes.len(), 1);
    assert_eq!(
        (remotes[0].name.as_str(), remotes[0].push_url.as_deref()),
        ("origin", None)
    );
    s.git
        .remote_add(&repo, "github", "git@github.com:DemiInfinity/Atelier.git")
        .await
        .unwrap();
    assert!(invalid(
        s.git
            .remote_add(&repo, "github", "https://x.test/a.git")
            .await
    )
    .contains("already a remote"));
    assert!(invalid(
        s.git
            .remote_add(&repo, "evil", "ext::sh -c 'touch /tmp/x'")
            .await
    )
    .contains("isn’t an address"));
    assert!(invalid(
        s.git
            .remote_add(&repo, "bad name", "https://x.test/a.git")
            .await
    )
    .contains("can’t be a remote name"));
    s.git.remote_rename(&repo, "github", "gh").await.unwrap();
    assert!(invalid(s.git.remote_rename(&repo, "gh", "origin").await).contains("already a remote"));
    s.git
        .remote_set_url(&repo, "gh", "https://github.com/DemiInfinity/Atelier.git")
        .await
        .unwrap();
    assert!(
        invalid(s.git.remote_set_url(&repo, "gh", "--upload-pack=x").await)
            .contains("isn’t an address")
    );
    let remotes = s.git.remote_list(&repo).await.unwrap();
    assert_eq!(remotes[0].name, "gh");
    assert_eq!(
        remotes[0].url,
        "https://github.com/DemiInfinity/Atelier.git"
    );
    s.git.remote_remove(&repo, "gh").await.unwrap();
    assert!(invalid(s.git.remote_remove(&repo, "gh").await).contains("no remote called"));
    assert_eq!(s.git.remote_list(&repo).await.unwrap().len(), 1);
    assert!(!s.status(&repo).await.contains("x"), "nothing ran");
}
