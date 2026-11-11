package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
	"atelier/internal/tether"
)

// Repositories for the app API: the list (with what a client needs to clone
// and open them), creating one like the New repository page does, and
// Tether's "Mirror to studio".

// appRepoJSON is the contract's repo object.
func (a *App) appRepoJSON(ctx context.Context, r *http.Request, rp store.Repo) map[string]any {
	def := "main"
	if g, err := a.git.Open(rp.Owner, rp.Name); err == nil && !g.IsEmpty(ctx) {
		def = g.DefaultBranch(ctx)
	}
	// The primary language, as repository lists show it (cache only; a
	// repository never counted is counted in the background).
	var lang, color any
	name := ""
	if s := a.langsCached(ctx, rp.ID); s != nil {
		name = s.Primary()
	} else if st := a.insCached(ctx, rp.ID); st != nil {
		a.saveLangs(ctx, rp.ID, st.Head, st)
		if len(st.Langs) > 0 {
			name = st.Langs[0].Name
		}
	} else {
		a.langListKick(rp.ID)
	}
	if name != "" {
		lang, color = name, langColor(name)
	}
	var ssh any
	if base := a.sshCloneBase(r); base != "" {
		ssh = base + "/" + rp.FullName() + ".git"
	}
	var gh any
	if l, err := a.db.TetherLink(ctx, rp.ID); err == nil {
		gh = map[string]any{"repo": l.GHRepo, "home": l.Home}
	}
	open := 0
	if counts, err := a.db.PullCounts(ctx, rp.ID); err == nil {
		open = counts["open"]
	}
	return map[string]any{
		"full_name": rp.FullName(), "owner": rp.Owner, "owner_kind": rp.OwnerKind, "name": rp.Name,
		"private": rp.Private, "description": rp.Description, "default_branch": def,
		"language": lang, "language_color": color,
		"clone_https": a.baseURL(r) + "/" + rp.FullName() + ".git", "clone_ssh": ssh,
		"github": gh, "open_pulls": open, "updated_at": appTime(rp.UpdatedAt),
	}
}

