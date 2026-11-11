package tether

// Read-only calls for browsing GitHub inside Atelier (/gh/…): profiles,
// repositories, files, commits, issues, pull requests and releases. Every
// GET goes through the Client's cache, so a page can be answered from it
// (MaxAge) or from what's left when GitHub can't be asked (StaleOK).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Profile is a GitHub user or organisation.
type Profile struct {
	ID              int64     `json:"id"`
	Login           string    `json:"login"`
	Name            string    `json:"name"`
	AvatarURL       string    `json:"avatar_url"`
	HTMLURL         string    `json:"html_url"`
	Type            string    `json:"type"` // User or Organization
	Bio             string    `json:"bio"`
	Description     string    `json:"description"` // organisations
	Company         string    `json:"company"`
	Location        string    `json:"location"`
	Blog            string    `json:"blog"`
	TwitterUsername string    `json:"twitter_username"`
	Followers       int       `json:"followers"`
	Following       int       `json:"following"`
	PublicRepos     int       `json:"public_repos"`
	CreatedAt       time.Time `json:"created_at"`
}

// IsOrg reports whether the profile is an organisation.
func (p Profile) IsOrg() bool { return p.Type == "Organization" }

// Entry is a file or directory from the contents API.
type Entry struct {
	Type        string `json:"type"` // file, dir, symlink, submodule
	Name        string `json:"name"`
	Path        string `json:"path"`
	SHA         string `json:"sha"`
	Size        int64  `json:"size"`
	Content     string `json:"content"`
	Encoding    string `json:"encoding"` // base64, or "none" when too large
	DownloadURL string `json:"download_url"`
	HTMLURL     string `json:"html_url"`
}

// Decoded returns a file's bytes (ok=false when GitHub didn't include them).
func (e Entry) Decoded() ([]byte, bool) {
	if e.Encoding != "base64" {
		return nil, false
	}
	b, err := Content{Content: e.Content, Encoding: "base64"}.Decoded()
	return b, err == nil
}

// Ref is a branch or tag.
type Ref struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

// CommitFile is one changed file in a commit or pull request.
type CommitFile struct {
	SHA              string `json:"sha"`
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"` // added, removed, modified, renamed…
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Changes          int    `json:"changes"`
	Patch            string `json:"patch"`
}

// Person is a commit's author or committer as git records it.
type Person struct {
	Name  string    `json:"name"`
	Email string    `json:"email"`
	Date  time.Time `json:"date"`
}

// CommitInfo is a commit from the commits API.
type CommitInfo struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Message   string `json:"message"`
		Author    Person `json:"author"`
		Committer Person `json:"committer"`
	} `json:"commit"`
	Author    *User `json:"author"` // the GitHub account, when one matches
	Committer *User `json:"committer"`
	Parents   []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
	Stats *struct {
		Additions int `json:"additions"`
		Deletions int `json:"deletions"`
		Total     int `json:"total"`
	} `json:"stats"`
	Files []CommitFile `json:"files"`
}

func loginPath(login string) (string, error) {
	if !ValidLogin(login) {
		return "", fmt.Errorf("%q isn't a GitHub login", login)
	}
	return url.PathEscape(login), nil
}

// pathSegs escapes a repository path ("" for the root).
func pathSegs(file string) (string, error) {
	if file == "" {
		return "", nil
	}
	var segs []string
	for _, s := range strings.Split(file, "/") {
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("bad path %q", file)
		}
		segs = append(segs, url.PathEscape(s))
	}
	return "/" + strings.Join(segs, "/"), nil
}

func pageQuery(v url.Values, page, perPage int) url.Values {
	if v == nil {
		v = url.Values{}
	}
	if perPage <= 0 || perPage > 100 {
		perPage = 30
	}
	if page < 1 {
		page = 1
	}
	v.Set("per_page", strconv.Itoa(perPage))
	v.Set("page", strconv.Itoa(page))
	return v
}

