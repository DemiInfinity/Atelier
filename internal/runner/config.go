// Package runner implements atelier-runner: it registers with an Atelier
// server, polls for Actions jobs, runs them on this device and reports back,
// following the protocol in internal/actions/contract.go.
package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"atelier/internal/actions"
)

// DefaultCodeloadURL is where pinned actions are downloaded from.
const DefaultCodeloadURL = "https://codeload.github.com"

// Config is the runner's persistent configuration. It holds the runner token,
// so it is always written with mode 0600.
type Config struct {
	ServerURL    string   `json:"server_url"`
	RunnerID     int64    `json:"runner_id"`
	RunnerToken  string   `json:"runner_token"`
	Name         string   `json:"name"`
	Labels       []string `json:"labels"`       // user labels; automatic ones are added by AllLabels
	Capabilities []string `json:"capabilities"` // jobs, docker, discovery
	WorkDir      string   `json:"work_dir,omitempty"`
	CacheDir     string   `json:"cache_dir,omitempty"`
	Parallel     int      `json:"parallel,omitempty"`
	// DiscoveryPaths are scanned for git repos when the discovery capability is on.
	DiscoveryPaths []string `json:"discovery_paths,omitempty"`
	// CodeloadURL overrides https://codeload.github.com (tests, mirrors).
	CodeloadURL string `json:"codeload_url,omitempty"`
}

// DefaultConfigPath is os.UserConfigDir()/atelier-runner/config.json.
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "atelier-runner", "config.json"), nil
}

// LoadConfig reads the config file. On Unix a file readable by others is
// tightened back to 0600.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no runner config at %s: run `atelier-runner register` first", path)
		}
		return nil, err
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
			_ = os.Chmod(path, 0o600)
		}
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return &c, nil
}

// Save writes the config atomically with mode 0600 (directory 0700).
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Chmod(path, 0o600)
	}
	return nil
}

// HasCap reports whether a capability is switched on.
func (c *Config) HasCap(name string) bool { return slices.Contains(c.Capabilities, name) }

// Dirs fills in defaults for the work and cache directories.
func (c *Config) Dirs() (work, cache string) {
	base := ""
	if d, err := os.UserCacheDir(); err == nil {
		base = filepath.Join(d, "atelier-runner")
	} else {
		base = filepath.Join(os.TempDir(), "atelier-runner")
	}
	work, cache = c.WorkDir, c.CacheDir
	if work == "" {
		work = filepath.Join(base, "work")
	}
	if cache == "" {
		cache = filepath.Join(base, "cache")
	}
	return work, cache
}

// OSLabel is the label for this OS: macos, linux or windows.
func OSLabel() string { return osLabel(runtime.GOOS) }

func osLabel(goos string) string {
	if goos == "darwin" {
		return "macos"
	}
	return goos
}

// ArchLabel is the label for this CPU: x64 or arm64.
func ArchLabel() string { return archLabel(runtime.GOARCH) }

func archLabel(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	case "386":
		return "x86"
	}
	return goarch
}

// AllLabels returns the user's labels plus self-hosted, OS and arch, deduplicated
// and lower-cased.
func AllLabels(user []string) []string { return allLabels(user, runtime.GOOS, runtime.GOARCH) }

func allLabels(user []string, goos, goarch string) []string {
	out := []string{"self-hosted", osLabel(goos), archLabel(goarch)}
	for _, l := range user {
		l = strings.ToLower(strings.TrimSpace(l))
		if l != "" && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// ParseCaps validates a comma-separated capability list.
func ParseCaps(s string) ([]string, error) {
	known := []string{actions.CapJobs, actions.CapDocker, actions.CapDiscovery}
	var out []string
	for _, c := range splitList(s) {
		c = strings.ToLower(c)
		if !slices.Contains(known, c) {
			return nil, fmt.Errorf("unknown capability %q (known: %s)", c, strings.Join(known, ", "))
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out, nil
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

// DefaultDiscoveryPaths are the folders scanned when none are configured.
func DefaultDiscoveryPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	if runtime.GOOS == "windows" {
		var out []string
		if p := os.Getenv("USERPROFILE"); p != "" {
			out = append(out, filepath.Join(p, "source"))
		}
		return append(out, filepath.Join(home, "Projects"))
	}
	return []string{filepath.Join(home, "Projects")}
}
