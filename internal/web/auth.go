package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

const (
	settingPasswordHash = store.SettingPasswordHash
	minPasswordLen      = 10
	githubStateCookie   = "atelier_gh_state"
)

// safeNext only allows local redirects after sign-in.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

// ---------- first-run setup ----------

func (a *App) setupPage(w http.ResponseWriter, r *http.Request) {
	if !a.needsSetup(r.Context()) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, r, "setup", page{Title: "Set up Atelier", Bare: true})
}

func (a *App) setupSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !a.needsSetup(ctx) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("username"))
	display := strings.TrimSpace(r.FormValue("display_name"))
	pw, pw2 := r.FormValue("password"), r.FormValue("password_confirm")

	fail := func(msg string) {
		a.render(w, r, "setup", page{Title: "Set up Atelier", Bare: true, Error: msg,
			Data: map[string]string{"Username": name, "DisplayName": display}})
	}
	switch {
	case !gitrepo.ValidName(name) || reserved[strings.ToLower(name)]:
		fail("Choose a username with letters, numbers, dots, dashes or underscores.")
		return
	case len(pw) < minPasswordLen:
		fail(fmt.Sprintf("Use at least %d characters for your password.", minPasswordLen))
		return
	case pw != pw2:
		fail("The two passwords don't match.")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.db.SetSetting(ctx, settingPasswordHash, string(hash)); err != nil {
		a.fail(w, r, err)
		return
	}
	if _, err := a.db.CreateOwner(ctx, name, display, "user"); err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.startSession(w, r); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---------- password sign-in ----------

func (a *App) signinPage(w http.ResponseWriter, r *http.Request) {
	if a.needsSetup(r.Context()) {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if a.signedIn(r) {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	a.render(w, r, "signin", page{Title: "Sign in", Bare: true, Data: a.signinData(r, "")})
}

func (a *App) signinData(r *http.Request, errMsg string) map[string]any {
	_, host := a.external(r)
	if a.cfg.PublicURL != "" && !isRemote(r) {
		if u, err := url.Parse(a.cfg.PublicURL); err == nil {
			host = u.Host
		}
	}
	return map[string]any{
		"Next":    safeNext(r.FormValue("next")),
		"GitHub":  a.cfg.GitHubEnabled() && !isRemote(r),
		"Remote":  isRemote(r), // through the tunnel: passkeys only
		"Host":    host,
		"Message": errMsg,
	}
}

func (a *App) signinSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := a.clientIP(r)
	if !a.lim.allow(ip) {
		w.WriteHeader(http.StatusTooManyRequests)
		a.render(w, r, "signin", page{Title: "Sign in", Bare: true, Data: a.signinData(r, "Too many attempts. Wait a minute and try again.")})
		return
	}
	hash, err := a.db.Setting(ctx, settingPasswordHash)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(r.FormValue("password"))) != nil {
		log.Printf("sign-in failed from %s", ip)
		w.WriteHeader(http.StatusUnauthorized)
		a.render(w, r, "signin", page{Title: "Sign in", Bare: true, Data: a.signinData(r, "That password isn't right.")})
		return
	}
	a.lim.reset(ip)
	if err := a.startSession(w, r); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

func (a *App) signout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.db.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.isHTTPS(r)})
	http.Redirect(w, r, "/signin", http.StatusSeeOther)
}

// ---------- Continue with GitHub (locked to one GitHub account) ----------

func (a *App) githubStart(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.GitHubEnabled() {
		http.NotFound(w, r)
		return
	}
	state := store.RandomToken(16)
	next := safeNext(r.URL.Query().Get("next"))
	http.SetCookie(w, &http.Cookie{
		Name: githubStateCookie, Value: state + "|" + url.QueryEscape(next), Path: "/auth/github",
		HttpOnly: true, Secure: a.isHTTPS(r), SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})
	q := url.Values{
		"client_id":    {a.cfg.GitHubClientID},
		"redirect_uri": {a.baseURL(r) + "/auth/github/callback"},
		"state":        {state},
		"scope":        {"read:user"},
		"allow_signup": {"false"},
	}
	http.Redirect(w, r, "https://github.com/login/oauth/authorize?"+q.Encode(), http.StatusSeeOther)
}

func (a *App) githubCallback(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.GitHubEnabled() {
		http.NotFound(w, r)
		return
	}
	c, err := r.Cookie(githubStateCookie)
	http.SetCookie(w, &http.Cookie{Name: githubStateCookie, Value: "", Path: "/auth/github", MaxAge: -1})
	if err != nil {
		a.githubDenied(w, r, "Sign-in expired. Please try again.")
		return
	}
	state, nextEsc, _ := strings.Cut(c.Value, "|")
	if state == "" || r.URL.Query().Get("state") != state {
		a.githubDenied(w, r, "Sign-in could not be verified. Please try again.")
		return
	}
	login, err := a.githubLogin(r.Context(), r.URL.Query().Get("code"), a.baseURL(r)+"/auth/github/callback")
	if err != nil {
		logErr(r, err)
		a.githubDenied(w, r, "GitHub sign-in failed. Please try again.")
		return
	}
	if !strings.EqualFold(login, a.cfg.GitHubLogin) {
		a.githubDenied(w, r, "That GitHub account isn't linked to this Atelier.")
		return
	}
	if err := a.startSession(w, r); err != nil {
		a.fail(w, r, err)
		return
	}
	next, _ := url.QueryUnescape(nextEsc)
	http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
}

func (a *App) githubDenied(w http.ResponseWriter, r *http.Request, msg string) {
	w.WriteHeader(http.StatusUnauthorized)
	a.render(w, r, "signin", page{Title: "Sign in", Bare: true, Data: a.signinData(r, msg)})
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

func (a *App) githubLogin(ctx context.Context, code, redirect string) (string, error) {
	if code == "" {
		return "", errors.New("missing code")
	}
	form := url.Values{
		"client_id": {a.cfg.GitHubClientID}, "client_secret": {a.cfg.GitHubClientSecret},
		"code": {code}, "redirect_uri": {redirect},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := doJSON(req, &tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("token exchange failed: %s", tok.Error)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	var user struct {
		Login string `json:"login"`
	}
	if err := doJSON(req, &user); err != nil {
		return "", err
	}
	return user.Login, nil
}

func doJSON(req *http.Request, v any) error {
	req.Header.Set("User-Agent", "Atelier")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, v)
}
