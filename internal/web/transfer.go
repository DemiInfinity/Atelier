package web

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// Moving a repository between your accounts (personal ↔ company), optionally
// renaming it. See internal/store/transfer.go for what moves and how old
// addresses keep working.

func init() {
	registerFeature(feature{
		name: "transfer",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("POST /{owner}/{repo}/transfer", a.authed(a.repoTransfer))
		},
	})
}

type moveOptions struct {
	Accounts  []store.Owner // accounts the repository can move to
	Redirects []string      // old names that still lead here
}

func (a *App) moveOptions(r *http.Request, rp store.Repo) moveOptions {
	ctx := r.Context()
	who := a.actor(r)
	var opts moveOptions
	if owners, err := a.db.Owners(ctx); err == nil {
		for _, o := range owners {
			if a.canEditAccount(ctx, who, o) {
				opts.Accounts = append(opts.Accounts, o)
			}
		}
	}
	opts.Redirects, _ = a.db.RepoRedirects(ctx, rp.ID)
	return opts
}

// redirectMovedRepo sends a browser that used an old address to the new one.
func (a *App) redirectMovedRepo(w http.ResponseWriter, r *http.Request, rp store.Repo) bool {
	owner, name := r.PathValue("owner"), r.PathValue("repo")
	if strings.EqualFold(owner, rp.Owner) && strings.EqualFold(name, rp.Name) {
		return false
	}
	oldBase := "/" + owner + "/" + name
	rest := strings.TrimPrefix(r.URL.Path, oldBase)
	if rest == r.URL.Path { // shouldn't happen; be safe
		rest = ""
	}
	target := "/" + rp.FullName() + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
	return true
}

func (a *App) repoTransfer(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings") // needs admin on the repository
	if !ok {
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	back := func(msg string) {
		http.Redirect(w, r, v.Base+"/settings?moveerr="+url.QueryEscape(msg)+"#move", http.StatusSeeOther)
	}

	if !strings.EqualFold(strings.TrimSpace(r.FormValue("confirm")), v.Repo.FullName()) {
		back("Type " + v.Repo.FullName() + " to confirm the move.")
		return
	}
	target, err := a.db.OwnerByName(ctx, r.FormValue("owner"))
	if err != nil || !a.canEditAccount(ctx, who, target) {
		back("You can only move repositories into accounts you manage.")
		return
	}
	newName := strings.TrimSpace(r.FormValue("name"))
	if newName == "" {
		newName = v.Repo.Name
	}
	if !gitrepo.ValidName(newName) || reserved[strings.ToLower(newName)] {
		back("Use letters, numbers, dots, dashes or underscores for the name.")
		return
	}
	if target.ID == v.Repo.OwnerID && newName == v.Repo.Name {
		back("The repository is already there.")
		return
	}

	// Move the bare repository on disk first; undo it if the database step fails.
	oldPath := a.git.Path(v.Repo.Owner, v.Repo.Name)
	newPath := a.git.Path(target.Name, newName)
	moved := false
	if oldPath != newPath {
		if _, err := os.Stat(newPath); err == nil {
			back(target.Name + " already has a repository folder called " + newName + ".")
			return
		}
		if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
			a.fail(w, r, err)
			return
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			a.fail(w, r, err)
			return
		}
		moved = true
	}
	repo, err := a.db.MoveRepo(ctx, v.Repo.ID, target.ID, newName)
	if err != nil {
		if moved {
			if rerr := os.Rename(newPath, oldPath); rerr != nil {
				log.Printf("transfer: could not move %s back to %s: %v", newPath, oldPath, rerr)
			}
		}
		if errors.Is(err, store.ErrNameTaken) {
			back(target.Name + " already has a repository called " + newName + ".")
			return
		}
		a.fail(w, r, err)
		return
	}
	// Server-side hooks know the repository's name: reinstall them.
	if err := a.git.AfterCreate(ctx, repo.Owner, repo.Name); err != nil {
		log.Printf("transfer: reinstalling hooks for %s: %v", repo.FullName(), err)
	}
	a.emit(ctx, Event{Kind: "repo.transferred", RepoID: repo.ID, Repo: repo,
		Title: "Moved " + v.Repo.FullName() + " to " + repo.FullName(), URL: "/" + repo.FullName(), Actor: who})
	http.Redirect(w, r, "/"+repo.FullName()+"/settings?saved=1", http.StatusSeeOther)
}
