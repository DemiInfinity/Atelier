package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"atelier/internal/store"
)

// ⌘K command palette: GET /-/palette?q=&repo= returns typed results for the
// palette in static/palette.js. "/-/" is a literal namespace no account can
// take ("-" is reserved and isn't a valid account name anyway), so these
// routes never clash with /{owner}.
//
// Sources, in priority order: repositories, files in the current repository,
// issues and pull requests, snippets, commands, then accounts. A prefix picks
// one source: ">" commands, "#" issues and pull requests, "/" files, "@"
// accounts.
//
// Everything is filtered through App.can before it is returned, so a person
// without read access never sees a private repository's name, its issue
// titles or its file paths. Anonymous callers only ever see public things.

func init() {
	registerFeature(feature{name: "palette", routes: func(a *App, m *http.ServeMux) {
		m.HandleFunc("GET /-/palette", a.paletteSearch)
	}})
}

const (
	paletteMax      = 20
	paletteMaxQuery = 200
	paletteMaxFiles = 100000
)

// palItem is one result.
type palItem struct {
	Kind  string `json:"kind"` // repo, file, issue, pull, snippet, command, account, github
	Group string `json:"group"`
	Title string `json:"title"`
	Hint  string `json:"hint,omitempty"`
	URL   string `json:"url"`
	score int
}

type palResponse struct {
	Q     string    `json:"q"`
	Mode  string    `json:"mode"` // "", "commands", "issues", "files", "accounts"
	Repo  string    `json:"repo,omitempty"`
	Items []palItem `json:"items"`
	Empty string    `json:"empty,omitempty"` // what to say when there are no items
}

var palModes = map[byte]string{'>': "commands", '#': "issues", '/': "files", '@': "accounts"}

