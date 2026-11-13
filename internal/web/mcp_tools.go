package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"atelier/internal/actions"
	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// The MCP tools. Names are a fixed contract shared with agentic workflows.

var mcpTools = sync.OnceValue(func() []*mcpTool {
	return append([]*mcpTool{
		// ----- repositories and code -----
		mcpDef(mcpTool{Name: "repo.info", Title: "Repository details", Repo: true,
			Desc: "Get a repository's description, visibility, default branch, branches, tags and open issue / pull request counts."},
			mcpRepoInfo),
		mcpDef(mcpTool{Name: "repo.list", Title: "List repositories",
			Desc: "List the repositories you can reach, optionally only one owner's. Use the owner/name it returns as the repo argument of other tools."},
			mcpRepoList),
		mcpDef(mcpTool{Name: "code.read", Title: "Read a file", Repo: true,
			Desc: "Read a text file at a branch, tag or commit (default: the default branch). Use start_line/end_line for part of a long file; content is cut at 64 KB."},
			mcpCodeRead),
		mcpDef(mcpTool{Name: "code.list", Title: "List files", Repo: true,
			Desc: "List the files and folders in a directory at a ref (default: the repository root on the default branch). Set recursive to list every file below the path."},
			mcpCodeList),
		mcpDef(mcpTool{Name: "code.search", Title: "Search code", Repo: true,
			Desc: "Search file contents at a ref with git grep. Plain text by default; set regex for an extended regular expression. Returns up to max_results matching lines (default 50, max 100)."},
			mcpCodeSearch),
		mcpDef(mcpTool{Name: "commits.list", Title: "List commits", Repo: true,
			Desc: "List commits on a branch, tag or commit (newest first), optionally only those touching a path."},
			mcpCommitsList),
		mcpDef(mcpTool{Name: "commit.get", Title: "Show a commit", Repo: true,
			Desc: "Show one commit: message, author, parents, a diffstat and the patch (cut at 64 KB)."},
			mcpCommitGet),

		// ----- issues -----
		mcpDef(mcpTool{Name: "issues.list", Title: "List issues", Repo: true,
			Desc: "List issues (newest first). state is open (default), closed or all; labels must all match; query matches words in the title or body."},
			mcpIssuesList),
		mcpDef(mcpTool{Name: "issues.get", Title: "Read an issue", Repo: true,
			Desc: "Read an issue with its labels and comments (comments are paginated, oldest first)."},
			mcpIssuesGet),
		mcpDef(mcpTool{Name: "issues.create", Title: "Open an issue", Repo: true, Write: true,
			Desc: "Open a new issue with a title, a Markdown body and optional existing label names."},
			mcpIssuesCreate),
		mcpDef(mcpTool{Name: "issues.comment", Title: "Comment on an issue", Repo: true, Write: true,
			Desc: "Add a Markdown comment to an issue."},
			mcpIssuesComment),
		mcpDef(mcpTool{Name: "issues.close", Title: "Close an issue", Repo: true, Write: true,
			Desc: "Close an open issue as completed (default) or not_planned, optionally with a closing comment."},
			mcpIssuesClose),
		mcpDef(mcpTool{Name: "issues.label", Title: "Label an issue", Repo: true, Write: true,
			Desc: "Add and/or remove labels on an issue by name. Labels must already exist in the repository (issues.get on any issue, or the error message, lists them)."},
			mcpIssuesLabel),

		// ----- pull requests -----
		mcpDef(mcpTool{Name: "pulls.list", Title: "List pull requests", Repo: true,
			Desc: "List pull requests (newest first). state is open (default), closed, merged or all."},
			mcpPullsList),
		mcpDef(mcpTool{Name: "pulls.get", Title: "Read a pull request", Repo: true,
			Desc: "Read a pull request: description, branches, state, changed files, comments and inline notes."},
			mcpPullsGet),
		mcpDef(mcpTool{Name: "pulls.diff", Title: "Pull request diff", Repo: true,
			Desc: "Get a pull request's diff as unified patches, one file per entry (paginated by file; each patch cut at 64 KB). Pass path for a single file."},
			mcpPullsDiff),
		mcpDef(mcpTool{Name: "pulls.comment", Title: "Comment on a pull request", Repo: true, Write: true,
			Desc: "Add a Markdown comment to a pull request's conversation."},
			mcpPullsComment),
		mcpDef(mcpTool{Name: "pulls.note", Title: "Note on a line", Repo: true, Write: true,
			Desc: "Leave an inline review note on one line of a pull request's diff. line is the line number in the new file (side new, default) or in the old file (side old, for removed lines); the line must be part of the diff."},
			mcpPullsNote),
		mcpDef(mcpTool{Name: "pulls.update_branch", Title: "Update a pull request's branch", Repo: true, Write: true, NoJobs: true,
			Desc: "Merge the base branch into an open pull request's head branch when it is behind (like GitHub's Update branch). Answers updated false when it is already up to date; on conflicts nothing is written and the conflicting files are listed."},
			mcpPullsUpdateBranch),

		// ----- actions -----
		mcpDef(mcpTool{Name: "actions.runs", Title: "List workflow runs", Repo: true,
			Desc: "List Actions workflow runs (newest first), optionally for one workflow file, branch or status."},
			mcpActionsRuns),
		mcpDef(mcpTool{Name: "actions.logs", Title: "Read job logs", Repo: true,
			Desc: "Read the logs of a workflow run's jobs (run is the run number shown in Atelier). Each job lists its steps with their status and the run's problems (error lines, and ::warning:: lines) first, then the log as plain text: colour codes, workflow plumbing and download progress are left out, and ::error:: lines read ERROR: …. Pass job to read one job and step to read one step. Logs keep their end when cut; secrets are already masked."},
			mcpActionsLogs),
		mcpDef(mcpTool{Name: "actions.dispatch", Title: "Run a workflow", Repo: true, Write: true, NoJobs: true,
			Desc: "Start a workflow that has a workflow_dispatch trigger, on a branch (default: the default branch), with optional inputs."},
			mcpActionsDispatch),

		// ----- releases -----
		mcpDef(mcpTool{Name: "releases.list", Title: "List releases", Repo: true,
			Desc: "List releases (newest first) with their notes. Drafts are included when you can write to the repository."},
			mcpReleasesList),
		mcpDef(mcpTool{Name: "releases.draft", Title: "Draft a release", Repo: true, Write: true,
			Desc: "Create a draft release with Markdown notes for a tag (it doesn't need to exist yet). Drafts are never published by this tool; a person reviews and publishes them."},
			mcpReleasesDraft),

		// ----- you -----
		mcpDef(mcpTool{Name: "notifications.list", Title: "List notifications", NoJobs: true,
			Desc: "List your Atelier notifications (newest first): issue and pull request activity, Actions results, releases and more."},
			mcpNotificationsList),
	}, mcpProjectTools()...) // projects (mcp_projects.go)
})

// ---------- repositories ----------

type mcpRepoOut struct {
	Repo        string `json:"repo"`
	Description string `json:"description,omitempty"`
	Private     bool   `json:"private"`
	OwnerKind   string `json:"owner_kind"`
	UpdatedAt   string `json:"updated_at"`
}

func repoOut(r store.Repo) mcpRepoOut {
	return mcpRepoOut{Repo: r.FullName(), Description: r.Description, Private: r.Private, OwnerKind: r.OwnerKind,
		UpdatedAt: timeStr(r.UpdatedAt)}
}

type mcpRepoInfoIn struct {
	mcpRepoArg
}

