package runner

// Cleaning up what jobs leave behind when the normal end-of-job cleanup in
// execute() never ran (the runner crashed, was force-quit or the machine lost
// power), and keeping caches from growing forever. It runs when the runner
// starts and once a day after that:
//
//   - job folders (work/job-*) whose runner process is gone are removed;
//   - containers Atelier started (label dev.atelier.job) whose runner process
//     is gone are removed;
//   - downloaded actions not used for actionCacheTTL are removed, and so are
//     docker action images Atelier built more than actionCacheTTL ago.
//
// Several runners can share one work folder (different --config, same
// defaults), so nothing that belongs to a live runner process is touched.
// Images pulled for jobs (ubuntu:24.04 …) are left alone: they may be the
// machine owner's own.

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"atelier/internal/actions"
)

const (
	jobOwnerFile   = ".atelier-runner-pid" // in each job folder: the runner's pid
	actionMarker   = ".atelier-complete"   // in each downloaded action; its mtime is the last use
	actionCacheTTL = 30 * 24 * time.Hour
	// A job folder or container with no owner recorded (made by an older
	// runner) is only removed once it is older than any job can run.
	unownedAfter = 24 * time.Hour
	cleanupEvery = 24 * time.Hour
)

func (r *Runner) cleanupLoop(ctx context.Context) {
	for {
		r.cleanupOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(cleanupEvery):
		}
	}
}

func (r *Runner) cleanupOnce(ctx context.Context) {
	work, cache := r.Config.Dirs()
	log := r.logger()
	if n := sweepJobDirs(work, time.Now()); n > 0 {
		log.Info("removed job folders left by an earlier runner", "count", n)
	}
	docker := r.Config.HasCap(actions.CapDocker) && dockerAvailable()
	if docker {
		if n := sweepContainers(ctx); n > 0 {
			log.Info("removed containers left by an earlier runner", "count", n)
		}
	}
	// Pruning only while idle, so a job never loses an action it is using.
	if r.ctl.running.Load() > 0 {
		return
	}
	r.fetchMu.Lock()
	n := pruneActionCache(filepath.Join(cache, "actions"), time.Now())
	r.fetchMu.Unlock()
	if n > 0 {
		log.Info("removed actions not used for 30 days", "count", n)
	}
	if docker {
		dctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		_, _ = dockerRun(dctx, "image", "prune", "-a", "-f",
			"--filter", "label="+imageLabelAction+"=1", "--filter", "until="+strconv.Itoa(int(actionCacheTTL.Hours()))+"h")
		cancel()
	}
}

// sweepJobDirs removes job-* folders in work whose runner process is gone.
func sweepJobDirs(work string, now time.Time) int {
	entries, err := os.ReadDir(work)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "job-") {
			continue
		}
		dir := filepath.Join(work, e.Name())
		if b, err := os.ReadFile(filepath.Join(dir, jobOwnerFile)); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && (pid == os.Getpid() || pidAlive(pid)) {
				continue
			}
		} else if fi, err := e.Info(); err != nil || now.Sub(fi.ModTime()) < unownedAfter {
			continue
		}
		if removeAll(dir) == nil {
			n++
		}
	}
	return n
}

// sweepContainers removes Atelier's job and step containers whose runner
// process is gone.
func sweepContainers(ctx context.Context) int {
	dctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := dockerRun(dctx, "ps", "-a", "--filter", "label="+containerLabelJobID,
		"--format", `{{.ID}}	{{.Label "`+containerLabelPID+`"}}	{{.State}}	{{.CreatedAt}}`)
	if err != nil {
		return 0
	}
	var gone []string
	for _, c := range parseContainerList(out) {
		if staleContainer(c, os.Getpid(), pidAlive, time.Now()) {
			gone = append(gone, c.id)
		}
	}
	if len(gone) == 0 {
		return 0
	}
	if _, err := dockerRun(dctx, append([]string{"rm", "-f"}, gone...)...); err != nil {
		return 0
	}
	return len(gone)
}

type listedContainer struct {
	id, state string
	pid       int // 0 when the container has no owner label
	created   time.Time
}

func parseContainerList(out string) []listedContainer {
	var cs []listedContainer
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) < 4 || f[0] == "" {
			continue
		}
		c := listedContainer{id: f[0], state: f[2]}
		c.pid, _ = strconv.Atoi(f[1])
		c.created, _ = time.Parse("2006-01-02 15:04:05 -0700 MST", f[3])
		cs = append(cs, c)
	}
	return cs
}

func staleContainer(c listedContainer, self int, alive func(int) bool, now time.Time) bool {
	if c.pid != 0 {
		return c.pid != self && !alive(c.pid)
	}
	// From a runner that didn't record itself: stopped ones are safe to
	// remove; running ones only once no job could still be using them.
	return c.state != "running" || (!c.created.IsZero() && now.Sub(c.created) > unownedAfter)
}

// pruneActionCache removes downloaded actions whose last use is older than
// actionCacheTTL, and download folders a crash left half-written.
func pruneActionCache(root string, now time.Time) int {
	n := 0
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == root {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".dl-") {
			if fi, err := d.Info(); err == nil && now.Sub(fi.ModTime()) > unownedAfter {
				_ = os.RemoveAll(p)
			}
			return filepath.SkipDir
		}
		fi, err := os.Stat(filepath.Join(p, actionMarker))
		if err != nil {
			return nil // an owner or repo folder: look inside
		}
		if now.Sub(fi.ModTime()) > actionCacheTTL && os.RemoveAll(p) == nil {
			n++
		}
		return filepath.SkipDir
	})
	return n
}

// touchAction records that a downloaded action was just used.
func touchAction(dir string) {
	now := time.Now()
	_ = os.Chtimes(filepath.Join(dir, actionMarker), now, now)
}

func dockerAvailable() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

func dockerRun(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}
