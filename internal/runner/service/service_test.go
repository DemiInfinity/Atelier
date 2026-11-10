package service

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read golden (run go test -update to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch (run go test -update to accept)\n--- got ---\n%s\n--- want ---\n%s", p, got, want)
	}
}

func unixSpec() Spec {
	return Spec{
		Exe:  "/Users/alice/bin/atelier-runner",
		Args: []string{"run", "--config", "/Users/alice/Library/Application Support/Atelier/runner.json", "--label", `100% "fast" $HOME\x`},
		Env: map[string]string{
			"PATH":         "/opt/homebrew/bin:/usr/bin:/bin",
			"ATELIER_NOTE": `a&b <c> "d" 50% $X`,
		},
		WorkingDir: "/Users/alice/Atelier Work",
		HomeDir:    "/Users/alice",
	}
}

func TestLaunchdPlistGolden(t *testing.T) {
	golden(t, "launchd.plist", LaunchdPlist(unixSpec()))
}

func TestSystemdUnitUserGolden(t *testing.T) {
	s := unixSpec()
	s.HomeDir = "/home/alice"
	s.Exe = "/home/alice/.local/bin/atelier-runner"
	s.WorkingDir = "/home/alice/atelier work"
	s.Args[2] = "/home/alice/.config/atelier/runner config.json"
	golden(t, "systemd-user.service", SystemdUnit(s))
}

func TestSystemdUnitSystemGolden(t *testing.T) {
	s := unixSpec()
	s.HomeDir = "/home/alice"
	s.Exe = "/usr/local/bin/atelier-runner"
	s.WorkingDir = ""
	s.Args[2] = "/etc/atelier/runner.json"
	s.System = true
	s.User = "atelier"
	golden(t, "systemd-system.service", SystemdUnit(s))
}

func TestWindowsServiceConfigGolden(t *testing.T) {
	s := Spec{
		Exe:  `C:\Program Files\Atelier\atelier-runner.exe`,
		Args: []string{"run", "--config", `C:\ProgramData\Atelier Runner\config.json`},
		Env:  map[string]string{"PATH": `C:\Windows\system32;C:\Program Files\Git\cmd`},
	}
	golden(t, "windows-service.txt", WindowsServiceConfig(s))
}

func TestSystemdExecArg(t *testing.T) {
	for in, want := range map[string]string{
		"run":          "run",
		"":             `""`,
		"a b":          `"a b"`,
		"100%":         "100%%",
		"$HOME":        "$$HOME",
		`say "hi"`:     `"say \"hi\""`,
		`C:\x`:         `"C:\\x"`,
		";":            `";"`,
		"tab\there":    `"tab\there"`,
		"it's":         `"it's"`,
		"%h $x \"q\"":  `"%%h $$x \"q\""`,
		"/usr/bin/foo": "/usr/bin/foo",
	} {
		if got := systemdExecArg(in); got != want {
			t.Errorf("systemdExecArg(%q) = %s, want %s", in, got, want)
		}
	}
}

// escapeArgCases encode syscall.EscapeArg semantics (see
// service_windows_test.go for the cross-check against the real function).
var escapeArgCases = []struct{ in, want string }{
	{"", `""`},
	{"run", "run"},
	{"a b", `"a b"`},
	{"a\tb", "\"a\tb\""},
	{`C:\Program Files\x.exe`, `"C:\Program Files\x.exe"`},
	{`C:\path\`, `C:\path\`},
	{`C:\my dir\`, `"C:\my dir\\"`},
	{`C:\my dir\\`, `"C:\my dir\\\\"`},
	{`a"b`, `a\"b`},
	{`"quoted"`, `\"quoted\"`},
	{`say "hi there"`, `"say \"hi there\""`},
	{`back\"slash`, `back\\\"slash`},
	{`two\\"x`, `two\\\\\"x`},
	{`\`, `\`},
	{`\ `, `"\ "`},
}

func TestWindowsEscapeArg(t *testing.T) {
	for _, c := range escapeArgCases {
		if got := windowsEscapeArg(c.in); got != c.want {
			t.Errorf("windowsEscapeArg(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestWindowsCommandLine(t *testing.T) {
	tests := []struct {
		exe  string
		args []string
		want string
	}{
		{`C:\atelier\runner.exe`, nil, `C:\atelier\runner.exe`},
		{`C:\Program Files\Atelier\atelier-runner.exe`, []string{"run", "--config", `C:\Data Dir\config.json`},
			`"C:\Program Files\Atelier\atelier-runner.exe" run --config "C:\Data Dir\config.json"`},
		{`C:\r.exe`, []string{"", `x"y`, `C:\trailing dir\`}, `C:\r.exe "" x\"y "C:\trailing dir\\"`},
	}
	for _, tt := range tests {
		if got := WindowsCommandLine(tt.exe, tt.args); got != tt.want {
			t.Errorf("WindowsCommandLine(%q, %q) = %s, want %s", tt.exe, tt.args, got, tt.want)
		}
	}
}

func TestWithDefaults(t *testing.T) {
	s := Spec{HomeDir: "/h"}.WithDefaults()
	if s.Name != "atelier-runner" || s.Label != "dev.atelier.runner" || s.DisplayName != "Atelier Runner" ||
		s.Description != "Runs Atelier Actions jobs on this device" || s.LogDir != "/h/Library/Logs" {
		t.Fatalf("unexpected defaults: %+v", s)
	}
	custom := Spec{Name: "n", Label: "l", DisplayName: "d", Description: "x", HomeDir: "/h", LogDir: "/logs"}
	if got := custom.WithDefaults(); got.Name != "n" || got.Label != "l" || got.LogDir != "/logs" {
		t.Fatalf("defaults overrode explicit values: %+v", got)
	}
}

func TestUnitPathFor(t *testing.T) {
	s := Spec{HomeDir: "/home/alice"}
	cases := []struct {
		goos string
		s    Spec
		want string
	}{
		{"darwin", s, filepath.Join("/home/alice", "Library", "LaunchAgents", "dev.atelier.runner.plist")},
		{"linux", s, filepath.Join("/home/alice", ".config", "systemd", "user", "atelier-runner.service")},
		{"linux", Spec{HomeDir: "/home/alice", System: true}, filepath.Join("/etc/systemd/system", "atelier-runner.service")},
		{"windows", s, ""},
	}
	for _, c := range cases {
		got, err := unitPathFor(c.goos, c.s)
		if err != nil || got != c.want {
			t.Errorf("unitPathFor(%s) = %q, %v; want %q", c.goos, got, err, c.want)
		}
	}
	if _, err := unitPathFor("plan9", s); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("plan9: want unsupported error, got %v", err)
	}
}

