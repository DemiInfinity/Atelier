package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func TestConfigSavePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atelier-runner", "config.json")
	c := &Config{ServerURL: "https://git.example.com", RunnerID: 3, RunnerToken: "rnr_x", Name: "mac", Capabilities: []string{"jobs"}}
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("config mode = %v", fi.Mode().Perm())
		}
		di, _ := os.Stat(filepath.Dir(path))
		if di.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v", di.Mode().Perm())
		}
		// a loosened file is tightened on load
		os.Chmod(path, 0o644)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Errorf("round trip: %+v", got)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("not tightened: %v", fi.Mode().Perm())
		}
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json")); err == nil || !strings.Contains(err.Error(), "register") {
		t.Errorf("missing config error = %v", err)
	}
}

func TestRegisterSavesConfig(t *testing.T) {
	f := newFakeServer(t)
	cfg, err := Register(context.Background(), f.srv.URL, "reg-once", "mac", []string{"Mac", "self-hosted"}, []string{"jobs", "docker"}, "1.0")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunnerToken != f.runnerTok || cfg.RunnerID != 7 || cfg.ServerURL != f.srv.URL {
		t.Errorf("cfg = %+v", cfg)
	}
	req := f.registered[0]
	if req.OS != runtime.GOOS || req.Protocol != 1 || req.Labels[0] != "self-hosted" || req.Labels[1] != OSLabel() || !strings.Contains(strings.Join(req.Labels, ","), "mac") {
		t.Errorf("register request = %+v", req)
	}
	// one-time token
	if _, err := Register(context.Background(), f.srv.URL, "reg-once", "mac", nil, nil, "1.0"); err == nil {
		t.Error("token reused")
	}
}

func TestLabels(t *testing.T) {
	got := allLabels([]string{"Docker", " gpu ", "", "linux"}, "linux", "amd64")
	want := []string{"self-hosted", "linux", "x64", "docker", "gpu"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v", got)
	}
	if l := allLabels(nil, "darwin", "arm64"); !reflect.DeepEqual(l, []string{"self-hosted", "macos", "arm64"}) {
		t.Errorf("mac labels = %v", l)
	}
	if l := allLabels(nil, "windows", "amd64"); l[1] != "windows" {
		t.Errorf("windows labels = %v", l)
	}
	if _, err := ParseCaps("jobs,nope"); err == nil {
		t.Error("unknown capability accepted")
	}
	if c, _ := ParseCaps("jobs, docker,jobs"); !reflect.DeepEqual(c, []string{"jobs", "docker"}) {
		t.Errorf("caps = %v", c)
	}
}

func TestMasker(t *testing.T) {
	m := NewMasker()
	m.Add("hunter2-secret")
	m.Add("multi\nline-secret-part")
	m.Add("")
	tok := "atl_tok_987654321"
	m.Add(tok)
	basic := base64.StdEncoding.EncodeToString([]byte("atelier:" + tok))
	cases := map[string]string{
		"pw=hunter2-secret!": "pw=***!",
		"b64 " + base64.StdEncoding.EncodeToString([]byte("hunter2-secret")): "b64 ***",
		"line-secret-part here":         "*** here",
		"AUTHORIZATION: basic " + basic: "",
		`json "hunter2-secret"`:         `json "***"`,
		"nothing to see":                "nothing to see",
	}
	for in, want := range cases {
		got := m.Mask(in)
		if want == "" {
			if strings.Contains(got, base64.RawStdEncoding.EncodeToString([]byte(tok))[:8]) || strings.Contains(got, tok) {
				t.Errorf("Mask(%q) = %q still shows the token", in, got)
			}
			continue
		}
		if got != want {
			t.Errorf("Mask(%q) = %q, want %q", in, got, want)
		}
	}
	if m.MaskMap(map[string]string{"a": "x hunter2-secret"})["a"] != "x ***" {
		t.Error("MaskMap")
	}
}

func TestParseCommand(t *testing.T) {
	n, p, v, ok := parseCommand("::set-output name=a%3Ab,x=1::val%0Aue%25")
	if !ok || n != "set-output" || p["name"] != "a:b" || p["x"] != "1" || v != "val\nue%" {
		t.Errorf("got %q %v %q %v", n, p, v, ok)
	}
	if _, _, _, ok := parseCommand("::not a command"); ok {
		t.Error("parsed garbage")
	}
	if n, _, v, ok := parseCommand("  ::add-mask::abc"); !ok || n != "add-mask" || v != "abc" {
		t.Error("leading space")
	}
}

func TestParseKVFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	os.WriteFile(p, []byte("A=1\r\nB=x=y\nC<<EOF\nl1\nl2\nEOF\n\nD<<X\nX\n"), 0o644)
	kv, err := parseKVFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"A", "1"}, {"B", "x=y"}, {"C", "l1\nl2"}, {"D", ""}}
	if !reflect.DeepEqual(kv, want) {
		t.Errorf("kv = %q", kv)
	}
	os.WriteFile(p, []byte("E<<EOF\nnever closed\n"), 0o644)
	if _, err := parseKVFile(p); err == nil {
		t.Error("unterminated heredoc accepted")
	}
	if kv, err := parseKVFile(filepath.Join(t.TempDir(), "none")); err != nil || kv != nil {
		t.Error("missing file")
	}
}

func TestHashFiles(t *testing.T) {
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, "a", "b"), 0o755)
	os.MkdirAll(filepath.Join(ws, ".git"), 0o755)
	os.WriteFile(filepath.Join(ws, "a", "b", "go.sum"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(ws, "go.sum"), []byte("y"), 0o644)
	os.WriteFile(filepath.Join(ws, "skip.txt"), []byte("z"), 0o644)
	all, _ := hashFiles(ws, []string{"**/go.sum"})
	top, _ := hashFiles(ws, []string{"go.sum"})
	neg, _ := hashFiles(ws, []string{"**/go.sum", "!a/**"})
	none, _ := hashFiles(ws, []string{"*.nothing"})
	if len(all) != 64 || all == top || none != "" || neg != top {
		t.Errorf("all=%s top=%s neg=%s none=%q", all, top, neg, none)
	}
	for g, cases := range map[string]map[string]bool{
		"**/*.js": {"a.js": true, "x/y/a.js": true, "a.ts": false},
		"src/*":   {"src/a": true, "src/a/b": false},
		"a?c":     {"abc": true, "a/c": false},
	} {
		re := globRegexp(g)
		for s, want := range cases {
			if re.MatchString(s) != want {
				t.Errorf("glob %q on %q = %v", g, s, !want)
			}
		}
	}
}

func TestStripCredentials(t *testing.T) {
	cases := map[string]string{
		"https://user:tok@github.com/a/b.git":      "https://github.com/a/b.git",
		"https://ghp_abc@github.com/a/b.git":       "https://github.com/a/b.git",
		"ssh://git:pw@host:22/a.git":               "ssh://git@host:22/a.git",
		"git@github.com:a/b.git":                   "git@github.com:a/b.git",
		"https://host/a.git?access_token=x&keep=1": "https://host/a.git?keep=1",
		"/local/path":                              "/local/path",
	}
	for in, want := range cases {
		if got := StripCredentials(in); got != want {
			t.Errorf("StripCredentials(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiscoverDepthAndSkips(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil && runtime.GOOS != "windows" {
		if _, err := lookPathIn("git", os.Getenv("PATH"), runtime.GOOS); err != nil {
			t.Skip("git not installed")
		}
	}
	root := t.TempDir()
	mk := func(rel string) {
		d := filepath.Join(root, filepath.FromSlash(rel))
		gitCmd(t, root, "init", "-q", d)
	}
	mk("one")
	mk("x/y/z/four")       // depth 4: found
	mk("x/y/z/w/five")     // depth 5: not found
	mk("node_modules/dep") // skipped
	mk("one/inner")        // inside a repo: not descended
	repos := Discover(context.Background(), []string{root, filepath.Join(root, "missing")})
	var got []string
	for _, r := range repos {
		rel, _ := filepath.Rel(root, r.Path)
		got = append(got, filepath.ToSlash(rel))
	}
	want := []string{"one", "x/y/z/four"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("found %v, want %v", got, want)
	}
	if repos[0].Dirty {
		t.Error("empty repo reported dirty")
	}
}

func TestResolveShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix shells")
	}
	path := os.Getenv("PATH")
	sh, err := resolveShell("", runtime.GOOS, path, false)
	if err != nil || !strings.HasSuffix(sh.argv[0], "bash") || sh.ext != ".sh" {
		t.Errorf("default = %+v %v", sh, err)
	}
	// no bash on PATH: fall back to sh
	dir := t.TempDir()
	os.Symlink("/bin/sh", filepath.Join(dir, "sh"))
	sh, err = resolveShell("", runtime.GOOS, dir, false)
	if err != nil || filepath.Base(sh.argv[0]) != "sh" {
		t.Errorf("fallback = %+v %v", sh, err)
	}
	sh, _ = resolveShell("bash", runtime.GOOS, path, false)
	if !reflect.DeepEqual(sh.argv[len(sh.argv)-3:], []string{"-eo", "pipefail", "{0}"}) {
		t.Errorf("bash = %v", sh.argv)
	}
	if _, err := resolveShell("cmd", "linux", path, false); err == nil {
		t.Error("cmd on linux")
	}
	sh, _ = resolveShell("perl -w {0}", runtime.GOOS, "/usr/bin:/bin", false)
	if w := sh.withScript("/tmp/s"); len(w.argv) != 3 || w.argv[2] != "/tmp/s" {
		t.Errorf("custom = %v", w.argv)
	}
	if sh, _ := resolveShell("", "linux", "", true); sh.argv[0] != "sh" || sh.argv[len(sh.argv)-1] != "{0}" {
		t.Errorf("container = %v", sh.argv)
	}
	if sh, _ := resolveShell("python", runtime.GOOS, path, false); sh.ext != ".py" && sh.argv != nil {
		t.Errorf("python = %+v", sh)
	}
}

