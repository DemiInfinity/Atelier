//! Working-copy helpers on throwaway repositories: changes, diffs, committing
//! exactly the chosen files, history, branches, undo. Nothing here touches the
//! real home folder, the real git config or the network.

use std::path::{Path, PathBuf};

use atelier_core::diff::{self, FileStatus, LineKind};
use atelier_core::git::{ChangeKind, RefKind, Segment};
use atelier_core::{Error, Git};

struct Sandbox {
    dir: tempfile::TempDir,
    git: Git,
}

impl Sandbox {
    fn new() -> Sandbox {
        let dir = tempfile::tempdir().unwrap();
        let home = dir.path().join("home");
        std::fs::create_dir_all(&home).unwrap();
        let git = Git::new()
            .env("HOME", &home)
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env("GIT_AUTHOR_NAME", "Test")
            .env("GIT_AUTHOR_EMAIL", "test@example.com")
            .env("GIT_COMMITTER_NAME", "Test")
            .env("GIT_COMMITTER_EMAIL", "test@example.com");
        Sandbox { dir, git }
    }

    fn path(&self, name: &str) -> PathBuf {
        self.dir.path().canonicalize().unwrap().join(name)
    }

    async fn g(&self, dir: &Path, args: &[&str]) -> String {
        self.git
            .run(Some(dir), args)
            .await
            .unwrap_or_else(|e| panic!("git {args:?}: {e}"))
    }

    async fn repo(&self, name: &str) -> PathBuf {
        let path = self.path(name);
        std::fs::create_dir_all(&path).unwrap();
        self.g(&path, &["init", "--quiet", "-b", "main"]).await;
        path
    }

    async fn commit(&self, repo: &Path, file: &str, text: &str) {
        write(repo, file, text);
        self.g(repo, &["add", "--", file]).await;
        self.g(
            repo,
            &["commit", "--quiet", "-m", &format!("change {file}")],
        )
        .await;
    }

    /// A repository with one commit, pushed to a bare "studio" it tracks.
    async fn repo_with_upstream(&self, name: &str) -> (PathBuf, PathBuf) {
        let bare = self.path(&format!("{name}.git"));
        std::fs::create_dir_all(&bare).unwrap();
        self.g(&bare, &["init", "--quiet", "--bare", "-b", "main"])
            .await;
        let repo = self.repo(name).await;
        self.commit(&repo, "README.md", "hello\n").await;
        self.g(&repo, &["remote", "add", "origin", bare.to_str().unwrap()])
            .await;
        self.g(&repo, &["push", "--quiet", "-u", "origin", "main"])
            .await;
        (repo, bare)
    }
}

fn write(repo: &Path, file: &str, text: &str) {
    let path = repo.join(file);
    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
    std::fs::write(path, text).unwrap();
}

fn invalid(result: Result<impl std::fmt::Debug, Error>) -> String {
    match result {
        Err(Error::Invalid(message)) => message,
        other => panic!("expected a refusal, got {other:?}"),
    }
}

// ---------------------------------------------------------------- changes

#[tokio::test]
async fn changes_lists_every_kind_with_line_counts() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    s.commit(&repo, "keep.txt", "one\ntwo\nthree\n").await;
    s.commit(&repo, "gone.txt", "bye\n").await;
    s.commit(&repo, "old name.txt", "a\nb\nc\nd\ne\nf\n").await;
    s.commit(&repo, "logo.bin", "x").await;

    write(&repo, "keep.txt", "one\n2\nthree\nfour\n");
    std::fs::remove_file(repo.join("gone.txt")).unwrap();
    s.g(&repo, &["mv", "old name.txt", "new name.txt"]).await;
    std::fs::write(repo.join("logo.bin"), [0u8, 1, 2, 0]).unwrap();
    write(&repo, "notes/todo.md", "- a\n- b");
    write(&repo, "staged.txt", "s\n");
    s.g(&repo, &["add", "staged.txt"]).await;
    write(&repo, ".gitignore", "ignored.log\n");
    write(&repo, "ignored.log", "noise\n");

    let changes = s.git.changes(&repo).await.unwrap();
    let find = |p: &str| {
        changes
            .iter()
            .find(|c| c.path == p)
            .unwrap_or_else(|| panic!("{p} missing in {changes:?}"))
    };
    assert!(changes.iter().all(|c| c.path != "ignored.log"));
    assert!(changes.windows(2).all(|w| w[0].path <= w[1].path));

    let keep = find("keep.txt");
    assert_eq!(keep.kind, ChangeKind::Modified);
    assert_eq!((keep.additions, keep.deletions), (Some(2), Some(1)));
    assert!(!keep.staged);

    assert_eq!(find("gone.txt").kind, ChangeKind::Deleted);
    assert_eq!(find("gone.txt").deletions, Some(1));

    let renamed = find("new name.txt");
    assert_eq!(renamed.kind, ChangeKind::Renamed);
    assert_eq!(renamed.old_path.as_deref(), Some("old name.txt"));
    assert!(renamed.staged);

    let binary = find("logo.bin");
    assert!(binary.binary);
    assert_eq!(binary.additions, None);

    let untracked = find("notes/todo.md");
    assert_eq!(untracked.kind, ChangeKind::Untracked);
    assert_eq!(
        (untracked.additions, untracked.deletions),
        (Some(2), Some(0))
    );

    let staged = find("staged.txt");
    assert_eq!(staged.kind, ChangeKind::Added);
    assert!(staged.staged);
    assert_eq!(staged.additions, Some(1));

    assert_eq!(find(".gitignore").kind, ChangeKind::Untracked);
}

