//! `atl` — your Atelier studio from the terminal.

mod cmd;
mod ctx;
mod error;
mod ui;

use clap::{Args, CommandFactory, Parser, Subcommand, ValueEnum};

use atelier_core::config::{Env, Settings};

use crate::ctx::Ctx;
use crate::error::{CliError, CliResult};

#[derive(Parser)]
#[command(
    name = "atl",
    version,
    about = "Your Atelier studio from the terminal",
    disable_help_subcommand = true,
    after_help = "Everything also takes --json. Exit codes: 0 fine, 1 error, 2 usage, 3 not signed in."
)]
struct Cli {
    /// Print machine-readable JSON instead of text
    #[arg(long, global = true)]
    json: bool,
    #[command(subcommand)]
    command: Option<Command>,
}

#[derive(Subcommand)]
enum Command {
    /// Sign this machine in to your studio (you approve it in the browser)
    Login(LoginArgs),
    /// Forget this machine's device token
    Logout,
    /// Who you are signed in as, and where
    Whoami,
    /// What needs you, across your studio
    Status,
    /// List, clone, publish, rename and delete repositories
    Repo {
        #[command(subcommand)]
        command: RepoCommand,
    },
    /// Pull requests for this repository
    #[command(args_conflicts_with_subcommands = true)]
    Pr(PrArgs),
    /// Issues of this repository
    Issue {
        #[command(subcommand)]
        command: Option<IssueCommand>,
    },
    /// Jobs and agents, live
    #[command(args_conflicts_with_subcommands = true)]
    Run {
        #[command(subcommand)]
        command: Option<RunCommand>,
        #[command(flatten)]
        list: RunListArgs,
    },
    /// Agents waiting for your approval
    Agent {
        #[command(subcommand)]
        command: Option<AgentCommand>,
    },
    /// Open a repository's working copy in your editor
    Open {
        /// A studio repository (owner/name or name); default: this folder's
        repo: Option<String>,
    },
    /// Reach your studio from outside
    Remote {
        #[command(subcommand)]
        command: Option<RemoteCommand>,
    },
    /// Your pop-up messages
    Say {
        #[command(subcommand)]
        command: Option<SayCommand>,
    },
    /// The runner on this machine
    Runner {
        #[command(subcommand)]
        command: Option<RunnerCommand>,
    },
    /// Print a shell completion script
    Completions { shell: clap_complete::Shell },
}

#[derive(Args)]
struct LoginArgs {
    /// Your studio's address, e.g. atelier.example.lan
    studio: String,
    /// What to call this device in Settings → Devices (default: its host name)
    #[arg(long)]
    name: Option<String>,
    /// Print the approval link instead of opening a browser
    #[arg(long)]
    no_browser: bool,
}

#[derive(Subcommand)]
enum RepoCommand {
    /// Repositories in your studio
    List,
    /// Clone from your studio, or from GitHub through your studio
    Clone(CloneArgs),
    /// Turn this folder into a repository in your studio
    Publish(PublishArgs),
    /// Give this repository a new name in your studio
    Rename {
        /// The new name
        new: String,
        #[command(flatten)]
        scope: RepoScope,
    },
    /// Delete this repository from your studio (the folder here stays)
    Delete {
        /// Don't ask: the repository's full name, to be sure
        #[arg(long, short = 'y', value_name = "OWNER/NAME")]
        yes: Option<String>,
        #[command(flatten)]
        scope: RepoScope,
    },
}

#[derive(Clone, Copy, ValueEnum)]
enum IssueStateArg {
    Open,
    Closed,
    All,
}

#[derive(Subcommand)]
enum IssueCommand {
    /// This repository's issues
    List {
        #[arg(long, value_enum, default_value = "open")]
        state: IssueStateArg,
        #[command(flatten)]
        scope: RepoScope,
    },
    /// One issue with its conversation
    View {
        number: u64,
        #[command(flatten)]
        scope: RepoScope,
    },
    /// Open a new issue
    New {
        /// The title, in one line
        title: String,
        /// What it is about
        #[arg(long, short = 'b', default_value = "")]
        body: String,
        #[command(flatten)]
        scope: RepoScope,
    },
    /// Close an issue
    Close {
        number: u64,
        #[command(flatten)]
        scope: RepoScope,
    },
}

