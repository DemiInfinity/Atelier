//! Config, tokens, SSH keys and friendly messages. Everything lives in
//! temporary folders with an injected environment.

use std::path::{Path, PathBuf};

use atelier_core::api::{Messages, Tone};
use atelier_core::config::{expand_tilde, normalize_studio, Config, Env, Settings};
use atelier_core::messages::{fill, pick, validate_yours, yours_with, yours_without, Vars};
use atelier_core::token::{
    default_store, FallbackStore, FileStore, MemoryStore, Place, Token, TokenStore,
};
use atelier_core::{ssh, Client, Error};

// ------------------------------------------------------------------ config

/// The contract's example file, verbatim.
const EXAMPLE: &str = r#"studio = "https://atelier.infinityhomelab.lan"
device_id = 7
device_name = "MacBook"
clone_root = "~/Atelier"
watch = ["~/Projects"]
editor = "vscode"
git_tool = "fork"
runner_config = "~/Library/Application Support/atelier-runner/config.json"
"#;

fn settings(dir: &Path, vars: &[(&str, &str)]) -> Settings {
    let mut all = vec![
        ("ATELIER_CONFIG_DIR", dir.to_str().unwrap().to_string()),
        ("HOME", "/Users/x".to_string()),
    ];
    all.extend(vars.iter().map(|(k, v)| (*k, v.to_string())));
    Settings::load(Env::fixed(all)).unwrap()
}

#[test]
fn config_reads_the_contract_example() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("config.toml"), EXAMPLE).unwrap();
    let settings = settings(dir.path(), &[]);
    let config = &settings.config;
    assert_eq!(
        config.studio.as_deref(),
        Some("https://atelier.infinityhomelab.lan")
    );
    assert_eq!(config.device_id, Some(7));
    assert_eq!(config.device_name.as_deref(), Some("MacBook"));
    assert_eq!(config.editor.as_deref(), Some("vscode"));
    assert_eq!(config.git_tool.as_deref(), Some("fork"));

    assert_eq!(
        settings.studio().as_deref(),
        Some("https://atelier.infinityhomelab.lan")
    );
    assert_eq!(settings.clone_root(), PathBuf::from("/Users/x/Atelier"));
    assert_eq!(settings.watch_dirs(), [PathBuf::from("/Users/x/Projects")]);
    assert_eq!(
        settings.repo_roots(),
        [
            PathBuf::from("/Users/x/Atelier"),
            PathBuf::from("/Users/x/Projects")
        ]
    );
    assert_eq!(
        settings.runner_config_path(),
        PathBuf::from("/Users/x/Library/Application Support/atelier-runner/config.json")
    );
    assert_eq!(
        settings.runner_socket(),
        PathBuf::from("/Users/x/Library/Application Support/atelier-runner/control.sock")
    );
    assert_eq!(settings.ssh_dir(), PathBuf::from("/Users/x/.ssh"));
    assert!(settings.ca_files().is_empty());
    assert!(settings.token_override().is_none());
}

#[test]
fn config_round_trips_and_keeps_keys_it_does_not_know() {
    let dir = tempfile::tempdir().unwrap();
    let text = format!("{EXAMPLE}ca_file = \"/etc/rootCA.pem\"\nfuture_key = \"kept\"\n\n[desktop]\ntheme = \"dark\"\n");
    std::fs::write(dir.path().join("config.toml"), &text).unwrap();
    let loaded = Config::load(dir.path()).unwrap();
    assert_eq!(loaded.ca_file.as_deref(), Some("/etc/rootCA.pem"));

    let elsewhere = dir.path().join("made/on/save");
    loaded.save(&elsewhere).unwrap();
    let again = Config::load(&elsewhere).unwrap();
    assert_eq!(again, loaded);
    let written = std::fs::read_to_string(elsewhere.join("config.toml")).unwrap();
    assert!(written.contains("future_key = \"kept\""), "{written}");
    assert!(written.contains("theme = \"dark\""), "{written}");
    // No token ever lands in the file, and no stray temp file is left.
    assert!(!written.contains("token"));
    assert!(!elsewhere.join("config.toml.tmp").exists());

    // Changing one thing keeps the rest.
    let mut changed = again.clone();
    changed.editor = Some("cursor".into());
    changed.save(&elsewhere).unwrap();
    let reread = Config::load(&elsewhere).unwrap();
    assert_eq!(reread.editor.as_deref(), Some("cursor"));
    assert_eq!(reread.watch, ["~/Projects"]);
}