#[tokio::test]
async fn changes_work_before_the_first_commit_and_outside_a_repo() {
    let s = Sandbox::new();
    let repo = s.repo("fresh").await;
    assert!(s.git.changes(&repo).await.unwrap().is_empty());
    write(&repo, "a.txt", "a\n");
    write(&repo, "b.txt", "b\n");
    s.g(&repo, &["add", "a.txt"]).await;
    let changes = s.git.changes(&repo).await.unwrap();
    assert_eq!(changes.len(), 2);
    assert_eq!(changes[0].kind, ChangeKind::Added);
    assert_eq!(changes[1].kind, ChangeKind::Untracked);

    let plain = s.path("plain");
    std::fs::create_dir_all(&plain).unwrap();
    assert!(matches!(
        s.git.changes(&plain).await,
        Err(Error::NotARepo(_))
    ));
}

// ------------------------------------------------------------------ diffs

#[tokio::test]
async fn diffs_of_real_files_parse_into_lines() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    s.commit(&repo, "a.txt", "one\ntwo\nthree\n").await;
    s.commit(&repo, "gone.txt", "bye\n").await;
    s.commit(
        &repo,
        "was.rs",
        "fn a() {}\nfn b() {}\nfn c() {}\nfn d() {}\n",
    )
    .await;
    s.commit(&repo, "img.bin", "x").await;
    s.commit(&repo, "tail.txt", "end").await;

    // Modified: staged and unstaged parts both show against HEAD.
    write(&repo, "a.txt", "one\nTWO\nthree\n");
    s.g(&repo, &["add", "a.txt"]).await;
    write(&repo, "a.txt", "one\nTWO\nthree\nfour\n");
    let text = s.git.diff_worktree(&repo, "a.txt", None).await.unwrap();
    let files = diff::parse(&text);
    assert_eq!(files.len(), 1);
    assert_eq!(files[0].status, FileStatus::Modified);
    assert_eq!((files[0].additions, files[0].deletions), (2, 1));
    let added: Vec<&str> = files[0]
        .lines
        .iter()
        .filter(|l| l.kind == LineKind::Add)
        .map(|l| l.text.as_str())
        .collect();
    assert_eq!(added, ["TWO", "four"]);

    // Only the staged part.
    let staged = diff::parse(&s.git.diff_staged(&repo, "a.txt").await.unwrap());
    assert_eq!((staged[0].additions, staged[0].deletions), (1, 1));

    // Deleted.
    std::fs::remove_file(repo.join("gone.txt")).unwrap();
    let files = diff::parse(&s.git.diff_worktree(&repo, "gone.txt", None).await.unwrap());
    assert_eq!(files[0].status, FileStatus::Deleted);
    assert_eq!(files[0].path(), "gone.txt");
    assert_eq!(files[0].lines[1].kind, LineKind::Delete);

    // Renamed with an edit.
    s.g(&repo, &["mv", "was.rs", "now is.rs"]).await;
    write(
        &repo,
        "now is.rs",
        "fn a() {}\nfn b() {}\nfn c() {}\nfn e() {}\n",
    );
    let text = s
        .git
        .diff_worktree(&repo, "now is.rs", Some("was.rs"))
        .await
        .unwrap();
    let files = diff::parse(&text);
    assert_eq!(files.len(), 1, "{text}");
    assert_eq!(files[0].status, FileStatus::Renamed);
    assert_eq!(files[0].old_path.as_deref(), Some("was.rs"));
    assert_eq!(files[0].new_path.as_deref(), Some("now is.rs"));
    assert_eq!((files[0].additions, files[0].deletions), (1, 1));

    // Binary.
    std::fs::write(repo.join("img.bin"), [0u8, 159, 146, 150]).unwrap();
    let files = diff::parse(&s.git.diff_worktree(&repo, "img.bin", None).await.unwrap());
    assert!(files[0].binary);
    assert!(files[0].lines.is_empty());

    // No newline at the end, before and after.
    write(&repo, "tail.txt", "END");
    let files = diff::parse(&s.git.diff_worktree(&repo, "tail.txt", None).await.unwrap());
    let kinds: Vec<LineKind> = files[0].lines.iter().map(|l| l.kind).collect();
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

    // Untracked, with a name that would be an option without `--`.
    write(&repo, "-n", "sneaky\nfile\n");
    let files = diff::parse(&s.git.diff_untracked(&repo, "-n").await.unwrap());
    assert_eq!(files[0].status, FileStatus::Added);
    assert_eq!(files[0].path(), "-n");
    assert_eq!(files[0].additions, 2);

    // Paths that leave the repository are refused before git runs.
    assert!(s
        .git
        .diff_worktree(&repo, "../r/a.txt", None)
        .await
        .is_err());
    assert!(s.git.diff_untracked(&repo, "/etc/hosts").await.is_err());
}

