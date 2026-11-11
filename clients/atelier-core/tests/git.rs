//! Git helpers on throwaway repositories. Nothing here touches the real
//! home folder, the real git config or the network.

mod common;

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use atelier_core::api::{GithubLink, Repo};
use atelier_core::git::{discover_repos, PublishOptions, PublishStep, RemoteRef, Transport};
use atelier_core::{Error, Git};
use serde_json::json;
use wiremock::matchers::{body_json, method, path};
use wiremock::{Mock, MockServer, ResponseTemplate};

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
        // Canonical, because git prints canonical paths (/private/var on macOS).
        self.dir.path().canonicalize().unwrap().join(name)
    }

    async fn g(&self, dir: &Path, args: &[&str]) -> String {
        self.git
            .run(Some(dir), args)
            .await
            .unwrap_or_else(|e| panic!("git {args:?}: {e}"))
    }

    async fn bare(&self, name: &str) -> PathBuf {
        let path = self.path(name);
        std::fs::create_dir_all(&path).unwrap();
        self.g(&path, &["init", "--quiet", "--bare", "-b", "main"])
            .await;
        path
    }

    async fn repo(&self, name: &str) -> PathBuf {
        let path = self.path(name);
        std::fs::create_dir_all(&path).unwrap();
        self.g(&path, &["init", "--quiet", "-b", "main"]).await;
        path
    }

    async fn commit(&self, repo: &Path, file: &str, text: &str) {
        std::fs::write(repo.join(file), text).unwrap();
        self.g(repo, &["add", "--", file]).await;
        self.g(
            repo,
            &["commit", "--quiet", "-m", &format!("change {file}")],
        )
        .await;
    }
}

/// Every file in a folder (outside `.git`) with its contents.
fn snapshot(dir: &Path) -> BTreeMap<PathBuf, Vec<u8>> {
    fn walk(root: &Path, dir: &Path, out: &mut BTreeMap<PathBuf, Vec<u8>>) {
        for entry in std::fs::read_dir(dir).unwrap().flatten() {
            let path = entry.path();
            if entry.file_name() == ".git" {
                continue;
            }
            if path.is_dir() {
                walk(root, &path, out);
            } else {
                out.insert(
                    path.strip_prefix(root).unwrap().to_path_buf(),
                    std::fs::read(&path).unwrap(),
                );
            }
        }
    }
    let mut out = BTreeMap::new();
    walk(dir, dir, &mut out);
    out
}

fn studio_repo(full_name: &str, https: &str, ssh: &str) -> Repo {
    let (owner, name) = full_name.split_once('/').unwrap();
    Repo {
        full_name: full_name.into(),
        owner: owner.into(),
        name: name.into(),
        clone_https: https.into(),
        clone_ssh: ssh.into(),
        ..Repo::default()
    }
}

// ----------------------------------------------------------- remote matching

#[test]
fn remote_urls_reduce_to_host_and_path() {
    let expect = |url: &str, host: &str, path: &str| {
        assert_eq!(
            RemoteRef::parse(url),
            Some(RemoteRef {
                host: host.into(),
                path: path.into()
            }),
            "{url}"
        );
    };
    // The contract's two clone forms.
    expect(
        "https://studio.example/DemiInfinity/Atelier.git",
        "studio.example",
        "demiinfinity/atelier",
    );
    expect(
        "ssh://git@studio.example:2223/DemiInfinity/Atelier.git",
        "studio.example",
        "demiinfinity/atelier",
    );
    // scp form, as in the designed `git push` output.
    expect(
        "git@atelier.infinityhomelab.lan:deminimmo/Atelier.git",
        "atelier.infinityhomelab.lan",
        "deminimmo/atelier",
    );
    expect("studio.example:Owner/Name", "studio.example", "owner/name");
    // Case, ports, credentials, trailing slashes, missing .git.
    expect(
        "HTTPS://Studio.Example:8443/Owner/Name/",
        "studio.example",
        "owner/name",
    );
    expect(
        "https://user:secret@studio.example/Owner/Name.git",
        "studio.example",
        "owner/name",
    );
    expect(
        "http://studio.example/Owner/Name.GIT",
        "studio.example",
        "owner/name",
    );
    expect(
        "ssh://studio.example/Owner/Name.git/",
        "studio.example",
        "owner/name",
    );
    expect(
        "git://studio.example/Owner/Name",
        "studio.example",
        "owner/name",
    );
    expect(
        "https://studio.example/forge/Owner/Name.git",
        "studio.example",
        "forge/owner/name",
    );
    expect(
        "ssh://git@[2001:db8::1]:2223/Owner/Name.git",
        "2001:db8::1",
        "owner/name",
    );

    for local in [
        "/Users/x/repos/thing.git",
        "../thing",
        "./a:b",
        "C:\\repos\\thing",
        "file:///Users/x/thing.git",
        "thing",
        "",
        "https://studio.example/",
    ] {
        assert_eq!(RemoteRef::parse(local), None, "{local}");
    }
}