func mcpRepoInfo(ctx context.Context, c *mcpCall, in mcpRepoInfoIn) (any, error) {
	g := c.git
	out := map[string]any{"repository": repoOut(c.repo), "web_path": "/" + c.repo.FullName()}
	if c.a.cfg.PublicURL != "" {
		out["clone_url"] = strings.TrimRight(c.a.cfg.PublicURL, "/") + "/" + c.repo.FullName() + ".git"
	}
	if g.IsEmpty(ctx) {
		out["empty"] = true
		return out, nil
	}
	branches, tags := g.Branches(ctx), g.Tags(ctx)
	out["default_branch"] = g.DefaultBranch(ctx)
	out["branches"], out["branches_truncated"] = capList(branches, 100)
	out["tags"], out["tags_truncated"] = capList(tags, 50)
	if open, closed, err := c.a.db.IssueCounts(ctx, c.repo.ID, store.IssueQuery{}); err == nil {
		out["open_issues"], out["closed_issues"] = open, closed
	}
	if pc, err := c.a.db.PullCounts(ctx, c.repo.ID); err == nil {
		out["open_pulls"] = pc["open"]
	}
	return out, nil
}

func capList(xs []string, n int) ([]string, bool) {
	if xs == nil {
		return []string{}, false
	}
	if len(xs) > n {
		return xs[:n], true
	}
	return xs, false
}

type mcpRepoListIn struct {
	Owner string `json:"owner,omitempty" jsonschema:"only repositories of this account (a username or company account name)"`
	mcpPage
}

func mcpRepoList(ctx context.Context, c *mcpCall, in mcpRepoListIn) (any, error) {
	repos, err := c.a.db.Repos(ctx)
	if err != nil {
		return nil, err
	}
	var ok []mcpRepoOut
	for _, r := range repos {
		if in.Owner != "" && !strings.EqualFold(r.Owner, in.Owner) {
			continue
		}
		if c.who.Kind == ActorJob && r.ID != jobRepoID(c.who) {
			continue
		}
		if c.a.can(ctx, c.who, r, PermRead) {
			ok = append(ok, repoOut(r))
		}
	}
	items, page, more := pageOf(ok, in.mcpPage)
	return map[string]any{"repositories": items, "page": page, "has_more": more}, nil
}

// resolveRef turns an optional ref into a commit (default: the default branch).
func (c *mcpCall) resolveRef(ctx context.Context, ref string) (string, string, error) {
	if c.git.IsEmpty(ctx) {
		return "", "", userErr("%s is empty: nothing has been pushed yet", c.repo.FullName())
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = c.git.DefaultBranch(ctx)
	}
	sha, err := c.git.ResolveCommit(ctx, ref)
	if err != nil {
		return "", "", userErr("%q isn't a branch, tag or commit in %s", ref, c.repo.FullName())
	}
	return ref, sha, nil
}

func cleanRepoPath(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", userErr("path can't contain ..")
		}
	}
	if strings.ContainsAny(p, "\x00\n") {
		return "", userErr("path contains invalid characters")
	}
	return p, nil
}

type mcpCodeReadIn struct {
	mcpRepoArg
	Path      string `json:"path" jsonschema:"file path from the repository root, e.g. src/main.go"`
	Ref       string `json:"ref,omitempty" jsonschema:"branch, tag or commit sha (default: the default branch)"`
	StartLine int    `json:"start_line,omitempty" jsonschema:"first line to return, 1-based (default 1)"`
	EndLine   int    `json:"end_line,omitempty" jsonschema:"last line to return, inclusive (default: end of file)"`
}

const mcpMaxBlob = 8 << 20

func mcpCodeRead(ctx context.Context, c *mcpCall, in mcpCodeReadIn) (any, error) {
	path, err := cleanRepoPath(in.Path)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, userErr("give a file path; use code.list to see the files")
	}
	ref, sha, err := c.resolveRef(ctx, in.Ref)
	if err != nil {
		return nil, err
	}
	c.target = path
	switch c.git.ObjectType(ctx, sha, path) {
	case "blob":
	case "tree":
		return nil, userErr("%s is a directory; use code.list", path)
	default:
		return nil, userErr("%s doesn't exist at %s", path, ref)
	}
	b, err := c.git.Blob(ctx, sha, path, mcpMaxBlob)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"path": path, "ref": ref, "commit": sha, "size": b.Size}
	if b.TooLarge {
		out["too_large"] = true
		out["content"] = ""
		return out, nil
	}
	if b.Binary {
		out["binary"] = true
		out["content"] = ""
		return out, nil
	}
	text := string(b.Data)
	lines := strings.SplitAfter(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	out["total_lines"] = len(lines)
	if in.StartLine > 0 || in.EndLine > 0 {
		start, end := max(in.StartLine, 1), in.EndLine
		if end == 0 || end > len(lines) {
			end = len(lines)
		}
		if start > len(lines) || end < start {
			return nil, userErr("the file has %d lines", len(lines))
		}
		text = strings.Join(lines[start-1:end], "")
		out["start_line"], out["end_line"] = start, end
	}
	content, cut := capText(text, mcpMaxText)
	out["content"] = content
	if cut {
		out["truncated"] = true
	}
	return out, nil
}

type mcpCodeListIn struct {
	mcpRepoArg
	Path      string `json:"path,omitempty" jsonschema:"directory path (default: the repository root)"`
	Ref       string `json:"ref,omitempty" jsonschema:"branch, tag or commit sha (default: the default branch)"`
	Recursive bool   `json:"recursive,omitempty" jsonschema:"list every file below the path instead of one level"`
	mcpPage
}

type mcpEntry struct {
	Path string `json:"path"`
	Type string `json:"type"` // file, dir, submodule, symlink
	Size int64  `json:"size,omitempty"`
}

const mcpMaxTreeEntries = 20000

func mcpCodeList(ctx context.Context, c *mcpCall, in mcpCodeListIn) (any, error) {
	path, err := cleanRepoPath(in.Path)
	if err != nil {
		return nil, err
	}
	ref, sha, err := c.resolveRef(ctx, in.Ref)
	if err != nil {
		return nil, err
	}
	c.target = path
	if t := c.git.ObjectType(ctx, sha, path); t != "tree" {
		if t == "blob" {
			return nil, userErr("%s is a file; use code.read", path)
		}
		return nil, userErr("%s doesn't exist at %s", path, ref)
	}
	var entries []mcpEntry
	truncated := false
	if in.Recursive {
		args := []string{"ls-tree", "-r", "-z", "-l", "--end-of-options", sha}
		if path != "" {
			args = append(args, "--", path+"/")
		}
		raw, cut, err := c.a.mcpGit(ctx, c.repo, 16<<20, args...)
		if err != nil {
			return nil, err
		}
		truncated = cut
		for _, rec := range strings.Split(raw, "\x00") {
			meta, name, ok := strings.Cut(rec, "\t")
			f := strings.Fields(meta)
			if !ok || len(f) < 4 {
				continue
			}
			size, _ := strconv.ParseInt(f[3], 10, 64)
			entries = append(entries, mcpEntry{Path: name, Type: entryType(f[0], f[1]), Size: size})
			if len(entries) >= mcpMaxTreeEntries {
				truncated = true
				break
			}
		}
	} else {
		es, err := c.git.Tree(ctx, sha, path)
		if err != nil {
			return nil, userErr("%s doesn't exist at %s", path, ref)
		}
		for _, e := range es {
			me := mcpEntry{Path: e.Path, Type: entryType(e.Mode, e.Type)}
			if !e.IsDir() {
				me.Size = e.Size
			}
			entries = append(entries, me)
		}
	}
	items, page, more := pageOf(entries, in.mcpPage)
	out := map[string]any{"ref": ref, "commit": sha, "path": path, "entries": items, "page": page, "has_more": more,
		"total": len(entries)}
	if truncated {
		out["truncated"] = true
	}
	return out, nil
}

func entryType(mode, typ string) string {
	switch {
	case typ == "tree":
		return "dir"
	case typ == "commit":
		return "submodule"
	case mode == "120000":
		return "symlink"
	}
	return "file"
}

