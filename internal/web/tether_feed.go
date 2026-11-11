package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strings"
	"time"

	"atelier/internal/tether"
)

// The home page's GitHub side: activity of the people you follow, releases
// of repositories you starred, and "Popular this week" (GitHub has no
// trending API: it's a search for repositories created in the last week,
// sorted by stars). Pages only ever read the cache; the background loop
// refreshes it, so the home page works offline and shows "as of".

// tetherFeedItem is one GitHub activity row. Every field is untrusted text.
type tetherFeedItem struct {
	Kind   string    `json:"kind"` // star, push, create, fork, release, pull, issue, comment, public
	Actor  string    `json:"actor"`
	Avatar string    `json:"avatar"`
	Repo   string    `json:"repo"`
	Text   string    `json:"text"`   // "starred", "pushed to main", …
	Detail string    `json:"detail"` // a title or tag
	URL    string    `json:"url"`
	When   time.Time `json:"when"`
}

type tetherFeedCache struct {
	Items []tetherFeedItem `json:"items"`
}

type tetherTrendRepo struct {
	FullName    string `json:"full_name"`
	Description string `json:"description"`
	Language    string `json:"language"`
	Stars       int    `json:"stars"`
	Owner       string `json:"owner"`
	Avatar      string `json:"avatar"`
	URL         string `json:"url"`
}

type tetherTrendCache struct {
	Repos []tetherTrendRepo `json:"repos"`
}

// tetherTrendLanguages are the language chips on the home page.
var tetherTrendLanguages = []string{"", "Go", "TypeScript", "Python", "Rust", "Swift"}

func tetherTrendKey(lang string) string { return "trending:" + strings.ToLower(lang) }

func tetherValidLang(lang string) bool {
	for _, l := range tetherTrendLanguages {
		if strings.EqualFold(l, lang) {
			return true
		}
	}
	return false
}

func tetherCanonLang(lang string) string {
	for _, l := range tetherTrendLanguages {
		if strings.EqualFold(l, lang) {
			return l
		}
	}
	return ""
}

// tetherRefreshIfStale refreshes the feed and trending lists in the background when old.
func (a *App) tetherRefreshIfStale(ctx context.Context, personID int64, lang string) {
	if _, err := a.db.TetherAccount(ctx, personID); err != nil {
		return
	}
	st := a.tether()
	bctx := context.WithoutCancel(ctx)
	if _, _, at, err := a.db.TetherCacheGet(ctx, personID, "feed"); err != nil || time.Since(at) > tetherFeedTTL {
		st.goBG(fmt.Sprintf("feed:%d", personID), func() {
			if err := a.tetherRefreshFeed(bctx, personID); err != nil {
				_ = a.db.SetTetherAccountError(bctx, personID, tetherErrText(err))
			}
		})
	}
	if _, _, at, err := a.db.TetherCacheGet(ctx, personID, tetherTrendKey(lang)); err != nil || time.Since(at) > tetherTrendingTTL {
		st.goBG(fmt.Sprintf("trend:%d:%s", personID, lang), func() {
			if err := a.tetherRefreshTrending(bctx, personID, lang); err != nil {
				log.Printf("tether: trending: %s", tetherErrText(err))
			}
		})
	}
}

func eventPayload(ev tether.Event) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(ev.Payload, &p)
	if p == nil {
		p = map[string]any{}
	}
	return p
}

func pstr(m map[string]any, keys ...string) string {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	switch v := cur.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	}
	return ""
}

