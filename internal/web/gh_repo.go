package web

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"atelier/internal/store"
	"atelier/internal/tether"
)

// /gh/{owner}/{repo}: a GitHub repository laid out like Atelier's own repo
// pages, with a blue Tether accent.

// ghRepoView is the header and About sidebar of every repository page.
type ghRepoView struct {
	ghBase
	Full     string // owner/repo
	Repo     tether.Repo
	Owner    tetherPerson
	Base     string // /gh/owner/repo
	GH       string // on github.com
	Tab      string
	Ref      string
	Path     string
	Crumbs   []crumb
	Branches []string
	Tags     []string
	Default  string
	Homepage string // a safe http(s) link
	License  string
	Topics   []string
	Local    string // "/owner/name" when it's linked or mirrored in Atelier
	LocalHow string // "linked" or "mirrored"
	Back     string // this page, for the Mirror form
	Desc     string
}

// ghLoadRepo reads the repository (and, with refs, its branches and tags).
func (a *App) ghLoadRepo(w http.ResponseWriter, r *http.Request, g *ghReq, tab string, refs bool) (*ghRepoView, bool) {
	ctx := r.Context()
	full := r.PathValue("owner") + "/" + r.PathValue("repo")
	if !tether.ValidRepo(full) {
		a.notFound(w, r)
		return nil, false
	}
	rp, err := g.c.Repo(ctx, full)
	if err != nil {
		a.ghFail(w, r, g, err, a.ghWebURL(full))
		return nil, false
	}
	if tether.ValidRepo(rp.FullName) && !strings.EqualFold(rp.FullName, full) {
		// Renamed or transferred: follow GitHub.
		http.Redirect(w, r, ghLocalURL(rp.FullName)+strings.TrimPrefix(r.URL.EscapedPath(), ghLocalURL(full)), http.StatusSeeOther)
		return nil, false
	}
	full = rp.FullName
	if !tether.ValidRepo(full) {
		full = r.PathValue("owner") + "/" + r.PathValue("repo")
	}
	v := &ghRepoView{Full: full, Repo: rp, Base: ghLocalURL(full), GH: a.ghWebURL(full), Tab: tab,
		Default: rp.DefaultBranch, Homepage: tetherSafeStudio(rp.Homepage), Desc: eventExcerpt(rp.Description, 400),
		Back: r.URL.EscapedPath()}
	if v.Default == "" {
		v.Default = "main"
	}
	if rp.License != nil && rp.License.Name != "" && rp.License.Key != "other" {
		v.License = eventExcerpt(rp.License.Name, 80)
	}
	for i, t := range rp.Topics {
		if i >= 20 {
			break
		}
		v.Topics = append(v.Topics, eventExcerpt(t, 50))
	}
	owner := rp.Owner
	if owner.Login == "" {
		owner.Login = r.PathValue("owner")
	}
	v.Owner = a.ghPerson(owner, a.ghBadges(ctx, g, []string{owner.Login}))
	if refs {
		if bs, err := g.c.Branches(ctx, full); err == nil {
			for _, b := range bs {
				v.Branches = append(v.Branches, b.Name)
			}
		}
		if ts, err := g.c.Tags(ctx, full); err == nil {
			for _, t := range ts {
				v.Tags = append(v.Tags, t.Name)
			}
		}
	}
	v.Local, v.LocalHow = a.ghLocalCopy(ctx, g, full)
	return v, true
}

// ghLocalCopy finds the repository in Atelier when it's linked by Tether or
// mirrored from GitHub, and the person may read it.
func (a *App) ghLocalCopy(ctx context.Context, g *ghReq, full string) (string, string) {
	if links, err := a.db.TetherLinks(ctx); err == nil {
		for _, l := range links {
			if strings.EqualFold(l.GHRepo, full) {
				if rp, err := a.db.RepoByID(ctx, l.RepoID); err == nil && a.can(ctx, g.who, rp, PermRead) {
					return "/" + rp.FullName(), "linked"
				}
			}
		}
	}
	if ms, err := a.db.ImportMirrors(ctx); err == nil {
		want := strings.ToLower(a.tetherRemote(full))
		for _, m := range ms {
			src := strings.ToLower(strings.TrimSuffix(m.SourceURL, "/"))
			if src == want || src+".git" == want || src == strings.ToLower(a.tetherWebURL(full)) {
				if rp, err := a.db.RepoByID(ctx, m.RepoID); err == nil && a.can(ctx, g.who, rp, PermRead) {
					return "/" + rp.FullName(), "mirrored"
				}
			}
		}
	}
	return "", ""
}