// mcpGit runs a read-only git command in a repository with a timeout and an
// output cap.
func (a *App) mcpGit(ctx context.Context, rp store.Repo, maxOut int, args ...string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + a.git.Path(rp.Owner, rp.Name),
		"-c", "core.quotePath=false"}, args...)...)
	cw := &mcpCapWriter{n: maxOut}
	var stderr bytes.Buffer
	cmd.Stdout = cw
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return cw.buf.String(), true, nil // timed out: return what we have
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 && stderr.Len() == 0 {
		return cw.buf.String(), cw.over, nil // git grep: no matches
	}
	if err != nil {
		return "", false, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return cw.buf.String(), cw.over, nil
}

type mcpCapWriter struct {
	buf  bytes.Buffer
	n    int
	over bool
}

func (w *mcpCapWriter) Write(p []byte) (int, error) {
	if room := w.n - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
			w.over = true
		} else {
			w.buf.Write(p)
		}
	} else if len(p) > 0 {
		w.over = true
	}
	return len(p), nil
}

type mcpCodeSearchIn struct {
	mcpRepoArg
	Query      string `json:"query" jsonschema:"text to find (or an extended regular expression when regex is true)"`
	Ref        string `json:"ref,omitempty" jsonschema:"branch, tag or commit sha (default: the default branch)"`
	Path       string `json:"path,omitempty" jsonschema:"only search below this path"`
	Regex      bool   `json:"regex,omitempty" jsonschema:"treat query as an extended regular expression"`
	IgnoreCase bool   `json:"ignore_case,omitempty" jsonschema:"match case-insensitively"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"maximum matching lines to return (default 50, max 100)"`
}

type mcpMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

func mcpCodeSearch(ctx context.Context, c *mcpCall, in mcpCodeSearchIn) (any, error) {
	q := in.Query
	if strings.TrimSpace(q) == "" || len(q) > 500 || strings.ContainsAny(q, "\x00\n") {
		return nil, userErr("query must be one line of up to 500 characters")
	}
	path, err := cleanRepoPath(in.Path)
	if err != nil {
		return nil, err
	}
	ref, sha, err := c.resolveRef(ctx, in.Ref)
	if err != nil {
		return nil, err
	}
	limit := in.MaxResults
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, mcpMaxPer)
	args := []string{"grep", "-n", "-I", "-z", "--no-color", "--max-count=20"}
	if in.Regex {
		args = append(args, "-E")
	} else {
		args = append(args, "-F")
	}
	if in.IgnoreCase {
		args = append(args, "-i")
	}
	args = append(args, "-e", q, sha, "--")
	if path != "" {
		args = append(args, path)
	}
	raw, cut, err := c.a.mcpGit(ctx, c.repo, 4<<20, args...)
	if err != nil {
		return nil, userErr("the search failed; check the regular expression")
	}
	var matches []mcpMatch
	more := cut
	for _, line := range strings.Split(raw, "\n") {
		// <sha>:<path>\0<line>\0<text>
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) != 3 {
			continue
		}
		file := strings.TrimPrefix(parts[0], sha+":")
		n, _ := strconv.Atoi(parts[1])
		if len(matches) >= limit {
			more = true
			break
		}
		text, _ := capText(parts[2], 400)
		matches = append(matches, mcpMatch{Path: file, Line: n, Text: text})
	}
	if matches == nil {
		matches = []mcpMatch{}
	}
	return map[string]any{"ref": ref, "commit": sha, "matches": matches, "has_more": more}, nil
}

type mcpCommitsListIn struct {
	mcpRepoArg
	Ref  string `json:"ref,omitempty" jsonschema:"branch, tag or commit sha (default: the default branch)"`
	Path string `json:"path,omitempty" jsonschema:"only commits that touch this path"`
	mcpPage
}

type mcpCommitOut struct {
	SHA     string   `json:"sha"`
	Author  string   `json:"author"`
	Email   string   `json:"email,omitempty"`
	Date    string   `json:"date"`
	Subject string   `json:"subject"`
	Body    string   `json:"body,omitempty"`
	Parents []string `json:"parents,omitempty"`
}

func commitOut(cm gitrepo.Commit) mcpCommitOut {
	body, _ := capText(cm.Body, 4096)
	return mcpCommitOut{SHA: cm.Hash, Author: cm.Author, Email: cm.Email, Date: timeStr(cm.When), Subject: cm.Subject,
		Body: body, Parents: cm.Parents}
}

func mcpCommitsList(ctx context.Context, c *mcpCall, in mcpCommitsListIn) (any, error) {
	path, err := cleanRepoPath(in.Path)
	if err != nil {
		return nil, err
	}
	ref, sha, err := c.resolveRef(ctx, in.Ref)
	if err != nil {
		return nil, err
	}
	off, lim, page := in.bounds()
	cs, err := c.git.Log(ctx, sha, path, off, lim+1)
	if err != nil {
		return nil, err
	}
	more := len(cs) > lim
	if more {
		cs = cs[:lim]
	}
	out := make([]mcpCommitOut, 0, len(cs))
	for _, cm := range cs {
		out = append(out, commitOut(cm))
	}
	return map[string]any{"ref": ref, "commits": out, "page": page, "has_more": more}, nil
}

type mcpCommitGetIn struct {
	mcpRepoArg
	SHA string `json:"sha" jsonschema:"the commit sha (full or abbreviated)"`
}

func mcpCommitGet(ctx context.Context, c *mcpCall, in mcpCommitGetIn) (any, error) {
	if c.git.IsEmpty(ctx) {
		return nil, userErr("%s is empty", c.repo.FullName())
	}
	sha, err := c.git.ResolveCommit(ctx, in.SHA)
	if err != nil {
		return nil, userErr("no commit %s in %s", in.SHA, c.repo.FullName())
	}
	c.target = sha[:12]
	cm, patch, err := c.git.Show(ctx, sha)
	if err != nil {
		return nil, userErr("no commit %s in %s", in.SHA, c.repo.FullName())
	}
	p, cut := capText(patch, mcpMaxText)
	out := map[string]any{"commit": commitOut(*cm), "patch": p}
	if cut {
		out["truncated"] = true
	}
	return out, nil
}

// ---------- issues ----------

type mcpIssueOut struct {
	Number    int64    `json:"number"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	Reason    string   `json:"state_reason,omitempty"`
	Author    string   `json:"author"`
	Labels    []string `json:"labels"`
	Milestone string   `json:"milestone,omitempty"`
	Comments  int      `json:"comments"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	ClosedAt  string   `json:"closed_at,omitempty"`
	Body      string   `json:"body,omitempty"`
	URL       string   `json:"web_path"`
}

func issueOut(rp store.Repo, is store.Issue, withBody bool) mcpIssueOut {
	o := mcpIssueOut{Number: is.Number, Title: is.Title, State: is.State, Reason: is.StateReason,
		Author: authorStr(is.AuthorKind, is.AuthorName), Labels: []string{}, Milestone: is.Milestone, Comments: is.Comments,
		CreatedAt: timeStr(is.CreatedAt), UpdatedAt: timeStr(is.UpdatedAt), ClosedAt: timePtrStr(is.ClosedAt),
		URL: fmt.Sprintf("/%s/issues/%d", rp.FullName(), is.Number)}
	for _, l := range is.Labels {
		o.Labels = append(o.Labels, l.Name)
	}
	if withBody {
		o.Body = cap64(is.Body)
	}
	return o
}

type mcpIssuesListIn struct {
	mcpRepoArg
	State  string   `json:"state,omitempty" jsonschema:"open (default), closed or all"`
	Labels []string `json:"labels,omitempty" jsonschema:"only issues with all of these label names"`
	Query  string   `json:"query,omitempty" jsonschema:"words that must appear in the title or body"`
	mcpPage
}

