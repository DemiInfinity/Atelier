package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// step is one action of an install or uninstall plan. Exactly one of the
// action fields is set.
type step struct {
	mkdir     string   // create directory (0755)
	writePath string   // write writeData to writePath (0644)
	writeData string   // file contents
	remove    string   // remove file (missing is fine)
	argv      []string // command to run, argv[0] looked up in PATH
	ignoreErr bool     // command failure is not fatal
	note      string   // only printed
}

func (st step) describe() string {
	switch {
	case st.mkdir != "":
		return "mkdir -p " + shellQuote(st.mkdir)
	case st.writePath != "":
		return "write " + shellQuote(st.writePath) + " (mode 0644)"
	case st.remove != "":
		return "rm -f " + shellQuote(st.remove)
	case len(st.argv) > 0:
		s := shellJoin(st.argv)
		if st.ignoreErr {
			s += "    # errors ignored"
		}
		return s
	default:
		return "# " + st.note
	}
}

// shellQuote quotes v for display as a POSIX shell word. Commands are never
// run through a shell; this is only for readable dry-run output.
func shellQuote(v string) string {
	if v == "" {
		return "''"
	}
	safe := true
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@%+,", r)) {
			safe = false
			break
		}
	}
	if safe {
		return v
	}
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

// printPlan writes a dry-run description of steps.
func printPlan(w io.Writer, title string, steps []step) {
	fmt.Fprintf(w, "Dry run: %s (nothing will be changed)\n", title)
	for _, st := range steps {
		if st.writePath != "" {
			fmt.Fprintf(w, "\nWould write %s (mode 0644):\n", st.writePath)
			fmt.Fprintln(w, "-----")
			fmt.Fprint(w, st.writeData)
			if !strings.HasSuffix(st.writeData, "\n") {
				fmt.Fprintln(w)
			}
			fmt.Fprintln(w, "-----")
			fmt.Fprintln(w)
			continue
		}
		if st.note != "" {
			fmt.Fprintf(w, "Note: %s\n", st.note)
			continue
		}
		fmt.Fprintf(w, "Would run: %s\n", st.describe())
	}
}

// execute performs steps in order.
func execute(w io.Writer, steps []step) error {
	for _, st := range steps {
		switch {
		case st.mkdir != "":
			if err := os.MkdirAll(st.mkdir, 0o755); err != nil {
				return fmt.Errorf("service: %w", err)
			}
		case st.writePath != "":
			if err := writeFile(st.writePath, st.writeData); err != nil {
				return err
			}
			fmt.Fprintf(w, "Wrote %s\n", st.writePath)
		case st.remove != "":
			if err := os.Remove(st.remove); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("service: %w", err)
			}
			fmt.Fprintf(w, "Removed %s\n", st.remove)
		case len(st.argv) > 0:
			out, err := runCmd(st.argv)
			if err != nil && !st.ignoreErr {
				return fmt.Errorf("service: %s: %w%s", shellJoin(st.argv), err, indentOutput(out))
			}
			if err == nil {
				fmt.Fprintf(w, "Ran %s\n", shellJoin(st.argv))
			}
		case st.note != "":
			fmt.Fprintf(w, "Note: %s\n", st.note)
		}
	}
	return nil
}

func indentOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	return "\n  " + strings.ReplaceAll(s, "\n", "\n  ")
}

