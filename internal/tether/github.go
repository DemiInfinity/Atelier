package tether

// A small GitHub REST client for Tether.
//
//   - Every GET can go through a Cache: the stored ETag is sent as
//     If-None-Match and a 304 answers from the cached body (304s don't count
//     against GitHub's rate limit).
//   - Rate limits are respected: the X-RateLimit-* headers are reported to
//     OnRate, and a 429 or a 403 that says the limit ran out (or carries
//     Retry-After) blocks every later call with the same token until the reset
//     time, without contacting GitHub.
//   - The token is only ever put in the Authorization header. It never
//     appears in errors or logs.
//
// Everything that comes back is untrusted: callers escape it before showing
// it and never execute it.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAPI is GitHub's REST API.
const DefaultAPI = "https://api.github.com"

// maxBody bounds a response body.
const maxBody = 8 << 20

// ErrRateLimited is matched by every *RateLimitError.
var ErrRateLimited = errors.New("tether: GitHub's rate limit ran out")

// RateLimitError says when calls may start again.
type RateLimitError struct{ Reset time.Time }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("GitHub's rate limit ran out; it resets at %s UTC", e.Reset.UTC().Format("15:04"))
}
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// APIError is a non-2xx answer from GitHub.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("GitHub answered %d", e.Status)
	}
	return fmt.Sprintf("GitHub answered %d: %s", e.Status, e.Message)
}

// IsStatus reports whether err is an APIError with that status.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

// Rate is the rate limit as last reported by GitHub.
type Rate struct {
	Limit, Remaining int
	Reset            time.Time
	Resource         string
}

// Cache stores conditional-request validators and bodies.
type Cache interface {
	Get(ctx context.Context, key string) (etag string, body []byte, ok bool)
	Put(ctx context.Context, key, etag string, body []byte)
}

// TimedCache is a Cache that also knows when each body was stored. With it,
// a Client can answer from the cache without asking GitHub (MaxAge) and fall
// back to what it has when GitHub can't be asked (StaleOK).
type TimedCache interface {
	Cache
	GetAt(ctx context.Context, key string) (etag string, body []byte, at time.Time, ok bool)
}

// Client talks to GitHub with one token.
type Client struct {
	API    string // DefaultAPI when empty
	Token  string
	HTTP   *http.Client
	Cache  Cache      // optional
	OnRate func(Rate) // optional

	// MaxAge > 0 (with a TimedCache): cached GETs younger than this are
	// answered from the cache without contacting GitHub.
	MaxAge time.Duration
	// StaleOK (with a TimedCache): when GitHub is rate limited, unreachable or
	// failing (5xx), cached GETs are answered from the cache and OnStale is
	// told how old the answer is.
	StaleOK bool
	OnStale func(at time.Time, cause error)
}

var blocked sync.Map // client key → time.Time

func (c *Client) key() string {
	h := sha256.Sum256([]byte(c.api() + "\x00" + c.Token))
	return hex.EncodeToString(h[:8])
}

