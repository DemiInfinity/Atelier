package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"atelier/internal/store"
	"atelier/internal/tether"
)

// The "I use Atelier" badge: an opt-in .atelier file in your GitHub profile
// repository (<login>/<login>):
//
//	{"atelier": true, "studio": "https://git.example.com/you"}
//
// When Tether shows GitHub people, it looks for the same file (cached for a
// day, re-checked with conditional requests) and shows a small amber mark
// and "Visit their studio".

const tetherBadgeFile = ".atelier"

type tetherBadgeFileJSON struct {
	Atelier bool   `json:"atelier"`
	Studio  string `json:"studio"`
}

// tetherStudioURL is what goes in your badge: your public profile, if known.
func (a *App) tetherStudioURL(ctx context.Context, personID int64) string {
	if a.cfg.PublicURL == "" {
		return ""
	}
	p, err := a.db.Person(ctx, personID)
	if err != nil {
		return ""
	}
	return strings.TrimRight(a.cfg.PublicURL, "/") + "/" + url.PathEscape(p.Name)
}

// tetherSetBadge writes or removes the badge file.
func (a *App) tetherSetBadge(ctx context.Context, personID int64, on bool) error {
	c, acc, err := a.tetherClient(ctx, personID)
	if err != nil {
		return err
	}
	full := acc.Login + "/" + acc.Login
	cur, err := c.GetContent(ctx, full, tetherBadgeFile)
	exists := err == nil
	if err != nil && !tether.IsStatus(err, http.StatusNotFound) {
		return err
	}
	if on {
		body, _ := json.Marshal(tetherBadgeFileJSON{Atelier: true, Studio: a.tetherStudioURL(ctx, personID)})
		body = append(body, '\n')
		if exists {
			if old, err := cur.Decoded(); err == nil && strings.TrimSpace(string(old)) == strings.TrimSpace(string(body)) {
				return a.db.SetTetherBadge(ctx, personID, true)
			}
		}
		if err := c.PutContent(ctx, full, tetherBadgeFile, "I use Atelier", body, cur.SHA); err != nil {
			if tether.IsStatus(err, http.StatusNotFound) {
				return fmt.Errorf("create your profile repository %s on GitHub first, then try again", full)
			}
			return err
		}
	} else if exists {
		if err := c.DeleteContent(ctx, full, tetherBadgeFile, "Remove the Atelier badge", cur.SHA); err != nil {
			return err
		}
	}
	_ = a.db.SaveTetherBadge(ctx, store.TetherBadge{Login: acc.Login, Present: on, Studio: a.tetherStudioURL(ctx, personID), CheckedAt: time.Now()})
	return a.db.SetTetherBadge(ctx, personID, on)
}

func (a *App) tetherBadgeToggle(w http.ResponseWriter, r *http.Request) {
	on := r.FormValue("on") == "1"
	if err := a.tetherSetBadge(r.Context(), a.actor(r).PersonID, on); err != nil {
		a.renderTetherSettings(w, r, "Couldn't update the badge: "+tetherErrText(err), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/settings/tether?saved=1", http.StatusSeeOther)
}

// tetherSafeStudio keeps a studio URL only if it's a plain http(s) link.
func tetherSafeStudio(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return ""
	}
	return u.String()
}

// tetherCheckBadge looks for login's badge file and caches the answer.
func (a *App) tetherCheckBadge(ctx context.Context, c *tether.Client, login string) (store.TetherBadge, error) {
	b := store.TetherBadge{Login: login, CheckedAt: time.Now()}
	cur, err := c.GetContent(ctx, login+"/"+login, tetherBadgeFile)
	switch {
	case tether.IsStatus(err, http.StatusNotFound):
	case err != nil:
		return b, err
	default:
		raw, derr := cur.Decoded()
		var f tetherBadgeFileJSON
		if derr == nil && len(raw) < 4096 && json.Unmarshal(raw, &f) == nil && f.Atelier {
			b.Present, b.Studio = true, tetherSafeStudio(f.Studio)
		}
		b.ETag = cur.SHA
	}
	return b, a.db.SaveTetherBadge(ctx, b)
}

// tetherBadges returns what's known about logins' badges, and checks the
// unknown or stale ones in the background with the person's token.
func (a *App) tetherBadges(ctx context.Context, personID int64, logins []string) map[string]store.TetherBadge {
	out := map[string]store.TetherBadge{}
	var stale []string
	seen := map[string]bool{}
	for _, l := range logins {
		k := strings.ToLower(l)
		if l == "" || seen[k] || !tether.ValidLogin(l) {
			continue
		}
		seen[k] = true
		b, err := a.db.TetherBadge(ctx, l)
		if err == nil {
			out[k] = b
		}
		if err != nil || time.Since(b.CheckedAt) > tetherBadgeTTL {
			stale = append(stale, l)
		}
	}
	if len(stale) > 0 {
		if len(stale) > 20 {
			stale = stale[:20]
		}
		st := a.tether()
		bctx := context.WithoutCancel(ctx)
		st.goBG(fmt.Sprintf("badges:%d:%s", personID, strings.Join(stale, ",")), func() {
			c, _, err := a.tetherClient(bctx, personID)
			if err != nil {
				return
			}
			for _, l := range stale {
				if _, err := a.tetherCheckBadge(bctx, c, l); err != nil {
					return // rate limited or offline: try again another time
				}
			}
		})
	}
	return out
}

// tetherPerson is a GitHub person as Tether shows them.
type tetherPerson struct {
	Login   string
	Avatar  string
	URL     string
	Atelier bool
	Studio  string
}

func (a *App) tetherPersonView(login, avatar string, badges map[string]store.TetherBadge) tetherPerson {
	p := tetherPerson{Login: login, Avatar: tetherAvatar(avatar), URL: a.tetherWebURL(url.PathEscape(login))}
	if b, ok := badges[strings.ToLower(login)]; ok && b.Present {
		p.Atelier, p.Studio = true, b.Studio
	}
	return p
}
