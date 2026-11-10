// Package service installs and removes atelier-runner as an operating
// system service: a launchd user agent on macOS, a systemd unit on Linux
// (user unit by default, system unit when Spec.System is set) and a
// Windows service registered with the Service Control Manager.
//
// The render functions (LaunchdPlist, SystemdUnit, WindowsCommandLine,
// WindowsServiceConfig) are pure and work on every OS. Install, Uninstall
// and Status act on the current OS only; Options.DryRun prints what would
// happen without changing anything.
package service

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Defaults applied by Spec.WithDefaults.
const (
	DefaultName        = "atelier-runner"
	DefaultLabel       = "dev.atelier.runner"
	DefaultDisplayName = "Atelier Runner"
	DefaultDescription = "Runs Atelier Actions jobs on this device"
)

// Windows recovery policy (also described by WindowsServiceConfig).
const (
	windowsRestartDelaySeconds = 5
	windowsRestartCount        = 3
	windowsResetPeriodSeconds  = 86400
	windowsStopWaitSeconds     = 30
)

// Spec describes the service to install.
type Spec struct {
	Name        string            // service name, default "atelier-runner"
	Label       string            // launchd label, default "dev.atelier.runner"
	DisplayName string            // default "Atelier Runner"
	Description string            // default "Runs Atelier Actions jobs on this device"
	Exe         string            // absolute path of the atelier-runner binary
	Args        []string          // e.g. ["run", "--config", "/path/config.json"]
	WorkingDir  string            // optional
	LogDir      string            // launchd stdout/stderr dir (default ~/Library/Logs)
	Env         map[string]string // e.g. PATH captured at install time; rendered sorted
	System      bool              // Linux: system unit in /etc/systemd/system instead of a user unit
	User        string            // Linux system unit: User= (optional)
	HomeDir     string            // used to compute paths; default os.UserHomeDir()
}

// WithDefaults returns a copy of s with empty fields filled in.
func (s Spec) WithDefaults() Spec {
	if s.Name == "" {
		s.Name = DefaultName
	}
	if s.Label == "" {
		s.Label = DefaultLabel
	}
	if s.DisplayName == "" {
		s.DisplayName = DefaultDisplayName
	}
	if s.Description == "" {
		s.Description = DefaultDescription
	}
	if s.HomeDir == "" {
		if h, err := os.UserHomeDir(); err == nil {
			s.HomeDir = h
		}
	}
	if s.LogDir == "" && s.HomeDir != "" {
		s.LogDir = path.Join(filepath.ToSlash(s.HomeDir), "Library", "Logs")
	}
	return s
}

// validate checks the fields Install relies on.
func (s Spec) validate() error {
	if s.Exe == "" {
		return errors.New("service: Exe (path of the atelier-runner binary) is required")
	}
	if !filepath.IsAbs(s.Exe) {
		return fmt.Errorf("service: Exe must be an absolute path, got %q", s.Exe)
	}
	for _, f := range []struct{ field, v string }{{"Name", s.Name}, {"Label", s.Label}} {
		if f.v == "" || strings.ContainsAny(f.v, "/\\ \t\r\n") || f.v == "." || f.v == ".." {
			return fmt.Errorf("service: invalid %s %q", f.field, f.v)
		}
	}
	return nil
}

func sortedEnv(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------- launchd

func xmlEscape(v string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(v))
	return b.String()
}

func logPaths(s Spec) (stdout, stderr string) {
	dir := filepath.ToSlash(s.LogDir)
	return path.Join(dir, s.Name+".log"), path.Join(dir, s.Name+".err.log")
}

