package web

// Settings → Tether: more than one GitHub token.
//
// A fine-grained token covers one GitHub owner, so a person may add extra
// tokens, each with the owners it covers. Repository work for those owners
// (links, mirroring, sync, checks, Merge on GitHub, the apps bridge, /gh/
// repository pages) uses the covering token; everything else uses the main
// token (tetherTokenFor in tether.go picks).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"atelier/internal/store"
	"atelier/internal/tether"
)

// tetherTokenRow is an extra token as Settings → Tether shows it.
type tetherTokenRow struct {
	T         store.TetherExtraToken
	Name      string
	Owners    string
	Profile   string
	Rate      string
	RateReset string
	Blocked   string
	Problem   string
}

// tetherTokenName is how a token is called on pages.
func tetherTokenName(t store.TetherExtraToken) string {
	if t.Label != "" {
		return t.Label
	}
	return "Token for " + strings.Join(t.Owners, ", ")
}

func tetherRateText(limit, remaining int, reset time.Time) (string, string) {
	if remaining < 0 || limit <= 0 {
		return "", ""
	}
	r := fmt.Sprintf("%d of %d requests left this hour", remaining, limit)
	if !reset.IsZero() && reset.After(time.Now()) {
		return r, "resets at " + reset.UTC().Format("15:04") + " UTC"
	}
	return r, ""
}

// tetherTokenRows lists a person's extra tokens for the settings page.
func (a *App) tetherTokenRows(ctx context.Context, personID int64) []tetherTokenRow {
	all, err := a.db.TetherExtraTokens(ctx, personID)
	if err != nil {
		return nil
	}
	var out []tetherTokenRow
	for _, t := range all {
		row := tetherTokenRow{T: t, Name: tetherTokenName(t), Owners: strings.Join(t.Owners, ", ")}
		if tether.ValidLogin(t.Login) {
			row.Profile = a.tetherWebURL(t.Login)
		}
		row.Rate, row.RateReset = tetherRateText(t.RateLimit, t.RateRemaining, t.RateReset)
		tok, err := a.db.TetherExtraTokenSecret(ctx, a.secretSealer(), personID, t.ID)
		if err != nil {
			row.Problem = errTetherTokenUnreadable.Error()
		} else {
			c := a.tetherClientWith(ctx, personID, tetherPick{Token: tok, Extra: &t})
			if until, blocked := c.BlockedUntil(); blocked {
				row.Blocked = "Paused until " + until.UTC().Format("15:04") + " UTC: GitHub's rate limit ran out."
			}
		}
		out = append(out, row)
	}
	return out
}

// tetherTokenTaken reports whether token is already one of the person's.
func (a *App) tetherTokenTaken(ctx context.Context, personID int64, token string) bool {
	if main, err := a.db.TetherToken(ctx, a.secretSealer(), personID); err == nil && main == token {
		return true
	}
	all, _ := a.db.TetherExtraTokens(ctx, personID)
	for _, t := range all {
		if tok, err := a.db.TetherExtraTokenSecret(ctx, a.secretSealer(), personID, t.ID); err == nil && tok == token {
			return true
		}
	}
	return false
}