func (a *App) paletteSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who, valid := a.requestActor(r, true)
	if !valid {
		w.Header().Set("WWW-Authenticate", `Bearer realm="Atelier"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > paletteMaxQuery {
		q = q[:paletteMaxQuery]
	}
	mode := ""
	if q != "" {
		if m, ok := palModes[q[0]]; ok {
			mode = m
			q = strings.TrimSpace(q[1:])
		}
	}
	res := palResponse{Q: q, Mode: mode, Items: []palItem{}}

	// Every repository the caller may read, most recently updated first.
	all, err := a.db.Repos(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "something went wrong"})
		return
	}
	var readable []store.Repo
	for _, rp := range all {
		if a.can(ctx, who, rp, PermRead) {
			readable = append(readable, rp)
		}
	}
	// The current repository counts only if the caller may read it; anything
	// else is ignored exactly as if it didn't exist.
	var cur *store.Repo
	if name := r.URL.Query().Get("repo"); name != "" {
		for i := range readable {
			if strings.EqualFold(readable[i].FullName(), name) {
				cur = &readable[i]
				res.Repo = cur.FullName()
				break
			}
		}
	}

	add := func(items []palItem, limit int) {
		for i, it := range items {
			if i >= limit || len(res.Items) >= paletteMax {
				return
			}
			res.Items = append(res.Items, it)
		}
	}
	switch mode {
	case "commands":
		add(a.palCommands(ctx, who, cur, q), paletteMax)
		if len(res.Items) == 0 {
			res.Empty = "No commands match."
		}
	case "issues":
		if cur == nil && q == "" {
			res.Empty = "Type an issue or pull request title, or open a repository to search by number."
			break
		}
		add(a.palIssues(ctx, readable, cur, q, paletteMax), paletteMax)
		if len(res.Items) == 0 {
			res.Empty = "No issues or pull requests match."
		}
	case "files":
		if cur == nil {
			res.Empty = "Open a repository to search its files."
			break
		}
		add(a.palFiles(ctx, *cur, q, paletteMax), paletteMax)
		if len(res.Items) == 0 {
			res.Empty = "No files match."
		}
	case "accounts":
		add(a.palAccounts(ctx, who, readable, q), paletteMax)
		if len(res.Items) == 0 {
			res.Empty = "No accounts match."
		}
	default:
		add(palRepos(readable, q), 6)
		if cur != nil && q != "" {
			add(a.palFiles(ctx, *cur, q, 5), 5)
		}
		if q != "" {
			add(a.palIssues(ctx, readable, cur, q, 4), 4)
			add(a.palSnippets(ctx, who, q), 3)
		}
		add(a.palCommands(ctx, who, cur, q), 6)
		if q != "" {
			add(a.palAccounts(ctx, who, readable, q), 3)
		}
		// Tether: hand the query to the search page; the palette itself never
		// calls GitHub.
		if q != "" && who.Kind == ActorOwner && len(res.Items) < paletteMax {
			res.Items = append(res.Items, palItem{Kind: "github", Group: "GitHub",
				Title: "Search GitHub for “" + q + "”", Hint: "via Tether",
				URL: "/search?" + url.Values{"src": {"github"}, "q": {q}}.Encode()})
		}
		if len(res.Items) == 0 {
			res.Empty = "Nothing matches. Try > for commands, # for issues, / for files or @ for accounts."
		}
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- sources ----------

func palRepos(readable []store.Repo, q string) []palItem {
	var out []palItem
	for _, rp := range readable {
		s := fuzzyScore(q, rp.FullName())
		if q != "" {
			if n := fuzzyScore(q, rp.Name); n >= 0 && n+4 > s {
				s = n + 4
			}
		}
		if s < 0 {
			continue
		}
		hint := "Public"
		if rp.Private {
			hint = "Private"
		}
		if rp.Description != "" {
			hint += " · " + ellipsize(rp.Description, 60)
		}
		out = append(out, palItem{Kind: "repo", Group: "Repositories", Title: rp.FullName(), Hint: hint,
			URL: "/" + rp.FullName(), score: s})
	}
	sortItems(out)
	return out
}

func (a *App) palFiles(ctx context.Context, rp store.Repo, q string, limit int) []palItem {
	files := a.repoFileList(ctx, rp)
	if len(files) == 0 {
		return nil
	}
	branch := files[0]
	files = files[1:]
	var out []palItem
	for _, p := range files {
		s := fuzzyScore(q, p)
		if s < 0 {
			continue
		}
		base := p[strings.LastIndexByte(p, '/')+1:]
		if q != "" {
			if n := fuzzyScore(q, base); n >= 0 && n+6 > s {
				s = n + 6
			}
		}
		out = append(out, palItem{Kind: "file", Group: "Files in " + rp.Name, Title: p, Hint: rp.Name,
			URL: "/" + rp.FullName() + "/blob/" + escapePath(branch) + "/" + escapePath(p), score: s})
	}
	sortItems(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// palIssues finds issues and pull requests: "12" is a number in the current
// repository, anything else searches titles in every readable repository
// (the current one first).
func (a *App) palIssues(ctx context.Context, readable []store.Repo, cur *store.Repo, q string, limit int) []palItem {
	var out []palItem
	item := func(rp store.Repo, kind string, number int64, title, state string) palItem {
		path, label := "issues", "Issue"
		if kind == "pull" {
			path, label = "pulls", "Pull request"
		}
		hint := fmt.Sprintf("%s #%d · %s", label, number, state)
		if cur == nil || rp.ID != cur.ID {
			hint = rp.FullName() + " · " + hint
		}
		return palItem{Kind: kind, Group: "Issues and pull requests", Title: title, Hint: hint,
			URL: fmt.Sprintf("/%s/%s/%d", rp.FullName(), path, number)}
	}
	seen := map[string]bool{}
	if cur != nil {
		if n, err := strconv.ParseInt(strings.TrimPrefix(q, "#"), 10, 64); err == nil && n > 0 {
			if is, err := a.db.IssueByNumber(ctx, cur.ID, n); err == nil {
				out = append(out, item(*cur, "issue", is.Number, is.Title, is.State))
			} else if p, err := a.db.PullByNumber(ctx, cur.ID, n); err == nil {
				out = append(out, item(*cur, "pull", p.Number, p.Title, p.State))
			}
			return out
		}
		if q == "" {
			issues, _, _ := a.db.Issues(ctx, cur.ID, store.IssueQuery{State: "open", Sort: "updated"}, 0, limit)
			for _, is := range issues {
				out = append(out, item(*cur, "issue", is.Number, is.Title, is.State))
			}
			pulls, _ := a.db.Pulls(ctx, cur.ID, "open")
			for i, p := range pulls {
				if i >= limit {
					break
				}
				out = append(out, item(*cur, "pull", p.Number, p.Title, p.State))
			}
			return out
		}
	}
	if utf8.RuneCountInString(q) < 2 {
		return out
	}
	var ids []int64
	byID := map[int64]store.Repo{}
	for _, rp := range readable {
		ids = append(ids, rp.ID)
		byID[rp.ID] = rp
	}
	items, err := a.db.SearchLocalItems(ctx, ids, q, 40)
	if err != nil {
		return out
	}
	// The current repository's matches come first; otherwise newest first.
	sort.SliceStable(items, func(i, j int) bool {
		return cur != nil && items[i].RepoID == cur.ID && items[j].RepoID != cur.ID
	})
	for _, it := range items {
		key := fmt.Sprintf("%d/%d", it.RepoID, it.Number)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, item(byID[it.RepoID], it.Kind, it.Number, it.Title, it.State))
		if len(out) >= limit {
			break
		}
	}
	return out
}

func (a *App) palSnippets(ctx context.Context, who Actor, q string) []palItem {
	sns, err := a.db.Snippets(ctx, q, 100)
	if err != nil {
		return nil
	}
	var out []palItem
	for _, sn := range sns {
		if !a.can(ctx, who, snippetRepo(sn), PermRead) {
			continue
		}
		hint := sn.Owner
		if sn.Description != "" {
			hint += " · " + ellipsize(sn.Description, 60)
		}
		out = append(out, palItem{Kind: "snippet", Group: "Snippets", Title: sn.Title(), Hint: hint,
			URL: "/snippets/" + sn.UID})
	}
	return out
}

// palSettings are the account settings sections (the "accountnav" partial).
var palSettings = []struct{ Name, Path string }{
	{"Profile", "profile"}, {"Repositories", "repositories"}, {"Access tokens", "tokens"}, {"SSH keys", "keys"},
	{"Passkeys", "passkeys"}, {"Secrets and variables", "secrets"}, {"Runners", "runners"},
	{"Backups and storage", "backups"}, {"Import", "import"}, {"Tether", "tether"}, {"Packages", "packages"},
	{"Apps and bots", "apps"}, {"MCP", "mcp"},
}

// palCommands are actions and places. Only a signed-in person gets them.
func (a *App) palCommands(ctx context.Context, who Actor, cur *store.Repo, q string) []palItem {
	if who.Kind != ActorOwner {
		return nil
	}
	type cmd struct{ title, hint, url, keys string }
	var cmds []cmd
	if cur != nil {
		if a.can(ctx, who, *cur, PermWrite) {
			cmds = append(cmds, cmd{"New issue in " + cur.Name, cur.FullName(), "/" + cur.FullName() + "/issues/new", "create"})
		}
		cmds = append(cmds, cmd{"Actions in " + cur.Name, cur.FullName(), "/" + cur.FullName() + "/actions", "workflows runs ci"})
	}
	cmds = append(cmds,
		cmd{"New repository", "Create", "/new", "create repo"},
		cmd{"New snippet", "Create", "/snippets/new", "create gist"},
		cmd{"Notifications", "Go to", "/notifications", "inbox"},
		cmd{"Snippets", "Go to", "/snippets", "gists"},
		cmd{"Search", "Go to", "/search", "find"},
	)
	for _, s := range palSettings {
		cmds = append(cmds, cmd{"Settings: " + s.Name, "Settings", "/settings/" + s.Path, "preferences"})
	}
	var out []palItem
	for i, c := range cmds {
		s := fuzzyScore(q, c.title)
		if q != "" {
			if n := fuzzyScore(q, c.keys); n >= 0 && n-2 > s {
				s = n - 2
			}
		} else {
			s = -i // keep the order above
		}
		if s < 0 && q != "" {
			continue
		}
		out = append(out, palItem{Kind: "command", Group: "Commands", Title: c.title, Hint: c.hint, URL: c.url, score: s})
	}
	sortItems(out)
	return out
}

// palAccounts lists accounts. The owner sees every account; anyone else only
// accounts that own a repository they can read, so an account with nothing
// but private repositories stays unseen.
func (a *App) palAccounts(ctx context.Context, who Actor, readable []store.Repo, q string) []palItem {
	owners, err := a.db.Owners(ctx)
	if err != nil {
		return nil
	}
	visible := map[int64]bool{}
	for _, rp := range readable {
		visible[rp.OwnerID] = true
	}
	all := (who.Kind == ActorOwner || who.Kind == ActorToken) && a.isOwnerPerson(ctx, who.PersonID)
	var out []palItem
	for _, o := range owners {
		if !all && !visible[o.ID] {
			continue
		}
		s := fuzzyScore(q, o.Name)
		if n := fuzzyScore(q, o.Label()); n > s {
			s = n
		}
		if s < 0 {
			continue
		}
		hint := "Personal account"
		if o.IsCompany() {
			hint = "Company account"
		}
		title := o.Name
		if o.DisplayName != "" && o.DisplayName != o.Name {
			title = o.DisplayName + " (" + o.Name + ")"
		}
		out = append(out, palItem{Kind: "account", Group: "Accounts", Title: title, Hint: hint, URL: "/" + o.Name, score: s})
	}
	sortItems(out)
	return out
}

// ---------- file lists, cached per (repository, commit) ----------

type palFileCache struct {
	mu    sync.Mutex
	m     map[string][]string
	order []string
}

const palFileCacheSize = 48

var palFilesCache = &palFileCache{m: map[string][]string{}}

func (c *palFileCache) get(key string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[key]
	return v, ok
}

func (c *palFileCache) put(key string, v []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; !ok {
		c.order = append(c.order, key)
		if len(c.order) > palFileCacheSize {
			delete(c.m, c.order[0])
			c.order = c.order[1:]
		}
	}
	c.m[key] = v
}

// repoFileList returns the default branch name followed by every file path
// at its tip. The list is cached by commit, so a push invalidates it.
func (a *App) repoFileList(ctx context.Context, rp store.Repo) []string {
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		return nil
	}
	branch := g.DefaultBranch(ctx)
	commit, err := g.ResolveCommit(ctx, branch)
	if err != nil {
		return nil
	}
	dir := a.git.Path(rp.Owner, rp.Name)
	key := dir + "@" + commit
	if files, ok := palFilesCache.get(key); ok {
		return append([]string{branch}, files...)
	}
	out, err := a.snipRun(ctx, dir, nil, nil, "ls-tree", "-r", "-z", "--name-only", commit)
	if err != nil {
		return nil
	}
	var files []string
	for _, p := range strings.Split(out, "\x00") {
		if p == "" {
			continue
		}
		files = append(files, p)
		if len(files) >= paletteMaxFiles {
			break
		}
	}
	palFilesCache.put(key, files)
	return append([]string{branch}, files...)
}

// ---------- matching ----------

// fuzzyScore matches pattern against s as a case-insensitive subsequence.
// It returns -1 for no match; higher is better. Consecutive characters, word
// starts and an early, short match score higher. An empty pattern matches
// everything with score 0.
func fuzzyScore(pattern, s string) int {
	if pattern == "" {
		return 0
	}
	p := []rune(strings.ReplaceAll(strings.ToLower(pattern), " ", ""))
	t := []rune(strings.ToLower(s))
	if len(p) == 0 {
		return 0
	}
	score, pi, prev, first := 0, 0, -2, -1
	for ti := 0; ti < len(t) && pi < len(p); ti++ {
		if t[ti] != p[pi] {
			continue
		}
		if first < 0 {
			first = ti
		}
		score++
		if ti == prev+1 {
			score += 4
		}
		if ti == 0 || strings.ContainsRune("/-_. ", t[ti-1]) {
			score += 3
		}
		prev = ti
		pi++
	}
	if pi < len(p) {
		return -1
	}
	if strings.Contains(string(t), string(p)) {
		score += 8
		if strings.HasPrefix(string(t), string(p)) {
			score += 6
		}
	}
	score -= first / 4
	score -= len(t) / 16
	if score < 0 {
		score = 0
	}
	return score
}

func sortItems(items []palItem) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].score > items[j].score })
}

// ellipsize shortens s to n runes, ending in an ellipsis when cut.
func ellipsize(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
