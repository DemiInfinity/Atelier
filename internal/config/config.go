// Package config reads Atelier's settings from environment variables.
package config

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Listen     string // address to listen on, e.g. ":3000"
	DataDir    string // holds atelier.db and repos/
	PublicURL  string // external URL behind the proxy, e.g. https://git.example.com
	TrustProxy bool   // honour X-Forwarded-* headers from nginx

	GitHubClientID     string // OAuth app for "Continue with GitHub"
	GitHubClientSecret string
	GitHubLogin        string // only this GitHub account may sign in

	// Backups (internal/backup). Secrets come only from the environment.
	BackupDir          string // where snapshots are written; default <data>/backups
	BackupTime         string // daily run time "HH:MM" in server local time, or "off"
	BackupKeepDaily    int
	BackupKeepWeekly   int
	BackupKeepMonthly  int
	BackupS3Endpoint   string // e.g. https://s3.eu-west-1.amazonaws.com; empty disables S3
	BackupS3Bucket     string
	BackupS3Prefix     string
	BackupS3Region     string
	BackupS3AccessKey  string
	BackupS3SecretKey  string
	MirrorIntervalMins int // how often import mirrors fetch, in minutes

	SSHListen     string // address for the git SSH server, e.g. ":2222"; empty disables SSH
	SSHPublicPort int    // port shown in SSH clone URLs (the host side of a Docker port mapping)

	// Pages (internal/web/pages_site*.go). Sites are served only on the host
	// of PagesURL, which must differ from the app's; empty turns Pages off.
	PagesURL      string // e.g. http://pages.example.lan
	PagesMaxBytes int64  // per deploy; ATELIER_PAGES_MAX_MB, default 1024
	PagesMaxFiles int    // per deploy; ATELIER_PAGES_MAX_FILES, default 50000
	PagesKeep     int    // deploys kept per site; ATELIER_PAGES_KEEP, default 5

	// Remote access (internal/web/tunnel*.go). Whether it's on is a setting
	// in the database (off by default); these only say where things are.
	TunnelListen string // loopback listener only the tunnel helper connects to; ATELIER_TUNNEL_LISTEN, default 127.0.0.1:3001
	TunnelBin    string // the cloudflared binary; ATELIER_CLOUDFLARED, default "cloudflared" on PATH
	// The Tailscale provider's two binaries; ATELIER_TAILSCALE and
	// ATELIER_TAILSCALED, default "tailscale" and "tailscaled" on PATH.
	TailscaleBin  string
	TailscaledBin string

	// Runner updates (internal/web/runner_update.go): atelier-runner builds for
	// each OS and CPU, named atelier-runner-<os>-<arch>[.exe], with their
	// version in a VERSION file. ATELIER_RUNNER_DIR; the image ships them in
	// /usr/share/atelier/runners. Missing: runners can't be updated from here.
	RunnerDir string
	Version   string // this server's build, set by main
}

func FromEnv() Config {
	c := Config{
		Listen:             env("ATELIER_LISTEN", ":3000"),
		DataDir:            env("ATELIER_DATA", "./data"),
		RunnerDir:          env("ATELIER_RUNNER_DIR", "/usr/share/atelier/runners"),
		PublicURL:          strings.TrimRight(os.Getenv("ATELIER_PUBLIC_URL"), "/"),
		TrustProxy:         truthy(os.Getenv("ATELIER_TRUST_PROXY")),
		GitHubClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		GitHubClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		GitHubLogin:        strings.ToLower(os.Getenv("ATELIER_GITHUB_LOGIN")),

		BackupDir:          os.Getenv("ATELIER_BACKUP_DIR"),
		BackupTime:         env("ATELIER_BACKUP_TIME", "03:00"),
		BackupKeepDaily:    envInt("ATELIER_BACKUP_KEEP_DAILY", 14),
		BackupKeepWeekly:   envInt("ATELIER_BACKUP_KEEP_WEEKLY", 8),
		BackupKeepMonthly:  envInt("ATELIER_BACKUP_KEEP_MONTHLY", 6),
		BackupS3Endpoint:   os.Getenv("ATELIER_BACKUP_S3_ENDPOINT"),
		BackupS3Bucket:     os.Getenv("ATELIER_BACKUP_S3_BUCKET"),
		BackupS3Prefix:     os.Getenv("ATELIER_BACKUP_S3_PREFIX"),
		BackupS3Region:     os.Getenv("ATELIER_BACKUP_S3_REGION"),
		BackupS3AccessKey:  os.Getenv("ATELIER_BACKUP_S3_ACCESS_KEY"),
		BackupS3SecretKey:  os.Getenv("ATELIER_BACKUP_S3_SECRET_KEY"),
		MirrorIntervalMins: envInt("ATELIER_MIRROR_INTERVAL", 30),
		PagesURL:           strings.TrimRight(strings.TrimSpace(os.Getenv("ATELIER_PAGES_URL")), "/"),
		PagesMaxBytes:      int64(envInt("ATELIER_PAGES_MAX_MB", 1024)) << 20,
		PagesMaxFiles:      envInt("ATELIER_PAGES_MAX_FILES", 50000),
		PagesKeep:          envInt("ATELIER_PAGES_KEEP", 5),
		SSHListen:          ":2222",
		TunnelListen:       env("ATELIER_TUNNEL_LISTEN", "127.0.0.1:3001"),
		TunnelBin:          env("ATELIER_CLOUDFLARED", "cloudflared"),
		TailscaleBin:       env("ATELIER_TAILSCALE", "tailscale"),
		TailscaledBin:      env("ATELIER_TAILSCALED", "tailscaled"),
	}
	// ATELIER_SSH_LISTEN set to an empty string turns the SSH server off.
	if v, ok := os.LookupEnv("ATELIER_SSH_LISTEN"); ok {
		c.SSHListen = strings.TrimSpace(v)
	}
	if p, err := strconv.Atoi(strings.TrimSpace(os.Getenv("ATELIER_SSH_PUBLIC_PORT"))); err == nil && p > 0 && p < 65536 {
		c.SSHPublicPort = p
	}
	if abs, err := filepath.Abs(c.DataDir); err == nil {
		c.DataDir = abs
	}
	if c.BackupDir == "" {
		c.BackupDir = filepath.Join(c.DataDir, "backups")
	} else if abs, err := filepath.Abs(c.BackupDir); err == nil {
		c.BackupDir = abs
	}
	return c
}

func (c Config) GitHubEnabled() bool {
	return c.GitHubClientID != "" && c.GitHubClientSecret != "" && c.GitHubLogin != ""
}

// SSHEnabled reports whether the git SSH server should run.
func (c Config) SSHEnabled() bool { return c.SSHListen != "" }

// SSHPort is the port people connect to for git over SSH: ATELIER_SSH_PUBLIC_PORT
// when set, otherwise the port Atelier listens on.
func (c Config) SSHPort() int {
	if c.SSHPublicPort > 0 {
		return c.SSHPublicPort
	}
	if _, p, err := net.SplitHostPort(c.SSHListen); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			return n
		}
	}
	return 22
}

// SSHHostKeyPath is where the server's persistent ed25519 host key lives.
func (c Config) SSHHostKeyPath() string { return filepath.Join(c.DataDir, "ssh_host_ed25519_key") }

func (c Config) DBPath() string   { return filepath.Join(c.DataDir, "atelier.db") }
func (c Config) RepoRoot() string { return filepath.Join(c.DataDir, "repos") }

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// envInt reads a non-negative integer, falling back to def when unset or invalid.
func envInt(key string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && n >= 0 {
		return n
	}
	return def
}
