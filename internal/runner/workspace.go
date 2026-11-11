package runner

// Device workspaces: "Open on <this device>" from the Atelier Code page.
//
// With the workspaces capability switched on (opt-in, see
// `atelier-runner workspaces enable`), the runner long-polls
// /workspaces/poll. For each request it:
//
//  1. clones the repository into <root>/<owner>/<repo> (default root
//     ~/Atelier), or fetches if it's already there, using the request's
//     short-lived token only through the environment of that one git command
//     (an http.extraHeader in GIT_CONFIG_*): it's never written to .git/config
//     or a credential store;
//  2. leaves origin at the normal clone URL (SSH when this device is set to
//     use SSH and the server has it, else HTTPS without credentials);
//  3. checks out the branch, without touching uncommitted work;
//  4. if asked, runs one of the open presets from this device's own
//     workspaces.json. The server only sends a preset's name: it can never
//     make the runner run a command of its choosing.
//
// Paths are always inside the root: owner and repository names are checked,
// and symlinks out of the root are refused.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"atelier/internal/actions"
)

// WorkspaceConfig is this device's workspaces setup, kept in workspaces.json
// next to the runner config. Only the person at the device edits it.
type WorkspaceConfig struct {
	// Root is the workspaces folder (default ~/Atelier). "~/" is expanded.
	Root string `json:"root,omitempty"`
	// Presets are the commands "Open with …" may run, by name. {path} is
	// replaced by the workspace folder (appended when it's missing). They run
	// directly, without a shell.
	Presets map[string]string `json:"presets,omitempty"`
	// Remote is what origin points at after a clone: "https" (default) or
	// "ssh" (when the server offers SSH and this device has a key set up).
	Remote string `json:"remote,omitempty"`
}

// WorkspaceConfigPath overrides where workspaces.json is read from (the
// command line sets it next to --config).
var WorkspaceConfigPath string

// DefaultWorkspaceConfigPath is workspaces.json next to the default config.
func DefaultWorkspaceConfigPath() (string, error) {
	p, err := DefaultConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "workspaces.json"), nil
}