#[derive(Args)]
struct CloneArgs {
    /// owner/name, or just the name
    #[arg(required_unless_present = "github")]
    name: Option<String>,
    /// Mirror this GitHub repository (owner/name) into your studio first
    #[arg(long, value_name = "OWNER/NAME", conflicts_with = "name")]
    github: Option<String>,
    /// Where to put it (default: <clone_root>/<owner>/<name>)
    #[arg(long, value_name = "DIR")]
    path: Option<std::path::PathBuf>,
    /// Clone over HTTPS instead of SSH
    #[arg(long)]
    https: bool,
}

#[derive(Args)]
struct PublishArgs {
    /// The account to publish under (default: you choose, or your own)
    #[arg(long)]
    owner: Option<String>,
    /// The repository's name (default: the folder's name)
    #[arg(long)]
    name: Option<String>,
    /// Make it public (default: private)
    #[arg(long)]
    public: bool,
    #[arg(long, default_value = "")]
    description: String,
    /// Don't ask anything
    #[arg(long, short = 'y')]
    yes: bool,
    /// If origin already points somewhere, point it at your studio
    #[arg(long)]
    replace_origin: bool,
    /// Push over HTTPS instead of SSH
    #[arg(long)]
    https: bool,
}

#[derive(Args)]
struct PrArgs {
    /// Show this pull request, with its checks and bot comments
    number: Option<u64>,
    #[command(flatten)]
    scope: RepoScope,
    /// Every open pull request in your studio
    #[arg(long, conflicts_with = "number")]
    all: bool,
    #[arg(long, value_enum, default_value = "open")]
    state: StateArg,
    #[command(subcommand)]
    command: Option<PrCommand>,
}

#[derive(Args)]
struct RepoScope {
    /// The repository (default: the one this folder belongs to)
    #[arg(long, short = 'R', value_name = "OWNER/NAME")]
    repo: Option<String>,
}

#[derive(Clone, Copy, ValueEnum)]
enum StateArg {
    Open,
    Closed,
    Merged,
    All,
}

#[derive(Subcommand)]
enum PrCommand {
    /// One pull request with its checks and bot comments (as `atl pr N`)
    View {
        number: u64,
        #[command(flatten)]
        scope: RepoScope,
    },
    /// Bring a pull request up to date: merge its base branch into it, in your studio
    Update {
        /// The pull request (default: the open one for the branch you are on)
        number: Option<u64>,
        #[command(flatten)]
        scope: RepoScope,
    },
    /// Merge a pull request, now or when its checks pass
    Merge {
        number: u64,
        /// Merge as soon as every check passes
        #[arg(long)]
        when_green: bool,
        /// How to merge (default: the repository's first allowed way)
        #[arg(long, value_enum)]
        method: Option<MethodArg>,
        /// Cancel a merge that is waiting for green checks
        #[arg(long, conflicts_with_all = ["when_green", "method", "delete_branch"])]
        cancel: bool,
        /// Delete the branch once it is merged
        #[arg(long)]
        delete_branch: bool,
        #[command(flatten)]
        scope: RepoScope,
    },
}

#[derive(Clone, Copy, ValueEnum)]
enum MethodArg {
    Squash,
    Merge,
    Rebase,
}

#[derive(Args)]
struct RunListArgs {
    /// How many runs to show
    #[arg(long, default_value_t = 20)]
    limit: u32,
    /// Only runs of this repository (owner/name)
    #[arg(long, short = 'R', value_name = "OWNER/NAME")]
    repo: Option<String>,
    /// Only runs that ran on this runner
    #[arg(long)]
    runner: Option<String>,
}