#[test]
fn a_missing_config_is_empty_and_a_broken_one_says_so() {
    let dir = tempfile::tempdir().unwrap();
    let settings = settings(dir.path(), &[]);
    assert_eq!(settings.config, Config::default());
    assert_eq!(settings.studio(), None);
    assert_eq!(settings.clone_root(), PathBuf::from("/Users/x/Atelier"));
    assert_eq!(settings.repo_roots(), [PathBuf::from("/Users/x/Atelier")]);
    // The runner's config defaults to a sibling of our config folder.
    assert_eq!(
        settings.runner_config_path(),
        dir.path()
            .parent()
            .unwrap()
            .join("atelier-runner/config.json")
    );
    // An empty config saves as an empty file.
    Config::default().save(dir.path()).unwrap();
    assert_eq!(
        std::fs::read_to_string(dir.path().join("config.toml")).unwrap(),
        ""
    );

    std::fs::write(dir.path().join("config.toml"), "studio = [not toml").unwrap();
    let err = Config::load(dir.path()).unwrap_err();
    assert!(matches!(err, Error::Config(_)));
    assert!(err.to_string().contains("config.toml"));
}

#[test]
fn environment_overrides() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("config.toml"), EXAMPLE).unwrap();
    let settings = settings(
        dir.path(),
        &[
            ("ATELIER_STUDIO", "other.example/"),
            ("ATELIER_TOKEN", " atd_fromenv \n"),
            ("SSL_CERT_FILE", "~/ca/root.pem"),
            ("ATELIER_SSH_DIR", "/tmp/keys"),
            ("ATELIER_RUNNER_SOCKET", "/tmp/r.sock"),
        ],
    );
    assert_eq!(settings.studio().as_deref(), Some("https://other.example"));
    assert_eq!(settings.token_override().unwrap().expose(), "atd_fromenv");
    assert_eq!(settings.ca_files(), [PathBuf::from("/Users/x/ca/root.pem")]);
    assert_eq!(settings.ssh_dir(), PathBuf::from("/tmp/keys"));
    assert_eq!(settings.runner_socket(), PathBuf::from("/tmp/r.sock"));
    // The file itself is untouched by overrides.
    assert_eq!(
        settings.config.studio.as_deref(),
        Some("https://atelier.infinityhomelab.lan")
    );

    // Empty variables count as unset.
    let settings = self::settings(
        dir.path(),
        &[("ATELIER_STUDIO", ""), ("ATELIER_TOKEN", " ")],
    );
    assert_eq!(
        settings.studio().as_deref(),
        Some("https://atelier.infinityhomelab.lan")
    );
    assert!(settings.token_override().is_none());
}

#[test]
fn ca_file_and_ssl_cert_file_are_both_honoured() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(
        dir.path().join("config.toml"),
        "ca_file = \"~/rootCA.pem\"\n",
    )
    .unwrap();
    let both = settings(dir.path(), &[("SSL_CERT_FILE", "/etc/ssl/extra.pem")]);
    assert_eq!(
        both.ca_files(),
        [
            PathBuf::from("/Users/x/rootCA.pem"),
            PathBuf::from("/etc/ssl/extra.pem")
        ]
    );
    let same = settings(dir.path(), &[("SSL_CERT_FILE", "/Users/x/rootCA.pem")]);
    assert_eq!(same.ca_files().len(), 1);
}

