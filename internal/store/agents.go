package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Agentic workflows: which Actions jobs run an agent (and with which MCP
// tools, for the job token's scopes), and which tool lists the owner has
// approved for each agentic workflow of a repository.

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS agent_jobs (
  job_id        INTEGER PRIMARY KEY REFERENCES actions_jobs(id) ON DELETE CASCADE,
  repo_id       INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  workflow_path TEXT NOT NULL,
  workflow_name TEXT NOT NULL,
  tools         TEXT NOT NULL DEFAULT '[]',
  created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS agent_approvals (
  repo_id       INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  workflow_path TEXT NOT NULL,
  tools_key     TEXT NOT NULL,
  approved_by   TEXT NOT NULL,
  approved_at   INTEGER NOT NULL,
  PRIMARY KEY (repo_id, workflow_path, tools_key)
);`)
}

// AgentJob records that an Actions job runs an agent with these MCP tools.
type AgentJob struct {
	JobID        int64
	RepoID       int64
	WorkflowPath string
	WorkflowName string
	Tools        []string
}

// SetAgentJob stores (or replaces) a job's agent record.
func (s *Store) SetAgentJob(ctx context.Context, j AgentJob) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_jobs (job_id, repo_id, workflow_path, workflow_name, tools, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(job_id) DO UPDATE SET repo_id=excluded.repo_id, workflow_path=excluded.workflow_path,
		workflow_name=excluded.workflow_name, tools=excluded.tools`,
		j.JobID, j.RepoID, j.WorkflowPath, j.WorkflowName, actJSON(actList(j.Tools)), now())
	return err
}

// AgentJobByID returns a job's agent record, or ErrNotFound for ordinary jobs.
func (s *Store) AgentJobByID(ctx context.Context, jobID int64) (AgentJob, error) {
	var j AgentJob
	var tools string
	err := s.db.QueryRowContext(ctx, `SELECT job_id, repo_id, workflow_path, workflow_name, tools FROM agent_jobs WHERE job_id=?`, jobID).
		Scan(&j.JobID, &j.RepoID, &j.WorkflowPath, &j.WorkflowName, &tools)
	if err != nil {
		return j, notFound(err)
	}
	actUnJSON(tools, &j.Tools)
	return j, nil
}

// AgentApproval is the owner's approval of one tool list for one agentic workflow.
type AgentApproval struct {
	RepoID       int64
	WorkflowPath string
	ToolsKey     string
	ApprovedBy   string
	ApprovedAt   time.Time
}

// ApproveAgentTools records that the owner let workflowPath run with toolsKey.
func (s *Store) ApproveAgentTools(ctx context.Context, repoID int64, workflowPath, toolsKey, by string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_approvals (repo_id, workflow_path, tools_key, approved_by, approved_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(repo_id, workflow_path, tools_key) DO NOTHING`, repoID, workflowPath, toolsKey, by, now())
	return err
}

// AgentToolsApproval returns the approval of exactly this tool list, or ErrNotFound.
func (s *Store) AgentToolsApproval(ctx context.Context, repoID int64, workflowPath, toolsKey string) (AgentApproval, error) {
	return scanAgentApproval(s.db.QueryRowContext(ctx, `SELECT repo_id, workflow_path, tools_key, approved_by, approved_at
		FROM agent_approvals WHERE repo_id=? AND workflow_path=? AND tools_key=?`, repoID, workflowPath, toolsKey))
}

// LatestAgentApproval returns the most recent approval for a workflow, or ErrNotFound.
func (s *Store) LatestAgentApproval(ctx context.Context, repoID int64, workflowPath string) (AgentApproval, error) {
	return scanAgentApproval(s.db.QueryRowContext(ctx, `SELECT repo_id, workflow_path, tools_key, approved_by, approved_at
		FROM agent_approvals WHERE repo_id=? AND workflow_path=? ORDER BY approved_at DESC, rowid DESC LIMIT 1`, repoID, workflowPath))
}

// HasAgentApprovals reports whether any agentic workflow ran in the repository with approval.
func (s *Store) HasAgentApprovals(ctx context.Context, repoID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_approvals WHERE repo_id=?`, repoID).Scan(&n)
	return n > 0, err
}

func scanAgentApproval(row *sql.Row) (AgentApproval, error) {
	var a AgentApproval
	var at int64
	err := row.Scan(&a.RepoID, &a.WorkflowPath, &a.ToolsKey, &a.ApprovedBy, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.ApprovedAt = time.Unix(at, 0)
	return a, nil
}
