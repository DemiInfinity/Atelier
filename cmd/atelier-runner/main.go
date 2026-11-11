// Command atelier-runner runs Atelier Actions jobs on this device.
//
//	atelier-runner register --url URL --token TOKEN [--name N] [--labels a,b] [--capabilities jobs,docker,discovery]
//	atelier-runner run [--parallel N] [--grace 2m]
//	atelier-runner install [--system] [--dry-run] | uninstall [--dry-run]
//	atelier-runner status | version
//
// Every command takes --config PATH (default: <user config dir>/atelier-runner/config.json).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"atelier/internal/actions"
	"atelier/internal/runner"
	"atelier/internal/runner/service"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "atelier-runner:", err)
		os.Exit(1)
	}
}

const usage = `atelier-runner runs Atelier Actions jobs on this device.

Commands:
  register   --url URL --token TOKEN [--name N] [--labels a,b] [--capabilities jobs,docker,discovery]
  run        run in the foreground [--parallel N] [--grace DURATION]
  install    install as a service (launchd / systemd / Windows service) [--system] [--dry-run]
  uninstall  remove the service [--dry-run]
  status     show configuration and service state
  workspaces enable|disable|show: "Open on <device>" workspaces (off by default)
  version    print the version

Every command accepts --config PATH.
`

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		// started by the Windows service manager without arguments
		if ok, err := service.RunAsService(service.DefaultName, func(ctx context.Context) error {
			return runCmd(ctx, nil, stderr)
		}); ok {
			return err
		}
		fmt.Fprint(stderr, usage)
		return errors.New("missing command")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "register":
		return registerCmd(rest, stdout)
	case "run":
		if ok, err := service.RunAsService(service.DefaultName, func(ctx context.Context) error {
			return runCmd(ctx, rest, stderr)
		}); ok {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		go func() {
			<-ctx.Done()
			// a second signal exits at once
			c := make(chan os.Signal, 1)
			signal.Notify(c, os.Interrupt, syscall.SIGTERM)
			<-c
			fmt.Fprintln(stderr, "atelier-runner: forced exit")
			os.Exit(130)
		}()
		return runCmd(ctx, rest, stderr)
	case "install":
		return installCmd(rest, stdout, false)
	case "uninstall":
		return installCmd(rest, stdout, true)
	case "status":
		return statusCmd(rest, stdout)
	case "workspaces":
		return workspacesCmd(rest, stdout)
	case "version", "--version", "-version":
		fmt.Fprintf(stdout, "atelier-runner %s (%s/%s, protocol %d)\n", version, runtime.GOOS, runtime.GOARCH, actions.ProtocolVersion)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	}
	fmt.Fprint(stderr, usage)
	return fmt.Errorf("unknown command %q", cmd)
}

func configFlag(fs *flag.FlagSet) *string {
	def, _ := runner.DefaultConfigPath()
	return fs.String("config", def, "config file")
}

func registerCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	cfgPath := configFlag(fs)
	url := fs.String("url", "", "Atelier server URL, e.g. https://git.example.com")
	token := fs.String("token", "", "one-time registration token from Settings → Runners")
	host, _ := os.Hostname()
	name := fs.String("name", host, "runner name")
	labels := fs.String("labels", "", "extra labels, comma-separated")
	caps := fs.String("capabilities", actions.CapJobs, "jobs,docker,discovery")
	work := fs.String("work-dir", "", "where job workspaces go")
	disc := fs.String("discovery-paths", "", "folders to scan for repos, comma-separated (default ~/Projects)")
	ws := fs.Bool("workspaces", false, "switch on device workspaces (\"Open on <device>\"); see `atelier-runner workspaces`")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *url == "" || *token == "" {
		return errors.New("register needs --url and --token")
	}
	cl, err := runner.ParseCaps(*caps)
	if err != nil {
		return err
	}
	if *ws && !contains(cl, actions.CapWorkspaces) {
		cl = append(cl, actions.CapWorkspaces)
	}
	if contains(cl, actions.CapDocker) {
		if _, err := exec.LookPath("docker"); err != nil {
			fmt.Fprintln(stdout, "warning: docker capability requested but docker is not on PATH")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg, err := runner.Register(ctx, strings.TrimRight(*url, "/"), *token, *name, splitList(*labels), cl, version)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	cfg.WorkDir = absOrEmpty(*work)
	for _, p := range splitList(*disc) {
		cfg.DiscoveryPaths = append(cfg.DiscoveryPaths, absOrEmpty(p))
	}
	if err := cfg.Save(*cfgPath); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Registered runner %q (id %d) with %s\nLabels: %s\nConfig: %s\nNext: `atelier-runner run` or `atelier-runner install`\n",
		cfg.Name, cfg.RunnerID, cfg.ServerURL, strings.Join(runner.AllLabels(cfg.Labels), ", "), *cfgPath)
	return nil
}