#[test]
fn the_default_config_folder_is_the_per_os_one() {
    let dir = atelier_core::config::config_dir(&Env::fixed([("HOME", "/Users/x")])).unwrap();
    assert!(dir.ends_with("atelier"), "{}", dir.display());
    #[cfg(target_os = "macos")]
    assert!(
        dir.ends_with("Library/Application Support/atelier"),
        "{}",
        dir.display()
    );
    assert_eq!(
        atelier_core::config::config_dir(&Env::fixed([("ATELIER_CONFIG_DIR", "/x/y")])).unwrap(),
        PathBuf::from("/x/y")
    );
}

#[test]
fn tilde_and_studio_normalising() {
    let home = Path::new("/Users/x");
    assert_eq!(expand_tilde("~", home), home);
    assert_eq!(expand_tilde("~/Atelier", home), home.join("Atelier"));
    assert_eq!(expand_tilde("/abs/path", home), Path::new("/abs/path"));
    assert_eq!(expand_tilde("rel/~/path", home), Path::new("rel/~/path"));
    assert_eq!(expand_tilde("~other/x", home), Path::new("~other/x"));

    assert_eq!(
        normalize_studio("atelier.infinityhomelab.lan"),
        "https://atelier.infinityhomelab.lan"
    );
    assert_eq!(
        normalize_studio(" https://studio.example/ "),
        "https://studio.example"
    );
    assert_eq!(
        normalize_studio("http://localhost:8080"),
        "http://localhost:8080"
    );
}

// ------------------------------------------------------------------ client

#[test]
fn client_from_settings() {
    let dir = tempfile::tempdir().unwrap();
    let store = MemoryStore::default();

    let nothing = settings(dir.path(), &[]);
    assert!(matches!(
        Client::from_settings(&nothing, &store).unwrap_err(),
        Error::NoStudio
    ));

    std::fs::write(dir.path().join("config.toml"), EXAMPLE).unwrap();
    let configured = settings(dir.path(), &[]);
    assert!(matches!(
        Client::from_settings(&configured, &store).unwrap_err(),
        Error::NotSignedIn
    ));

    store
        .set(
            "https://atelier.infinityhomelab.lan",
            &Token::new("atd_stored"),
        )
        .unwrap();
    let client = Client::from_settings(&configured, &store).unwrap();
    assert_eq!(client.studio_url(), "https://atelier.infinityhomelab.lan");
    assert_eq!(client.studio_host(), "atelier.infinityhomelab.lan");
    assert_eq!(
        client.web_url("/DemiInfinity/Atelier/pulls/14"),
        "https://atelier.infinityhomelab.lan/DemiInfinity/Atelier/pulls/14"
    );
    assert!(!format!("{client:?}").contains("atd_stored"));

    // ATELIER_TOKEN needs no stored token; ATELIER_STUDIO needs no file.
    let scripted = settings(
        tempfile::tempdir().unwrap().path(),
        &[
            ("ATELIER_STUDIO", "ci.example"),
            ("ATELIER_TOKEN", "atd_ci"),
        ],
    );
    let client = Client::from_settings(&scripted, &MemoryStore::default()).unwrap();
    assert_eq!(client.studio_url(), "https://ci.example");
}

#[test]
fn bad_studio_addresses_and_ca_files() {
    let options = atelier_core::ClientOptions::default;
    for bad in ["", "ftp://studio.example", "https://"] {
        assert!(
            matches!(Client::new(bad, None, options()), Err(Error::Config(_))),
            "{bad:?}"
        );
    }
    let dir = tempfile::tempdir().unwrap();
    let missing = atelier_core::ClientOptions {
        ca_files: vec![dir.path().join("missing.pem")],
        ..options()
    };
    assert!(matches!(
        Client::new("studio.example", None, missing),
        Err(Error::Io { .. })
    ));
    let not_pem = dir.path().join("not.pem");
    std::fs::write(&not_pem, "hello").unwrap();
    let wrong = atelier_core::ClientOptions {
        ca_files: vec![not_pem],
        ..options()
    };
    let err = Client::new("studio.example", None, wrong).unwrap_err();
    assert!(matches!(err, Error::Config(_)), "{err:?}");
}

