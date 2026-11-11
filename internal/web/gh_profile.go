package web

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"atelier/internal/tether"
)

// /gh/{login}: a GitHub person or organisation. People who opted in to the
// Atelier badge get the amber mark and "Visit their studio".

const (
	ghPinnedTTL  = 10 * time.Minute
	ghReposPage  = 30
	ghPinnedSize = 6
)

type ghRepoCard struct {
	Full, Name, Desc, Lang, URL, GH string
	Stars, Forks                    int
	Private, Fork, Archived         bool
	Updated                         time.Time
}

type ghProfileData struct {
	ghBase
	P          tether.Profile
	Person     tetherPerson
	Name       string
	Bio        string
	Company    string
	Location   string
	Blog       string
	BlogLabel  string
	Twitter    string
	Avatar     string
	GH         string
	Org        bool
	Self       bool
	Atelier    bool
	Studio     string
	Pinned     []ghRepoCard
	PinnedKind string // "pinned" or "popular"
	Repos      []ghRepoCard
	Q, Sort    string
	Sorts      []tetherLangChip
	Pager      ghPager
	ReposErr   string
	Readme     template.HTML
	ReadmeFrom string
}

type ghPinnedCache struct {
	Kind  string        `json:"kind"`
	Repos []tether.Repo `json:"repos"`
}

var ghTwitterRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,15}$`)

func (a *App) ghCard(rp tether.Repo) (ghRepoCard, bool) {
	if !tether.ValidRepo(rp.FullName) {
		return ghRepoCard{}, false
	}
	return ghRepoCard{Full: rp.FullName, Name: rp.Name, Desc: eventExcerpt(rp.Description, 200), Lang: eventExcerpt(rp.Language, 30),
		URL: ghLocalURL(rp.FullName), GH: a.ghWebURL(rp.FullName), Stars: rp.Stars, Forks: rp.Forks, Private: rp.Private,
		Fork: rp.Fork, Archived: rp.Archived, Updated: rp.PushedAt}, true
}

func (a *App) ghProfile(w http.ResponseWriter, r *http.Request, g *ghReq) {
	ctx := r.Context()
	login := r.PathValue("login")
	if !tether.ValidLogin(login) {
		a.notFound(w, r)
		return
	}
	p, err := g.c.Profile(ctx, login)
	if err != nil {
		a.ghFail(w, r, g, err, a.ghWebURL(login))
		return
	}
	if tether.ValidLogin(p.Login) && p.Login != login && strings.EqualFold(p.Login, login) {
		http.Redirect(w, r, ghLocalURL(p.Login), http.StatusSeeOther)
		return
	}
	login = p.Login
	if !tether.ValidLogin(login) {
		login = r.PathValue("login")
	}
	d := &ghProfileData{P: p, Name: eventExcerpt(p.Name, 100), Company: eventExcerpt(p.Company, 100),
		Location: eventExcerpt(p.Location, 100), Avatar: tetherAvatar(p.AvatarURL), GH: a.ghWebURL(login), Org: p.IsOrg(),
		Self: strings.EqualFold(login, g.acc.Login)}
	d.Bio = eventExcerpt(p.Bio, 400)
	if d.Org && d.Bio == "" {
		d.Bio = eventExcerpt(p.Description, 400)
	}
	if blog := strings.TrimSpace(p.Blog); blog != "" {
		if !strings.Contains(blog, "://") {
			blog = "https://" + blog
		}
		if safe := tetherSafeStudio(blog); safe != "" {
			d.Blog = safe
			d.BlogLabel = eventExcerpt(strings.TrimPrefix(strings.TrimPrefix(safe, "https://"), "http://"), 60)
		}
	}
	if ghTwitterRe.MatchString(p.TwitterUsername) {
		d.Twitter = p.TwitterUsername
	}
	// The badge: checked now if it isn't known yet.
	b, present := a.tetherBadgeNow(ctx, g.c, login)
	d.Atelier, d.Studio = present, ""
	if present {
		d.Studio = b.Studio
	}
	d.Person = tetherPerson{Login: login, Avatar: d.Avatar, URL: ghLocalURL(login), GH: d.GH, Atelier: d.Atelier, Studio: d.Studio}

	a.ghProfilePinned(r, g, login, d)
	a.ghProfileRepos(r, g, login, d)
	a.ghProfileReadme(r, g, login, d)
	d.ghBase = g.base()
	title := login
	if d.Name != "" {
		title = d.Name + " (" + login + ")"
	}
	a.render(w, r, "gh_profile", page{Title: title, Data: d})
}

// ghProfilePinned: GraphQL pinnedItems, cached; most-starred repositories
// when GraphQL isn't available.
func (a *App) ghProfilePinned(r *http.Request, g *ghReq, login string, d *ghProfileData) {
	ctx := r.Context()
	key := "gh:pinned:" + strings.ToLower(login)
	var cached ghPinnedCache
	_, body, at, cerr := a.db.TetherCacheGet(ctx, g.who.PersonID, key)
	have := cerr == nil && json.Unmarshal(body, &cached) == nil
	use := func(pc ghPinnedCache) {
		d.PinnedKind = pc.Kind
		for _, rp := range pc.Repos {
			if c, ok := a.ghCard(rp); ok {
				d.Pinned = append(d.Pinned, c)
			}
		}
	}
	if have && time.Since(at) < ghPinnedTTL {
		use(cached)
		return
	}
	pc := ghPinnedCache{Kind: "pinned"}
	repos, err := g.c.Pinned(ctx, login)
	if err != nil {
		if _, blocked := g.c.BlockedUntil(); blocked && have {
			g.noteStale(at, err)
			use(cached)
			return
		}
		q := "user:" + login + " fork:true"
		if d.Org {
			q = "org:" + login + " fork:true"
		}
		repos, _, err = g.c.SearchReposPage(ctx, q, "stars", 1, ghPinnedSize)
		pc.Kind = "popular"
		if err != nil {
			if have {
				use(cached)
			}
			return
		}
	}
	if len(repos) > ghPinnedSize {
		repos = repos[:ghPinnedSize]
	}
	pc.Repos = repos
	if b, err := json.Marshal(pc); err == nil {
		_ = a.db.TetherCachePut(ctx, g.who.PersonID, key, "", b)
	}
	use(pc)
}

// ghProfileRepos is the searchable, sortable, paged repository list.
func (a *App) ghProfileRepos(r *http.Request, g *ghReq, login string, d *ghProfileData) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > 100 {
		q = q[:100]
	}
	sort := r.URL.Query().Get("sort")
	if sort != "stars" && sort != "name" {
		sort = "updated"
	}
	d.Q, d.Sort = q, sort
	for _, s := range []struct{ key, label string }{{"updated", "Last updated"}, {"stars", "Stars"}, {"name", "Name"}} {
		v := url.Values{}
		if q != "" {
			v.Set("q", q)
		}
		if s.key != "updated" {
			v.Set("sort", s.key)
		}
		u := ghLocalURL(login)
		if len(v) > 0 {
			u += "?" + v.Encode()
		}
		d.Sorts = append(d.Sorts, tetherLangChip{Label: s.label, URL: u + "#repositories", Active: s.key == sort})
	}
	page := ghPageNum(r)
	var repos []tether.Repo
	var more bool
	var err error
	if q != "" || sort == "stars" {
		// Search: needed for a query or for sorting by stars.
		sq := "user:" + login + " fork:true"
		if d.Org {
			sq = "org:" + login + " fork:true"
		}
		if q != "" {
			sq = strings.Join(strings.Fields(strings.ReplaceAll(q, ":", " ")), " ") + " in:name,description " + sq
		}
		gsort := map[string]string{"updated": "updated", "stars": "stars", "name": ""}[sort]
		var total int
		repos, total, err = g.c.SearchReposPage(ctx, sq, gsort, page, ghReposPage)
		more = page*ghReposPage < total && page*ghReposPage < 1000
	} else {
		repos, err = g.c.OwnerRepos(ctx, login, d.Org, d.Self, sort, page, ghReposPage)
		more = len(repos) == ghReposPage
	}
	if err != nil {
		d.ReposErr = tetherErrText(err)
		return
	}
	for _, rp := range repos {
		if c, ok := a.ghCard(rp); ok {
			d.Repos = append(d.Repos, c)
		}
	}
	d.Pager = pagerFor(r, page, more)
	if d.Pager.Next != "" {
		d.Pager.Next += "#repositories"
	}
	if d.Pager.Prev != "" {
		d.Pager.Prev += "#repositories"
	}
}

// ghProfileReadme: <login>/<login>'s README for people, .github's
// profile/README.md for organisations. Errors just leave it out.
func (a *App) ghProfileReadme(r *http.Request, g *ghReq, login string, d *ghProfileData) {
	ctx := r.Context()
	if d.Org {
		full := login + "/.github"
		if !tether.ValidRepo(full) {
			return
		}
		if _, f, err := g.c.Contents(ctx, full, "", "profile/README.md"); err == nil && f != nil {
			if b, ok := f.Decoded(); ok && len(b) <= 1<<20 {
				d.Readme = a.ghMarkdown(string(b), ghMD{Repo: full, Ref: "HEAD", Dir: "profile"})
				d.ReadmeFrom = full
			}
		}
		return
	}
	full := login + "/" + login
	if !tether.ValidRepo(full) {
		return
	}
	if rd, err := g.c.Readme(ctx, full, "", ""); err == nil {
		if b, ok := rd.Decoded(); ok && len(b) <= 1<<20 && isMarkdown(rd.Name) {
			d.Readme = a.ghMarkdown(string(b), ghMD{Repo: full, Ref: "HEAD"})
			d.ReadmeFrom = full
		}
	}
}
