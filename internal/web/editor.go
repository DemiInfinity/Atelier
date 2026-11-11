package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

	"atelier/internal/gitrepo"
)

// The web editor: edit, create, upload, delete and rename files from the code
// browser. Every change becomes a commit authored as the signed-in person and
// goes through editorCommit (editor_commit.go), which applies the same
// pre-receive checks as a push.
//
// Routes (none clash with tree/blob: each has its own literal segment):
//
//	GET  /{owner}/{repo}/edit/{branch}/{path...}         edit, rename or move a file
//	GET  /{owner}/{repo}/new/{branch}[/{folder...}]      create a file (path may add folders)
//	GET  /{owner}/{repo}/upload/{branch}[/{folder...}]   upload files
//	GET  /{owner}/{repo}/delete-file/{branch}/{path...}  delete a file
//	POST /{owner}/{repo}/{edit,new,upload,delete-file}?at=<branch>/<path>
//	POST /{owner}/{repo}/edit-preview                    Markdown preview
//
// The POSTs take the branch and path in ?at= because a POST pattern ending in
// {rest...} would clash with POST /settings/<x>/{id}/<verb> in ServeMux.

const (
	editMaxText        = 1 << 20  // largest file the text editor opens or saves
	editMaxUploadFile  = 10 << 20 // per uploaded file
	editMaxUploadTotal = 25 << 20 // per upload
	editMaxUploadFiles = 50
)

func init() {
	registerFeature(feature{name: "editor", routes: func(a *App, m *http.ServeMux) {
		m.HandleFunc("GET /{owner}/{repo}/edit/{rest...}", a.authed(a.editPage))
		m.HandleFunc("POST /{owner}/{repo}/edit", a.authed(a.editSubmit))
		m.HandleFunc("GET /{owner}/{repo}/new/{rest...}", a.authed(a.newFilePage))
		m.HandleFunc("POST /{owner}/{repo}/new", a.authed(a.newFileSubmit))
		m.HandleFunc("GET /{owner}/{repo}/upload/{rest...}", a.authed(a.uploadPage))
		m.HandleFunc("POST /{owner}/{repo}/upload", a.authed(a.uploadSubmit))
		m.HandleFunc("GET /{owner}/{repo}/delete-file/{rest...}", a.authed(a.deleteFilePage))
		m.HandleFunc("POST /{owner}/{repo}/delete-file", a.authed(a.deleteFileSubmit))
		m.HandleFunc("POST /{owner}/{repo}/edit-preview", a.authed(a.editPreview))
	}})
}

// editorForm is what the editor templates get.
type editorForm struct {
	V              *repoView
	Mode           string // edit, new, upload, delete
	Branch         string
	Dir            string // new / upload: the folder
	OrigPath       string // edit / delete: the file as it was
	Path           string // edit / new: where it goes
	Name           string
	Content        string
	Editable       bool // the text editor is shown (edit: false for binary or huge files)
	Markdown       bool
	BaseCommit     string
	DefaultMessage string
	Message        string
	Description    string
	Choice         string // direct, branch
	NewBranch      string
	Protection     string // why a direct commit isn't allowed ("" when it is)
	Problem        string
	Rejected       []string
	Stale          *editorStale
	Action         string
	CancelURL      string
	MaxUploadMB    int
	MaxTotalMB     int
	MaxFiles       int
}

