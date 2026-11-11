package tether

// Issue writes, labels, milestones and pull request reviews for Tether's
// two-way issue sync. Lists are read with conditional requests (the Cache),
// so asking again when nothing changed costs no rate limit.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Milestone is a GitHub milestone.
type Milestone struct {
	Number      int64      `json:"number"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	State       string     `json:"state"` // open, closed
	DueOn       *time.Time `json:"due_on"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Review is a pull request review.
type Review struct {
	ID          int64     `json:"id"`
	User        User      `json:"user"`
	Body        string    `json:"body"`
	State       string    `json:"state"` // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, PENDING
	HTMLURL     string    `json:"html_url"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// CreateIssue opens an issue. labels are names; milestone 0 means none.
func (c *Client) CreateIssue(ctx context.Context, full, title, body string, labels []string, milestone int64) (Issue, error) {
	p, err := repoPath(full)
	if err != nil {
		return Issue{}, err
	}
	in := map[string]any{"title": title, "body": body}
	if len(labels) > 0 {
		in["labels"] = labels
	}
	if milestone > 0 {
		in["milestone"] = milestone
	}
	var out Issue
	_, err = c.do(ctx, http.MethodPost, p+"/issues", in, &out, false)
	return out, err
}

// UpdateIssue changes an issue. fields are sent as they are: title, body,
// state, state_reason, labels ([]string, replaces them all) and milestone
// (a number, or nil to clear it).
func (c *Client) UpdateIssue(ctx context.Context, full string, number int64, fields map[string]any) (Issue, error) {
	p, err := repoPath(full)
	if err != nil {
		return Issue{}, err
	}
	var out Issue
	_, err = c.do(ctx, http.MethodPatch, p+"/issues/"+strconv.FormatInt(number, 10), fields, &out, false)
	return out, err
}

// RepoLabels lists a repository's labels.
func (c *Client) RepoLabels(ctx context.Context, full string) ([]Label, error) {
	p, err := repoPath(full)
	if err != nil {
		return nil, err
	}
	return pages[Label](ctx, c, p+"/labels?per_page=100", 5)
}

// CreateLabel makes a label; color is six hex digits without '#'.
func (c *Client) CreateLabel(ctx context.Context, full, name, color, description string) (Label, error) {
	p, err := repoPath(full)
	if err != nil {
		return Label{}, err
	}
	var out Label
	_, err = c.do(ctx, http.MethodPost, p+"/labels", map[string]string{"name": name, "color": color, "description": description}, &out, false)
	return out, err
}

// Milestones lists a repository's milestones, open and closed.
func (c *Client) Milestones(ctx context.Context, full string) ([]Milestone, error) {
	p, err := repoPath(full)
	if err != nil {
		return nil, err
	}
	v := url.Values{"state": {"all"}, "per_page": {"100"}}
	return pages[Milestone](ctx, c, p+"/milestones?"+v.Encode(), 5)
}

// CreateMilestone makes a milestone. due may be nil; state is open or closed.
func (c *Client) CreateMilestone(ctx context.Context, full, title, description, state string, due *time.Time) (Milestone, error) {
	p, err := repoPath(full)
	if err != nil {
		return Milestone{}, err
	}
	in := map[string]any{"title": title, "description": description, "state": state}
	if due != nil {
		in["due_on"] = due.UTC().Format(time.RFC3339)
	}
	var out Milestone
	_, err = c.do(ctx, http.MethodPost, p+"/milestones", in, &out, false)
	return out, err
}

// UpdateMilestone changes a milestone's state ("" = keep) and due date
// (setDue: due, which may be nil to clear it).
func (c *Client) UpdateMilestone(ctx context.Context, full string, number int64, state string, setDue bool, due *time.Time) (Milestone, error) {
	p, err := repoPath(full)
	if err != nil {
		return Milestone{}, err
	}
	in := map[string]any{}
	if state != "" {
		in["state"] = state
	}
	if setDue {
		if due == nil {
			in["due_on"] = nil
		} else {
			in["due_on"] = due.UTC().Format(time.RFC3339)
		}
	}
	var out Milestone
	_, err = c.do(ctx, http.MethodPatch, p+"/milestones/"+strconv.FormatInt(number, 10), in, &out, false)
	return out, err
}

// PullReviews lists a pull request's reviews (oldest first).
func (c *Client) PullReviews(ctx context.Context, full string, number int64) ([]Review, error) {
	p, err := repoPath(full)
	if err != nil {
		return nil, err
	}
	return pages[Review](ctx, c, p+"/pulls/"+strconv.FormatInt(number, 10)+"/reviews?per_page=100", 3)
}