#[test]
fn a_private_ca_bundle_is_accepted() {
    // Make a throwaway root with the system's openssl; skip where there is none.
    let dir = tempfile::tempdir().unwrap();
    let pem = dir.path().join("rootCA.pem");
    let made = std::process::Command::new("openssl")
        .args([
            "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
        ])
        .args(["-subj", "/CN=atelier test root", "-keyout"])
        .arg(dir.path().join("key.pem"))
        .arg("-out")
        .arg(&pem)
        .output();
    if !made.is_ok_and(|o| o.status.success()) {
        eprintln!("openssl not available; skipping");
        return;
    }
    std::fs::write(
        dir.path().join("config.toml"),
        "ca_file = \"~/rootCA.pem\"\n",
    )
    .unwrap();
    let home = dir.path().to_str().unwrap();
    let settings = Settings::load(Env::fixed([
        ("ATELIER_CONFIG_DIR", home),
        ("HOME", home),
        ("ATELIER_STUDIO", "studio.example"),
        ("ATELIER_TOKEN", "atd_x"),
    ]))
    .unwrap();
    assert_eq!(settings.ca_files(), [pem]);
    Client::from_settings(&settings, &MemoryStore::default()).unwrap();
}

// ------------------------------------------------------------------ tokens

#[test]
fn tokens_are_never_shown() {
    let secret = "atd_0123456789abcdefghijABCDEFGHIJ0123456789";
    let token = Token::new(secret);
    assert_eq!(format!("{token:?}"), "Token(REDACTED)");
    assert_eq!(format!("{token}"), "REDACTED");
    assert_eq!(
        format!("{:#?}", Some(&token)),
        "Some(\n    Token(REDACTED),\n)"
    );
    assert_eq!(token.expose(), secret);

    // Inside the things that carry one.
    let client = Client::new("studio.example", Some(token.clone()), Default::default()).unwrap();
    assert!(!format!("{client:?}").contains(secret));
    let grant: atelier_core::api::DeviceGrant =
        serde_json::from_str(&format!(r#"{{"token":"{secret}","device_id":7}}"#)).unwrap();
    assert_eq!(grant.token, token);
    assert!(!format!("{grant:?}").contains(secret));
    let settings = settings(
        tempfile::tempdir().unwrap().path(),
        &[("ATELIER_TOKEN", secret)],
    );
    assert!(!format!("{:?}", settings.token_override()).contains(secret));
}

#[test]
fn the_token_file_is_private_and_per_studio() {
    let dir = tempfile::tempdir().unwrap();
    let store = FileStore::in_dir(&dir.path().join("atelier"));
    let home = "https://studio.example";
    assert_eq!(store.get(home).unwrap(), None);
    store.delete(home).unwrap(); // nothing to delete is fine

    let place = store.set(home, &Token::new("atd_one")).unwrap();
    assert_eq!(place, Place::File(store.path().to_path_buf()));
    store
        .set("https://other.example", &Token::new("atd_two"))
        .unwrap();
    assert_eq!(store.get(home).unwrap().unwrap().expose(), "atd_one");
    assert_eq!(
        store
            .get("https://other.example")
            .unwrap()
            .unwrap()
            .expose(),
        "atd_two"
    );
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let mode = std::fs::metadata(store.path())
            .unwrap()
            .permissions()
            .mode();
        assert_eq!(mode & 0o777, 0o600);
        // Replacing a token keeps it private even if someone loosened it.
        std::fs::set_permissions(store.path(), std::fs::Permissions::from_mode(0o644)).unwrap();
        store.set(home, &Token::new("atd_three")).unwrap();
        let mode = std::fs::metadata(store.path())
            .unwrap()
            .permissions()
            .mode();
        assert_eq!(mode & 0o777, 0o600);
    }
    assert_eq!(store.get(home).unwrap().unwrap().expose(), "atd_three");

    store.delete(home).unwrap();
    assert_eq!(store.get(home).unwrap(), None);
    assert!(store.get("https://other.example").unwrap().is_some());
    // The last token gone, the file goes too.
    store.delete("https://other.example").unwrap();
    assert!(!store.path().exists());
}

/// A keychain that isn't there (a headless box).
struct NoKeychain;
impl TokenStore for NoKeychain {
    fn get(&self, _: &str) -> atelier_core::Result<Option<Token>> {
        Err(Error::Keychain("no keychain".into()))
    }
    fn set(&self, _: &str, _: &Token) -> atelier_core::Result<Place> {
        Err(Error::Keychain("no keychain".into()))
    }
    fn delete(&self, _: &str) -> atelier_core::Result<()> {
        Err(Error::Keychain("no keychain".into()))
    }
}

#[test]
fn the_file_is_used_only_when_there_is_no_keychain() {
    let dir = tempfile::tempdir().unwrap();
    let studio = "https://studio.example";
    let token = Token::new("atd_one");

    // A working keychain (a stand-in): the file is never written.
    let store = FallbackStore::new(
        Box::new(MemoryStore::default()),
        FileStore::in_dir(dir.path()),
    );
    assert_eq!(store.set(studio, &token).unwrap(), Place::Memory);
    assert!(!dir.path().join("tokens.json").exists());
    assert_eq!(store.get(studio).unwrap(), Some(token.clone()));
    store.delete(studio).unwrap();
    assert_eq!(store.get(studio).unwrap(), None);

    // No keychain: the file steps in, and says so through `Place`.
    let store = FallbackStore::new(Box::new(NoKeychain), FileStore::in_dir(dir.path()));
    assert_eq!(
        store.set(studio, &token).unwrap(),
        Place::File(dir.path().join("tokens.json"))
    );
    assert_eq!(store.get(studio).unwrap(), Some(token.clone()));
    store.delete(studio).unwrap();
    assert_eq!(store.get(studio).unwrap(), None);

    // A token left in the file by a headless sign-in is still found later,
    // and cleaned up once the keychain takes over.
    FileStore::in_dir(dir.path()).set(studio, &token).unwrap();
    let store = FallbackStore::new(
        Box::new(MemoryStore::default()),
        FileStore::in_dir(dir.path()),
    );
    assert_eq!(store.get(studio).unwrap(), Some(token.clone()));
    store.set(studio, &token).unwrap();
    assert!(!dir.path().join("tokens.json").exists());
}

#[test]
fn atelier_token_store_file_never_touches_the_keychain() {
    let dir = tempfile::tempdir().unwrap();
    let store = default_store(dir.path(), &Env::fixed([("ATELIER_TOKEN_STORE", "file")]));
    let place = store
        .set("https://studio.example", &Token::new("atd_x"))
        .unwrap();
    assert_eq!(place, Place::File(dir.path().join("tokens.json")));
}

// ---------------------------------------------------------------- SSH keys

const PUBLIC: &str =
    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEexampleexampleexampleexampleexampleexample me@mac";

#[tokio::test]
async fn an_existing_key_is_used_as_it_is() {
    let dir = tempfile::tempdir().unwrap();
    assert_eq!(ssh::find_public_key(dir.path()), None);
    std::fs::write(dir.path().join("id_rsa.pub"), "ssh-rsa AAAA old").unwrap();
    assert_eq!(ssh::find_public_key(dir.path()), None);
    std::fs::write(dir.path().join("id_ed25519.pub"), format!("{PUBLIC}\n")).unwrap();

    let (key, generated) = ssh::ensure_public_key(dir.path(), "MacBook (atl)")
        .await
        .unwrap();
    assert!(!generated);
    assert_eq!(key.text, PUBLIC);
    assert_eq!(key.path, dir.path().join("id_ed25519.pub"));
    assert_eq!(key.private_path(), dir.path().join("id_ed25519"));
    assert!(!key.is_dedicated());
    assert_eq!(ssh::dedicated_identity(dir.path()), None);
    // Nothing new was written.
    assert_eq!(std::fs::read_dir(dir.path()).unwrap().count(), 2);
}

#[tokio::test]
async fn a_dedicated_key_is_generated_and_registered() {
    use wiremock::matchers::{method, path};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    let home = tempfile::tempdir().unwrap();
    let ssh_dir = home.path().join(".ssh");
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/api/app/v1/device/ssh-key"))
        .respond_with(
            ResponseTemplate::new(201)
                .set_body_raw(r#"{"id":12,"fingerprint":"SHA256:…"}"#, "application/json"),
        )
        .expect(2)
        .mount(&server)
        .await;
    let client = Client::new(&server.uri(), Some(Token::new("atd_x")), Default::default()).unwrap();

    let first = ssh::register_device_key(&client, &ssh_dir, "MacBook (atl)")
        .await
        .unwrap();
    assert!(first.generated);
    assert!(first.key.is_dedicated());
    assert_eq!(first.key.path, ssh_dir.join("atelier_device_ed25519.pub"));
    assert!(first.key.text.starts_with("ssh-ed25519 AAAA"));
    assert!(first.key.text.ends_with("MacBook (atl)"));
    assert_eq!((first.registered.id, first.registered.created), (12, true));
    assert_eq!(
        ssh::dedicated_identity(&ssh_dir),
        Some(ssh_dir.join("atelier_device_ed25519"))
    );
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let mode = |p: &Path| std::fs::metadata(p).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode(&ssh_dir), 0o700);
        assert_eq!(mode(&ssh_dir.join("atelier_device_ed25519")), 0o600);
    }

    // What was sent is the public key, with the title.
    let requests = server.received_requests().await.unwrap();
    let body: serde_json::Value = requests[0].body_json().unwrap();
    assert_eq!(body["title"], "MacBook (atl)");
    assert_eq!(body["key"], first.key.text);
    let private = std::fs::read_to_string(ssh_dir.join("atelier_device_ed25519")).unwrap();
    assert!(private.contains("PRIVATE KEY"));
    assert!(!String::from_utf8_lossy(&requests[0].body).contains("PRIVATE"));

    // The second time the same key is found, not regenerated.
    let second = ssh::register_device_key(&client, &ssh_dir, "MacBook (atl)")
        .await
        .unwrap();
    assert!(!second.generated);
    assert_eq!(second.key, first.key);
}