// editLoad opens the repository for an editor page: the person needs write
// access (404 without read, 403 with read only). rest is "<branch>/<path>".
func (a *App) editLoad(w http.ResponseWriter, r *http.Request) (*repoView, *gitrepo.Repo, string, string, bool) {
	v, g, ok := a.loadRepo(w, r, "code")
	if !ok {
		return nil, nil, "", "", false
	}
	if !a.can(r.Context(), a.actor(r), v.Repo, PermWrite) {
		a.forbidden(w, r)
		return nil, nil, "", "", false
	}
	rest := r.PathValue("rest")
	if r.Method == http.MethodPost {
		rest = r.URL.Query().Get("at")
	}
	rest = strings.Trim(rest, "/")
	var branch, p string
	if v.Empty {
		branch, p, _ = strings.Cut(rest, "/")
		if !gitrepo.ValidBranchName(branch) {
			a.notFound(w, r)
			return nil, nil, "", "", false
		}
	} else {
		ref, rp, found := g.SplitRefPath(r.Context(), rest)
		if !found {
			a.notFound(w, r)
			return nil, nil, "", "", false
		}
		if _, err := g.BranchSHA(r.Context(), ref); err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			a.render(w, r, "error", page{Title: "Not a branch", Data: map[string]string{
				"Heading": "Files can only be changed on a branch", "Message": ref + " is a tag or a commit. Switch to a branch to edit.",
			}})
			return nil, nil, "", "", false
		}
		branch, p = ref, rp
	}
	v.Ref, v.Path = branch, p
	return v, g, branch, p, true
}

func (a *App) newEditorForm(ctx context.Context, v *repoView, g *gitrepo.Repo, who Actor, mode, branch string) *editorForm {
	f := &editorForm{V: v, Mode: mode, Branch: branch, Choice: "direct",
		MaxUploadMB: editMaxUploadFile >> 20, MaxTotalMB: editMaxUploadTotal >> 20, MaxFiles: editMaxUploadFiles}
	if !v.Empty {
		f.BaseCommit, _ = g.BranchSHA(ctx, branch)
	}
	f.Protection = a.editProtection(ctx, v, branch)
	if f.Protection != "" {
		f.Choice = "branch"
	}
	f.NewBranch = a.editBranchName(ctx, g, who)
	return f
}

// editProtection says why branch can't take a direct commit ("" if it can).
func (a *App) editProtection(ctx context.Context, v *repoView, branch string) string {
	p, err := a.db.ProtectionFor(ctx, v.Repo.ID, branch)
	if err != nil {
		return "Atelier couldn't read the branch rules for " + branch + ", so commit to a new branch."
	}
	if p.RequirePR {
		return branch + " is protected by branch rule " + p.Patterns() + ": changes must go through a pull request. " +
			"Commit to a new branch and Atelier opens the pull request for you."
	}
	return ""
}

// editBranchName suggests "<name>-patch-N", the first one that's free.
func (a *App) editBranchName(ctx context.Context, g *gitrepo.Repo, who Actor) string {
	name := strings.ToLower(who.Name)
	if p, err := a.db.Person(ctx, who.PersonID); err == nil {
		name = strings.ToLower(p.Name)
	}
	if !gitrepo.ValidBranchName(name + "-patch-1") {
		name = "web"
	}
	for i := 1; i < 1000; i++ {
		b := fmt.Sprintf("%s-patch-%d", name, i)
		if _, err := g.BranchSHA(ctx, b); err != nil {
			return b
		}
	}
	return name + "-patch"
}

