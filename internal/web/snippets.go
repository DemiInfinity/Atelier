package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// Snippets: small versioned files, like GitHub gists. Each one belongs to an
// account, holds one or more files and is private or public. Its content is a
// git repository (see snippets_git.go), so it has history and can be cloned
// and pushed at /snippets/<id>.git with the same tokens as repositories.
//
// Access goes through App.can on a stand-in repository (snippetRepo), so
// snippets follow exactly the rules repositories do: the owner can do
// anything, tokens by scope, anyone else may read public snippets. Anything a
// reader may not see answers 404.

func init() {
	registerFeature(feature{name: "snippets", routes: snippetRoutes})
}

func snippetRoutes(a *App, m *http.ServeMux) {
	m.HandleFunc("GET /snippets", a.authed(a.snippetsList))
	m.HandleFunc("GET /snippets/new", a.authed(a.snippetNewPage))
	m.HandleFunc("POST /snippets", a.authed(a.snippetCreate))
	m.HandleFunc("GET /snippets/{id}", a.snippetView)
	m.HandleFunc("GET /snippets/{id}/raw/{name...}", a.snippetRaw)
	m.HandleFunc("GET /snippets/{id}/archive.zip", a.snippetZip)
	m.HandleFunc("GET /snippets/{id}/revisions", a.snippetRevisions)
	m.HandleFunc("GET /snippets/{id}/edit", a.authed(a.snippetEditPage))
	m.HandleFunc("POST /snippets/{id}/edit", a.authed(a.snippetEditSubmit))
	m.HandleFunc("POST /snippets/{id}/delete", a.authed(a.snippetDelete))
	m.HandleFunc("POST /snippets/{id}/star", a.authed(a.snippetStar))
}

// ---------- loading and access ----------

// viewer is who is looking at a page that public snippets show to anyone: the
// signed-in person, else anonymous.
func (a *App) viewer(r *http.Request) Actor {
	if who, ok := r.Context().Value(actorKey{}).(Actor); ok {
		return who
	}
	if who, ok := a.sessionActor(r); ok {
		return who
	}
	return Actor{Kind: ActorAnonymous, Name: "anonymous"}
}

// loadSnippet finds the snippet in the URL and checks need. A snippet the
// viewer can't read is "not found"; one they can read but not change is
// "forbidden".
func (a *App) loadSnippet(w http.ResponseWriter, r *http.Request, need Perm) (store.Snippet, *gitrepo.Repo, Actor, bool) {
	ctx := r.Context()
	who := a.viewer(r)
	uid := strings.ToLower(r.PathValue("id"))
	if !validSnippetUID(uid) {
		a.snippetNotFound(w, r, who)
		return store.Snippet{}, nil, who, false
	}
	sn, err := a.db.SnippetByUID(ctx, uid)
	if err != nil || !a.can(ctx, who, snippetRepo(sn), PermRead) {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			a.fail(w, r, err)
		} else {
			a.snippetNotFound(w, r, who)
		}
		return sn, nil, who, false
	}
	if need > PermRead && !a.can(ctx, who, snippetRepo(sn), need) {
		a.forbidden(w, r)
		return sn, nil, who, false
	}
	g, err := a.openSnippet(sn.UID)
	if err != nil {
		a.fail(w, r, err)
		return sn, nil, who, false
	}
	return sn, g, who, true
}

func (a *App) snippetNotFound(w http.ResponseWriter, r *http.Request, who Actor) {
	if who.Kind == ActorAnonymous {
		w.WriteHeader(http.StatusNotFound)
		a.render(w, r, "error", page{Title: "Not found", Bare: true, Data: map[string]string{
			"Heading": "Nothing here", "Message": "That snippet doesn't exist, or you need to sign in to see it.",
		}})
		return
	}
	a.notFound(w, r)
}

