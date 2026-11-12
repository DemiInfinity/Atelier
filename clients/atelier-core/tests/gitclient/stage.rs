use atelier_core::diff::FileStatus;
use atelier_core::git::stage::{ChunkLineKind, LineRef, Selection, Side};
use atelier_core::git::ChangeKind;

use crate::kit::*;

fn lines(refs: &[(usize, usize)]) -> Selection {
    Selection::Lines {
        lines: refs
            .iter()
            .map(|&(chunk, line)| LineRef { chunk, line })
            .collect(),
    }
}

fn chunks(which: &[usize]) -> Selection {
    Selection::Chunks {
        chunks: which.to_vec(),
    }
}

#[tokio::test]
async fn status_is_split_into_staged_and_not() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    s.commit(&repo, "staged.txt", "a\n", "one").await;
    s.commit(&repo, "both.txt", "a\nb\nc\n", "two").await;
    s.commit(&repo, "old name.txt", "same\ncontent\nhere\n", "three")
        .await;
    s.commit(&repo, "gone.txt", "bye\n", "four").await;
    std::fs::write(repo.join("pic.bin"), [0u8, 1, 2, 3]).unwrap();
    s.g(&repo, &["add", "pic.bin"]).await;
    s.g(&repo, &["commit", "--quiet", "-m", "five"]).await;

    write(&repo, "staged.txt", "a\nmore\n");
    s.g(&repo, &["add", "staged.txt"]).await;
    write(&repo, "both.txt", "A\nb\nc\n");
    s.g(&repo, &["add", "both.txt"]).await;
    write(&repo, "both.txt", "A\nb\nC\nd\n");
    s.g(&repo, &["mv", "old name.txt", "new name.txt"]).await;
    std::fs::remove_file(repo.join("gone.txt")).unwrap();
    write(&repo, "fresh/new file.txt", "x\ny\n");
    std::fs::write(repo.join("pic.bin"), [0u8, 9, 9]).unwrap();

    let status = s.git.stage_status(&repo).await.unwrap();
    assert_eq!(status.branch.as_deref(), Some("main"));
    assert_eq!(status.head.as_ref().unwrap().subject, "five");
    assert!(!status.head.as_ref().unwrap().pushed);
    let staged: Vec<(&str, ChangeKind)> = status
        .staged
        .iter()
        .map(|e| (e.path.as_str(), e.kind))
        .collect();
    assert_eq!(
        staged,
        [
            ("both.txt", ChangeKind::Modified),
            ("new name.txt", ChangeKind::Renamed),
            ("staged.txt", ChangeKind::Modified),
        ]
    );
    assert_eq!(status.staged[1].old_path.as_deref(), Some("old name.txt"));
    assert_eq!(
        (status.staged[2].additions, status.staged[2].deletions),
        (Some(1), Some(0))
    );
    let unstaged: Vec<(&str, ChangeKind)> = status
        .unstaged
        .iter()
        .map(|e| (e.path.as_str(), e.kind))
        .collect();
    assert_eq!(
        unstaged,
        [
            ("both.txt", ChangeKind::Modified),
            ("fresh/new file.txt", ChangeKind::Untracked),
            ("gone.txt", ChangeKind::Deleted),
            ("pic.bin", ChangeKind::Modified),
        ]
    );
    assert_eq!(status.unstaged[1].additions, Some(2));
    assert!(status.unstaged[3].binary);
    assert_eq!(status.unstaged[3].additions, None);
    assert!(status.conflicted.is_empty());

    // The same file has a different diff on each side.
    let staged = s
        .git
        .stage_diff(&repo, "both.txt", Side::Staged)
        .await
        .unwrap();
    assert_eq!((staged.additions, staged.deletions), (1, 1));
    let unstaged = s
        .git
        .stage_diff(&repo, "both.txt", Side::Unstaged)
        .await
        .unwrap();
    assert_eq!((unstaged.additions, unstaged.deletions), (2, 1));
    assert_ne!(staged.token, unstaged.token);
    let renamed = s
        .git
        .stage_diff(&repo, "new name.txt", Side::Staged)
        .await
        .unwrap();
    assert_eq!(renamed.status, FileStatus::Renamed);
    assert!(renamed.chunks.is_empty());
    let binary = s
        .git
        .stage_diff(&repo, "pic.bin", Side::Unstaged)
        .await
        .unwrap();
    assert!(binary.binary && binary.chunks.is_empty());
    assert!(
        invalid(s.git.stage_diff(&repo, "staged.txt", Side::Unstaged).await)
            .contains("no changes that aren’t staged")
    );
    assert!(invalid(s.git.stage_diff(&repo, "../x", Side::Staged).await).contains("isn’t a file"));
}

