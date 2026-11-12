//! The Tauri part: windows, the menu bar icon and its popover, the two
//! background tasks (live events from the studio, the runner socket), and
//! native notifications.

use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use atelier_core::config::Env;
use atelier_core::Event;
use serde::Serialize;
use tauri::image::Image;
use tauri::menu::{MenuBuilder, MenuItemBuilder};
use tauri::tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent};
use tauri::{
    AppHandle, Emitter, Manager, PhysicalPosition, RunEvent, WebviewUrl, WebviewWindowBuilder,
    WindowEvent,
};
use tauri_plugin_autostart::ManagerExt as _;
use tauri_plugin_dialog::DialogExt as _;
use tauri_plugin_notification::NotificationExt as _;

use crate::commands;
use crate::desk::{lock, Desk, Effects, Recorder, ENV_DRY_RUN, ENV_NO_BROWSER};
use crate::notify::{self, Target, Toast};

/// Runs the scripted first-run check in the window and logs what it sees.
pub const ENV_SELFTEST: &str = "ATELIER_DESKTOP_SELFTEST";
/// A folder to answer the folder chooser with (tests; no dialog is shown).
pub const ENV_PICK: &str = "ATELIER_DESKTOP_PICK";

const MAIN: &str = "main";
const POPOVER: &str = "tray";
const TRAY_ID: &str = "atelier";
const POPOVER_SIZE: (f64, f64) = (380.0, 640.0);

const ICON: &[u8] = include_bytes!("../icons/tray.png");
const ICON_DOT: &[u8] = include_bytes!("../icons/tray-dot.png");

/// Opens links with the system and launches editors without a shell; in a
/// dry run only remembers them.
struct SystemEffects {
    no_browser: bool,
    dry_run: bool,
    recorder: Recorder,
}

impl Effects for SystemEffects {
    fn open_url(&self, url: &str) -> Result<(), String> {
        if self.no_browser || self.dry_run {
            eprintln!("[atelier] would open {url}");
            return self.recorder.open_url(url);
        }
        tauri_plugin_opener::open_url(url, None::<&str>).map_err(|e| e.to_string())
    }

    fn launch(&self, program: &str, args: &[String]) -> Result<(), String> {
        if self.dry_run {
            eprintln!("[atelier] would run {program} {args:?}");
            return self.recorder.launch(program, args);
        }
        let mut command = std::process::Command::new(program);
        command
            .args(args)
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::piped());
        // `open` answers at once and says when the app isn't there; other
        // launchers may stay around, so they aren't waited for.
        if program == "open" {
            let output = command.output().map_err(|e| e.to_string())?;
            if output.status.success() {
                Ok(())
            } else {
                Err(String::from_utf8_lossy(&output.stderr).trim().to_string())
            }
        } else {
            let mut child = command.spawn().map_err(|e| e.to_string())?;
            std::thread::spawn(move || {
                let _ = child.wait();
            });
            Ok(())
        }
    }
}

/// What the shell keeps between events.
#[derive(Default)]
struct Live {
    tasks: Mutex<Vec<tauri::async_runtime::JoinHandle<()>>>,
    /// Where the last native notification should lead, for a little while.
    pending: Mutex<Option<(Instant, Target)>>,
    badge: Mutex<Option<bool>>,
}

/// Sends an event to every window.
pub fn tell_all<T: Serialize + Clone>(app: &AppHandle, event: &str, payload: &T) {
    let _ = app.emit(event, payload.clone());
}

// ------------------------------------------------------------------ badge

/// Shows or clears the dot on the menu bar icon.
pub fn set_badge(app: &AppHandle, needs_you: bool) {
    let live = app.state::<Live>();
    {
        let mut badge = lock(&live.badge);
        if *badge == Some(needs_you) {
            return;
        }
        *badge = Some(needs_you);
    }
    if let Some(tray) = app.tray_by_id(TRAY_ID) {
        if let Ok(icon) = Image::from_bytes(if needs_you { ICON_DOT } else { ICON }) {
            let _ = tray.set_icon(Some(icon));
            let _ = tray.set_icon_as_template(true);
        }
        let _ = tray.set_tooltip(Some(if needs_you {
            "Atelier · something needs you"
        } else {
            "Atelier"
        }));
    }
}