func (a *App) renderEditor(w http.ResponseWriter, r *http.Request, f *editorForm, status int) {
	if f.Message == "" {
		f.Message = f.DefaultMessage
	}
	tmpl, title := "editor_edit", "Editing "+f.Name
	switch f.Mode {
	case "new":
		title = "New file"
	case "upload":
		tmpl, title = "editor_upload", "Upload files"
	case "delete":
		tmpl, title = "editor_delete", "Delete "+f.Name
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	a.render(w, r, tmpl, page{Title: title + " · " + f.V.Repo.FullName(), Nav: "repos", Data: f})
}

func joinRepoPath(dir, name string) string {
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// ---------- edit, rename, move ----------

func (a *App) editPage(w http.ResponseWriter, r *http.Request) {
	v, g, branch, p, ok := a.editLoad(w, r)
	if !ok {
		return
	}
	if v.Empty || p == "" {
		a.notFound(w, r)
		return
	}
	ctx := r.Context()
	f := a.newEditorForm(ctx, v, g, a.actor(r), "edit", branch)
	if g.ObjectType(ctx, f.BaseCommit, p) != "blob" {
		a.notFound(w, r)
		return
	}
	b, err := g.Blob(ctx, f.BaseCommit, p, editMaxText)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.fillEditForm(f, v, branch, p)
	f.Editable = !b.Binary && !b.TooLarge && utf8.Valid(b.Data)
	if f.Editable {
		f.Content = string(b.Data)
	}
	a.renderEditor(w, r, f, http.StatusOK)
}

func (a *App) fillEditForm(f *editorForm, v *repoView, branch, p string) {
	f.OrigPath, f.Path, f.Name = p, p, path.Base(p)
	f.Markdown = isMarkdown(f.Name)
	f.DefaultMessage = "Update " + f.Name
	f.Action = v.Base + "/edit?at=" + url.QueryEscape(branch+"/"+p)
	f.CancelURL = v.Base + "/blob/" + branch + "/" + p
}

func (a *App) editSubmit(w http.ResponseWriter, r *http.Request) {
	v, g, branch, p, ok := a.editLoad(w, r)
	if !ok {
		return
	}
	if v.Empty || p == "" {
		a.notFound(w, r)
		return
	}
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, 3*editMaxText)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the file is too large for the web editor", http.StatusRequestEntityTooLarge)
		return
	}
	who := a.actor(r)
	f := a.newEditorForm(ctx, v, g, who, "edit", branch)
	a.fillEditForm(f, v, branch, p)
	a.readCommitForm(r, f)
	f.Path = strings.Trim(strings.TrimSpace(r.FormValue("path")), "/")
	f.Editable = r.FormValue("keep_content") != "1"
	f.Content = r.FormValue("content")

	base := f.BaseCommit
	if base == "" {
		f.Problem = "Atelier lost track of which version you were editing. Reload the page and try again."
		a.renderEditor(w, r, f, http.StatusUnprocessableEntity)
		return
	}
	if !gitrepo.ValidEditPath(f.Path) {
		f.Problem = "Use a file path like docs/notes.md: no leading slash, no empty, “.” or “..” folders, and nothing inside .git."
		a.renderEditor(w, r, f, http.StatusUnprocessableEntity)
		return
	}
	change := gitrepo.FileChange{Path: f.Path}
	if f.Path != p {
		change.From = p
	}
	var orig []byte
	if b, err := g.Blob(ctx, base, p, editMaxText); err == nil && !b.TooLarge {
		orig = b.Data
	}
	if f.Editable {
		if len(f.Content) > editMaxText {
			f.Problem = "That's too much text for the web editor (1 MB at most)."
			a.renderEditor(w, r, f, http.StatusRequestEntityTooLarge)
			return
		}
		change.Content = editNormalize(f.Content, orig)
	} else {
		change.KeepContent = true
	}
	def := "Update " + path.Base(p)
	switch {
	case change.From != "" && (change.KeepContent || bytes.Equal(change.Content, orig)):
		def = "Rename " + p + " to " + f.Path
	case change.From != "":
		def = "Update and rename " + p + " to " + f.Path
	}
	f.DefaultMessage = def
	touched := []string{p}
	if change.From != "" {
		touched = append(touched, f.Path)
	}
	a.editorFinish(w, r, v, g, f, editorCommit{Branch: branch, Base: base, Changes: []gitrepo.FileChange{change},
		Touched: touched, Message: f.Message, Description: f.Description, DefaultMessage: def,
		Choice: f.Choice, NewBranch: f.NewBranch}, f.Path)
}

// editNormalize turns the browser's CRLF line endings back into the file's own.
func editNormalize(text string, orig []byte) []byte {
	s := strings.ReplaceAll(text, "\r\n", "\n")
	if bytes.Contains(orig, []byte("\r\n")) {
		s = strings.ReplaceAll(s, "\n", "\r\n")
	}
	return []byte(s)
}