#[tokio::test]
async fn chunks_and_lines_are_staged_unstaged_and_discarded() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    s.commit(&repo, "f.txt", &twenty(), "twenty").await;
    let changed = twenty()
        .replace("line 2\n", "line two\nline two and a half\n")
        .replace("line 18\n", "line eighteen\n")
        .replace("line 19\n", "");
    write(&repo, "f.txt", &changed);

    let diff = s
        .git
        .stage_diff(&repo, "f.txt", Side::Unstaged)
        .await
        .unwrap();
    assert_eq!(diff.chunks.len(), 2);
    assert_eq!((diff.additions, diff.deletions), (3, 3));
    let first = &diff.chunks[0];
    assert_eq!((first.old_start, first.new_start), (1, 1));
    assert_eq!(first.lines[1].kind, ChunkLineKind::Delete);
    assert_eq!(first.lines[1].old, Some(2));
    assert_eq!(first.lines[2].kind, ChunkLineKind::Add);
    assert_eq!(first.lines[2].new, Some(2));
    assert_eq!(first.lines[2].text, "line two");

    // One whole chunk.
    s.git
        .stage(&repo, "f.txt", &chunks(&[1]), Some(&diff.token))
        .await
        .unwrap();
    let staged = s.staged(&repo, "f.txt").await;
    assert!(staged.contains("line 2\n") && staged.contains("line eighteen\n"));
    assert!(!staged.contains("line 19\n"));

    // The token is for the diff as it was: the file has moved on.
    assert!(invalid(
        s.git
            .stage(&repo, "f.txt", &lines(&[(0, 2)]), Some(&diff.token))
            .await
    )
    .contains("changed since you looked"));

    // One added line of the other chunk, leaving its neighbours.
    let diff = s
        .git
        .stage_diff(&repo, "f.txt", Side::Unstaged)
        .await
        .unwrap();
    assert_eq!(diff.chunks.len(), 1);
    s.git
        .stage(&repo, "f.txt", &lines(&[(0, 3)]), Some(&diff.token))
        .await
        .unwrap();
    let staged = s.staged(&repo, "f.txt").await;
    assert!(staged.starts_with("line 1\nline 2\nline two and a half\nline 3\n"));
    // The working tree was never touched.
    assert_eq!(read(&repo, "f.txt"), changed);

    // Take the first staged chunk's line back out of the index.
    let staged_diff = s
        .git
        .stage_diff(&repo, "f.txt", Side::Staged)
        .await
        .unwrap();
    assert_eq!(staged_diff.chunks.len(), 2);
    s.git
        .unstage(&repo, "f.txt", &lines(&[(0, 2)]), Some(&staged_diff.token))
        .await
        .unwrap();
    let staged = s.staged(&repo, "f.txt").await;
    assert!(staged.starts_with("line 1\nline 2\nline 3\n"));
    assert!(staged.contains("line eighteen\n"));
    // And a whole chunk.
    let staged_diff = s
        .git
        .stage_diff(&repo, "f.txt", Side::Staged)
        .await
        .unwrap();
    s.git
        .unstage(&repo, "f.txt", &chunks(&[0]), Some(&staged_diff.token))
        .await
        .unwrap();
    assert_eq!(s.staged(&repo, "f.txt").await, twenty());
    assert!(
        invalid(s.git.unstage(&repo, "f.txt", &Selection::File, None).await)
            .contains("nothing staged")
    );

    // Discard: the removed "line 19" comes back, the rest stays changed.
    let diff = s
        .git
        .stage_diff(&repo, "f.txt", Side::Unstaged)
        .await
        .unwrap();
    let last = &diff.chunks[1];
    let gone = last
        .lines
        .iter()
        .find(|l| l.kind == ChunkLineKind::Delete && l.text == "line 19")
        .unwrap()
        .index;
    let journal = s
        .git
        .discard(&repo, "f.txt", &lines(&[(1, gone)]), Some(&diff.token))
        .await
        .unwrap()
        .expect("something was discarded");
    let now = read(&repo, "f.txt");
    // A removed line comes back where the diff had it: before the added one.
    assert!(now.contains("line 19\nline eighteen\nline 20\n"));
    assert!(now.contains("line two\n"));
    // It is kept, and comes back.
    let kept = s.git.discarded_list(&repo).await.unwrap();
    assert_eq!(kept.len(), 1);
    assert_eq!(kept[0].id, journal);
    assert_eq!(kept[0].title, "Discarded a line of f.txt");
    let d = kept[0].discarded.as_ref().unwrap();
    assert_eq!((d.files, d.additions, d.deletions), (1, 0, 1));
    s.git
        .restore_discarded(&repo, journal, false)
        .await
        .unwrap();
    assert_eq!(read(&repo, "f.txt"), changed);
    assert!(s.git.discarded_list(&repo).await.unwrap().is_empty());

    // A whole chunk, then the whole file.
    let diff = s
        .git
        .stage_diff(&repo, "f.txt", Side::Unstaged)
        .await
        .unwrap();
    s.git
        .discard(&repo, "f.txt", &chunks(&[0]), Some(&diff.token))
        .await
        .unwrap();
    assert!(read(&repo, "f.txt").starts_with("line 1\nline 2\nline 3\n"));
    s.git
        .discard(&repo, "f.txt", &Selection::File, None)
        .await
        .unwrap();
    assert_eq!(read(&repo, "f.txt"), twenty());
    assert_eq!(s.status(&repo).await, "");
    assert_eq!(s.git.discarded_list(&repo).await.unwrap().len(), 2);
    // Nothing chosen, or a context line chosen: refused.
    write(&repo, "f.txt", &changed);
    assert!(
        invalid(s.git.stage(&repo, "f.txt", &chunks(&[]), None).await).contains("at least one")
    );
    assert!(
        invalid(s.git.stage(&repo, "f.txt", &lines(&[(0, 0)]), None).await)
            .contains("changed since")
    );
    assert!(
        invalid(s.git.stage(&repo, "f.txt", &chunks(&[7]), None).await).contains("changed since")
    );
}

