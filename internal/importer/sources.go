// Package importer brings repositories into Atelier from Gitea/Forgejo,
// GitHub or any git URL, and keeps optional mirrors in sync.
//
// TODO(issues): importing issues, pull requests, releases and wikis is not
// done yet. Issues are being built now; add them here once that lands.
package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Remote is a repository offered by a source.
type Remote struct {
	FullName      string
	Owner         string
	Name          string
	Description   string
	Private       bool
	Fork          bool
	Archived      bool
	CloneURL      string
	DefaultBranch string
}

// ErrUnauthorized means the source rejected the token.
var ErrUnauthorized = errors.New("the server rejected the token")

// DefaultHTTP is used when a nil client is passed.
var DefaultHTTP = &http.Client{Timeout: 30 * time.Second}

// GitHubAPI is GitHub's REST API base; tests point it elsewhere.
const GitHubAPI = "https://api.github.com"

const maxPages = 200

type apiRepo struct {
	FullName string `json:"full_name"`
	Name     string `json:"name"`
	Owner    struct {
		Login    string `json:"login"`
		Username string `json:"username"`
	} `json:"owner"`
	Description   string `json:"description"`
	Private       bool   `json:"private"`
	Fork          bool   `json:"fork"`
	Archived      bool   `json:"archived"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
}

func (r apiRepo) remote() Remote {
	owner := r.Owner.Login
	if owner == "" {
		owner = r.Owner.Username
	}
	full := r.FullName
	if full == "" {
		full = owner + "/" + r.Name
	}
	return Remote{FullName: full, Owner: owner, Name: r.Name, Description: r.Description, Private: r.Private,
		Fork: r.Fork, Archived: r.Archived, CloneURL: r.CloneURL, DefaultBranch: r.DefaultBranch}
}

// NormalizeServer checks and tidies a server base URL (http or https only).
func NormalizeServer(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw != "" && !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("enter the server's address, e.g. https://gitea.example.com")
	}
	if u.User != nil {
		return "", errors.New("put the token in the token field, not in the address")
	}
	u.RawQuery, u.Fragment = "", ""
	return strings.TrimRight(u.String(), "/"), nil
}

func getJSON(ctx context.Context, hc *http.Client, u, authHeader string, v any) (http.Header, error) {
	if hc == nil {
		hc = DefaultHTTP
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Atelier-importer")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("couldn't reach %s: %v", req.URL.Host, ue.Err)
		}
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, ErrUnauthorized
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s answered %s", req.URL.Host, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(v); err != nil {
		return nil, fmt.Errorf("unexpected answer from %s: %v", req.URL.Host, err)
	}
	return resp.Header, nil
}

// ListGitea lists every repository the token can see on a Gitea or Forgejo
// server: the user's own, collaborations and all of their organizations'.
// It returns the token owner's login too (used as the git username).
func ListGitea(ctx context.Context, hc *http.Client, server, token string) ([]Remote, string, error) {
	base, err := NormalizeServer(server)
	if err != nil {
		return nil, "", err
	}
	auth := "token " + token
	var me struct {
		Login    string `json:"login"`
		Username string `json:"username"`
	}
	if _, err := getJSON(ctx, hc, base+"/api/v1/user", auth, &me); err != nil {
		return nil, "", err
	}
	login := me.Login
	if login == "" {
		login = me.Username
	}

	const limit = 50
	seen := map[string]bool{}
	var out []Remote
	pages := func(path string) error {
		for page := 1; page <= maxPages; page++ {
			var batch []apiRepo
			u := fmt.Sprintf("%s%s?limit=%d&page=%d", base, path, limit, page)
			if _, err := getJSON(ctx, hc, u, auth, &batch); err != nil {
				return err
			}
			for _, r := range batch {
				rm := r.remote()
				if !seen[strings.ToLower(rm.FullName)] {
					seen[strings.ToLower(rm.FullName)] = true
					out = append(out, rm)
				}
			}
			if len(batch) < limit {
				return nil
			}
		}
		return nil
	}
	if err := pages("/api/v1/user/repos"); err != nil {
		return nil, login, err
	}
	var orgs []string
	for page := 1; page <= maxPages; page++ {
		var batch []struct {
			Username string `json:"username"`
			Name     string `json:"name"`
		}
		if _, err := getJSON(ctx, hc, fmt.Sprintf("%s/api/v1/user/orgs?limit=%d&page=%d", base, limit, page), auth, &batch); err != nil {
			return nil, login, err
		}
		for _, o := range batch {
			n := o.Username
			if n == "" {
				n = o.Name
			}
			orgs = append(orgs, n)
		}
		if len(batch) < limit {
			break
		}
	}
	for _, org := range orgs {
		if err := pages("/api/v1/orgs/" + url.PathEscape(org) + "/repos"); err != nil {
			return nil, login, err
		}
	}
	sortRemotes(out)
	return out, login, nil
}

var nextLink = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)

// ListGitHub lists the repositories the token can see on GitHub (owned,
// organization and collaborator), following pagination links. apiBase is
// normally GitHubAPI.
func ListGitHub(ctx context.Context, hc *http.Client, apiBase, token string) ([]Remote, string, error) {
	if apiBase == "" {
		apiBase = GitHubAPI
	}
	apiBase = strings.TrimRight(apiBase, "/")
	auth := "Bearer " + token
	var me struct {
		Login string `json:"login"`
	}
	if _, err := getJSON(ctx, hc, apiBase+"/user", auth, &me); err != nil {
		return nil, "", err
	}
	u := apiBase + "/user/repos?per_page=100&affiliation=owner,collaborator,organization_member&sort=full_name"
	var out []Remote
	seen := map[string]bool{}
	for page := 0; u != "" && page < maxPages; page++ {
		var batch []apiRepo
		h, err := getJSON(ctx, hc, u, auth, &batch)
		if err != nil {
			return nil, me.Login, err
		}
		for _, r := range batch {
			rm := r.remote()
			if !seen[strings.ToLower(rm.FullName)] {
				seen[strings.ToLower(rm.FullName)] = true
				out = append(out, rm)
			}
		}
		u = ""
		if m := nextLink.FindStringSubmatch(h.Get("Link")); m != nil {
			next, err := url.Parse(m[1])
			base, _ := url.Parse(apiBase)
			// Only follow links back to the same API host, so the token never leaks elsewhere.
			if err == nil && base != nil && next.Host == base.Host && next.Scheme == base.Scheme {
				u = next.String()
			}
		}
	}
	sortRemotes(out)
	return out, me.Login, nil
}

func sortRemotes(r []Remote) {
	sort.Slice(r, func(i, j int) bool { return strings.ToLower(r[i].FullName) < strings.ToLower(r[j].FullName) })
}