async fn refresh_badge(app: &AppHandle) {
    let desk = app.state::<Arc<Desk>>().inner().clone();
    if !desk.signed_in() {
        set_badge(app, false);
        return;
    }
    if let Ok(overview) = desk.overview().await {
        set_badge(app, !overview.needs.is_empty());
    }
}

// ---------------------------------------------------------------- windows

pub fn show_main(app: &AppHandle, target: Option<Target>) {
    hide_popover(app);
    if let Some(window) = app.get_webview_window(MAIN) {
        let _ = window.unminimize();
        let _ = window.show();
        let _ = window.set_focus();
    }
    if let Some(target) = target {
        let _ = app.emit_to(MAIN, "navigate", target);
    }
}

pub fn hide_popover(app: &AppHandle) {
    if let Some(window) = app.get_webview_window(POPOVER) {
        let _ = window.hide();
    }
}

fn any_window_visible(app: &AppHandle) -> bool {
    [MAIN, POPOVER].iter().any(|label| {
        app.get_webview_window(label)
            .is_some_and(|w| w.is_visible().unwrap_or(false))
    })
}

/// Shows the popover under the menu bar icon, or hides it when it is up.
fn toggle_popover(app: &AppHandle, icon: tauri::Rect) {
    let Some(window) = app.get_webview_window(POPOVER) else {
        return;
    };
    if window.is_visible().unwrap_or(false) {
        let _ = window.hide();
        return;
    }
    let scale = window.scale_factor().unwrap_or(1.0);
    let at = icon.position.to_physical::<f64>(scale);
    let size = icon.size.to_physical::<f64>(scale);
    let width = POPOVER_SIZE.0 * scale;
    let mut x = at.x + size.width / 2.0 - width / 2.0;
    let y = at.y + size.height + 6.0 * scale;
    // Keep it on the screen the icon is on.
    if let Ok(Some(monitor)) = app.monitor_from_point(at.x, at.y) {
        let left = f64::from(monitor.position().x) + 8.0 * scale;
        let right =
            f64::from(monitor.position().x) + f64::from(monitor.size().width) - width - 8.0 * scale;
        x = x.clamp(left, right.max(left));
    }
    let _ = window.set_position(PhysicalPosition::new(x, y));
    let _ = window.show();
    let _ = window.set_focus();
    let _ = app.emit_to(POPOVER, "popover-shown", ());
}

/// Keeps the main window above the others (self-test only).
pub fn keep_in_front(app: &AppHandle) {
    if let Some(window) = app.get_webview_window(MAIN) {
        let _ = window.set_always_on_top(true);
        let _ = window.set_focus();
    }
}

/// Shows the popover at a fixed place on the screen (self-test only).
pub fn show_popover_at(app: &AppHandle, x: f64, y: f64) {
    if let Some(window) = app.get_webview_window(POPOVER) {
        let _ = window.set_position(tauri::LogicalPosition::new(x, y));
        let _ = window.show();
        let _ = app.emit_to(POPOVER, "popover-shown", ());
    }
}

fn build_popover(app: &AppHandle) -> tauri::Result<()> {
    let builder = WebviewWindowBuilder::new(app, POPOVER, WebviewUrl::App("tray.html".into()))
        .title("Atelier")
        .inner_size(POPOVER_SIZE.0, POPOVER_SIZE.1)
        .resizable(false)
        .decorations(false)
        .always_on_top(true)
        .skip_taskbar(true)
        .visible(false)
        .shadow(true)
        .transparent(true);
    builder.build().map(|_| ())
}