#[tokio::test]
async fn crlf_and_a_missing_final_newline_survive() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    s.commit(&repo, "w.txt", "one\r\ntwo\r\nthree\r\nfour", "windows")
        .await;
    write(&repo, "w.txt", "one\r\nTWO\r\nthree\r\nfour\r\nfive");
    let diff = s
        .git
        .stage_diff(&repo, "w.txt", Side::Unstaged)
        .await
        .unwrap();
    let chunk = &diff.chunks[0];
    assert_eq!(
        chunk.lines[1].text, "two",
        "the line end isn't part of the text"
    );
    assert!(chunk.lines.iter().any(|l| l.no_newline));
    // Only the first change (two → TWO).
    let pick: Vec<(usize, usize)> = chunk
        .lines
        .iter()
        .filter(|l| l.text.eq_ignore_ascii_case("two") && l.kind != ChunkLineKind::Context)
        .map(|l| (0, l.index))
        .collect();
    assert_eq!(pick.len(), 2);
    s.git
        .stage(&repo, "w.txt", &lines(&pick), Some(&diff.token))
        .await
        .unwrap();
    assert_eq!(
        s.staged(&repo, "w.txt").await,
        "one\r\nTWO\r\nthree\r\nfour"
    );
    // The rest of the chunk: the file's end changes too.
    let diff = s
        .git
        .stage_diff(&repo, "w.txt", Side::Unstaged)
        .await
        .unwrap();
    s.git
        .stage(&repo, "w.txt", &chunks(&[0]), Some(&diff.token))
        .await
        .unwrap();
    assert_eq!(
        s.staged(&repo, "w.txt").await,
        "one\r\nTWO\r\nthree\r\nfour\r\nfive"
    );
    // And back out again, line by line.
    let diff = s
        .git
        .stage_diff(&repo, "w.txt", Side::Staged)
        .await
        .unwrap();
    s.git
        .unstage(&repo, "w.txt", &chunks(&[0]), Some(&diff.token))
        .await
        .unwrap();
    assert_eq!(
        s.staged(&repo, "w.txt").await,
        "one\r\ntwo\r\nthree\r\nfour"
    );
    // Discarding the end of the file puts the missing newline back as missing.
    let diff = s
        .git
        .stage_diff(&repo, "w.txt", Side::Unstaged)
        .await
        .unwrap();
    let tail: Vec<(usize, usize)> = diff.chunks[0]
        .lines
        .iter()
        .filter(|l| l.kind != ChunkLineKind::Context && l.text != "two" && l.text != "TWO")
        .map(|l| (0, l.index))
        .collect();
    s.git
        .discard(&repo, "w.txt", &lines(&tail), Some(&diff.token))
        .await
        .unwrap();
    assert_eq!(read(&repo, "w.txt"), "one\r\nTWO\r\nthree\r\nfour");
}