// snippetOwners are the accounts who may create snippets in.
func (a *App) snippetOwners(ctx context.Context, who Actor) ([]store.Owner, error) {
	all, err := a.db.Owners(ctx)
	if err != nil {
		return nil, err
	}
	var out []store.Owner
	for _, o := range all {
		if a.can(ctx, who, ownerRepo(o), PermWrite) {
			out = append(out, o)
		}
	}
	return out, nil
}

// ---------- list ----------

type snippetCard struct {
	S       store.Snippet
	File    string
	Extra   int // more files beyond the first
	Preview string
	More    bool
}

type snippetsListData struct {
	Tab, Q  string
	Cards   []snippetCard
	Counts  map[string]int
	TabURLs map[string]string
}

const snippetPreviewLines = 10

func (a *App) snippetsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := a.actor(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > 200 {
		q = q[:200]
	}
	tab := r.URL.Query().Get("tab")
	if tab != "starred" {
		tab = "yours"
	}
	all, err := a.db.Snippets(ctx, q, 500)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var yours []store.Snippet
	for _, sn := range all {
		if a.can(ctx, who, snippetRepo(sn), PermWrite) {
			yours = append(yours, sn)
		}
	}
	var starred []store.Snippet
	if who.PersonID != 0 {
		st, err := a.db.StarredSnippets(ctx, who.PersonID, q)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		for _, sn := range st {
			if a.can(ctx, who, snippetRepo(sn), PermRead) {
				starred = append(starred, sn)
			}
		}
	}
	d := &snippetsListData{Tab: tab, Q: q, Counts: map[string]int{"yours": len(yours), "starred": len(starred)},
		TabURLs: map[string]string{}}
	for _, t := range []string{"yours", "starred"} {
		v := url.Values{}
		if t != "yours" {
			v.Set("tab", t)
		}
		if q != "" {
			v.Set("q", q)
		}
		d.TabURLs[t] = "/snippets"
		if len(v) > 0 {
			d.TabURLs[t] += "?" + v.Encode()
		}
	}
	list := yours
	if tab == "starred" {
		list = starred
	}
	if len(list) > 60 {
		list = list[:60]
	}
	for _, sn := range list {
		d.Cards = append(d.Cards, a.snippetPreview(ctx, sn))
	}
	a.render(w, r, "snippets_list", page{Title: "Snippets", Nav: "snippets", Data: d})
}

// snippetPreview reads the first lines of a snippet's first file.
func (a *App) snippetPreview(ctx context.Context, sn store.Snippet) snippetCard {
	c := snippetCard{S: sn, File: sn.Title(), Extra: max(len(sn.Files)-1, 0)}
	if len(sn.Files) == 0 {
		return c
	}
	dir := a.snippetGit().Path(snippetShard(sn.UID), sn.UID)
	out, err := a.snipRun(ctx, dir, nil, nil, "cat-file", "-p", "HEAD:"+sn.Files[0])
	if err != nil {
		return c
	}
	if len(out) > 4000 {
		out, c.More = out[:4000], true
	}
	if strings.IndexByte(out, 0) >= 0 {
		c.Preview = "Binary file"
		return c
	}
	lines := strings.Split(out, "\n")
	if len(lines) > snippetPreviewLines {
		lines, c.More = lines[:snippetPreviewLines], true
	}
	c.Preview = strings.Join(lines, "\n")
	return c
}

// ---------- create and edit ----------

type snippetFormData struct {
	Edit        bool
	S           store.Snippet
	Owners      []store.Owner
	Owner       string
	Description string
	Private     bool
	Files       []snippetFile
	Parent      string
	Action      string
	Focus       int // index of the file to focus (after Add file), or -1
}

// LastIndex is the index of the editor's last file (Add file jumps to it).
func (d *snippetFormData) LastIndex() int { return len(d.Files) - 1 }

func (a *App) snippetNewPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := a.actor(r)
	owners, err := a.snippetOwners(ctx, who)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if len(owners) == 0 {
		a.forbidden(w, r)
		return
	}
	d := &snippetFormData{Owners: owners, Owner: owners[0].Name, Private: true, Action: "/snippets",
		Files: []snippetFile{{}}, Focus: -1}
	if o := r.URL.Query().Get("owner"); o != "" {
		d.Owner = o
	}
	a.render(w, r, "snippets_edit", page{Title: "New snippet", Nav: "snippets", Data: d})
}

