// Package actions holds the contract between the Atelier server and
// atelier-runner, plus the small hooks other features use (secrets).
//
// The server owns workflows, runs, the job queue and logs. A runner is a small
// separate program on one of your devices. It only ever connects out to the
// server (so laptops behind home Wi-Fi need no open ports), asks for work,
// runs it, and reports back.
//
// # Protocol (JSON over HTTPS, all under /api/runner/v1)
//
//	POST /register                 one-time registration token → runner credentials
//	POST /poll                     long-poll (≤ PollWait) for a job; also a heartbeat
//	POST /jobs/{id}/logs           append log lines; reply says whether to cancel
//	POST /jobs/{id}/steps/{index}  report one step's result
//	POST /jobs/{id}/finish         report the job's result
//	POST /discovery                optional: git repos found on the device
//
// Every call except /register sends "Authorization: Bearer <RunnerToken>".
// Requests and replies use the types below. Unknown JSON fields must be
// ignored by both sides, so either can add fields without breaking the other.
//
// # Expressions
//
// The server resolves ${{ }} expressions that only need server-side context
// (github/atelier, inputs, vars, matrix, needs) before sending a job. It leaves
// expressions that need run-time context — steps.*, env.*, secrets.*, job.*,
// runner.* — in place, and the runner resolves those as the job runs. The
// runner never sends secret values back: it masks every value in Job.Secrets
// (and their base64 forms) in logs as "***".
package actions

import (
	"context"
	"time"
)

// APIPrefix is where the runner API is served.
const APIPrefix = "/api/runner/v1"

// PollWait is the longest the server holds a /poll request open.
const PollWait = 30 * time.Second

// ProtocolVersion lets server and runner refuse versions they don't understand.
const ProtocolVersion = 1

// Capabilities a runner can offer. A runner advertises the ones the owner
// switched on for that device.
const (
	CapJobs       = "jobs"       // run Actions jobs
	CapDocker     = "docker"     // jobs may use container: and docker actions
	CapDiscovery  = "discovery"  // report git repos found on the device
	CapOpen       = "open"       // "Open in editor / Fork / Finder" (Phase 5)
	CapBackups    = "backups"    // store backup copies (later)
	CapWorkspaces = "workspaces" // device workspaces (Phase 5)
)

// Status values for jobs and steps.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSuccess   = "success"
	StatusFailure   = "failure"
	StatusCancelled = "cancelled"
	StatusSkipped   = "skipped"
	StatusWaiting   = "waiting" // waiting for an approval (e.g. a production environment or an outside pull request)
)

// ---------- registration ----------

type RegisterRequest struct {
	Token        string   `json:"token"` // one-time registration token from Settings → Runners
	Name         string   `json:"name"`  // e.g. "MacBook"
	OS           string   `json:"os"`    // runtime.GOOS: darwin, linux, windows
	Arch         string   `json:"arch"`  // runtime.GOARCH: arm64, amd64
	Labels       []string `json:"labels"`
	Capabilities []string `json:"capabilities"`
	Version      string   `json:"version"`
	Protocol     int      `json:"protocol"`
}

type RegisterResponse struct {
	RunnerID    int64  `json:"runner_id"`
	RunnerToken string `json:"runner_token"` // long-lived; store it in the runner's config file (mode 0600)
	ServerURL   string `json:"server_url"`
}

// ---------- polling ----------

type PollRequest struct {
	Labels       []string `json:"labels"`
	Capabilities []string `json:"capabilities"`
	Busy         bool     `json:"busy"` // true while a job is running: heartbeat only
	Version      string   `json:"version"`
}

// PollResponse: Job is nil when there was nothing to do within PollWait.
type PollResponse struct {
	Job *Job `json:"job,omitempty"`
}

// Job is one job of a workflow run, ready to execute.
type Job struct {
	ID       int64  `json:"id"`
	RunID    int64  `json:"run_id"`
	Name     string `json:"name"`
	Workflow string `json:"workflow"` // file path, e.g. .atelier/workflows/ci.yml

	Repo     RepoRef `json:"repo"`
	Event    string  `json:"event"` // push, pull_request, schedule, workflow_dispatch, release
	Ref      string  `json:"ref"`   // e.g. refs/heads/main
	SHA      string  `json:"sha"`
	CloneURL string  `json:"clone_url"` // HTTPS; use JobToken as the password (username "atelier")

	// JobToken is short-lived and limited to this job: read the repository,
	// and whatever the workflow's permissions grant (e.g. publishing packages).
	JobToken string `json:"job_token"`

	RunsOn         []string          `json:"runs_on"`
	Container      *Container        `json:"container,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Secrets        map[string]string `json:"secrets,omitempty"` // resolved values; mask in logs
	Steps          []Step            `json:"steps"`
	TimeoutMinutes int               `json:"timeout_minutes"`

	// Outputs are the job's outputs: (name → expression text, usually
	// ${{ steps.<id>.outputs.<name> }}) for the runner to resolve after the
	// last step and report in FinishRequest.Outputs. Optional.
	Outputs map[string]string `json:"outputs,omitempty"`

	// Context is the github/atelier context (event payload, actor, repository…)
	// as JSON, for actions that read it.
	Context map[string]any `json:"context,omitempty"`
}

type RepoRef struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

type Container struct {
	Image string            `json:"image"`
	Env   map[string]string `json:"env,omitempty"`
}

// Step is one step. Exactly one of Run, Uses or Agent is set.
type Step struct {
	Index            int               `json:"index"`
	ID               string            `json:"id,omitempty"`
	Name             string            `json:"name"`
	If               string            `json:"if,omitempty"` // expression; empty means success()
	Run              string            `json:"run,omitempty"`
	Shell            string            `json:"shell,omitempty"`    // bash, sh, pwsh, cmd; default per OS
	Uses             string            `json:"uses,omitempty"`     // owner/repo@<full commit sha>, pinned by the server from the lockfile
	UsesTag          string            `json:"uses_tag,omitempty"` // the human tag it was pinned from, e.g. v4
	With             map[string]string `json:"with,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	WorkingDirectory string            `json:"working_directory,omitempty"`
	ContinueOnError  bool              `json:"continue_on_error,omitempty"`
	TimeoutMinutes   int               `json:"timeout_minutes,omitempty"`

	// Agent, when set, makes this an agent step (an agentic Markdown
	// workflow) instead of a run/uses step. Optional: runners that don't
	// know it fail the step. The API key is in With["api-key"] as a
	// ${{ secrets.* }} expression, so it's resolved and masked like any secret.
	Agent *AgentStep `json:"agent,omitempty"`
}

