package importer

// API clients for importing issues, pull requests, labels and milestones from
// GitHub and Gitea/Forgejo. Both are reduced to the same small set of types.
//
// The token only ever goes to the API base the owner chose: pagination URLs
// are built here (page=N), never taken from the server's answers.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrRateLimited means the source's API rate limit ran out.
var ErrRateLimited = errors.New("the API rate limit ran out")

// RateLimitError says when the limit resets (zero if unknown).
type RateLimitError struct{ Reset time.Time }

func (e *RateLimitError) Error() string        { return ErrRateLimited.Error() }
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

var errAPINotFound = errors.New("not found at the source")

type apiClient struct {
	hc        *http.Client
	auth      string
	host      string
	exhausted bool
	reset     time.Time
}

func newAPIClient(hc *http.Client, base, auth string) (*apiClient, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return nil, errors.New("bad API address")
	}
	if hc == nil {
		hc = DefaultHTTP
	}
	return &apiClient{hc: hc, auth: auth, host: u.Host}, nil
}

func (c *apiClient) noteLimit(h http.Header) {
	if h.Get("X-RateLimit-Remaining") != "0" {
		return
	}
	c.exhausted = true
	if n, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && n > 0 {
		c.reset = time.Unix(n, 0)
	}
}

// get fetches one JSON document. Once the rate limit is used up it refuses
// further requests instead of spending more of it.
func (c *apiClient) get(ctx context.Context, u string, v any) (http.Header, error) {
	if c.exhausted {
		return nil, &RateLimitError{Reset: c.reset}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if req.URL.Host != c.host {
		return nil, errors.New("refusing to send the token to another host")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Atelier-importer")
	if c.auth != "" {
		req.Header.Set("Authorization", c.auth)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("couldn't reach %s: %v", req.URL.Host, ue.Err)
		}
		return nil, err
	}
	defer resp.Body.Close()
	c.noteLimit(resp.Header)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusTooManyRequests:
		if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			c.exhausted = true
			c.reset = time.Now().Add(time.Duration(ra) * time.Second)
		}
		if c.exhausted || resp.StatusCode == http.StatusTooManyRequests {
			c.exhausted = true
			return nil, &RateLimitError{Reset: c.reset}
		}
		return nil, ErrUnauthorized
	case http.StatusUnauthorized:
		return nil, ErrUnauthorized
	case http.StatusNotFound, http.StatusGone:
		return nil, errAPINotFound
	default:
		return nil, fmt.Errorf("%s answered %s", req.URL.Host, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(v); err != nil {
		return nil, fmt.Errorf("unexpected answer from %s: %v", req.URL.Host, err)
	}
	return resp.Header, nil
}

// ---------- normalised source data ----------

type srcLabel struct {
	Name, Color, Description string
}

type srcMilestone struct {
	Title, Description, State string
	Due                       *time.Time
	Created, Updated          time.Time
	Closed                    *time.Time
}

type srcIssue struct {
	Number           int64
	Title, Body      string
	State            string
	StateReason      string
	Author           string
	Labels           []srcLabel
	Milestone        *srcMilestone
	Comments         int
	Created, Updated time.Time
	Closed           *time.Time
	URL              string
	IsPull           bool
}

type srcComment struct {
	ID               int64
	Author, Body     string
	Created, Updated time.Time
}

type srcPull struct {
	Head, Base       string // branch names
	HeadLabel        string // owner:branch
	HeadRepo         string // owner/name the head branch lives in ("" if gone)
	HeadSHA, BaseSHA string
	Merged           bool
	MergedAt         *time.Time
	MergeSHA         string
	MergedBy         string
}

// ---------- JSON shapes shared by GitHub and Gitea ----------

type apiUser struct {
	Login    string `json:"login"`
	Username string `json:"username"`
}

func (u *apiUser) name() string {
	if u == nil {
		return "ghost"
	}
	if u.Login != "" {
		return u.Login
	}
	if u.Username != "" {
		return u.Username
	}
	return "ghost"
}

type apiLabel struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type apiMilestone struct {
	Title       string     `json:"title"`
	Description string     `json:"description"`
	State       string     `json:"state"`
	DueOn       *time.Time `json:"due_on"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   *time.Time `json:"updated_at"`
	ClosedAt    *time.Time `json:"closed_at"`
}

func (m apiMilestone) src() srcMilestone {
	up := m.CreatedAt
	if m.UpdatedAt != nil {
		up = *m.UpdatedAt
	}
	return srcMilestone{Title: m.Title, Description: m.Description, State: strings.ToLower(m.State), Due: m.DueOn,
		Created: m.CreatedAt, Updated: up, Closed: m.ClosedAt}
}

type apiIssue struct {
	Number      int64         `json:"number"`
	Title       string        `json:"title"`
	Body        string        `json:"body"`
	State       string        `json:"state"`
	StateReason string        `json:"state_reason"`
	User        *apiUser      `json:"user"`
	Labels      []apiLabel    `json:"labels"`
	Milestone   *apiMilestone `json:"milestone"`
	Comments    int           `json:"comments"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	ClosedAt    *time.Time    `json:"closed_at"`
	HTMLURL     string        `json:"html_url"`
	PullRequest *struct{}     `json:"pull_request"`
}

func (a apiIssue) src() srcIssue {
	it := srcIssue{Number: a.Number, Title: a.Title, Body: a.Body, State: strings.ToLower(a.State),
		StateReason: a.StateReason, Author: a.User.name(), Comments: a.Comments, Created: a.CreatedAt,
		Updated: a.UpdatedAt, Closed: a.ClosedAt, URL: a.HTMLURL, IsPull: a.PullRequest != nil}
	if it.Updated.IsZero() {
		it.Updated = it.Created
	}
	for _, l := range a.Labels {
		it.Labels = append(it.Labels, srcLabel(l))
	}
	if a.Milestone != nil {
		m := a.Milestone.src()
		it.Milestone = &m
	}
	return it
}

type apiComment struct {
	ID        int64      `json:"id"`
	User      *apiUser   `json:"user"`
	Body      string     `json:"body"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt *time.Time `json:"updated_at"`
}

func (a apiComment) src() srcComment {
	up := a.CreatedAt
	if a.UpdatedAt != nil {
		up = *a.UpdatedAt
	}
	return srcComment{ID: a.ID, Author: a.User.name(), Body: a.Body, Created: a.CreatedAt, Updated: up}
}

type apiBranch struct {
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
	Label string `json:"label"`
	Repo  *struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

type apiPull struct {
	Head           apiBranch  `json:"head"`
	Base           apiBranch  `json:"base"`
	Merged         bool       `json:"merged"`
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	MergedBy       *apiUser   `json:"merged_by"`
}

func (a apiPull) src() srcPull {
	p := srcPull{Head: a.Head.Ref, Base: a.Base.Ref, HeadLabel: a.Head.Label, HeadSHA: a.Head.SHA, BaseSHA: a.Base.SHA,
		Merged: a.Merged || a.MergedAt != nil, MergedAt: a.MergedAt, MergeSHA: a.MergeCommitSHA}
	if a.Head.Repo != nil {
		p.HeadRepo = a.Head.Repo.FullName
	}
	if p.Merged && a.MergedBy != nil {
		p.MergedBy = a.MergedBy.name()
	}
	return p
}

// ---------- sources ----------

type metaSource interface {
	prefix() string // "github" or "gitea", for author names
	labels(ctx context.Context) ([]srcLabel, error)
	milestones(ctx context.Context) ([]srcMilestone, error)
	// issues lists issues and/or pull requests (as issues), oldest first.
	issues(ctx context.Context, wantIssues, wantPulls bool, page func(n int)) ([]srcIssue, error)
	comments(ctx context.Context, number int64) ([]srcComment, error)
	pull(ctx context.Context, number int64) (srcPull, error)
}

func repoPath(fullName string) (string, error) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("%q isn't an owner/name", fullName)
	}
	return url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}

// GitHub: page-numbered, 100 per page, more pages while Link has rel="next".
type githubSource struct {
	c    *apiClient
	base string // https://api.github.com/repos/o/r
}

func (g *githubSource) prefix() string { return "github" }

func ghPages[T any](ctx context.Context, c *apiClient, u string, each func([]T), page func(n int)) error {
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	for p := 1; p <= maxPages*10; p++ {
		var batch []T
		h, err := c.get(ctx, fmt.Sprintf("%s%sper_page=100&page=%d", u, sep, p), &batch)
		if err != nil {
			return err
		}
		each(batch)
		if page != nil {
			page(p)
		}
		if len(batch) == 0 || nextLink.FindStringSubmatch(h.Get("Link")) == nil {
			return nil
		}
	}
	return nil
}

func (g *githubSource) labels(ctx context.Context) ([]srcLabel, error) {
	var out []srcLabel
	err := ghPages(ctx, g.c, g.base+"/labels", func(b []apiLabel) {
		for _, l := range b {
			out = append(out, srcLabel(l))
		}
	}, nil)
	return out, err
}

func (g *githubSource) milestones(ctx context.Context) ([]srcMilestone, error) {
	var out []srcMilestone
	err := ghPages(ctx, g.c, g.base+"/milestones?state=all", func(b []apiMilestone) {
		for _, m := range b {
			out = append(out, m.src())
		}
	}, nil)
	return out, err
}

// GitHub's issues list includes pull requests (marked by "pull_request").
func (g *githubSource) issues(ctx context.Context, wantIssues, wantPulls bool, page func(n int)) ([]srcIssue, error) {
	var out []srcIssue
	err := ghPages(ctx, g.c, g.base+"/issues?state=all&sort=created&direction=asc", func(b []apiIssue) {
		for _, a := range b {
			it := a.src()
			if (it.IsPull && wantPulls) || (!it.IsPull && wantIssues) {
				out = append(out, it)
			}
		}
	}, page)
	return out, err
}

func (g *githubSource) comments(ctx context.Context, number int64) ([]srcComment, error) {
	var out []srcComment
	err := ghPages(ctx, g.c, fmt.Sprintf("%s/issues/%d/comments", g.base, number), func(b []apiComment) {
		for _, c := range b {
			out = append(out, c.src())
		}
	}, nil)
	return out, err
}

func (g *githubSource) pull(ctx context.Context, number int64) (srcPull, error) {
	var a apiPull
	_, err := g.c.get(ctx, fmt.Sprintf("%s/pulls/%d", g.base, number), &a)
	return a.src(), err
}

// Gitea/Forgejo: limit/page; a short page is the last one.
type giteaSource struct {
	c    *apiClient
	base string // https://gitea.example.com/api/v1/repos/o/r
}

func (g *giteaSource) prefix() string { return "gitea" }

const giteaLimit = 50

// giteaPages fetches pages until a short one. key de-duplicates items in case
// an older server ignores paging and repeats the same list.
func giteaPages[T any](ctx context.Context, c *apiClient, u string, key func(T) string, each func(T), page func(n int)) error {
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	seen := map[string]bool{}
	for p := 1; p <= maxPages*10; p++ {
		var batch []T
		if _, err := c.get(ctx, fmt.Sprintf("%s%slimit=%d&page=%d", u, sep, giteaLimit, p), &batch); err != nil {
			return err
		}
		added := 0
		for _, it := range batch {
			k := key(it)
			if seen[k] {
				continue
			}
			seen[k] = true
			added++
			each(it)
		}
		if page != nil {
			page(p)
		}
		if len(batch) < giteaLimit || added == 0 {
			return nil
		}
	}
	return nil
}

func (g *giteaSource) labels(ctx context.Context) ([]srcLabel, error) {
	var out []srcLabel
	err := giteaPages(ctx, g.c, g.base+"/labels", func(l apiLabel) string { return strings.ToLower(l.Name) },
		func(l apiLabel) { out = append(out, srcLabel(l)) }, nil)
	return out, err
}

func (g *giteaSource) milestones(ctx context.Context) ([]srcMilestone, error) {
	var out []srcMilestone
	err := giteaPages(ctx, g.c, g.base+"/milestones?state=all", func(m apiMilestone) string { return strings.ToLower(m.Title) },
		func(m apiMilestone) { out = append(out, m.src()) }, nil)
	return out, err
}

func (g *giteaSource) issues(ctx context.Context, wantIssues, wantPulls bool, page func(n int)) ([]srcIssue, error) {
	var out []srcIssue
	pages := 0
	list := func(typ string) error {
		return giteaPages(ctx, g.c, g.base+"/issues?state=all&type="+typ,
			func(a apiIssue) string { return strconv.FormatInt(a.Number, 10) },
			func(a apiIssue) {
				it := a.src()
				it.IsPull = it.IsPull || typ == "pulls"
				out = append(out, it)
			}, func(int) {
				pages++
				if page != nil {
					page(pages)
				}
			})
	}
	if wantIssues {
		if err := list("issues"); err != nil {
			return out, err
		}
	}
	if wantPulls {
		if err := list("pulls"); err != nil {
			return out, err
		}
	}
	// Oldest first, like GitHub.
	sortIssues(out)
	return out, nil
}

func sortIssues(s []srcIssue) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Number < s[j-1].Number; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func (g *giteaSource) comments(ctx context.Context, number int64) ([]srcComment, error) {
	var out []srcComment
	err := giteaPages(ctx, g.c, fmt.Sprintf("%s/issues/%d/comments", g.base, number),
		func(c apiComment) string { return strconv.FormatInt(c.ID, 10) },
		func(c apiComment) { out = append(out, c.src()) }, nil)
	return out, err
}

func (g *giteaSource) pull(ctx context.Context, number int64) (srcPull, error) {
	var a apiPull
	_, err := g.c.get(ctx, fmt.Sprintf("%s/pulls/%d", g.base, number), &a)
	return a.src(), err
}

// newMetaSource builds the client for one source repository.
func newMetaSource(hc *http.Client, source, apiBase, repo, token string) (metaSource, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	switch source {
	case "github":
		if apiBase == "" {
			apiBase = GitHubAPI
		}
		apiBase = strings.TrimRight(apiBase, "/")
		auth := ""
		if token != "" {
			auth = "Bearer " + token
		}
		c, err := newAPIClient(hc, apiBase, auth)
		if err != nil {
			return nil, err
		}
		return &githubSource{c: c, base: apiBase + "/repos/" + rp}, nil
	case "gitea":
		base, err := NormalizeServer(apiBase)
		if err != nil {
			return nil, err
		}
		auth := ""
		if token != "" {
			auth = "token " + token
		}
		c, err := newAPIClient(hc, base, auth)
		if err != nil {
			return nil, err
		}
		return &giteaSource{c: c, base: base + "/api/v1/repos/" + rp}, nil
	}
	return nil, fmt.Errorf("issues can't be imported from %q", source)
}