// Profile reads a user or organisation.
func (c *Client) Profile(ctx context.Context, login string) (Profile, error) {
	lp, err := loginPath(login)
	if err != nil {
		return Profile{}, err
	}
	var p Profile
	_, err = c.do(ctx, http.MethodGet, "/users/"+lp, nil, &p, true)
	return p, err
}

// OwnerRepos lists an account's repositories. sort is "updated" (last
// pushed, newest first) or "name". self lists the token user's own
// repositories, private ones included.
func (c *Client) OwnerRepos(ctx context.Context, login string, org, self bool, sort string, page, perPage int) ([]Repo, error) {
	lp, err := loginPath(login)
	if err != nil {
		return nil, err
	}
	v := url.Values{}
	if sort == "name" {
		v.Set("sort", "full_name")
		v.Set("direction", "asc")
	} else {
		v.Set("sort", "pushed")
		v.Set("direction", "desc")
	}
	var p string
	switch {
	case self:
		v.Set("affiliation", "owner")
		p = "/user/repos"
	case org:
		v.Set("type", "all")
		p = "/orgs/" + lp + "/repos"
	default:
		v.Set("type", "owner")
		p = "/users/" + lp + "/repos"
	}
	var out []Repo
	_, err = c.do(ctx, http.MethodGet, p+"?"+pageQuery(v, page, perPage).Encode(), nil, &out, true)
	return out, err
}

// SearchReposPage is SearchRepos with a page number.
func (c *Client) SearchReposPage(ctx context.Context, q, sort string, page, perPage int) ([]Repo, int, error) {
	v := url.Values{"q": {q}}
	if sort != "" {
		v.Set("sort", sort)
		v.Set("order", "desc")
	}
	var out struct {
		Total int    `json:"total_count"`
		Items []Repo `json:"items"`
	}
	_, err := c.do(ctx, http.MethodGet, "/search/repositories?"+pageQuery(v, page, perPage).Encode(), nil, &out, true)
	return out.Items, out.Total, err
}

// graphqlURL is the GraphQL endpoint next to the REST API.
func (c *Client) graphqlURL() string {
	api := c.api()
	if strings.HasSuffix(api, "/api/v3") { // GitHub Enterprise Server
		return strings.TrimSuffix(api, "/v3") + "/graphql"
	}
	return api + "/graphql"
}

const pinnedQuery = `query($login: String!) {
  repositoryOwner(login: $login) {
    ... on ProfileItemOwner {
      pinnedItems(first: 6, types: REPOSITORY) {
        nodes { ... on Repository {
          name nameWithOwner description url isPrivate isFork isArchived stargazerCount forkCount pushedAt
          primaryLanguage { name }
          owner { login avatarUrl }
        } }
      }
    }
  }
}`