// ----------------------------------------------------------------- commit

#[tokio::test]
async fn commit_takes_exactly_the_chosen_files() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    s.commit(&repo, "a.txt", "a\n").await;
    s.commit(&repo, "b.txt", "b\n").await;
    s.commit(&repo, "c.txt", "c\n").await;
    s.commit(&repo, "old.txt", "1\n2\n3\n4\n5\n").await;

    write(&repo, "a.txt", "a changed\n");
    write(&repo, "b.txt", "b changed\n");
    s.g(&repo, &["add", "b.txt"]).await; // staged by hand, but not ticked
    std::fs::remove_file(repo.join("c.txt")).unwrap();
    s.g(&repo, &["mv", "old.txt", "new.txt"]).await;
    write(&repo, "fresh.txt", "new\n");
    write(&repo, "left.txt", "stay untracked\n");
    write(&repo, "*.txt", "a file whose name is a glob\n");

    let paths: Vec<String> = ["a.txt", "c.txt", "new.txt", "fresh.txt", "*.txt"]
        .iter()
        .map(|p| p.to_string())
        .collect();
    let sha = s
        .git
        .commit_paths(
            &repo,
            &paths,
            "  Pick some files  ",
            "With a description.\n",
        )
        .await
        .unwrap();
    assert_eq!(sha.len(), 40);

    let committed = s
        .g(&repo, &["show", "--name-status", "--format=", "-M", "HEAD"])
        .await;
    let mut lines: Vec<&str> = committed.lines().collect();
    lines.sort_unstable();
    assert_eq!(
        lines,
        [
            "A\t*.txt",
            "A\tfresh.txt",
            "D\tc.txt",
            "M\ta.txt",
            "R100\told.txt\tnew.txt"
        ]
    );
    assert_eq!(
        s.g(&repo, &["log", "-1", "--format=%s%n--%n%b"]).await,
        "Pick some files\n--\nWith a description."
    );

    // What wasn't ticked is exactly as it was: b.txt still staged,
    // left.txt still untracked.
    let left = s.git.changes(&repo).await.unwrap();
    let summary: Vec<(&str, ChangeKind, bool)> = left
        .iter()
        .map(|c| (c.path.as_str(), c.kind, c.staged))
        .collect();
    assert_eq!(
        summary,
        [
            ("b.txt", ChangeKind::Modified, true),
            ("left.txt", ChangeKind::Untracked, false)
        ]
    );
}