func (c *Client) api() string {
	if c.API == "" {
		return DefaultAPI
	}
	return strings.TrimRight(c.API, "/")
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// BlockedUntil reports when a rate-limited client may call again.
func (c *Client) BlockedUntil() (time.Time, bool) {
	if v, ok := blocked.Load(c.key()); ok {
		if t := v.(time.Time); time.Now().Before(t) {
			return t, true
		}
		blocked.Delete(c.key())
	}
	return time.Time{}, false
}

// Unblock forgets a rate-limit block (tests, or after a token change).
func (c *Client) Unblock() { blocked.Delete(c.key()) }

func (c *Client) block(until time.Time) {
	if until.Before(time.Now().Add(5 * time.Second)) {
		until = time.Now().Add(time.Minute)
	}
	blocked.Store(c.key(), until)
}

// do sends one request. out may be nil. When cached is true and a Cache is
// set, the response is stored and a 304 is answered from it.
func (c *Client) do(ctx context.Context, method, path string, in, out any, cached bool) (http.Header, error) {
	cacheKey := method + " " + path
	if c.fresh(ctx, cached, method, cacheKey, out) {
		return http.Header{}, nil
	}
	if until, ok := c.BlockedUntil(); ok {
		err := &RateLimitError{Reset: until}
		if c.stale(ctx, cached, method, cacheKey, out, err) {
			return http.Header{}, nil
		}
		return nil, err
	}
	u := path
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = c.api() + path
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "Atelier-Tether")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	var cachedBody []byte
	if cached && c.Cache != nil && method == http.MethodGet {
		if etag, b, ok := c.Cache.Get(ctx, cacheKey); ok && etag != "" {
			req.Header.Set("If-None-Match", etag)
			cachedBody = b
		}
	}
	resp, err := c.http().Do(req)
	if err != nil {
		err = fmt.Errorf("couldn't reach GitHub: %w", scrubErr(err))
		if ctx.Err() == nil && c.stale(ctx, cached, method, cacheKey, out, err) {
			return http.Header{}, nil
		}
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.Header, err
	}
	c.noteRate(resp)
	switch {
	case resp.StatusCode == http.StatusNotModified && cachedBody != nil:
		raw = cachedBody
		if c.MaxAge > 0 {
			// Still current: restart its MaxAge.
			c.Cache.Put(ctx, cacheKey, req.Header.Get("If-None-Match"), raw)
		}
	case limited(resp, raw):
		reset := resetTime(resp.Header)
		c.block(reset)
		rl := &RateLimitError{Reset: reset}
		if c.stale(ctx, cached, method, cacheKey, out, rl) {
			return resp.Header, nil
		}
		return resp.Header, rl
	case resp.StatusCode >= 500 && c.stale(ctx, cached, method, cacheKey, out, &APIError{Status: resp.StatusCode}):
		return resp.Header, nil
	case resp.StatusCode >= 300:
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &m)
		msg := m.Message
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return resp.Header, &APIError{Status: resp.StatusCode, Message: msg}
	default:
		if cached && c.Cache != nil && method == http.MethodGet {
			if etag := resp.Header.Get("ETag"); etag != "" {
				c.Cache.Put(ctx, cacheKey, etag, raw)
			}
		}
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.Header, fmt.Errorf("GitHub sent something unexpected: %w", err)
		}
	}
	return resp.Header, nil
}

// fresh answers a GET from a TimedCache entry younger than MaxAge.
func (c *Client) fresh(ctx context.Context, cached bool, method, key string, out any) bool {
	if !cached || c.MaxAge <= 0 || method != http.MethodGet {
		return false
	}
	tc, ok := c.Cache.(TimedCache)
	if !ok {
		return false
	}
	_, body, at, ok := tc.GetAt(ctx, key)
	if !ok || time.Since(at) >= c.MaxAge || time.Since(at) < 0 {
		return false
	}
	return out == nil || json.Unmarshal(body, out) == nil
}

// stale answers a GET from whatever a TimedCache holds, when allowed.
func (c *Client) stale(ctx context.Context, cached bool, method, key string, out any, cause error) bool {
	if !c.StaleOK || !cached || method != http.MethodGet {
		return false
	}
	tc, ok := c.Cache.(TimedCache)
	if !ok {
		return false
	}
	_, body, at, ok := tc.GetAt(ctx, key)
	if !ok || (out != nil && json.Unmarshal(body, out) != nil) {
		return false
	}
	if c.OnStale != nil {
		c.OnStale(at, cause)
	}
	return true
}

func scrubErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func (c *Client) noteRate(resp *http.Response) {
	h := resp.Header
	rem, err := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	if err != nil {
		return
	}
	lim, _ := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	r := Rate{Limit: lim, Remaining: rem, Reset: resetTime(h), Resource: h.Get("X-RateLimit-Resource")}
	if rem == 0 && r.Resource != "search" {
		// The core limit ran out: stop before GitHub has to say so.
		c.block(r.Reset)
	}
	if c.OnRate != nil {
		c.OnRate(r)
	}
}

func limited(resp *http.Response, body []byte) bool {
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" {
			return true
		}
		return bytes.Contains(bytes.ToLower(body), []byte("rate limit"))
	}
	return false
}

func resetTime(h http.Header) time.Time {
	if s := h.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return time.Now().Add(time.Duration(n) * time.Second)
		}
	}
	if s := h.Get("X-RateLimit-Reset"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.Unix(n, 0)
		}
	}
	return time.Now().Add(time.Minute)
}