// LaunchdPlist renders the launchd property list for a user agent.
func LaunchdPlist(s Spec) string {
	s = s.WithDefaults()
	var b strings.Builder
	str := func(indent, v string) { fmt.Fprintf(&b, "%s<string>%s</string>\n", indent, xmlEscape(v)) }
	key := func(indent, k string) { fmt.Fprintf(&b, "%s<key>%s</key>\n", indent, xmlEscape(k)) }

	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	key("\t", "Label")
	str("\t", s.Label)
	key("\t", "ProgramArguments")
	b.WriteString("\t<array>\n")
	str("\t\t", s.Exe)
	for _, a := range s.Args {
		str("\t\t", a)
	}
	b.WriteString("\t</array>\n")
	if s.WorkingDir != "" {
		key("\t", "WorkingDirectory")
		str("\t", s.WorkingDir)
	}
	if len(s.Env) > 0 {
		key("\t", "EnvironmentVariables")
		b.WriteString("\t<dict>\n")
		for _, k := range sortedEnv(s.Env) {
			key("\t\t", k)
			str("\t\t", s.Env[k])
		}
		b.WriteString("\t</dict>\n")
	}
	key("\t", "RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("\t", "KeepAlive")
	b.WriteString("\t<true/>\n")
	key("\t", "ProcessType")
	str("\t", "Background")
	key("\t", "ExitTimeOut")
	b.WriteString("\t<integer>60</integer>\n")
	if s.LogDir != "" {
		out, errp := logPaths(s)
		key("\t", "StandardOutPath")
		str("\t", out)
		key("\t", "StandardErrorPath")
		str("\t", errp)
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// ---------------------------------------------------------------- systemd

// systemdEscapeSpecifiers doubles '%' so systemd does not treat it as a
// specifier (%h, %n, ...). Specifiers are expanded in almost every setting.
func systemdEscapeSpecifiers(v string) string {
	return strings.ReplaceAll(v, "%", "%%")
}

// systemdQuoteBody escapes backslash, double quote and control characters
// for use inside a double-quoted systemd string.
func systemdQuoteBody(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// systemdExecArg quotes one ExecStart word. '%' becomes '%%' (specifiers),
// '$' becomes '$$' (environment variable substitution), and the word is
// double-quoted with C-style escapes when it contains whitespace, quotes,
// backslashes, control characters or ';', or is empty.
func systemdExecArg(a string) string {
	v := strings.ReplaceAll(systemdEscapeSpecifiers(a), "$", "$$")
	needQuote := v == "" || v == ";"
	for _, r := range v {
		if r <= ' ' || r == '"' || r == '\'' || r == '\\' || r == 0x7f {
			needQuote = true
			break
		}
	}
	if !needQuote {
		return v
	}
	return `"` + systemdQuoteBody(v) + `"`
}

// SystemdUnit renders the systemd unit file.
func SystemdUnit(s Spec) string {
	s = s.WithDefaults()
	var b strings.Builder
	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Description=%s\n", systemdEscapeSpecifiers(oneLine(s.Description)))
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("\n[Service]\n")
	b.WriteString("Type=simple\n")
	words := make([]string, 0, len(s.Args)+1)
	words = append(words, systemdExecArg(s.Exe))
	for _, a := range s.Args {
		words = append(words, systemdExecArg(a))
	}
	fmt.Fprintf(&b, "ExecStart=%s\n", strings.Join(words, " "))
	if s.WorkingDir != "" {
		// WorkingDirectory= takes the rest of the line verbatim (no quoting).
		fmt.Fprintf(&b, "WorkingDirectory=%s\n", systemdEscapeSpecifiers(oneLine(s.WorkingDir)))
	}
	if s.System && s.User != "" {
		fmt.Fprintf(&b, "User=%s\n", systemdEscapeSpecifiers(oneLine(s.User)))
	}
	for _, k := range sortedEnv(s.Env) {
		// Environment= does no $ expansion, but does expand specifiers and
		// honours double quotes with C escapes.
		kv := systemdEscapeSpecifiers(k + "=" + s.Env[k])
		fmt.Fprintf(&b, "Environment=\"%s\"\n", systemdQuoteBody(kv))
	}
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=5\n")
	b.WriteString("KillSignal=SIGTERM\n")
	b.WriteString("TimeoutStopSec=90\n")
	b.WriteString("\n[Install]\n")
	if s.System {
		b.WriteString("WantedBy=multi-user.target\n")
	} else {
		b.WriteString("WantedBy=default.target\n")
	}
	return b.String()
}

// oneLine replaces line breaks so a value cannot inject extra unit lines.
func oneLine(v string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(v)
}

// ---------------------------------------------------------------- windows

// windowsEscapeArg quotes one argument with the same rules as Go's
// syscall.EscapeArg on Windows (CommandLineToArgvW conventions).
func windowsEscapeArg(s string) string {
	if s == "" {
		return `""`
	}
	needsBackslash, hasSpace := false, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\':
			needsBackslash = true
		case ' ', '\t':
			hasSpace = true
		}
	}
	if !needsBackslash && !hasSpace {
		return s
	}
	if !needsBackslash {
		return `"` + s + `"`
	}
	var b strings.Builder
	if hasSpace {
		b.WriteByte('"')
	}
	slashes := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		default:
			slashes = 0
		case '\\':
			slashes++
		case '"':
			for ; slashes > 0; slashes-- {
				b.WriteByte('\\')
			}
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	if hasSpace {
		for ; slashes > 0; slashes-- {
			b.WriteByte('\\')
		}
		b.WriteByte('"')
	}
	return b.String()
}

// WindowsCommandLine builds the service binary path the same way
// golang.org/x/sys/windows/svc/mgr.CreateService does: every word,
// including the executable, escaped with syscall.EscapeArg rules and
// joined with single spaces.
func WindowsCommandLine(exe string, args []string) string {
	words := make([]string, 0, len(args)+1)
	words = append(words, windowsEscapeArg(exe))
	for _, a := range args {
		words = append(words, windowsEscapeArg(a))
	}
	return strings.Join(words, " ")
}

// WindowsServiceConfig describes, in human-readable form, what Install
// registers with the Windows Service Control Manager.
func WindowsServiceConfig(s Spec) string {
	s = s.WithDefaults()
	var b strings.Builder
	row := func(k, v string) { fmt.Fprintf(&b, "%-14s %s\n", k+":", v) }
	row("Service name", s.Name)
	row("Display name", s.DisplayName)
	row("Description", oneLine(s.Description))
	row("Start type", "Automatic (Delayed Start)")
	row("Account", "LocalSystem")
	row("Binary path", WindowsCommandLine(s.Exe, s.Args))
	row("Recovery", fmt.Sprintf("restart after %ds on failures 1-%d (also on non-zero exit); reset failure count after %ds (1 day)",
		windowsRestartDelaySeconds, windowsRestartCount, windowsResetPeriodSeconds))
	if len(s.Env) > 0 {
		b.WriteString("Environment:   (HKLM\\SYSTEM\\CurrentControlSet\\Services\\" + s.Name + "\\Environment)\n")
		for _, k := range sortedEnv(s.Env) {
			fmt.Fprintf(&b, "  %s=%s\n", k, oneLine(s.Env[k]))
		}
	}
	if s.WorkingDir != "" {
		row("Working dir", oneLine(s.WorkingDir)+" (not settable for Windows services; pass it to atelier-runner instead)")
	}
	return b.String()
}

// windowsEnvBlock is the REG_MULTI_SZ value for the service Environment key.
func windowsEnvBlock(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for _, k := range sortedEnv(env) {
		out = append(out, k+"="+env[k])
	}
	return out
}

// ---------------------------------------------------------------- paths

// UnitPath returns where the service definition file is written for the
// current GOOS. On Windows it returns "" (the definition lives in the
// registry).
func UnitPath(s Spec) (string, error) {
	return unitPathFor(runtime.GOOS, s)
}

func unitPathFor(goos string, s Spec) (string, error) {
	s = s.WithDefaults()
	switch goos {
	case "darwin":
		if s.HomeDir == "" {
			return "", errors.New("service: cannot determine home directory")
		}
		return filepath.Join(s.HomeDir, "Library", "LaunchAgents", s.Label+".plist"), nil
	case "linux":
		if s.System {
			return filepath.Join("/etc/systemd/system", s.Name+".service"), nil
		}
		if s.HomeDir == "" {
			return "", errors.New("service: cannot determine home directory")
		}
		return filepath.Join(s.HomeDir, ".config", "systemd", "user", s.Name+".service"), nil
	case "windows":
		return "", nil
	default:
		return "", unsupported(goos)
	}
}

func unsupported(goos string) error {
	return fmt.Errorf("service: unsupported operating system %q (supported: darwin, linux, windows)", goos)
}

// Options controls Install and Uninstall.
type Options struct {
	DryRun bool      // print the file path, contents and commands; change nothing
	Out    io.Writer // progress and dry-run output; nil means io.Discard
}

func (o Options) out() io.Writer {
	if o.Out == nil {
		return io.Discard
	}
	return o.Out
}