// Pinned is a profile's pinned repositories (GraphQL; not cached here).
func (c *Client) Pinned(ctx context.Context, login string) ([]Repo, error) {
	if !ValidLogin(login) {
		return nil, fmt.Errorf("%q isn't a GitHub login", login)
	}
	var out struct {
		Data struct {
			RepositoryOwner *struct {
				PinnedItems struct {
					Nodes []struct {
						Name            string    `json:"name"`
						NameWithOwner   string    `json:"nameWithOwner"`
						Description     string    `json:"description"`
						URL             string    `json:"url"`
						IsPrivate       bool      `json:"isPrivate"`
						IsFork          bool      `json:"isFork"`
						IsArchived      bool      `json:"isArchived"`
						StargazerCount  int       `json:"stargazerCount"`
						ForkCount       int       `json:"forkCount"`
						PushedAt        time.Time `json:"pushedAt"`
						PrimaryLanguage *struct {
							Name string `json:"name"`
						} `json:"primaryLanguage"`
						Owner struct {
							Login     string `json:"login"`
							AvatarURL string `json:"avatarUrl"`
						} `json:"owner"`
					} `json:"nodes"`
				} `json:"pinnedItems"`
			} `json:"repositoryOwner"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	in := map[string]any{"query": pinnedQuery, "variables": map[string]string{"login": login}}
	if _, err := c.do(ctx, http.MethodPost, c.graphqlURL(), in, &out, false); err != nil {
		return nil, err
	}
	if len(out.Errors) > 0 {
		msg := out.Errors[0].Message
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, &APIError{Status: http.StatusBadGateway, Message: msg}
	}
	if out.Data.RepositoryOwner == nil {
		return nil, &APIError{Status: http.StatusNotFound, Message: "Not Found"}
	}
	var repos []Repo
	for _, n := range out.Data.RepositoryOwner.PinnedItems.Nodes {
		if n.NameWithOwner == "" {
			continue
		}
		r := Repo{Name: n.Name, FullName: n.NameWithOwner, Description: n.Description, HTMLURL: n.URL, Private: n.IsPrivate,
			Fork: n.IsFork, Archived: n.IsArchived, Stars: n.StargazerCount, Forks: n.ForkCount, PushedAt: n.PushedAt,
			Owner: User{Login: n.Owner.Login, AvatarURL: n.Owner.AvatarURL}}
		if n.PrimaryLanguage != nil {
			r.Language = n.PrimaryLanguage.Name
		}
		repos = append(repos, r)
	}
	return repos, nil
}

// Contents lists a directory (dir) or reads a file (file) at ref.
func (c *Client) Contents(ctx context.Context, full, ref, file string) (dir []Entry, f *Entry, err error) {
	p, err := repoPath(full)
	if err != nil {
		return nil, nil, err
	}
	sp, err := pathSegs(file)
	if err != nil {
		return nil, nil, err
	}
	u := p + "/contents" + sp
	if ref != "" {
		u += "?" + url.Values{"ref": {ref}}.Encode()
	}
	var raw json.RawMessage
	if _, err := c.do(ctx, http.MethodGet, u, nil, &raw, true); err != nil {
		return nil, nil, err
	}
	if t := bytes.TrimSpace(raw); len(t) > 0 && t[0] == '[' {
		if err := json.Unmarshal(raw, &dir); err != nil {
			return nil, nil, fmt.Errorf("GitHub sent something unexpected: %w", err)
		}
		if dir == nil {
			dir = []Entry{}
		}
		return dir, nil, nil
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, nil, fmt.Errorf("GitHub sent something unexpected: %w", err)
	}
	return nil, &e, nil
}

// Readme is the README of a directory ("" for the root) at ref.
func (c *Client) Readme(ctx context.Context, full, ref, dir string) (Entry, error) {
	p, err := repoPath(full)
	if err != nil {
		return Entry{}, err
	}
	sp, err := pathSegs(dir)
	if err != nil {
		return Entry{}, err
	}
	u := p + "/readme" + sp
	if ref != "" {
		u += "?" + url.Values{"ref": {ref}}.Encode()
	}
	var e Entry
	_, err = c.do(ctx, http.MethodGet, u, nil, &e, true)
	return e, err
}

// Branches lists up to 200 branches.
func (c *Client) Branches(ctx context.Context, full string) ([]Ref, error) {
	p, err := repoPath(full)
	if err != nil {
		return nil, err
	}
	return pages[Ref](ctx, c, p+"/branches?per_page=100", 2)
}

// Tags lists up to 100 tags, newest first.
func (c *Client) Tags(ctx context.Context, full string) ([]Ref, error) {
	p, err := repoPath(full)
	if err != nil {
		return nil, err
	}
	var out []Ref
	_, err = c.do(ctx, http.MethodGet, p+"/tags?per_page=100", nil, &out, true)
	return out, err
}

// Commits lists commits reachable from ref (optionally touching path).
func (c *Client) Commits(ctx context.Context, full, ref, file string, page, perPage int) ([]CommitInfo, error) {
	p, err := repoPath(full)
	if err != nil {
		return nil, err
	}
	v := url.Values{}
	if ref != "" {
		v.Set("sha", ref)
	}
	if file != "" {
		if _, err := pathSegs(file); err != nil {
			return nil, err
		}
		v.Set("path", file)
	}
	var out []CommitInfo
	_, err = c.do(ctx, http.MethodGet, p+"/commits?"+pageQuery(v, page, perPage).Encode(), nil, &out, true)
	return out, err
}

// Commit reads one commit with its changed files.
func (c *Client) Commit(ctx context.Context, full, ref string) (CommitInfo, error) {
	p, err := repoPath(full)
	if err != nil {
		return CommitInfo{}, err
	}
	if ref == "" || len(ref) > 250 {
		return CommitInfo{}, fmt.Errorf("bad commit %q", ref)
	}
	var out CommitInfo
	_, err = c.do(ctx, http.MethodGet, p+"/commits/"+url.PathEscape(ref), nil, &out, true)
	return out, err
}

// Issues lists issues and pull requests in state (open, closed or all).
func (c *Client) Issues(ctx context.Context, full, state string, page, perPage int) ([]Issue, error) {
	p, err := repoPath(full)
	if err != nil {
		return nil, err
	}
	v := url.Values{"state": {state}, "sort": {"created"}, "direction": {"desc"}}
	var out []Issue
	_, err = c.do(ctx, http.MethodGet, p+"/issues?"+pageQuery(v, page, perPage).Encode(), nil, &out, true)
	return out, err
}

// Issue reads one issue (or a pull request's conversation side).
func (c *Client) Issue(ctx context.Context, full string, number int64) (Issue, error) {
	p, err := repoPath(full)
	if err != nil {
		return Issue{}, err
	}
	var out Issue
	_, err = c.do(ctx, http.MethodGet, p+"/issues/"+strconv.FormatInt(number, 10), nil, &out, true)
	return out, err
}

// IssueComments lists up to 300 conversation comments, oldest first.
func (c *Client) IssueComments(ctx context.Context, full string, number int64) ([]Comment, error) {
	p, err := repoPath(full)
	if err != nil {
		return nil, err
	}
	return pages[Comment](ctx, c, p+"/issues/"+strconv.FormatInt(number, 10)+"/comments?per_page=100", 3)
}

// Pulls lists pull requests in state (open, closed or all).
func (c *Client) Pulls(ctx context.Context, full, state string, page, perPage int) ([]Pull, error) {
	p, err := repoPath(full)
	if err != nil {
		return nil, err
	}
	v := url.Values{"state": {state}, "sort": {"created"}, "direction": {"desc"}}
	var out []Pull
	_, err = c.do(ctx, http.MethodGet, p+"/pulls?"+pageQuery(v, page, perPage).Encode(), nil, &out, true)
	return out, err
}

// PullCached reads one pull request through the cache (Pull never caches).
func (c *Client) PullCached(ctx context.Context, full string, number int64) (Pull, error) {
	p, err := repoPath(full)
	if err != nil {
		return Pull{}, err
	}
	var out Pull
	_, err = c.do(ctx, http.MethodGet, p+"/pulls/"+strconv.FormatInt(number, 10), nil, &out, true)
	return out, err
}

// PullFiles lists up to 300 changed files of a pull request.
func (c *Client) PullFiles(ctx context.Context, full string, number int64) ([]CommitFile, error) {
	p, err := repoPath(full)
	if err != nil {
		return nil, err
	}
	return pages[CommitFile](ctx, c, p+"/pulls/"+strconv.FormatInt(number, 10)+"/files?per_page=100", 3)
}

// ReleasesPage lists releases with their notes.
func (c *Client) ReleasesPage(ctx context.Context, full string, page, perPage int) ([]Release, error) {
	p, err := repoPath(full)
	if err != nil {
		return nil, err
	}
	var out []Release
	_, err = c.do(ctx, http.MethodGet, p+"/releases?"+pageQuery(nil, page, perPage).Encode(), nil, &out, true)
	return out, err
}