#[tokio::test]
async fn new_deleted_renamed_and_binary_files() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    s.commit(&repo, "keep.txt", "k\n", "start").await;
    s.commit(&repo, "doomed.txt", "a\nb\nc\nd\n", "doomed")
        .await;
    s.commit(
        &repo,
        "moved.txt",
        "one\ntwo\nthree\nfour\nfive\nsix\n",
        "moved",
    )
    .await;

    // A new file: two of its four lines go into the index.
    write(&repo, "dir/new file.txt", "n1\nn2\nn3\nn4\n");
    let diff = s
        .git
        .stage_diff(&repo, "dir/new file.txt", Side::Unstaged)
        .await
        .unwrap();
    assert_eq!(diff.status, FileStatus::Added);
    assert_eq!(diff.kind, ChangeKind::Untracked);
    assert_eq!(diff.chunks[0].lines.len(), 4);
    s.git
        .stage(
            &repo,
            "dir/new file.txt",
            &lines(&[(0, 1), (0, 2)]),
            Some(&diff.token),
        )
        .await
        .unwrap();
    assert_eq!(s.staged(&repo, "dir/new file.txt").await, "n2\nn3\n");
    assert_eq!(read(&repo, "dir/new file.txt"), "n1\nn2\nn3\nn4\n");
    // One of them back out: the file stays in the index with the other.
    let staged = s
        .git
        .stage_diff(&repo, "dir/new file.txt", Side::Staged)
        .await
        .unwrap();
    s.git
        .unstage(
            &repo,
            "dir/new file.txt",
            &lines(&[(0, 0)]),
            Some(&staged.token),
        )
        .await
        .unwrap();
    assert_eq!(s.staged(&repo, "dir/new file.txt").await, "n3\n");
    // The whole file out: git doesn't know it any more.
    s.git
        .unstage(&repo, "dir/new file.txt", &Selection::File, None)
        .await
        .unwrap();
    assert!(s.status(&repo).await.contains("?? dir/"));
    // Discarding lines of a file git doesn't know edits it; discarding it
    // whole deletes it, and it can be brought back.
    let diff = s
        .git
        .stage_diff(&repo, "dir/new file.txt", Side::Unstaged)
        .await
        .unwrap();
    s.git
        .discard(
            &repo,
            "dir/new file.txt",
            &lines(&[(0, 0), (0, 3)]),
            Some(&diff.token),
        )
        .await
        .unwrap();
    assert_eq!(read(&repo, "dir/new file.txt"), "n2\nn3\n");
    let id = s
        .git
        .discard(&repo, "dir/new file.txt", &Selection::File, None)
        .await
        .unwrap()
        .unwrap();
    assert!(!repo.join("dir/new file.txt").exists());
    s.git.restore_discarded(&repo, id, false).await.unwrap();
    assert_eq!(read(&repo, "dir/new file.txt"), "n2\nn3\n");
    std::fs::remove_dir_all(repo.join("dir")).unwrap();

    // A deleted file: staging half the deletion leaves the other half.
    std::fs::remove_file(repo.join("doomed.txt")).unwrap();
    let diff = s
        .git
        .stage_diff(&repo, "doomed.txt", Side::Unstaged)
        .await
        .unwrap();
    assert_eq!(diff.status, FileStatus::Deleted);
    s.git
        .stage(
            &repo,
            "doomed.txt",
            &lines(&[(0, 0), (0, 1)]),
            Some(&diff.token),
        )
        .await
        .unwrap();
    assert_eq!(s.staged(&repo, "doomed.txt").await, "c\nd\n");
    // Discarding part of the rest brings those lines back as a file.
    let diff = s
        .git
        .stage_diff(&repo, "doomed.txt", Side::Unstaged)
        .await
        .unwrap();
    s.git
        .discard(&repo, "doomed.txt", &lines(&[(0, 1)]), Some(&diff.token))
        .await
        .unwrap();
    assert_eq!(read(&repo, "doomed.txt"), "d\n");
    std::fs::remove_file(repo.join("doomed.txt")).unwrap();
    s.git
        .stage(&repo, "doomed.txt", &Selection::File, None)
        .await
        .unwrap();
    assert!(s.status(&repo).await.contains("D  doomed.txt"));
    s.git
        .unstage(&repo, "doomed.txt", &Selection::File, None)
        .await
        .unwrap();
    s.git
        .discard(&repo, "doomed.txt", &Selection::File, None)
        .await
        .unwrap();
    assert_eq!(read(&repo, "doomed.txt"), "a\nb\nc\nd\n");

    // A renamed file with an edit: unstaging the edited line keeps the rename.
    s.g(&repo, &["mv", "moved.txt", "renamed.txt"]).await;
    write(&repo, "renamed.txt", "one\nTWO\nthree\nfour\nfive\nsix\n");
    s.g(&repo, &["add", "renamed.txt"]).await;
    let diff = s
        .git
        .stage_diff(&repo, "renamed.txt", Side::Staged)
        .await
        .unwrap();
    assert_eq!(diff.status, FileStatus::Renamed);
    assert_eq!(diff.old_path.as_deref(), Some("moved.txt"));
    assert_eq!((diff.additions, diff.deletions), (1, 1));
    s.git
        .unstage(&repo, "renamed.txt", &chunks(&[0]), Some(&diff.token))
        .await
        .unwrap();
    assert_eq!(
        s.staged(&repo, "renamed.txt").await,
        "one\ntwo\nthree\nfour\nfive\nsix\n"
    );
    assert!(s
        .status(&repo)
        .await
        .contains("RM moved.txt -> renamed.txt"));
    // Unstaging the file undoes the rename in the index, both halves.
    s.git
        .unstage(&repo, "renamed.txt", &Selection::File, None)
        .await
        .unwrap();
    let status = s.status(&repo).await;
    assert!(status.contains(" D moved.txt") && status.contains("?? renamed.txt"));
    s.g(&repo, &["add", "-A"]).await;
    s.g(&repo, &["commit", "--quiet", "-m", "rename"]).await;

    // A binary file is all or nothing.
    std::fs::write(repo.join("pic.bin"), [0u8, 1, 2, 3]).unwrap();
    assert!(
        invalid(s.git.stage(&repo, "pic.bin", &chunks(&[0]), None).await).contains("binary file")
    );
    s.git
        .stage(&repo, "pic.bin", &Selection::File, None)
        .await
        .unwrap();
    s.g(&repo, &["commit", "--quiet", "-m", "pic"]).await;
    std::fs::write(repo.join("pic.bin"), [0u8, 7, 7, 7, 7]).unwrap();
    assert!(
        invalid(s.git.discard(&repo, "pic.bin", &chunks(&[0]), None).await).contains("binary file")
    );
    let id = s
        .git
        .discard(&repo, "pic.bin", &Selection::File, None)
        .await
        .unwrap()
        .unwrap();
    assert_eq!(std::fs::read(repo.join("pic.bin")).unwrap(), [0u8, 1, 2, 3]);
    s.git.restore_discarded(&repo, id, false).await.unwrap();
    assert_eq!(
        std::fs::read(repo.join("pic.bin")).unwrap(),
        [0u8, 7, 7, 7, 7]
    );
}

