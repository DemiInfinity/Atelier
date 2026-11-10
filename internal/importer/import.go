package importer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// Request is one repository to bring in.
type Request struct {
	Source      string // "gitea", "github" or "git"
	URL         string // clone URL without credentials
	Creds       Creds
	Owner       string // Atelier owner (personal or company account)
	Name        string
	Description string
	Private     bool
	// Mirror keeps fetching from the source every MirrorInterval. Only mirrors
	// store their credentials; one-off imports forget them when done.
	Mirror         bool
	MirrorInterval time.Duration
	// Meta selects issues, pull requests, labels and milestones to import
	// after the git data (gitea and github sources only). MetaAPI is the
	// GitHub API base or Gitea server address; MetaRepo is owner/name there.
	Meta     MetaOptions
	MetaAPI  string
	MetaRepo string
	// ResumeRepoID, when set, means the code is already in that repository:
	// only the metadata is fetched (again), skipping what's already here.
	ResumeRepoID int64
}

// Check validates a request against the database without touching disk.
func Check(ctx context.Context, db *store.Store, req Request) (store.Owner, error) {
	owner, err := db.OwnerByName(ctx, req.Owner)
	if err != nil {
		return owner, fmt.Errorf("there's no account called %q", req.Owner)
	}
	if !gitrepo.ValidName(req.Name) {
		return owner, fmt.Errorf("%q isn't a usable name: use letters, numbers, dots, dashes or underscores", req.Name)
	}
	if _, err := db.RepoByName(ctx, owner.Name, req.Name); err == nil {
		return owner, fmt.Errorf("%s/%s already exists", owner.Name, req.Name)
	}
	return owner, nil
}

// Import clones the source into Atelier, creates the repository row and runs
// the AfterCreate hooks. Nothing is left behind on failure.
func Import(ctx context.Context, db *store.Store, gs *gitrepo.Store, req Request) (store.Repo, error) {
	owner, err := Check(ctx, db, req)
	if err != nil {
		return store.Repo{}, err
	}
	final := gs.Path(owner.Name, req.Name)
	if _, err := os.Stat(final); err == nil {
		return store.Repo{}, errors.New("a repository already exists on disk at that name")
	}
	// Clone next to the final path, then move it into place, so a half-done
	// clone never looks like a repository.
	tmp := final + ".importing"
	_ = os.RemoveAll(tmp)
	if err := Mirror(ctx, gs.Git, req.URL, req.Creds, tmp, req.Mirror); err != nil {
		return store.Repo{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.RemoveAll(tmp)
		return store.Repo{}, err
	}
	cleanup := func() { _ = gs.Remove(owner.Name, req.Name) }
	if err := gs.AfterCreate(ctx, owner.Name, req.Name); err != nil {
		cleanup()
		return store.Repo{}, err
	}
	repo, err := db.CreateRepo(ctx, owner.ID, req.Name, strings.TrimSpace(req.Description), req.Private)
	if err != nil {
		cleanup()
		return store.Repo{}, err
	}
	if req.Mirror {
		iv := req.MirrorInterval
		if iv <= 0 {
			iv = 30 * time.Minute
		}
		m := store.ImportMirror{RepoID: repo.ID, Source: req.Source, SourceURL: req.URL,
			Username: req.Creds.Username, Secret: req.Creds.Token, Interval: iv}
		if err := db.SaveImportMirror(ctx, m); err != nil {
			return repo, fmt.Errorf("imported, but couldn't save the mirror settings: %w", err)
		}
		_ = db.MarkImportMirrorSynced(ctx, repo.ID, time.Now(), "")
	}
	return repo, nil
}

// SyncMirror fetches one mirror and records the result.
func SyncMirror(ctx context.Context, db *store.Store, gs *gitrepo.Store, m store.ImportMirror) error {
	err := Fetch(ctx, gs.Git, gs.Path(m.Owner, m.Name), Creds{Username: m.Username, Token: m.Secret})
	msg := ""
	if err != nil {
		msg = err.Error()
	} else {
		_ = db.TouchRepo(ctx, m.RepoID)
	}
	if merr := db.MarkImportMirrorSynced(context.WithoutCancel(ctx), m.RepoID, time.Now(), msg); merr != nil && err == nil {
		err = merr
	}
	return err
}

// Due reports whether a mirror should be synced at now.
func Due(m store.ImportMirror, now time.Time) bool {
	return m.LastSyncAt == nil || !now.Before(m.LastSyncAt.Add(m.Interval))
}

// NameFromURL guesses a repository name from a clone URL ("…/project.git" → "project").
func NameFromURL(u string) string {
	u = strings.TrimRight(strings.TrimSpace(u), "/")
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		u = u[i+1:]
	}
	u = strings.TrimSuffix(u, ".git")
	return u
}