#[tokio::test]
async fn a_half_present_device_key_is_not_overwritten() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("atelier_device_ed25519"), "private half").unwrap();
    let err = ssh::ensure_public_key(dir.path(), "x").await.unwrap_err();
    assert!(matches!(err, Error::Ssh(_)));
    assert_eq!(
        std::fs::read_to_string(dir.path().join("atelier_device_ed25519")).unwrap(),
        "private half"
    );
}

// ---------------------------------------------------------------- messages

/// The contract's example, verbatim.
const MESSAGES: &str = r#"{"tone":"friendly",
 "moments":{"push":{"ours":["Home and dry. {branch} is safe in your studio.","Off it goes. {count} commits landed on {repo}.","Pushed. Checks are warming up on {device}."],"yours":["Shipped. Go outside."]},
            "clean":{"ours":["All tidy. The kettle is free.","Nothing to commit. Go and make something."],"yours":[]},
            "pass":{"ours":["Green across the board on {device}.","Checks passed. A little smugness is allowed."],"yours":[]},
            "fail":{"ours":["One test disagrees with you on {branch}.","Not this time. The log knows why."],"yours":[]},
            "merge":{"ours":["Merged into main. Nicely done.","In it goes. {repo} is {count} commits better."],"yours":[]},
            "first":{"ours":["Good morning. First one of the day.","And we are off."],"yours":[]},
            "late":{"ours":["Pushed at {time}. It will still be here tomorrow.","Saved. Now go to bed."],"yours":[]},
            "mile":{"ours":["That was commit {count} on {repo}."],"yours":[]},
            "agent":{"ours":["{workflow} left you a note.","{workflow} finished while you were busy."],"yours":[]}},
 "plain":{"push":"Pushed {branch}.","clean":"No changes.","pass":"Checks passed.","fail":"Checks failed.","merge":"Merged into {base}.","first":"Committed.","late":"Pushed {branch}.","mile":"Committed.","agent":"{workflow} finished."},
 "rules":{"quiet_fullscreen":true,"terminal":true,"milestones":true}}"#;