#[tokio::test]
async fn stage_all_unstage_all_and_several_files_discarded_at_once() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    s.commit(&repo, "a.txt", "a\n", "a").await;
    s.commit(&repo, "b.txt", "b\n", "b").await;
    write(&repo, "a.txt", "A\n");
    write(&repo, "b.txt", "B\n");
    write(&repo, "c.txt", "c\n");
    s.git.stage_all(&repo).await.unwrap();
    let status = s.git.stage_status(&repo).await.unwrap();
    assert_eq!((status.staged.len(), status.unstaged.len()), (3, 0));
    s.git.unstage_all(&repo).await.unwrap();
    let status = s.git.stage_status(&repo).await.unwrap();
    assert_eq!((status.staged.len(), status.unstaged.len()), (0, 3));

    let id = s
        .git
        .discard_files(&repo, &["a.txt".into(), "c.txt".into(), "nope.txt".into()])
        .await
        .unwrap()
        .unwrap();
    assert_eq!(read(&repo, "a.txt"), "a\n");
    assert!(!repo.join("c.txt").exists());
    assert_eq!(read(&repo, "b.txt"), "B\n");
    let kept = s.git.discarded_list(&repo).await.unwrap();
    assert_eq!(kept[0].title, "Discarded changes in 2 files");
    assert_eq!(kept[0].discarded.as_ref().unwrap().files, 2);

    // The file changed again since: the old change no longer fits on top.
    write(&repo, "a.txt", "something else entirely\n");
    assert!(invalid(s.git.restore_discarded(&repo, id, false).await).contains("changed since"));
    assert_eq!(read(&repo, "a.txt"), "something else entirely\n");
    // Replacing is possible, and what was there is kept in turn.
    s.git.restore_discarded(&repo, id, true).await.unwrap();
    assert_eq!(read(&repo, "a.txt"), "A\n");
    assert_eq!(read(&repo, "c.txt"), "c\n");
    let kept = s.git.discarded_list(&repo).await.unwrap();
    assert_eq!(kept.len(), 1);
    assert!(kept[0].title.starts_with("Replaced 2 files"));
    assert!(invalid(s.git.restore_discarded(&repo, id, false).await).contains("already"));
    assert!(invalid(s.git.restore_discarded(&repo, 42, false).await).contains("isn’t kept"));

    // Everything at once.
    let all = s.git.discard_all(&repo).await.unwrap().unwrap();
    assert_eq!(s.status(&repo).await, "");
    assert_eq!(s.git.discard_all(&repo).await.unwrap(), None);
    s.git.restore_discarded(&repo, all, false).await.unwrap();
    assert_eq!(read(&repo, "b.txt"), "B\n");
    assert_eq!(read(&repo, "c.txt"), "c\n");
    // Nothing of the app's is in the working tree.
    let files: Vec<String> = std::fs::read_dir(&repo)
        .unwrap()
        .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
        .collect();
    assert!(files
        .iter()
        .all(|f| [".git", "a.txt", "b.txt", "c.txt"].contains(&f.as_str())));
    assert!(repo.join(".git/atelier/journal.jsonl").is_file());
}

