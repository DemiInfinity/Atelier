package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// Profiles: the page at /{owner} for people and company accounts (avatar,
// bio, profile README, pinned repositories, Embers and the repository list),
// and the editor at /settings/profile.

func init() {
	registerFeature(feature{
		name: "profiles",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /settings/profile", a.authed(a.profileEditPage))
			m.HandleFunc("POST /settings/profile", a.authed(a.profileEditSubmit))
			m.HandleFunc("POST /settings/profile/pins", a.authed(a.profilePinsSubmit))
		},
	})
}

// ---------- who may edit an account ----------

// AccountEditPolicy may decide whether who may edit an account's profile and
// pins. Return decided=false to leave it to the next policy (and finally the
// core rule). The Team / Company addons register policies here (e.g. company
// admins) rather than adding checks around the codebase.
type AccountEditPolicy func(ctx context.Context, a *App, who Actor, account store.Owner) (allowed, decided bool)

var accountEditPolicies []AccountEditPolicy

func registerAccountEditPolicy(p AccountEditPolicy) {
	accountEditPolicies = append(accountEditPolicies, p)
}

// canEditAccount reports whether who may change account's profile and pins.
//
// Core rule: only a signed-in person (never a token, bot or anonymous visitor).
// A person may edit their own profile; company accounts are the personal
// owner's in core, so only the personal owner may edit them. The Team /
// Company addons will extend this (company admins, org permissions) through
// registerAccountEditPolicy.
func (a *App) canEditAccount(ctx context.Context, who Actor, account store.Owner) bool {
	for _, p := range accountEditPolicies {
		if allowed, decided := p(ctx, a, who, account); decided {
			return allowed
		}
	}
	if who.Kind != ActorOwner || who.PersonID == 0 {
		return false
	}
	if account.IsCompany() {
		return a.isOwnerPerson(ctx, who.PersonID)
	}
	return who.PersonID == account.ID
}

// ---------- helpers ----------

const (
	maxDisplayName = 80
	maxBio         = 280
	maxLocation    = 80
	maxPronouns    = 40
	maxWebsite     = 200
)

var errBadWebsite = errors.New("website must be an http or https address")