func mcpIssuesList(ctx context.Context, c *mcpCall, in mcpIssuesListIn) (any, error) {
	q := store.IssueQuery{Labels: in.Labels, Text: strings.Fields(in.Query)}
	switch in.State {
	case "", "open":
		q.State = "open"
	case "closed":
		q.State = "closed"
	case "all":
	default:
		return nil, userErr("state must be open, closed or all")
	}
	off, lim, page := in.bounds()
	issues, more, err := c.a.db.Issues(ctx, c.repo.ID, q, off, lim)
	if err != nil {
		return nil, err
	}
	out := make([]mcpIssueOut, 0, len(issues))
	for _, is := range issues {
		out = append(out, issueOut(c.repo, is, false))
	}
	return map[string]any{"issues": out, "page": page, "has_more": more}, nil
}

type mcpNumberIn struct {
	mcpRepoArg
	Number int64 `json:"number" jsonschema:"the issue or pull request number"`
}

func (c *mcpCall) issue(ctx context.Context, n int64) (store.Issue, error) {
	c.target = "#" + strconv.FormatInt(n, 10)
	is, err := c.a.db.IssueByNumber(ctx, c.repo.ID, n)
	if errors.Is(err, store.ErrNotFound) {
		if _, perr := c.a.db.PullByNumber(ctx, c.repo.ID, n); perr == nil {
			return is, userErr("#%d is a pull request; use the pulls tools", n)
		}
		return is, userErr("no issue #%d in %s", n, c.repo.FullName())
	}
	return is, err
}

type mcpCommentOut struct {
	ID        int64  `json:"id"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
	Body      string `json:"body"`
}

type mcpIssuesGetIn struct {
	mcpNumberIn
	mcpPage
}

func mcpIssuesGet(ctx context.Context, c *mcpCall, in mcpIssuesGetIn) (any, error) {
	is, err := c.issue(ctx, in.Number)
	if err != nil {
		return nil, err
	}
	cs, err := c.a.db.IssueComments(ctx, is.ID)
	if err != nil {
		return nil, err
	}
	all := make([]mcpCommentOut, 0, len(cs))
	for _, cm := range cs {
		all = append(all, mcpCommentOut{ID: cm.ID, Author: authorStr(cm.AuthorKind, cm.AuthorName),
			CreatedAt: timeStr(cm.CreatedAt), Body: cap64(cm.Body)})
	}
	items, page, more := pageOf(all, in.mcpPage)
	return map[string]any{"issue": issueOut(c.repo, is, true), "comments": items, "page": page, "has_more": more}, nil
}

func (c *mcpCall) view() *repoView {
	return &repoView{Repo: c.repo, Base: "/" + c.repo.FullName()}
}

func checkBody(body string, required bool) (string, error) {
	body = mcpNormalize(body)
	if required && body == "" {
		return "", userErr("body can't be empty")
	}
	if len(body) > pullBodyMax {
		return "", userErr("body is too long (max %d bytes)", pullBodyMax)
	}
	return body, nil
}

// labelIDs maps label names to ids (case-insensitive) and lists unknown names.
func (c *mcpCall) labelIDs(ctx context.Context, names []string) (map[string]store.Label, []string, error) {
	labels, err := c.a.db.Labels(ctx, c.repo.ID)
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]store.Label{}
	for _, l := range labels {
		byName[strings.ToLower(l.Name)] = l
	}
	var unknown []string
	for _, n := range names {
		if _, ok := byName[strings.ToLower(strings.TrimSpace(n))]; !ok {
			unknown = append(unknown, n)
		}
	}
	return byName, unknown, nil
}

func unknownLabelsErr(byName map[string]store.Label, unknown []string) error {
	var have []string
	for _, l := range byName {
		have = append(have, l.Name)
	}
	sort.Strings(have)
	return userErr("no label named %s; this repository has: %s", strings.Join(unknown, ", "), strings.Join(have, ", "))
}

type mcpIssuesCreateIn struct {
	mcpRepoArg
	Title  string   `json:"title" jsonschema:"the issue title (up to 256 characters)"`
	Body   string   `json:"body,omitempty" jsonschema:"the description, in Markdown"`
	Labels []string `json:"labels,omitempty" jsonschema:"names of existing labels to add"`
}

func mcpIssuesCreate(ctx context.Context, c *mcpCall, in mcpIssuesCreateIn) (any, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" || len([]rune(title)) > 256 || strings.ContainsAny(title, "\n\r") {
		return nil, userErr("title must be one line of 1-256 characters")
	}
	body, err := checkBody(in.Body, false)
	if err != nil {
		return nil, err
	}
	byName, unknown, err := c.labelIDs(ctx, in.Labels)
	if err != nil {
		return nil, err
	}
	if len(unknown) > 0 {
		return nil, unknownLabelsErr(byName, unknown)
	}
	who := c.recordAs(ctx)
	is, err := c.a.db.CreateIssue(ctx, c.repo.ID, title, body, who.Kind, who.Name)
	if errors.Is(err, store.ErrInvalidIssue) {
		return nil, userErr("give the issue a title (up to 256 characters)")
	}
	if err != nil {
		return nil, err
	}
	c.target = "#" + strconv.FormatInt(is.Number, 10)
	if len(in.Labels) > 0 {
		var ids []int64
		for _, n := range in.Labels {
			ids = append(ids, byName[strings.ToLower(strings.TrimSpace(n))].ID)
		}
		if err := c.a.db.SetIssueLabels(ctx, is.ID, ids, who.Kind, who.Name); err != nil {
			return nil, err
		}
	}
	c.a.emit(ctx, issueEvent(EventIssueOpened, c.view(), is, who, body))
	if fresh, err := c.a.db.IssueByNumber(ctx, c.repo.ID, is.Number); err == nil {
		is = fresh
	}
	return map[string]any{"issue": issueOut(c.repo, is, false)}, nil
}

type mcpIssuesCommentIn struct {
	mcpNumberIn
	Body string `json:"body" jsonschema:"the comment, in Markdown"`
}

func mcpIssuesComment(ctx context.Context, c *mcpCall, in mcpIssuesCommentIn) (any, error) {
	body, err := checkBody(in.Body, true)
	if err != nil {
		return nil, err
	}
	is, err := c.issue(ctx, in.Number)
	if err != nil {
		return nil, err
	}
	who := c.recordAs(ctx)
	cm, err := c.a.db.AddIssueComment(ctx, is.ID, body, who.Kind, who.Name)
	if err != nil {
		return nil, err
	}
	anchor := fmt.Sprintf("#comment-%d", cm.ID)
	c.a.emit(ctx, issueEvent(EventIssueCommented, c.view(), is, who, body, anchor))
	return map[string]any{"comment_id": cm.ID, "web_path": fmt.Sprintf("/%s/issues/%d%s", c.repo.FullName(), is.Number, anchor)}, nil
}

type mcpIssuesCloseIn struct {
	mcpNumberIn
	Reason  string `json:"reason,omitempty" jsonschema:"completed (default) or not_planned"`
	Comment string `json:"comment,omitempty" jsonschema:"optional closing comment, in Markdown"`
}

func mcpIssuesClose(ctx context.Context, c *mcpCall, in mcpIssuesCloseIn) (any, error) {
	reason := store.ReasonCompleted
	switch in.Reason {
	case "", "completed":
	case "not_planned":
		reason = store.ReasonNotPlanned
	default:
		return nil, userErr("reason must be completed or not_planned")
	}
	comment, err := checkBody(in.Comment, false)
	if err != nil {
		return nil, err
	}
	is, err := c.issue(ctx, in.Number)
	if err != nil {
		return nil, err
	}
	if !is.IsOpen() {
		return nil, userErr("#%d is already closed", is.Number)
	}
	who := c.recordAs(ctx)
	if comment != "" {
		cm, err := c.a.db.AddIssueComment(ctx, is.ID, comment, who.Kind, who.Name)
		if err != nil {
			return nil, err
		}
		c.a.emit(ctx, issueEvent(EventIssueCommented, c.view(), is, who, comment, fmt.Sprintf("#comment-%d", cm.ID)))
	}
	if err := c.a.db.CloseIssue(ctx, is.ID, who.Kind, who.Name, reason); err != nil {
		return nil, err
	}
	c.a.emit(ctx, issueEvent(EventIssueClosed, c.view(), is, who, ""))
	return map[string]any{"number": is.Number, "state": "closed", "state_reason": reason}, nil
}

type mcpIssuesLabelIn struct {
	mcpNumberIn
	Add    []string `json:"add,omitempty" jsonschema:"label names to add"`
	Remove []string `json:"remove,omitempty" jsonschema:"label names to remove"`
}

func mcpIssuesLabel(ctx context.Context, c *mcpCall, in mcpIssuesLabelIn) (any, error) {
	if len(in.Add) == 0 && len(in.Remove) == 0 {
		return nil, userErr("give labels to add or remove")
	}
	is, err := c.issue(ctx, in.Number)
	if err != nil {
		return nil, err
	}
	byName, unknown, err := c.labelIDs(ctx, append(append([]string{}, in.Add...), in.Remove...))
	if err != nil {
		return nil, err
	}
	if len(unknown) > 0 {
		return nil, unknownLabelsErr(byName, unknown)
	}
	set := map[int64]bool{}
	for _, l := range is.Labels {
		set[l.ID] = true
	}
	for _, n := range in.Add {
		set[byName[strings.ToLower(strings.TrimSpace(n))].ID] = true
	}
	for _, n := range in.Remove {
		delete(set, byName[strings.ToLower(strings.TrimSpace(n))].ID)
	}
	ids := make([]int64, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	who := c.recordAs(ctx)
	if err := c.a.db.SetIssueLabels(ctx, is.ID, ids, who.Kind, who.Name); err != nil {
		return nil, err
	}
	fresh, err := c.a.db.IssueByNumber(ctx, c.repo.ID, is.Number)
	if err != nil {
		return nil, err
	}
	out := issueOut(c.repo, fresh, false)
	c.a.emit(ctx, issueEvent("issue.labeled", c.view(), fresh, who, "labels: "+strings.Join(out.Labels, ", ")))
	return map[string]any{"number": fresh.Number, "labels": out.Labels}, nil
}

// ---------- pull requests ----------

type mcpPullOut struct {
	Number    int64  `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Head      string `json:"head"`
	Base      string `json:"base"`
	HeadSHA   string `json:"head_sha,omitempty"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	MergedAt  string `json:"merged_at,omitempty"`
	MergedBy  string `json:"merged_by,omitempty"`
	Body      string `json:"body,omitempty"`
	URL       string `json:"web_path"`
}

func pullOut(rp store.Repo, p store.Pull, withBody bool) mcpPullOut {
	o := mcpPullOut{Number: p.Number, Title: p.Title, State: p.State, Head: p.Head, Base: p.Base, HeadSHA: p.HeadSHA,
		Author: authorStr(p.AuthorKind, p.AuthorName), CreatedAt: timeStr(p.CreatedAt), UpdatedAt: timeStr(p.UpdatedAt),
		MergedAt: timePtrStr(p.MergedAt), MergedBy: p.MergedBy, URL: fmt.Sprintf("/%s/pulls/%d", rp.FullName(), p.Number)}
	if withBody {
		o.Body = cap64(p.Body)
	}
	return o
}

type mcpPullsListIn struct {
	mcpRepoArg
	State string `json:"state,omitempty" jsonschema:"open (default), closed, merged or all"`
	mcpPage
}

func mcpPullsList(ctx context.Context, c *mcpCall, in mcpPullsListIn) (any, error) {
	state := in.State
	switch state {
	case "":
		state = "open"
	case "open", "closed", "merged":
	case "all":
		state = ""
	default:
		return nil, userErr("state must be open, closed, merged or all")
	}
	ps, err := c.a.db.Pulls(ctx, c.repo.ID, state)
	if err != nil {
		return nil, err
	}
	items, page, more := pageOf(ps, in.mcpPage)
	out := make([]mcpPullOut, 0, len(items))
	for _, p := range items {
		out = append(out, pullOut(c.repo, p, false))
	}
	return map[string]any{"pulls": out, "page": page, "has_more": more}, nil
}

func (c *mcpCall) pull(ctx context.Context, n int64) (store.Pull, error) {
	c.target = "#" + strconv.FormatInt(n, 10)
	p, err := c.a.db.PullByNumber(ctx, c.repo.ID, n)
	if errors.Is(err, store.ErrNotFound) {
		return p, userErr("no pull request #%d in %s", n, c.repo.FullName())
	}
	return p, err
}

// pullCompare compares a pull request the way its page does.
func (c *mcpCall) pullCompare(ctx context.Context, p store.Pull) (*gitrepo.Comparison, string, error) {
	headSHA, baseSHA := p.HeadSHA, p.BaseSHA
	if !p.IsMerged() {
		if h, err := c.git.BranchSHA(ctx, p.Head); err == nil {
			headSHA = h
		}
		b, err := c.git.BranchSHA(ctx, p.Base)
		if err != nil {
			return nil, headSHA, userErr("the base branch %s no longer exists", p.Base)
		}
		baseSHA = b
	}
	if headSHA == "" || baseSHA == "" {
		return nil, headSHA, userErr("this pull request has no commits to compare")
	}
	cmp, err := c.git.Compare(ctx, baseSHA, headSHA)
	if err != nil {
		return nil, headSHA, err
	}
	if cmp.Unrelated {
		return nil, headSHA, userErr("%s and %s have no common history", p.Head, p.Base)
	}
	return cmp, headSHA, nil
}

type mcpFileOut struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary,omitempty"`
}