var ghFullSHARe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ghSplitRef splits "<ref>/<path>" using the repository's branches and tags
// (longest match), so branches with slashes work.
func (a *App) ghSplitRef(ctx context.Context, g *ghReq, full, rest string) (ref, p string) {
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return "", ""
	}
	first, after, _ := strings.Cut(rest, "/")
	if ghFullSHARe.MatchString(first) {
		return first, after
	}
	var names []string
	if bs, err := g.c.Branches(ctx, full); err == nil {
		for _, b := range bs {
			names = append(names, b.Name)
		}
	}
	if strings.Contains(rest, "/") {
		if ts, err := g.c.Tags(ctx, full); err == nil {
			for _, t := range ts {
				names = append(names, t.Name)
			}
		}
	}
	best := ""
	for _, n := range names {
		if (rest == n || strings.HasPrefix(rest, n+"/")) && len(n) > len(best) {
			best = n
		}
	}
	if best == "" {
		return first, after
	}
	return best, strings.TrimPrefix(strings.TrimPrefix(rest, best), "/")
}

func (v *ghRepoView) setCrumbs(kind string) {
	v.Crumbs = nil
	if v.Path == "" {
		return
	}
	acc := ""
	for _, seg := range strings.Split(v.Path, "/") {
		acc = strings.TrimPrefix(acc+"/"+seg, "/")
		v.Crumbs = append(v.Crumbs, crumb{Name: seg, URL: v.Base + "/" + kind + "/" + ghEscPath(v.Ref) + "/" + ghEscPath(acc)})
	}
}

// RefURL is the tree of another branch or tag at the same path.
func (v *ghRepoView) TreeURL(ref string) string {
	return v.Base + "/tree/" + ghEscPath(ref)
}

// RootURL is the tree root of the current ref.
func (v *ghRepoView) RootURL() string { return v.TreeURL(v.Ref) }

// CommitsURL lists commits for the current ref (and path).
func (v *ghRepoView) CommitsURL() string {
	ref := v.Ref
	if ref == "" {
		ref = v.Default
	}
	u := v.Base + "/commits/" + ghEscPath(ref)
	if v.Path != "" {
		u += "/" + ghEscPath(v.Path)
	}
	return u
}

// ---------- code ----------

type ghFileRow struct {
	Name, URL string
	Dir       bool
	Kind      string // dir, file, symlink, submodule
}

type ghCommitRow struct {
	SHA, Short, Subject, URL, GH string
	Author                       tetherPerson
	AuthorName                   string
	When                         time.Time
}

func (a *App) ghCode(w http.ResponseWriter, r *http.Request, g *ghReq) {
	v, ok := a.ghLoadRepo(w, r, g, "code", true)
	if !ok {
		return
	}
	v.Ref = v.Default
	a.ghShowTree(w, r, g, v)
}

func (a *App) ghTree(w http.ResponseWriter, r *http.Request, g *ghReq) {
	v, ok := a.ghLoadRepo(w, r, g, "code", true)
	if !ok {
		return
	}
	v.Ref, v.Path = a.ghSplitRef(r.Context(), g, v.Full, r.PathValue("rest"))
	if v.Ref == "" {
		v.Ref = v.Default
	}
	a.ghShowTree(w, r, g, v)
}