#[tokio::test]
async fn commit_refuses_nonsense_and_leaves_no_trace() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    s.commit(&repo, "a.txt", "a\n").await;
    write(&repo, "a.txt", "changed\n");
    write(&repo, "new.txt", "n\n");
    let all = vec!["a.txt".to_string()];

    assert!(invalid(s.git.commit_paths(&repo, &all, "  ", "").await).contains("message"));
    assert!(invalid(s.git.commit_paths(&repo, &[], "msg", "").await).contains("Tick"));
    assert!(invalid(
        s.git
            .commit_paths(&repo, &["nope.txt".into()], "msg", "")
            .await
    )
    .contains("no changes"));
    assert!(s
        .git
        .commit_paths(&repo, &["../x".into()], "msg", "")
        .await
        .is_err());
    assert_eq!(s.git.count_commits(&repo, &["HEAD"]).await.unwrap(), 1);

    // A failing hook: the untracked file we added is taken back out.
    let hook = repo.join(".git/hooks/pre-commit");
    std::fs::write(&hook, "#!/bin/sh\necho no >&2\nexit 1\n").unwrap();
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&hook, std::fs::Permissions::from_mode(0o755)).unwrap();
    }
    let result = s
        .git
        .commit_paths(&repo, &["new.txt".into()], "msg", "")
        .await;
    assert!(matches!(result, Err(Error::Git { .. })), "{result:?}");
    let changes = s.git.changes(&repo).await.unwrap();
    assert_eq!(changes[1].path, "new.txt");
    assert_eq!(changes[1].kind, ChangeKind::Untracked);
}

#[tokio::test]
async fn the_first_commit_of_a_repository_works() {
    let s = Sandbox::new();
    let repo = s.repo("fresh").await;
    write(&repo, "README.md", "# hi\n");
    write(&repo, "later.txt", "not yet\n");
    s.git
        .commit_paths(&repo, &["README.md".into()], "Start", "")
        .await
        .unwrap();
    assert_eq!(s.g(&repo, &["ls-files"]).await, "README.md");
    assert_eq!(s.git.changes(&repo).await.unwrap().len(), 1);
}

// ------------------------------------------------------ discard and undo

#[tokio::test]
async fn discard_puts_chosen_files_back() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    s.commit(&repo, "a.txt", "a\n").await;
    s.commit(&repo, "b.txt", "b\n").await;
    s.commit(&repo, "c.txt", "c\n").await;
    s.commit(&repo, "old.txt", "o\n").await;
    write(&repo, "a.txt", "changed\n");
    s.g(&repo, &["add", "a.txt"]).await;
    write(&repo, "b.txt", "keep this change\n");
    std::fs::remove_file(repo.join("c.txt")).unwrap();
    s.g(&repo, &["mv", "old.txt", "moved.txt"]).await;
    write(&repo, "new.txt", "n\n");
    write(&repo, "added.txt", "x\n");
    s.g(&repo, &["add", "added.txt"]).await;

    let paths: Vec<String> = ["a.txt", "c.txt", "moved.txt", "new.txt", "added.txt"]
        .iter()
        .map(|p| p.to_string())
        .collect();
    s.git.discard_paths(&repo, &paths).await.unwrap();

    assert_eq!(std::fs::read_to_string(repo.join("a.txt")).unwrap(), "a\n");
    assert_eq!(std::fs::read_to_string(repo.join("c.txt")).unwrap(), "c\n");
    assert_eq!(
        std::fs::read_to_string(repo.join("old.txt")).unwrap(),
        "o\n"
    );
    assert!(!repo.join("moved.txt").exists());
    assert!(!repo.join("new.txt").exists());
    assert!(!repo.join("added.txt").exists());
    let left = s.git.changes(&repo).await.unwrap();
    assert_eq!(left.len(), 1);
    assert_eq!(left[0].path, "b.txt");
}