#[test]
fn remotes_match_studio_repos_in_every_form() {
    let repos = vec![
        studio_repo(
            "DemiInfinity/Tray",
            "https://studio.example/DemiInfinity/Tray.git",
            "ssh://git@studio.example:2223/DemiInfinity/Tray.git",
        ),
        studio_repo(
            "DemiInfinity/Atelier",
            "https://studio.example/DemiInfinity/Atelier.git",
            "ssh://git@studio.example:2223/DemiInfinity/Atelier.git",
        ),
        Repo {
            github: Some(GithubLink {
                repo: "elyndra/Sparky-Ventures".into(),
                home: "github".into(),
            }),
            ..studio_repo(
                "elyndrastudios/sparky-ventures",
                "https://studio.example/elyndrastudios/sparky-ventures.git",
                "ssh://git@studio.example:2223/elyndrastudios/sparky-ventures.git",
            )
        },
    ];
    let matched = |url: &str| {
        RemoteRef::parse(url).and_then(|r| r.match_repo(&repos).map(|repo| repo.full_name.clone()))
    };
    for url in [
        "https://studio.example/DemiInfinity/Atelier.git",
        "https://studio.example/DemiInfinity/Atelier",
        "https://STUDIO.example/demiinfinity/ATELIER.git",
        "ssh://git@studio.example:2223/DemiInfinity/Atelier.git",
        "ssh://git@studio.example/demiinfinity/atelier",
        "git@studio.example:DemiInfinity/Atelier.git",
        "git@Studio.Example:demiinfinity/atelier",
    ] {
        assert_eq!(
            matched(url).as_deref(),
            Some("DemiInfinity/Atelier"),
            "{url}"
        );
    }
    // Another host or another path is another repository.
    assert_eq!(
        matched("https://elsewhere.example/DemiInfinity/Atelier.git"),
        None
    );
    assert_eq!(
        matched("https://studio.example/DemiInfinity/Atelier2.git"),
        None
    );
    assert_eq!(matched("https://studio.example/Atelier.git"), None);
    // A GitHub remote matches the studio repository tethered to it.
    assert_eq!(
        matched("git@github.com:Elyndra/sparky-ventures.git").as_deref(),
        Some("elyndrastudios/sparky-ventures")
    );
    assert_eq!(matched("https://github.com/someone/else.git"), None);
}

// ------------------------------------------------------------ local state