// readSnippetForm reads the editor and applies Add file / Remove buttons.
// done is true when the form was submitted to save.
func readSnippetForm(r *http.Request, d *snippetFormData) (done bool) {
	d.Description = strings.TrimSpace(r.PostFormValue("description"))
	if len(d.Description) > snippetMaxDesc {
		d.Description = d.Description[:snippetMaxDesc]
	}
	d.Private = r.PostFormValue("visibility") != "public"
	if o := r.PostFormValue("owner"); o != "" {
		d.Owner = o
	}
	d.Parent = r.PostFormValue("parent")
	names, contents := r.PostForm["name"], r.PostForm["content"]
	d.Files = nil
	for i := range names {
		f := snippetFile{Name: strings.TrimSpace(names[i])}
		if i < len(contents) {
			f.Content = contents[i]
		}
		d.Files = append(d.Files, f)
	}
	d.Focus = -1
	op := r.PostFormValue("op")
	switch {
	case op == "add":
		if len(d.Files) < snippetMaxFiles {
			d.Files = append(d.Files, snippetFile{})
		}
		d.Focus = len(d.Files) - 1
		return false
	case strings.HasPrefix(op, "remove:"):
		i, err := strconv.Atoi(strings.TrimPrefix(op, "remove:"))
		if err == nil && i >= 0 && i < len(d.Files) {
			d.Files = append(d.Files[:i], d.Files[i+1:]...)
		}
		if len(d.Files) == 0 {
			d.Files = []snippetFile{{}}
		}
		return false
	}
	// Drop files left completely blank (an extra Add file).
	var kept []snippetFile
	for _, f := range d.Files {
		if f.Name == "" && strings.TrimSpace(f.Content) == "" {
			continue
		}
		kept = append(kept, f)
	}
	d.Files = kept
	return true
}

func (a *App) snippetCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := a.actor(r)
	owners, err := a.snippetOwners(ctx, who)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, snippetMaxFiles*snippetMaxBytes+64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the snippet is too large", http.StatusRequestEntityTooLarge)
		return
	}
	d := &snippetFormData{Owners: owners, Action: "/snippets"}
	again := func(msg string) {
		if len(d.Files) == 0 {
			d.Files = []snippetFile{{}}
		}
		a.render(w, r, "snippets_edit", page{Title: "New snippet", Nav: "snippets", Error: msg, Data: d})
	}
	if !readSnippetForm(r, d) {
		again("")
		return
	}
	var owner store.Owner
	found := false
	for _, o := range owners {
		if strings.EqualFold(o.Name, d.Owner) {
			owner, found = o, true
		}
	}
	if !found {
		w.WriteHeader(http.StatusForbidden)
		again("Choose an account you can create snippets in.")
		return
	}
	if err := checkSnippetFiles(d.Files); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		again(capitalise(err.Error()) + ".")
		return
	}
	sn, err := a.createSnippet(ctx, who, owner, d.Description, d.Private, d.Files)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/snippets/"+sn.UID, http.StatusSeeOther)
}