func (a *App) ghShowTree(w http.ResponseWriter, r *http.Request, g *ghReq, v *ghRepoView) {
	ctx := r.Context()
	dir, file, err := g.c.Contents(ctx, v.Full, v.Ref, v.Path)
	if err != nil {
		if tether.IsStatus(err, http.StatusNotFound) && v.Path == "" {
			// An empty repository has no contents.
			v.ghBase = g.base()
			a.render(w, r, "gh_code", page{Title: v.Full, Data: map[string]any{"V": v, "Empty": true}})
			return
		}
		a.ghFail(w, r, g, err, v.GH)
		return
	}
	if file != nil {
		http.Redirect(w, r, v.Base+"/blob/"+ghEscPath(v.Ref)+"/"+ghEscPath(v.Path), http.StatusSeeOther)
		return
	}
	v.setCrumbs("tree")
	var rows []ghFileRow
	readme := ""
	for _, e := range dir {
		if e.Name == "" || strings.ContainsAny(e.Name, "/\x00") {
			continue
		}
		p := strings.TrimPrefix(v.Path+"/"+e.Name, "/")
		row := ghFileRow{Name: e.Name, Kind: e.Type, Dir: e.Type == "dir"}
		switch e.Type {
		case "dir":
			row.URL = v.Base + "/tree/" + ghEscPath(v.Ref) + "/" + ghEscPath(p)
		case "submodule":
			row.URL = a.ghWebURL(v.Full) + "/tree/" + url.PathEscape(v.Ref) + "/" + ghEscPath(p)
		default:
			row.URL = v.Base + "/blob/" + ghEscPath(v.Ref) + "/" + ghEscPath(p)
		}
		rows = append(rows, row)
		if e.Type == "file" && readme == "" && strings.HasPrefix(strings.ToLower(e.Name), "readme") {
			readme = e.Name
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Dir != rows[j].Dir {
			return rows[i].Dir
		}
		return strings.ToLower(rows[i].Name) < strings.ToLower(rows[j].Name)
	})
	data := map[string]any{"V": v, "Rows": rows}
	if v.Path == "" {
		data["Langs"] = a.ghLangBar(ctx, g, v) // langbar.go
	}
	// Latest commit touching this folder.
	if cs, err := g.c.Commits(ctx, v.Full, v.Ref, v.Path, 1, 1); err == nil && len(cs) > 0 {
		row := a.ghCommitRow(v, cs[0], a.ghBadges(ctx, g, ghCommitLogins(cs)))
		data["Last"] = row
	}
	if readme != "" {
		if rd, err := g.c.Readme(ctx, v.Full, v.Ref, v.Path); err == nil {
			if b, ok := rd.Decoded(); ok && len(b) <= 1<<20 {
				data["ReadmeName"] = rd.Name
				if isMarkdown(rd.Name) {
					data["ReadmeHTML"] = a.ghMarkdown(string(b), ghMD{Repo: v.Full, Ref: v.Ref, Dir: path.Dir(rd.Path)})
				} else if utf8.Valid(b) {
					data["ReadmeText"] = string(b)
				}
			}
		}
	}
	v.ghBase = g.base()
	a.render(w, r, "gh_code", page{Title: v.Full, Data: data})
}

func ghCommitLogins(cs []tether.CommitInfo) []string {
	var out []string
	for _, c := range cs {
		if c.Author != nil {
			out = append(out, c.Author.Login)
		}
	}
	return out
}

func (a *App) ghCommitRow(v *ghRepoView, c tether.CommitInfo, badges map[string]store.TetherBadge) ghCommitRow {
	subject, _, _ := strings.Cut(c.Commit.Message, "\n")
	row := ghCommitRow{SHA: c.SHA, Subject: eventExcerpt(subject, 200), AuthorName: eventExcerpt(c.Commit.Author.Name, 80),
		When: c.Commit.Author.Date}
	if ghFullSHARe.MatchString(c.SHA) {
		row.Short = c.SHA[:7]
		row.URL = v.Base + "/commit/" + c.SHA
		row.GH = a.ghWebURL(v.Full + "/commit/" + c.SHA)
	}
	if c.Author != nil && tether.ValidLogin(c.Author.Login) {
		row.Author = a.ghPerson(*c.Author, badges)
	}
	return row
}

// ---------- files ----------

const ghMaxShow = 1 << 20 // larger files: "View on GitHub"

var ghImageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".svg": true, ".avif": true, ".ico": true, ".bmp": true}