#[tokio::test]
async fn ahead_behind_changed_files_and_branch() {
    let sandbox = Sandbox::new();
    let remote = sandbox.bare("remote.git").await;
    let work = sandbox.repo("work").await;
    sandbox.commit(&work, "a.txt", "one").await;
    let url = "ssh://git@studio.example:2223/DemiInfinity/Atelier.git";

    // No remote, no upstream.
    let state = sandbox.git.inspect(&work).await.unwrap();
    assert_eq!(state.branch.as_deref(), Some("main"));
    assert_eq!(
        (state.upstream.as_deref(), state.ahead, state.behind),
        (None, None, None)
    );
    assert_eq!(state.changed_files, 0);
    assert_eq!((state.remote_url.as_deref(), &state.remote), (None, &None));

    // Push with an upstream: level.
    sandbox
        .g(
            &work,
            &["remote", "add", "origin", remote.to_str().unwrap()],
        )
        .await;
    sandbox.git.push(&work, "origin", true).await.unwrap();
    let state = sandbox.git.inspect(&work).await.unwrap();
    assert_eq!(state.upstream.as_deref(), Some("origin/main"));
    assert_eq!((state.ahead, state.behind), (Some(0), Some(0)));
    assert_eq!(state.remote_name.as_deref(), Some("origin"));

    // Two local commits: 2 ahead.
    sandbox.commit(&work, "b.txt", "two").await;
    sandbox.commit(&work, "c.txt", "three").await;
    let state = sandbox.git.inspect(&work).await.unwrap();
    assert_eq!((state.ahead, state.behind), (Some(2), Some(0)));

    // Someone else pushes one commit; after a fetch we're 2 ahead, 1 behind.
    let other = sandbox.path("other");
    sandbox
        .git
        .clone_repo(remote.to_str().unwrap(), &other)
        .await
        .unwrap();
    sandbox.commit(&other, "d.txt", "four").await;
    sandbox.git.push(&other, "origin", false).await.unwrap();
    sandbox.git.fetch(&work, None).await.unwrap();
    let state = sandbox.git.inspect(&work).await.unwrap();
    assert_eq!((state.ahead, state.behind), (Some(2), Some(1)));

    // Changed files: one modified, one staged, one untracked.
    std::fs::write(work.join("a.txt"), "edited").unwrap();
    std::fs::write(work.join("staged.txt"), "s").unwrap();
    sandbox.g(&work, &["add", "staged.txt"]).await;
    std::fs::write(work.join("untracked.txt"), "u").unwrap();
    let state = sandbox.git.inspect(&work).await.unwrap();
    assert_eq!(state.changed_files, 3);

    // The remote's address is matched to the studio by host and path.
    let previous = sandbox.git.set_remote(&work, "origin", url).await.unwrap();
    assert_eq!(previous.as_deref(), remote.to_str());
    let state = sandbox.git.inspect(&work).await.unwrap();
    assert_eq!(state.remote_url.as_deref(), Some(url));
    let repos = [studio_repo(
        "DemiInfinity/Atelier",
        "https://studio.example/DemiInfinity/Atelier.git",
        url,
    )];
    assert_eq!(
        state.studio_repo(&repos).unwrap().full_name,
        "DemiInfinity/Atelier"
    );

    // Detached HEAD has no branch.
    sandbox.g(&work, &["checkout", "--quiet", "--detach"]).await;
    let state = sandbox.git.inspect(&work).await.unwrap();
    assert_eq!(
        (state.branch, state.upstream, state.ahead),
        (None, None, None)
    );
}

#[tokio::test]
async fn inspecting_changes_nothing() {
    let sandbox = Sandbox::new();
    let work = sandbox.repo("work").await;
    sandbox.commit(&work, "a.txt", "one").await;
    std::fs::write(work.join("a.txt"), "edited").unwrap();
    let files = snapshot(&work);
    let index = std::fs::metadata(work.join(".git/index"))
        .unwrap()
        .modified()
        .unwrap();
    std::thread::sleep(std::time::Duration::from_millis(1100));
    sandbox.git.inspect(&work).await.unwrap();
    sandbox.git.toplevel(&work).await.unwrap();
    assert_eq!(snapshot(&work), files);
    // Not even the index was refreshed.
    assert_eq!(
        std::fs::metadata(work.join(".git/index"))
            .unwrap()
            .modified()
            .unwrap(),
        index
    );
}

#[tokio::test]
async fn folders_that_are_not_repositories() {
    let sandbox = Sandbox::new();
    let plain = sandbox.path("plain");
    std::fs::create_dir_all(&plain).unwrap();
    assert_eq!(sandbox.git.toplevel(&plain).await.unwrap(), None);
    assert_eq!(
        sandbox.git.toplevel(&plain.join("missing")).await.unwrap(),
        None
    );
    assert!(matches!(
        sandbox.git.inspect(&plain).await.unwrap_err(),
        Error::NotARepo(_)
    ));
    assert!(matches!(
        sandbox.git.publish_plan(&plain).await.unwrap_err(),
        Error::NotARepo(_)
    ));

    let work = sandbox.repo("work").await;
    std::fs::create_dir_all(work.join("deep/er")).unwrap();
    assert_eq!(
        sandbox.git.toplevel(&work.join("deep/er")).await.unwrap(),
        Some(work.clone())
    );
    // A fresh repository without commits still has a branch.
    let state = sandbox.git.inspect(&work).await.unwrap();
    assert_eq!(state.branch.as_deref(), Some("main"));
}