func runCmd(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := configFlag(fs)
	parallel := fs.Int("parallel", 0, "jobs to run at once (default from config, else 1)")
	grace := fs.Duration("grace", 0, "on shutdown, let running jobs finish for this long before cancelling them")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := runner.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	runner.WorkspaceConfigPath = workspaceConfigPath(*cfgPath)
	p := cfg.Parallel
	if *parallel > 0 {
		p = *parallel
	}
	r := &runner.Runner{
		Config:   cfg,
		Client:   &runner.Client{BaseURL: cfg.ServerURL, Token: cfg.RunnerToken},
		Version:  version,
		Log:      slog.New(slog.NewTextHandler(stderr, nil)),
		Parallel: p,
		Grace:    *grace,
	}
	return r.Run(ctx)
}

func serviceSpec(cfgPath string, system bool) (service.Spec, error) {
	exe, err := os.Executable()
	if err != nil {
		return service.Spec{}, err
	}
	if e, err := filepath.EvalSymlinks(exe); err == nil {
		exe = e
	}
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return service.Spec{}, err
	}
	s := service.Spec{Exe: exe, Args: []string{"run", "--config", abs}, System: system,
		Env: map[string]string{"PATH": os.Getenv("PATH")}}
	if system && runtime.GOOS == "linux" {
		s.User = os.Getenv("SUDO_USER")
	}
	return s.WithDefaults(), nil
}

func installCmd(args []string, stdout io.Writer, uninstall bool) error {
	name := "install"
	if uninstall {
		name = "uninstall"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	cfgPath := configFlag(fs)
	system := fs.Bool("system", false, "Linux: install a system unit instead of a user unit")
	dry := fs.Bool("dry-run", false, "print what would be done, change nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" && os.Geteuid() == 0 {
		return errors.New("on macOS run install as your own user, not with sudo (it installs a LaunchAgent)")
	}
	if !uninstall && !*dry {
		if _, err := runner.LoadConfig(*cfgPath); err != nil {
			return err
		}
	}
	spec, err := serviceSpec(*cfgPath, *system)
	if err != nil {
		return err
	}
	o := service.Options{DryRun: *dry, Out: stdout}
	if uninstall {
		return service.Uninstall(spec, o)
	}
	return service.Install(spec, o)
}

func statusCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	cfgPath := configFlag(fs)
	system := fs.Bool("system", false, "Linux: check the system unit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "atelier-runner %s (%s/%s)\nConfig: %s\n", version, runtime.GOOS, runtime.GOARCH, *cfgPath)
	cfg, err := runner.LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stdout, "Not registered:", err)
	} else {
		work, cache := cfg.Dirs()
		fmt.Fprintf(stdout, "Server: %s\nRunner: %s (id %d)\nLabels: %s\nCapabilities: %s\nWork dir: %s\nCache dir: %s\n",
			cfg.ServerURL, cfg.Name, cfg.RunnerID, strings.Join(runner.AllLabels(cfg.Labels), ", "),
			strings.Join(cfg.Capabilities, ", "), work, cache)
	}
	if spec, err := serviceSpec(*cfgPath, *system); err == nil {
		st, err := service.Status(spec)
		if err != nil {
			st = err.Error()
		}
		fmt.Fprintf(stdout, "Service: %s\n", st)
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func absOrEmpty(p string) string {
	if p == "" {
		return ""
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
