package tether

import (
	"context"
	"sort"
	"strings"
)

// VisibleOwners lists the GitHub owners (users and organisations) whose
// repositories the token can see, from GET /user/repos (up to 5 pages). For a
// fine-grained token that is its resource owner. Logins GitHub sends that
// aren't valid are skipped.
func (c *Client) VisibleOwners(ctx context.Context) ([]string, error) {
	repos, err := pages[Repo](ctx, c, "/user/repos?per_page=100&sort=full_name", 5)
	if err != nil && len(repos) == 0 {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range repos {
		l := r.Owner.Login
		if l == "" {
			l, _, _ = strings.Cut(r.FullName, "/")
		}
		if !ValidLogin(l) || seen[strings.ToLower(l)] {
			continue
		}
		seen[strings.ToLower(l)] = true
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out, nil
}