#[tokio::test]
async fn a_missing_git_is_said_plainly() {
    let git = Git::new().with_program("/nonexistent/git-binary");
    let err = git.run(None, ["--version"]).await.unwrap_err();
    assert!(matches!(err, Error::MissingProgram(_)));
    assert_eq!(
        err.to_string(),
        "git isn’t installed or isn’t on your PATH."
    );
}

#[tokio::test]
async fn discovery_is_bounded_and_skips_what_it_should() {
    let sandbox = Sandbox::new();
    let root = sandbox.path("root");
    let mkrepo = |relative: &str| {
        let dir = root.join(relative);
        std::fs::create_dir_all(dir.join(".git")).unwrap();
        dir
    };
    let a = mkrepo("Owner/A");
    let b = mkrepo("B");
    let deep = mkrepo("x/y/z/Deep");
    mkrepo("B/nested/Inner"); // inside another working copy
    mkrepo(".hidden/H");
    mkrepo("node_modules/pkg");
    // A worktree-style ".git" file counts too.
    let linked = root.join("Linked");
    std::fs::create_dir_all(&linked).unwrap();
    std::fs::write(linked.join(".git"), "gitdir: /elsewhere").unwrap();
    // A symlink to a repository is not followed.
    #[cfg(unix)]
    std::os::unix::fs::symlink(&a, root.join("link-to-a")).unwrap();

    assert_eq!(
        discover_repos(std::slice::from_ref(&root), 2),
        [b.clone(), linked.clone(), a.clone()]
    );
    assert_eq!(
        discover_repos(std::slice::from_ref(&root), 4),
        [b.clone(), linked.clone(), a.clone(), deep]
    );
    assert_eq!(
        discover_repos(std::slice::from_ref(&root), 1),
        [b.clone(), linked]
    );
    assert!(discover_repos(std::slice::from_ref(&root), 0).is_empty());
    // A root that is itself a repository, a missing root, and duplicates.
    assert_eq!(
        discover_repos(&[a.clone(), root.join("missing"), a.clone()], 3),
        [a]
    );
}

#[tokio::test]
async fn scan_inspects_what_it_finds() {
    let sandbox = Sandbox::new();
    let root = sandbox.path("root");
    for name in ["one", "two", "three"] {
        let repo = sandbox.repo(&format!("root/{name}")).await;
        sandbox.commit(&repo, "f", name).await;
    }
    std::fs::write(root.join("two/extra"), "x").unwrap();
    // A folder with a broken .git is skipped, not fatal.
    std::fs::create_dir_all(root.join("broken")).unwrap();
    std::fs::write(root.join("broken/.git"), "gitdir: /nowhere").unwrap();

    let found = sandbox.git.scan(&[root], 3).await;
    let names: Vec<String> = found.iter().map(|r| r.folder_name()).collect();
    assert_eq!(names, ["one", "three", "two"]);
    assert_eq!(found[2].changed_files, 1);
}

// ----------------------------------------------------------------- publish

fn created_repo(clone_ssh: &Path) -> serde_json::Value {
    json!({"full_name":"DemiInfinity/Ingle","owner":"DemiInfinity","owner_kind":"user","name":"Ingle",
           "private":true,"description":"","default_branch":"main",
           "clone_https":"https://studio.example/DemiInfinity/Ingle.git",
           "clone_ssh": clone_ssh, "github":null,"open_pulls":0,"updated_at":"2026-11-11T09:00:00Z"})
}

fn options(path: &Path) -> PublishOptions {
    PublishOptions {
        path: path.to_path_buf(),
        owner: "DemiInfinity".into(),
        name: "Ingle".into(),
        private: true,
        description: String::new(),
        transport: Transport::Ssh,
        replace_origin: false,
    }
}