func TestInstallValidates(t *testing.T) {
	var out strings.Builder
	err := Install(Spec{Exe: "relative/atelier-runner", HomeDir: t.TempDir()}, Options{DryRun: true, Out: &out})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("want absolute-path error, got %v", err)
	}
}

// listFiles returns every path under dir.
func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p != dir {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestDryRunInstallUninstall(t *testing.T) {
	// Fail rather than touch real service managers if dry-run ever
	// regresses into executing commands.
	orig := runCmd
	runCmd = func(argv []string) ([]byte, error) {
		t.Fatalf("dry run executed a command: %q", argv)
		return nil, nil
	}
	defer func() { runCmd = orig }()

	home := t.TempDir()
	exe := filepath.Join(home, "bin", "atelier-runner")
	s := Spec{
		Exe:     exe,
		Args:    []string{"run", "--config", filepath.Join(home, "runner config.json")},
		Env:     map[string]string{"PATH": "/usr/bin:/bin"},
		HomeDir: home,
		LogDir:  filepath.Join(home, "logs"),
	}

	var out strings.Builder
	err := Install(s, Options{DryRun: true, Out: &out})
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		if err != nil {
			t.Fatalf("Install dry run: %v", err)
		}
	default:
		if err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("want unsupported error, got %v", err)
		}
		return
	}
	got := out.String()

	var wantParts []string
	switch runtime.GOOS {
	case "darwin":
		p, _ := UnitPath(s)
		wantParts = []string{p, LaunchdPlist(s), "launchctl bootout gui/", "launchctl bootstrap gui/", "dev.atelier.runner"}
	case "linux":
		p, _ := UnitPath(s)
		wantParts = []string{p, SystemdUnit(s), "systemctl --user daemon-reload", "systemctl --user enable --now atelier-runner.service", "loginctl enable-linger", "docker group"}
	case "windows":
		wantParts = []string{WindowsServiceConfig(s)}
	}
	for _, w := range wantParts {
		if !strings.Contains(got, w) {
			t.Errorf("install dry-run output missing %q\noutput:\n%s", w, got)
		}
	}

	out.Reset()
	if err := Uninstall(s, Options{DryRun: true, Out: &out}); err != nil {
		t.Fatalf("Uninstall dry run: %v", err)
	}
	got = out.String()
	switch runtime.GOOS {
	case "darwin":
		p, _ := UnitPath(s)
		for _, w := range []string{"launchctl bootout gui/", "rm -f " + shellQuote(p)} {
			if !strings.Contains(got, w) {
				t.Errorf("uninstall dry-run output missing %q\noutput:\n%s", w, got)
			}
		}
	case "linux":
		for _, w := range []string{"systemctl --user disable --now atelier-runner.service", "systemctl --user daemon-reload", "rm -f"} {
			if !strings.Contains(got, w) {
				t.Errorf("uninstall dry-run output missing %q\noutput:\n%s", w, got)
			}
		}
	case "windows":
		if !strings.Contains(got, "delete") {
			t.Errorf("uninstall dry-run output: %s", got)
		}
	}

	if files := listFiles(t, home); len(files) != 0 {
		t.Fatalf("dry run created files: %v", files)
	}
}

func TestRunAsServiceNotService(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("on windows the result depends on how the test is launched")
	}
	called := false
	ok, err := RunAsService("atelier-runner", func(context.Context) error { called = true; return nil })
	if ok || err != nil || called {
		t.Fatalf("RunAsService = %v, %v (called=%v); want false, nil, not called", ok, err, called)
	}
}