// tetherEventItem turns a GitHub event into a feed row (ok=false to skip it).
func (a *App) tetherEventItem(ev tether.Event) (tetherFeedItem, bool) {
	repo := ev.Repo.Name
	if !tether.ValidRepo(repo) {
		return tetherFeedItem{}, false
	}
	it := tetherFeedItem{Actor: ev.Actor.Login, Avatar: tetherAvatar(ev.Actor.AvatarURL), Repo: repo, When: ev.CreatedAt,
		URL: a.tetherWebURL(repo)}
	p := eventPayload(ev)
	switch ev.Type {
	case "WatchEvent":
		it.Kind, it.Text = "star", "starred"
	case "ForkEvent":
		it.Kind, it.Text = "fork", "forked"
	case "PublicEvent":
		it.Kind, it.Text = "public", "made public"
	case "PushEvent":
		it.Kind, it.Text = "push", "pushed to "+strings.TrimPrefix(pstr(p, "ref"), "refs/heads/")
	case "CreateEvent":
		switch pstr(p, "ref_type") {
		case "repository":
			it.Kind, it.Text = "create", "created a repository"
		case "tag":
			it.Kind, it.Text, it.Detail = "create", "tagged", pstr(p, "ref")
		default:
			return it, false // new branches are noise
		}
	case "ReleaseEvent":
		it.Kind, it.Text = "release", "released"
		it.Detail = pstr(p, "release", "name")
		if it.Detail == "" {
			it.Detail = pstr(p, "release", "tag_name")
		}
		it.URL = a.tetherHTMLURL(pstr(p, "release", "html_url"), repo+"/releases")
	case "PullRequestEvent":
		if pstr(p, "action") != "opened" {
			return it, false
		}
		it.Kind, it.Text, it.Detail = "pull", "opened pull request #"+pstr(p, "number"), pstr(p, "pull_request", "title")
		it.URL = a.tetherHTMLURL(pstr(p, "pull_request", "html_url"), repo+"/pulls")
	case "IssuesEvent":
		if pstr(p, "action") != "opened" {
			return it, false
		}
		it.Kind, it.Text, it.Detail = "issue", "opened issue #"+pstr(p, "issue", "number"), pstr(p, "issue", "title")
		it.URL = a.tetherHTMLURL(pstr(p, "issue", "html_url"), repo+"/issues")
	default:
		return it, false
	}
	it.Detail = eventExcerpt(it.Detail, 140)
	return it, true
}

// tetherRefreshFeed fetches a person's GitHub activity into the cache.
func (a *App) tetherRefreshFeed(ctx context.Context, personID int64) error {
	c, acc, err := a.tetherClient(ctx, personID)
	if err != nil {
		return err
	}
	var items []tetherFeedItem
	events, err := c.ReceivedEvents(ctx, acc.Login, 60)
	if err != nil {
		return err
	}
	for _, ev := range events {
		if it, ok := a.tetherEventItem(ev); ok {
			items = append(items, it)
		}
	}
	// Releases of recently active starred repositories.
	if starred, err := c.Starred(ctx, 30); err == nil {
		sort.SliceStable(starred, func(i, j int) bool { return starred[i].PushedAt.After(starred[j].PushedAt) })
		cutoff := time.Now().AddDate(0, 0, -30)
		for i, rp := range starred {
			if i >= 8 {
				break
			}
			rels, err := c.Releases(ctx, rp.FullName, 3)
			if err != nil {
				break
			}
			for _, rel := range rels {
				if rel.Draft || rel.PublishedAt.Before(cutoff) {
					continue
				}
				name := rel.Name
				if name == "" {
					name = rel.TagName
				}
				items = append(items, tetherFeedItem{Kind: "release", Actor: rp.Owner.Login, Avatar: tetherAvatar(rp.Owner.AvatarURL),
					Repo: rp.FullName, Text: "released", Detail: eventExcerpt(name, 140), When: rel.PublishedAt,
					URL: a.tetherHTMLURL(rel.HTMLURL, rp.FullName+"/releases")})
			}
		}
	}
	// One row per release even if it came from both sources.
	seen := map[string]bool{}
	var out []tetherFeedItem
	sort.SliceStable(items, func(i, j int) bool { return items[i].When.After(items[j].When) })
	for _, it := range items {
		k := it.Kind + "\x00" + it.Repo + "\x00" + it.Text + "\x00" + it.Detail
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, it)
		if len(out) >= 40 {
			break
		}
	}
	body, _ := json.Marshal(tetherFeedCache{Items: out})
	_ = a.db.SetTetherAccountError(ctx, personID, "")
	return a.db.TetherCachePut(ctx, personID, "feed", "", body)
}

