package backup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

func isolateGit(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// pushCommit commits a file in a scratch clone and pushes it to the bare repo.
func pushCommit(t *testing.T, bare, branch, file, content string) string {
	t.Helper()
	work := t.TempDir()
	run(t, work, "init", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(work, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "add", ".")
	run(t, work, "commit", "-q", "-m", "add "+file)
	run(t, work, "push", "-q", "--force", bare, branch+":refs/heads/"+branch)
	return run(t, work, "rev-parse", "HEAD")
}

type env struct {
	data string
	db   *store.Store
	git  *gitrepo.Store
}

func newEnv(t *testing.T, data string) env {
	t.Helper()
	db, err := store.Open(filepath.Join(data, "atelier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	g, err := gitrepo.New(filepath.Join(data, "repos"))
	if err != nil {
		t.Fatal(err)
	}
	return env{data: data, db: db, git: g}
}

func (e env) restoreOpts(archive string) RestoreOptions {
	return RestoreOptions{
		Archive: archive, DataDir: e.data, DBPath: filepath.Join(e.data, "atelier.db"),
		RepoRoot: filepath.Join(e.data, "repos"), AfterRepo: func(ctx context.Context, owner, name string) error {
			return e.git.AfterCreate(ctx, owner, name)
		},
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()
	src := newEnv(t, t.TempDir())

	me, err := src.db.CreateOwner(ctx, "demi", "Demi", "user")
	if err != nil {
		t.Fatal(err)
	}
	co, err := src.db.CreateOwner(ctx, "elyndra", "Elyndra Studios", "company")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		o    store.Owner
		name string
	}{{me, "atelier"}, {me, "empty"}, {co, "site"}} {
		if err := src.git.Init(ctx, r.o.Name, r.name); err != nil {
			t.Fatal(err)
		}
		if _, err := src.db.CreateRepo(ctx, r.o.ID, r.name, "about "+r.name, r.name != "site"); err != nil {
			t.Fatal(err)
		}
	}
	mainSHA := pushCommit(t, src.git.Path("demi", "atelier"), "main", "README.md", "hello")
	devSHA := pushCommit(t, src.git.Path("demi", "atelier"), "dev", "dev.txt", "dev")
	run(t, src.git.Path("demi", "atelier"), "tag", "v1", mainSHA)
	siteSHA := pushCommit(t, src.git.Path("elyndra", "site"), "trunk", "index.html", "<p>hi</p>")
	run(t, src.git.Path("elyndra", "site"), "symbolic-ref", "HEAD", "refs/heads/trunk")

	backups := filepath.Join(src.data, "backups")
	when := time.Date(2026, 11, 10, 3, 0, 0, 0, time.Local)
	res, err := Create(ctx, Options{DB: src.db.DB(), RepoRoot: src.git.Root, Dir: backups, Now: when})
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "atelier-20261110-030000.tar.gz" || res.Size == 0 || len(res.SHA256) != 64 {
		t.Fatalf("result = %+v", res.Snapshot)
	}
	m, err := ReadManifest(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != ManifestVersion || len(m.Repos) != 3 || len(m.Files) != 1 {
		t.Fatalf("manifest = %+v", m)
	}
	for _, r := range m.Repos {
		if (r.Name == "empty") != (r.Bundle == "") {
			t.Errorf("repo %s bundle %q", r.FullName(), r.Bundle)
		}
	}
	list, _ := List(backups)
	if len(list) != 1 || list[0].Name != res.Name {
		t.Fatalf("List = %+v", list)
	}

	// Full restore into a fresh data directory.
	dstDir := t.TempDir()
	o := RestoreOptions{Archive: res.Path, DataDir: dstDir, DBPath: filepath.Join(dstDir, "atelier.db"), RepoRoot: filepath.Join(dstDir, "repos")}
	if _, err := RestoreAll(ctx, o); err != nil {
		t.Fatal(err)
	}
	dst := newEnv(t, dstDir)
	repos, err := dst.db.Repos(ctx)
	if err != nil || len(repos) != 3 {
		t.Fatalf("restored repos = %v, %v", repos, err)
	}
	r, err := dst.db.RepoByName(ctx, "elyndra", "site")
	if err != nil || r.Private || r.Description != "about site" || r.OwnerKind != "company" {
		t.Fatalf("restored site = %+v, %v", r, err)
	}
	ap := dst.git.Path("demi", "atelier")
	if got := run(t, ap, "rev-parse", "main"); got != mainSHA {
		t.Errorf("main = %s, want %s", got, mainSHA)
	}
	if got := run(t, ap, "rev-parse", "dev"); got != devSHA {
		t.Errorf("dev = %s, want %s", got, devSHA)
	}
	if got := run(t, ap, "rev-parse", "v1"); got != mainSHA {
		t.Errorf("tag = %s", got)
	}
	if got := run(t, ap, "config", "http.receivepack"); got != "true" {
		t.Errorf("receivepack = %q", got)
	}
	sp := dst.git.Path("elyndra", "site")
	if got := run(t, sp, "symbolic-ref", "HEAD"); got != "refs/heads/trunk" {
		t.Errorf("site HEAD = %s", got)
	}
	if got := run(t, sp, "rev-parse", "HEAD"); got != siteSHA {
		t.Errorf("site HEAD sha = %s", got)
	}
	if _, err := os.Stat(dst.git.Path("demi", "empty")); err != nil {
		t.Errorf("empty repo not restored: %v", err)
	}
	if err := exec.Command("git", "-C", ap, "fsck", "--no-progress").Run(); err != nil {
		t.Errorf("fsck: %v", err)
	}

	// A running server blocks a second full restore unless forced.
	lctx, cancel := context.WithCancel(ctx)
	if err := HoldLock(lctx, dstDir); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreAll(ctx, o); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("restore with lock held: %v", err)
	}
	o.Force = true
	dst.db.Close()
	if _, err := RestoreAll(ctx, o); err != nil {
		t.Fatalf("forced restore: %v", err)
	}
	cancel()
	matches, _ := filepath.Glob(filepath.Join(dstDir, "pre-restore-*", "atelier.db"))
	if len(matches) != 1 {
		t.Errorf("previous data not kept aside: %v", matches)
	}
}

func TestRestoreSingleRepo(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()
	e := newEnv(t, t.TempDir())
	me, _ := e.db.CreateOwner(ctx, "demi", "", "user")
	co, _ := e.db.CreateOwner(ctx, "acme", "Acme", "company")
	for _, o := range []store.Owner{me, co} {
		if err := e.git.Init(ctx, o.Name, "proj"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.CreateRepo(ctx, o.ID, "proj", "desc "+o.Name, true); err != nil {
			t.Fatal(err)
		}
	}
	good := pushCommit(t, e.git.Path("demi", "proj"), "main", "a.txt", "good")
	pushCommit(t, e.git.Path("acme", "proj"), "main", "b.txt", "acme")
	res, err := Create(ctx, Options{DB: e.db.DB(), RepoRoot: e.git.Root, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	// Damage demi/proj after the snapshot, then bring it back.
	pushCommit(t, e.git.Path("demi", "proj"), "main", "a.txt", "oops")
	if err := RestoreRepo(ctx, e.restoreOpts(res.Path), e.db, "DEMI/proj"); err != nil {
		t.Fatal(err)
	}
	if got := run(t, e.git.Path("demi", "proj"), "rev-parse", "main"); got != good {
		t.Errorf("main = %s, want %s", got, good)
	}
	aside, _ := filepath.Glob(e.git.Path("demi", "proj") + ".pre-restore-*")
	if len(aside) != 1 {
		t.Errorf("old copy not kept: %v", aside)
	}

	// A deleted company repo and account come back with their row.
	r, _ := e.db.RepoByName(ctx, "acme", "proj")
	if err := e.db.DeleteRepo(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	_ = e.git.Remove("acme", "proj")
	if _, err := e.db.DB().Exec(`DELETE FROM owners WHERE name='acme'`); err != nil {
		t.Fatal(err)
	}
	if err := RestoreRepo(ctx, e.restoreOpts(res.Path), e.db, "acme/proj"); err != nil {
		t.Fatal(err)
	}
	r, err = e.db.RepoByName(ctx, "acme", "proj")
	if err != nil || r.Description != "desc acme" || r.OwnerKind != "company" {
		t.Fatalf("restored row = %+v, %v", r, err)
	}
	if err := RestoreRepo(ctx, e.restoreOpts(res.Path), e.db, "nobody/none"); err == nil {
		t.Error("restoring a missing repo should fail")
	}
}

func TestExtractRejectsDamage(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()
	e := newEnv(t, t.TempDir())
	res, err := Create(ctx, Options{DB: e.db.DB(), RepoRoot: e.git.Root, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Path)
	b = b[:len(b)/2]
	bad := filepath.Join(t.TempDir(), "atelier-20260101-000000.tar.gz")
	_ = os.WriteFile(bad, b, 0o600)
	if _, err := extract(bad, t.TempDir(), nil); err == nil {
		t.Error("truncated archive extracted without error")
	}
}

func snapsDaily(start time.Time, days int) []Snapshot {
	var s []Snapshot
	for i := 0; i < days; i++ {
		tm := start.AddDate(0, 0, -i)
		s = append(s, Snapshot{Name: "atelier-" + tm.Format(nameTime) + ".tar.gz", Time: tm})
	}
	return s
}

func TestRetention(t *testing.T) {
	now := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC) // a Tuesday
	snaps := snapsDaily(now, 400)
	r := Retention{Daily: 14, Weekly: 8, Monthly: 6}
	expired := r.Expired(snaps)
	gone := map[string]bool{}
	for _, s := range expired {
		gone[s.Name] = true
	}
	var kept []Snapshot
	for _, s := range snaps {
		if !gone[s.Name] {
			kept = append(kept, s)
		}
	}
	for i := 0; i < 14; i++ {
		if gone[snaps[i].Name] {
			t.Errorf("daily snapshot %d expired", i)
		}
	}
	// 14 dailies span the newest 2-3 ISO weeks, so weeklies add 5-6 more;
	// monthlies add one per older month.
	if len(kept) < 14+5 || len(kept) > 14+8+6 {
		t.Errorf("kept %d snapshots", len(kept))
	}
	oldest := kept[len(kept)-1].Time
	if oldest.Before(now.AddDate(0, -6, 0)) {
		t.Errorf("kept a snapshot from %s, older than 6 months", oldest)
	}
	if !oldest.Before(now.AddDate(0, -4, 0)) {
		t.Errorf("monthly snapshots missing, oldest %s", oldest)
	}

	// Hand-made: two snapshots on one day keeps only the newer as the daily.
	d := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	small := []Snapshot{
		{Name: "a", Time: d.Add(2 * time.Hour)},
		{Name: "b", Time: d.Add(1 * time.Hour)},
		{Name: "c", Time: d.AddDate(0, 0, -1)},
		{Name: "d", Time: d.AddDate(0, 0, -2)},
	}
	exp := Retention{Daily: 2}.Expired(small)
	if len(exp) != 2 || exp[0].Name != "b" || exp[1].Name != "d" {
		t.Errorf("expired = %+v", exp)
	}
	if got := (Retention{}).Expired(small); got != nil {
		t.Errorf("zero retention expired %v", got)
	}
	if got := (Retention{Monthly: 1}).Expired(small[:1]); len(got) != 0 {
		t.Errorf("newest must always be kept: %v", got)
	}
}

func TestNames(t *testing.T) {
	for name, ok := range map[string]bool{
		"atelier-20261110-030000.tar.gz":    true,
		"atelier-20261110-030000-2.tar.gz":  true,
		"atelier-20261110-0300.tar.gz":      false,
		"atelier-20261110-030000-x.tar.gz":  false,
		"../atelier-20261110-030000.tar.gz": false,
		"other.tar.gz":                      false,
	} {
		if ValidName(name) != ok {
			t.Errorf("ValidName(%q) = %v", name, !ok)
		}
	}
	now := time.Date(2026, 11, 10, 2, 0, 0, 0, time.Local)
	if n, ok := NextRun(now, "03:00"); !ok || !n.Equal(now.Add(time.Hour)) {
		t.Errorf("NextRun = %v", n)
	}
	if n, _ := NextRun(now, "01:30"); n.Day() != 11 || n.Hour() != 1 || n.Minute() != 30 {
		t.Errorf("NextRun tomorrow = %v", n)
	}
	if _, ok := NextRun(now, "off"); ok {
		t.Error("off should disable")
	}
	if _, _, ok := ParseClock("25:00"); ok {
		t.Error("25:00 accepted")
	}
}

func TestS3Where(t *testing.T) {
	s := S3{Endpoint: "https://s3.example.com/", Bucket: "b", Prefix: "/atelier/", SecretKey: "shh"}
	if s.Where() != "s3.example.com/b/atelier" || strings.Contains(s.Where(), "shh") {
		t.Errorf("Where = %q", s.Where())
	}
	if s.key("x.tar.gz") != "atelier/x.tar.gz" {
		t.Errorf("key = %q", s.key("x.tar.gz"))
	}
	if _, secure := (S3{Endpoint: "http://minio:9000"}).host(); secure {
		t.Error("http endpoint should be insecure")
	}
}
