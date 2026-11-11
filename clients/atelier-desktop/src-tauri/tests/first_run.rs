//! First run against a mock studio: address → code → approval → token,
//! config and SSH key → folders → runner. And leaving again.

mod common;

use atelier_core::config::Config;
use atelier_desktop::desk::ConnectPoll;
use common::*;
use serde_json::json;
use wiremock::matchers::{body_partial_json, method, path};
use wiremock::{Mock, ResponseTemplate};

async fn mock_start(world: &World, verify_url: &str) {
    Mock::given(method("POST"))
        .and(path(format!("{API}/device/start")))
        .and(body_partial_json(
            json!({"kind": "desktop", "os": std::env::consts::OS}),
        ))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "device_code": "c".repeat(64), "user_code": "AMBER-4127",
            "verify_url": verify_url, "expires_in": 600, "interval": 2
        })))
        .mount(&world.server)
        .await;
}

#[tokio::test]
async fn first_run_signs_in_registers_a_key_and_finds_folders() {
    let world = World::fresh().await;
    let studio = world.server.uri();
    let state = world.desk.app_state().await;
    assert!(!state.signed_in);
    assert_eq!(state.studio_url, None);

    let verify = format!("{studio}/settings/devices/approve?code=AMBER-4127");
    mock_start(&world, &verify).await;
    // Pending once, then approved.
    Mock::given(method("POST"))
        .and(path(format!("{API}/device/poll")))
        .respond_with(ResponseTemplate::new(202).set_body_json(json!({"status": "pending"})))
        .up_to_n_times(1)
        .mount(&world.server)
        .await;
    Mock::given(method("POST"))
        .and(path(format!("{API}/device/poll")))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "token": TOKEN, "device_id": 7,
            "person": {"name": "Demi", "login": "DemiInfinity"},
            "studio": {"name": "Atelier", "url": studio, "version": "7f82d31"}
        })))
        .mount(&world.server)
        .await;
    world.mock_get("/me", me()).await;
    world
        .mock(
            "POST",
            "/device/ssh-key",
            201,
            json!({"id": 12, "fingerprint": "SHA256:abc"}),
        )
        .await;

    // Typed without a scheme or with a trailing slash: still found.
    let address = format!("{}/", studio);
    let started = world.desk.connect_start(&address).await.unwrap();
    assert_eq!(started.user_code, "AMBER-4127");
    assert_eq!(started.studio, studio);
    assert!(started.opened);
    assert_eq!(*world.effects.opened.lock().unwrap(), vec![verify.clone()]);

    assert!(matches!(
        world.desk.connect_poll().await.unwrap(),
        ConnectPoll::Pending { .. }
    ));
    assert!(!world.desk.signed_in());
    let ConnectPoll::Approved(connected) = world.desk.connect_poll().await.unwrap() else {
        panic!("expected approval");
    };
    assert_eq!(connected.person_name, "Demi");
    assert_eq!(connected.device_name, "MacBook");
    assert_eq!(connected.token_place, "file");
    let ssh = connected.ssh.expect("a key was registered");
    assert!(ssh.generated);
    assert_eq!(ssh.fingerprint, "SHA256:abc");

    // The token is in the file store of the temp config folder, the config
    // names the studio, and the token is in neither the config nor the state.
    let config_dir = world.base().join("config");
    let tokens = std::fs::read_to_string(config_dir.join("tokens.json")).unwrap();
    assert!(tokens.contains(TOKEN));
    let config_text = std::fs::read_to_string(config_dir.join("config.toml")).unwrap();
    assert!(!config_text.contains("atd_"));
    let config = Config::load(&config_dir).unwrap();
    assert_eq!(config.studio.as_deref(), Some(studio.as_str()));
    assert_eq!(config.device_id, Some(7));
    assert_eq!(config.device_name.as_deref(), Some("MacBook"));
    assert_eq!(config.watch, ["~/Projects"], "what was there is kept");
    let state = world.desk.app_state().await;
    assert!(state.signed_in);
    assert_eq!(state.person_name.as_deref(), Some("Demi"));
    assert_eq!(state.accounts.len(), 2);
    assert!(!serde_json::to_string(&state).unwrap().contains("atd_"));

    // The key was made in the temp SSH folder and sent with this device's name.
    assert!(world.base().join("ssh/atelier_device_ed25519.pub").exists());
    let sent = world.received("POST", "/device/ssh-key").await;
    assert_eq!(sent[0]["title"], "MacBook (Atelier Desktop)");
    assert!(sent[0]["key"].as_str().unwrap().starts_with("ssh-ed25519 "));

    // Find your folders: one in the clone root that the studio knows, one in
    // the watch folder that it doesn't.
    world.copy("DemiInfinity/Atelier").await;
    let stray = world.home().join("Projects/scratch");
    std::fs::create_dir_all(&stray).unwrap();
    world.g(&stray, &["init", "--quiet", "-b", "main"]).await;
    world
        .g(
            &stray,
            &[
                "remote",
                "add",
                "origin",
                "git@github.com:someone/scratch.git",
            ],
        )
        .await;
    world
        .mock_get(
            "/repos",
            json!({"repos": [repo_json("DemiInfinity/Atelier", json!({}))]}),
        )
        .await;
    let folders = world.desk.find_folders().await.unwrap();
    assert_eq!(folders.roots, ["~/Atelier", "~/Projects"]);
    assert_eq!((folders.matched, folders.studio_repos), (1, 1));
    let matched = folders.folders.iter().find(|f| f.repo.is_some()).unwrap();
    assert_eq!(matched.repo.as_deref(), Some("DemiInfinity/Atelier"));
    assert_eq!(matched.display, "~/Atelier/DemiInfinity/Atelier");
    let other = folders.folders.iter().find(|f| f.repo.is_none()).unwrap();
    assert_eq!(other.remote_host.as_deref(), Some("github.com"));

    // Lend a hand: nothing is installed in the temp home, nothing answers.
    let probe = world.desk.runner_probe().await;
    assert!(!probe.installed && !probe.running);
    assert_eq!(probe.how_web_path, "/settings/runners");
}