// readCommitForm reads the commit message, description and target.
func (a *App) readCommitForm(r *http.Request, f *editorForm) {
	f.Message = strings.TrimSpace(r.FormValue("message"))
	f.Description = strings.TrimSpace(strings.ReplaceAll(r.FormValue("description"), "\r\n", "\n"))
	if c := r.FormValue("commit_choice"); c == "branch" || c == "direct" {
		f.Choice = c
	}
	if nb := strings.TrimSpace(r.FormValue("new_branch")); nb != "" {
		f.NewBranch = nb
	}
	// The form's base, not the branch tip: that's what conflict safety compares.
	f.BaseCommit = strings.TrimSpace(r.FormValue("base_commit"))
}

// editorFinish commits and redirects, or shows the form again with the reason.
func (a *App) editorFinish(w http.ResponseWriter, r *http.Request, v *repoView, g *gitrepo.Repo, f *editorForm, ec editorCommit, showPath string) {
	res, err := a.editorCommit(r.Context(), a.actor(r), v, g, ec)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if res.Problem != "" || res.Stale != nil || len(res.Rejected) > 0 || res.ProtectedMsg != "" {
		f.Problem, f.Stale, f.Rejected = res.Problem, res.Stale, res.Rejected
		if res.ProtectedMsg != "" {
			f.Protection, f.Choice = res.ProtectedMsg, "branch"
		}
		status := http.StatusUnprocessableEntity
		if res.Stale != nil {
			status = http.StatusConflict
		}
		a.renderEditor(w, r, f, status)
		return
	}
	if res.PullNumber != 0 {
		http.Redirect(w, r, fmt.Sprintf("%s/pulls/%d", v.Base, res.PullNumber), http.StatusSeeOther)
		return
	}
	switch {
	case strings.Trim(showPath, "/") == "":
		http.Redirect(w, r, v.Base+"/tree/"+res.Branch, http.StatusSeeOther)
	case strings.HasSuffix(showPath, "/"):
		http.Redirect(w, r, v.Base+"/tree/"+res.Branch+"/"+strings.TrimSuffix(showPath, "/"), http.StatusSeeOther)
	default:
		http.Redirect(w, r, v.Base+"/blob/"+res.Branch+"/"+showPath, http.StatusSeeOther)
	}
}

// ---------- new file ----------

func (a *App) newFilePage(w http.ResponseWriter, r *http.Request) {
	v, g, branch, dir, ok := a.editLoad(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	f := a.newEditorForm(ctx, v, g, a.actor(r), "new", branch)
	if !a.editFolderOK(ctx, g, f.BaseCommit, dir) {
		a.notFound(w, r)
		return
	}
	a.fillNewForm(f, v, branch, dir)
	f.Path = joinRepoPath(dir, "")
	a.renderEditor(w, r, f, http.StatusOK)
}

// editFolderOK: the folder exists, or is being created by a first commit.
func (a *App) editFolderOK(ctx context.Context, g *gitrepo.Repo, commit, dir string) bool {
	if dir == "" || commit == "" {
		return true
	}
	return g.ObjectType(ctx, commit, dir) == "tree"
}

func (a *App) fillNewForm(f *editorForm, v *repoView, branch, dir string) {
	f.Dir, f.Editable = dir, true
	f.Name = "new file"
	f.DefaultMessage = "Create new file"
	f.Action = v.Base + "/new?at=" + url.QueryEscape(joinRepoPath(branch, dir))
	f.CancelURL = strings.TrimSuffix(v.Base+"/tree/"+branch+"/"+dir, "/")
	if v.Empty {
		f.CancelURL = v.Base
	}
}

func (a *App) newFileSubmit(w http.ResponseWriter, r *http.Request) {
	v, g, branch, dir, ok := a.editLoad(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, 3*editMaxText)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the file is too large for the web editor", http.StatusRequestEntityTooLarge)
		return
	}
	f := a.newEditorForm(ctx, v, g, a.actor(r), "new", branch)
	a.fillNewForm(f, v, branch, dir)
	a.readCommitForm(r, f)
	f.Path = strings.Trim(strings.TrimSpace(r.FormValue("path")), "/")
	f.Content = r.FormValue("content")
	f.Name = path.Base(f.Path)
	f.Markdown = isMarkdown(f.Name)
	if v.Empty {
		f.BaseCommit = ""
	} else if f.BaseCommit == "" {
		f.Problem = "Atelier lost track of which version you started from. Reload the page and try again."
		a.renderEditor(w, r, f, http.StatusUnprocessableEntity)
		return
	}
	if !gitrepo.ValidEditPath(f.Path) {
		f.Problem = "Name the file, e.g. notes.md or docs/notes.md (folders are created for you). No leading slash, no “.” or “..” folders, nothing inside .git."
		a.renderEditor(w, r, f, http.StatusUnprocessableEntity)
		return
	}
	if len(f.Content) > editMaxText {
		f.Problem = "That's too much text for the web editor (1 MB at most). Upload the file instead."
		a.renderEditor(w, r, f, http.StatusRequestEntityTooLarge)
		return
	}
	def := "Create " + f.Name
	a.editorFinish(w, r, v, g, f, editorCommit{Branch: branch, Base: f.BaseCommit,
		Changes: []gitrepo.FileChange{{Path: f.Path, Content: editNormalize(f.Content, nil), MustNotExist: true}},
		Touched: []string{f.Path}, Message: f.Message, Description: f.Description, DefaultMessage: def,
		Choice: f.Choice, NewBranch: f.NewBranch, CreateBranch: v.Empty}, f.Path)
}

