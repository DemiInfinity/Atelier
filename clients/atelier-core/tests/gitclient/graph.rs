use atelier_core::diff::{FileStatus, LineKind};
use atelier_core::git::history::{EdgeKind, GraphQuery};
use atelier_core::git::RefKind;

use crate::kit::*;

/// main: A - B ------- M (merge of side) - C
///            \- S1 - S2 -/        tag v1 on B, branch `other` at S2
async fn merged(s: &Sandbox) -> std::path::PathBuf {
    let repo = s.repo("r").await;
    s.commit(&repo, "a.txt", "a\n", "A: start").await;
    s.commit(&repo, "b.txt", "b\n", "B: second").await;
    s.g(&repo, &["tag", "v1"]).await;
    s.g(&repo, &["switch", "--quiet", "-c", "other"]).await;
    s.g(&repo, &["config", "user.name", "Rhea Okafor"]).await;
    s.commit(&repo, "docs/guide.md", "one\ntwo\n", "S1: docs")
        .await;
    s.commit(&repo, "docs/guide.md", "one\ntwo\nthree\n", "S2: more docs")
        .await;
    s.g(&repo, &["config", "user.name", "Test Person"]).await;
    s.g(&repo, &["switch", "--quiet", "main"]).await;
    s.commit(&repo, "a.txt", "a\na2\n", "B2: on main").await;
    s.g(
        &repo,
        &[
            "merge",
            "--quiet",
            "--no-ff",
            "-m",
            "M: merge other",
            "other",
        ],
    )
    .await;
    s.commit(&repo, "c.txt", "c\n", "C: after").await;
    repo
}