func (a *App) appRepos(w http.ResponseWriter, r *http.Request, c *appCaller) {
	ctx := r.Context()
	repos, _, err := a.appReadable(ctx, c.Who)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	out := []map[string]any{}
	for _, rp := range repos {
		if q != "" && !strings.Contains(strings.ToLower(rp.Name), q) && !strings.Contains(strings.ToLower(rp.FullName()), q) {
			continue
		}
		out = append(out, a.appRepoJSON(ctx, r, rp))
	}
	body := map[string]any{"repos": out}
	if q != "" && r.URL.Query().Get("github") == "1" {
		hits, problem := a.appGitHubHits(ctx, c, q)
		body["github_hits"] = hits
		if problem != "" {
			body["github_error"] = problem
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// appGitHubHits searches GitHub through the caller's Tether token (at most
// 10 repositories), marking owners who show the Atelier badge.
func (a *App) appGitHubHits(ctx context.Context, c *appCaller, q string) ([]map[string]any, string) {
	hits := []map[string]any{}
	cl, _, err := a.tetherClient(ctx, c.Who.PersonID)
	if err != nil {
		return hits, tetherErrText(err)
	}
	repos, _, err := cl.SearchRepos(ctx, q, "", 10)
	if err != nil {
		return hits, tetherErrText(err)
	}
	var logins []string
	for _, rp := range repos {
		logins = append(logins, rp.Owner.Login)
	}
	badges := a.tetherBadges(ctx, c.Who.PersonID, logins)
	for _, rp := range repos {
		if !tether.ValidRepo(rp.FullName) || len(hits) == 10 {
			continue
		}
		hits = append(hits, map[string]any{"full_name": rp.FullName, "description": eventExcerpt(rp.Description, 200),
			"atelier_badge": a.tetherPersonView(rp.Owner.Login, rp.Owner.AvatarURL, badges).Atelier,
			"web_path":      ghLocalURL(rp.FullName)})
	}
	return hits, ""
}

func (a *App) appRepoCreate(w http.ResponseWriter, r *http.Request, c *appCaller) {
	ctx := r.Context()
	var in struct {
		Owner       string `json:"owner"`
		Name        string `json:"name"`
		Private     *bool  `json:"private"`
		Description string `json:"description"`
	}
	if !appDecode(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	desc := appCleanText(in.Description, 300)
	private := in.Private == nil || *in.Private
	if in.Owner == "" {
		in.Owner = c.Person.Name
	}
	owner, err := a.db.OwnerByName(ctx, in.Owner)
	// Ask the shared permission check about the repository as it would exist
	// (like push to create): an account the caller can't write to is a 403.
	if err != nil || !a.can(ctx, c.Who, store.Repo{OwnerID: owner.ID, Owner: owner.Name, OwnerKind: owner.Kind, Name: name, Private: true}, PermWrite) {
		appError(w, http.StatusForbidden, "forbidden", "You can't create repositories under that account.")
		return
	}
	if !gitrepo.ValidName(name) {
		appError(w, http.StatusUnprocessableEntity, "invalid_name", "Use letters, numbers, dots, dashes or underscores, and don't end the name with .git.")
		return
	}
	if _, err := a.db.RepoByName(ctx, owner.Name, name); err == nil {
		appError(w, http.StatusConflict, "name_taken", "You already have a repository with that name.")
		return
	}
	if err := a.git.Init(ctx, owner.Name, name); err != nil { // installs the hooks too
		if _, err2 := a.db.RepoByName(ctx, owner.Name, name); err2 == nil {
			appError(w, http.StatusConflict, "name_taken", "You already have a repository with that name.")
			return
		}
		a.appFail(w, r, err)
		return
	}
	rp, err := a.db.CreateRepo(ctx, owner.ID, name, desc, private)
	if err != nil {
		_ = a.git.Remove(owner.Name, name)
		if _, err2 := a.db.RepoByName(ctx, owner.Name, name); err2 == nil {
			appError(w, http.StatusConflict, "name_taken", "You already have a repository with that name.")
			return
		}
		a.appFail(w, r, err)
		return
	}
	log.Printf("appapi: %s created %s", c.Who.Name, rp.FullName())
	a.emit(ctx, Event{Kind: "repo.created", RepoID: rp.ID, Repo: rp, Title: "Created " + rp.FullName(),
		URL: "/" + rp.FullName(), Actor: c.Who})
	writeJSON(w, http.StatusCreated, a.appRepoJSON(ctx, r, rp))
}

// appRepoManage loads the repository in the path for the two things a device
// may be allowed to administer: renaming and deleting it. Device tokens never
// pass App.can(PermAdmin) (corePolicy), and this doesn't change that: no scope
// is added and no policy widened, so nothing else that asks for admin opens
// up. Instead, here only, a device whose person switched on "Can rename and
// delete repositories" (Settings → Devices) is let through when that person,
// signed in on the web, could administer the repository themselves. Anything
// App.can already allows (an addon's policy) still counts. Ordinary access
// tokens with the app scope have no such switch and stay forbidden.
func (a *App) appRepoManage(w http.ResponseWriter, r *http.Request, c *appCaller) (store.Repo, bool) {
	ctx := r.Context()
	rp, ok := a.appRepo(w, r, c, PermRead)
	if !ok {
		return rp, false
	}
	if a.can(ctx, c.Who, rp, PermAdmin) {
		return rp, true
	}
	if c.Device == nil || !a.can(ctx, personActor(c.Person), rp, PermAdmin) {
		appError(w, http.StatusForbidden, "forbidden", "You don't have admin access to this repository.")
		return rp, false
	}
	if !c.Device.CanAdmin {
		appError(w, http.StatusForbidden, "device_not_allowed", "This device isn't allowed to rename or delete repositories. Turn it on in Settings → Devices.")
		return rp, false
	}
	return rp, true
}

// appRepoRename renames a repository in place, like "Move or rename" on its
// settings page (moveRepo): the old name keeps redirecting.
func (a *App) appRepoRename(w http.ResponseWriter, r *http.Request, c *appCaller) {
	rp, ok := a.appRepoManage(w, r, c)
	if !ok {
		return
	}
	ctx := r.Context()
	var in struct {
		Name string `json:"name"`
	}
	if !appDecode(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		appError(w, http.StatusUnprocessableEntity, "invalid_name", "Give the repository a name.")
		return
	}
	if name == rp.Name { // nothing to do
		writeJSON(w, http.StatusOK, a.appRepoJSON(ctx, r, rp))
		return
	}
	owner, err := a.db.OwnerByID(ctx, rp.OwnerID)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	renamed, err := a.moveRepo(ctx, c.Who, rp, owner, name)
	var me *repoMoveError
	switch {
	case errors.As(err, &me) && me.Code == "name_taken":
		appError(w, http.StatusConflict, "name_taken", me.Msg)
		return
	case errors.As(err, &me):
		appError(w, http.StatusUnprocessableEntity, "invalid_name", me.Msg)
		return
	case err != nil:
		a.appFail(w, r, err)
		return
	}
	log.Printf("appapi: %s renamed %s to %s", c.Who.Name, rp.FullName(), renamed.FullName())
	writeJSON(w, http.StatusOK, a.appRepoJSON(ctx, r, renamed))
}

// appRepoDelete deletes a repository for good, like the settings page
// (deleteRepo). confirm must be its full name.
func (a *App) appRepoDelete(w http.ResponseWriter, r *http.Request, c *appCaller) {
	rp, ok := a.appRepoManage(w, r, c)
	if !ok {
		return
	}
	var in struct {
		Confirm string `json:"confirm"`
	}
	if !appDecode(w, r, &in) {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(in.Confirm), rp.FullName()) {
		appError(w, http.StatusUnprocessableEntity, "confirm_mismatch", "Send confirm with the repository's full name, "+rp.FullName()+", to delete it.")
		return
	}
	if err := a.deleteRepo(r.Context(), rp); err != nil {
		a.appFail(w, r, err)
		return
	}
	log.Printf("appapi: %s deleted %s", c.Who.Name, rp.FullName())
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) appRepoMirror(w http.ResponseWriter, r *http.Request, c *appCaller) {
	ctx := r.Context()
	var in struct {
		GitHub string `json:"github"`
	}
	if !appDecode(w, r, &in) {
		return
	}
	// The mirror lands in the caller's personal account.
	standIn := store.Repo{OwnerID: c.Person.ID, Owner: c.Person.Name, OwnerKind: c.Person.Kind, Name: "mirror", Private: true}
	if !a.can(ctx, c.Who, standIn, PermWrite) {
		appError(w, http.StatusForbidden, "forbidden", "You can't create repositories here.")
		return
	}
	gh := strings.TrimSpace(in.GitHub)
	if !tether.ValidRepo(gh) {
		appError(w, http.StatusUnprocessableEntity, "mirror_failed", "That isn't a GitHub repository. Use owner/name.")
		return
	}
	if _, _, err := a.tetherClient(ctx, c.Who.PersonID); err != nil {
		appError(w, http.StatusConflict, "tether_off", tetherErrText(err))
		return
	}
	full, err := a.tetherMirrorRepo(ctx, c.Who, gh)
	var me tetherMirrorError
	switch {
	case errors.As(err, &me):
		appError(w, http.StatusUnprocessableEntity, "mirror_failed", string(me))
		return
	case err != nil:
		a.appFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"full_name": full})
}