#[tokio::test]
async fn undo_only_takes_back_an_unpushed_commit() {
    let s = Sandbox::new();
    let (repo, _bare) = s.repo_with_upstream("r").await;

    // Pushed: refused.
    s.commit(&repo, "pushed.txt", "p\n").await;
    s.g(&repo, &["push", "--quiet"]).await;
    assert!(invalid(s.git.undo_last_commit(&repo).await).contains("already pushed"));

    s.commit(&repo, "work.txt", "w\n").await;
    assert!(!s.git.is_pushed(&repo, "HEAD").await.unwrap());
    let subject = s.git.undo_last_commit(&repo).await.unwrap();
    assert_eq!(subject, "change work.txt");
    assert_eq!(s.git.count_commits(&repo, &["HEAD"]).await.unwrap(), 2);
    // Soft: the work is still there, staged.
    let changes = s.git.changes(&repo).await.unwrap();
    assert_eq!(changes[0].path, "work.txt");
    assert!(changes[0].staged);

    // The first commit of a repository: refused.
    let lone = s.repo("lone").await;
    s.commit(&lone, "a.txt", "a\n").await;
    assert!(invalid(s.git.undo_last_commit(&lone).await).contains("first commit"));
    let empty = s.repo("empty").await;
    assert!(invalid(s.git.undo_last_commit(&empty).await).contains("no commit"));
}

// --------------------------------------------------------------- branches

#[tokio::test]
async fn branches_are_listed_and_switching_refuses_to_lose_work() {
    let s = Sandbox::new();
    let (repo, _bare) = s.repo_with_upstream("r").await;
    s.git.create_branch(&repo, "feature/x", None).await.unwrap();
    s.commit(&repo, "README.md", "hello from the feature\n")
        .await;
    s.commit(&repo, "extra.txt", "e\n").await;

    let branches = s.git.branches(&repo).await.unwrap();
    let names: Vec<(&str, bool)> = branches
        .iter()
        .map(|b| (b.name.as_str(), b.current))
        .collect();
    assert_eq!(names, [("feature/x", true), ("main", false)]);
    assert_eq!(branches[0].subject, "change extra.txt");
    assert_eq!(branches[1].upstream.as_deref(), Some("origin/main"));
    assert_eq!((branches[1].ahead, branches[1].behind), (0, 0));

    // A change to a file that differs between the branches would be lost.
    write(&repo, "README.md", "uncommitted work\n");
    let message = invalid(s.git.switch_branch(&repo, "main").await);
    assert!(
        message.contains("would lose your uncommitted changes"),
        "{message}"
    );
    assert!(message.contains("README.md"), "{message}");
    assert_eq!(
        s.git.current_branch(&repo).await.unwrap().as_deref(),
        Some("feature/x")
    );
    assert_eq!(
        std::fs::read_to_string(repo.join("README.md")).unwrap(),
        "uncommitted work\n"
    );

    // A change that doesn't clash comes along.
    s.g(&repo, &["checkout", "--quiet", "--", "README.md"])
        .await;
    write(&repo, "scratch.txt", "untracked\n");
    s.git.switch_branch(&repo, "main").await.unwrap();
    assert_eq!(
        s.git.current_branch(&repo).await.unwrap().as_deref(),
        Some("main")
    );
    assert!(repo.join("scratch.txt").exists());

    assert!(invalid(s.git.switch_branch(&repo, "nope").await).contains("no branch"));
    assert!(invalid(s.git.switch_branch(&repo, "--detach").await).contains("branch name"));
    assert!(invalid(s.git.create_branch(&repo, "main", None).await).contains("already"));
    assert!(invalid(s.git.create_branch(&repo, "bad..name", None).await).contains("branch name"));
    assert!(s
        .git
        .create_branch(&repo, "ok", Some("HEAD"))
        .await
        .is_err());
}

#[tokio::test]
async fn pull_is_fast_forward_only() {
    let s = Sandbox::new();
    let (repo, bare) = s.repo_with_upstream("r").await;
    let other = s.path("other");
    s.g(
        s.dir.path(),
        &[
            "clone",
            "--quiet",
            bare.to_str().unwrap(),
            other.to_str().unwrap(),
        ],
    )
    .await;
    s.commit(&other, "theirs.txt", "t\n").await;
    s.commit(&other, "theirs2.txt", "t\n").await;
    s.g(&other, &["push", "--quiet"]).await;

    assert_eq!(s.git.pull_ff(&repo).await.unwrap().commits, 2);
    assert_eq!(s.git.pull_ff(&repo).await.unwrap().commits, 0);

    // Both sides move: refused, nothing changes.
    s.commit(&other, "theirs3.txt", "t\n").await;
    s.g(&other, &["push", "--quiet"]).await;
    s.commit(&repo, "mine.txt", "m\n").await;
    let head = s.g(&repo, &["rev-parse", "HEAD"]).await;
    assert!(invalid(s.git.pull_ff(&repo).await).contains("both moved on"));
    assert_eq!(s.g(&repo, &["rev-parse", "HEAD"]).await, head);

    // No upstream at all.
    s.git.create_branch(&repo, "solo", None).await.unwrap();
    assert!(invalid(s.git.pull_ff(&repo).await).contains("Push it first"));
}

