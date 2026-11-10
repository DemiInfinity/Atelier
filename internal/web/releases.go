package web

// Releases: tag-based releases with Markdown notes and downloadable assets.
//
//	GET  /{owner}/{repo}/releases                     list (drafts too, for writers)
//	GET  /{owner}/{repo}/releases/latest              redirect to the latest release
//	GET  /{owner}/{repo}/releases/tag/{tag...}        one release
//	GET  /{owner}/{repo}/releases/new                 form (write)
//	POST /{owner}/{repo}/releases/new                 create; multipart with assets
//	GET  /{owner}/{repo}/releases/edit/{id}           form (write)
//	POST /{owner}/{repo}/releases/edit/{id}           save; multipart with assets
//	POST /{owner}/{repo}/releases/delete/{id}         delete (write)
//	POST /{owner}/{repo}/releases/assets/{id}/delete  delete one asset (write)
//	POST /{owner}/{repo}/releases/notes               generate notes (JSON, write)
//	GET  /{owner}/{repo}/releases/download/{tag}/{name}  download an asset (read; anonymous for public repos)
//	GET  /{owner}/{repo}/archive/{ref}.{zip|tar.gz}   source archive (read)
//
// The REST API for automation lives in releases_api.go. Publishing a release
// creates its tag (if missing) and emits "release.published".

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// Publishing a release (creating a non-draft, or publishing a draft) emits
// EventReleasePublished ("release.published", declared with the Actions
// engine, which runs `on: release` workflows from it). Number is the release
// id, Detail the raw tag and URL the release page.

// releaseApp lets repoView methods (used by shared templates such as the repo
// header) reach the database. There is one App per process; tests create
// them one at a time.
var releaseApp atomic.Pointer[App]

func init() {
	registerFeature(feature{
		name: "releases",
		routes: func(a *App, m *http.ServeMux) {
			releaseApp.Store(a)
			m.HandleFunc("GET /{owner}/{repo}/releases", a.authed(a.releasesList))
			m.HandleFunc("GET /{owner}/{repo}/releases/latest", a.authed(a.releasesLatest))
			m.HandleFunc("GET /{owner}/{repo}/releases/tag/{tag...}", a.authed(a.releasesView))
			m.HandleFunc("GET /{owner}/{repo}/releases/new", a.authed(a.releasesNew))
			m.HandleFunc("POST /{owner}/{repo}/releases/new", a.authed(a.releasesCreate))
			m.HandleFunc("GET /{owner}/{repo}/releases/edit/{id}", a.authed(a.releasesEdit))
			m.HandleFunc("POST /{owner}/{repo}/releases/edit/{id}", a.authed(a.releasesSave))
			m.HandleFunc("POST /{owner}/{repo}/releases/delete/{id}", a.authed(a.releasesDelete))
			m.HandleFunc("POST /{owner}/{repo}/releases/assets/{id}/delete", a.authed(a.releasesAssetDelete))
			m.HandleFunc("POST /{owner}/{repo}/releases/notes", a.authed(a.releasesNotes))
			// Downloads work for anonymous visitors of public repos and for
			// scripts with a token, so they sign people in themselves.
			m.HandleFunc("GET /{owner}/{repo}/releases/download/{tag}/{name}", a.releasesDownload)
			m.HandleFunc("GET /{owner}/{repo}/archive/{file...}", a.releasesArchive)
		},
	})
}

// ---------- shared logic (UI and API) ----------

// relError is a problem the person can fix; Code is the HTTP status for the API.
type relError struct {
	Code int
	Msg  string
}

func (e *relError) Error() string { return e.Msg }

