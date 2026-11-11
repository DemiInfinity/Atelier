package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
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
	Connect string `json:"connect,omitempty"` // the studio's Atelier Connect actor (ConnectBadgeActor), for "Follow on Atelier"
}

// tetherStudioURL is what goes in your badge: your public profile, if known.
// A LAN-only address (ATELIER_PUBLIC_URL pointing at a .lan name, a private
// IP…) is never published to GitHub.
func (a *App) tetherStudioURL(ctx context.Context, personID int64) string {
	if a.cfg.PublicURL == "" || !tetherPublicAddress(a.cfg.PublicURL) {
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
		body, _ := json.Marshal(tetherBadgeFileJSON{Atelier: true, Studio: a.tetherStudioURL(ctx, personID),
			Connect: a.ConnectBadgeActor(ctx, personID)})
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

// tetherLANSuffixes are hostname endings that only resolve on a local network.
var tetherLANSuffixes = []string{".lan", ".local", ".localdomain", ".home", ".home.arpa", ".internal", ".intranet",
	".corp", ".private", ".localhost", ".test", ".invalid"}

// tetherPublicAddress reports whether a URL's host could be reached from the
// internet: not localhost, not a private, loopback or link-local IP, not a
// single-label name and not a LAN-only suffix like .lan or .local.
func tetherPublicAddress(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" || host == "localhost" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !(ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() ||
			ip.IsLinkLocalMulticast() || ip.IsMulticast() || isCGNAT(ip))
	}
	if !strings.Contains(host, ".") {
		return false
	}
	for _, sfx := range tetherLANSuffixes {
		if strings.HasSuffix(host, sfx) {
			return false
		}
	}
	return true
}

// isCGNAT is 100.64.0.0/10 (Tailscale and carrier NAT): not public either.
func isCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64
}

// tetherBadgeStatus says, for a badge that is on, whether it carries a
// "Visit their studio" link. stale is true when the published file's studio
// address no longer matches (re-adding the badge updates it).
func (a *App) tetherBadgeStatus(ctx context.Context, personID int64, login string) (msg string, stale bool) {
	studio := a.tetherStudioURL(ctx, personID)
	if b, err := a.db.TetherBadge(ctx, login); err == nil && b.Studio != studio {
		stale = true
	}
	if studio == "" {
		return "Badge on: no public address yet, so there's no 'Visit their studio' link.", stale
	}
	return "Badge on, linking to " + studio + ".", stale
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
		a.connectNoteBadge(ctx, login, f.Connect)
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

// tetherBadgeNow is login's badge, checked right away (with the person's
// client) when it isn't known or is stale. A failed check falls back to
// whatever is cached. This is the lookup to reuse wherever one person's
// badge matters (a profile page, Atelier Connect).
func (a *App) tetherBadgeNow(ctx context.Context, c *tether.Client, login string) (store.TetherBadge, bool) {
	if !tether.ValidLogin(login) {
		return store.TetherBadge{}, false
	}
	b, err := a.db.TetherBadge(ctx, login)
	if err == nil && time.Since(b.CheckedAt) <= tetherBadgeTTL {
		return b, b.Present
	}
	if c != nil {
		if fresh, cerr := a.tetherCheckBadge(ctx, c, login); cerr == nil {
			return fresh, fresh.Present
		}
	}
	return b, err == nil && b.Present
}

// tetherPerson is a GitHub person as Tether shows them. URL opens them in
// Atelier (/gh/<login>); GH is their page on GitHub.
type tetherPerson struct {
	Login   string
	Avatar  string
	URL     string
	GH      string
	Atelier bool
	Studio  string
}

func (a *App) tetherPersonView(login, avatar string, badges map[string]store.TetherBadge) tetherPerson {
	p := tetherPerson{Login: login, Avatar: tetherAvatar(avatar), URL: ghLocalURL(login), GH: a.tetherWebURL(url.PathEscape(login))}
	if b, ok := badges[strings.ToLower(login)]; ok && b.Present {
		p.Atelier, p.Studio = true, b.Studio
	}
	return p
}
