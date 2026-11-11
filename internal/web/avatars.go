package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"atelier/internal/backup"
	"atelier/internal/store"
	"atelier/internal/tether"
)

// Profile pictures (#15)
//
//	avatars_image.go  checking and re-encoding an image
//	avatars.go        storage, /-/avatar/{owner}/{size}.png, the template
//	                  helpers that draw a picture or the initial, and
//	                  Settings → Profile → Picture (upload, GitHub, remove)
//
// Files live in <data>/avatars/<owner id>/<size>.png and go into every backup.
// Only re-encoded PNGs are ever written there; the uploaded bytes are thrown
// away once decoded. The picture is served under /-/, a literal namespace no
// account can take, by a path handler (a four-segment mux pattern would
// clash with /{owner}/{repo}/commit/{sha}).

const avatarDir = "avatars"

func init() {
	backup.RegisterDataDir(avatarDir)
	registerPathHandler(avatarPathHandler)
	registerTemplateFuncs(avatarTemplateFuncs)
	registerFeature(feature{
		name: "avatars",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("POST /settings/profile/picture", a.authed(a.avatarSubmit))
			m.HandleFunc("POST /settings/profile/links", a.authed(a.profileLinksSubmit))
		},
	})
}

// ---------- storage ----------

func (a *App) avatarPath(ownerID int64, size int) string {
	return filepath.Join(a.cfg.DataDir, avatarDir, strconv.FormatInt(ownerID, 10), strconv.Itoa(size)+".png")
}