#[derive(Subcommand)]
enum RunCommand {
    /// Recent runs
    List(RunListArgs),
    /// The log of a run's jobs (or of one job)
    Log {
        /// A run id (as in atl run list), or a job id
        id: u64,
        /// Last N lines of each job
        #[arg(long, default_value_t = 200)]
        tail: u32,
        /// Treat ID as a job id
        #[arg(long)]
        job: bool,
    },
    /// What is running, updated as it changes (Ctrl-C to stop)
    Watch {
        /// Stop after this many updates
        #[arg(long, value_name = "N")]
        events: Option<u64>,
    },
}

#[derive(Subcommand)]
enum AgentCommand {
    /// Agents waiting for you
    List,
    /// Let a waiting agent run
    Approve {
        /// The job id (as in atl status)
        id: u64,
        /// Don't ask
        #[arg(long, short = 'y')]
        yes: bool,
        /// Stay and show the agent's log until it finishes
        #[arg(long)]
        follow: bool,
    },
}

#[derive(Subcommand)]
enum RemoteCommand {
    /// Whether your studio can be reached from outside
    Status,
    /// Turn remote access on
    On,
    /// Turn remote access off, signing everyone out
    Off,
    /// Who is connected from outside
    Who,
    /// A one-time link to add a passkey for the remote address
    Link,
}

#[derive(Subcommand)]
enum SayCommand {
    /// The messages for a moment (push, clean, pass, fail, merge, first, late, mile, agent)
    List { moment: String },
    /// Add a message of your own
    Add { moment: String, text: String },
    /// Remove one of your own messages (numbered from 1)
    Remove { moment: String, number: usize },
    /// How your studio talks: friendly, plain or off
    Tone {
        #[arg(value_enum)]
        tone: ToneArg,
    },
    /// The same as: atl say tone off
    Off,
}

#[derive(Clone, Copy, ValueEnum)]
enum ToneArg {
    Friendly,
    Plain,
    Off,
}

#[derive(Subcommand)]
enum RunnerCommand {
    /// What the runner is doing
    Status,
    /// Stop taking new jobs
    Pause,
    /// Take jobs again
    Resume,
}

fn main() {
    // Before any thread starts: the local UTC offset can't be read safely later.
    let local_offset = time::UtcOffset::current_local_offset().ok();
    let cli = Cli::parse();
    let json = cli.json;

    let runtime = match tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
    {
        Ok(runtime) => runtime,
        Err(e) => {
            eprintln!("Couldn’t start: {e}.");
            std::process::exit(error::EXIT_ERROR);
        }
    };
    let result = runtime.block_on(run(cli, local_offset));
    // Don't wait for stray background work (a browser being opened).
    runtime.shutdown_background();
    if let Err(e) = result {
        e.report(json);
        std::process::exit(e.exit);
    }
}