// ---------------------------------------------------------------- history

#[tokio::test]
async fn history_is_a_two_lane_graph() {
    let s = Sandbox::new();
    let (repo, bare) = s.repo_with_upstream("r").await;
    s.commit(&repo, "two.txt", "2\n").await;
    s.g(&repo, &["push", "--quiet"]).await;
    s.g(&repo, &["tag", "v0.9"]).await;

    // In step with the upstream: one lane.
    let h = s.git.history(&repo, 50).await.unwrap();
    assert_eq!(h.branch.as_deref(), Some("main"));
    assert_eq!(h.upstream.as_deref(), Some("origin/main"));
    assert_eq!((h.ahead, h.behind), (0, 0));
    assert_eq!(h.commits.len(), 2);
    assert!(h.commits.iter().all(|c| c.lane == 0 && !c.unpushed));
    assert_eq!(h.commits[0].line0, Segment::Start);
    assert_eq!(h.commits[1].line0, Segment::Full);
    assert!(h.commits.iter().all(|c| c.line1 == Segment::None));
    let refs: Vec<(RefKind, &str)> = h.commits[0]
        .refs
        .iter()
        .map(|r| (r.kind, r.name.as_str()))
        .collect();
    assert!(refs.contains(&(RefKind::Head, "main")), "{refs:?}");
    assert!(refs.contains(&(RefKind::Remote, "main")), "{refs:?}");
    assert!(refs.contains(&(RefKind::Tag, "v0.9")), "{refs:?}");
    assert_eq!(h.commits[0].subject, "change two.txt");
    assert_eq!(h.commits[0].author, "Test");
    assert_eq!(h.commits[0].parents, [h.commits[1].sha.clone()]);
    assert!(h.commits[0].date.contains('T'));

    // Two commits only here, one only in the upstream.
    let other = s.path("other");
    s.g(
        s.dir.path(),
        &[
            "clone",
            "--quiet",
            bare.to_str().unwrap(),
            other.to_str().unwrap(),
        ],
    )
    .await;
    s.commit(&other, "theirs.txt", "t\n").await;
    s.g(&other, &["push", "--quiet"]).await;
    s.commit(&repo, "mine1.txt", "m\n").await;
    s.commit(&repo, "mine2.txt", "m\n").await;
    s.git.fetch(&repo, None).await.unwrap();

    let h = s.git.history(&repo, 50).await.unwrap();
    assert_eq!((h.ahead, h.behind), (2, 1));
    assert_eq!(h.commits.len(), 5);
    let mine: Vec<&str> = h
        .commits
        .iter()
        .filter(|c| c.unpushed)
        .map(|c| c.subject.as_str())
        .collect();
    assert_eq!(mine, ["change mine2.txt", "change mine1.txt"]);
    let theirs: Vec<&str> = h
        .commits
        .iter()
        .filter(|c| c.incoming)
        .map(|c| c.subject.as_str())
        .collect();
    assert_eq!(theirs, ["change theirs.txt"]);
    for c in &h.commits {
        assert_eq!(c.lane, u8::from(c.unpushed), "{c:?}");
    }
    // Lane 1 starts at the newest local commit and bends into lane 0 at the
    // newest commit both sides have.
    let first_local = h.commits.iter().position(|c| c.unpushed).unwrap();
    assert_eq!(h.commits[first_local].line1, Segment::Start);
    let join = h
        .commits
        .iter()
        .position(|c| c.subject == "change two.txt")
        .unwrap();
    assert_eq!(h.commits[join].line1, Segment::Join);
    assert_eq!(h.commits[join].lane, 0);
    assert_eq!(h.commits[4].line1, Segment::None);
    assert_eq!(h.commits[4].line0, Segment::Full);
    let first_shared_lane = h.commits.iter().position(|c| c.lane == 0).unwrap();
    assert_eq!(h.commits[first_shared_lane].line0, Segment::Start);

    // A limit, and what is left out.
    let h = s.git.history(&repo, 2).await.unwrap();
    assert_eq!(h.commits.len(), 2);
    assert!(h.more);

    // No upstream: everything on lane 0, nothing marked unpushed.
    let lone = s.repo("lone").await;
    s.commit(&lone, "a.txt", "a\n").await;
    let h = s.git.history(&lone, 10).await.unwrap();
    assert_eq!(h.upstream, None);
    assert_eq!(h.commits.len(), 1);
    assert_eq!(h.commits[0].lane, 0);

    // No commits at all.
    let empty = s.repo("empty").await;
    let h = s.git.history(&empty, 10).await.unwrap();
    assert!(h.commits.is_empty());
    assert_eq!(h.branch.as_deref(), Some("main"));
}