// saveAvatar processes b and stores it as the account's picture.
func (a *App) saveAvatar(ctx context.Context, o store.Owner, b []byte, source, login string) error {
	sizes, hash, err := processAvatar(b)
	if err != nil {
		return err
	}
	dir := filepath.Dir(a.avatarPath(o.ID, avatarSizes[0]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, n := range avatarSizes {
		// A hidden temporary name: backups skip dot files, so a half-written
		// picture never lands in a snapshot.
		rb := make([]byte, 6)
		_, _ = rand.Read(rb)
		tmp := filepath.Join(dir, ".tmp-"+hex.EncodeToString(rb))
		if err := os.WriteFile(tmp, sizes[n], 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, a.avatarPath(o.ID, n)); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := a.db.SaveAccountAvatar(ctx, store.AccountAvatar{OwnerID: o.ID, Hash: hash, Source: source, SourceLogin: login}); err != nil {
		return err
	}
	a.avatarIndexDirty()
	return nil
}

func (a *App) removeAvatar(ctx context.Context, o store.Owner) error {
	if err := a.db.DeleteAccountAvatar(ctx, o.ID); err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Dir(a.avatarPath(o.ID, avatarSizes[0])))
	a.avatarIndexDirty()
	return nil
}

// ---------- serving ----------

// avatarPathHandler answers GET /-/avatar/{owner}/{size}.png.
func avatarPathHandler(a *App, w http.ResponseWriter, r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, "/-/avatar/")
	if !ok {
		return false
	}
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	name, file, ok := strings.Cut(rest, "/")
	size, err := strconv.Atoi(strings.TrimSuffix(file, ".png"))
	if !ok || !strings.HasSuffix(file, ".png") || err != nil || !validAvatarSize(size) {
		http.NotFound(w, r)
		return true
	}
	ctx := r.Context()
	who, _ := a.sessionActor(r)
	if who.Kind == "" {
		who = Actor{Kind: ActorAnonymous, Name: "anonymous"}
	}
	o, err := a.db.OwnerByName(ctx, name)
	if err != nil || !a.accountKnownTo(ctx, who, o) {
		http.NotFound(w, r)
		return true
	}
	av, err := a.db.AccountAvatar(ctx, o.ID)
	if err != nil {
		http.NotFound(w, r)
		return true
	}
	etag := `"` + av.Hash + "-" + strconv.Itoa(size) + `"`
	h.Set("ETag", etag)
	h.Set("Cache-Control", "private, max-age=86400")
	if match := r.Header.Get("If-None-Match"); match != "" && (match == etag || match == "W/"+etag || match == "*") {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	f, err := os.Open(a.avatarPath(o.ID, size))
	if err != nil {
		http.NotFound(w, r)
		return true
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return true
	}
	h.Set("Content-Type", "image/png")
	h.Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	h.Set("Content-Disposition", `inline; filename="`+strconv.Itoa(size)+`.png"`)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = io.Copy(w, f)
	}
	return true
}

func validAvatarSize(n int) bool {
	for _, s := range avatarSizes {
		if s == n {
			return true
		}
	}
	return false
}

// ---------- who's who: the index the templates use ----------

type avatarEntry struct {
	Name, Hash string
	Company    bool
}

type avatarIndexState struct {
	mu      sync.Mutex
	at      time.Time
	byName  map[string]avatarEntry // account name, display name, GitHub login (lower case)
	byEmail map[string]avatarEntry
}

var avatarIndexes sync.Map // *App → *avatarIndexState

const avatarIndexTTL = 5 * time.Second

func (a *App) avatarIdx() *avatarIndexState {
	v, _ := avatarIndexes.LoadOrStore(a, &avatarIndexState{})
	return v.(*avatarIndexState)
}

func (a *App) avatarIndexDirty() {
	st := a.avatarIdx()
	st.mu.Lock()
	st.at = time.Time{}
	st.mu.Unlock()
}

// avatarLookup returns the index, rebuilt when it is stale.
func (a *App) avatarLookup() (map[string]avatarEntry, map[string]avatarEntry) {
	st := a.avatarIdx()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.byName != nil && time.Since(st.at) < avatarIndexTTL {
		return st.byName, st.byEmail
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	byName, byEmail := map[string]avatarEntry{}, map[string]avatarEntry{}
	owners, err := a.db.Owners(ctx)
	if err != nil {
		log.Printf("avatars: %v", err)
		return st.byName, st.byEmail
	}
	pics, _ := a.db.AccountAvatars(ctx)
	byID := map[int64]avatarEntry{}
	displays := map[string]int{}
	for _, o := range owners {
		if o.DisplayName != "" {
			displays[strings.ToLower(o.DisplayName)]++
		}
	}
	for _, o := range owners {
		e := avatarEntry{Name: o.Name, Hash: pics[o.ID].Hash, Company: o.IsCompany()}
		byID[o.ID] = e
		byName[strings.ToLower(o.Name)] = e
		byEmail[strings.ToLower(o.Name)+"@atelier.local"] = e
	}
	for _, o := range owners { // display names only where they're unambiguous and not someone's handle
		d := strings.ToLower(o.DisplayName)
		if _, taken := byName[d]; d != "" && displays[d] == 1 && !taken {
			byName[d] = byID[o.ID]
		}
	}
	if accs, err := a.db.TetherAccounts(ctx); err == nil {
		for _, acc := range accs {
			e, ok := byID[acc.PersonID]
			if !ok || !tether.ValidLogin(acc.Login) {
				continue
			}
			l := strings.ToLower(acc.Login)
			byEmail[l+"@users.noreply.github.com"] = e
			byEmail["noreply:"+l] = e // id+login@users.noreply.github.com, see commitAccount
			if _, taken := byName[l]; !taken {
				byName[l] = e
			}
		}
	}
	if gh, err := a.db.AccountGitHubs(ctx); err == nil {
		for id, login := range gh {
			if e, ok := byID[id]; ok {
				if _, taken := byName[strings.ToLower(login)]; !taken {
					byName[strings.ToLower(login)] = e
				}
			}
		}
	}
	if me, err := a.db.PersonalOwner(ctx); err == nil {
		if e, err := a.db.Setting(ctx, settingPullsCommitterEmail); err == nil && e != "" {
			byEmail[strings.ToLower(e)] = byID[me.ID]
		}
	}
	if emails, err := a.db.AllAccountEmails(ctx); err == nil {
		for e, id := range emails {
			if en, ok := byID[id]; ok {
				byEmail[e] = en
			}
		}
	}
	st.byName, st.byEmail, st.at = byName, byEmail, time.Now()
	return byName, byEmail
}

func (a *App) accountByAuthor(name string) (avatarEntry, bool) {
	byName, _ := a.avatarLookup()
	e, ok := byName[strings.ToLower(strings.TrimSpace(name))]
	return e, ok
}

// commitAccount maps a commit author to an account: by email first (the
// account's own commit emails, Atelier's <name>@atelier.local, the merge
// committer email, the GitHub noreply address of a Tether login), then name.
func (a *App) commitAccount(name, email string) (avatarEntry, bool) {
	byName, byEmail := a.avatarLookup()
	email = strings.ToLower(strings.TrimSpace(email))
	if e, ok := byEmail[email]; ok && email != "" {
		return e, true
	}
	if local, ok := strings.CutSuffix(email, "@users.noreply.github.com"); ok {
		if _, login, ok := strings.Cut(local, "+"); ok {
			if e, ok := byEmail["noreply:"+login]; ok {
				return e, true
			}
		}
	}
	e, ok := byName[strings.ToLower(strings.TrimSpace(name))]
	return e, ok
}

func avatarSrc(e avatarEntry, size int) string {
	return "/-/avatar/" + url.PathEscape(e.Name) + "/" + strconv.Itoa(size) + ".png?v=" + url.QueryEscape(e.Hash)
}

// avatarURLFor is the picture of an account name at a stored size, or "".
func (a *App) avatarURLFor(name string, size int) string {
	byName, _ := a.avatarLookup()
	e, ok := byName[strings.ToLower(name)]
	if !ok || e.Hash == "" {
		return ""
	}
	if !validAvatarSize(size) {
		size = avatarSizes[0]
	}
	return avatarSrc(e, size)
}

func firstLetter(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "?"
	}
	r, _ := utf8.DecodeRuneInString(s)
	return strings.ToUpper(string(r))
}

// smallAvatar is the 20px avatar beside an author: their picture, or the
// initial as before.
func smallAvatar(e avatarEntry, ok bool, label string) template.HTML {
	if ok && e.Hash != "" {
		return template.HTML(`<img class="avatar avatar-sm avatar-pic" src="` + template.HTMLEscapeString(avatarSrc(e, 64)) +
			`" alt="" width="20" height="20" loading="lazy" decoding="async">`)
	}
	return template.HTML(`<span class="avatar avatar-sm" aria-hidden="true">` + template.HTMLEscapeString(firstLetter(label)) + `</span>`)
}

func avatarTemplateFuncs(a *App) template.FuncMap {
	return template.FuncMap{
		// avatarURL "zoe" 64 → the picture's URL, or "" (draw the initial).
		"avatarURL": func(name string, size int) string { return a.avatarURLFor(name, size) },
		// personAvatar "zoe" → a small picture or initial for an author name.
		"personAvatar": func(name string) template.HTML {
			e, ok := a.accountByAuthor(name)
			return smallAvatar(e, ok, name)
		},
		// commitAvatar author email → the same, matched by commit email first.
		"commitAvatar": func(name, email string) template.HTML {
			e, ok := a.commitAccount(name, email)
			return smallAvatar(e, ok, name)
		},
	}
}

// ---------- GitHub ----------

// accountGitHub is the GitHub login an account stands for: a person's Tether
// login; for a company, the organisation set in Settings → Profile, or else
// the one most of its Tether-linked repositories live under.
func (a *App) accountGitHub(ctx context.Context, o store.Owner) string {
	if !o.IsCompany() {
		if acc, err := a.db.TetherAccount(ctx, o.ID); err == nil && tether.ValidLogin(acc.Login) {
			return acc.Login
		}
		return ""
	}
	if login, err := a.db.AccountGitHub(ctx, o.ID); err == nil && login != "" {
		return login
	}
	return a.companyGitHubFromLinks(ctx, o)
}

func (a *App) companyGitHubFromLinks(ctx context.Context, o store.Owner) string {
	links, err := a.db.TetherLinks(ctx)
	if err != nil || len(links) == 0 {
		return ""
	}
	repos, err := a.db.ReposByOwner(ctx, o.ID)
	if err != nil {
		return ""
	}
	mine := map[int64]bool{}
	for _, rp := range repos {
		mine[rp.ID] = true
	}
	people := map[string]bool{}
	if accs, err := a.db.TetherAccounts(ctx); err == nil {
		for _, acc := range accs {
			people[strings.ToLower(acc.Login)] = true
		}
	}
	count := map[string]int{}
	best, bestN := "", 0
	for _, l := range links {
		if !mine[l.RepoID] {
			continue
		}
		owner, _, ok := strings.Cut(l.GHRepo, "/")
		if !ok || !tether.ValidLogin(owner) || people[strings.ToLower(owner)] {
			continue
		}
		count[owner]++
		if count[owner] > bestN || (count[owner] == bestN && owner < best) {
			best, bestN = owner, count[owner]
		}
	}
	return best
}

// githubAvatarURL works out the avatar address of login on GitHub with the
// signed-in person's Tether connection.
func (a *App) githubAvatarURL(ctx context.Context, who Actor, o store.Owner, login string) (string, error) {
	c, acc, err := a.tetherClient(ctx, who.PersonID)
	if err != nil {
		return "", errors.New("Connect GitHub in Settings → Tether first.")
	}
	u := ""
	if !o.IsCompany() && strings.EqualFold(acc.Login, login) {
		u = tetherAvatar(acc.AvatarURL)
	}
	if u == "" {
		p, err := c.Profile(ctx, login)
		if err != nil {
			return "", fmt.Errorf("GitHub didn't answer for %s: %s", login, tetherErrText(err))
		}
		u = tetherAvatar(p.AvatarURL)
	}
	if u == "" {
		return "", errors.New("GitHub has no avatar for " + login + ".")
	}
	return u, nil
}

// fetchGitHubAvatar downloads an avatar from GitHub's avatar host only,
// through the image proxy's guarded client (public addresses only, no
// redirects to other hosts), with a size cap.
func (a *App) fetchGitHubAvatar(ctx context.Context, raw string) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "avatars.githubusercontent.com") || u.User != nil {
		return nil, errors.New("That isn't a GitHub avatar address.")
	}
	q := u.Query()
	q.Set("s", "460")
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(ctx, ghImgTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Atelier-Avatar")
	req.Header.Set("Accept", "image/png,image/jpeg,image/webp")
	resp, err := a.gh().img.client().Do(req)
	if err != nil {
		return nil, errors.New("GitHub's avatar host couldn't be reached.")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub's avatar host answered %d.", resp.StatusCode)
	}
	if resp.ContentLength > avatarMaxBytes {
		return nil, errAvatarTooBig
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, avatarMaxBytes+1))
	if err != nil {
		return nil, errors.New("The avatar couldn't be downloaded.")
	}
	if len(b) > avatarMaxBytes {
		return nil, errAvatarTooBig
	}
	return b, nil
}