// AgentStep is what a runner needs to run an agent: the task, the model and
// limits, and the MCP endpoint plus the tools the job token may call there.
// The runner connects to MCPURL with the job token and offers the model only
// the tools listed here.
type AgentStep struct {
	Engine         string   `json:"engine"` // "claude"
	Model          string   `json:"model"`
	Effort         string   `json:"effort"`
	Task           string   `json:"task"`
	Tools          []string `json:"tools"`      // MCP tool names, e.g. "pulls.diff"
	MaxTurns       int      `json:"max_turns"`  // model requests
	MaxTokens      int      `json:"max_tokens"` // budget: output + uncached input tokens over the whole run
	TimeoutMinutes int      `json:"timeout_minutes"`
	MCPURL         string   `json:"mcp_url"`
	WorkflowName   string   `json:"workflow_name"`
}

// ---------- logs and results ----------

type LogLine struct {
	Time   time.Time `json:"t"`
	Step   int       `json:"step"`   // Step.Index; -1 for job setup/teardown
	Stream string    `json:"stream"` // "stdout", "stderr" or "system"
	Text   string    `json:"text"`
}

type LogsRequest struct {
	Lines []LogLine `json:"lines"`
}

// LogsResponse tells the runner to stop the job (the owner pressed Cancel).
type LogsResponse struct {
	Cancel bool `json:"cancel"`
}

type StepResult struct {
	Status     string            `json:"status"`
	ExitCode   int               `json:"exit_code"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at"`
	Outputs    map[string]string `json:"outputs,omitempty"`

	// Conclusion is set only when continue-on-error hid a failure (Status
	// keeps the real outcome). Optional.
	Conclusion string `json:"conclusion,omitempty"`
	// Summary is the step's GITHUB_STEP_SUMMARY Markdown (the server keeps
	// up to 1 MB and shows it on the run page). Optional.
	Summary string `json:"summary,omitempty"`
}

type FinishRequest struct {
	Status     string            `json:"status"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at"`
	Outputs    map[string]string `json:"outputs,omitempty"` // job outputs
	Error      string            `json:"error,omitempty"`   // runner-side failure before steps ran
}

// ---------- discovery (optional capability) ----------

type DiscoveryRequest struct {
	Repos []DiscoveredRepo `json:"repos"`
}

type DiscoveredRepo struct {
	Path    string            `json:"path"`    // absolute path on the device
	Remotes map[string]string `json:"remotes"` // name → URL (credentials stripped)
	Branch  string            `json:"branch"`
	Dirty   bool              `json:"dirty"`
}

// ---------- secrets hook ----------

// SecretSource resolves secrets for a job. The secrets feature registers one;
// the Actions engine asks it for the names a job uses. env is the job's
// environment name ("" for none). Values never leave the server except inside
// a Job sent to a runner.
type SecretSource interface {
	Resolve(ctx context.Context, repoID int64, env string, names []string) (map[string]string, error)
	// Variables returns non-secret configuration variables (vars.* context).
	Variables(ctx context.Context, repoID int64, env string) (map[string]string, error)
	// EnvironmentGate reports whether a job targeting env must wait for an
	// approval, and which runner labels it's restricted to (if any).
	EnvironmentGate(ctx context.Context, repoID int64, env string) (needsApproval bool, runnerLabels []string, err error)
}

var secretSource SecretSource

// RegisterSecretSource is called once by the secrets feature from init().
func RegisterSecretSource(s SecretSource) { secretSource = s }

// Secrets returns the registered source, or a no-op one.
func Secrets() SecretSource {
	if secretSource == nil {
		return noSecrets{}
	}
	return secretSource
}

type noSecrets struct{}

func (noSecrets) Resolve(context.Context, int64, string, []string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (noSecrets) Variables(context.Context, int64, string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (noSecrets) EnvironmentGate(context.Context, int64, string) (bool, []string, error) {
	return false, nil, nil
}
