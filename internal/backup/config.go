package backup

import (
	"path/filepath"
	"strings"

	"atelier/internal/config"
	"atelier/internal/store"
)

// PlanFromConfig builds a run plan from the server's environment settings.
func PlanFromConfig(cfg config.Config, db *store.Store, gitBin string) Plan {
	dir := cfg.BackupDir
	if dir == "" {
		dir = filepath.Join(cfg.DataDir, "backups")
	}
	return Plan{
		Options: Options{DB: db.DB(), RepoRoot: cfg.RepoRoot(), Git: gitBin, Dir: dir, DataDir: cfg.DataDir},
		S3:      S3FromConfig(cfg),
		Keep:    Retention{Daily: cfg.BackupKeepDaily, Weekly: cfg.BackupKeepWeekly, Monthly: cfg.BackupKeepMonthly},
	}
}

func S3FromConfig(cfg config.Config) S3 {
	return S3{
		Endpoint: cfg.BackupS3Endpoint, Bucket: cfg.BackupS3Bucket, Prefix: cfg.BackupS3Prefix,
		Region: cfg.BackupS3Region, AccessKey: cfg.BackupS3AccessKey, SecretKey: cfg.BackupS3SecretKey,
	}
}

// Record turns an outcome into a history row.
func (o Outcome) Record(trigger, actorKind, actorName string) store.BackupRun {
	r := store.BackupRun{
		StartedAt:    o.Result.Time,
		Duration:     o.Duration,
		File:         o.Result.Name,
		Size:         o.Result.Size,
		SHA256:       o.SHA256(),
		Destinations: strings.Join(o.Destinations, ", "),
		OK:           o.Err == nil,
		Trigger:      trigger,
		ActorKind:    actorKind,
		ActorName:    actorName,
	}
	if o.Err != nil {
		r.Error = o.Err.Error()
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = o.Started
	}
	return r
}

func (o Outcome) SHA256() string { return o.Result.SHA256 }