#[tokio::test]
async fn commit_and_amend_take_what_is_staged() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    // Before the first commit: stage, unstage, commit.
    write(&repo, "a.txt", "a\n");
    write(&repo, "b.txt", "b\n");
    assert!(
        invalid(s.git.commit_staged(&repo, "First", "", false).await).contains("Stage something")
    );
    s.git.stage_all(&repo).await.unwrap();
    s.git
        .unstage(&repo, "b.txt", &Selection::File, None)
        .await
        .unwrap();
    assert!(invalid(s.git.commit_staged(&repo, "  ", "", false).await).contains("message"));
    assert!(invalid(s.git.commit_staged(&repo, "x", "", true).await).contains("no commit to amend"));
    let first = s
        .git
        .commit_staged(&repo, "First", "", false)
        .await
        .unwrap();
    assert_eq!(first.subject, "First");
    assert_eq!(s.status(&repo).await, "?? b.txt\n");

    // Only what is staged goes in; the rest stays as it is.
    write(&repo, "a.txt", "a\nstaged\n");
    s.git
        .stage(&repo, "a.txt", &Selection::File, None)
        .await
        .unwrap();
    write(&repo, "a.txt", "a\nstaged\nnot staged\n");
    let second = s
        .git
        .commit_staged(&repo, "Second", "With a description.", false)
        .await
        .unwrap();
    assert_eq!(second.sha, s.head(&repo).await);
    assert_eq!(
        s.g(&repo, &["log", "-1", "--format=%an <%ae>|%s|%b"]).await,
        "Test Person <test@example.com>|Second|With a description."
    );
    assert_eq!(s.g(&repo, &["show", "HEAD:a.txt"]).await, "a\nstaged");
    assert!(s.status(&repo).await.contains(" M a.txt"));

    // Amend: the message changes and what is staged joins the commit.
    s.git
        .stage(&repo, "b.txt", &Selection::File, None)
        .await
        .unwrap();
    let amended = s
        .git
        .commit_staged(&repo, "Second, better", "", true)
        .await
        .unwrap();
    assert!(amended.amended);
    assert_ne!(amended.sha, second.sha);
    assert_eq!(s.subjects(&repo, "HEAD").await, ["Second, better", "First"]);
    assert_eq!(s.g(&repo, &["show", "HEAD:b.txt"]).await, "b");
    // A message-only amend with nothing staged is fine.
    s.git
        .commit_staged(&repo, "Second, best", "", true)
        .await
        .unwrap();

    // Each is in Recent actions, and undo steps back through them exactly.
    let journal = s.git.journal_list(&repo).await.unwrap();
    let titles: Vec<&str> = journal.iter().map(|e| e.title.as_str()).collect();
    assert_eq!(
        titles,
        [
            "Amended “Second, best”",
            "Amended “Second, better”",
            "Committed “Second”",
            "Committed “First”",
        ]
    );
    let undone = s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert!(undone.done, "{undone:?}");
    assert_eq!(undone.title, "Undid “Amended “Second, best””");
    assert_eq!(s.subjects(&repo, "HEAD").await, ["Second, better", "First"]);
    let undone = s.git.undo_last(&repo, Default::default()).await.unwrap();
    assert!(undone.done);
    // Back to before the first amend: b.txt staged, a.txt's extra line unstaged.
    assert_eq!(s.head(&repo).await, second.sha);
    assert_eq!(s.status(&repo).await, " M a.txt\nA  b.txt\n");
    assert_eq!(read(&repo, "a.txt"), "a\nstaged\nnot staged\n");
}
