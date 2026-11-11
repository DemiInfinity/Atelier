package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"atelier/internal/store"
	"atelier/internal/tether"
)

// Getting to an account (#16)
//
//   - The top bar's avatar opens an account menu listing your personal
//     account and every company you can edit, and profile pages carry account
//     tabs ("Repositories: Personal · Elyndra Studios"). Both come from
//     switchAccounts, so they never list an account the viewer can't edit.
//   - Forgiving addresses: /Elyndra-Studios (or /elyndra.studios, or the
//     GitHub organisation a company is linked to) redirects to the account it
//     means. Otherwise the not-found page suggests close matches and offers
//     /gh/<name>. Only accounts the viewer may know about are ever matched or
//     suggested (accountKnownTo); GitHub links count only for accounts the
//     viewer can edit, since they're the owner's own settings.

// switchAccounts are the accounts who may switch between: their own first,
// then companies. Nobody else's personal account is ever listed.
func (a *App) switchAccounts(ctx context.Context, who Actor) []store.Owner {
	if who.Kind != ActorOwner || who.PersonID == 0 {
		return nil
	}
	owners, err := a.db.Owners(ctx)
	if err != nil {
		return nil
	}
	var mine, companies []store.Owner
	for _, o := range owners {
		if !a.canEditAccount(ctx, who, o) {
			continue
		}
		if o.ID == who.PersonID {
			mine = append(mine, o)
		} else {
			companies = append(companies, o)
		}
	}
	return append(mine, companies...)
}

// accountKnownTo reports whether who may learn that an account exists by a
// guess at its name: accounts they can edit, and accounts with something they
// can read.
func (a *App) accountKnownTo(ctx context.Context, who Actor, o store.Owner) bool {
	if a.canEditAccount(ctx, who, o) {
		return true
	}
	if (who.Kind == ActorOwner || who.Kind == ActorToken) && a.isOwnerPerson(ctx, who.PersonID) {
		return true
	}
	if who.Kind == ActorOwner && who.PersonID == o.ID {
		return true
	}
	repos, err := a.db.ReposByOwner(ctx, o.ID)
	if err != nil {
		return false
	}
	for _, rp := range repos {
		if a.can(ctx, who, rp, PermRead) {
			return true
		}
	}
	return false
}

// foldName is a name with case, spaces, hyphens, dots and underscores ignored.
func foldName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch r {
		case ' ', '-', '.', '_', '\t':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// companySuffixes are dropped from display names when matching, so
// "Elyndra Studios LTD" also answers to /Elyndra-Studios.
var companySuffixes = []string{"limited", "ltd", "llc", "inc", "gmbh", "plc", "co"}

func foldDisplay(s string) []string {
	f := foldName(s)
	out := []string{f}
	for _, suf := range companySuffixes {
		if t, ok := strings.CutSuffix(f, suf); ok && len(t) >= 3 {
			out = append(out, t)
		}
	}
	return out
}

type accountGuess struct {
	exact   []store.Owner // same name once folded
	similar []store.Owner // worth suggesting
}

// guessAccount finds the accounts a mistyped name might mean.
func (a *App) guessAccount(ctx context.Context, who Actor, name string) accountGuess {
	var g accountGuess
	want := foldName(name)
	if want == "" {
		return g
	}
	owners, err := a.db.Owners(ctx)
	if err != nil {
		return g
	}
	type scored struct {
		o store.Owner
		d int
	}
	var near []scored
	for _, o := range owners {
		if !a.accountKnownTo(ctx, who, o) {
			continue
		}
		keys := []string{foldName(o.Name)}
		if o.DisplayName != "" {
			keys = append(keys, foldDisplay(o.DisplayName)...)
		}
		if a.canEditAccount(ctx, who, o) {
			if gh := a.accountGitHub(ctx, o); gh != "" {
				keys = append(keys, foldName(gh))
			}
		}
		best := -1
		for _, k := range keys {
			if k == want {
				best = 0
				break
			}
			d := editDistance(k, want)
			if strings.HasPrefix(k, want) || strings.HasPrefix(want, k) {
				if d > 1 {
					d = 1 + d/4
				}
			}
			if best < 0 || d < best {
				best = d
			}
		}
		switch {
		case best == 0:
			g.exact = append(g.exact, o)
		case best >= 0 && best <= 2+len(want)/6:
			near = append(near, scored{o, best})
		}
	}
	sort.SliceStable(near, func(i, j int) bool { return near[i].d < near[j].d })
	for i, s := range near {
		if i == 5 {
			break
		}
		g.similar = append(g.similar, s.o)
	}
	return g
}

// editDistance is the Levenshtein distance (names are short).
func editDistance(x, y string) int {
	a, b := []rune(x), []rune(y)
	if len(a) > 100 || len(b) > 100 {
		return 100
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			c := 1
			if a[i-1] == b[j-1] {
				c = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+c)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// ownerMissing answers /{name} when no account has exactly that name:
// a redirect when one account clearly matches, else a helpful 404.
func (a *App) ownerMissing(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	who := a.actor(r)
	g := a.guessAccount(ctx, who, name)
	if len(g.exact) == 1 {
		target := "/" + url.PathEscape(g.exact[0].Name)
		if q := r.URL.RawQuery; q != "" {
			target += "?" + q
		}
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	d := map[string]any{"Name": name}
	sugg := append(append([]store.Owner{}, g.exact...), g.similar...)
	d["Suggestions"] = sugg
	if tether.ValidLogin(name) {
		d["GH"] = ghLocalURL(name)
	}
	w.WriteHeader(http.StatusNotFound)
	a.render(w, r, "account_missing", page{Title: "Not found", Data: d})
}

// ownerNotFound reports whether err means "no such account".
func ownerNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
