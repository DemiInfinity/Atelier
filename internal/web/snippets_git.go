package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"atelier/internal/backup"
	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// Snippet storage. Every snippet is a bare git repository at
// <data>/snippets/<uid[:2]>/<uid>.git, kept apart from <data>/repos so it never
// shows up in repository listings, search, Actions or backups-as-repos. The
// two-character shard plays the "owner" part of a gitrepo.Store path, so the
// reading helpers (Tree, Blob, Log, Show, Archive) and git http-backend are
// reused as they are.
//
// Snippet repos get no server-side hooks: branch rules and push protection
// belong to real repositories.

const (
	snippetDir       = "snippets"
	snippetBranch    = "main"
	snippetMaxFiles  = 20
	snippetMaxBytes  = 1 << 20 // per file, from the web editor
	snippetMaxName   = 100
	snippetMaxDesc   = 300
	snippetUIDLength = 20
)

func init() {
	// Snapshots bundle every snippet repository, like <data>/repos, so a push
	// landing mid-snapshot can't leave an inconsistent copy (issue #12).
	backup.RegisterGitDataDir(snippetDir)
}

var snippetUIDRe = regexp.MustCompile(`^[0-9a-f]{20}$`)

func validSnippetUID(s string) bool { return snippetUIDRe.MatchString(s) }