// tetherAddToken checks a token with GitHub and saves it as an extra token.
// With no owners given, it covers the owners whose repositories the token
// can see (for a fine-grained token, its resource owner), or else its login.
func (a *App) tetherAddToken(ctx context.Context, personID int64, label, token string, owners []string) (store.TetherExtraToken, error) {
	if _, err := a.db.TetherAccount(ctx, personID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.TetherExtraToken{}, errors.New("connect your main GitHub token first")
		}
		return store.TetherExtraToken{}, err
	}
	if a.tetherTokenTaken(ctx, personID, token) {
		return store.TetherExtraToken{}, errors.New("that token is already saved")
	}
	st := a.tether()
	var rate tether.Rate
	c := &tether.Client{API: st.api, Token: token, HTTP: st.hc, OnRate: func(r tether.Rate) {
		if r.Resource == "" || r.Resource == "core" {
			rate = r
		}
	}}
	me, h, err := c.Me(ctx)
	if err != nil {
		return store.TetherExtraToken{}, err
	}
	if !tether.ValidLogin(me.Login) {
		return store.TetherExtraToken{}, errors.New("GitHub sent an unexpected login")
	}
	owners = store.NormalizeTetherOwners(owners)
	for _, o := range owners {
		if !tether.ValidLogin(o) {
			return store.TetherExtraToken{}, fmt.Errorf("%q isn't a GitHub user or organisation name", eventExcerpt(o, 60))
		}
	}
	if len(owners) == 0 {
		owners, _ = c.VisibleOwners(ctx)
		if len(owners) == 0 {
			owners = []string{me.Login}
		}
	}
	scopes := strings.TrimSpace(h.Get("X-OAuth-Scopes"))
	if scopes == "" && strings.HasPrefix(token, "github_pat_") {
		scopes = "fine-grained"
	}
	if len(label) > 80 {
		label = label[:80]
	}
	t, err := a.db.AddTetherExtraToken(ctx, a.secretSealer(), store.TetherExtraToken{PersonID: personID, Label: label,
		Login: me.Login, Owners: owners, Method: "pat", Scopes: scopes}, token)
	if err != nil {
		return t, err
	}
	if rate.Limit > 0 {
		_ = a.db.SetTetherExtraTokenRate(ctx, personID, t.ID, rate.Limit, rate.Remaining, rate.Reset)
	}
	c.Unblock()
	return t, nil
}

func (a *App) tetherAddTokenPost(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimSpace(r.FormValue("token"))
	if tok == "" || len(tok) > 255 || strings.ContainsAny(tok, " \t\r\n") {
		a.renderTetherSettings(w, r, "Paste a GitHub personal access token.", http.StatusUnprocessableEntity)
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	if _, err := a.tetherAddToken(r.Context(), a.actor(r).PersonID, label, tok, []string{r.FormValue("owners")}); err != nil {
		a.renderTetherSettings(w, r, "Couldn't add the token: "+tetherErrText(err), http.StatusUnprocessableEntity)
		return
	}
	http.Redirect(w, r, "/settings/tether?saved=1", http.StatusSeeOther)
}

func tetherTokenID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (a *App) tetherTokenOwnersPost(w http.ResponseWriter, r *http.Request) {
	id, ok := tetherTokenID(r)
	if !ok {
		a.notFound(w, r)
		return
	}
	owners := store.NormalizeTetherOwners([]string{r.FormValue("owners")})
	for _, o := range owners {
		if !tether.ValidLogin(o) {
			a.renderTetherSettings(w, r, fmt.Sprintf("%q isn't a GitHub user or organisation name.", eventExcerpt(o, 60)), http.StatusUnprocessableEntity)
			return
		}
	}
	err := a.db.SetTetherExtraTokenOwners(r.Context(), a.actor(r).PersonID, id, owners)
	switch {
	case errors.Is(err, store.ErrNotFound):
		a.notFound(w, r)
		return
	case err != nil:
		a.renderTetherSettings(w, r, "Couldn't save the owners: "+tetherErrText(err), http.StatusUnprocessableEntity)
		return
	}
	http.Redirect(w, r, "/settings/tether?saved=1", http.StatusSeeOther)
}

func (a *App) tetherTokenDeletePost(w http.ResponseWriter, r *http.Request) {
	id, ok := tetherTokenID(r)
	if !ok {
		a.notFound(w, r)
		return
	}
	err := a.db.DeleteTetherExtraToken(r.Context(), a.actor(r).PersonID, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		a.notFound(w, r)
		return
	case err != nil:
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/tether?saved=1", http.StatusSeeOther)
}

// tetherNoteToken records how the extra token covering ghRepo's owner fared
// (a rejected token shows as its last problem; success clears it). Nothing
// happens when the main token was used.
func (a *App) tetherNoteToken(ctx context.Context, personID int64, ghRepo string, err error) {
	t, terr := a.db.TetherTokenForOwner(ctx, personID, tetherOwnerOf(ghRepo))
	if terr != nil {
		return
	}
	msg := ""
	if tether.IsStatus(err, http.StatusUnauthorized) {
		msg = tetherErrText(err)
	} else if err != nil {
		return // not the token's fault (or not clearly): leave it be
	}
	if msg != t.LastError {
		_ = a.db.SetTetherExtraTokenError(ctx, personID, t.ID, msg)
	}
}