type mcpNoteOut struct {
	ID       int64  `json:"id"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Side     string `json:"side"`
	Commit   string `json:"commit"`
	Author   string `json:"author"`
	Resolved bool   `json:"resolved"`
	Body     string `json:"body"`
}

func mcpPullsGet(ctx context.Context, c *mcpCall, in mcpNumberIn) (any, error) {
	p, err := c.pull(ctx, in.Number)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"pull": pullOut(c.repo, p, true)}
	if cmp, head, err := c.pullCompare(ctx, p); err == nil {
		out["head_sha"] = head
		out["commits"] = len(cmp.Commits)
		files := []mcpFileOut{}
		if cmp.Diff != nil {
			for _, f := range cmp.Diff.Files {
				files = append(files, mcpFileOut{Path: f.Path, OldPath: f.OldPath, Status: f.Status,
					Additions: f.Additions, Deletions: f.Deletions, Binary: f.Binary})
			}
			out["additions"], out["deletions"] = cmp.Diff.Additions, cmp.Diff.Deletions
		}
		if len(files) > 300 {
			files = files[:300]
			out["files_truncated"] = true
		}
		out["files"] = files
	} else {
		var ue *mcpUserError
		if !errors.As(err, &ue) {
			return nil, err
		}
		out["compare_error"] = ue.msg
	}
	cs, err := c.a.db.PullComments(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	comments := []mcpCommentOut{}
	for _, cm := range cs {
		comments = append(comments, mcpCommentOut{ID: cm.ID, Author: authorStr(cm.AuthorKind, cm.AuthorName),
			CreatedAt: timeStr(cm.CreatedAt), Body: cap64(cm.Body)})
	}
	if len(comments) > 100 {
		out["comments_truncated"] = true
		comments = comments[len(comments)-100:]
	}
	out["comments"] = comments
	ns, err := c.a.db.PullNotes(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	notes := []mcpNoteOut{}
	for _, n := range ns {
		notes = append(notes, mcpNoteOut{ID: n.ID, Path: n.Path, Line: n.Line, Side: n.Side, Commit: n.CommitSHA,
			Author: authorStr(n.AuthorKind, n.AuthorName), Resolved: n.Resolved, Body: cap64(n.Body)})
	}
	if len(notes) > 200 {
		out["notes_truncated"] = true
		notes = notes[len(notes)-200:]
	}
	out["notes"] = notes
	return out, nil
}

type mcpPullsDiffIn struct {
	mcpNumberIn
	Path string `json:"path,omitempty" jsonschema:"only this file"`
	mcpPage
}

type mcpPatchOut struct {
	mcpFileOut
	Patch     string `json:"patch"`
	Truncated bool   `json:"truncated,omitempty"`
}

func mcpPullsDiff(ctx context.Context, c *mcpCall, in mcpPullsDiffIn) (any, error) {
	p, err := c.pull(ctx, in.Number)
	if err != nil {
		return nil, err
	}
	cmp, head, err := c.pullCompare(ctx, p)
	if err != nil {
		return nil, err
	}
	var files []gitrepo.DiffFile
	if cmp.Diff != nil {
		files = cmp.Diff.Files
	}
	if in.Path != "" {
		var one []gitrepo.DiffFile
		for _, f := range files {
			if f.Path == in.Path || f.OldPath == in.Path {
				one = append(one, f)
			}
		}
		if len(one) == 0 {
			return nil, userErr("%s isn't changed in #%d", in.Path, p.Number)
		}
		files = one
	}
	items, page, more := pageOf(files, in.mcpPage)
	out := make([]mcpPatchOut, 0, len(items))
	for _, f := range items {
		po := mcpPatchOut{mcpFileOut: mcpFileOut{Path: f.Path, OldPath: f.OldPath, Status: f.Status,
			Additions: f.Additions, Deletions: f.Deletions, Binary: f.Binary}}
		po.Patch, po.Truncated = capText(renderPatch(f), mcpMaxText)
		po.Truncated = po.Truncated || f.Truncated
		out = append(out, po)
	}
	res := map[string]any{"number": p.Number, "head_sha": head, "base": p.Base, "head": p.Head, "files": out,
		"page": page, "has_more": more}
	if cmp.Diff != nil && cmp.Diff.Truncated {
		res["diff_truncated"] = true
	}
	return res, nil
}

// renderPatch turns parsed diff lines back into a unified patch.
func renderPatch(f gitrepo.DiffFile) string {
	var b strings.Builder
	old := f.Path
	if f.OldPath != "" {
		old = f.OldPath
	}
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", old, f.Path)
	if f.Binary {
		b.WriteString("(binary file)\n")
		return b.String()
	}
	for _, l := range f.Lines {
		switch l.Kind {
		case "add":
			b.WriteString("+" + l.Text + "\n")
		case "del":
			b.WriteString("-" + l.Text + "\n")
		case "ctx":
			b.WriteString(" " + l.Text + "\n")
		default:
			b.WriteString(l.Text + "\n")
		}
	}
	return b.String()
}

type mcpPullsCommentIn struct {
	mcpNumberIn
	Body string `json:"body" jsonschema:"the comment, in Markdown"`
}

func mcpPullsComment(ctx context.Context, c *mcpCall, in mcpPullsCommentIn) (any, error) {
	body, err := checkBody(in.Body, true)
	if err != nil {
		return nil, err
	}
	p, err := c.pull(ctx, in.Number)
	if err != nil {
		return nil, err
	}
	who := c.recordAs(ctx)
	cm, err := c.a.db.AddPullComment(ctx, p.ID, who.Kind, who.Name, body)
	if err != nil {
		return nil, err
	}
	anchor := fmt.Sprintf("#comment-%d", cm.ID)
	c.a.emit(ctx, pullEvent(EventPullCommented, c.view(), p, who, body, anchor))
	return map[string]any{"comment_id": cm.ID, "web_path": fmt.Sprintf("/%s/pulls/%d%s", c.repo.FullName(), p.Number, anchor)}, nil
}

func mcpPullsUpdateBranch(ctx context.Context, c *mcpCall, in mcpNumberIn) (any, error) {
	p, err := c.pull(ctx, in.Number)
	if err != nil {
		return nil, err
	}
	res, err := c.a.updatePullBranch(ctx, c.repo, c.git, p, c.who)
	var ue *pullUpdateError
	if errors.As(err, &ue) {
		return nil, userErr("%s", ue.Msg)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"updated": res.Updated, "head_sha": res.HeadSHA}, nil
}

type mcpPullsNoteIn struct {
	mcpNumberIn
	Path string `json:"path" jsonschema:"the changed file"`
	Line int    `json:"line" jsonschema:"line number in the new file (side new) or the old file (side old)"`
	Side string `json:"side,omitempty" jsonschema:"new (default) for added or unchanged lines, old for removed lines"`
	Body string `json:"body" jsonschema:"the note, in Markdown"`
}

func mcpPullsNote(ctx context.Context, c *mcpCall, in mcpPullsNoteIn) (any, error) {
	body, err := checkBody(in.Body, true)
	if err != nil {
		return nil, err
	}
	side := in.Side
	if side == "" {
		side = "new"
	}
	if side != "new" && side != "old" {
		return nil, userErr("side must be new or old")
	}
	if in.Path == "" || len(in.Path) > 4096 || in.Line < 1 {
		return nil, userErr("give the changed file's path and a line number")
	}
	p, err := c.pull(ctx, in.Number)
	if err != nil {
		return nil, err
	}
	cmp, head, err := c.pullCompare(ctx, p)
	if err != nil {
		return nil, err
	}
	var file *gitrepo.DiffFile
	if cmp.Diff != nil {
		for i := range cmp.Diff.Files {
			if cmp.Diff.Files[i].Path == in.Path {
				file = &cmp.Diff.Files[i]
				break
			}
		}
	}
	if file == nil {
		return nil, userErr("%s isn't changed in #%d; pulls.get lists the changed files", in.Path, p.Number)
	}
	found := false
	for _, l := range file.Lines {
		if side == "new" && (l.Kind == "add" || l.Kind == "ctx") && l.New == in.Line ||
			side == "old" && (l.Kind == "del" || l.Kind == "ctx") && l.Old == in.Line {
			found = true
			break
		}
	}
	if !found {
		return nil, userErr("line %d (%s side) isn't part of the diff of %s; pulls.diff shows the lines you can note", in.Line, side, in.Path)
	}
	who := c.recordAs(ctx)
	n, err := c.a.db.AddPullNote(ctx, store.NewPullNote{PullID: p.ID, CommitSHA: head, Path: in.Path, Line: in.Line,
		Side: side, Body: body, AuthorKind: who.Kind, AuthorName: who.Name})
	if err != nil {
		return nil, err
	}
	suffix := fmt.Sprintf("/files#note-%d", n.ID)
	c.a.emit(ctx, pullEvent(EventPullNote, c.view(), p, who, body, suffix))
	return map[string]any{"note_id": n.ID, "commit": head, "web_path": fmt.Sprintf("/%s/pulls/%d%s", c.repo.FullName(), p.Number, suffix)}, nil
}

// ---------- actions ----------

type mcpActionsRunsIn struct {
	mcpRepoArg
	Workflow string `json:"workflow,omitempty" jsonschema:"only runs of this workflow file, e.g. .atelier/workflows/ci.yml"`
	Branch   string `json:"branch,omitempty" jsonschema:"only runs on this branch"`
	Status   string `json:"status,omitempty" jsonschema:"only runs with this status or conclusion: queued, in_progress, completed, success, failure, cancelled"`
	mcpPage
}

type mcpRunOut struct {
	Run        int64  `json:"run"`
	Workflow   string `json:"workflow"`
	Path       string `json:"workflow_path"`
	Number     int64  `json:"workflow_run_number"`
	Event      string `json:"event"`
	Branch     string `json:"branch"`
	SHA        string `json:"sha"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	Actor      string `json:"actor"`
	CreatedAt  string `json:"created_at"`
	URL        string `json:"web_path"`
}