// ---------- names ----------

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)
var loginRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// ValidRepo checks a GitHub "owner/repo".
func ValidRepo(s string) bool {
	if !repoNameRe.MatchString(s) {
		return false
	}
	_, name, _ := strings.Cut(s, "/")
	return name != "." && name != ".." && !strings.HasSuffix(strings.ToLower(name), ".git")
}

// ValidLogin checks a GitHub user or organisation name.
func ValidLogin(s string) bool { return loginRe.MatchString(s) }

func repoPath(full string) (string, error) {
	if !ValidRepo(full) {
		return "", fmt.Errorf("%q isn't a GitHub owner/repo", full)
	}
	o, n, _ := strings.Cut(full, "/")
	return "/repos/" + url.PathEscape(o) + "/" + url.PathEscape(n), nil
}

// ---------- types (only the fields Tether uses) ----------

type User struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
	HTMLURL   string `json:"html_url"`
	Type      string `json:"type"`
}

type Repo struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	FullName      string    `json:"full_name"`
	Description   string    `json:"description"`
	HTMLURL       string    `json:"html_url"`
	Language      string    `json:"language"`
	DefaultBranch string    `json:"default_branch"`
	Private       bool      `json:"private"`
	Fork          bool      `json:"fork"`
	Archived      bool      `json:"archived"`
	Stars         int       `json:"stargazers_count"`
	Forks         int       `json:"forks_count"`
	Owner         User      `json:"owner"`
	CreatedAt     time.Time `json:"created_at"`
	PushedAt      time.Time `json:"pushed_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	Homepage      string    `json:"homepage"`
	Topics        []string  `json:"topics"`
	OpenIssues    int       `json:"open_issues_count"`
	Watchers      int       `json:"subscribers_count"`
	License       *struct {
		Key    string `json:"key"`
		Name   string `json:"name"`
		SPDXID string `json:"spdx_id"`
	} `json:"license"`
	Parent *struct {
		FullName string `json:"full_name"`
	} `json:"parent,omitempty"`
	Permissions *struct {
		Admin bool `json:"admin"`
		Push  bool `json:"push"`
		Pull  bool `json:"pull"`
	} `json:"permissions,omitempty"`
}

type Issue struct {
	Number        int64      `json:"number"`
	Title         string     `json:"title"`
	Body          string     `json:"body"`
	State         string     `json:"state"`
	StateReason   string     `json:"state_reason"`
	HTMLURL       string     `json:"html_url"`
	RepositoryURL string     `json:"repository_url"`
	User          User       `json:"user"`
	Comments      int        `json:"comments"`
	Labels        []Label    `json:"labels"`
	Milestone     *Milestone `json:"milestone,omitempty"`
	PullRequest   *struct{}  `json:"pull_request,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ClosedAt      *time.Time `json:"closed_at"`
}

// IsPull reports whether the issue is a pull request.
func (i Issue) IsPull() bool { return i.PullRequest != nil }

// Repo is "owner/repo" from the repository_url.
func (i Issue) Repo() string {
	u := i.RepositoryURL
	if j := strings.Index(u, "/repos/"); j >= 0 {
		return u[j+len("/repos/"):]
	}
	return ""
}

type Comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	IssueURL  string    `json:"issue_url"`
	User      User      `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// IssueNumber is the number at the end of issue_url.
func (c Comment) IssueNumber() int64 {
	i := strings.LastIndex(c.IssueURL, "/")
	if i < 0 {
		return 0
	}
	n, _ := strconv.ParseInt(c.IssueURL[i+1:], 10, 64)
	return n
}

type Branch struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *Repo  `json:"repo"`
}

type Pull struct {
	Number         int64      `json:"number"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	State          string     `json:"state"`
	HTMLURL        string     `json:"html_url"`
	User           User       `json:"user"`
	Head           Branch     `json:"head"`
	Base           Branch     `json:"base"`
	Draft          bool       `json:"draft"`
	Merged         bool       `json:"merged"`
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	Comments       int        `json:"comments"`
	Commits        int        `json:"commits"`
	Additions      int        `json:"additions"`
	Deletions      int        `json:"deletions"`
	ChangedFiles   int        `json:"changed_files"`
	Labels         []Label    `json:"labels"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ClosedAt       *time.Time `json:"closed_at"`
}

type Event struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Actor struct {
		Login        string `json:"login"`
		DisplayLogin string `json:"display_login"`
		AvatarURL    string `json:"avatar_url"`
	} `json:"actor"`
	Repo struct {
		Name string `json:"name"`
	} `json:"repo"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