#[tokio::test]
async fn publish_creates_the_repo_and_pushes_branches_and_tags() {
    let sandbox = Sandbox::new();
    let studio_side = sandbox.bare("studio/DemiInfinity/Ingle.git").await;
    let work = sandbox.repo("Ingle").await;
    sandbox.commit(&work, "README.md", "# Ingle").await;
    sandbox.commit(&work, "src.txt", "code").await;
    sandbox.g(&work, &["tag", "v0.1.0"]).await;
    sandbox.g(&work, &["branch", "feature/fire"]).await;
    std::fs::write(work.join("scratch.txt"), "uncommitted").unwrap();
    let before = snapshot(&work);

    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/repos"))
        .and(body_json(
            json!({"owner":"DemiInfinity","name":"Ingle","private":true,"description":""}),
        ))
        .respond_with(ResponseTemplate::new(201).set_body_json(created_repo(&studio_side)))
        .expect(1)
        .mount(&server)
        .await;
    let client = common::client(&server);

    // Called from a subfolder: the whole working copy is published.
    std::fs::create_dir_all(work.join("sub")).unwrap();
    let mut steps = Vec::new();
    let published = sandbox
        .git
        .publish_folder(&client, &options(&work.join("sub")), &mut |s| steps.push(s))
        .await
        .unwrap();

    assert_eq!(
        steps,
        [
            PublishStep::Creating {
                full_name: "DemiInfinity/Ingle".into()
            },
            PublishStep::Created,
            PublishStep::Pushing {
                branch: Some("main".into()),
                commits: 2
            },
            PublishStep::Pushed
        ]
    );
    assert_eq!(published.repo.full_name, "DemiInfinity/Ingle");
    assert_eq!(
        published.web_url,
        format!("{}/DemiInfinity/Ingle", server.uri())
    );
    assert_eq!(published.path, work);
    assert_eq!(
        (published.branch.as_deref(), published.commits),
        (Some("main"), 2)
    );
    assert_eq!(published.origin, studio_side.to_str().unwrap());
    assert_eq!(published.previous_origin, None);

    // The studio side has every branch and tag, at the same commits.
    let local_refs = sandbox.g(&work, &["show-ref", "--heads", "--tags"]).await;
    let studio_refs = sandbox
        .g(&studio_side, &["show-ref", "--heads", "--tags"])
        .await;
    assert_eq!(studio_refs, local_refs);
    assert!(studio_refs.contains("refs/heads/main"));
    assert!(studio_refs.contains("refs/heads/feature/fire"));
    assert!(studio_refs.contains("refs/tags/v0.1.0"));

    // origin points at the studio and main tracks it.
    let state = sandbox.git.inspect(&work).await.unwrap();
    assert_eq!(state.remote_url.as_deref(), studio_side.to_str());
    assert_eq!(state.upstream.as_deref(), Some("origin/main"));
    assert_eq!((state.ahead, state.behind), (Some(0), Some(0)));

    // The folder's files stayed exactly as they were.
    assert_eq!(snapshot(&work), before);
}

#[tokio::test]
async fn publish_never_replaces_origin_without_a_yes() {
    let sandbox = Sandbox::new();
    let studio_side = sandbox.bare("studio.git").await;
    let work = sandbox.repo("Ingle").await;
    sandbox.commit(&work, "README.md", "# Ingle").await;
    let elsewhere = "https://github.com/someone/Ingle.git";
    sandbox
        .g(&work, &["remote", "add", "origin", elsewhere])
        .await;

    let server = MockServer::start().await;
    let client = common::client(&server);
    let err = sandbox
        .git
        .publish_folder(&client, &options(&work), &mut |_| {})
        .await
        .unwrap_err();
    assert!(
        matches!(&err, Error::OriginExists { url } if url == elsewhere),
        "{err:?}"
    );
    // Refused before anything was created or changed.
    assert!(server.received_requests().await.unwrap().is_empty());
    assert_eq!(
        sandbox
            .git
            .remote_url(&work, "origin")
            .await
            .unwrap()
            .as_deref(),
        Some(elsewhere)
    );
    assert_eq!(
        sandbox
            .git
            .publish_plan(&work)
            .await
            .unwrap()
            .origin
            .as_deref(),
        Some(elsewhere)
    );

    // With the person's say-so it is replaced, and the old address reported.
    Mock::given(method("POST"))
        .and(path("/api/app/v1/repos"))
        .respond_with(ResponseTemplate::new(201).set_body_json(created_repo(&studio_side)))
        .mount(&server)
        .await;
    let published = sandbox
        .git
        .publish_folder(
            &client,
            &PublishOptions {
                replace_origin: true,
                ..options(&work)
            },
            &mut |_| {},
        )
        .await
        .unwrap();
    assert_eq!(published.previous_origin.as_deref(), Some(elsewhere));
    let state = sandbox.git.inspect(&work).await.unwrap();
    assert_eq!(state.remote_url.as_deref(), studio_side.to_str());
    assert_eq!(state.upstream.as_deref(), Some("origin/main"));
}