fn messages(tone: Tone) -> Messages {
    let mut messages: Messages = serde_json::from_str(MESSAGES).unwrap();
    messages.tone = tone;
    messages
}

fn all_vars() -> Vars {
    Vars::new()
        .set("repo", "Atelier")
        .set("branch", "feature/tunnel-ui")
        .set("base", "main")
        .set("count", 3)
        .set("device", "MacBook")
        .set("time", "23:48")
        .set("workflow", "weekly-tidy")
}

#[test]
fn friendly_picks_from_ours_then_yours_by_seed() {
    let friendly = messages(Tone::Friendly);
    let picked: Vec<String> = (0..5)
        .map(|seed| pick(&friendly, "push", &all_vars(), Some(seed)).unwrap())
        .collect();
    assert_eq!(
        picked,
        [
            "Home and dry. feature/tunnel-ui is safe in your studio.",
            "Off it goes. 3 commits landed on Atelier.",
            "Pushed. Checks are warming up on MacBook.",
            "Shipped. Go outside.",
            "Home and dry. feature/tunnel-ui is safe in your studio.",
        ]
    );
    // The same seed always says the same thing.
    assert_eq!(
        pick(&friendly, "push", &all_vars(), Some(2)),
        pick(&friendly, "push", &all_vars(), Some(2))
    );
    // Without a seed it is still one of them.
    let unseeded = pick(&friendly, "push", &all_vars(), None).unwrap();
    assert!(picked.contains(&unseeded));
    assert_eq!(
        pick(&friendly, "agent", &all_vars(), Some(1)).unwrap(),
        "weekly-tidy finished while you were busy."
    );
}