type Release struct {
	ID          int64     `json:"id"`
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	HTMLURL     string    `json:"html_url"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Author      User      `json:"author"`
	Body        string    `json:"body"`
}

// Label is an issue or pull request label. Its colour and description are
// untrusted: the colour is only used after validation as six hex digits, the
// description only as text.
type Label struct {
	Name        string `json:"name"`
	Color       string `json:"color,omitempty"`
	Description string `json:"description,omitempty"`
}

type Status struct {
	State       string `json:"state"` // error, failure, pending, success
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description,omitempty"`
	Context     string `json:"context"`
}

type Content struct {
	SHA      string `json:"sha"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

// Decoded returns the file's bytes.
func (c Content) Decoded() ([]byte, error) {
	if c.Encoding != "base64" {
		return []byte(c.Content), nil
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(c.Content, "\n", ""))
}

// ---------- calls ----------

// Me is the token's user.
func (c *Client) Me(ctx context.Context) (User, http.Header, error) {
	var u User
	h, err := c.do(ctx, http.MethodGet, "/user", nil, &u, false)
	return u, h, err
}

// RateLimit asks GitHub for the current limits (free: doesn't count).
func (c *Client) RateLimit(ctx context.Context) (Rate, error) {
	var out struct {
		Resources struct {
			Core struct {
				Limit     int   `json:"limit"`
				Remaining int   `json:"remaining"`
				Reset     int64 `json:"reset"`
			} `json:"core"`
		} `json:"resources"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/rate_limit", nil, &out, false); err != nil {
		return Rate{}, err
	}
	co := out.Resources.Core
	return Rate{Limit: co.Limit, Remaining: co.Remaining, Reset: time.Unix(co.Reset, 0), Resource: "core"}, nil
}

func (c *Client) Repo(ctx context.Context, full string) (Repo, error) {
	p, err := repoPath(full)
	if err != nil {
		return Repo{}, err
	}
	var r Repo
	_, err = c.do(ctx, http.MethodGet, p, nil, &r, true)
	return r, err
}

// CreateRepo makes a repository for the token's user.
func (c *Client) CreateRepo(ctx context.Context, name, description string, private bool) (Repo, error) {
	var r Repo
	_, err := c.do(ctx, http.MethodPost, "/user/repos", map[string]any{
		"name": name, "description": description, "private": private, "auto_init": false,
	}, &r, false)
	return r, err
}

// SearchRepos runs a repository search.
func (c *Client) SearchRepos(ctx context.Context, q, sort string, perPage int) ([]Repo, int, error) {
	v := url.Values{"q": {q}, "per_page": {strconv.Itoa(perPage)}}
	if sort != "" {
		v.Set("sort", sort)
		v.Set("order", "desc")
	}
	var out struct {
		Total int    `json:"total_count"`
		Items []Repo `json:"items"`
	}
	_, err := c.do(ctx, http.MethodGet, "/search/repositories?"+v.Encode(), nil, &out, true)
	return out.Items, out.Total, err
}

// SearchIssues runs an issue and pull request search.
func (c *Client) SearchIssues(ctx context.Context, q string, perPage int) ([]Issue, int, error) {
	v := url.Values{"q": {q}, "per_page": {strconv.Itoa(perPage)}}
	var out struct {
		Total int     `json:"total_count"`
		Items []Issue `json:"items"`
	}
	_, err := c.do(ctx, http.MethodGet, "/search/issues?"+v.Encode(), nil, &out, true)
	return out.Items, out.Total, err
}

// ReceivedEvents is the activity of people and repositories login follows.
func (c *Client) ReceivedEvents(ctx context.Context, login string, perPage int) ([]Event, error) {
	if !ValidLogin(login) {
		return nil, fmt.Errorf("%q isn't a GitHub login", login)
	}
	var out []Event
	_, err := c.do(ctx, http.MethodGet, "/users/"+url.PathEscape(login)+"/received_events?per_page="+strconv.Itoa(perPage), nil, &out, true)
	return out, err
}