#[tokio::test]
async fn an_approval_page_elsewhere_is_never_opened() {
    let world = World::fresh().await;
    mock_start(&world, "https://evil.example/approve?code=AMBER-4127").await;
    let started = world.desk.connect_start(&world.server.uri()).await.unwrap();
    assert!(!started.opened);
    assert_eq!(started.verify_url, "", "and it isn't shown either");
    assert!(world.effects.opened.lock().unwrap().is_empty());
    assert_eq!(
        world.desk.connect_reopen().await.unwrap_err().kind,
        "link_refused"
    );
}

#[tokio::test]
async fn refusals_and_wrong_addresses_say_so() {
    let world = World::fresh().await;
    assert_eq!(
        world.desk.connect_start("  ").await.unwrap_err().kind,
        "invalid"
    );
    assert_eq!(world.desk.connect_poll().await.unwrap_err().kind, "invalid");
    // Something that isn't a studio answers 404.
    let err = world
        .desk
        .connect_start(&world.server.uri())
        .await
        .unwrap_err();
    assert_eq!(err.kind, "not_a_studio");
    // Nothing listening at all.
    let err = world
        .desk
        .connect_start("http://127.0.0.1:9")
        .await
        .unwrap_err();
    assert_eq!(err.kind, "network");
    assert!(err.hint.is_some());

    let verify = format!(
        "{}/settings/devices/approve?code=AMBER-4127",
        world.server.uri()
    );
    mock_start(&world, &verify).await;
    Mock::given(method("POST"))
        .and(path(format!("{API}/device/poll")))
        .respond_with(ResponseTemplate::new(410).set_body_json(json!({"error": "denied"})))
        .mount(&world.server)
        .await;
    world.desk.connect_start(&world.server.uri()).await.unwrap();
    let err = world.desk.connect_poll().await.unwrap_err();
    assert_eq!(err.kind, "denied");
    assert!(!world.desk.signed_in());
    assert!(!world.base().join("config/tokens.json").exists());
    // The sign-in is over; polling again has nothing to ask about.
    assert_eq!(world.desk.connect_poll().await.unwrap_err().kind, "invalid");
}

#[tokio::test]
async fn disconnect_removes_the_token_and_the_config() {
    let world = World::signed_in().await;
    assert!(world.desk.app_state().await.signed_in);
    world.desk.disconnect().unwrap();
    let config_dir = world.base().join("config");
    assert!(!config_dir.join("config.toml").exists());
    assert!(!config_dir.join("tokens.json").exists());
    let state = world.desk.app_state().await;
    assert!(!state.signed_in);
    assert_eq!(state.studio_url, None);
    assert_eq!(world.desk.client().unwrap_err().kind, "no_studio");
}

#[tokio::test]
async fn a_revoked_device_is_signed_out_with_a_note() {
    let world = World::fresh().await;
    // A token on disk that the studio no longer accepts.
    let world = {
        drop(world);
        let world = World::signed_in().await;
        world.server.reset().await;
        world
    };
    world
        .mock("GET", "/me", 401, json!({"error": "revoked"}))
        .await;
    let state = world.desk.app_state().await;
    assert!(!state.signed_in);
    assert!(state.note.unwrap().contains("signed out"));
}