func relErr(code int, format string, args ...any) error {
	return &relError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

type releaseInput struct {
	Tag, Target, Title, Body string
	Draft, Prerelease        bool
	BuiltBy                  string
}

// builtByFor says what built a release: automation actors (Actions job
// tokens, bots) are recorded by name; people and their tokens are not.
func builtByFor(who Actor, explicit string) string {
	if s := strings.TrimSpace(explicit); s != "" {
		return truncate(s, 120)
	}
	switch who.Kind {
	case ActorOwner, ActorToken, ActorAnonymous, "":
		return ""
	}
	return truncate(who.Name, 120)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// ensureTag makes sure the release's tag exists, creating a lightweight tag
// at its target if needed, and returns the commit it points at.
func (a *App) ensureTag(ctx context.Context, g *gitrepo.Repo, tag, target, def string) (string, error) {
	if sha, ok := g.TagCommit(ctx, tag); ok {
		return sha, nil
	}
	if target == "" {
		target = def
	}
	if g.IsEmpty(ctx) {
		return "", relErr(http.StatusUnprocessableEntity, "Push some commits before publishing a release.")
	}
	sha, err := g.ResolveCommit(ctx, target)
	if err != nil {
		return "", relErr(http.StatusUnprocessableEntity, "The target %q isn't a branch or commit in this repository.", target)
	}
	if err := g.CreateTag(ctx, tag, sha); err != nil && !errors.Is(err, gitrepo.ErrTagExists) {
		return "", err
	}
	if got, ok := g.TagCommit(ctx, tag); ok {
		return got, nil
	}
	return sha, nil
}

func (a *App) validateReleaseInput(ctx context.Context, in *releaseInput) error {
	in.Tag = strings.TrimSpace(in.Tag)
	in.Target = strings.TrimSpace(in.Target)
	in.Title = strings.TrimSpace(in.Title)
	if in.Tag == "" {
		return relErr(http.StatusUnprocessableEntity, "Choose a tag for the release, like v1.0.0.")
	}
	if !a.git.ValidTagName(ctx, in.Tag) {
		return relErr(http.StatusUnprocessableEntity, "%q isn't a valid tag name. Use letters, numbers, dots, dashes and slashes.", in.Tag)
	}
	if len(in.Title) > 200 {
		return relErr(http.StatusUnprocessableEntity, "Keep the title under 200 characters.")
	}
	if len(in.Body) > 1<<20 {
		return relErr(http.StatusUnprocessableEntity, "The release notes are too long.")
	}
	return nil
}

// createRelease validates and stores a new release. Non-drafts get their tag
// created and are announced.
func (a *App) createRelease(ctx context.Context, repo store.Repo, g *gitrepo.Repo, who Actor, in releaseInput) (store.Release, error) {
	if err := a.validateReleaseInput(ctx, &in); err != nil {
		return store.Release{}, err
	}
	if _, err := a.db.ReleaseByTag(ctx, repo.ID, in.Tag); err == nil {
		return store.Release{}, relErr(http.StatusUnprocessableEntity, "There's already a release for %s.", in.Tag)
	}
	def := g.DefaultBranch(ctx)
	if in.Target == "" {
		in.Target = def
	}
	sha := ""
	if !in.Draft {
		var err error
		if sha, err = a.ensureTag(ctx, g, in.Tag, in.Target, def); err != nil {
			return store.Release{}, err
		}
	} else if s, ok := g.TagCommit(ctx, in.Tag); ok {
		sha = s
	}
	rel, err := a.db.CreateRelease(ctx, store.NewRelease{
		RepoID: repo.ID, Tag: in.Tag, Target: in.Target, CommitSHA: sha, Title: in.Title, Body: in.Body,
		Draft: in.Draft, Prerelease: in.Prerelease, AuthorKind: who.Kind, AuthorName: who.Name, AuthorPerson: who.PersonID,
		BuiltBy: builtByFor(who, in.BuiltBy),
	})
	if errors.Is(err, store.ErrReleaseExists) {
		return store.Release{}, relErr(http.StatusUnprocessableEntity, "There's already a release for %s.", in.Tag)
	}
	if err != nil {
		return store.Release{}, err
	}
	if !rel.Draft {
		a.announceRelease(ctx, repo, rel, who)
	}
	return rel, nil
}

// updateRelease applies edits; publishing a draft creates its tag and
// announces it.
func (a *App) updateRelease(ctx context.Context, repo store.Repo, g *gitrepo.Repo, who Actor, rel store.Release, in releaseInput) (store.Release, error) {
	if err := a.validateReleaseInput(ctx, &in); err != nil {
		return rel, err
	}
	if in.Tag != rel.Tag {
		if other, err := a.db.ReleaseByTag(ctx, repo.ID, in.Tag); err == nil && other.ID != rel.ID {
			return rel, relErr(http.StatusUnprocessableEntity, "There's already a release for %s.", in.Tag)
		}
	}
	wasDraft := rel.Draft
	def := g.DefaultBranch(ctx)
	if in.Target == "" {
		in.Target = rel.Target
	}
	if in.Target == "" {
		in.Target = def
	}
	rel.Tag, rel.Target, rel.Title, rel.Body = in.Tag, in.Target, in.Title, in.Body
	rel.Draft, rel.Prerelease = in.Draft, in.Prerelease
	if b := builtByFor(who, in.BuiltBy); b != "" {
		rel.BuiltBy = b
	}
	if !rel.Draft {
		sha, err := a.ensureTag(ctx, g, rel.Tag, rel.Target, def)
		if err != nil {
			return rel, err
		}
		rel.CommitSHA = sha
	} else if s, ok := g.TagCommit(ctx, rel.Tag); ok {
		rel.CommitSHA = s
	} else {
		rel.CommitSHA = ""
	}
	if err := a.db.UpdateRelease(ctx, rel); err != nil {
		if errors.Is(err, store.ErrReleaseExists) {
			return rel, relErr(http.StatusUnprocessableEntity, "There's already a release for %s.", in.Tag)
		}
		return rel, err
	}
	rel, err := a.db.ReleaseByID(ctx, repo.ID, rel.ID)
	if err != nil {
		return rel, err
	}
	if wasDraft && !rel.Draft {
		a.announceRelease(ctx, repo, rel, who)
	}
	return rel, nil
}

func (a *App) announceRelease(ctx context.Context, repo store.Repo, rel store.Release, who Actor) {
	a.emit(ctx, Event{Kind: EventReleasePublished, Repo: repo, RepoID: repo.ID, Number: rel.ID, Title: rel.Name(),
		URL: releaseTagURL(repo, rel.Tag), Actor: who, Detail: rel.Tag})
}

// deleteRelease removes a release and any asset files nothing else uses.
// The tag stays, like on GitHub.
func (a *App) deleteRelease(ctx context.Context, repo store.Repo, rel store.Release) error {
	shas, err := a.db.DeleteRelease(ctx, repo.ID, rel.ID)
	if err != nil {
		return err
	}
	a.dropUnusedReleaseBlobs(ctx, shas...)
	return nil
}

func (a *App) dropUnusedReleaseBlobs(ctx context.Context, shas ...string) {
	for _, sha := range shas {
		if used, err := a.db.ReleaseBlobInUse(ctx, sha); err == nil && !used {
			_ = a.releaseBlobs().remove(sha)
		}
	}
}

// validAssetName: a file name, not a path.
func validAssetName(name string) bool {
	if name == "" || len(name) > 255 || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return false
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// cleanAssetName turns an uploaded file name into an asset name.
func cleanAssetName(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	return strings.TrimSpace(path.Base("/" + name))
}

// addReleaseAsset streams body into the asset store and records it.
// declared is the Content-Length if known (-1 otherwise).
func (a *App) addReleaseAsset(ctx context.Context, rel store.Release, who Actor, name, ctype string, body io.Reader, declared int64) (store.ReleaseAsset, error) {
	if !validAssetName(name) {
		return store.ReleaseAsset{}, relErr(http.StatusUnprocessableEntity, "%q isn't a usable file name.", name)
	}
	if _, err := a.db.ReleaseAssetByName(ctx, rel.ID, name); err == nil {
		return store.ReleaseAsset{}, relErr(http.StatusUnprocessableEntity, "This release already has a file called %s.", name)
	}
	limit := releaseAssetLimit()
	if declared > limit {
		return store.ReleaseAsset{}, relErr(http.StatusRequestEntityTooLarge, "%s is larger than the %s limit for release files.", name, limitText(limit))
	}
	sha, size, err := a.releaseBlobs().put(body, limit, "", -1)
	if errors.Is(err, errBlobTooLarge) {
		return store.ReleaseAsset{}, relErr(http.StatusRequestEntityTooLarge, "%s is larger than the %s limit for release files.", name, limitText(limit))
	}
	if err != nil {
		return store.ReleaseAsset{}, err
	}
	ctype = cleanContentType(ctype, name)
	as, err := a.db.AddReleaseAsset(ctx, store.ReleaseAsset{
		ReleaseID: rel.ID, Name: name, Size: size, ContentType: ctype, SHA256: sha,
		UploaderKind: who.Kind, UploaderName: who.Name, UploaderPerson: who.PersonID,
	})
	if err != nil {
		a.dropUnusedReleaseBlobs(ctx, sha)
		if errors.Is(err, store.ErrAssetExists) {
			return store.ReleaseAsset{}, relErr(http.StatusUnprocessableEntity, "This release already has a file called %s.", name)
		}
		return store.ReleaseAsset{}, err
	}
	return as, nil
}

func cleanContentType(ctype, name string) string {
	if mt, _, err := mime.ParseMediaType(ctype); err == nil && mt != "" && mt != "application/x-www-form-urlencoded" && !strings.HasPrefix(mt, "multipart/") {
		return mt
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		if mt, _, err := mime.ParseMediaType(t); err == nil {
			return mt
		}
	}
	return "application/octet-stream"
}

// ---------- release notes ----------

// generateReleaseNotes drafts notes from the pull requests merged and the
// commits made since the previous release (no AI involved).
func (a *App) generateReleaseNotes(ctx context.Context, repo store.Repo, g *gitrepo.Repo, tag, target, previous string) (string, string, error) {
	if g.IsEmpty(ctx) {
		return "", "", relErr(http.StatusUnprocessableEntity, "This repository has no commits yet.")
	}
	to, ok := g.TagCommit(ctx, tag)
	if !ok {
		if target == "" {
			target = g.DefaultBranch(ctx)
		}
		sha, err := g.ResolveCommit(ctx, target)
		if err != nil {
			return "", "", relErr(http.StatusUnprocessableEntity, "The target %q isn't a branch or commit in this repository.", target)
		}
		to = sha
	}
	from := ""
	if previous != "" {
		sha, ok := g.TagCommit(ctx, previous)
		if !ok {
			return "", "", relErr(http.StatusUnprocessableEntity, "There's no tag called %q.", previous)
		}
		from = sha
	} else {
		rels, err := a.db.Releases(ctx, repo.ID, false)
		if err != nil {
			return "", "", err
		}
		for _, r := range rels {
			if r.Tag == tag {
				continue
			}
			if sha, ok := g.TagCommit(ctx, r.Tag); ok {
				from, previous = sha, r.Tag
				break
			}
		}
	}
	inRange, err := g.RangeSHAs(ctx, from, to)
	if err != nil {
		return "", "", err
	}
	pulls, err := a.db.Pulls(ctx, repo.ID, "merged")
	if err != nil {
		return "", "", err
	}
	base := "/" + repo.FullName()
	var b strings.Builder
	prSHAs := map[string]bool{}
	var prLines []string
	for i := len(pulls) - 1; i >= 0; i-- { // oldest first
		p := pulls[i]
		if p.MergedSHA == "" || !inRange[p.MergedSHA] {
			continue
		}
		prSHAs[p.MergedSHA] = true
		line := fmt.Sprintf("- %s ([#%d](%s/pulls/%d))", mdEscapeLine(p.Title), p.Number, base, p.Number)
		if p.AuthorName != "" {
			line += " by " + mdEscapeLine(p.AuthorName)
		}
		prLines = append(prLines, line)
	}
	commits, err := g.RangeCommits(ctx, from, to, 200)
	if err != nil {
		return "", "", err
	}
	var commitLines []string
	for i := len(commits) - 1; i >= 0; i-- {
		c := commits[i]
		if prSHAs[c.Hash] {
			continue
		}
		commitLines = append(commitLines, fmt.Sprintf("- %s ([%s](%s/commit/%s))", mdEscapeLine(c.Subject), c.Short, base, c.Hash))
	}
	if len(prLines) > 0 {
		b.WriteString("## What's changed\n\n" + strings.Join(prLines, "\n") + "\n")
	}
	if len(commitLines) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		heading := "## Commits"
		if len(prLines) > 0 {
			heading = "## Other commits"
		}
		b.WriteString(heading + "\n\n" + strings.Join(commitLines, "\n") + "\n")
	}
	if b.Len() == 0 {
		if previous != "" {
			b.WriteString("No changes since " + previous + ".\n")
		} else {
			b.WriteString("No changes yet.\n")
		}
	}
	if previous != "" {
		fmt.Fprintf(&b, "\n**Full changelog:** [%s…%s](%s/commits/%s)\n", previous, tag, base, url.PathEscape(to))
	}
	return b.String(), previous, nil
}

// mdEscapeLine keeps a title on one line and stops it from starting Markdown
// structure or links of its own.
func mdEscapeLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := strings.NewReplacer(`[`, `\[`, `]`, `\]`, `<`, `&lt;`, `>`, `&gt;`)
	return r.Replace(s)
}

// ---------- view models ----------

type releaseView struct {
	store.Release
	Assets          []assetView
	URL, EditURL    string
	IsLatest        bool
	TagExists       bool
	CommitURL       string
	Short           string
	TarURL, ZipURL  string
	PortainerImage  string
	AssetTotalBytes int64
	CanEdit         bool
}

// BodyHTML renders the notes as Markdown.
func (r releaseView) BodyHTML() template.HTML { return renderMarkdown([]byte(r.Body)) }

// Excerpt is the first line of the notes, for compact rows.
func (r releaseView) Excerpt() string {
	for _, l := range strings.Split(r.Body, "\n") {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#-*> "))
		if l != "" {
			return eventExcerpt(l, 90)
		}
	}
	return ""
}

type assetView struct {
	store.ReleaseAsset
	URL   string
	Kind  string // "binary", "archive", "image", "text"
	Label string // short description of the type
}

func pathEscapeTag(tag string) string {
	parts := strings.Split(tag, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func releaseTagURL(repo store.Repo, tag string) string {
	return "/" + repo.FullName() + "/releases/tag/" + pathEscapeTag(tag)
}

func releaseDownloadURL(repo store.Repo, tag, name string) string {
	return "/" + repo.FullName() + "/releases/download/" + url.PathEscape(tag) + "/" + url.PathEscape(name)
}

func assetKind(a store.ReleaseAsset) (kind, label string) {
	n := strings.ToLower(a.Name)
	switch {
	case strings.HasSuffix(n, ".tar.gz"), strings.HasSuffix(n, ".tgz"), strings.HasSuffix(n, ".zip"),
		strings.HasSuffix(n, ".tar"), strings.HasSuffix(n, ".xz"), strings.HasSuffix(n, ".zst"):
		return "archive", "Archive"
	case strings.HasPrefix(a.ContentType, "image/"):
		return "image", "Image"
	case strings.HasPrefix(a.ContentType, "text/"), strings.HasSuffix(n, ".txt"), strings.HasSuffix(n, ".sha256"),
		strings.HasSuffix(n, ".json"), strings.HasSuffix(n, ".md"):
		return "text", "Text"
	case strings.Contains(n, "darwin"), strings.Contains(n, "macos"):
		return "binary", "macOS"
	case strings.Contains(n, "linux"):
		return "binary", "Linux"
	case strings.Contains(n, "windows"), strings.HasSuffix(n, ".exe"):
		return "binary", "Windows"
	}
	return "binary", "File"
}

// releaseViewFor builds the view of one release, assets included.
func (a *App) releaseViewFor(ctx context.Context, r *http.Request, repo store.Repo, g *gitrepo.Repo, rel store.Release, latestID int64) (releaseView, error) {
	rv := releaseView{Release: rel, URL: releaseTagURL(repo, rel.Tag), IsLatest: rel.ID == latestID && !rel.Draft}
	rv.EditURL = fmt.Sprintf("/%s/releases/edit/%d", repo.FullName(), rel.ID)
	sha := rel.CommitSHA
	if s, ok := g.TagCommit(ctx, rel.Tag); ok {
		rv.TagExists, sha = true, s
	}
	if sha != "" {
		rv.CommitURL = "/" + repo.FullName() + "/commit/" + sha
		rv.Short = sha
		if len(sha) > 7 {
			rv.Short = sha[:7]
		}
	}
	if rv.TagExists {
		file := "/" + repo.FullName() + "/archive/" + pathEscapeTag(rel.Tag)
		rv.TarURL, rv.ZipURL = file+".tar.gz", file+".zip"
		if b, err := g.Blob(ctx, sha, "Dockerfile", 1); err == nil && b != nil {
			_, host := a.external(r)
			if a.cfg.PublicURL != "" {
				if u, err := url.Parse(a.cfg.PublicURL); err == nil {
					host = u.Host
				}
			}
			rv.PortainerImage = strings.ToLower(host + "/" + repo.Owner + "/" + repo.Name + ":" + strings.TrimPrefix(rel.Tag, "v"))
		}
	}
	assets, err := a.db.ReleaseAssets(ctx, rel.ID)
	if err != nil {
		return rv, err
	}
	for _, as := range assets {
		k, l := assetKind(as)
		rv.Assets = append(rv.Assets, assetView{ReleaseAsset: as, URL: releaseDownloadURL(repo, rel.Tag, as.Name), Kind: k, Label: l})
		rv.AssetTotalBytes += as.Size
	}
	return rv, nil
}

func (a *App) latestReleaseID(ctx context.Context, repoID int64) int64 {
	if l, err := a.db.LatestRelease(ctx, repoID); err == nil {
		return l.ID
	}
	return 0
}

// ---------- repoView helpers for shared templates ----------

// ReleaseCount is the number of published releases (repo header tab).
func (v *repoView) ReleaseCount() int {
	a := releaseApp.Load()
	if a == nil || v == nil {
		return 0
	}
	n, _ := a.db.ReleaseCount(context.Background(), v.Repo.ID)
	return n
}

// LatestRelease is the latest published release, or nil (repo code sidebar).
func (v *repoView) LatestRelease() *releaseSummary {
	a := releaseApp.Load()
	if a == nil || v == nil {
		return nil
	}
	rel, err := a.db.LatestRelease(context.Background(), v.Repo.ID)
	if err != nil {
		return nil
	}
	n, _ := a.db.ReleaseCount(context.Background(), v.Repo.ID)
	return &releaseSummary{Tag: rel.Tag, Name: rel.Name(), When: rel.When(), URL: releaseTagURL(v.Repo, rel.Tag),
		ListURL: v.Base + "/releases", Count: n}
}

type releaseSummary struct {
	Tag, Name, URL, ListURL string
	When                    time.Time
	Count                   int
}

// ---------- pages ----------

func (a *App) canWrite(r *http.Request, repo store.Repo) bool {
	return a.can(r.Context(), a.actor(r), repo, PermWrite)
}

func (a *App) releasesList(w http.ResponseWriter, r *http.Request) {
	v, g, ok := a.loadRepo(w, r, "releases")
	if !ok {
		return
	}
	ctx := r.Context()
	canWrite := a.canWrite(r, v.Repo)
	rels, err := a.db.Releases(ctx, v.Repo.ID, canWrite)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	latest := a.latestReleaseID(ctx, v.Repo.ID)
	var featured *releaseView
	var earlier, drafts []releaseView
	for _, rel := range rels {
		rv, err := a.releaseViewFor(ctx, r, v.Repo, g, rel, latest)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		rv.CanEdit = canWrite
		switch {
		case rel.Draft:
			drafts = append(drafts, rv)
		case featured == nil && (latest == 0 || rel.ID == latest):
			featured = &rv
		default:
			earlier = append(earlier, rv)
		}
	}
	// Without a "latest" (only prereleases), the newest one is featured.
	if featured == nil && len(earlier) > 0 {
		featured, earlier = &earlier[0], earlier[1:]
	}
	a.render(w, r, "releases_list", page{Title: "Releases · " + v.Repo.FullName(), Nav: "repos", Data: map[string]any{
		"V": v, "Featured": featured, "Earlier": earlier, "Drafts": drafts, "CanWrite": canWrite,
		"Tags": len(v.Tags), "API": a.releasesAPIHelp(r, v.Repo),
	}})
}

func (a *App) releasesLatest(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "releases")
	if !ok {
		return
	}
	rel, err := a.db.LatestRelease(r.Context(), v.Repo.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, releaseTagURL(v.Repo, rel.Tag), http.StatusSeeOther)
}

func (a *App) releasesView(w http.ResponseWriter, r *http.Request) {
	v, g, ok := a.loadRepo(w, r, "releases")
	if !ok {
		return
	}
	ctx := r.Context()
	tag := r.PathValue("tag")
	canWrite := a.canWrite(r, v.Repo)
	rel, err := a.db.ReleaseByTag(ctx, v.Repo.ID, tag)
	if err == nil && rel.Draft && !canWrite {
		err = store.ErrNotFound
	}
	if errors.Is(err, store.ErrNotFound) {
		// A tag without a release: offer to make one.
		if sha, ok := g.TagCommit(ctx, tag); ok {
			a.render(w, r, "releases_view", page{Title: tag + " · " + v.Repo.FullName(), Nav: "repos", Data: map[string]any{
				"V": v, "Tag": tag, "TagOnly": true, "Commit": sha, "CanWrite": canWrite,
				"NewURL": v.Base + "/releases/new?tag=" + url.QueryEscape(tag),
			}})
			return
		}
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	rv, err := a.releaseViewFor(ctx, r, v.Repo, g, rel, a.latestReleaseID(ctx, v.Repo.ID))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	rv.CanEdit = canWrite
	a.render(w, r, "releases_view", page{Title: rel.Name() + " · " + v.Repo.FullName(), Nav: "repos", Notice: r.URL.Query().Get("saved"), Data: map[string]any{
		"V": v, "R": rv, "Tag": rel.Tag, "CanWrite": canWrite,
	}})
}

// ---------- new / edit ----------

type releaseForm struct {
	V          *repoView
	Edit       bool
	ID         int64
	Draft      bool // the stored release is a draft
	In         releaseInput
	Assets     []assetView
	Limit      string
	ListURL    string
	Action     string
	Published  bool
	TagMissing bool
}

func (a *App) renderReleaseForm(w http.ResponseWriter, r *http.Request, f releaseForm, errMsg string, status int) {
	f.Limit = limitText(releaseAssetLimit())
	f.ListURL = f.V.Base + "/releases"
	f.Action = f.V.Base + "/releases/new"
	if f.Edit {
		f.Action = fmt.Sprintf("%s/releases/edit/%d", f.V.Base, f.ID)
	}
	title := "New release"
	if f.Edit {
		title = "Edit " + f.In.Tag
	}
	if status != 0 {
		w.WriteHeader(status)
	}
	a.render(w, r, "releases_form", page{Title: title + " · " + f.V.Repo.FullName(), Nav: "repos", Error: errMsg, Data: f})
}

func (a *App) loadReleaseWrite(w http.ResponseWriter, r *http.Request) (*repoView, *gitrepo.Repo, bool) {
	v, g, ok := a.loadRepo(w, r, "releases")
	if !ok {
		return nil, nil, false
	}
	if !a.canWrite(r, v.Repo) {
		a.forbidden(w, r)
		return nil, nil, false
	}
	return v, g, true
}

func (a *App) releasesNew(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadReleaseWrite(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	in := releaseInput{Tag: q.Get("tag"), Target: v.Default}
	a.renderReleaseForm(w, r, releaseForm{V: v, In: in}, "", 0)
}

func (a *App) releasesEdit(w http.ResponseWriter, r *http.Request) {
	v, g, ok := a.loadReleaseWrite(w, r)
	if !ok {
		return
	}
	rel, ok := a.releaseFromPath(w, r, v)
	if !ok {
		return
	}
	rv, err := a.releaseViewFor(r.Context(), r, v.Repo, g, rel, 0)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	f := releaseForm{V: v, Edit: true, ID: rel.ID, Draft: rel.Draft, Assets: rv.Assets, Published: !rel.Draft,
		In: releaseInput{Tag: rel.Tag, Target: rel.Target, Title: rel.Title, Body: rel.Body, Draft: rel.Draft, Prerelease: rel.Prerelease}}
	msg := ""
	if e := r.URL.Query().Get("error"); e != "" {
		msg = truncate(e, 300)
	}
	a.renderReleaseForm(w, r, f, msg, 0)
}

func (a *App) releaseFromPath(w http.ResponseWriter, r *http.Request, v *repoView) (store.Release, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return store.Release{}, false
	}
	rel, err := a.db.ReleaseByID(r.Context(), v.Repo.ID, id)
	if err != nil {
		a.fail(w, r, err)
		return store.Release{}, false
	}
	return rel, true
}

func (a *App) releasesCreate(w http.ResponseWriter, r *http.Request) { a.releasesSubmit(w, r, false) }
func (a *App) releasesSave(w http.ResponseWriter, r *http.Request)   { a.releasesSubmit(w, r, true) }

// releasesSubmit handles the new and edit forms. The form is read part by
// part so attached files stream straight to disk. Files can arrive before the
// submit button's value, so a new release is stored as a draft first and
// published at the end if that's what was asked.
func (a *App) releasesSubmit(w http.ResponseWriter, r *http.Request, edit bool) {
	v, g, ok := a.loadReleaseWrite(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	var rel store.Release
	if edit {
		if rel, ok = a.releaseFromPath(w, r, v); !ok {
			return
		}
	}
	vals := url.Values{}
	saved := edit // whether rel exists in the database
	var fileErrs []string
	form := func() releaseForm {
		in := formReleaseInput(vals)
		if !vals.Has("action") && edit {
			in.Draft = rel.Draft
		}
		f := releaseForm{V: v, Edit: saved, ID: rel.ID, Draft: rel.Draft, Published: saved && !rel.Draft, In: in}
		if saved {
			if rv, err := a.releaseViewFor(ctx, r, v.Repo, g, rel, 0); err == nil {
				f.Assets = rv.Assets
			}
		}
		return f
	}
	// ensure stores the release (as a draft if new) before the first file.
	ensure := func() error {
		if saved {
			return nil
		}
		in := formReleaseInput(vals)
		in.Draft = true
		created, err := a.createRelease(ctx, v.Repo, g, who, in)
		if err != nil {
			return err
		}
		rel, saved = created, true
		return nil
	}
	fail := func(err error) {
		var re *relError
		if errors.As(err, &re) {
			a.renderReleaseForm(w, r, form(), re.Msg, http.StatusUnprocessableEntity)
			return
		}
		a.fail(w, r, err)
	}

	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "multipart/form-data" {
		mr, err := r.MultipartReader()
		if err != nil {
			a.fail(w, r, relErr(http.StatusBadRequest, "That form couldn't be read."))
			return
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				fail(relErr(http.StatusBadRequest, "The upload was interrupted."))
				return
			}
			if part.FileName() == "" {
				b, err := io.ReadAll(io.LimitReader(part, 2<<20))
				part.Close()
				if err != nil {
					fail(relErr(http.StatusBadRequest, "The upload was interrupted."))
					return
				}
				vals.Add(part.FormName(), string(b))
				continue
			}
			if err := a.formAsset(ctx, part, ensure, &rel, who); err != nil {
				var re *relError
				if !errors.As(err, &re) || !saved {
					fail(err)
					return
				}
				fileErrs = append(fileErrs, re.Msg)
			}
		}
	} else if err := r.ParseForm(); err == nil {
		vals = r.PostForm
	}

	in := formReleaseInput(vals)
	action := vals.Get("action")
	switch {
	case !saved:
		created, err := a.createRelease(ctx, v.Repo, g, who, in)
		if err != nil {
			fail(err)
			return
		}
		rel = created
	default:
		if !vals.Has("action") {
			in.Draft = rel.Draft
		}
		if edit && action == "update" {
			in.Draft = rel.Draft
		}
		updated, err := a.updateRelease(ctx, v.Repo, g, who, rel, in)
		if err != nil {
			rel = updated
			fail(err)
			return
		}
		rel = updated
	}
	if len(fileErrs) > 0 {
		http.Redirect(w, r, fmt.Sprintf("%s/releases/edit/%d?error=%s", v.Base, rel.ID, url.QueryEscape(strings.Join(fileErrs, " "))), http.StatusSeeOther)
		return
	}
	if rel.Draft {
		http.Redirect(w, r, v.Base+"/releases", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, releaseTagURL(v.Repo, rel.Tag)+"?saved=1", http.StatusSeeOther)
}

// formAsset stores one uploaded file from the form.
func (a *App) formAsset(ctx context.Context, part *multipart.Part, ensure func() error, rel *store.Release, who Actor) error {
	defer part.Close()
	name := cleanAssetName(part.FileName())
	if name == "" || name == "." {
		_, _ = io.Copy(io.Discard, part) // an empty file input
		return nil
	}
	if err := ensure(); err != nil {
		return err
	}
	_, err := a.addReleaseAsset(ctx, *rel, who, name, part.Header.Get("Content-Type"), part, -1)
	if err != nil {
		_, _ = io.Copy(io.Discard, part)
	}
	return err
}

func formReleaseInput(vals url.Values) releaseInput {
	in := releaseInput{
		Tag: vals.Get("tag"), Target: vals.Get("target"), Title: vals.Get("title"),
		Body:       strings.ReplaceAll(vals.Get("body"), "\r\n", "\n"),
		Prerelease: vals.Get("prerelease") != "",
	}
	in.Draft = vals.Get("action") == "draft"
	return in
}

func (a *App) releasesDelete(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadReleaseWrite(w, r)
	if !ok {
		return
	}
	rel, ok := a.releaseFromPath(w, r, v)
	if !ok {
		return
	}
	if err := a.deleteRelease(r.Context(), v.Repo, rel); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, v.Base+"/releases", http.StatusSeeOther)
}

func (a *App) releasesAssetDelete(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadReleaseWrite(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	as, err := a.db.ReleaseAssetInRepo(ctx, v.Repo.ID, id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.db.DeleteReleaseAsset(ctx, as.ReleaseID, as.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	a.dropUnusedReleaseBlobs(ctx, as.SHA256)
	http.Redirect(w, r, fmt.Sprintf("%s/releases/edit/%d", v.Base, as.ReleaseID), http.StatusSeeOther)
}

// releasesNotes drafts notes for the form ("Generate notes").
func (a *App) releasesNotes(w http.ResponseWriter, r *http.Request) {
	v, g, ok := a.loadRepo(w, r, "releases")
	if !ok {
		return
	}
	if !a.canWrite(r, v.Repo) {
		writeAPIError(w, http.StatusForbidden, "You can't create releases in this repository.")
		return
	}
	body, prev, err := a.generateReleaseNotes(r.Context(), v.Repo, g, strings.TrimSpace(r.FormValue("tag")),
		strings.TrimSpace(r.FormValue("target")), strings.TrimSpace(r.FormValue("previous")))
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"body": body, "previous_tag": prev})
}

// ---------- downloads ----------

// requestActor works out who is calling a route that isn't wrapped in
// a.authed: a credential in the Authorization header (Bearer, "token" or Basic,
// resolved like git over HTTPS), else the signed-in person when allowSession,
// else anonymous. ok is false when a credential was given but isn't valid.
func (a *App) requestActor(r *http.Request, allowSession bool) (Actor, bool) {
	anon := Actor{Kind: ActorAnonymous, Name: "anonymous"}
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, cred, _ := strings.Cut(h, " ")
		var cands []string
		switch strings.ToLower(scheme) {
		case "bearer", "token":
			cands = []string{strings.TrimSpace(cred)}
		case "basic":
			if u, p, ok := r.BasicAuth(); ok {
				cands = []string{p, u}
			}
		}
		for _, c := range cands {
			if c == "" {
				continue
			}
			if who, ok := a.resolveCredential(r.Context(), c); ok {
				return who, true
			}
		}
		return anon, false
	}
	if allowSession {
		if who, ok := a.sessionActor(r); ok {
			return who, true
		}
	}
	return anon, true
}

// publicRepoAccess loads a repository for a download-style route and checks
// read access, answering 401/404 itself.
func (a *App) publicRepoAccess(w http.ResponseWriter, r *http.Request) (store.Repo, Actor, bool) {
	ctx := r.Context()
	who, valid := a.requestActor(r, true)
	if !valid {
		if !a.lim.allow(a.clientIP(r)) {
			http.Error(w, "too many attempts, try again in a minute", http.StatusTooManyRequests)
			return store.Repo{}, who, false
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="Atelier", charset="UTF-8"`)
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return store.Repo{}, who, false
	}
	repo, err := a.db.RepoByName(ctx, r.PathValue("owner"), r.PathValue("repo"))
	if err != nil || !a.can(ctx, who, repo, PermRead) {
		if err == nil && who.Kind == ActorAnonymous {
			w.Header().Set("WWW-Authenticate", `Basic realm="Atelier", charset="UTF-8"`)
			http.Error(w, "authentication required: use an Atelier access token", http.StatusUnauthorized)
			return repo, who, false
		}
		http.Error(w, "not found", http.StatusNotFound)
		return repo, who, false
	}
	return repo, who, true
}

func (a *App) releasesDownload(w http.ResponseWriter, r *http.Request) {
	repo, who, ok := a.publicRepoAccess(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	rel, err := a.db.ReleaseByTag(ctx, repo.ID, r.PathValue("tag"))
	if err != nil || (rel.Draft && !a.can(ctx, who, repo, PermWrite)) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	as, err := a.db.ReleaseAssetByName(ctx, rel.ID, r.PathValue("name"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	f, err := a.releaseBlobs().open(as.SHA256)
	if err != nil {
		logErr(r, fmt.Errorf("release asset %d: %w", as.ID, err))
		http.Error(w, "this file is missing from the server", http.StatusGone)
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": as.Name}))
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Cache-Control", "private, no-cache")
	h.Set("ETag", `"`+as.SHA256+`"`)
	h.Set("X-Checksum-Sha256", as.SHA256)
	// Count a download once: the first (or only) range of a GET.
	if r.Method == http.MethodGet {
		if rg := r.Header.Get("Range"); rg == "" || strings.HasPrefix(rg, "bytes=0-") {
			if err := a.db.CountReleaseDownload(context.WithoutCancel(ctx), as.ID); err != nil {
				logErr(r, err)
			}
		}
	}
	http.ServeContent(w, r, "", as.CreatedAt, f)
}

// releasesArchive streams a source archive: /{owner}/{repo}/archive/<ref>.tar.gz or .zip.
func (a *App) releasesArchive(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := a.publicRepoAccess(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	file := r.PathValue("file")
	format := ""
	ref := ""
	switch {
	case strings.HasSuffix(file, ".tar.gz"):
		format, ref = "tar.gz", strings.TrimSuffix(file, ".tar.gz")
	case strings.HasSuffix(file, ".zip"):
		format, ref = "zip", strings.TrimSuffix(file, ".zip")
	default:
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	g, err := a.git.Open(repo.Owner, repo.Name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	sha, ok := g.TagCommit(ctx, ref)
	if !ok {
		if sha, err = g.ResolveCommit(ctx, ref); err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}
	prefix := repo.Name + "-" + strings.ReplaceAll(strings.TrimPrefix(ref, "v"), "/", "-")
	ctype := "application/gzip"
	if format == "zip" {
		ctype = "application/zip"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": prefix + "." + format}))
	h.Set("Cache-Control", "private, max-age=300")
	if err := g.Archive(ctx, w, sha, format, prefix); err != nil {
		logErr(r, err)
	}
}