func mcpActionsRuns(ctx context.Context, c *mcpCall, in mcpActionsRunsIn) (any, error) {
	if len(in.Status) > 40 {
		return nil, userErr("status is too long")
	}
	off, lim, page := in.bounds()
	runs, err := c.a.db.ActionsRuns(ctx, store.ActionsRunFilter{RepoID: c.repo.ID, Workflow: in.Workflow, Branch: in.Branch,
		Status: in.Status, Limit: lim + 1, Offset: off})
	if err != nil {
		return nil, err
	}
	more := len(runs) > lim
	if more {
		runs = runs[:lim]
	}
	out := make([]mcpRunOut, 0, len(runs))
	for _, r := range runs {
		out = append(out, mcpRunOut{Run: r.Seq, Workflow: r.WorkflowName, Path: r.WorkflowPath, Number: r.RunNumber,
			Event: r.Event, Branch: r.Branch(), SHA: r.SHA, Title: truncate(r.Title, 200), Status: r.Status,
			Conclusion: r.Conclusion, Actor: authorStr(r.ActorKind, r.ActorName), CreatedAt: timeStr(r.CreatedAt),
			URL: actionsRunURL(c.repo, r)})
	}
	return map[string]any{"runs": out, "page": page, "has_more": more}, nil
}

type mcpActionsLogsIn struct {
	mcpRepoArg
	Run  int64  `json:"run" jsonschema:"the run number (from actions.runs)"`
	Job  int64  `json:"job,omitempty" jsonschema:"only this job id (from a previous actions.logs answer)"`
	Step string `json:"step,omitempty" jsonschema:"only this step: its number (from steps, 0 is the first) or part of its name"`
}