func (a *App) ghBlob(w http.ResponseWriter, r *http.Request, g *ghReq) {
	ctx := r.Context()
	v, ok := a.ghLoadRepo(w, r, g, "code", true)
	if !ok {
		return
	}
	v.Ref, v.Path = a.ghSplitRef(ctx, g, v.Full, r.PathValue("rest"))
	if v.Ref == "" || v.Path == "" {
		http.Redirect(w, r, v.Base, http.StatusSeeOther)
		return
	}
	dir, f, err := g.c.Contents(ctx, v.Full, v.Ref, v.Path)
	if err != nil {
		a.ghFail(w, r, g, err, v.GH+"/blob/"+url.PathEscape(v.Ref)+"/"+ghEscPath(v.Path))
		return
	}
	if dir != nil {
		http.Redirect(w, r, v.Base+"/tree/"+ghEscPath(v.Ref)+"/"+ghEscPath(v.Path), http.StatusSeeOther)
		return
	}
	v.setCrumbs("blob")
	name := path.Base(v.Path)
	data := map[string]any{"V": v, "Name": name, "Size": f.Size,
		"GHFile": v.GH + "/blob/" + ghEscPath(v.Ref) + "/" + ghEscPath(v.Path)}
	if cs, err := g.c.Commits(ctx, v.Full, v.Ref, v.Path, 1, 1); err == nil && len(cs) > 0 {
		data["Last"] = a.ghCommitRow(v, cs[0], a.ghBadges(ctx, g, ghCommitLogins(cs)))
	}
	ext := strings.ToLower(path.Ext(name))
	switch {
	case f.Type != "file":
		data["Unsupported"] = true
	case ghImageExts[ext] && f.Size <= ghImgMax:
		data["Image"] = "/gh/-/img?" + url.Values{"u": {"https://" + ghRawHost + "/" + v.Full + "/" + ghEscPath(v.Ref) + "/" + ghEscPath(v.Path)}}.Encode()
	case f.Size > ghMaxShow:
		data["TooLarge"] = true
	default:
		b, ok := f.Decoded()
		switch {
		case !ok:
			data["TooLarge"] = true
		case ghBinary(b):
			data["Binary"] = true
		default:
			data["Lines"] = len(codeLines(b))
			data["Lang"] = codeLang(name, b)
			if isMarkdown(name) && r.URL.Query().Get("plain") != "1" {
				data["Markdown"] = a.ghMarkdown(string(b), ghMD{Repo: v.Full, Ref: v.Ref, Dir: path.Dir(v.Path)})
				data["IsMarkdown"] = true
			} else {
				data["Code"] = highlightFile(name, b)
				data["IsMarkdown"] = isMarkdown(name)
			}
		}
	}
	v.ghBase = g.base()
	a.render(w, r, "gh_blob", page{Title: name + " · " + v.Full, Data: data})
}

func ghBinary(b []byte) bool {
	head := b
	if len(head) > 8000 {
		head = head[:8000]
	}
	for _, c := range head {
		if c == 0 {
			return true
		}
	}
	return !utf8.Valid(b)
}

// ---------- commits ----------

func (a *App) ghCommits(w http.ResponseWriter, r *http.Request, g *ghReq) {
	ctx := r.Context()
	v, ok := a.ghLoadRepo(w, r, g, "commits", true)
	if !ok {
		return
	}
	v.Ref, v.Path = a.ghSplitRef(ctx, g, v.Full, r.PathValue("rest"))
	if v.Ref == "" {
		v.Ref = v.Default
	}
	page := ghPageNum(r)
	cs, err := g.c.Commits(ctx, v.Full, v.Ref, v.Path, page, 30)
	if err != nil {
		a.ghFail(w, r, g, err, v.GH+"/commits")
		return
	}
	badges := a.ghBadges(ctx, g, ghCommitLogins(cs))
	var rows []ghCommitRow
	for _, c := range cs {
		rows = append(rows, a.ghCommitRow(v, c, badges))
	}
	v.setCrumbs("tree")
	v.ghBase = g.base()
	a.render(w, r, "gh_commits", page2(v.Full, map[string]any{"V": v, "Rows": rows, "Pager": pagerFor(r, page, len(cs) == 30)}))
}

func page2(title string, data any) page { return page{Title: title, Data: data} }

type ghFileDiff struct {
	Name, Old, Status string
	Add, Del          int
	Patch             string
	NoPatch           bool
	URL               string
}

