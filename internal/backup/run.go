package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Plan is a complete backup run: snapshot, upload, prune.
type Plan struct {
	Options
	S3   S3
	Keep Retention
}

// Outcome is what a run did. Err is set if anything failed; a snapshot may
// still have been written locally.
type Outcome struct {
	Result       Result
	Destinations []string // where the snapshot ended up: "local", "s3"
	Pruned       []string // snapshot names removed by retention
	Started      time.Time
	Duration     time.Duration
	Err          error
}

// Run takes a snapshot, uploads it to S3 if configured and prunes old
// snapshots in both places.
func Run(ctx context.Context, p Plan) Outcome {
	start := time.Now()
	out := Outcome{Started: start}
	var errs []error
	res, err := Create(ctx, p.Options)
	if err != nil {
		out.Err = err
		out.Duration = time.Since(start)
		return out
	}
	out.Result = res
	out.Destinations = append(out.Destinations, "local")

	if p.S3.Enabled() {
		if err := p.S3.Upload(ctx, res.Snapshot); err != nil {
			errs = append(errs, fmt.Errorf("upload to S3: %w", err))
		} else {
			out.Destinations = append(out.Destinations, "s3")
		}
	}

	// Prune local.
	if local, err := List(p.Dir); err != nil {
		errs = append(errs, fmt.Errorf("list local snapshots: %w", err))
	} else {
		for _, s := range p.Keep.Expired(local) {
			if err := os.Remove(s.Path); err != nil {
				errs = append(errs, err)
				continue
			}
			out.Pruned = append(out.Pruned, s.Name)
		}
	}
	// Prune remote, only when this run's upload worked (so a broken bucket
	// never loses its old copies).
	if p.S3.Enabled() && len(out.Destinations) > 1 {
		if remote, err := p.S3.List(ctx); err != nil {
			errs = append(errs, fmt.Errorf("list S3 snapshots: %w", err))
		} else {
			for _, s := range p.Keep.Expired(remote) {
				if err := p.S3.Remove(ctx, s.Name); err != nil {
					errs = append(errs, fmt.Errorf("prune S3 %s: %w", s.Name, err))
					continue
				}
				out.Pruned = append(out.Pruned, "s3:"+s.Name)
			}
		}
	}
	out.Err = errors.Join(errs...)
	out.Duration = time.Since(start)
	return out
}

// Summary is a one-line description of the outcome for logs.
func (o Outcome) Summary() string {
	if o.Result.Name == "" {
		return fmt.Sprintf("backup failed after %s: %v", o.Duration.Round(time.Millisecond), o.Err)
	}
	s := fmt.Sprintf("backup %s (%d repos) to %s in %s", o.Result.Name, len(o.Result.Manifest.Repos),
		strings.Join(o.Destinations, ", "), o.Duration.Round(time.Millisecond))
	if len(o.Pruned) > 0 {
		s += fmt.Sprintf(", pruned %d", len(o.Pruned))
	}
	if o.Err != nil {
		s += ": " + o.Err.Error()
	}
	return s
}
