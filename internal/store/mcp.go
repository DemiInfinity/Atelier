package store

import (
	"context"
	"time"
)

// MCP call log: one row per tools/call on Atelier's MCP server, so the owner
// can see what Claude (or an agentic workflow) did. Arguments are never
// stored; Target holds ids only ("#12", "v1.2.0").

func init() {
	RegisterSchema(`CREATE TABLE IF NOT EXISTS mcp_calls (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  person_id   INTEGER,
  actor_kind  TEXT NOT NULL,
  actor_name  TEXT NOT NULL,
  tool        TEXT NOT NULL,
  repo_id     INTEGER,
  repo        TEXT NOT NULL DEFAULT '',
  target      TEXT NOT NULL DEFAULT '',
  ok          INTEGER NOT NULL,
  error       TEXT NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS mcp_calls_person ON mcp_calls(person_id, id);`)
}

// MCPCall is one logged tool call.
type MCPCall struct {
	ID         int64
	PersonID   int64
	ActorKind  string
	ActorName  string
	Tool       string
	RepoID     int64
	Repo       string // owner/name at the time of the call
	Target     string // ids only, e.g. "#12"
	OK         bool
	Error      string
	DurationMS int64
	CreatedAt  time.Time
}

// mcpCallsKept is how many rows the log keeps per person.
const mcpCallsKept = 2000

// LogMCPCall records a call and trims old rows for that person.
func (s *Store) LogMCPCall(ctx context.Context, c MCPCall) error {
	var person, repo any
	if c.PersonID != 0 {
		person = c.PersonID
	}
	if c.RepoID != 0 {
		repo = c.RepoID
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO mcp_calls (person_id, actor_kind, actor_name, tool, repo_id, repo,
		target, ok, error, duration_ms, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		person, c.ActorKind, c.ActorName, c.Tool, repo, c.Repo, c.Target, c.OK, c.Error, c.DurationMS, now())
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil && id%100 == 0 {
		_, err = s.db.ExecContext(ctx, `DELETE FROM mcp_calls WHERE person_id IS ? AND id <= (
			SELECT id FROM mcp_calls WHERE person_id IS ? ORDER BY id DESC LIMIT 1 OFFSET ?)`, person, person, mcpCallsKept)
		return err
	}
	return nil
}

// MCPCalls lists a person's most recent calls, newest first.
func (s *Store) MCPCalls(ctx context.Context, personID int64, limit int) ([]MCPCall, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(person_id, 0), actor_kind, actor_name, tool,
		COALESCE(repo_id, 0), repo, target, ok, error, duration_ms, created_at
		FROM mcp_calls WHERE person_id=? ORDER BY id DESC LIMIT ?`, personID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPCall
	for rows.Next() {
		var c MCPCall
		var created int64
		if err := rows.Scan(&c.ID, &c.PersonID, &c.ActorKind, &c.ActorName, &c.Tool, &c.RepoID, &c.Repo, &c.Target,
			&c.OK, &c.Error, &c.DurationMS, &created); err != nil {
			return nil, err
		}
		c.CreatedAt = time.Unix(created, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}