fn build_tray(app: &AppHandle) -> tauri::Result<()> {
    let open = MenuItemBuilder::with_id("open", "Open Atelier").build(app)?;
    let quit = MenuItemBuilder::with_id("quit", "Quit Atelier").build(app)?;
    let menu = MenuBuilder::new(app).items(&[&open, &quit]).build()?;
    TrayIconBuilder::with_id(TRAY_ID)
        .icon(Image::from_bytes(ICON)?)
        .icon_as_template(true)
        .tooltip("Atelier")
        .menu(&menu)
        .show_menu_on_left_click(false)
        .on_menu_event(|app, event| match event.id().as_ref() {
            "open" => show_main(app, None),
            "quit" => app.exit(0),
            _ => {}
        })
        .on_tray_icon_event(|tray, event| {
            if let TrayIconEvent::Click {
                button: MouseButton::Left,
                button_state: MouseButtonState::Up,
                rect,
                ..
            } = event
            {
                toggle_popover(tray.app_handle(), rect);
            }
        })
        .build(app)?;
    Ok(())
}

/// Applies "The app" switches: login item, menu bar icon, Dock icon.
pub fn apply_app_prefs(app: &AppHandle, desk: &Desk) -> Result<(), String> {
    let prefs = desk.prefs();
    if let Some(tray) = app.tray_by_id(TRAY_ID) {
        tray.set_visible(prefs.menu_bar)
            .map_err(|e| e.to_string())?;
    }
    #[cfg(target_os = "macos")]
    {
        let policy = if prefs.hide_dock && prefs.menu_bar {
            tauri::ActivationPolicy::Accessory
        } else {
            tauri::ActivationPolicy::Regular
        };
        app.set_activation_policy(policy)
            .map_err(|e| e.to_string())?;
    }
    // The login item lives outside the app's own files; a dry run leaves it be.
    if !desk.dry_run() {
        let autostart = app.autolaunch();
        let enabled = autostart.is_enabled().unwrap_or(false);
        if prefs.autostart && !enabled {
            autostart.enable().map_err(|e| e.to_string())?;
        } else if !prefs.autostart && enabled {
            autostart.disable().map_err(|e| e.to_string())?;
        }
    }
    Ok(())
}

/// Asks for a folder with the system's chooser.
pub async fn pick_folder(app: &AppHandle, desk: &Desk, title: &str) -> Option<PathBuf> {
    if let Some(fixed) = desk.settings().env().get(ENV_PICK) {
        return Some(PathBuf::from(fixed));
    }
    let (tx, rx) = tokio::sync::oneshot::channel();
    let mut dialog = app.dialog().file().set_title(title);
    if let Some(window) = app.get_webview_window(MAIN) {
        dialog = dialog.set_parent(&window);
    }
    dialog.pick_folder(move |folder| {
        let _ = tx.send(folder);
    });
    rx.await.ok().flatten().and_then(|f| f.into_path().ok())
}

// ------------------------------------------------------- background tasks

fn pop_up(app: &AppHandle, desk: &Desk, toast: &Toast) {
    tell_all(app, "notify", toast);
    if desk.dry_run() {
        eprintln!("[atelier] would notify: {} / {}", toast.text, toast.sub);
        return;
    }
    let live = app.state::<Live>();
    *lock(&live.pending) = Some((Instant::now(), toast.target.clone()));
    let _ = app
        .notification()
        .builder()
        .title(&toast.text)
        .body(&toast.sub)
        .show();
}

async fn on_notify(app: &AppHandle, desk: &Desk, event: &atelier_core::api::Notify) {
    let messages = desk.messages(Duration::from_secs(600)).await.ok();
    let now = crate::desk::local_now();
    let context = notify::Context {
        device: Some(format!("your {}", desk.device_name())),
        time: Some(format!("{:02}:{:02}", now.hour(), now.minute())),
        seed: desk.seed(),
    };
    if let Some(toast) = notify::dress(event, messages.as_ref(), &desk.prefs().notify, &context) {
        pop_up(app, desk, &toast);
    }
}

