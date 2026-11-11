package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"atelier/internal/store"
	"atelier/internal/tether"
)

// Settings → Tether: link a GitHub account with a fine-grained personal
// access token (pasted, then sealed) or through the GitHub OAuth app that
// "Continue with GitHub" already uses.

const tetherStateCookie = "atelier_tether_state"

func tetherSettingsRoutes(a *App, m *http.ServeMux) {
	m.HandleFunc("GET /settings/tether", a.authed(a.tetherSettingsPage))
	m.HandleFunc("POST /settings/tether/token", a.authed(a.tetherConnectToken))
	m.HandleFunc("POST /settings/tether/disconnect", a.authed(a.tetherDisconnect))
	m.HandleFunc("POST /settings/tether/badge", a.authed(a.tetherBadgeToggle))
	m.HandleFunc("GET /settings/tether/oauth", a.authed(a.tetherOAuthStart))
	// A subpath of the OAuth app's registered callback, which GitHub accepts.
	m.HandleFunc("GET /auth/github/callback/tether", a.authed(a.tetherOAuthCallback))
}

// tetherScopesNeeded is shown next to the token field.
var tetherScopesNeeded = []struct{ Name, Access, Why string }{
	{"Contents", "Read and write", "mirror pushes, the .atelier badge file"},
	{"Pull requests", "Read and write", "mirror pull requests, Merge on GitHub"},
	{"Issues", "Read and write", "sync issues and comments both ways"},
	{"Commit statuses", "Read and write", "report Actions results on GitHub pull requests"},
	{"Metadata", "Read-only", "required by GitHub for every token"},
	{"Starring (optional)", "Read-only", "private stars in the home feed; without it, your public stars are used"},
}

type tetherLinkRow struct {
	Repo store.Repo
	Link store.TetherLink
}

func (a *App) tetherSettingsPage(w http.ResponseWriter, r *http.Request) {
	a.renderTetherSettings(w, r, "", http.StatusOK)
}

func (a *App) renderTetherSettings(w http.ResponseWriter, r *http.Request, errMsg string, code int) {
	ctx := r.Context()
	who := a.actor(r)
	d := map[string]any{"Scopes": tetherScopesNeeded, "OAuth": a.cfg.GitHubEnabled(),
		"TokenURL": a.tetherWebURL("settings/personal-access-tokens/new")}
	acc, err := a.db.TetherAccount(ctx, who.PersonID)
	if err == nil {
		d["Account"] = acc
		d["Avatar"] = tetherAvatar(acc.AvatarURL)
		d["Profile"] = a.tetherWebURL(acc.Login)
		d["ProfileRepo"] = acc.Login + "/" + acc.Login
		if acc.RateRemaining >= 0 && acc.RateLimit > 0 {
			d["Rate"] = fmt.Sprintf("%d of %d requests left this hour", acc.RateRemaining, acc.RateLimit)
			if !acc.RateReset.IsZero() && acc.RateReset.After(time.Now()) {
				d["RateReset"] = "resets at " + acc.RateReset.UTC().Format("15:04") + " UTC"
			}
		}
		if c, _, err := a.tetherClient(ctx, who.PersonID); err == nil {
			if until, blocked := c.BlockedUntil(); blocked {
				d["Blocked"] = "Paused until " + until.UTC().Format("15:04") + " UTC: GitHub's rate limit ran out."
			}
		} else {
			d["KeyProblem"] = err.Error()
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		a.fail(w, r, err)
		return
	}
	var rows []tetherLinkRow
	if links, err := a.db.TetherLinks(ctx); err == nil {
		for _, l := range links {
			if l.PersonID != who.PersonID {
				continue
			}
			rp, err := a.repoByID(ctx, l.RepoID)
			if err != nil || !a.can(ctx, who, rp, PermRead) {
				continue
			}
			rows = append(rows, tetherLinkRow{Repo: rp, Link: l})
		}
	}
	d["Links"] = rows
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	a.render(w, r, "tether_settings", page{Title: "Tether", Nav: "tokens", Error: errMsg,
		Notice: r.URL.Query().Get("saved"), Data: d})
}

// tetherConnect checks a token with GitHub and saves the account.
func (a *App) tetherConnect(ctx context.Context, personID int64, token, method string, privateMirror bool) (store.TetherAccount, error) {
	st := a.tether()
	c := &tether.Client{API: st.api, Token: token, HTTP: st.hc}
	me, h, err := c.Me(ctx)
	if err != nil {
		return store.TetherAccount{}, err
	}
	if !tether.ValidLogin(me.Login) {
		return store.TetherAccount{}, errors.New("GitHub sent an unexpected login")
	}
	scopes := strings.TrimSpace(h.Get("X-OAuth-Scopes"))
	if scopes == "" && strings.HasPrefix(token, "github_pat_") {
		scopes = "fine-grained"
	}
	prev, err := a.db.TetherAccount(ctx, personID)
	acc := store.TetherAccount{PersonID: personID, Login: me.Login, Name: me.Name, AvatarURL: tetherAvatar(me.AvatarURL),
		Method: method, Scopes: scopes, PrivateMirror: privateMirror}
	if err == nil {
		acc.Badge = prev.Badge
	}
	if err := a.db.SaveTetherAccount(ctx, a.secretSealer(), acc, token); err != nil {
		return acc, err
	}
	c.Unblock()
	return acc, nil
}

func (a *App) tetherConnectToken(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimSpace(r.FormValue("token"))
	if tok == "" || len(tok) > 255 || strings.ContainsAny(tok, " \t\r\n") {
		a.renderTetherSettings(w, r, "Paste a GitHub personal access token.", http.StatusUnprocessableEntity)
		return
	}
	if _, err := a.tetherConnect(r.Context(), a.actor(r).PersonID, tok, "pat", r.FormValue("private_mirror") != ""); err != nil {
		a.renderTetherSettings(w, r, "Couldn't connect: "+tetherErrText(err), http.StatusUnprocessableEntity)
		return
	}
	a.tetherRefreshIfStale(context.WithoutCancel(r.Context()), a.actor(r).PersonID, "")
	http.Redirect(w, r, "/settings/tether?saved=1", http.StatusSeeOther)
}

func (a *App) tetherDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := a.db.DeleteTetherAccount(r.Context(), a.actor(r).PersonID); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/tether?saved=1", http.StatusSeeOther)
}