#[tokio::test]
async fn commit_detail_has_the_message_and_the_files() {
    let s = Sandbox::new();
    let (repo, _bare) = s.repo_with_upstream("r").await;
    write(&repo, "a.txt", "1\n2\n3\n");
    write(&repo, "README.md", "hello again\n");
    s.g(&repo, &["add", "."]).await;
    s.g(
        &repo,
        &[
            "commit",
            "--quiet",
            "-m",
            "Tray: show who's connected",
            "-m",
            "Lists live sessions.\n\nSecond paragraph.",
        ],
    )
    .await;
    let sha = s.g(&repo, &["rev-parse", "HEAD"]).await;

    let d = s.git.commit_detail(&repo, &sha[..8]).await.unwrap();
    assert_eq!(d.sha, sha);
    assert_eq!(d.subject, "Tray: show who's connected");
    assert_eq!(d.body, "Lists live sessions.\n\nSecond paragraph.");
    assert_eq!(d.author, "Test");
    assert_eq!(d.email, "test@example.com");
    assert!(d.is_head);
    assert!(!d.pushed);
    let files: Vec<(&str, Option<u32>, Option<u32>)> = d
        .files
        .iter()
        .map(|f| (f.path.as_str(), f.additions, f.deletions))
        .collect();
    assert_eq!(
        files,
        [("README.md", Some(1), Some(1)), ("a.txt", Some(3), Some(0))]
    );

    let first = s.g(&repo, &["rev-parse", "HEAD~1"]).await;
    let d = s.git.commit_detail(&repo, &first).await.unwrap();
    assert!(d.pushed);
    assert!(!d.is_head);
    assert_eq!(d.files.len(), 1, "the root commit lists its files too");

    assert!(invalid(s.git.commit_detail(&repo, "deadbeef").await).contains("no commit"));
    assert!(invalid(s.git.commit_detail(&repo, "--all").await).contains("commit ID"));
}

// --------------------------------------------------------------- identity

#[tokio::test]
async fn identity_is_set_in_the_repository_only() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    let git = Git::new()
        .env("HOME", s.path("home"))
        .env("GIT_CONFIG_GLOBAL", s.path("home/gitconfig"))
        .env("GIT_CONFIG_NOSYSTEM", "1");
    assert_eq!(git.identity(&repo).await.unwrap(), (None, None));
    git.set_identity(&repo, " Demi ", "demi@company.example")
        .await
        .unwrap();
    assert_eq!(
        git.identity(&repo).await.unwrap(),
        (Some("Demi".into()), Some("demi@company.example".into()))
    );
    let local = std::fs::read_to_string(repo.join(".git/config")).unwrap();
    assert!(local.contains("demi@company.example"));
    assert!(
        !s.path("home/gitconfig").exists(),
        "the global config is never written"
    );

    let other = s.repo("other").await;
    assert_eq!(git.identity(&other).await.unwrap(), (None, None));
}

#[tokio::test]
async fn init_and_last_fetch() {
    let s = Sandbox::new();
    let dir = s.path("made/here");
    s.git.init(&dir, "main").await.unwrap();
    assert_eq!(
        s.git.current_branch(&dir).await.unwrap().as_deref(),
        Some("main")
    );
    assert!(!s.git.has_head(&dir).await.unwrap());
    assert_eq!(s.git.last_fetch(&dir).await.unwrap(), None);

    let (repo, _bare) = s.repo_with_upstream("r").await;
    s.git.fetch(&repo, None).await.unwrap();
    assert!(s.git.last_fetch(&repo).await.unwrap().is_some());
}

// ------------------------------------------------------- remote matching