/// Owns the studio's event stream for as long as the app is signed in.
async fn events_task(app: AppHandle) {
    let desk = app.state::<Arc<Desk>>().inner().clone();
    loop {
        let client = match desk.client() {
            Ok(client) => client,
            Err(_) => return,
        };
        let mut stream = client.events();
        let mut was_connected = false;
        loop {
            let next = tokio::select! {
                next = stream.next() => next,
                // Say so when the connection drops or comes back.
                _ = tokio::time::sleep(Duration::from_secs(5)) => {
                    if stream.is_connected() != was_connected {
                        was_connected = stream.is_connected();
                        tell_all(&app, "connection", &was_connected);
                    }
                    continue;
                }
            };
            match next {
                Ok(Event::Notify(event)) => {
                    on_notify(&app, &desk, &event).await;
                    refresh_badge(&app).await;
                }
                Ok(Event::Status(changed)) => {
                    tell_all(&app, "status", &changed);
                    refresh_badge(&app).await;
                }
                Ok(Event::Remote(remote)) => tell_all(&app, "remote", &remote),
                Err(e) => {
                    // Retrying is pointless: the token was refused.
                    tell_all(&app, "connection", &false);
                    if e.needs_sign_in() {
                        tell_all(&app, "signed-out", &());
                        return;
                    }
                    break;
                }
            }
        }
        tokio::time::sleep(Duration::from_secs(30)).await;
    }
}

/// Asks the runner how it is every two seconds while something is on show,
/// and tells the windows when the answer changes.
async fn runner_task(app: AppHandle) {
    let desk = app.state::<Arc<Desk>>().inner().clone();
    let mut last: Option<String> = None;
    let mut ticker = tokio::time::interval(Duration::from_secs(2));
    ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        ticker.tick().await;
        if !any_window_visible(&app) {
            continue;
        }
        let status = desk.runner_snapshot().await;
        let text = serde_json::to_string(&status).unwrap_or_default();
        if last.as_deref() != Some(text.as_str()) {
            last = Some(text);
            tell_all(&app, "runner", &status);
        }
    }
}

/// Starts the background tasks (again). Safe to call more than once.
pub fn start_live(app: &AppHandle) {
    stop_live(app);
    let live = app.state::<Live>();
    let mut tasks = lock(&live.tasks);
    tasks.push(tauri::async_runtime::spawn(events_task(app.clone())));
    tasks.push(tauri::async_runtime::spawn(runner_task(app.clone())));
    let badge_app = app.clone();
    tasks.push(tauri::async_runtime::spawn(async move {
        loop {
            refresh_badge(&badge_app).await;
            tokio::time::sleep(Duration::from_secs(60)).await;
        }
    }));
}

pub fn stop_live(app: &AppHandle) {
    let live = app.state::<Live>();
    for task in lock(&live.tasks).drain(..) {
        task.abort();
    }
}

// -------------------------------------------------------------------- run

/// The commands of everything but the git client's four areas.
fn main_handler() -> impl Fn(tauri::ipc::Invoke) -> bool + Send + Sync + 'static {
    tauri::generate_handler![
        commands::app_state,
        commands::connect_start,
        commands::connect_poll,
        commands::connect_reopen,
        commands::connect_cancel,
        commands::disconnect,
        commands::find_folders,
        commands::add_watch_folder,
        commands::remove_watch_folder,
        commands::choose_clone_root,
        commands::suggest_folders,
        commands::add_suggested_folders,
        commands::folders_done,
        commands::open_folder,
        commands::runner_probe,
        commands::overview,
        commands::run_log,
        commands::repo_view,
        commands::repo_changes,
        commands::repo_diff,
        commands::repo_commit,
        commands::repo_discard,
        commands::repo_undo_commit,
        commands::repo_history,
        commands::repo_commit_detail,
        commands::repo_branches,
        commands::repo_switch,
        commands::repo_new_branch,
        commands::repo_fetch,
        commands::repo_pull,
        commands::repo_push,
        commands::repo_open,
        commands::open_web,
        commands::open_studio,
        commands::open_github,
        commands::open_link,
        commands::markdown_preview,
        commands::pulls_list,
        commands::pull_detail,
        commands::pull_merge,
        commands::pull_update,
        commands::pull_cancel_merge,
        commands::pull_checkout,
        commands::agent_approve,
        commands::device_view,
        commands::runner_set_taking,
        commands::runner_set_rule,
        commands::runner_tail,
        commands::job_stop,
        commands::choose_folder,
        commands::folder_plan,
        commands::folder_init,
        commands::publish,
        commands::search_repos,
        commands::clone_repo,
        commands::mirror_and_clone,
        commands::create_new,
        commands::remote_view,
        commands::remote_set,
        commands::remote_link,
        commands::set_editor,
        commands::set_notify_pref,
        commands::set_app_pref,
        commands::messages_view,
        commands::messages_set_tone,
        commands::messages_add,
        commands::messages_remove,
        commands::messages_set_rule,
        commands::show_main,
        commands::hide_popover,
        commands::quit,
        commands::selftest_log,
        commands::selftest_plan,
        commands::selftest_popover,
        commands::repo_rename,
        commands::repo_delete,
        commands::repo_set_pinned,
        commands::set_group_collapsed,
        commands::repo_runs,
        commands::issues_list,
        commands::issue_detail,
        commands::issue_create,
        commands::issue_comment,
        commands::issue_set_open,
    ]
}