#[tokio::test]
async fn publish_refusals_leave_the_folder_alone() {
    let sandbox = Sandbox::new();
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/repos"))
        .respond_with(
            ResponseTemplate::new(409)
                .set_body_raw(r#"{"error":"name_taken"}"#, "application/json"),
        )
        .mount(&server)
        .await;
    let client = common::client(&server);

    // Nothing committed yet: said before the API is called.
    let empty = sandbox.repo("Empty").await;
    let err = sandbox
        .git
        .publish_folder(&client, &options(&empty), &mut |_| {})
        .await
        .unwrap_err();
    assert!(matches!(err, Error::Invalid(_)), "{err:?}");
    assert!(server.received_requests().await.unwrap().is_empty());

    // The name is taken: no origin is added.
    let work = sandbox.repo("Ingle").await;
    sandbox.commit(&work, "README.md", "# Ingle").await;
    let err = sandbox
        .git
        .publish_folder(&client, &options(&work), &mut |_| {})
        .await
        .unwrap_err();
    assert_eq!(err.code(), Some("name_taken"));
    assert_eq!(sandbox.git.remote_url(&work, "origin").await.unwrap(), None);
}

#[tokio::test]
async fn the_device_key_is_remembered_for_ssh_remotes() {
    let sandbox = Sandbox::new();
    let key = sandbox.path("home").join(".ssh/atelier_device_ed25519");
    let git = sandbox.git.clone().with_ssh_identity(&key);
    let work = sandbox.repo("Ingle").await;
    sandbox.commit(&work, "README.md", "# Ingle").await;

    let server = MockServer::start().await;
    // An SSH address nothing answers on: the push fails, which is fine here.
    Mock::given(method("POST"))
        .and(path("/api/app/v1/repos"))
        .respond_with(ResponseTemplate::new(201).set_body_json(json!({
            "full_name":"DemiInfinity/Ingle",
            "clone_ssh":"ssh://git@127.0.0.1:1/DemiInfinity/Ingle.git"
        })))
        .mount(&server)
        .await;
    let err = git
        .publish_folder(&common::client(&server), &options(&work), &mut |_| {})
        .await
        .unwrap_err();
    assert!(matches!(err, Error::Git { .. }), "{err:?}");
    assert_eq!(
        sandbox.g(&work, &["config", "core.sshCommand"]).await,
        format!("ssh -i '{}'", key.display())
    );
    // Local remotes don't get it.
    let other = sandbox.repo("Other").await;
    sandbox.commit(&other, "f", "x").await;
    let bare = sandbox.bare("other.git").await;
    let clone = sandbox.path("clone");
    git.push(&other, "origin", false).await.unwrap_err();
    git.set_remote(&other, "origin", bare.to_str().unwrap())
        .await
        .unwrap();
    git.push(&other, "origin", true).await.unwrap();
    git.clone_repo(bare.to_str().unwrap(), &clone)
        .await
        .unwrap();
    assert!(git
        .run(Some(&clone), ["config", "core.sshCommand"])
        .await
        .is_err());
    assert_eq!(std::fs::read_to_string(clone.join("f")).unwrap(), "x");
}