// LoadWorkspaceConfig reads workspaces.json; a missing file means defaults.
func LoadWorkspaceConfig(path string) (*WorkspaceConfig, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &WorkspaceConfig{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c WorkspaceConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Validate checks preset names and commands, and the remote.
func (c *WorkspaceConfig) Validate() error {
	for name, cmd := range c.Presets {
		if !validPresetName(name) {
			return fmt.Errorf("preset name %q: use lower-case letters, digits, - and _", name)
		}
		if _, err := presetArgv(cmd, "/x"); err != nil {
			return fmt.Errorf("preset %q: %w", name, err)
		}
	}
	if c.Remote != "" && c.Remote != "https" && c.Remote != "ssh" {
		return errors.New(`remote must be "https" or "ssh"`)
	}
	return nil
}

// Save writes workspaces.json (mode 0600).
func (c *WorkspaceConfig) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RootDir is the absolute workspaces folder.
func (c *WorkspaceConfig) RootDir() (string, error) {
	root := c.Root
	home, _ := os.UserHomeDir()
	if root == "" {
		if home == "" {
			return "", errors.New("no home folder: set root in workspaces.json")
		}
		return filepath.Join(home, "Atelier"), nil
	}
	if root == "~" || strings.HasPrefix(root, "~/") {
		if home == "" {
			return "", errors.New("no home folder to expand ~")
		}
		root = filepath.Join(home, strings.TrimPrefix(root, "~"))
	}
	return filepath.Abs(root)
}

// PresetNames lists the presets, sorted.
func (c *WorkspaceConfig) PresetNames() []string {
	var out []string
	for n := range c.Presets {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// DefaultPresets are offered by `atelier-runner workspaces enable`, for the
// editors found on this device.
func DefaultPresets() map[string]string {
	out := map[string]string{}
	for name, cmd := range map[string]string{"vscode": "code {path}", "cursor": "cursor {path}", "zed": "zed {path}"} {
		if _, err := exec.LookPath(strings.Fields(cmd)[0]); err == nil {
			out[name] = cmd
		}
	}
	switch runtime.GOOS {
	case "darwin":
		out["finder"] = "open {path}"
		if _, err := os.Stat("/Applications/Fork.app"); err == nil {
			out["fork"] = "open -a Fork {path}"
		}
	case "linux":
		if _, err := exec.LookPath("xdg-open"); err == nil {
			out["files"] = "xdg-open {path}"
		}
	case "windows":
		out["explorer"] = "explorer {path}"
	}
	return out
}

var presetNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

func validPresetName(s string) bool { return presetNameRE.MatchString(s) }

// presetArgv splits a preset command into arguments (quotes group words; no
// shell is involved) and puts the workspace path in for {path}.
func presetArgv(cmd, path string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inArg, quote := false, rune(0)
	for _, c := range cmd {
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(c)
		case c == '"' || c == '\'':
			quote, inArg = c, true
		case c == ' ' || c == '\t':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(c)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unclosed quote")
	}
	if inArg {
		args = append(args, cur.String())
	}
	if len(args) == 0 {
		return nil, errors.New("empty command")
	}
	used := false
	for i, a := range args {
		if strings.Contains(a, "{path}") {
			args[i] = strings.ReplaceAll(a, "{path}", path)
			used = used || i > 0
		}
	}
	if strings.Contains(cmd, "{path}") && !used {
		return nil, errors.New("{path} can't be the program itself")
	}
	if !used {
		args = append(args, path)
	}
	return args, nil
}

var wsNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,99}$`)

// workspacePath is <root>/<owner>/<repo>, refusing anything that could end
// up outside root (odd names, symlinks).
func workspacePath(root, owner, repo string) (string, error) {
	for _, n := range []string{owner, repo} {
		if !wsNameRE.MatchString(n) || strings.Contains(n, "..") {
			return "", fmt.Errorf("refusing workspace name %q", n)
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	p := filepath.Join(realRoot, owner, repo)
	for _, part := range []string{filepath.Join(realRoot, owner), p} {
		if fi, err := os.Lstat(part); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing %s: it's a symlink", part)
		}
	}
	rel, err := filepath.Rel(realRoot, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("refusing a path outside %s", realRoot)
	}
	return p, nil
}

// wsGit runs git with extra environment.
func wsGit(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "LC_ALL=C"), env...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("git %s: %v: %s", args[0], err, lastLine(s))
	}
	return s, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// tokenEnv authenticates one git command with the request's token, through
// the environment only (GIT_CONFIG_*), and switches credential helpers off so
// nothing is stored.
func tokenEnv(token string) []string {
	h := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("atelier:"+token))
	return []string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=" + h,
		"GIT_CONFIG_KEY_1=credential.helper", "GIT_CONFIG_VALUE_1=",
	}
}

// checkCloneURL makes sure the clone URL is on this runner's server and
// carries no credentials of its own.
func (r *Runner) checkCloneURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Host == "" {
		return "", errors.New("the server sent an unusable clone URL")
	}
	srv, err := url.Parse(r.Config.ServerURL)
	if err != nil || !strings.EqualFold(srv.Host, u.Host) || srv.Scheme != u.Scheme {
		return "", fmt.Errorf("refusing to clone from %s: it isn't this runner's server", u.Host)
	}
	return u.String(), nil
}

func sameRemote(a, b string) bool {
	norm := func(s string) string { return strings.TrimSuffix(strings.TrimRight(strings.ToLower(s), "/"), ".git") }
	return a != "" && norm(a) == norm(b)
}

// wsOutcome is what preparing a workspace did.
type wsOutcome struct {
	Path    string
	State   string // cloned, updated
	Branch  string // checked out
	Dirty   bool
	Message string
}

// prepareWorkspace clones or updates the repository and checks out the branch.
func (r *Runner) prepareWorkspace(ctx context.Context, wc *WorkspaceConfig, root string, req *actions.WorkspaceRequest,
	progress func(state, msg string)) (*wsOutcome, error) {
	p, err := workspacePath(root, req.Repo.Owner, req.Repo.Name)
	if err != nil {
		return nil, err
	}
	if req.Branch == "" || strings.HasPrefix(req.Branch, "-") {
		return nil, fmt.Errorf("refusing branch name %q", req.Branch)
	}
	if _, err := wsGit(ctx, "", nil, "check-ref-format", "--branch", req.Branch); err != nil {
		return nil, fmt.Errorf("refusing branch name %q", req.Branch)
	}
	cloneURL, err := r.checkCloneURL(req.CloneURL)
	if err != nil {
		return nil, err
	}
	origin := cloneURL
	if wc.Remote == "ssh" && strings.HasPrefix(req.SSHURL, "ssh://") {
		origin = req.SSHURL
	}
	auth := tokenEnv(req.Token)
	out := &wsOutcome{Path: p, Branch: req.Branch}

	entries, statErr := os.ReadDir(p)
	if errors.Is(statErr, os.ErrNotExist) || (statErr == nil && len(entries) == 0) {
		progress(actions.WorkspaceCloning, "Cloning into "+p)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		if _, err := wsGit(cctx, filepath.Dir(p), auth, "clone", "--branch", req.Branch, "--origin", "origin", "--", cloneURL, p); err != nil {
			return nil, err
		}
		if _, err := wsGit(ctx, p, nil, "remote", "set-url", "origin", origin); err != nil {
			return nil, err
		}
		out.State, out.Message = actions.WorkspaceCloned, "Cloned "+req.Branch
		return out, nil
	}
	if statErr != nil {
		return nil, statErr
	}

	// Already there: it must be this repository.
	top, err := wsGit(ctx, p, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s exists and isn't a git repository; move it away first", p)
	}
	if realTop, _ := filepath.EvalSymlinks(top); realTop != p && top != p {
		return nil, fmt.Errorf("%s exists but isn't the top of a git repository", p)
	}
	cur, _ := wsGit(ctx, p, nil, "remote", "get-url", "origin")
	if !sameRemote(cur, cloneURL) && !sameRemote(cur, req.SSHURL) {
		return nil, fmt.Errorf("%s holds a different repository (origin %s)", p, cur)
	}
	progress("fetching", "Fetching "+req.Branch)
	fctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if _, err := wsGit(fctx, p, auth, "fetch", "--no-tags", "--", cloneURL,
		"+refs/heads/"+req.Branch+":refs/remotes/origin/"+req.Branch); err != nil {
		return nil, err
	}
	status, _ := wsGit(ctx, p, nil, "status", "--porcelain")
	out.Dirty = status != ""
	head, _ := wsGit(ctx, p, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
	out.State = actions.WorkspaceUpdated
	upstream := "origin/" + req.Branch
	switch {
	case head != req.Branch && out.Dirty:
		out.Branch = head
		out.Message = "Fetched " + req.Branch + ", but stayed on " + head + ": the workspace has uncommitted changes."
		return out, nil
	case head != req.Branch:
		if _, err := wsGit(ctx, p, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+req.Branch); err == nil {
			if _, err := wsGit(ctx, p, nil, "checkout", "--quiet", req.Branch, "--"); err != nil {
				return nil, err
			}
		} else {
			if _, err := wsGit(ctx, p, nil, "checkout", "--quiet", "-b", req.Branch, upstream, "--"); err != nil {
				return nil, err
			}
			_, _ = wsGit(ctx, p, nil, "branch", "--quiet", "--set-upstream-to="+upstream, req.Branch)
			out.Message = "Checked out " + req.Branch
			return out, nil
		}
	case out.Dirty:
		out.Message = "Fetched " + req.Branch + "; not fast-forwarded because the workspace has uncommitted changes."
		return out, nil
	}
	if _, err := wsGit(ctx, p, nil, "merge", "--ff-only", "--quiet", upstream); err != nil {
		out.Message = "Fetched " + req.Branch + "; it has local commits, so it wasn't fast-forwarded."
		return out, nil
	}
	out.Message = "Updated " + req.Branch
	return out, nil
}

// openPreset runs a preset from this device's config. It never runs anything
// the server named beyond picking the preset.
func openPreset(wc *WorkspaceConfig, name, path string) error {
	cmd, ok := wc.Presets[name]
	if !ok {
		return fmt.Errorf("this device has no open preset called %q", name)
	}
	argv, err := presetArgv(cmd, path)
	if err != nil {
		return err
	}
	c := exec.Command(argv[0], argv[1:]...)
	c.Dir = path
	if err := c.Start(); err != nil {
		return fmt.Errorf("open with %s: %w", name, err)
	}
	// Launchers like `code` or `open` return at once: report their failure.
	// Anything still running after a moment is left to run (an editor).
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("open with %s: %w", name, err)
		}
	case <-time.After(3 * time.Second):
	}
	return nil
}

// ---------- the loop ----------

func (r *Runner) workspaceLoop(ctx context.Context) {
	path := WorkspaceConfigPath
	if path == "" {
		var err error
		if path, err = DefaultWorkspaceConfigPath(); err != nil {
			r.logger().Error("workspaces: no config folder", "err", err)
			return
		}
	}
	wc, err := LoadWorkspaceConfig(path)
	if err != nil {
		r.logger().Error("workspaces are off: bad config", "err", err)
		return
	}
	if err := r.RunWorkspaces(ctx, wc); err != nil {
		r.logger().Error("workspaces stopped", "err", err)
	}
}

// RunWorkspaces long-polls for workspace requests until ctx is done.
func (r *Runner) RunWorkspaces(ctx context.Context, wc *WorkspaceConfig) error {
	root, err := wc.RootDir()
	if err != nil {
		return err
	}
	// One spelling of the root everywhere (macOS /var is /private/var).
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	maxBackoff := r.MaxBackoff
	if maxBackoff == 0 {
		maxBackoff = 60 * time.Second
	}
	r.logger().Info("workspaces on", "root", root, "presets", wc.PresetNames())
	r.syncWorkspaces(ctx, root)
	lastSync := time.Now()
	failures := 0
	for ctx.Err() == nil {
		started := time.Now()
		req, err := r.Client.WorkspacePoll(ctx, actions.WorkspacePollRequest{Capabilities: r.Config.Capabilities,
			Version: r.Version, Presets: wc.PresetNames(), Root: root, SSH: wc.Remote == "ssh"})
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			failures++
			d := backoff(failures, maxBackoff)
			var he *HTTPError
			if errors.As(err, &he) && (he.Status == 401 || he.Status == 403 || he.Status == 404) {
				d = maxBackoff
			}
			r.logger().Warn("workspace poll failed", "err", err, "retry_in", d.Round(time.Millisecond))
			sleepCtx(ctx, d)
			continue
		}
		failures = 0
		if req != nil {
			r.handleWorkspace(ctx, wc, root, req)
			r.syncWorkspaces(ctx, root)
			lastSync = time.Now()
			continue
		}
		if time.Since(lastSync) > 10*time.Minute {
			r.syncWorkspaces(ctx, root)
			lastSync = time.Now()
		}
		if time.Since(started) < time.Second {
			sleepCtx(ctx, time.Second)
		}
	}
	return nil
}

func (r *Runner) handleWorkspace(ctx context.Context, wc *WorkspaceConfig, root string, req *actions.WorkspaceRequest) {
	log := r.logger().With("workspace", req.ID, "repo", req.Repo.Owner+"/"+req.Repo.Name, "branch", req.Branch)
	report := func(st actions.WorkspaceStatus) {
		if err := r.Client.WorkspaceStatus(ctx, req.ID, st); err != nil {
			log.Warn("couldn't report workspace status", "err", err)
		}
	}
	fail := func(err error) {
		log.Warn("workspace failed", "err", err)
		report(actions.WorkspaceStatus{State: actions.WorkspaceFailed, Message: err.Error(), Final: true})
	}
	// Refuse an unknown preset before touching the disk.
	if req.Preset != "" {
		if _, ok := wc.Presets[req.Preset]; !ok {
			fail(fmt.Errorf("this device has no open preset called %q", req.Preset))
			return
		}
	}
	out, err := r.prepareWorkspace(ctx, wc, root, req, func(state, msg string) {
		report(actions.WorkspaceStatus{State: state, Message: msg})
	})
	if err != nil {
		fail(err)
		return
	}
	st := actions.WorkspaceStatus{State: out.State, Message: out.Message, Path: out.Path, Branch: out.Branch, Dirty: out.Dirty}
	if req.Preset != "" {
		if err := openPreset(wc, req.Preset, out.Path); err != nil {
			fail(err)
			return
		}
		st.State, st.Message = actions.WorkspaceOpened, out.Message+". Opened with "+req.Preset+"."
	}
	markOpened(root, req.Repo.Owner, req.Repo.Name)
	st.Final = true
	log.Info("workspace ready", "state", st.State, "path", out.Path)
	report(st)
}

// ---------- the list of workspaces ----------

const wsStateFile = ".atelier-workspaces.json"

var wsStateMu sync.Mutex

// markOpened remembers when Atelier last opened a workspace.
func markOpened(root, owner, repo string) {
	wsStateMu.Lock()
	defer wsStateMu.Unlock()
	st := readWsState(root)
	st[owner+"/"+repo] = time.Now().UTC()
	b, _ := json.MarshalIndent(st, "", "  ")
	_ = os.WriteFile(filepath.Join(root, wsStateFile), b, 0o600)
}

func readWsState(root string) map[string]time.Time {
	st := map[string]time.Time{}
	if b, err := os.ReadFile(filepath.Join(root, wsStateFile)); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

// ListWorkspaces reports the git repositories at <root>/<owner>/<repo>.
func ListWorkspaces(ctx context.Context, root string) []actions.WorkspaceInfo {
	wsStateMu.Lock()
	opened := readWsState(root)
	wsStateMu.Unlock()
	var out []actions.WorkspaceInfo
	owners, _ := os.ReadDir(root)
	for _, o := range owners {
		if !o.IsDir() || !wsNameRE.MatchString(o.Name()) {
			continue
		}
		repos, _ := os.ReadDir(filepath.Join(root, o.Name()))
		for _, rp := range repos {
			p := filepath.Join(root, o.Name(), rp.Name())
			if !rp.IsDir() || !wsNameRE.MatchString(rp.Name()) {
				continue
			}
			if _, err := os.Stat(filepath.Join(p, ".git")); err != nil {
				continue
			}
			info := actions.WorkspaceInfo{Owner: o.Name(), Repo: rp.Name(), Path: p, LastOpened: opened[o.Name()+"/"+rp.Name()]}
			info.Branch, _ = wsGit(ctx, p, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
			info.Head, _ = wsGit(ctx, p, nil, "rev-parse", "--verify", "--quiet", "HEAD")
			if s, err := wsGit(ctx, p, nil, "status", "--porcelain"); err == nil {
				info.Dirty = s != ""
			}
			out = append(out, info)
			if len(out) >= 500 {
				return out
			}
		}
	}
	return out
}

func (r *Runner) syncWorkspaces(ctx context.Context, root string) {
	list := ListWorkspaces(ctx, root)
	if list == nil {
		list = []actions.WorkspaceInfo{}
	}
	if err := r.Client.WorkspaceSync(ctx, actions.WorkspaceSyncRequest{Workspaces: list}); err != nil && ctx.Err() == nil {
		r.logger().Warn("couldn't report workspaces", "err", err)
	}
}

// ---------- client ----------

// WorkspacePoll waits for a workspace request (nil when none came).
func (c *Client) WorkspacePoll(ctx context.Context, req actions.WorkspacePollRequest) (*actions.WorkspaceRequest, error) {
	var out actions.WorkspacePollResponse
	if err := c.do(ctx, "/workspaces/poll", req, &out); err != nil {
		return nil, err
	}
	return out.Request, nil
}

func (c *Client) WorkspaceStatus(ctx context.Context, id int64, st actions.WorkspaceStatus) error {
	return c.do(ctx, fmt.Sprintf("/workspaces/%d/status", id), st, nil)
}

func (c *Client) WorkspaceSync(ctx context.Context, req actions.WorkspaceSyncRequest) error {
	return c.do(ctx, "/workspaces/sync", req, nil)
}