const (
	mcpLogBudget   = 64 << 10 // all jobs' logs in one answer
	mcpQuietTail   = 4 << 10  // the end of each job that passed
	mcpMaxProblems = 30       // error and warning lines listed per job
)

func mcpStepName(names map[int]string, i int) string {
	if n := names[i]; n != "" {
		return n
	}
	return "Step " + strconv.Itoa(i)
}

// mcpStepFilter turns the step argument into a test on step indexes (nil:
// every step). A number is the step's index; anything else matches names.
func mcpStepFilter(arg string, names map[int]string) func(int) bool {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return nil
	}
	if n, err := strconv.Atoi(arg); err == nil {
		return func(i int) bool { return i == n }
	}
	low := strings.ToLower(arg)
	return func(i int) bool { return strings.Contains(strings.ToLower(names[i]), low) }
}

// mcpPlainLog is a job's log as plain text for an AI to read, the way the
// run page shows it (actions_log.go) but in words: no colour codes, workflow
// plumbing left out, download progress counted instead of listed, and
// ::error:: lines written ERROR: …. It also returns the error lines and the
// ::warning:: lines, with N set to their step index.
func mcpPlainLog(chunks []store.ActionsLogChunk, names map[int]string, want func(int) bool) (string, []logRow, []logRow) {
	var (
		b           strings.Builder
		errs, warns []logRow
		parsers     = map[int]*logParser{}
		last        = -2
		pulls       int
	)
	flush := func() {
		if pulls > 0 {
			fmt.Fprintf(&b, "[%d lines of download progress]\n", pulls)
			pulls = 0
		}
	}
	for _, ch := range chunks {
		var lines []actions.LogLine
		if json.Unmarshal([]byte(ch.Lines), &lines) != nil {
			continue
		}
		for _, l := range lines {
			if want != nil && !want(l.Step) {
				continue
			}
			if l.Step != last {
				flush()
				last = l.Step
				b.WriteString("=== " + mcpStepName(names, l.Step) + " ===\n")
			}
			p := parsers[l.Step]
			if p == nil {
				p = &logParser{}
				parsers[l.Step] = p
			}
			r := p.row(l)
			switch r.Kind {
			case logEndGroup, logCommand, logDebug, logQuiet:
				continue
			case logPull:
				pulls++
				continue
			}
			flush()
			r.N = l.Step
			switch {
			case r.Kind == logGroup:
				b.WriteString("> " + r.Text)
			case r.Label != "":
				b.WriteString(strings.ToUpper(r.Kind) + ": " + r.Text)
				if r.Where != "" {
					b.WriteString(" (" + r.Where + ")")
				}
			default:
				b.WriteString(r.Text)
			}
			b.WriteByte('\n')
			switch {
			case r.Kind == logError:
				errs = append(errs, r)
			case r.Kind == logWarning && r.Label != "":
				warns = append(warns, r)
			}
			if b.Len() > 64<<20 {
				return b.String(), errs, warns
			}
		}
	}
	flush()
	return b.String(), errs, warns
}

