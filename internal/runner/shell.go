package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

func afterGrace() <-chan time.Time { return time.After(5 * time.Second) }

// shellCmd is a resolved shell invocation for a script file.
type shellCmd struct {
	argv    []string // {0} already replaced by the script path
	ext     string
	cmdLine string // Windows cmd.exe raw command line, if any
	wrap    func(script string) string
}

// resolveShell picks the shell for a run step, GitHub style. shell is the
// step's `shell:` (may be empty), goos the target OS, pathEnv the PATH the
// step sees. inContainer means the step runs in a Linux container.
func resolveShell(shell, goos, pathEnv string, inContainer bool) (shellCmd, error) {
	have := func(name string) string {
		if inContainer {
			return name
		}
		p, _ := lookPathIn(name, pathEnv, goos)
		return p
	}
	ps := func(exe string) shellCmd {
		return shellCmd{argv: []string{exe, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", ". '{0}'"}, ext: ".ps1",
			wrap: func(s string) string {
				return "$ErrorActionPreference = 'stop'\r\n" + s + "\r\nif ((Test-Path -LiteralPath variable:\\LASTEXITCODE)) { exit $LASTEXITCODE }\r\n"
			}}
	}
	switch strings.TrimSpace(shell) {
	case "":
		if goos == "windows" && !inContainer {
			if p := have("pwsh"); p != "" {
				return ps(p), nil
			}
			if p := have("powershell"); p != "" {
				return ps(p), nil
			}
			return resolveShell("cmd", goos, pathEnv, inContainer)
		}
		if inContainer {
			// bash when the image has it, else sh; the script path is $0.
			return shellCmd{argv: []string{"sh", "-c", `if command -v bash >/dev/null 2>&1; then exec bash --noprofile --norc -e "$0"; else exec sh -e "$0"; fi`, "{0}"}, ext: ".sh"}, nil
		}
		if p := have("bash"); p != "" {
			return shellCmd{argv: []string{p, "--noprofile", "--norc", "-e", "{0}"}, ext: ".sh"}, nil
		}
		if p := have("sh"); p != "" {
			return shellCmd{argv: []string{p, "-e", "{0}"}, ext: ".sh"}, nil
		}
		return shellCmd{}, fmt.Errorf("no bash or sh found on PATH")
	case "bash":
		if p := have("bash"); p != "" {
			return shellCmd{argv: []string{p, "--noprofile", "--norc", "-eo", "pipefail", "{0}"}, ext: ".sh"}, nil
		}
		return shellCmd{}, fmt.Errorf("shell bash: not found on PATH")
	case "sh":
		if p := have("sh"); p != "" {
			return shellCmd{argv: []string{p, "-e", "{0}"}, ext: ".sh"}, nil
		}
		return shellCmd{}, fmt.Errorf("shell sh: not found on PATH")
	case "pwsh", "powershell":
		if p := have(shell); p != "" {
			return ps(p), nil
		}
		return shellCmd{}, fmt.Errorf("shell %s: not found on PATH", shell)
	case "cmd":
		if goos != "windows" {
			return shellCmd{}, fmt.Errorf("shell cmd is only available on Windows")
		}
		exe := os.Getenv("ComSpec")
		if exe == "" {
			exe = "cmd.exe"
		}
		return shellCmd{argv: []string{exe}, ext: ".cmd", cmdLine: `"` + exe + `" /D /E:ON /V:OFF /S /C "CALL "{0}""`}, nil
	case "python":
		for _, n := range []string{"python3", "python"} {
			if p := have(n); p != "" {
				return shellCmd{argv: []string{p, "{0}"}, ext: ".py"}, nil
			}
		}
		return shellCmd{}, fmt.Errorf("shell python: python3/python not found on PATH")
	}
	// custom: "perl {0}" etc. Split on whitespace only; no shell is involved.
	f := strings.Fields(shell)
	if !strings.Contains(shell, "{0}") {
		f = append(f, "{0}")
	}
	if p := have(f[0]); p != "" {
		f[0] = p
	} else if !inContainer {
		return shellCmd{}, fmt.Errorf("shell %s: %s not found on PATH", shell, f[0])
	}
	return shellCmd{argv: f}, nil
}

// withScript replaces {0} with the script path.
func (s shellCmd) withScript(path string) shellCmd {
	out := s
	out.argv = make([]string, len(s.argv))
	for i, a := range s.argv {
		out.argv[i] = strings.ReplaceAll(a, "{0}", path)
	}
	out.cmdLine = strings.ReplaceAll(s.cmdLine, "{0}", path)
	return out
}

// lookPathIn finds an executable in the given PATH value.
func lookPathIn(name, pathEnv, goos string) (string, error) {
	if strings.ContainsAny(name, `/\`) {
		return exec.LookPath(name)
	}
	sep := string(os.PathListSeparator)
	exts := []string{""}
	if goos == "windows" {
		exts = []string{".exe", ".cmd", ".bat", ".com", ""}
	}
	for _, dir := range strings.Split(pathEnv, sep) {
		if dir == "" {
			continue
		}
		for _, e := range exts {
			p := filepath.Join(dir, name+e)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() && (goos == "windows" || fi.Mode()&0o111 != 0) {
				return p, nil
			}
		}
	}
	if pathEnv == "" && runtime.GOOS == goos {
		return exec.LookPath(name)
	}
	return "", exec.ErrNotFound
}

// envMap is an environment with Windows-aware (case-insensitive) keys.
type envMap map[string]string

func (e envMap) key(k string) string {
	if runtime.GOOS == "windows" {
		for ek := range e {
			if strings.EqualFold(ek, k) {
				return ek
			}
		}
	}
	return k
}

func (e envMap) Set(k, v string) {
	if runtime.GOOS == "windows" {
		delete(e, e.key(k))
	}
	e[k] = v
}

func (e envMap) Get(k string) string { return e[e.key(k)] }

func (e envMap) Clone() envMap {
	out := envMap{}
	for k, v := range e {
		out[k] = v
	}
	return out
}

func (e envMap) Slice() []string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+e[k])
	}
	return out
}

func osEnv() envMap {
	e := envMap{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			e[k] = v
		}
	}
	return e
}

// pathKey is the PATH variable's name in e.
func (e envMap) pathKey() string {
	if k := e.key("PATH"); k != "" {
		return k
	}
	return "PATH"
}

func runtimeGOOS() string { return runtime.GOOS }