func newSnippetUID() string {
	b := make([]byte, snippetUIDLength/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// snippetGit is the gitrepo.Store for snippet repositories.
func (a *App) snippetGit() *gitrepo.Store {
	return &gitrepo.Store{Root: filepath.Join(a.cfg.DataDir, snippetDir), Git: a.git.Git}
}

func snippetShard(uid string) string { return uid[:2] }

// openSnippet opens a snippet's repository.
func (a *App) openSnippet(uid string) (*gitrepo.Repo, error) {
	return a.snippetGit().Open(snippetShard(uid), uid)
}

// snipRun runs git in a snippet repository.
func (a *App) snipRun(ctx context.Context, dir string, env []string, stdin io.Reader, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.git.Git, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"), env...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// initSnippetRepo creates an empty bare repository for uid.
func (a *App) initSnippetRepo(ctx context.Context, uid string) (string, error) {
	st := a.snippetGit()
	p := st.Path(snippetShard(uid), uid)
	if _, err := os.Stat(p); err == nil {
		return "", errors.New("a snippet already exists at that path")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if _, err := a.snipRun(ctx, st.Root, nil, nil, "init", "--bare", "--initial-branch="+snippetBranch, p); err != nil {
		return "", err
	}
	// receive-pack over HTTP is gated by Atelier's own auth.
	if _, err := a.snipRun(ctx, p, nil, nil, "config", "http.receivepack", "true"); err != nil {
		return "", err
	}
	return p, nil
}

func (a *App) removeSnippetRepo(uid string) error {
	st := a.snippetGit()
	return os.RemoveAll(st.Path(snippetShard(uid), uid))
}

// snippetFile is one file in the editor.
type snippetFile struct {
	Name    string
	Content string
}

// checkSnippetName validates a file name: one path segment, printable, short.
func checkSnippetName(name string) error {
	switch {
	case name == "":
		return errors.New("every file needs a name")
	case len(name) > snippetMaxName:
		return fmt.Errorf("file names can be at most %d characters", snippetMaxName)
	case !utf8.ValidString(name):
		return errors.New("file names must be valid UTF-8")
	case name == "." || name == ".." || strings.EqualFold(name, ".git"):
		return fmt.Errorf("%q can't be used as a file name", name)
	case strings.ContainsAny(name, `/\`):
		return errors.New("file names can't contain slashes")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("file names can't contain control characters")
		}
	}
	return nil
}

// checkSnippetFiles validates the editor's files.
func checkSnippetFiles(files []snippetFile) error {
	if len(files) == 0 {
		return errors.New("add at least one file")
	}
	if len(files) > snippetMaxFiles {
		return fmt.Errorf("a snippet can have at most %d files", snippetMaxFiles)
	}
	seen := map[string]bool{}
	for _, f := range files {
		if err := checkSnippetName(f.Name); err != nil {
			return err
		}
		if seen[strings.ToLower(f.Name)] {
			return fmt.Errorf("two files are called %q", f.Name)
		}
		seen[strings.ToLower(f.Name)] = true
		if len(f.Content) > snippetMaxBytes {
			return fmt.Errorf("%s is larger than %s", f.Name, humanBytes(snippetMaxBytes))
		}
		if strings.TrimSpace(f.Content) == "" {
			return fmt.Errorf("%s is empty", f.Name)
		}
	}
	return nil
}

// snippetTip is the commit main points at, or "" for an empty snippet.
func (a *App) snippetTip(ctx context.Context, g *gitrepo.Repo) string {
	c, err := g.ResolveCommit(ctx, snippetBranch)
	if err != nil {
		// A push may have used another branch name.
		for _, b := range g.Branches(ctx) {
			if c, err := g.ResolveCommit(ctx, b); err == nil {
				return c
			}
		}
		return ""
	}
	return c
}

// commitSnippet writes a commit on main whose top-level files are files plus
// the parent's files named in keep. Folders pushed over git are kept as they
// are. It fails if main moved
// away from parent in the meantime.
func (a *App) commitSnippet(ctx context.Context, uid, parent string, files []snippetFile, keep map[string]bool, msg string) (string, error) {
	st := a.snippetGit()
	dir := st.Path(snippetShard(uid), uid)
	var tree strings.Builder
	if parent != "" {
		out, err := a.snipRun(ctx, dir, nil, nil, "ls-tree", "-z", parent)
		if err != nil {
			return "", err
		}
		for _, rec := range strings.Split(out, "\x00") {
			meta, name, ok := strings.Cut(rec, "\t")
			if !ok {
				continue
			}
			if !strings.Contains(meta, " blob ") || keep[name] {
				tree.WriteString(rec + "\x00")
			}
		}
	}
	for _, f := range files {
		content := strings.ReplaceAll(f.Content, "\r\n", "\n")
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		sha, err := a.snipRun(ctx, dir, nil, strings.NewReader(content), "hash-object", "-w", "--stdin")
		if err != nil {
			return "", err
		}
		tree.WriteString("100644 blob " + sha + "\t" + f.Name + "\x00")
	}
	treeSHA, err := a.snipRun(ctx, dir, nil, strings.NewReader(tree.String()), "mktree", "-z")
	if err != nil {
		return "", err
	}
	sig := a.pullIdentity(ctx)
	env := []string{
		"GIT_AUTHOR_NAME=" + sig.Name, "GIT_AUTHOR_EMAIL=" + sig.Email,
		"GIT_COMMITTER_NAME=" + sig.Name, "GIT_COMMITTER_EMAIL=" + sig.Email,
	}
	args := []string{"commit-tree", treeSHA}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	args = append(args, "-F", "-")
	commit, err := a.snipRun(ctx, dir, env, strings.NewReader(msg+"\n"), args...)
	if err != nil {
		return "", err
	}
	old := parent
	if old == "" {
		old = strings.Repeat("0", len(commit))
	}
	if _, err := a.snipRun(ctx, dir, nil, nil, "update-ref", "-m", "atelier: edit snippet",
		"refs/heads/"+snippetBranch, commit, old); err != nil {
		return "", errors.New("the snippet changed while you were editing it; reload and try again")
	}
	return commit, nil
}

// refreshSnippetFiles re-reads the tip's file names into the database after a
// web edit or a push.
func (a *App) refreshSnippetFiles(ctx context.Context, sn store.Snippet) {
	g, err := a.openSnippet(sn.UID)
	if err != nil {
		return
	}
	_ = a.db.SetSnippetFiles(ctx, sn.ID, a.snippetFilesAt(ctx, sn.UID, g, a.snippetTip(ctx, g)))
}

// snippetFilesAt lists every file at commit: top-level files first, in tree
// order, then files in folders (which only a push can create).
func (a *App) snippetFilesAt(ctx context.Context, uid string, g *gitrepo.Repo, commit string) []string {
	if commit == "" {
		return nil
	}
	entries, err := g.Tree(ctx, commit, "")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.Type == "blob" {
			names = append(names, e.Name)
		}
	}
	dir := a.snippetGit().Path(snippetShard(uid), uid)
	if out, err := a.snipRun(ctx, dir, nil, nil, "ls-tree", "-r", "-z", "--name-only", commit); err == nil {
		for _, p := range strings.Split(out, "\x00") {
			if strings.Contains(p, "/") {
				names = append(names, p)
			}
		}
	}
	return names
}

// ---------- git over HTTPS ----------

// snippetGitPath matches /snippets/<uid>.git/<rest> for the smart-HTTP
// endpoints git http-backend serves.
func snippetGitPath(urlPath string) (uid, rest string, ok bool) {
	p, found := strings.CutPrefix(urlPath, "/snippets/")
	if !found {
		return "", "", false
	}
	_, repo, rest, ok := gitrepo.IsGitPath("/x/" + p)
	if !ok {
		return "", "", false
	}
	return strings.ToLower(repo), rest, true
}

func init() {
	registerPathHandler(func(a *App, w http.ResponseWriter, r *http.Request) bool {
		uid, rest, ok := snippetGitPath(r.URL.Path)
		if !ok {
			return false
		}
		a.serveSnippetGit(w, r, uid, rest)
		return true
	})
}

// serveSnippetGit is serveGit for snippets: the same credentials, rate limit
// and App.can decision (on the snippet's stand-in repository), then git
// http-backend rooted at <data>/snippets.
func (a *App) serveSnippetGit(w http.ResponseWriter, r *http.Request, uid, rest string) {
	ctx := r.Context()
	notFound := func() { http.Error(w, "snippet not found", http.StatusNotFound) }
	if !validSnippetUID(uid) {
		notFound()
		return
	}
	sn, err := a.db.SnippetByUID(ctx, uid)
	if err != nil {
		notFound()
		return
	}
	write := gitrepo.IsWrite(r, rest)
	need := PermRead
	if write {
		need = PermWrite
	}
	repo := snippetRepo(sn)
	who := Actor{Kind: ActorAnonymous, Name: "anonymous"}
	if !a.can(ctx, who, repo, need) {
		if !a.lim.allow(a.clientIP(r)) {
			http.Error(w, "too many attempts, try again in a minute", http.StatusTooManyRequests)
			return
		}
		if u, p, ok := r.BasicAuth(); ok {
			for _, cand := range []string{p, u} {
				if c, ok := a.resolveCredential(ctx, cand); ok {
					who = c
					break
				}
			}
		}
		if who.Kind == ActorAnonymous || !a.can(ctx, who, repo, need) {
			// Like private repositories, a private snippet asks for credentials
			// rather than revealing whether it exists.
			w.Header().Set("WWW-Authenticate", `Basic realm="Atelier", charset="UTF-8"`)
			http.Error(w, "authentication required: use an Atelier access token as the password", http.StatusUnauthorized)
			return
		}
		a.lim.reset(a.clientIP(r))
	}
	if _, err := a.openSnippet(uid); err != nil {
		notFound()
		return
	}
	a.snippetGit().ServeHTTP(w, r, snippetShard(uid), uid, rest, who.Kind+":"+who.Name)
	if write && rest == "git-receive-pack" {
		a.refreshSnippetFiles(context.WithoutCancel(ctx), sn)
	}
}

// ---------- git over SSH ----------

// sshSnippet is the SSH server's hook for snippets/<uid>.git: the same App.can
// decision as HTTPS (read to clone, write to push), with the key acting as the
// person it belongs to. A snippet the person can't read is "not found".
func (a *App) sshSnippet(ctx context.Context, keyID int64, uid string, write bool) (string, func(), error) {
	notFound := fmt.Errorf("snippet '%s' not found", uid)
	if !validSnippetUID(uid) {
		return "", nil, notFound
	}
	sn, err := a.db.SnippetByUID(ctx, uid)
	if err != nil {
		return "", nil, notFound
	}
	personID, err := a.db.SSHKeyPerson(ctx, keyID)
	if err != nil {
		return "", nil, notFound
	}
	p, err := a.db.Person(ctx, personID)
	if err != nil {
		return "", nil, notFound
	}
	who := Actor{Kind: ActorOwner, Name: p.Name, PersonID: p.ID}
	repo := snippetRepo(sn)
	if !a.can(ctx, who, repo, PermRead) {
		return "", nil, notFound
	}
	if write && !a.can(ctx, who, repo, PermWrite) {
		return "", nil, fmt.Errorf("you don't have permission to push to snippet '%s'", uid)
	}
	if _, err := a.openSnippet(uid); err != nil {
		return "", nil, notFound
	}
	dir := a.snippetGit().Path(snippetShard(uid), uid)
	after := func() { a.refreshSnippetFiles(context.WithoutCancel(ctx), sn) }
	return dir, after, nil
}

// snippetRepo is the stand-in repository App.can decides snippet access on:
// the snippet's owner account and visibility. ID 0 never matches a real
// repository, so per-repository rules from addons don't apply by accident.
func snippetRepo(sn store.Snippet) store.Repo {
	return store.Repo{OwnerID: sn.OwnerID, Owner: sn.Owner, OwnerKind: sn.OwnerKind,
		Name: "snippet-" + sn.UID, Private: sn.Private, CreatedAt: sn.CreatedAt, UpdatedAt: sn.UpdatedAt}
}

// ownerRepo is the stand-in for "a new snippet in this account".
func ownerRepo(o store.Owner) store.Repo {
	return store.Repo{OwnerID: o.ID, Owner: o.Name, OwnerKind: o.Kind, Name: "snippet-new", Private: true}
}