// ---------- OAuth ----------

func (a *App) tetherOAuthStart(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.GitHubEnabled() {
		a.notFound(w, r)
		return
	}
	private := r.URL.Query().Get("private") == "1"
	state := store.RandomToken(16)
	flag := "0"
	if private {
		flag = "1"
	}
	http.SetCookie(w, &http.Cookie{Name: tetherStateCookie, Value: state + "|" + flag, Path: "/auth/github/callback/tether",
		HttpOnly: true, Secure: a.isHTTPS(r), SameSite: http.SameSiteLaxMode, MaxAge: 600})
	// repo only when private mirroring is wanted; public_repo otherwise.
	scope := "read:user public_repo"
	if private {
		scope = "read:user repo"
	}
	q := url.Values{"client_id": {a.cfg.GitHubClientID}, "redirect_uri": {a.baseURL(r) + "/auth/github/callback/tether"},
		"state": {state}, "scope": {scope}, "allow_signup": {"false"}}
	http.Redirect(w, r, a.tetherWebURL("login/oauth/authorize")+"?"+q.Encode(), http.StatusSeeOther)
}

func (a *App) tetherOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.GitHubEnabled() {
		a.notFound(w, r)
		return
	}
	c, err := r.Cookie(tetherStateCookie)
	http.SetCookie(w, &http.Cookie{Name: tetherStateCookie, Value: "", Path: "/auth/github/callback/tether", MaxAge: -1})
	if err != nil {
		a.renderTetherSettings(w, r, "Linking expired. Please try again.", http.StatusBadRequest)
		return
	}
	state, flag, _ := strings.Cut(c.Value, "|")
	if state == "" || r.URL.Query().Get("state") != state {
		a.renderTetherSettings(w, r, "Linking could not be verified. Please try again.", http.StatusBadRequest)
		return
	}
	tok, err := a.tetherExchange(r.Context(), r.URL.Query().Get("code"), a.baseURL(r)+"/auth/github/callback/tether")
	if err != nil {
		logErr(r, err)
		a.renderTetherSettings(w, r, "GitHub didn't complete the link. Please try again.", http.StatusBadGateway)
		return
	}
	if _, err := a.tetherConnect(r.Context(), a.actor(r).PersonID, tok, "oauth", flag == "1"); err != nil {
		a.renderTetherSettings(w, r, "Couldn't connect: "+tetherErrText(err), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/settings/tether?saved=1", http.StatusSeeOther)
}

// tetherExchange swaps an OAuth code for a token.
func (a *App) tetherExchange(ctx context.Context, code, redirect string) (string, error) {
	if code == "" || len(code) > 200 {
		return "", errors.New("missing code")
	}
	form := url.Values{"client_id": {a.cfg.GitHubClientID}, "client_secret": {a.cfg.GitHubClientSecret},
		"code": {code}, "redirect_uri": {redirect}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.tetherWebURL("login/oauth/access_token"), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := doJSON(req, &out); err != nil {
		return "", errors.New("token exchange failed")
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("token exchange failed: %s", eventExcerpt(out.Error, 80))
	}
	return out.AccessToken, nil
}