#[tokio::test]
async fn the_graph_has_lanes_refs_and_pages() {
    let s = Sandbox::new();
    let repo = merged(&s).await;
    // Something of the app's own must never show up as history.
    write(&repo, "a.txt", "dirty\n");
    s.git
        .discard(
            &repo,
            "a.txt",
            &atelier_core::git::stage::Selection::File,
            None,
        )
        .await
        .unwrap();

    let page = s.git.graph(&repo, &GraphQuery::default()).await.unwrap();
    let subjects: Vec<&str> = page.rows.iter().map(|r| r.subject.as_str()).collect();
    assert_eq!(
        subjects,
        [
            "C: after",
            "M: merge other",
            "S2: more docs",
            "S1: docs",
            "B2: on main",
            "B: second",
            "A: start"
        ]
    );
    assert!(!page.more && !page.filtered);
    assert_eq!(page.branch.as_deref(), Some("main"));
    assert_eq!(page.columns, 2);
    let row = |subject: &str| {
        page.rows
            .iter()
            .find(|r| r.subject.starts_with(subject))
            .unwrap()
    };
    assert!(row("C").is_head && row("C").unpushed);
    assert_eq!(row("C").refs[0].kind, RefKind::Head);
    assert_eq!(row("M").parents.len(), 2);
    // The merge sends a line into a second column; the side commits sit there.
    assert_eq!(
        (row("M").column, row("S2").column, row("S1").column),
        (0, 1, 1)
    );
    assert_eq!(row("B2").column, 0);
    let outs: Vec<(u32, u32)> = row("M")
        .edges
        .iter()
        .filter(|e| e.kind == EdgeKind::Out)
        .map(|e| (e.from, e.to))
        .collect();
    assert_eq!(outs, [(0, 0), (0, 1)]);
    // Colours follow the line of work.
    assert_eq!(row("S1").color, row("S2").color);
    assert_ne!(row("S1").color, row("C").color);
    assert!(row("S2")
        .edges
        .iter()
        .any(|e| e.kind == EdgeKind::Pass && e.from == 0));
    assert!(row("S2")
        .refs
        .iter()
        .any(|r| r.kind == RefKind::Branch && r.name == "other"));
    assert!(row("B:")
        .refs
        .iter()
        .any(|r| r.kind == RefKind::Tag && r.name == "v1"));
    assert_eq!(row("S1").author, "Rhea Okafor");
    assert!(row("A").edges.iter().all(|e| e.kind == EdgeKind::In));

    // Pages: the second page's lanes carry on from the first.
    let first = s
        .git
        .graph(
            &repo,
            &GraphQuery {
                limit: 3,
                ..Default::default()
            },
        )
        .await
        .unwrap();
    assert_eq!(first.rows.len(), 3);
    assert!(first.more);
    let second = s
        .git
        .graph(
            &repo,
            &GraphQuery {
                skip: 3,
                limit: 3,
                ..Default::default()
            },
        )
        .await
        .unwrap();
    assert_eq!(second.rows[0].subject, "S1: docs");
    assert_eq!(second.rows[0].column, 1);
    assert_eq!(second.rows[0].edges, row("S1").edges);
    assert!(second.more);

    // One branch only.
    let other = s
        .git
        .graph(
            &repo,
            &GraphQuery {
                branch: Some("other".into()),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    assert_eq!(other.rows.len(), 4);
    assert_eq!(other.columns, 1);
    assert!(invalid(
        s.git
            .graph(
                &repo,
                &GraphQuery {
                    branch: Some("--all".into()),
                    ..Default::default()
                }
            )
            .await
    )
    .contains("isn’t a commit"));
}

#[tokio::test]
async fn search_by_message_author_path_and_id() {
    let s = Sandbox::new();
    let repo = merged(&s).await;
    let find = |query: GraphQuery| {
        let (git, repo) = (s.git.clone(), repo.clone());
        async move {
            let page = git.graph(&repo, &query).await.unwrap();
            assert!(page.filtered);
            assert!(page.rows.iter().all(|r| r.edges.is_empty()));
            page.rows
                .into_iter()
                .map(|r| r.subject)
                .collect::<Vec<String>>()
        }
    };
    assert_eq!(
        find(GraphQuery {
            text: Some("DOCS".into()),
            ..Default::default()
        })
        .await,
        ["S2: more docs", "S1: docs"]
    );
    // The same box finds people.
    let by_name = find(GraphQuery {
        text: Some("rhea".into()),
        ..Default::default()
    })
    .await;
    assert_eq!(by_name.len(), 2);
    let by_author = find(GraphQuery {
        author: Some("Okafor".into()),
        ..Default::default()
    })
    .await;
    assert_eq!(by_author, by_name);
    // A path, with and without a word.
    let by_path = find(GraphQuery {
        path: Some("docs/".into()),
        ..Default::default()
    })
    .await;
    assert_eq!(by_path.len(), 2);
    let both = find(GraphQuery {
        path: Some("docs/guide.md".into()),
        text: Some("more".into()),
        ..Default::default()
    })
    .await;
    assert_eq!(both, ["S2: more docs"]);
    // The start of an id.
    let b = s.g(&repo, &["rev-parse", "v1"]).await;
    let by_id = find(GraphQuery {
        text: Some(b[..8].to_string()),
        ..Default::default()
    })
    .await;
    assert_eq!(by_id, ["B: second"]);
    assert!(find(GraphQuery {
        text: Some("no such words".into()),
        ..Default::default()
    })
    .await
    .is_empty());
    // An empty repository has an empty history.
    let empty = s.repo("empty").await;
    let page = s.git.graph(&empty, &GraphQuery::default()).await.unwrap();
    assert!(page.rows.is_empty() && page.head.is_none());
}

#[tokio::test]
async fn commits_files_file_history_and_blame() {
    let s = Sandbox::new();
    let repo = s.repo("r").await;
    let first = s
        .commit(
            &repo,
            "run.go",
            "package tunnel\n\nfunc run() {\n\tdial()\n}\n",
            "Tunnel: skeleton",
        )
        .await;
    s.commit(
        &repo,
        "run.go",
        "package tunnel\n\nfunc run() {\n\tdial()\n\tlog()\n}\n",
        "Log on start",
    )
    .await;
    s.g(&repo, &["mv", "run.go", "supervisor.go"]).await;
    write(
        &repo,
        "supervisor.go",
        "package tunnel\n\nfunc run() {\n\tdial()\n\tlog()\n\tretry()\n}\n",
    );
    std::fs::write(repo.join("logo.png"), [0u8, 1, 2]).unwrap();
    s.g(&repo, &["add", "-A"]).await;
    s.g(
        &repo,
        &[
            "commit",
            "--quiet",
            "-m",
            "Rename and retry",
            "-m",
            "The body.\n\nTwo paragraphs.",
        ],
    )
    .await;
    let renamed = s.head(&repo).await;
    s.g(&repo, &["config", "user.name", "Rhea Okafor"]).await;
    let last = s
        .commit(
            &repo,
            "supervisor.go",
            "package tunnel\n\nfunc run() {\n\tdial()\n\tlog()\n\tretry()\n\tnotify()\n}\n",
            "Notify the tray",
        )
        .await;

    let info = s.git.commit_info(&repo, &renamed[..9]).await.unwrap();
    assert_eq!(info.id, renamed);
    assert_eq!(info.subject, "Rename and retry");
    assert_eq!(info.body, "The body.\n\nTwo paragraphs.");
    assert_eq!(info.author, "Test Person");
    assert!(!info.pushed && !info.is_head && info.on_current);
    assert_eq!(info.parents.len(), 1);
    let files: Vec<(&str, FileStatus, Option<u32>, bool)> = info
        .files
        .iter()
        .map(|f| (f.path.as_str(), f.status, f.additions, f.binary))
        .collect();
    assert_eq!(
        files,
        [
            ("logo.png", FileStatus::Added, None, true),
            ("supervisor.go", FileStatus::Renamed, Some(1), false),
        ]
    );
    assert_eq!(info.files[1].old_path.as_deref(), Some("run.go"));
    assert!(s.git.commit_info(&repo, &last).await.unwrap().is_head);
    // The first commit has files too.
    assert_eq!(
        s.git.commit_info(&repo, &first).await.unwrap().files.len(),
        1
    );
    assert!(invalid(s.git.commit_info(&repo, "nope").await).contains("no commit"));

    let patch = s
        .git
        .commit_file_diff(&repo, &renamed, "supervisor.go", Some("run.go"))
        .await
        .unwrap();
    assert_eq!(patch.status, FileStatus::Renamed);
    assert_eq!((patch.additions, patch.deletions), (1, 0));
    let added: Vec<&str> = patch
        .lines
        .iter()
        .filter(|l| l.kind == LineKind::Add)
        .map(|l| l.text.as_str())
        .collect();
    assert_eq!(added, ["\tretry()"]);
    assert!(
        invalid(s.git.commit_file_diff(&repo, &last, "logo.png", None).await)
            .contains("didn’t change")
    );

    // The file's history crosses its rename.
    let history = s
        .git
        .file_history(&repo, "supervisor.go", None, 0)
        .await
        .unwrap();
    let rows: Vec<(&str, &str, FileStatus)> = history
        .entries
        .iter()
        .map(|e| (e.subject.as_str(), e.path.as_str(), e.status))
        .collect();
    assert_eq!(
        rows,
        [
            ("Notify the tray", "supervisor.go", FileStatus::Modified),
            ("Rename and retry", "supervisor.go", FileStatus::Renamed),
            ("Log on start", "run.go", FileStatus::Modified),
            ("Tunnel: skeleton", "run.go", FileStatus::Added),
        ]
    );
    assert_eq!(history.entries[1].old_path.as_deref(), Some("run.go"));
    assert_eq!(
        (history.entries[0].additions, history.entries[0].deletions),
        (Some(1), Some(0))
    );
    assert_eq!(history.entries[3].additions, Some(5));
    let short = s
        .git
        .file_history(&repo, "supervisor.go", None, 2)
        .await
        .unwrap();
    assert!(short.more && short.entries.len() == 2);

    // Blame: runs of neighbouring lines from the same commit.
    let blame = s.git.blame(&repo, "supervisor.go", None).await.unwrap();
    assert_eq!(blame.lines, 8);
    let runs: Vec<(u32, u32, &str, &str)> = blame
        .runs
        .iter()
        .map(|r| (r.start, r.end, r.summary.as_str(), r.author.as_str()))
        .collect();
    assert_eq!(
        runs,
        [
            (1, 4, "Tunnel: skeleton", "Test Person"),
            (5, 5, "Log on start", "Test Person"),
            (6, 6, "Rename and retry", "Test Person"),
            (7, 7, "Notify the tray", "Rhea Okafor"),
            (8, 8, "Tunnel: skeleton", "Test Person"),
        ]
    );
    assert_eq!(blame.runs[0].path, "run.go");
    assert_eq!(blame.runs[0].lines[3], "\tdial()");
    assert_eq!(blame.runs[0].email, "test@example.com");
    assert!(blame.runs[0].time.starts_with("2026-11-10T09:00:00"));
    // Blame before the last change: by the run's own pointer, or by commit.
    let previous = blame.runs[3].previous.clone().unwrap();
    assert_eq!(previous.commit, renamed);
    let before = s
        .git
        .blame(&repo, &previous.path, Some(&previous.commit))
        .await
        .unwrap();
    assert_eq!(before.lines, 7);
    assert_eq!(before.at.as_deref(), Some(renamed.as_str()));
    let same = s
        .git
        .blame_before(&repo, "supervisor.go", &last)
        .await
        .unwrap();
    assert_eq!(same.runs, before.runs);
    // Before the rename the file had another name.
    let old = blame.runs[2].previous.clone().unwrap();
    assert_eq!(old.path, "run.go");
    assert_eq!(
        s.git
            .blame(&repo, &old.path, Some(&old.commit))
            .await
            .unwrap()
            .lines,
        6
    );
    assert!(invalid(s.git.blame_before(&repo, "run.go", &first).await).contains("starts"));
    assert!(
        invalid(s.git.blame(&repo, "supervisor.go", Some(&first)).await)
            .contains("doesn’t exist at")
    );
    // Lines not committed yet say so.
    write(&repo, "supervisor.go", "package tunnel\n// new\n");
    let dirty = s.git.blame(&repo, "supervisor.go", None).await.unwrap();
    assert!(dirty.runs.iter().any(|r| r.uncommitted));

    let content = s.git.file_at(&repo, &first, "run.go").await.unwrap();
    assert!(content.text.starts_with("package tunnel\n") && !content.binary);
    assert!(
        s.git
            .file_at(&repo, &renamed, "logo.png")
            .await
            .unwrap()
            .binary
    );
    assert!(invalid(s.git.file_at(&repo, &first, "supervisor.go").await).contains("doesn’t exist"));
}