// ---------- Settings → Profile → Picture ----------

func (a *App) avatarSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, avatarMaxBytes+64<<10)
	if err := r.ParseMultipartForm(1 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			a.avatarBack(w, r, r.URL.Query().Get("account"), errAvatarTooBig.Error())
			return
		}
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	o, ok := a.editAccount(w, r, r.FormValue("account"))
	if !ok {
		return
	}
	who := a.actor(r)
	var err error
	switch r.FormValue("action") {
	case "remove":
		err = a.removeAvatar(ctx, o)
	case "github":
		login := a.accountGitHub(ctx, o)
		if login == "" {
			err = errors.New("This account isn't linked to GitHub yet.")
			break
		}
		var u string
		if u, err = a.githubAvatarURL(ctx, who, o, login); err != nil {
			break
		}
		var b []byte
		if b, err = a.fetchGitHubAvatar(ctx, u); err != nil {
			break
		}
		err = a.saveAvatar(ctx, o, b, store.AvatarGitHub, login)
	default:
		f, _, ferr := r.FormFile("picture")
		if ferr != nil {
			err = errors.New("Choose a picture to upload.")
			break
		}
		b, rerr := io.ReadAll(io.LimitReader(f, avatarMaxBytes+1))
		f.Close()
		if rerr != nil {
			err = errAvatarBroken
			break
		}
		err = a.saveAvatar(ctx, o, b, store.AvatarUpload, "")
	}
	if err != nil {
		a.avatarBack(w, r, o.Name, err.Error())
		return
	}
	http.Redirect(w, r, "/settings/profile?account="+url.QueryEscape(o.Name)+"&picture=1#picture", http.StatusSeeOther)
}

