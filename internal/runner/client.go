package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"atelier/internal/actions"
)

// Client speaks the runner protocol (internal/actions/contract.go).
type Client struct {
	BaseURL string // server URL without APIPrefix
	Token   string // runner token
	HTTP    *http.Client
}

// HTTPError is a non-2xx reply.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("server replied %d: %s", e.Status, strings.TrimSpace(e.Body))
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: actions.PollWait + 30*time.Second}
}

func (c *Client) do(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	u := strings.TrimRight(c.BaseURL, "/") + actions.APIPrefix + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "atelier-runner")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &HTTPError{Status: resp.StatusCode, Body: string(b)}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
		return fmt.Errorf("decode %s reply: %w", path, err)
	}
	return nil
}

func (c *Client) Register(ctx context.Context, req actions.RegisterRequest) (*actions.RegisterResponse, error) {
	var out actions.RegisterResponse
	return &out, c.do(ctx, "/register", req, &out)
}

// Poll returns the next job (nil when none). The job is decoded into JobSpec,
// which also carries optional fields the contract may grow.
func (c *Client) Poll(ctx context.Context, req actions.PollRequest) (*JobSpec, error) {
	var out struct {
		Job *JobSpec `json:"job,omitempty"`
	}
	return out.Job, c.do(ctx, "/poll", req, &out)
}

func (c *Client) Logs(ctx context.Context, jobID int64, lines []actions.LogLine) (*actions.LogsResponse, error) {
	var out actions.LogsResponse
	return &out, c.do(ctx, fmt.Sprintf("/jobs/%d/logs", jobID), actions.LogsRequest{Lines: lines}, &out)
}

func (c *Client) Step(ctx context.Context, jobID int64, index int, res StepReport) error {
	return c.do(ctx, fmt.Sprintf("/jobs/%d/steps/%d", jobID, index), res, nil)
}

func (c *Client) Finish(ctx context.Context, jobID int64, res actions.FinishRequest) error {
	return c.do(ctx, fmt.Sprintf("/jobs/%d/finish", jobID), res, nil)
}

func (c *Client) Discovery(ctx context.Context, req actions.DiscoveryRequest) error {
	return c.do(ctx, "/discovery", req, nil)
}

// JobSpec is actions.Job plus optional fields the runner understands if the
// server sends them (unknown fields are ignored by both sides).
type JobSpec struct {
	actions.Job
	// Outputs are the job's `outputs:` expressions, resolved by the runner at the end.
	Outputs map[string]string `json:"outputs,omitempty"`
}

// StepReport is actions.StepResult plus optional extras.
type StepReport struct {
	actions.StepResult
	Conclusion string `json:"conclusion,omitempty"` // differs from Status when continue-on-error hid a failure
	Summary    string `json:"summary,omitempty"`    // GITHUB_STEP_SUMMARY Markdown
}