func ghDiffs(v *ghRepoView, ref string, files []tether.CommitFile) []ghFileDiff {
	var out []ghFileDiff
	for _, f := range files {
		d := ghFileDiff{Name: f.Filename, Status: f.Status, Add: f.Additions, Del: f.Deletions}
		if f.PreviousFilename != "" && f.PreviousFilename != f.Filename {
			d.Old = f.PreviousFilename
		}
		if f.Patch == "" {
			d.NoPatch = true
		} else {
			old := f.Filename
			if d.Old != "" {
				old = d.Old
			}
			patch := f.Patch
			if len(patch) > 512<<10 {
				patch = patch[:512<<10] + "\n…"
			}
			d.Patch = "--- a/" + old + "\n+++ b/" + f.Filename + "\n" + patch
		}
		if ref != "" && f.Status != "removed" && !strings.Contains(f.Filename, "..") {
			d.URL = v.Base + "/blob/" + ghEscPath(ref) + "/" + ghEscPath(f.Filename)
		}
		out = append(out, d)
	}
	return out
}

func (a *App) ghCommit(w http.ResponseWriter, r *http.Request, g *ghReq) {
	ctx := r.Context()
	v, ok := a.ghLoadRepo(w, r, g, "commits", false)
	if !ok {
		return
	}
	sha := r.PathValue("sha")
	if !ghSHARe.MatchString(sha) {
		a.notFound(w, r)
		return
	}
	c, err := g.c.Commit(ctx, v.Full, sha)
	if err != nil {
		a.ghFail(w, r, g, err, v.GH+"/commit/"+sha)
		return
	}
	if !ghFullSHARe.MatchString(c.SHA) {
		a.notFound(w, r)
		return
	}
	v.Ref = c.SHA
	logins := ghCommitLogins([]tether.CommitInfo{c})
	if c.Committer != nil {
		logins = append(logins, c.Committer.Login)
	}
	badges := a.ghBadges(ctx, g, logins)
	subject, body, _ := strings.Cut(c.Commit.Message, "\n")
	var parents []string
	for _, p := range c.Parents {
		if ghFullSHARe.MatchString(p.SHA) {
			parents = append(parents, p.SHA)
		}
	}
	data := map[string]any{"V": v, "C": a.ghCommitRow(v, c, badges), "Subject": subject, "Body": strings.TrimSpace(body),
		"Parents": parents, "Files": ghDiffs(v, c.SHA, c.Files), "GHCommit": a.ghWebURL(v.Full + "/commit/" + c.SHA)}
	if c.Stats != nil {
		data["Add"], data["Del"] = c.Stats.Additions, c.Stats.Deletions
	}
	v.ghBase = g.base()
	a.render(w, r, "gh_commit", page2(eventExcerpt(subject, 80)+" · "+v.Full, data))
}

// ---------- releases ----------

type ghReleaseRow struct {
	Name, Tag, URL, Tree string
	Draft, Pre           bool
	When                 time.Time
	Author               tetherPerson
	Notes                any
}

func (a *App) ghReleases(w http.ResponseWriter, r *http.Request, g *ghReq) {
	ctx := r.Context()
	v, ok := a.ghLoadRepo(w, r, g, "releases", false)
	if !ok {
		return
	}
	page := ghPageNum(r)
	rels, err := g.c.ReleasesPage(ctx, v.Full, page, 10)
	if err != nil {
		a.ghFail(w, r, g, err, v.GH+"/releases")
		return
	}
	var logins []string
	for _, rel := range rels {
		logins = append(logins, rel.Author.Login)
	}
	badges := a.ghBadges(ctx, g, logins)
	var rows []ghReleaseRow
	for _, rel := range rels {
		name := rel.Name
		if name == "" {
			name = rel.TagName
		}
		row := ghReleaseRow{Name: eventExcerpt(name, 200), Tag: eventExcerpt(rel.TagName, 100), Draft: rel.Draft, Pre: rel.Prerelease,
			When: rel.PublishedAt, URL: a.tetherHTMLURL(rel.HTMLURL, v.Full+"/releases")}
		if rel.TagName != "" {
			row.Tree = v.Base + "/tree/" + ghEscPath(rel.TagName)
		}
		if rel.Author.Login != "" {
			row.Author = a.ghPerson(rel.Author, badges)
		}
		if strings.TrimSpace(rel.Body) != "" {
			row.Notes = a.ghMarkdown(rel.Body, ghMD{Repo: v.Full, Ref: rel.TagName, Issues: true})
		}
		rows = append(rows, row)
	}
	v.ghBase = g.base()
	a.render(w, r, "gh_releases", page2("Releases · "+v.Full, map[string]any{"V": v, "Rows": rows, "Pager": pagerFor(r, page, len(rels) == 10)}))
}