#[test]
fn plain_and_off() {
    let plain = messages(Tone::Plain);
    for seed in 0..4 {
        assert_eq!(
            pick(&plain, "push", &all_vars(), Some(seed)).unwrap(),
            "Pushed feature/tunnel-ui."
        );
    }
    assert_eq!(
        pick(&plain, "merge", &all_vars(), None).unwrap(),
        "Merged into main."
    );
    assert_eq!(
        pick(&plain, "clean", &Vars::new(), None).unwrap(),
        "No changes."
    );

    let off = messages(Tone::Off);
    for moment in ["push", "clean", "merge", "agent"] {
        assert_eq!(pick(&off, moment, &all_vars(), Some(0)), None);
    }
    // A tone from the future reads as plain.
    assert_eq!(
        pick(&messages(Tone::Unknown), "pass", &all_vars(), Some(0)).unwrap(),
        "Checks passed."
    );
}

#[test]
fn messages_that_cannot_be_filled_are_passed_over() {
    let friendly = messages(Tone::Friendly);
    // Only the branch is known: 2 of the 4 push messages can be said.
    let vars = Vars::new().set("branch", "main");
    let picked: Vec<String> = (0..4)
        .map(|seed| pick(&friendly, "push", &vars, Some(seed)).unwrap())
        .collect();
    assert_eq!(
        picked,
        [
            "Home and dry. main is safe in your studio.",
            "Shipped. Go outside.",
            "Home and dry. main is safe in your studio.",
            "Shipped. Go outside.",
        ]
    );
    // None can be filled: fall back to plain; plain can't either: say nothing.
    assert_eq!(
        pick(&friendly, "mile", &Vars::new(), Some(0)).unwrap(),
        "Committed."
    );
    assert_eq!(pick(&friendly, "agent", &Vars::new(), Some(0)), None);
    assert_eq!(
        pick(&messages(Tone::Plain), "push", &Vars::new(), None),
        None
    );
    // A moment nobody knows.
    assert_eq!(pick(&friendly, "eclipse", &all_vars(), Some(0)), None);
    // Empty values count as unknown.
    assert_eq!(
        pick(
            &friendly,
            "mile",
            &Vars::new().set("count", "").set("repo", "x"),
            Some(0)
        )
        .unwrap(),
        "Committed."
    );
}