// Starred lists the token user's starred repositories, recently starred first.
// Reading /user/starred needs the optional Starring permission; without it,
// login's public stars (which need no permission) are used instead.
func (c *Client) Starred(ctx context.Context, login string, perPage int) ([]Repo, error) {
	var out []Repo
	_, err := c.do(ctx, http.MethodGet, "/user/starred?sort=updated&per_page="+strconv.Itoa(perPage), nil, &out, true)
	if err == nil || login == "" {
		return out, err
	}
	out = nil
	_, err2 := c.do(ctx, http.MethodGet, "/users/"+url.PathEscape(login)+"/starred?sort=updated&per_page="+strconv.Itoa(perPage), nil, &out, true)
	if err2 != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) Releases(ctx context.Context, full string, perPage int) ([]Release, error) {
	p, err := repoPath(full)
	if err != nil {
		return nil, err
	}
	var out []Release
	_, err = c.do(ctx, http.MethodGet, p+"/releases?per_page="+strconv.Itoa(perPage), nil, &out, true)
	return out, err
}

var nextLinkRe = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)

// pages follows Link: rel="next" up to max pages.
func pages[T any](ctx context.Context, c *Client, first string, max int) ([]T, error) {
	var all []T
	u := first
	for i := 0; i < max && u != ""; i++ {
		var batch []T
		h, err := c.do(ctx, http.MethodGet, u, nil, &batch, i == 0)
		if err != nil {
			return all, err
		}
		all = append(all, batch...)
		u = ""
		if m := nextLinkRe.FindStringSubmatch(h.Get("Link")); m != nil && strings.HasPrefix(m[1], c.api()+"/") {
			u = m[1]
		}
	}
	return all, nil
}

// IssuesSince lists issues and pull requests updated since t (oldest first).
func (c *Client) IssuesSince(ctx context.Context, full string, since time.Time) ([]Issue, error) {
	p, err := repoPath(full)
	if err != nil {
		return nil, err
	}
	v := url.Values{"state": {"all"}, "sort": {"updated"}, "direction": {"asc"}, "per_page": {"100"}}
	if !since.IsZero() {
		v.Set("since", since.UTC().Format(time.RFC3339))
	}
	return pages[Issue](ctx, c, p+"/issues?"+v.Encode(), 10)
}

// CommentsSince lists issue and pull request conversation comments updated since t.
func (c *Client) CommentsSince(ctx context.Context, full string, since time.Time) ([]Comment, error) {
	p, err := repoPath(full)
	if err != nil {
		return nil, err
	}
	v := url.Values{"sort": {"updated"}, "direction": {"asc"}, "per_page": {"100"}}
	if !since.IsZero() {
		v.Set("since", since.UTC().Format(time.RFC3339))
	}
	return pages[Comment](ctx, c, p+"/issues/comments?"+v.Encode(), 10)
}

func (c *Client) OpenPulls(ctx context.Context, full string) ([]Pull, error) {
	p, err := repoPath(full)
	if err != nil {
		return nil, err
	}
	return pages[Pull](ctx, c, p+"/pulls?state=open&per_page=100", 5)
}

func (c *Client) Pull(ctx context.Context, full string, number int64) (Pull, error) {
	p, err := repoPath(full)
	if err != nil {
		return Pull{}, err
	}
	var out Pull
	_, err = c.do(ctx, http.MethodGet, p+"/pulls/"+strconv.FormatInt(number, 10), nil, &out, false)
	return out, err
}

func (c *Client) CreatePull(ctx context.Context, full, title, head, base, body string) (Pull, error) {
	p, err := repoPath(full)
	if err != nil {
		return Pull{}, err
	}
	var out Pull
	_, err = c.do(ctx, http.MethodPost, p+"/pulls", map[string]any{"title": title, "head": head, "base": base, "body": body}, &out, false)
	return out, err
}

// UpdatePull changes title, body, base or state ("open"/"closed"); empty fields stay.
func (c *Client) UpdatePull(ctx context.Context, full string, number int64, fields map[string]string) (Pull, error) {
	p, err := repoPath(full)
	if err != nil {
		return Pull{}, err
	}
	in := map[string]string{}
	for k, v := range fields {
		if v != "" {
			in[k] = v
		}
	}
	var out Pull
	_, err = c.do(ctx, http.MethodPatch, p+"/pulls/"+strconv.FormatInt(number, 10), in, &out, false)
	return out, err
}