func TestResolveShellWindows(t *testing.T) {
	// pwsh → powershell → cmd fallback, using fake executables on a fake PATH
	if runtime.GOOS != "windows" {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "powershell.exe"), nil, 0o755)
		sh, err := resolveShell("", "windows", dir, false)
		if err != nil || !strings.HasSuffix(sh.argv[0], "powershell.exe") || sh.ext != ".ps1" || sh.wrap == nil {
			t.Errorf("windows default = %+v %v", sh, err)
		}
		os.WriteFile(filepath.Join(dir, "pwsh.exe"), nil, 0o755)
		if sh, _ := resolveShell("", "windows", dir, false); !strings.HasSuffix(sh.argv[0], "pwsh.exe") {
			t.Errorf("pwsh preferred: %v", sh.argv)
		}
		sh, _ = resolveShell("", "windows", t.TempDir(), false)
		if sh.ext != ".cmd" || !strings.Contains(sh.withScript(`C:\t\s.cmd`).cmdLine, `CALL "C:\t\s.cmd"`) {
			t.Errorf("cmd fallback = %+v", sh)
		}
	}
}

func TestSplitArgsAndUses(t *testing.T) {
	a, err := splitArgs(`sh -c "echo hi there" 'x y' z`)
	if err != nil || !reflect.DeepEqual(a, []string{"sh", "-c", "echo hi there", "x y", "z"}) {
		t.Errorf("args = %q %v", a, err)
	}
	if _, err := splitArgs(`"open`); err == nil {
		t.Error("unterminated quote")
	}
	r, err := parseUses("owner/repo/sub/dir@" + fakeSHA)
	if err != nil || r.owner != "owner" || r.repo != "repo" || r.sub != "sub/dir" || r.ref != fakeSHA {
		t.Errorf("uses = %+v %v", r, err)
	}
	for _, bad := range []string{"repo@x", "a/b", "a/../b@x", "a/b/..@x"} {
		if _, err := parseUses(bad); err == nil {
			t.Errorf("parseUses(%q) accepted", bad)
		}
	}
	if !isCheckout("Actions/Checkout@abc") || isCheckout("actions/checkoutx@1") {
		t.Error("isCheckout")
	}
}

func TestExtractRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	top := "x-" + fakeSHA + "/"
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: top + "../../evil", Size: 1, Mode: 0o644})
	tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	if err := extractActionTarball(&buf, t.TempDir(), fakeSHA); err == nil {
		t.Error("path traversal accepted")
	}
}

func TestCloneURLFor(t *testing.T) {
	u, err := cloneURLFor("https://atelier:x@git.example.com/demi/app.git", "demi/app", "elyndra/lib")
	if err != nil || u != "https://git.example.com/elyndra/lib.git" {
		t.Errorf("u = %q %v", u, err)
	}
	if _, err := cloneURLFor("https://h/demi/app.git", "demi/app", "../x"); err == nil {
		t.Error("bad repo accepted")
	}
}

func TestBackoff(t *testing.T) {
	for n := 1; n < 40; n++ {
		d := backoff(n, 30*time.Second)
		if d <= 0 || d > 30*time.Second {
			t.Fatalf("backoff(%d) = %v", n, d)
		}
	}
	if backoff(1, time.Minute) > time.Second {
		t.Error("first backoff too long")
	}
}