#[test]
fn placeholder_filling() {
    let vars = all_vars();
    assert_eq!(
        fill(
            "{repo} {branch} {base} {count} {device} {time} {workflow}",
            &vars
        )
        .unwrap(),
        "Atelier feature/tunnel-ui main 3 MacBook 23:48 weekly-tidy"
    );
    assert_eq!(fill("{repo}{repo}", &vars).unwrap(), "AtelierAtelier");
    // Braces that aren't placeholders stay; values are not re-expanded.
    assert_eq!(
        fill("{nope} {} { {repo", &vars).unwrap(),
        "{nope} {} { {repo"
    );
    assert_eq!(fill("{{repo}}", &vars).unwrap(), "{Atelier}");
    let sneaky = Vars::new().set("repo", "{branch}").set("branch", "main");
    assert_eq!(fill("{repo}", &sneaky).unwrap(), "{branch}");
    assert_eq!(fill("{branch}", &Vars::new()), None);
    assert_eq!(
        fill("No placeholders — “quotes” stay.", &Vars::new()).unwrap(),
        "No placeholders — “quotes” stay."
    );
    assert_eq!(Vars::new().set_opt("repo", None::<&str>), Vars::new());
}

#[test]
fn editing_your_own_messages() {
    let current = messages(Tone::Friendly);
    assert_eq!(
        yours_with(&current, "push", "  Away   it goes. ").unwrap(),
        ["Shipped. Go outside.", "Away it goes."]
    );
    // Adding the same one twice doesn't duplicate it.
    assert_eq!(
        yours_with(&current, "push", "Shipped. Go outside.").unwrap(),
        ["Shipped. Go outside."]
    );
    assert_eq!(
        yours_with(&current, "clean", "Spotless.").unwrap(),
        ["Spotless."]
    );
    assert!(matches!(
        yours_with(&current, "eclipse", "x"),
        Err(Error::Invalid(_))
    ));

    // The server's limits: 140 characters, 20 per moment.
    assert!(validate_yours(&"é".repeat(140)).is_ok());
    let long = validate_yours(&"é".repeat(141)).unwrap_err();
    assert_eq!(
        long.to_string(),
        "That message is 141 characters; the most is 140."
    );
    assert!(validate_yours("   ").is_err());
    let mut full = current.clone();
    full.moments.get_mut("push").unwrap().yours = (0..20).map(|n| format!("m{n}")).collect();
    assert!(yours_with(&full, "push", "one more").is_err());

    let (left, removed) = yours_without(&current, "push", 1).unwrap();
    assert!(left.is_empty());
    assert_eq!(removed, "Shipped. Go outside.");
    assert!(yours_without(&current, "push", 0).is_err());
    assert!(yours_without(&current, "push", 2).is_err());
    assert_eq!(
        yours_without(&current, "clean", 1).unwrap_err().to_string(),
        "You have no messages of your own for clean."
    );
}