// MergePull merges on GitHub. method is merge, squash or rebase; sha (optional)
// makes GitHub refuse if the head moved.
func (c *Client) MergePull(ctx context.Context, full string, number int64, method, sha, title string) (string, error) {
	p, err := repoPath(full)
	if err != nil {
		return "", err
	}
	in := map[string]string{"merge_method": method}
	if sha != "" {
		in["sha"] = sha
	}
	if title != "" {
		in["commit_title"] = title
	}
	var out struct {
		SHA    string `json:"sha"`
		Merged bool   `json:"merged"`
	}
	_, err = c.do(ctx, http.MethodPut, p+"/pulls/"+strconv.FormatInt(number, 10)+"/merge", in, &out, false)
	return out.SHA, err
}

func (c *Client) CreateComment(ctx context.Context, full string, number int64, body string) (Comment, error) {
	p, err := repoPath(full)
	if err != nil {
		return Comment{}, err
	}
	var out Comment
	_, err = c.do(ctx, http.MethodPost, p+"/issues/"+strconv.FormatInt(number, 10)+"/comments", map[string]string{"body": body}, &out, false)
	return out, err
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (c *Client) CreateStatus(ctx context.Context, full, sha string, s Status) error {
	p, err := repoPath(full)
	if err != nil {
		return err
	}
	if !shaRe.MatchString(sha) {
		return fmt.Errorf("%q isn't a commit id", sha)
	}
	_, err = c.do(ctx, http.MethodPost, p+"/statuses/"+sha, s, nil, false)
	return err
}

func contentPath(full, file string) (string, error) {
	p, err := repoPath(full)
	if err != nil {
		return "", err
	}
	var segs []string
	for _, s := range strings.Split(file, "/") {
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("bad path %q", file)
		}
		segs = append(segs, url.PathEscape(s))
	}
	return p + "/contents/" + strings.Join(segs, "/"), nil
}

// GetContent reads a file (with a conditional request when cached).
func (c *Client) GetContent(ctx context.Context, full, file string) (Content, error) {
	p, err := contentPath(full, file)
	if err != nil {
		return Content{}, err
	}
	var out Content
	_, err = c.do(ctx, http.MethodGet, p, nil, &out, true)
	return out, err
}

// PutContent creates or replaces a file; sha is the current blob for a replace.
func (c *Client) PutContent(ctx context.Context, full, file, message string, data []byte, sha string) error {
	p, err := contentPath(full, file)
	if err != nil {
		return err
	}
	in := map[string]string{"message": message, "content": base64.StdEncoding.EncodeToString(data)}
	if sha != "" {
		in["sha"] = sha
	}
	_, err = c.do(ctx, http.MethodPut, p, in, nil, false)
	return err
}

func (c *Client) DeleteContent(ctx context.Context, full, file, message, sha string) error {
	p, err := contentPath(full, file)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodDelete, p, map[string]string{"message": message, "sha": sha}, nil, false)
	return err
}

// BranchSHA is the commit a branch points at on GitHub.
func (c *Client) BranchSHA(ctx context.Context, full, branch string) (string, error) {
	p, err := repoPath(full)
	if err != nil {
		return "", err
	}
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	_, err = c.do(ctx, http.MethodGet, p+"/branches/"+url.PathEscape(branch), nil, &out, false)
	return out.Commit.SHA, err
}

// ReviewComment is a pull request review comment on a line.
type ReviewComment struct {
	Comment
	Path           string `json:"path"`
	Line           int    `json:"line"`
	PullRequestURL string `json:"pull_request_url"`
}

// PullNumber is the number at the end of pull_request_url.
func (c ReviewComment) PullNumber() int64 {
	i := strings.LastIndex(c.PullRequestURL, "/")
	if i < 0 {
		return 0
	}
	n, _ := strconv.ParseInt(c.PullRequestURL[i+1:], 10, 64)
	return n
}

// ReviewCommentsSince lists pull request review comments updated since t.
func (c *Client) ReviewCommentsSince(ctx context.Context, full string, since time.Time) ([]ReviewComment, error) {
	p, err := repoPath(full)
	if err != nil {
		return nil, err
	}
	v := url.Values{"sort": {"updated"}, "direction": {"asc"}, "per_page": {"100"}}
	if !since.IsZero() {
		v.Set("since", since.UTC().Format(time.RFC3339))
	}
	return pages[ReviewComment](ctx, c, p+"/pulls/comments?"+v.Encode(), 10)
}