// avatarBack shows the edit page again with a message about the picture.
func (a *App) avatarBack(w http.ResponseWriter, r *http.Request, account, msg string) {
	o, ok := a.editAccount(w, r, account)
	if !ok {
		return
	}
	prof, err := a.db.Profile(r.Context(), o.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v := profileEditView{DisplayName: o.DisplayName, Bio: prof.Bio, Location: prof.Location, Website: prof.Website,
		Pronouns: prof.Pronouns, AvatarStyle: prof.Avatar, PictureError: msg}
	a.renderProfileEdit(w, r, o, prof, v, "", "", nil)
}

// profileLinksSubmit saves a company's GitHub organisation and an account's
// commit emails.
func (a *App) profileLinksSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	o, ok := a.editAccount(w, r, r.FormValue("account"))
	if !ok {
		return
	}
	back := func(msg string) {
		prof, err := a.db.Profile(ctx, o.ID)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		v := profileEditView{DisplayName: o.DisplayName, Bio: prof.Bio, Location: prof.Location, Website: prof.Website,
			Pronouns: prof.Pronouns, AvatarStyle: prof.Avatar, LinksError: msg,
			GitHubOrg: r.FormValue("github"), EmailsText: r.FormValue("emails")}
		a.renderProfileEdit(w, r, o, prof, v, "", "", nil)
	}
	if o.IsCompany() {
		login := strings.TrimPrefix(strings.TrimSpace(r.FormValue("github")), "@")
		if login != "" && !tether.ValidLogin(login) {
			back("That isn't a GitHub organisation name. Use the name from its address, like Elyndra-Studios.")
			return
		}
		if err := a.db.SetAccountGitHub(ctx, o.ID, login); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	var emails []string
	for _, f := range strings.FieldsFunc(r.FormValue("emails"), func(c rune) bool { return c == '\n' || c == ',' || c == ' ' || c == '\r' || c == '\t' }) {
		if !strings.Contains(f, "@") || len(f) > 200 || strings.ContainsAny(f, "<>\"'") {
			back(fmt.Sprintf("%q doesn't look like an email address.", f))
			return
		}
		emails = append(emails, f)
	}
	if len(emails) > 20 {
		back("Up to 20 commit emails, please.")
		return
	}
	if err := a.db.SetAccountEmails(ctx, o.ID, emails); err != nil {
		if errors.Is(err, store.ErrEmailTaken) {
			back("One of those emails already belongs to another account.")
			return
		}
		a.fail(w, r, err)
		return
	}
	a.avatarIndexDirty()
	http.Redirect(w, r, "/settings/profile?account="+url.QueryEscape(o.Name)+"&saved=1#links", http.StatusSeeOther)
}

// fillPictureAndLinks adds the picture and GitHub sections of the edit page.
func (a *App) fillPictureAndLinks(ctx context.Context, who Actor, o store.Owner, v *profileEditView) {
	if pic, err := a.db.AccountAvatar(ctx, o.ID); err == nil {
		v.Picture, v.HasPicture = pic, true
	}
	if acc, err := a.db.TetherAccount(ctx, who.PersonID); err == nil {
		v.TetherOn, v.TetherLogin = true, acc.Login
	}
	if !o.IsCompany() {
		v.SignInGitHub = a.cfg.GitHubLogin
	}
	v.GitHub = a.accountGitHub(ctx, o)
	if o.IsCompany() {
		explicit, _ := a.db.AccountGitHub(ctx, o.ID)
		if v.GitHubOrg == "" && v.LinksError == "" {
			v.GitHubOrg = explicit
		}
		v.GitHubFromLinks = explicit == "" && v.GitHub != ""
	}
	v.Emails, _ = a.db.AccountEmails(ctx, o.ID)
	if v.EmailsText == "" && v.LinksError == "" {
		v.EmailsText = strings.Join(v.Emails, "\n")
	}
	v.AtelierEmail = strings.ToLower(o.Name) + "@atelier.local"
}

// profileGitHub is the GitHub login a profile shows. A person's Tether login
// and a company's chosen organisation are shown to everyone who can see the
// profile; an organisation only guessed from linked repositories (which may
// be private) is shown to people who can edit the account.
func (a *App) profileGitHub(ctx context.Context, who Actor, o store.Owner, canEdit bool) string {
	if canEdit || !o.IsCompany() {
		return a.accountGitHub(ctx, o)
	}
	login, _ := a.db.AccountGitHub(ctx, o.ID)
	return login
}