// sanitizeWebsite turns what someone typed into a safe link: http or https
// only, a real host, no embedded credentials. A bare "example.com" becomes
// "https://example.com". An empty string stays empty.
func sanitizeWebsite(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > maxWebsite || strings.ContainsAny(raw, " \t\r\n<>\"'`\\") {
		return "", errBadWebsite
	}
	if !strings.Contains(raw, "://") {
		if i := strings.IndexByte(raw, ':'); i >= 0 && !strings.ContainsAny(raw[:i], "./") {
			return "", errBadWebsite // "javascript:…", "mailto:…" and friends
		}
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errBadWebsite
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.User != nil || u.Opaque != "" {
		return "", errBadWebsite
	}
	host := u.Hostname()
	if host == "" || strings.HasPrefix(host, ".") || strings.HasSuffix(host, "-") {
		return "", errBadWebsite
	}
	u.Scheme = scheme
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

// websiteLabel is the link text: the address without its scheme.
func websiteLabel(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	return strings.TrimSuffix(s, "/")
}

// avatarStyles are the choices on the edit page. "" is the default.
var avatarStyles = []struct{ Value, Label string }{
	{"", "Initial"},
	{"initials", "Two initials"},
	{"spark", "Spark mark"},
}

func validAvatar(v string) bool {
	for _, s := range avatarStyles {
		if s.Value == v {
			return true
		}
	}
	return false
}

// avatarView is what the avatar partial draws.
type avatarView struct {
	Text    string
	Spark   bool
	Company bool
	Image   string // the profile picture's URL (avatars.go), when there is one
	Image2x string
}

func profileAvatar(o store.Owner, p store.Profile) avatarView {
	v := avatarView{Text: o.Initial(), Company: o.IsCompany()}
	switch p.Avatar {
	case "spark":
		v.Spark = true
	case "initials":
		v.Text = initialsOf(o.Label())
	}
	return v
}

// initialsOf returns up to two capital letters from the first two words.
func initialsOf(s string) string {
	var out []rune
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '-' || r == '_' || r == '.' }) {
		r, _ := utf8.DecodeRuneInString(w)
		out = append(out, []rune(strings.ToUpper(string(r)))...)
		if len(out) >= 2 {
			break
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

// countLabel is "1 repository" / "3 repositories".
func countLabel(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func tooLong(s string, n int) bool { return utf8.RuneCountInString(s) > n }

// readableRepos keeps the repositories who may read (private ones vanish for
// everyone else).
func (a *App) readableRepos(ctx context.Context, who Actor, repos []store.Repo) []store.Repo {
	out := make([]store.Repo, 0, len(repos))
	for _, rp := range repos {
		if a.can(ctx, who, rp, PermRead) {
			out = append(out, rp)
		}
	}
	return out
}

// profileRepoNames are the repositories whose README becomes the profile
// README, in order: one named exactly like the account (GitHub's convention,
// e.g. demi/demi) and ".profile" (Gitea / Forgejo's, e.g. elyndra/.profile).
func profileRepoNames(account string) []string { return []string{account, ".profile"} }

// profileReadme finds the profile README the viewer may read. repo is the
// first profile repository found (even without a README), so the page can
// point at it; readme is nil when there's nothing to show.
func (a *App) profileReadme(ctx context.Context, who Actor, o store.Owner) (repo *store.Repo, readme []byte) {
	for _, name := range profileRepoNames(o.Name) {
		rp, err := a.db.RepoByName(ctx, o.Name, name)
		if err != nil || !strings.EqualFold(rp.Name, name) || !a.can(ctx, who, rp, PermRead) {
			continue
		}
		if repo == nil {
			found := rp
			repo = &found
		}
		if b := a.readmeAtRoot(ctx, rp); b != nil {
			found := rp
			return &found, b
		}
	}
	return repo, nil
}

// readmeAtRoot returns README.md (any case, or README.markdown) from the top
// of the default branch.
func (a *App) readmeAtRoot(ctx context.Context, rp store.Repo) []byte {
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil || g.IsEmpty(ctx) {
		return nil
	}
	commit, err := g.ResolveCommit(ctx, g.DefaultBranch(ctx))
	if err != nil {
		return nil
	}
	entries, err := g.Tree(ctx, commit, "")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		n := strings.ToLower(e.Name)
		if e.IsDir() || (n != "readme.md" && n != "readme.markdown") {
			continue
		}
		b, err := g.Blob(ctx, commit, e.Path, gitrepo.MaxBlob)
		if err != nil || b.Binary || b.TooLarge {
			return nil
		}
		return b.Data
	}
	return nil
}

// profilePins returns the readable pinned repositories, or, when there are
// none, the most recently updated readable ones (fallback=true). The profile
// README repository is left out of the fallback.
func (a *App) profilePins(ctx context.Context, who Actor, o store.Owner, readable []store.Repo, readmeRepo *store.Repo) (pins []store.Repo, fallback bool, err error) {
	pinned, err := a.db.PinnedRepos(ctx, o.ID)
	if err != nil {
		return nil, false, err
	}
	pins = a.readableRepos(ctx, who, pinned)
	if len(pins) > 0 {
		return pins, false, nil
	}
	for _, rp := range readable { // already newest first
		if readmeRepo != nil && rp.ID == readmeRepo.ID {
			continue
		}
		pins = append(pins, rp)
		if len(pins) == store.MaxPins {
			break
		}
	}
	return pins, true, nil
}

var repoSorts = []struct{ Value, Label string }{
	{"updated", "Last updated"},
	{"name", "Name"},
	{"created", "Newest"},
}

// filterRepos applies the profile page's search box and sort.
func filterRepos(repos []store.Repo, q, by string) []store.Repo {
	q = strings.ToLower(strings.TrimSpace(q))
	out := make([]store.Repo, 0, len(repos))
	for _, rp := range repos {
		if q == "" || strings.Contains(strings.ToLower(rp.Name), q) || strings.Contains(strings.ToLower(rp.Description), q) {
			out = append(out, rp)
		}
	}
	switch by {
	case "name":
		sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	case "created":
		sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	default:
		sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	}
	return out
}

// ---------- profile page ----------

type profileView struct {
	Owner        store.Owner
	Profile      store.Profile
	Avatar       avatarView
	Website      string
	WebsiteLabel string
	CanEdit      bool
	EditURL      string

	Readme     []byte
	ReadmeRepo *store.Repo
	ReadmeHint string // repository name to create for a profile README

	Pinned    []store.Repo
	PinnedIDs map[int64]bool
	Fallback  bool

	Repos      []store.Repo
	RepoCount  int
	Query      string
	Sort       string
	Sorts      any
	Embers     []emberCell
	EmberTotal int

	RepoCountLabel, EmberLabel, ResultLabel string

	Tab      string // "" (overview) or "repositories"
	AcctTabs acctTabs
	GitHub   string // the GitHub login this account is linked to
	GitHubGH string // and its github.com address
}

// acctTabs is what the account-tabs partial needs.
type acctTabs struct {
	Accounts []store.Owner
	Current  store.Owner
}

// showProfile renders /{owner}. Called from ownerPage.
func (a *App) showProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := a.actor(r)
	o, err := a.db.OwnerByName(ctx, r.PathValue("owner"))
	if ownerNotFound(err) {
		a.ownerMissing(w, r, r.PathValue("owner"))
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if o.Name != r.PathValue("owner") { // /ZOE → /zoe: one address per account
		target := "/" + url.PathEscape(o.Name)
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	prof, err := a.db.Profile(ctx, o.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	all, err := a.db.ReposByOwner(ctx, o.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	readable := a.readableRepos(ctx, who, all)
	readmeRepo, readme := a.profileReadme(ctx, who, o)
	pins, fallback, err := a.profilePins(ctx, who, o, readable, readmeRepo)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	by := q.Get("sort")
	if by != "name" && by != "created" {
		by = "updated"
	}
	v := profileView{
		Owner: o, Profile: prof, Avatar: a.pictureAvatar(o, prof),
		CanEdit: a.canEditAccount(ctx, who, o), EditURL: "/settings/profile?account=" + url.QueryEscape(o.Name),
		Readme: readme, ReadmeRepo: readmeRepo, ReadmeHint: o.Name,
		Pinned: pins, Fallback: fallback, PinnedIDs: map[int64]bool{},
		Repos: filterRepos(readable, q.Get("q"), by), RepoCount: len(readable),
		Query: q.Get("q"), Sort: by, Sorts: repoSorts,
	}
	if !fallback {
		for _, p := range pins {
			v.PinnedIDs[p.ID] = true
		}
	}
	if site, err := sanitizeWebsite(prof.Website); err == nil && site != "" {
		v.Website, v.WebsiteLabel = site, websiteLabel(site)
	}
	v.Embers, v.EmberTotal = a.embers(ctx, readable)
	v.RepoCountLabel = countLabel(len(readable), "repository", "repositories")
	v.EmberLabel = countLabel(v.EmberTotal, "commit", "commits")
	v.ResultLabel = countLabel(len(v.Repos), "result", "results")
	if q.Get("tab") == "repositories" {
		v.Tab = "repositories"
	}
	v.AcctTabs = acctTabs{Accounts: a.switchAccounts(ctx, who), Current: o}
	if gh := a.profileGitHub(ctx, who, o, v.CanEdit); gh != "" {
		v.GitHub, v.GitHubGH = gh, a.ghWebURL(gh)
	}
	a.render(w, r, "owner", page{Title: o.Label(), Nav: a.navFor(ctx, who, o), Data: v})
}

// navFor highlights "Repositories" in the top bar on your own profile and on
// the company accounts you can switch to.
func (a *App) navFor(ctx context.Context, who Actor, o store.Owner) string {
	if who.Kind == ActorOwner && (who.PersonID == o.ID || a.canEditAccount(ctx, who, o)) {
		return "repos"
	}
	return ""
}

// pictureAvatar is profileAvatar plus the account's picture, if it has one.
func (a *App) pictureAvatar(o store.Owner, p store.Profile) avatarView {
	v := profileAvatar(o, p)
	v.Image, v.Image2x = a.avatarURLFor(o.Name, 160), a.avatarURLFor(o.Name, 460)
	return v
}

// ---------- edit page ----------

type pinRow struct {
	Repo   store.Repo
	Pinned bool
	First  bool
	Last   bool
}

type profileEditView struct {
	Account     store.Owner
	Accounts    []store.Owner
	Avatar      avatarView
	DisplayName string
	Bio         string
	Location    string
	Website     string
	Pronouns    string
	AvatarStyle string
	Styles      any
	Pins        []pinRow
	PinCount    int
	MaxPins     int
	MaxBio      int
	ProfileURL  string
	PinsError   string

	// Picture and links (avatars.go).
	PictureError    string
	LinksError      string
	Picture         store.AccountAvatar
	HasPicture      bool
	GitHub          string // the GitHub login the account is linked to
	GitHubFromLinks bool   // a company's organisation guessed from its linked repositories
	GitHubOrg       string // a company's explicit organisation (the form field)
	TetherOn        bool   // the signed-in person has Tether
	TetherLogin     string
	SignInGitHub    string // "Continue with GitHub" is locked to this login
	Emails          []string
	EmailsText      string
	AtelierEmail    string
}

// editAccount works out which account /settings/profile is editing:
// ?account=name, or the signed-in person's own account.
func (a *App) editAccount(w http.ResponseWriter, r *http.Request, name string) (store.Owner, bool) {
	ctx := r.Context()
	who := a.actor(r)
	var o store.Owner
	var err error
	if name != "" {
		o, err = a.db.OwnerByName(ctx, name)
	} else {
		o, err = a.db.Person(ctx, who.PersonID)
	}
	if err != nil {
		a.fail(w, r, err)
		return store.Owner{}, false
	}
	if !a.canEditAccount(ctx, who, o) {
		a.forbidden(w, r)
		return store.Owner{}, false
	}
	return o, true
}

func (a *App) profileEditPage(w http.ResponseWriter, r *http.Request) {
	o, ok := a.editAccount(w, r, r.URL.Query().Get("account"))
	if !ok {
		return
	}
	prof, err := a.db.Profile(r.Context(), o.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v := profileEditView{
		DisplayName: o.DisplayName, Bio: prof.Bio, Location: prof.Location, Website: prof.Website,
		Pronouns: prof.Pronouns, AvatarStyle: prof.Avatar,
	}
	notice := ""
	if q := r.URL.Query(); q.Get("saved") != "" || q.Get("pins") != "" || q.Get("picture") != "" {
		notice = "1"
	}
	a.renderProfileEdit(w, r, o, prof, v, "", notice, nil)
}

// renderProfileEdit fills in everything the edit page needs around the form
// values in v. order, when set, is the pin order to show (after a failed save).
func (a *App) renderProfileEdit(w http.ResponseWriter, r *http.Request, o store.Owner, prof store.Profile, v profileEditView, errMsg, notice string, order []int64) {
	ctx := r.Context()
	who := a.actor(r)
	owners, err := a.db.Owners(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	for _, x := range owners {
		if a.canEditAccount(ctx, who, x) {
			v.Accounts = append(v.Accounts, x)
		}
	}
	all, err := a.db.ReposByOwner(ctx, o.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	readable := a.readableRepos(ctx, who, all)
	if order == nil {
		pinned, err := a.db.PinnedRepos(ctx, o.ID)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		for _, p := range pinned {
			order = append(order, p.ID)
		}
	}
	v.Pins = pinRows(readable, order)
	for _, p := range v.Pins {
		if p.Pinned {
			v.PinCount++
		}
	}
	v.Account, v.Styles, v.MaxPins, v.MaxBio = o, avatarStyles, store.MaxPins, maxBio
	v.ProfileURL = "/" + o.Name
	shown := prof
	shown.Avatar = v.AvatarStyle
	v.Avatar = a.pictureAvatar(o, shown)
	a.fillPictureAndLinks(ctx, who, o, &v)
	if errMsg != "" || v.PinsError != "" || v.PictureError != "" || v.LinksError != "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	a.render(w, r, "profiles_edit", page{Title: "Profile · " + o.Label(), Nav: "tokens", Error: errMsg, Notice: notice, Data: v})
}

// pinRows lists the pinned repositories first (in order), then the rest.
func pinRows(readable []store.Repo, order []int64) []pinRow {
	byID := map[int64]store.Repo{}
	for _, rp := range readable {
		byID[rp.ID] = rp
	}
	var rows []pinRow
	used := map[int64]bool{}
	for _, id := range order {
		if rp, ok := byID[id]; ok && !used[id] {
			used[id] = true
			rows = append(rows, pinRow{Repo: rp, Pinned: true})
		}
	}
	n := len(rows)
	for i := range rows {
		rows[i].First, rows[i].Last = i == 0, i == n-1
	}
	for _, rp := range readable {
		if !used[rp.ID] {
			rows = append(rows, pinRow{Repo: rp})
		}
	}
	return rows
}

func (a *App) profileEditSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	o, ok := a.editAccount(w, r, r.FormValue("account"))
	if !ok {
		return
	}
	prof, err := a.db.Profile(ctx, o.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v := profileEditView{
		DisplayName: strings.TrimSpace(r.FormValue("display_name")),
		Bio:         strings.TrimSpace(strings.ReplaceAll(r.FormValue("bio"), "\r\n", "\n")),
		Location:    strings.TrimSpace(r.FormValue("location")),
		Website:     strings.TrimSpace(r.FormValue("website")),
		Pronouns:    strings.TrimSpace(r.FormValue("pronouns")),
		AvatarStyle: r.FormValue("avatar"),
	}
	bad := func(msg string) { a.renderProfileEdit(w, r, o, prof, v, msg, "", nil) }
	switch {
	case tooLong(v.DisplayName, maxDisplayName):
		bad("Keep the name to " + strconv.Itoa(maxDisplayName) + " characters.")
		return
	case tooLong(v.Bio, maxBio):
		bad("Keep the bio to " + strconv.Itoa(maxBio) + " characters.")
		return
	case tooLong(v.Location, maxLocation):
		bad("Keep the location to " + strconv.Itoa(maxLocation) + " characters.")
		return
	case tooLong(v.Pronouns, maxPronouns):
		bad("Keep pronouns to " + strconv.Itoa(maxPronouns) + " characters.")
		return
	case !validAvatar(v.AvatarStyle):
		bad("Pick one of the avatar styles.")
		return
	}
	site, err := sanitizeWebsite(v.Website)
	if err != nil {
		bad("The website needs to be an http:// or https:// address, like https://example.com.")
		return
	}
	who := a.actor(r)
	next := store.Profile{
		OwnerID: o.ID, Bio: v.Bio, Location: v.Location, Website: site, Pronouns: v.Pronouns, Avatar: v.AvatarStyle,
		UpdatedBy: who.Name, UpdatedByKind: who.Kind, UpdatedByPerson: who.PersonID,
	}
	if err := a.db.SaveProfile(ctx, next); err != nil {
		a.fail(w, r, err)
		return
	}
	if v.DisplayName != o.DisplayName {
		if err := a.db.SetDisplayName(ctx, o.ID, v.DisplayName); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	http.Redirect(w, r, "/settings/profile?account="+url.QueryEscape(o.Name)+"&saved=1", http.StatusSeeOther)
}

// applyPinMove moves one id up or down within ids. move is "up:ID" or "down:ID".
func applyPinMove(ids []int64, move string) []int64 {
	dir, idStr, ok := strings.Cut(move, ":")
	if !ok {
		return ids
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return ids
	}
	for i, x := range ids {
		if x != id {
			continue
		}
		switch {
		case dir == "up" && i > 0:
			ids[i-1], ids[i] = ids[i], ids[i-1]
		case dir == "down" && i < len(ids)-1:
			ids[i+1], ids[i] = ids[i], ids[i+1]
		}
		break
	}
	return ids
}

// profilePinsSubmit saves the pin picker. The checked boxes arrive in page
// order (pinned first, in their current order), and the up / down buttons are
// submit buttons, so reordering works without JavaScript.
func (a *App) profilePinsSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	o, ok := a.editAccount(w, r, r.FormValue("account"))
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		a.fail(w, r, err)
		return
	}
	who := a.actor(r)
	all, err := a.db.ReposByOwner(ctx, o.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	allowed := map[int64]bool{}
	for _, rp := range a.readableRepos(ctx, who, all) {
		allowed[rp.ID] = true
	}
	var ids []int64
	seen := map[int64]bool{}
	for _, s := range r.PostForm["pin"] {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || !allowed[id] || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	ids = applyPinMove(ids, r.PostFormValue("move"))

	if len(ids) > store.MaxPins {
		prof, err := a.db.Profile(ctx, o.ID)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		v := profileEditView{
			DisplayName: o.DisplayName, Bio: prof.Bio, Location: prof.Location, Website: prof.Website,
			Pronouns: prof.Pronouns, AvatarStyle: prof.Avatar,
			PinsError: "You can pin up to " + strconv.Itoa(store.MaxPins) + " repositories. Untick " +
				strconv.Itoa(len(ids)-store.MaxPins) + " and save again.",
		}
		a.renderProfileEdit(w, r, o, prof, v, "", "", ids)
		return
	}
	if err := a.db.SetPinnedRepos(ctx, o.ID, ids); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/profile?account="+url.QueryEscape(o.Name)+"&pins=1#pins", http.StatusSeeOther)
}