// ---------- upload ----------

func (a *App) uploadPage(w http.ResponseWriter, r *http.Request) {
	v, g, branch, dir, ok := a.editLoad(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	f := a.newEditorForm(ctx, v, g, a.actor(r), "upload", branch)
	if !a.editFolderOK(ctx, g, f.BaseCommit, dir) {
		a.notFound(w, r)
		return
	}
	a.fillUploadForm(f, v, branch, dir)
	a.renderEditor(w, r, f, http.StatusOK)
}

func (a *App) fillUploadForm(f *editorForm, v *repoView, branch, dir string) {
	f.Dir = dir
	f.DefaultMessage = "Add files via upload"
	f.Action = v.Base + "/upload?at=" + url.QueryEscape(joinRepoPath(branch, dir))
	f.CancelURL = strings.TrimSuffix(v.Base+"/tree/"+branch+"/"+dir, "/")
	if v.Empty {
		f.CancelURL = v.Base
	}
}

func (a *App) uploadSubmit(w http.ResponseWriter, r *http.Request) {
	v, g, branch, dir, ok := a.editLoad(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	f := a.newEditorForm(ctx, v, g, a.actor(r), "upload", branch)
	a.fillUploadForm(f, v, branch, dir)
	r.Body = http.MaxBytesReader(w, r.Body, editMaxUploadTotal+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			f.Problem = fmt.Sprintf("That's more than %d MB in one upload. Upload fewer files at a time.", editMaxUploadTotal>>20)
			a.renderEditor(w, r, f, http.StatusRequestEntityTooLarge)
			return
		}
		f.Problem = "Choose one or more files to upload."
		a.renderEditor(w, r, f, http.StatusUnprocessableEntity)
		return
	}
	defer r.MultipartForm.RemoveAll()
	a.readCommitForm(r, f)
	if v.Empty {
		f.BaseCommit = ""
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		f.Problem = "Choose one or more files to upload."
		a.renderEditor(w, r, f, http.StatusUnprocessableEntity)
		return
	}
	if len(files) > editMaxUploadFiles {
		f.Problem = fmt.Sprintf("Upload at most %d files at a time.", editMaxUploadFiles)
		a.renderEditor(w, r, f, http.StatusUnprocessableEntity)
		return
	}
	var changes []gitrepo.FileChange
	var touched []string
	seen := map[string]bool{}
	for _, fh := range files {
		name := path.Base(strings.ReplaceAll(fh.Filename, "\\", "/"))
		p := joinRepoPath(dir, name)
		if !gitrepo.ValidEditPath(p) || seen[p] {
			f.Problem = fmt.Sprintf("Can't upload %q: rename it (and upload each name once).", fh.Filename)
			a.renderEditor(w, r, f, http.StatusUnprocessableEntity)
			return
		}
		if fh.Size > editMaxUploadFile {
			f.Problem = fmt.Sprintf("%s is larger than %d MB, the most the web upload takes. Push it with git instead.", name, editMaxUploadFile>>20)
			a.renderEditor(w, r, f, http.StatusRequestEntityTooLarge)
			return
		}
		seen[p] = true
		rd, err := fh.Open()
		if err != nil {
			a.fail(w, r, err)
			return
		}
		data, err := io.ReadAll(io.LimitReader(rd, editMaxUploadFile+1))
		rd.Close()
		if err != nil {
			a.fail(w, r, err)
			return
		}
		changes = append(changes, gitrepo.FileChange{Path: p, Content: data})
		touched = append(touched, p)
	}
	def := "Add files via upload"
	if len(changes) == 1 {
		def = "Add " + path.Base(changes[0].Path)
	}
	a.editorFinish(w, r, v, g, f, editorCommit{Branch: branch, Base: f.BaseCommit, Changes: changes, Touched: touched,
		Message: f.Message, Description: f.Description, DefaultMessage: def, Choice: f.Choice, NewBranch: f.NewBranch,
		CreateBranch: v.Empty}, dir+"/")
}

// ---------- delete ----------

func (a *App) deleteFilePage(w http.ResponseWriter, r *http.Request) {
	v, g, branch, p, ok := a.editLoad(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	f := a.newEditorForm(ctx, v, g, a.actor(r), "delete", branch)
	if v.Empty || p == "" || g.ObjectType(ctx, f.BaseCommit, p) != "blob" {
		a.notFound(w, r)
		return
	}
	a.fillDeleteForm(f, v, branch, p)
	a.renderEditor(w, r, f, http.StatusOK)
}

func (a *App) fillDeleteForm(f *editorForm, v *repoView, branch, p string) {
	f.OrigPath, f.Path, f.Name = p, p, path.Base(p)
	f.DefaultMessage = "Delete " + f.Name
	f.Action = v.Base + "/delete-file?at=" + url.QueryEscape(branch+"/"+p)
	f.CancelURL = v.Base + "/blob/" + branch + "/" + p
}

func (a *App) deleteFileSubmit(w http.ResponseWriter, r *http.Request) {
	v, g, branch, p, ok := a.editLoad(w, r)
	if !ok {
		return
	}
	if v.Empty || p == "" {
		a.notFound(w, r)
		return
	}
	ctx := r.Context()
	f := a.newEditorForm(ctx, v, g, a.actor(r), "delete", branch)
	a.fillDeleteForm(f, v, branch, p)
	a.readCommitForm(r, f)
	if f.BaseCommit == "" {
		f.Problem = "Atelier lost track of which version you started from. Reload the page and try again."
		a.renderEditor(w, r, f, http.StatusUnprocessableEntity)
		return
	}
	dir := path.Dir(p)
	if dir == "." {
		dir = ""
	}
	a.editorFinish(w, r, v, g, f, editorCommit{Branch: branch, Base: f.BaseCommit,
		Changes: []gitrepo.FileChange{{Path: p, Delete: true}}, Touched: []string{p},
		Message: f.Message, Description: f.Description, DefaultMessage: f.DefaultMessage,
		Choice: f.Choice, NewBranch: f.NewBranch}, dir+"/")
}

// ---------- Markdown preview ----------

func (a *App) editPreview(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "code") // POST: needs write
	if !ok {
		return
	}
	_ = v
	r.Body = http.MaxBytesReader(w, r.Body, 2*editMaxText)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, string(renderMarkdown([]byte(r.FormValue("text")))))
}