async fn run(cli: Cli, local_offset: Option<time::UtcOffset>) -> CliResult<()> {
    let settings = Settings::load(Env::process()).map_err(|e| CliError::plain(e.to_string()))?;
    let ctx = Ctx::new(cli.json, settings, local_offset);
    match cli.command {
        None => cmd::help::overview(&ctx),
        Some(Command::Login(args)) => cmd::auth::login(&ctx, args).await,
        Some(Command::Logout) => cmd::auth::logout(&ctx).await,
        Some(Command::Whoami) => cmd::auth::whoami(&ctx).await,
        Some(Command::Status) => cmd::status::status(&ctx).await,
        Some(Command::Repo { command }) => match command {
            RepoCommand::List => cmd::repo::list(&ctx).await,
            RepoCommand::Clone(args) => cmd::repo::clone(&ctx, args).await,
            RepoCommand::Publish(args) => cmd::repo::publish(&ctx, args).await,
            RepoCommand::Rename { new, scope } => cmd::repo::rename(&ctx, &new, scope).await,
            RepoCommand::Delete { yes, scope } => cmd::repo::delete(&ctx, yes, scope).await,
        },
        Some(Command::Issue { command }) => match command {
            None => cmd::issue::list(&ctx, IssueStateArg::Open, RepoScope { repo: None }).await,
            Some(IssueCommand::List { state, scope }) => cmd::issue::list(&ctx, state, scope).await,
            Some(IssueCommand::View { number, scope }) => {
                cmd::issue::view(&ctx, number, scope).await
            }
            Some(IssueCommand::New { title, body, scope }) => {
                cmd::issue::new(&ctx, &title, &body, scope).await
            }
            Some(IssueCommand::Close { number, scope }) => {
                cmd::issue::close(&ctx, number, scope).await
            }
        },
        Some(Command::Pr(args)) => match args.command {
            Some(PrCommand::Merge {
                number,
                when_green,
                method,
                cancel,
                delete_branch,
                scope,
            }) => {
                cmd::pr::merge(
                    &ctx,
                    number,
                    when_green,
                    method,
                    cancel,
                    delete_branch,
                    scope,
                )
                .await
            }
            Some(PrCommand::View { number, scope }) => cmd::pr::show(&ctx, number, scope).await,
            Some(PrCommand::Update { number, scope }) => cmd::pr::update(&ctx, number, scope).await,
            None => match args.number {
                Some(number) => cmd::pr::show(&ctx, number, args.scope).await,
                None => cmd::pr::list(&ctx, args.scope, args.all, args.state).await,
            },
        },
        Some(Command::Run { command, list }) => match command {
            None => cmd::run::list(&ctx, list).await,
            Some(RunCommand::List(args)) => cmd::run::list(&ctx, args).await,
            Some(RunCommand::Log { id, tail, job }) => cmd::run::log(&ctx, id, tail, job).await,
            Some(RunCommand::Watch { events }) => cmd::run::watch(&ctx, events).await,
        },
        Some(Command::Agent { command }) => match command {
            None | Some(AgentCommand::List) => cmd::agent::list(&ctx).await,
            Some(AgentCommand::Approve { id, yes, follow }) => {
                cmd::agent::approve(&ctx, id, yes, follow).await
            }
        },
        Some(Command::Open { repo }) => cmd::open::open(&ctx, repo).await,
        Some(Command::Remote { command }) => match command {
            None | Some(RemoteCommand::Status) => cmd::remote::status(&ctx).await,
            Some(RemoteCommand::On) => cmd::remote::on(&ctx).await,
            Some(RemoteCommand::Off) => cmd::remote::off(&ctx).await,
            Some(RemoteCommand::Who) => cmd::remote::who(&ctx).await,
            Some(RemoteCommand::Link) => cmd::remote::link(&ctx).await,
        },
        Some(Command::Say { command }) => match command {
            None => cmd::say::overview(&ctx).await,
            Some(SayCommand::List { moment }) => cmd::say::list(&ctx, &moment).await,
            Some(SayCommand::Add { moment, text }) => cmd::say::add(&ctx, &moment, &text).await,
            Some(SayCommand::Remove { moment, number }) => {
                cmd::say::remove(&ctx, &moment, number).await
            }
            Some(SayCommand::Tone { tone }) => cmd::say::tone(&ctx, tone).await,
            Some(SayCommand::Off) => cmd::say::tone(&ctx, ToneArg::Off).await,
        },
        Some(Command::Runner { command }) => match command {
            None | Some(RunnerCommand::Status) => cmd::runner::status(&ctx).await,
            Some(RunnerCommand::Pause) => cmd::runner::set(&ctx, false).await,
            Some(RunnerCommand::Resume) => cmd::runner::set(&ctx, true).await,
        },
        Some(Command::Completions { shell }) => {
            let mut command = Cli::command();
            let mut script = Vec::new();
            clap_complete::generate(shell, &mut command, "atl", &mut script);
            ui::write_out(&String::from_utf8_lossy(&script));
            Ok(())
        }
    }
}

/// Prints a value as pretty JSON on standard output.
fn print_json(value: &impl serde::Serialize) {
    match serde_json::to_string_pretty(value) {
        Ok(text) => ui::outln!("{text}"),
        Err(e) => eprintln!("Couldn’t write JSON: {e}."),
    }
}