pub fn run() {
    let env = Env::process();
    let effects = Arc::new(SystemEffects {
        no_browser: env.get(ENV_NO_BROWSER).is_some(),
        dry_run: env.get(ENV_DRY_RUN).is_some(),
        recorder: Recorder::default(),
    });
    let desk = match Desk::load(env, effects) {
        Ok(desk) => Arc::new(desk),
        Err(e) => {
            eprintln!("atelier-desktop: {}", e.message);
            std::process::exit(1);
        }
    };

    let main = main_handler();
    let stage = crate::gitui_stage::handler();
    let history = crate::gitui_history::handler();
    let branches = crate::gitui_branches::handler();
    let flow = crate::gitui_flow::handler();

    let app = tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_notification::init())
        .plugin(tauri_plugin_autostart::init(
            tauri_plugin_autostart::MacosLauncher::LaunchAgent,
            None,
        ))
        .manage(desk)
        .manage(Live::default())
        .invoke_handler(move |invoke| {
            // The git client's areas each own their commands; the name's
            // first word says whose it is.
            let command = invoke.message.command();
            if command.starts_with("stage_") {
                stage(invoke)
            } else if command.starts_with("history_") {
                history(invoke)
            } else if command.starts_with("branches_") {
                branches(invoke)
            } else if command.starts_with("flow_") {
                flow(invoke)
            } else {
                main(invoke)
            }
        })
        .setup(|app| {
            let handle = app.handle().clone();
            build_tray(&handle)?;
            build_popover(&handle)?;
            let desk = handle.state::<Arc<Desk>>().inner().clone();
            if let Err(e) = apply_app_prefs(&handle, &desk) {
                eprintln!("atelier-desktop: {e}");
            }
            if desk.signed_in() {
                start_live(&handle);
            }
            Ok(())
        })
        .on_window_event(|window, event| match (window.label(), event) {
            // Closing the main window keeps the app in the menu bar.
            (MAIN, WindowEvent::CloseRequested { api, .. }) => {
                let app = window.app_handle();
                let in_menu_bar = app.state::<Arc<Desk>>().prefs().menu_bar;
                if in_menu_bar {
                    api.prevent_close();
                    let _ = window.hide();
                } else {
                    app.exit(0);
                }
            }
            (POPOVER, WindowEvent::Focused(false)) => {
                let _ = window.hide();
            }
            _ => {}
        })
        .build(tauri::generate_context!())
        .expect("Atelier Desktop couldn't start");

    app.run(|app, event| match event {
        // Clicking the Dock icon, or a notification, brings the window back;
        // a notification from the last minute also says where to go.
        #[cfg(target_os = "macos")]
        RunEvent::Reopen { .. } => {
            let target = lock(&app.state::<Live>().pending)
                .take()
                .filter(|(at, _)| at.elapsed() < Duration::from_secs(60))
                .map(|(_, target)| target);
            show_main(app, target);
        }
        RunEvent::ExitRequested { .. } => stop_live(app),
        _ => {}
    });
}