// createSnippet makes the repository, the first commit and the row.
func (a *App) createSnippet(ctx context.Context, who Actor, owner store.Owner, desc string, private bool,
	files []snippetFile) (store.Snippet, error) {
	uid := newSnippetUID()
	if _, err := a.initSnippetRepo(ctx, uid); err != nil {
		return store.Snippet{}, err
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	if _, err := a.commitSnippet(ctx, uid, "", files, nil, "Create "+strings.Join(names, ", ")); err != nil {
		_ = a.removeSnippetRepo(uid)
		return store.Snippet{}, err
	}
	sn, err := a.db.CreateSnippet(ctx, uid, owner.ID, desc, private, nil, who.Kind, who.Name)
	if err != nil {
		_ = a.removeSnippetRepo(uid)
		return store.Snippet{}, err
	}
	a.refreshSnippetFiles(ctx, sn)
	return a.db.SnippetByUID(ctx, uid)
}

func (a *App) snippetEditPage(w http.ResponseWriter, r *http.Request) {
	sn, g, _, ok := a.loadSnippet(w, r, PermWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	d := &snippetFormData{Edit: true, S: sn, Owner: sn.Owner, Description: sn.Description, Private: sn.Private,
		Action: "/snippets/" + sn.UID + "/edit", Focus: -1}
	d.Parent = a.snippetTip(ctx, g)
	if d.Parent != "" {
		entries, err := g.Tree(ctx, d.Parent, "")
		if err != nil {
			a.fail(w, r, err)
			return
		}
		for _, e := range entries {
			if e.Type != "blob" {
				continue
			}
			b, err := g.Blob(ctx, d.Parent, e.Name, snippetMaxBytes)
			if err != nil || b.Binary || b.TooLarge {
				// Files the editor can't hold are kept as they are on save.
				continue
			}
			d.Files = append(d.Files, snippetFile{Name: e.Name, Content: string(b.Data)})
		}
	}
	if len(d.Files) == 0 {
		d.Files = []snippetFile{{}}
	}
	a.render(w, r, "snippets_edit", page{Title: "Edit snippet", Nav: "snippets", Data: d})
}

func (a *App) snippetEditSubmit(w http.ResponseWriter, r *http.Request) {
	sn, g, _, ok := a.loadSnippet(w, r, PermWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, snippetMaxFiles*snippetMaxBytes+64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the snippet is too large", http.StatusRequestEntityTooLarge)
		return
	}
	d := &snippetFormData{Edit: true, S: sn, Owner: sn.Owner, Action: "/snippets/" + sn.UID + "/edit"}
	again := func(msg string) {
		if len(d.Files) == 0 {
			d.Files = []snippetFile{{}}
		}
		a.render(w, r, "snippets_edit", page{Title: "Edit snippet", Nav: "snippets", Error: msg, Data: d})
	}
	if !readSnippetForm(r, d) {
		again("")
		return
	}
	d.Owner = sn.Owner
	if err := checkSnippetFiles(d.Files); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		again(capitalise(err.Error()) + ".")
		return
	}
	tip := a.snippetTip(ctx, g)
	if d.Parent != tip {
		w.WriteHeader(http.StatusConflict)
		d.Parent = tip
		again("The snippet changed since you opened the editor (perhaps a push). Your text is below; save again to replace the newer version.")
		return
	}
	files := d.Files
	changed, err := a.snippetFilesChanged(ctx, g, tip, files)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if changed {
		if _, err := a.commitSnippetKeeping(ctx, sn.UID, g, tip, files); err != nil {
			w.WriteHeader(http.StatusConflict)
			again(capitalise(err.Error()) + ".")
			return
		}
	}
	if err := a.db.UpdateSnippet(ctx, sn.ID, d.Description, d.Private); err != nil {
		a.fail(w, r, err)
		return
	}
	a.refreshSnippetFiles(ctx, sn)
	http.Redirect(w, r, "/snippets/"+sn.UID, http.StatusSeeOther)
}

// snippetFilesChanged reports whether saving files would change the snippet's
// editable files, so a description-only edit makes no empty revision.
func (a *App) snippetFilesChanged(ctx context.Context, g *gitrepo.Repo, tip string, files []snippetFile) (bool, error) {
	if tip == "" {
		return true, nil
	}
	entries, err := g.Tree(ctx, tip, "")
	if err != nil {
		return false, err
	}
	editable := map[string]string{}
	for _, e := range entries {
		if e.Type != "blob" {
			continue
		}
		b, err := g.Blob(ctx, tip, e.Name, snippetMaxBytes)
		if err != nil || b.Binary || b.TooLarge {
			continue
		}
		editable[e.Name] = string(b.Data)
	}
	if len(editable) != len(files) {
		return true, nil
	}
	for _, f := range files {
		old, ok := editable[f.Name]
		content := strings.ReplaceAll(f.Content, "\r\n", "\n")
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if !ok || old != content {
			return true, nil
		}
	}
	return false, nil
}

// commitSnippetKeeping commits files, carrying over top-level files the
// editor can't show (binary or too large) unless one of files replaces them.
func (a *App) commitSnippetKeeping(ctx context.Context, uid string, g *gitrepo.Repo, tip string, files []snippetFile) (string, error) {
	keep := map[string]bool{}
	if tip != "" {
		inForm := map[string]bool{}
		for _, f := range files {
			inForm[strings.ToLower(f.Name)] = true
		}
		entries, err := g.Tree(ctx, tip, "")
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			if e.Type != "blob" || inForm[strings.ToLower(e.Name)] {
				continue
			}
			if b, err := g.Blob(ctx, tip, e.Name, snippetMaxBytes); err == nil && (b.Binary || b.TooLarge) {
				keep[e.Name] = true
			}
		}
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	return a.commitSnippet(ctx, uid, tip, files, keep, "Update "+strings.Join(names, ", "))
}

func (a *App) snippetDelete(w http.ResponseWriter, r *http.Request) {
	sn, _, _, ok := a.loadSnippet(w, r, PermAdmin)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := a.db.DeleteSnippet(ctx, sn.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.removeSnippetRepo(sn.UID); err != nil {
		logErr(r, fmt.Errorf("remove snippet %s: %w", sn.UID, err))
	}
	http.Redirect(w, r, "/snippets", http.StatusSeeOther)
}

func (a *App) snippetStar(w http.ResponseWriter, r *http.Request) {
	sn, _, who, ok := a.loadSnippet(w, r, PermRead)
	if !ok {
		return
	}
	if who.PersonID == 0 {
		a.forbidden(w, r)
		return
	}
	if err := a.db.SetSnippetStar(r.Context(), who.PersonID, sn.ID, r.PostFormValue("star") == "1"); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/snippets/"+sn.UID, http.StatusSeeOther)
}

// ---------- view ----------

type snippetViewFile struct {
	Name     string
	Anchor   string
	Blob     *gitrepo.Blob
	Markdown bool
	RawURL   string
}

type snippetViewData struct {
	S        store.Snippet
	Anon     bool
	CanEdit  bool
	CanAdmin bool
	Starred  bool
	CanStar  bool
	Rev      string // a past revision being viewed, or ""
	Tip      string
	Files    []snippetViewFile
	CloneURL string
	Base     string
	Count    int
	Last     *gitrepo.Commit
}

func (a *App) snippetView(w http.ResponseWriter, r *http.Request) {
	sn, g, who, ok := a.loadSnippet(w, r, PermRead)
	if !ok {
		return
	}
	ctx := r.Context()
	d := &snippetViewData{S: sn, Anon: who.Kind == ActorAnonymous, Base: "/snippets/" + sn.UID,
		CloneURL: a.baseURL(r) + "/snippets/" + sn.UID + ".git"}
	d.CanEdit = a.can(ctx, who, snippetRepo(sn), PermWrite)
	d.CanAdmin = a.can(ctx, who, snippetRepo(sn), PermAdmin)
	d.CanStar = who.PersonID != 0
	if d.CanStar {
		d.Starred = a.db.SnippetStarred(ctx, who.PersonID, sn.ID)
	}
	d.Tip = a.snippetTip(ctx, g)
	commit := d.Tip
	if rev := r.URL.Query().Get("rev"); rev != "" {
		c, err := g.ResolveCommit(ctx, rev)
		if err != nil {
			a.snippetNotFound(w, r, who)
			return
		}
		commit = c
		if c != d.Tip {
			d.Rev = c
		}
	}
	if commit != "" {
		d.Count = g.CommitCount(ctx, d.Tip)
		d.Last = g.LastCommit(ctx, commit, "")
		for i, name := range a.snippetFilesAt(ctx, sn.UID, g, commit) {
			b, err := g.Blob(ctx, commit, name, gitrepo.MaxBlob)
			if err != nil {
				continue
			}
			raw := d.Base + "/raw/" + escapePath(name)
			if d.Rev != "" {
				raw += "?rev=" + d.Rev
			}
			d.Files = append(d.Files, snippetViewFile{Name: name, Anchor: "file-" + strconv.Itoa(i+1), Blob: b,
				Markdown: isMarkdown(name) && !b.Binary && !b.TooLarge && r.URL.Query().Get("plain") == "",
				RawURL:   raw})
		}
	}
	title := sn.Title()
	a.render(w, r, "snippets_view", page{Title: title + " · Snippet", Nav: "snippets", Bare: d.Anon, Data: d})
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func (a *App) snippetRaw(w http.ResponseWriter, r *http.Request) {
	_, g, who, ok := a.loadSnippet(w, r, PermRead)
	if !ok {
		return
	}
	ctx := r.Context()
	commit := a.snippetTip(ctx, g)
	if rev := r.URL.Query().Get("rev"); rev != "" {
		c, err := g.ResolveCommit(ctx, rev)
		if err != nil {
			a.snippetNotFound(w, r, who)
			return
		}
		commit = c
	}
	if commit == "" {
		a.snippetNotFound(w, r, who)
		return
	}
	b, err := g.Blob(ctx, commit, r.PathValue("name"), 0)
	if err != nil {
		a.snippetNotFound(w, r, who)
		return
	}
	// Never let raw files render as HTML in Atelier's origin.
	if b.Binary {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("Content-Security-Policy", "sandbox")
	_, _ = w.Write(b.Data)
}

func (a *App) snippetZip(w http.ResponseWriter, r *http.Request) {
	sn, g, who, ok := a.loadSnippet(w, r, PermRead)
	if !ok {
		return
	}
	ctx := r.Context()
	commit := a.snippetTip(ctx, g)
	if commit == "" {
		a.snippetNotFound(w, r, who)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="snippet-`+sn.UID+`.zip"`)
	if err := g.Archive(ctx, w, commit, "zip", "snippet-"+sn.UID); err != nil {
		logErr(r, err)
	}
}

// ---------- revisions ----------

type snippetRevision struct {
	C     gitrepo.Commit
	Patch string
	First bool
}

type snippetRevisionsData struct {
	S         store.Snippet
	Anon      bool
	Base      string
	Revisions []snippetRevision
	Page      int
	Prev      int
	Next      int
	More      bool
}

const snippetRevisionsPerPage = 10

func (a *App) snippetRevisions(w http.ResponseWriter, r *http.Request) {
	sn, g, who, ok := a.loadSnippet(w, r, PermRead)
	if !ok {
		return
	}
	ctx := r.Context()
	d := &snippetRevisionsData{S: sn, Anon: who.Kind == ActorAnonymous, Base: "/snippets/" + sn.UID, Page: 1}
	if n, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && n > 1 {
		d.Page = n
	}
	if tip := a.snippetTip(ctx, g); tip != "" {
		cs, err := g.Log(ctx, tip, "", (d.Page-1)*snippetRevisionsPerPage, snippetRevisionsPerPage+1)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if len(cs) > snippetRevisionsPerPage {
			cs, d.More = cs[:snippetRevisionsPerPage], true
		}
		for _, c := range cs {
			_, patch, _ := g.Show(ctx, c.Hash)
			d.Revisions = append(d.Revisions, snippetRevision{C: c, Patch: patch, First: len(c.Parents) == 0})
		}
	}
	d.Prev, d.Next = d.Page-1, d.Page+1
	a.render(w, r, "snippets_revisions", page{Title: "Revisions · " + sn.Title(), Nav: "snippets", Bare: d.Anon, Data: d})
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