func mcpActionsLogs(ctx context.Context, c *mcpCall, in mcpActionsLogsIn) (any, error) {
	c.target = "run " + strconv.FormatInt(in.Run, 10)
	run, err := c.a.db.ActionsRunBySeq(ctx, c.repo.ID, in.Run)
	if errors.Is(err, store.ErrNotFound) {
		return nil, userErr("no run %d in %s", in.Run, c.repo.FullName())
	}
	if err != nil {
		return nil, err
	}
	jobs, err := c.a.db.ActionsJobs(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	if in.Job != 0 {
		var one []store.ActionsJob
		for _, j := range jobs {
			if j.ID == in.Job {
				one = append(one, j)
			}
		}
		if len(one) == 0 {
			return nil, userErr("run %d has no job %d", in.Run, in.Job)
		}
		jobs = one
	}
	// One job: up to mcpMaxText. Several: jobs that passed only send their
	// end (mcpQuietTail), and the jobs that failed, were cancelled or are
	// still running share the rest of mcpLogBudget, so a whole release run
	// fits in what an AI client takes in one answer.
	loud := 0
	for _, j := range jobs {
		if j.Status != actions.StatusSuccess && j.Status != actions.StatusSkipped {
			loud++
		}
	}
	perLoud := mcpMaxText
	if len(jobs) > 1 && loud > 0 {
		perLoud = min(max((mcpLogBudget-mcpQuietTail*(len(jobs)-loud))/loud, 8<<10), mcpMaxText)
	}
	perJob := func(j store.ActionsJob) int {
		if len(jobs) > 1 && (j.Status == actions.StatusSuccess || j.Status == actions.StatusSkipped) {
			return mcpQuietTail
		}
		return perLoud
	}
	type stepOut struct {
		Index  int    `json:"step"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	type problem struct {
		Step  string `json:"step"`
		Kind  string `json:"kind"` // error or warning
		Text  string `json:"text"`
		Where string `json:"where,omitempty"`
	}
	type jobOut struct {
		ID         int64     `json:"job"`
		Name       string    `json:"name"`
		Status     string    `json:"status"`
		Error      string    `json:"error,omitempty"`
		Runner     string    `json:"runner,omitempty"`
		Steps      []stepOut `json:"steps"`
		Problems   []problem `json:"problems,omitempty"`
		Log        string    `json:"log"`
		Truncated  bool      `json:"truncated,omitempty"`
		Unfinished bool      `json:"running,omitempty"`
	}
	out := []jobOut{}
	for _, j := range jobs {
		chunks, err := c.a.db.ActionsLogs(ctx, j.ID, 0, 0)
		if err != nil {
			return nil, err
		}
		steps, _ := c.a.db.ActionsSteps(ctx, j.ID)
		jo := jobOut{ID: j.ID, Name: j.Name, Status: j.Status, Error: truncate(j.Error, 500),
			Runner: j.RunnerName, Unfinished: j.Status == actions.StatusRunning, Steps: []stepOut{}}
		names := map[int]string{-1: "Set up job"}
		for _, st := range steps {
			names[st.Index] = st.Name
			jo.Steps = append(jo.Steps, stepOut{Index: st.Index, Name: st.Name, Status: st.Status})
		}
		want := mcpStepFilter(in.Step, names)
		text, errs, warns := mcpPlainLog(chunks, names, want)
		jo.Log, jo.Truncated = capTail(text, perJob(j))
		for _, list := range [][]logRow{errs, warns} {
			for _, r := range list {
				if len(jo.Problems) == mcpMaxProblems {
					break
				}
				jo.Problems = append(jo.Problems, problem{Step: mcpStepName(names, r.N), Kind: r.Kind, Text: truncate(r.Text, 500), Where: r.Where})
			}
		}
		out = append(out, jo)
	}
	return map[string]any{"run": run.Seq, "workflow": run.WorkflowName, "status": run.Status,
		"conclusion": run.Conclusion, "jobs": out}, nil
}

type mcpActionsDispatchIn struct {
	mcpRepoArg
	Workflow string            `json:"workflow" jsonschema:"the workflow file path, e.g. .atelier/workflows/deploy.yml"`
	Ref      string            `json:"ref,omitempty" jsonschema:"the branch to run on (default: the default branch)"`
	Inputs   map[string]string `json:"inputs,omitempty" jsonschema:"workflow_dispatch inputs by name"`
}

func mcpActionsDispatch(ctx context.Context, c *mcpCall, in mcpActionsDispatchIn) (any, error) {
	wf := strings.TrimSpace(in.Workflow)
	if wf == "" || len(wf) > 4096 {
		return nil, userErr("give the workflow file path")
	}
	if len(in.Inputs) > 50 {
		return nil, userErr("too many inputs")
	}
	branch := strings.TrimSpace(in.Ref)
	if c.git.IsEmpty(ctx) {
		return nil, userErr("%s is empty", c.repo.FullName())
	}
	if branch == "" {
		branch = c.git.DefaultBranch(ctx)
	}
	c.target = wf
	run, err := c.a.dispatchWorkflow(ctx, c.repo, c.git, wf, branch, in.Inputs, c.recordAs(ctx))
	if err != nil {
		return nil, userErr("couldn't run the workflow: %s", truncate(err.Error(), 300))
	}
	return map[string]any{"run": run.Seq, "status": run.Status, "web_path": actionsRunURL(c.repo, run)}, nil
}

// ---------- releases ----------

type mcpReleaseOut struct {
	ID          int64  `json:"id"`
	Tag         string `json:"tag"`
	Title       string `json:"title"`
	Target      string `json:"target,omitempty"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	Author      string `json:"author"`
	CreatedAt   string `json:"created_at"`
	PublishedAt string `json:"published_at,omitempty"`
	Body        string `json:"notes"`
	URL         string `json:"web_path"`
}

func releaseOut(rp store.Repo, r store.Release) mcpReleaseOut {
	u := releaseTagURL(rp, r.Tag)
	if r.Draft {
		u = fmt.Sprintf("/%s/releases/edit/%d", rp.FullName(), r.ID)
	}
	return mcpReleaseOut{ID: r.ID, Tag: r.Tag, Title: r.Name(), Target: r.Target, Draft: r.Draft, Prerelease: r.Prerelease,
		Author: authorStr(r.AuthorKind, r.AuthorName), CreatedAt: timeStr(r.CreatedAt), PublishedAt: timePtrStr(r.PublishedAt),
		Body: cap64(r.Body), URL: u}
}

type mcpReleasesListIn struct {
	mcpRepoArg
	mcpPage
}

func mcpReleasesList(ctx context.Context, c *mcpCall, in mcpReleasesListIn) (any, error) {
	drafts := c.a.can(ctx, c.who, c.repo, PermWrite) ||
		c.who.Kind == ActorJob && c.who.hasScope(mcpToolScopePfx+"releases.draft")
	rs, err := c.a.db.Releases(ctx, c.repo.ID, drafts)
	if err != nil {
		return nil, err
	}
	items, page, more := pageOf(rs, in.mcpPage)
	out := make([]mcpReleaseOut, 0, len(items))
	for _, r := range items {
		out = append(out, releaseOut(c.repo, r))
	}
	return map[string]any{"releases": out, "page": page, "has_more": more}, nil
}

type mcpReleasesDraftIn struct {
	mcpRepoArg
	Tag        string `json:"tag" jsonschema:"the tag the release will use, e.g. v1.4.0 (it can be created when a person publishes)"`
	Title      string `json:"title,omitempty" jsonschema:"the release title (default: the tag)"`
	Body       string `json:"body" jsonschema:"the release notes, in Markdown"`
	Target     string `json:"target,omitempty" jsonschema:"branch or commit to tag when published (default: the default branch)"`
	Prerelease bool   `json:"prerelease,omitempty" jsonschema:"mark as a pre-release"`
}

func mcpReleasesDraft(ctx context.Context, c *mcpCall, in mcpReleasesDraftIn) (any, error) {
	body, err := checkBody(in.Body, false)
	if err != nil {
		return nil, err
	}
	c.target = strings.TrimSpace(in.Tag)
	who := c.recordAs(ctx)
	rel, err := c.a.createRelease(ctx, c.repo, c.git, who, releaseInput{Tag: in.Tag, Target: in.Target, Title: in.Title,
		Body: body, Draft: true, Prerelease: in.Prerelease})
	var re *relError
	if errors.As(err, &re) {
		return nil, userErr("%s", re.Msg)
	}
	if err != nil {
		return nil, err
	}
	ro := releaseOut(c.repo, rel)
	c.a.emit(ctx, Event{Kind: "release.drafted", Repo: c.repo, RepoID: c.repo.ID, Number: rel.ID, Title: rel.Name(),
		URL: ro.URL, Actor: who, Detail: eventExcerpt(body, 140)})
	return map[string]any{"release": ro}, nil
}

// ---------- notifications ----------

type mcpNotificationsListIn struct {
	UnreadOnly bool `json:"unread_only,omitempty" jsonschema:"only unread notifications"`
	mcpPage
}

func mcpNotificationsList(ctx context.Context, c *mcpCall, in mcpNotificationsListIn) (any, error) {
	if c.who.PersonID == 0 || c.who.Kind != ActorToken {
		return nil, userErr("notifications are only available to people's tokens")
	}
	readable, err := c.a.readableRepoIDs(ctx, c.who)
	if err != nil {
		return nil, err
	}
	off, lim, page := in.bounds()
	ns, err := c.a.db.Notifications(ctx, store.NotificationQuery{PersonID: c.who.PersonID, UnreadOnly: in.UnreadOnly,
		ReadableRepos: readable, Limit: lim + 1, Offset: off})
	if err != nil {
		return nil, err
	}
	more := len(ns) > lim
	if more {
		ns = ns[:lim]
	}
	type nOut struct {
		ID        int64  `json:"id"`
		Kind      string `json:"kind"`
		Repo      string `json:"repo,omitempty"`
		Number    int64  `json:"number,omitempty"`
		Title     string `json:"title"`
		Detail    string `json:"detail,omitempty"`
		Actor     string `json:"actor"`
		Reason    string `json:"reason"`
		Unread    bool   `json:"unread"`
		UpdatedAt string `json:"updated_at"`
		URL       string `json:"web_path"`
	}
	out := make([]nOut, 0, len(ns))
	for _, n := range ns {
		out = append(out, nOut{ID: n.ID, Kind: n.Kind, Repo: n.Repo, Number: n.Number, Title: n.Title, Detail: n.Detail,
			Actor: authorStr(n.ActorKind, n.ActorName), Reason: n.Reason, Unread: n.Unread, UpdatedAt: timeStr(n.UpdatedAt),
			URL: n.URL})
	}
	return map[string]any{"notifications": out, "page": page, "has_more": more}, nil
}
