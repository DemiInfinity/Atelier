package web

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"atelier/internal/store"
)

// Snippet comments (issue #12). The rules are the ones issue comments follow,
// decided by App.can on the snippet's stand-in repository: anyone who can read
// the snippet sees its comments, commenting needs write (as commenting on an
// issue does), people edit and delete their own comments, and whoever can
// administer the snippet may also delete any comment on it. Bodies go through
// the shared Markdown renderer, which escapes raw HTML.

const snippetCommentMax = 65536

func init() {
	registerFeature(feature{name: "snippet-comments", routes: func(a *App, m *http.ServeMux) {
		m.HandleFunc("POST /snippets/{id}/comments", a.authed(a.snippetCommentAdd))
		m.HandleFunc("POST /snippets/{id}/comments/{cid}/edit", a.authed(a.snippetCommentEdit))
		m.HandleFunc("POST /snippets/{id}/comments/{cid}/delete", a.authed(a.snippetCommentDelete))
	}})
}

// snippetCommentView is one comment as the snippet page shows it.
type snippetCommentView struct {
	store.SnippetComment
	HTML      template.HTML
	CanEdit   bool
	CanDelete bool
}

// snippetCommentsData is the comments part of the snippet page.
type snippetCommentsData struct {
	List       []snippetCommentView
	CanComment bool
}

// ownsSnippetComment: the comment was written by who (same person, or the same
// token or bot name for comments not tied to a person).
func ownsSnippetComment(c store.SnippetComment, who Actor) bool {
	if c.PersonID != 0 && who.PersonID != 0 {
		return c.PersonID == who.PersonID && c.AuthorKind == who.Kind
	}
	return c.AuthorKind == who.Kind && c.AuthorName == who.Name
}

// snippetComments loads the comments a reader of sn sees.
func (a *App) snippetComments(r *http.Request, sn store.Snippet, who Actor) (snippetCommentsData, error) {
	ctx := r.Context()
	repo := snippetRepo(sn)
	d := snippetCommentsData{CanComment: who.Kind != ActorAnonymous && a.can(ctx, who, repo, PermWrite)}
	cs, err := a.db.SnippetComments(ctx, sn.ID)
	if err != nil {
		return d, err
	}
	admin := a.can(ctx, who, repo, PermAdmin)
	for _, c := range cs {
		own := d.CanComment && ownsSnippetComment(c, who)
		d.List = append(d.List, snippetCommentView{SnippetComment: c, HTML: renderMarkdown([]byte(c.Body)),
			CanEdit: own, CanDelete: own || admin})
	}
	return d, nil
}

func (a *App) snippetCommentAdd(w http.ResponseWriter, r *http.Request) {
	sn, _, who, ok := a.loadSnippet(w, r, PermWrite)
	if !ok {
		return
	}
	body := strings.TrimRight(normalizeNewlines(r.FormValue("body")), " \n\t")
	if strings.TrimSpace(body) == "" || len(body) > snippetCommentMax {
		http.Redirect(w, r, "/snippets/"+sn.UID+"?comment=empty#comments", http.StatusSeeOther)
		return
	}
	c, err := a.db.AddSnippetComment(r.Context(), sn.ID, body, who.Kind, who.Name, who.PersonID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/snippets/%s#snippet-comment-%d", sn.UID, c.ID), http.StatusSeeOther)
}

// loadSnippetComment finds comment {cid} on the snippet in the URL. The
// snippet must be readable; what else is needed is up to the caller.
func (a *App) loadSnippetComment(w http.ResponseWriter, r *http.Request) (store.Snippet, store.SnippetComment, Actor, bool) {
	sn, _, who, ok := a.loadSnippet(w, r, PermRead)
	if !ok {
		return sn, store.SnippetComment{}, who, false
	}
	id, err := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return sn, store.SnippetComment{}, who, false
	}
	c, err := a.db.SnippetCommentByID(r.Context(), id)
	if err != nil || c.SnippetID != sn.ID {
		a.notFound(w, r)
		return sn, store.SnippetComment{}, who, false
	}
	return sn, c, who, true
}

func (a *App) snippetCommentEdit(w http.ResponseWriter, r *http.Request) {
	sn, c, who, ok := a.loadSnippetComment(w, r)
	if !ok {
		return
	}
	if !a.can(r.Context(), who, snippetRepo(sn), PermWrite) || !ownsSnippetComment(c, who) {
		a.forbidden(w, r)
		return
	}
	body := strings.TrimRight(normalizeNewlines(r.FormValue("body")), " \n\t")
	if strings.TrimSpace(body) == "" || len(body) > snippetCommentMax {
		http.Redirect(w, r, fmt.Sprintf("/snippets/%s?comment=empty#snippet-comment-%d", sn.UID, c.ID), http.StatusSeeOther)
		return
	}
	if err := a.db.UpdateSnippetComment(r.Context(), c.ID, body); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/snippets/%s#snippet-comment-%d", sn.UID, c.ID), http.StatusSeeOther)
}

func (a *App) snippetCommentDelete(w http.ResponseWriter, r *http.Request) {
	sn, c, who, ok := a.loadSnippetComment(w, r)
	if !ok {
		return
	}
	repo := snippetRepo(sn)
	own := a.can(r.Context(), who, repo, PermWrite) && ownsSnippetComment(c, who)
	if !own && !a.can(r.Context(), who, repo, PermAdmin) {
		a.forbidden(w, r)
		return
	}
	if err := a.db.DeleteSnippetComment(r.Context(), c.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/snippets/"+sn.UID+"#comments", http.StatusSeeOther)
}