// writeFile writes data atomically-ish (temp file + rename) with mode 0644.
func writeFile(p, data string) error {
	dir := filepath.Dir(p)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(p)+".tmp-*")
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if _, err := tmp.WriteString(data); err != nil {
		tmp.Close()
		return fmt.Errorf("service: write %s: %w", p, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("service: write %s: %w", p, err)
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	if err := os.Rename(name, p); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	return nil
}

// runCmd runs argv directly (never through a shell) and returns combined
// output. It is a variable so tests could stub it; the package tests only
// use dry-run and never run service managers.
var runCmd = func(argv []string) ([]byte, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

// ---------------------------------------------------------------- plans

func launchdDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func darwinInstallPlan(s Spec) ([]step, error) {
	p, err := unitPathFor("darwin", s)
	if err != nil {
		return nil, err
	}
	steps := []step{{mkdir: filepath.Dir(p)}}
	if s.LogDir != "" {
		steps = append(steps, step{mkdir: s.LogDir})
	}
	dom := launchdDomain()
	steps = append(steps,
		step{writePath: p, writeData: LaunchdPlist(s)},
		step{argv: []string{"launchctl", "bootout", dom + "/" + s.Label}, ignoreErr: true},
		step{argv: []string{"launchctl", "bootstrap", dom, p}},
	)
	if s.LogDir != "" {
		out, errp := logPaths(s)
		steps = append(steps, step{note: "logs: " + out + " and " + errp})
	}
	return steps, nil
}

func darwinUninstallPlan(s Spec) ([]step, error) {
	p, err := unitPathFor("darwin", s)
	if err != nil {
		return nil, err
	}
	return []step{
		{argv: []string{"launchctl", "bootout", launchdDomain() + "/" + s.Label}, ignoreErr: true},
		{remove: p},
	}, nil
}

func systemctl(s Spec, args ...string) []string {
	argv := []string{"systemctl"}
	if !s.System {
		argv = append(argv, "--user")
	}
	return append(argv, args...)
}

func linuxInstallPlan(s Spec) ([]step, error) {
	p, err := unitPathFor("linux", s)
	if err != nil {
		return nil, err
	}
	steps := []step{
		{mkdir: filepath.Dir(p)},
		{writePath: p, writeData: SystemdUnit(s)},
		{argv: systemctl(s, "daemon-reload")},
		{argv: systemctl(s, "enable", "--now", s.Name+".service")},
	}
	if !s.System {
		steps = append(steps, step{note: "run `loginctl enable-linger $USER` to keep the user unit running after you log out"})
	}
	steps = append(steps,
		step{note: "jobs that use Docker need the service user in the docker group (`sudo usermod -aG docker <user>`, then log in again)"},
		step{note: "logs: journalctl " + strings.Join(systemctl(s)[1:], " ") + " -u " + s.Name + " -f"},
	)
	return steps, nil
}

func linuxUninstallPlan(s Spec) ([]step, error) {
	p, err := unitPathFor("linux", s)
	if err != nil {
		return nil, err
	}
	return []step{
		{argv: systemctl(s, "disable", "--now", s.Name+".service"), ignoreErr: true},
		{remove: p},
		{argv: systemctl(s, "daemon-reload")},
	}, nil
}

// ---------------------------------------------------------------- API

// Install writes the service definition for the current OS, registers it
// and starts it. Re-running Install replaces an existing installation.
func Install(s Spec, o Options) error {
	s = s.WithDefaults()
	if err := s.validate(); err != nil {
		return err
	}
	w := o.out()
	var steps []step
	var err error
	switch runtime.GOOS {
	case "darwin":
		steps, err = darwinInstallPlan(s)
	case "linux":
		if s.User != "" && !s.System {
			return errors.New("service: User is only valid for a system unit (set System)")
		}
		steps, err = linuxInstallPlan(s)
	case "windows":
		if o.DryRun {
			fmt.Fprintln(w, "Dry run: install Windows service (nothing will be changed)")
			fmt.Fprintln(w)
			fmt.Fprint(w, WindowsServiceConfig(s))
			fmt.Fprintln(w)
			fmt.Fprintf(w, "Would create (or update) service %q in the Service Control Manager, set its recovery actions, then start it.\n", s.Name)
			return nil
		}
		return windowsInstall(s, w)
	default:
		return unsupported(runtime.GOOS)
	}
	if err != nil {
		return err
	}
	if o.DryRun {
		printPlan(w, "install "+s.Name, steps)
		return nil
	}
	return execute(w, steps)
}

// Uninstall stops the service, unregisters it and removes its definition.
// Uninstalling a service that is not installed is not an error.
func Uninstall(s Spec, o Options) error {
	s = s.WithDefaults()
	w := o.out()
	var steps []step
	var err error
	switch runtime.GOOS {
	case "darwin":
		steps, err = darwinUninstallPlan(s)
	case "linux":
		steps, err = linuxUninstallPlan(s)
	case "windows":
		if o.DryRun {
			fmt.Fprintln(w, "Dry run: uninstall Windows service (nothing will be changed)")
			fmt.Fprintf(w, "Would stop service %q (waiting up to %ds), then delete it from the Service Control Manager.\n", s.Name, windowsStopWaitSeconds)
			return nil
		}
		return windowsUninstall(s, w)
	default:
		return unsupported(runtime.GOOS)
	}
	if err != nil {
		return err
	}
	if o.DryRun {
		printPlan(w, "uninstall "+s.Name, steps)
		return nil
	}
	return execute(w, steps)
}

// Status reports, best effort, whether the service is installed and
// running. It never modifies anything.
func Status(s Spec) (string, error) {
	s = s.WithDefaults()
	switch runtime.GOOS {
	case "darwin":
		p, err := unitPathFor("darwin", s)
		if err != nil {
			return "", err
		}
		installed := fileExists(p)
		target := launchdDomain() + "/" + s.Label
		out, err := runCmd([]string{"launchctl", "print", target})
		if err != nil {
			if installed {
				return fmt.Sprintf("installed (%s) but not loaded in launchd", p), nil
			}
			return "not installed", nil
		}
		state := "loaded"
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(line, "state = "); ok {
				state = v
				break
			}
		}
		if !installed {
			return fmt.Sprintf("loaded in launchd as %s (state: %s) but %s is missing", target, state, p), nil
		}
		return fmt.Sprintf("installed (%s), %s", p, state), nil
	case "linux":
		p, err := unitPathFor("linux", s)
		if err != nil {
			return "", err
		}
		unit := s.Name + ".service"
		active, _ := runCmd(systemctl(s, "is-active", unit))
		enabled, _ := runCmd(systemctl(s, "is-enabled", unit))
		a := firstLine(active, "unknown")
		e := firstLine(enabled, "unknown")
		if !fileExists(p) {
			return fmt.Sprintf("not installed (%s missing; active: %s)", p, a), nil
		}
		return fmt.Sprintf("installed (%s), enabled: %s, active: %s", p, e, a), nil
	case "windows":
		return windowsStatus(s)
	default:
		return "", unsupported(runtime.GOOS)
	}
}

func firstLine(b []byte, def string) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return def
	}
	return s
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