fn studio(full_name: &str) -> atelier_core::api::Repo {
    let (owner, name) = full_name.split_once('/').unwrap();
    atelier_core::api::Repo {
        full_name: full_name.into(),
        owner: owner.into(),
        name: name.into(),
        clone_https: format!("https://studio.example/{full_name}.git"),
        clone_ssh: format!("ssh://git@studio.example:2223/{full_name}.git"),
        ..Default::default()
    }
}

#[tokio::test]
async fn a_working_copy_matches_through_a_remote_that_is_not_origin() {
    let s = Sandbox::new();
    let repo = s.repo("checkout").await;
    s.commit(&repo, "a.txt", "a\n").await;
    let repos = [studio("DemiInfinity/Atelier"), studio("DemiInfinity/Other")];

    // No remotes at all.
    let local = s.git.inspect(&repo).await.unwrap();
    assert!(local.remotes.is_empty());
    assert_eq!(local.studio_repo(&repos), None);

    // Only a remote called `atelier`, written in another case and form.
    s.g(
        &repo,
        &[
            "remote",
            "add",
            "atelier",
            "git@Studio.Example:deminfinity/atelier.git",
        ],
    )
    .await;
    let local = s.git.inspect(&repo).await.unwrap();
    assert_eq!(local.remotes.len(), 1);
    assert_eq!(
        local.studio_repo(&repos),
        None,
        "a different owner is a different repository"
    );
    s.g(
        &repo,
        &[
            "remote",
            "set-url",
            "atelier",
            "git@Studio.Example:demiINFINITY/atelier.git",
        ],
    )
    .await;
    let local = s.git.inspect(&repo).await.unwrap();
    let (remote, matched) = local.studio_remote(&repos).unwrap();
    assert_eq!(remote.name, "atelier");
    assert_eq!(matched.full_name, "DemiInfinity/Atelier");

    // `origin` on GitHub next to it: the studio remote still matches.
    s.g(
        &repo,
        &[
            "remote",
            "add",
            "origin",
            "https://github.com/someone/atelier.git",
        ],
    )
    .await;
    let local = s.git.inspect(&repo).await.unwrap();
    assert_eq!(local.remote_name.as_deref(), Some("origin"));
    assert_eq!(local.remotes.len(), 2);
    assert_eq!(
        local.studio_repo(&repos).map(|r| r.full_name.as_str()),
        Some("DemiInfinity/Atelier")
    );
}

#[tokio::test]
async fn with_several_studio_remotes_the_one_the_branch_follows_wins() {
    let s = Sandbox::new();
    let (repo, bare) = s.repo_with_upstream("r").await;
    // `origin` is a local bare repository (matches nothing). Two more
    // remotes point at two studio repositories.
    s.g(
        &repo,
        &[
            "remote",
            "add",
            "first",
            "https://studio.example/DemiInfinity/Other.git",
        ],
    )
    .await;
    s.g(
        &repo,
        &[
            "remote",
            "add",
            "team/fork",
            "https://studio.example/DemiInfinity/Atelier.git",
        ],
    )
    .await;
    let repos = [studio("DemiInfinity/Atelier"), studio("DemiInfinity/Other")];
    let local = s.git.inspect(&repo).await.unwrap();
    assert_eq!(local.upstream_remote.as_deref(), Some("origin"));
    assert_eq!(
        local.studio_remote(&repos).map(|(r, _)| r.name.as_str()),
        Some("first"),
        "no upstream among them: git's order"
    );

    // Make the branch follow `team/fork` (a remote name with a slash).
    s.g(
        &repo,
        &["config", "remote.team/fork.url", bare.to_str().unwrap()],
    )
    .await;
    s.g(&repo, &["fetch", "--quiet", "team/fork"]).await;
    s.g(
        &repo,
        &["branch", "--quiet", "--set-upstream-to=team/fork/main"],
    )
    .await;
    s.g(
        &repo,
        &[
            "config",
            "remote.team/fork.url",
            "https://studio.example/DemiInfinity/Atelier.git",
        ],
    )
    .await;
    let local = s.git.inspect(&repo).await.unwrap();
    assert_eq!(local.upstream.as_deref(), Some("team/fork/main"));
    assert_eq!(local.upstream_remote.as_deref(), Some("team/fork"));
    let (remote, matched) = local.studio_remote(&repos).unwrap();
    assert_eq!(remote.name, "team/fork");
    assert_eq!(matched.full_name, "DemiInfinity/Atelier");
}
