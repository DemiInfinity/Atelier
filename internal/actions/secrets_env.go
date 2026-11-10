package actions

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Environment helpers for the secrets feature (internal/web/secrets.go).
//
// SecretSource methods only receive the repository and environment name, so
// the engine passes the job's ref in the context:
//
//	ctx = actions.WithEnvironmentRef(ctx, job.Ref) // e.g. "refs/heads/main"
//	approve, labels, err := actions.Secrets().EnvironmentGate(ctx, repoID, env)
//	var be *actions.EnvironmentBranchError
//	if errors.As(err, &be) { /* fail the job with err.Error() */ }
//
// An environment restricted to some branches refuses (fails closed) when no
// ref is in the context. Resolve applies the same branch check to an
// environment's secrets.

type envRefKey struct{}

// WithEnvironmentRef records the ref a job runs on ("refs/heads/main").
func WithEnvironmentRef(ctx context.Context, ref string) context.Context {
	return context.WithValue(ctx, envRefKey{}, ref)
}

// EnvironmentRef returns the ref recorded by WithEnvironmentRef, or "".
func EnvironmentRef(ctx context.Context) string {
	s, _ := ctx.Value(envRefKey{}).(string)
	return s
}

// EnvironmentBranchError says a job's ref may not use an environment. Its
// message is written for people and can be shown on the job as is.
type EnvironmentBranchError struct {
	Environment string
	Ref         string   // "" when the engine didn't say
	Allowed     []string // branch patterns
}

func (e *EnvironmentBranchError) Error() string {
	allowed := strings.Join(e.Allowed, ", ")
	if e.Ref == "" {
		return fmt.Sprintf("environment %q only accepts jobs from branches %s, and this job's branch isn't known", e.Environment, allowed)
	}
	ref := strings.TrimPrefix(e.Ref, "refs/heads/")
	return fmt.Sprintf("environment %q doesn't accept jobs from %s (allowed branches: %s)", e.Environment, ref, allowed)
}

// EnvironmentWaiter is optionally implemented by the SecretSource: a wait
// timer to hold a job for after approval (or after queueing when no approval
// is needed). Check with a type assertion on Secrets().
type EnvironmentWaiter interface {
	EnvironmentWait(ctx context.Context, repoID int64, env string) (time.Duration, error)
}
