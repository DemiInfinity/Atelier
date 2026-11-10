package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"atelier/internal/backup"
	"atelier/internal/store"
)

func init() {
	registerFeature(feature{
		name: "backups",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /settings/backups", a.authed(a.backupsPage))
			m.HandleFunc("POST /settings/backups/run", a.authed(a.backupsRunNow))
			m.HandleFunc("GET /settings/backups/download/{name}", a.authed(a.backupsDownload))
		},
		start: func(ctx context.Context, a *App) {
			st := a.backupState()
			st.mu.Lock()
			st.ctx = ctx
			st.mu.Unlock()
			if err := backup.HoldLock(ctx, a.cfg.DataDir); err != nil {
				log.Printf("backups: lock file: %v", err)
			}
			a.backupScheduler(ctx)
		},
	})
}

// backupState is the backups feature's per-App runtime state.
type backupState struct {
	mu      sync.Mutex
	ctx     context.Context // server lifetime; Background until start runs
	running bool
	since   time.Time
}

var backupStates sync.Map // *App → *backupState

func (a *App) backupState() *backupState {
	v, _ := backupStates.LoadOrStore(a, &backupState{ctx: context.Background()})
	return v.(*backupState)
}

// startBackup runs one backup in the background unless one is running.
// It returns false if a run was already in progress.
func (a *App) startBackup(trigger string, who Actor) (<-chan backup.Outcome, bool) {
	st := a.backupState()
	st.mu.Lock()
	if st.running {
		st.mu.Unlock()
		return nil, false
	}
	st.running, st.since = true, time.Now()
	ctx := st.ctx
	st.mu.Unlock()

	done := make(chan backup.Outcome, 1)
	go func() {
		out := backup.Run(ctx, backup.PlanFromConfig(a.cfg, a.db, a.git.Git))
		if _, err := a.db.AddBackupRun(context.WithoutCancel(ctx), out.Record(trigger, who.Kind, who.Name)); err != nil {
			log.Printf("backups: record run: %v", err)
		}
		log.Print("backups: ", out.Summary())
		st.mu.Lock()
		st.running = false
		st.mu.Unlock()
		done <- out
	}()
	return done, true
}

func (a *App) backupScheduler(ctx context.Context) {
	for {
		next, ok := backup.NextRun(time.Now(), a.cfg.BackupTime)
		if !ok {
			log.Printf("backups: schedule off (ATELIER_BACKUP_TIME=%q)", a.cfg.BackupTime)
			return
		}
		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if done, ok := a.startBackup("schedule", Actor{Kind: "agent", Name: "scheduler"}); ok {
			select {
			case <-done:
			case <-ctx.Done():
				return
			}
		}
		// Guard against firing twice in the same minute.
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

// ---------- handlers ----------

type backupRunView struct {
	store.BackupRun
	DurationText string
}

type snapshotView struct {
	backup.Snapshot
	Local, Remote bool
}

type usageRow struct {
	Label string
	Size  int64
	Class string
}

func (a *App) backupsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runs, err := a.db.BackupRuns(ctx, 30)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var views []backupRunView
	for _, run := range runs {
		views = append(views, backupRunView{BackupRun: run, DurationText: shortDuration(run.Duration)})
	}
	var last *backupRunView
	if len(views) > 0 {
		last = &views[0]
	}

	// Snapshots: local ones can be downloaded; S3 ones are listed for reference.
	local, err := backup.List(a.backupDir())
	if err != nil {
		logErr(r, err)
	}
	byName := map[string]*snapshotView{}
	var snaps []*snapshotView
	for _, s := range local {
		v := &snapshotView{Snapshot: s, Local: true}
		byName[s.Name] = v
		snaps = append(snaps, v)
	}
	s3 := backup.S3FromConfig(a.cfg)
	s3Err := ""
	if s3.Enabled() {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		remote, err := s3.List(lctx)
		cancel()
		if err != nil {
			s3Err = "Couldn't list the bucket: " + err.Error()
		}
		for _, s := range remote {
			if v, ok := byName[s.Name]; ok {
				v.Remote = true
				continue
			}
			snaps = append(snaps, &snapshotView{Snapshot: s, Remote: true})
		}
	}
	sortSnapshotViews(snaps)

	st := a.backupState()
	st.mu.Lock()
	running, since := st.running, st.since
	st.mu.Unlock()

	next, scheduled := backup.NextRun(time.Now(), a.cfg.BackupTime)
	u := backup.DiskUsage(a.cfg.RepoRoot(), a.cfg.DBPath(), a.backupDir())
	used := u.Repos + u.Database + u.Backups
	total := used
	if u.FreeData > 0 {
		total += u.FreeData
	}
	keep := backup.Retention{Daily: a.cfg.BackupKeepDaily, Weekly: a.cfg.BackupKeepWeekly, Monthly: a.cfg.BackupKeepMonthly}

	errMsg := ""
	switch r.URL.Query().Get("msg") {
	case "busy":
		errMsg = "A backup is already running."
	}
	a.render(w, r, "backups_settings", page{Title: "Backups and storage", Error: errMsg, Data: map[string]any{
		"Started":   r.URL.Query().Get("msg") == "started",
		"Running":   running,
		"Since":     since,
		"Last":      last,
		"Runs":      views,
		"Next":      next,
		"Scheduled": scheduled,
		"Clock":     a.cfg.BackupTime,
		"Keep":      keep.String(),
		"Dir":       a.backupDir(),
		"S3":        s3.Enabled(),
		"S3Where":   s3.Where(),
		"S3Error":   s3Err,
		"Snapshots": snaps,
		"Usage":     u,
		"UsageRows": []usageRow{
			{"Repositories", u.Repos, "u-repos"},
			{"Database", u.Database, "u-db"},
			{"Backups", u.Backups, "u-backups"},
		},
		"Used":       used,
		"Total":      total,
		"Free":       u.FreeData,
		"FreeBackup": u.FreeBackups,
		"SameDisk":   u.SameDisk,
	}})
}

func sortSnapshotViews(s []*snapshotView) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Time.After(s[j-1].Time); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func (a *App) backupsRunNow(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.startBackup("manual", a.actor(r)); !ok {
		http.Redirect(w, r, "/settings/backups?msg=busy", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings/backups?msg=started", http.StatusSeeOther)
}

func (a *App) backupsDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !backup.ValidName(name) {
		a.notFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(a.backupDir(), name))
	if errors.Is(err, os.ErrNotExist) {
		a.notFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		a.notFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// backupDir is where snapshots live (config.FromEnv always sets it; tests may not).
func (a *App) backupDir() string {
	if a.cfg.BackupDir != "" {
		return a.cfg.BackupDir
	}
	return filepath.Join(a.cfg.DataDir, "backups")
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1f s", d.Seconds())
	}
	return d.Round(time.Second).String()
}