// tetherRefreshTrending runs the "Popular this week" search.
func (a *App) tetherRefreshTrending(ctx context.Context, personID int64, lang string) error {
	c, _, err := a.tetherClient(ctx, personID)
	if err != nil {
		return err
	}
	q := "created:>" + time.Now().AddDate(0, 0, -7).UTC().Format("2006-01-02")
	if lang = tetherCanonLang(lang); lang != "" {
		q += " language:" + lang
	}
	repos, _, err := c.SearchRepos(ctx, q, "stars", 10)
	if err != nil {
		return err
	}
	var out []tetherTrendRepo
	for _, rp := range repos {
		if !tether.ValidRepo(rp.FullName) {
			continue
		}
		out = append(out, tetherTrendRepo{FullName: rp.FullName, Description: eventExcerpt(rp.Description, 160),
			Language: eventExcerpt(rp.Language, 30), Stars: rp.Stars, Owner: rp.Owner.Login,
			Avatar: tetherAvatar(rp.Owner.AvatarURL), URL: a.tetherWebURL(rp.FullName)})
	}
	body, _ := json.Marshal(tetherTrendCache{Repos: out})
	return a.db.TetherCachePut(ctx, personID, tetherTrendKey(lang), "", body)
}

// ---------- the home page ----------

// tetherHome is the Tether side of the home page.
type tetherHome struct {
	Connected  bool
	Login      string
	Avatar     string
	Profile    string
	Error      string
	FeedAsOf   time.Time
	Feed       []tetherFeedView
	Trending   []tetherTrendView
	TrendAsOf  time.Time
	Lang       string
	Languages  []tetherLangChip
	Refreshing bool
}

type tetherLangChip struct {
	Label, URL string
	Active     bool
}

type tetherFeedView struct {
	tetherFeedItem
	Person tetherPerson
}

type tetherTrendView struct {
	tetherTrendRepo
	Person tetherPerson
}

// homeItem is a row of the blended activity feed: local or from GitHub.
type homeItem struct {
	When   time.Time
	Local  *activity
	Tether *tetherFeedView
}

func (a *App) tetherHomeData(ctx context.Context, personID int64, lang, account string) tetherHome {
	h := tetherHome{Lang: tetherCanonLang(lang)}
	for _, l := range tetherTrendLanguages {
		label := l
		if label == "" {
			label = "All"
		}
		q := url.Values{}
		if account != "" {
			q.Set("account", account)
		}
		if l != "" {
			q.Set("lang", l)
		}
		u := "/"
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		h.Languages = append(h.Languages, tetherLangChip{Label: label, URL: u + "#tether-trending", Active: l == h.Lang})
	}
	acc, err := a.db.TetherAccount(ctx, personID)
	if err != nil {
		return h
	}
	h.Connected, h.Login, h.Avatar, h.Profile, h.Error = true, acc.Login, tetherAvatar(acc.AvatarURL), a.tetherWebURL(acc.Login), acc.LastError
	var logins []string
	var feed tetherFeedCache
	if _, body, at, err := a.db.TetherCacheGet(ctx, personID, "feed"); err == nil && json.Unmarshal(body, &feed) == nil {
		h.FeedAsOf = at
		for _, it := range feed.Items {
			logins = append(logins, it.Actor)
		}
	} else {
		h.Refreshing = true
	}
	var trend tetherTrendCache
	if _, body, at, err := a.db.TetherCacheGet(ctx, personID, tetherTrendKey(h.Lang)); err == nil && json.Unmarshal(body, &trend) == nil {
		h.TrendAsOf = at
		for _, rp := range trend.Repos {
			logins = append(logins, rp.Owner)
		}
	} else {
		h.Refreshing = true
	}
	badges := a.tetherBadges(ctx, personID, logins)
	for _, it := range feed.Items {
		h.Feed = append(h.Feed, tetherFeedView{tetherFeedItem: it, Person: a.tetherPersonView(it.Actor, it.Avatar, badges)})
	}
	for i, rp := range trend.Repos {
		if i >= 6 {
			break
		}
		h.Trending = append(h.Trending, tetherTrendView{tetherTrendRepo: rp, Person: a.tetherPersonView(rp.Owner, rp.Avatar, badges)})
	}
	a.tetherRefreshIfStale(ctx, personID, h.Lang)
	return h
}

// blendHomeFeed merges local activity with GitHub's, newest first.
func blendHomeFeed(local []activity, gh []tetherFeedView, max int) []homeItem {
	var out []homeItem
	for i := range local {
		out = append(out, homeItem{When: local[i].Commit.When, Local: &local[i]})
	}
	for i := range gh {
		out = append(out, homeItem{When: gh[i].When, Tether: &gh[i]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].When.After(out[j].When) })
	if len(out) > max {
		out = out[:max]
	}
	return out
}
