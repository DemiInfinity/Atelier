package web

// Runner updates. The Atelier image carries atelier-runner builds for every
// OS and CPU (Dockerfile, stage runner-builds), all made from the same commit
// as the server. A runner is offered the build for its OS and CPU when its
// version differs and either the owner pressed Update on Settings → Runners
// or "Update runners automatically" is on. The offer rides on the poll reply;
// the runner finishes its jobs, downloads the build from /update, checks its
// SHA-256, swaps it in and restarts (internal/runner/update.go).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"atelier/internal/actions"
	"atelier/internal/store"
)

const settingRunnerAutoUpdate = "runners_auto_update"

func settingRunnerUpdate(id int64) string { return "runner_update:" + strconv.FormatInt(id, 10) }

// runnerTargets are the builds the image ships (GOOS/GOARCH as runners report them).
var runnerTargets = map[string]bool{
	"darwin/arm64": true, "darwin/amd64": true,
	"linux/amd64": true, "linux/arm64": true,
	"windows/amd64": true, "windows/arm64": true,
}

type runnerBuild struct {
	Path, SHA256 string
	Size         int64
	mod          time.Time
}

var runnerBuilds struct {
	sync.Mutex
	byPath map[string]runnerBuild
}

// runnerBuildsVersion is the version of the builds in RunnerDir ("" when
// there are none).
func (a *App) runnerBuildsVersion() string {
	if a.cfg.RunnerDir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(a.cfg.RunnerDir, "VERSION"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// runnerBuildFor finds the build for an OS and CPU, with its checksum
// (computed once per file version).
func (a *App) runnerBuildFor(goos, arch string) (runnerBuild, bool) {
	if a.cfg.RunnerDir == "" || !runnerTargets[goos+"/"+arch] {
		return runnerBuild{}, false
	}
	name := "atelier-runner-" + goos + "-" + arch
	if goos == "windows" {
		name += ".exe"
	}
	path := filepath.Join(a.cfg.RunnerDir, name)
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return runnerBuild{}, false
	}
	runnerBuilds.Lock()
	defer runnerBuilds.Unlock()
	if b, ok := runnerBuilds.byPath[path]; ok && b.Size == fi.Size() && b.mod.Equal(fi.ModTime()) {
		return b, true
	}
	f, err := os.Open(path)
	if err != nil {
		return runnerBuild{}, false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return runnerBuild{}, false
	}
	b := runnerBuild{Path: path, SHA256: hex.EncodeToString(h.Sum(nil)), Size: fi.Size(), mod: fi.ModTime()}
	if runnerBuilds.byPath == nil {
		runnerBuilds.byPath = map[string]runnerBuild{}
	}
	runnerBuilds.byPath[path] = b
	return b, true
}

func (a *App) runnerAutoUpdate(ctx context.Context) bool {
	v, err := a.db.Setting(ctx, settingRunnerAutoUpdate)
	return err == nil && v == "1"
}

func (a *App) runnerUpdateRequested(ctx context.Context, id int64) bool {
	v, err := a.db.Setting(ctx, settingRunnerUpdate(id))
	return err == nil && v == "1"
}

// runnerLatest is the version a runner could update to, or "" when it is
// current or no build fits it.
func (a *App) runnerLatest(rn store.ActionsRunner) string {
	v := a.runnerBuildsVersion()
	if v == "" || v == rn.Version || rn.Version == "" {
		return ""
	}
	if _, ok := a.runnerBuildFor(rn.OS, rn.Arch); !ok {
		return ""
	}
	return v
}

// runnerUpdateOffer is what the poll reply carries for this runner, if
// anything. A runner that reports the offered version has updated: its
// request is cleared.
func (a *App) runnerUpdateOffer(ctx context.Context, rn store.ActionsRunner, failed string) *actions.UpdateOffer {
	v := a.runnerBuildsVersion()
	if v != "" && v == rn.Version && a.runnerUpdateRequested(ctx, rn.ID) {
		_ = a.db.SetSetting(ctx, settingRunnerUpdate(rn.ID), "")
	}
	latest := a.runnerLatest(rn)
	if latest == "" || latest == failed {
		return nil
	}
	if !a.runnerAutoUpdate(ctx) && !a.runnerUpdateRequested(ctx, rn.ID) {
		return nil
	}
	b, ok := a.runnerBuildFor(rn.OS, rn.Arch)
	if !ok {
		return nil
	}
	return &actions.UpdateOffer{Version: latest, SHA256: b.SHA256, Size: b.Size}
}

// runnerDownload serves the build for the calling runner's OS and CPU.
func (a *App) runnerDownload(w http.ResponseWriter, r *http.Request, rn store.ActionsRunner) {
	b, ok := a.runnerBuildFor(rn.OS, rn.Arch)
	if !ok {
		runnerError(w, http.StatusNotFound, "no atelier-runner build for "+rn.OS+"/"+rn.Arch)
		return
	}
	f, err := os.Open(b.Path)
	if err != nil {
		runnerError(w, http.StatusNotFound, "no atelier-runner build")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Atelier-SHA256", b.SHA256)
	http.ServeContent(w, r, filepath.Base(b.Path), b.mod, f)
}

// ---------- Settings → Runners ----------

func (a *App) runnerUpdate(w http.ResponseWriter, r *http.Request) {
	rn, ok := a.runnerFromPath(w, r)
	if !ok {
		return
	}
	if a.runnerLatest(rn) == "" {
		a.renderRunners(w, r, "", rn.Name+" is already up to date, or there's no build for its system.", http.StatusUnprocessableEntity)
		return
	}
	if err := a.db.SetSetting(r.Context(), settingRunnerUpdate(rn.ID), "1"); err != nil {
		a.fail(w, r, err)
		return
	}
	a.actions().signal() // a waiting poll answers now
	http.Redirect(w, r, "/settings/runners?saved=1", http.StatusSeeOther)
}

func (a *App) runnersAutoUpdate(w http.ResponseWriter, r *http.Request) {
	v := ""
	if r.FormValue("auto") == "1" {
		v = "1"
	}
	if err := a.db.SetSetting(r.Context(), settingRunnerAutoUpdate, v); err != nil {
		a.fail(w, r, err)
		return
	}
	a.actions().signal()
	http.Redirect(w, r, "/settings/runners?saved=1", http.StatusSeeOther)
}
