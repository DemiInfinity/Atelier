package web

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

// featureSession completes setup and returns a signed-in Cookie header.
func featureSession(t *testing.T, a *App) map[string]string {
	t.Helper()
	rec := do(t, a, "POST", "/setup", url.Values{
		"username": {"demi"}, "password": {"correct horse battery"}, "password_confirm": {"correct horse battery"},
	}, nil)
	return map[string]string{"Cookie": sessionCookie + "=" + sessionFrom(t, rec)}
}

func TestBackupsPageRunAndDownload(t *testing.T) {
	a := newTestApp(t)
	ck := featureSession(t, a)
	ctx := context.Background()
	me, _ := a.db.PersonalOwner(ctx)
	if err := a.git.Init(ctx, me.Name, "proj"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.CreateRepo(ctx, me.ID, "proj", "", true); err != nil {
		t.Fatal(err)
	}

	if rec := do(t, a, "GET", "/settings/backups", nil, nil); rec.Code != 303 {
		t.Fatalf("signed-out GET = %d", rec.Code)
	}
	rec := do(t, a, "GET", "/settings/backups", nil, ck)
	if rec.Code != 200 {
		t.Fatalf("GET /settings/backups = %d\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"Back up now", "No snapshots yet", "atelier restore", "Repositories", `aria-current="page">Backups and storage`, "/static/backups.css"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}

	rec = do(t, a, "POST", "/settings/backups/run", url.Values{}, ck)
	if rec.Code != 303 || !strings.Contains(rec.Header().Get("Location"), "started") {
		t.Fatalf("run now = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	var runs int
	for i := 0; i < 100; i++ {
		rs, _ := a.db.BackupRuns(ctx, 5)
		if runs = len(rs); runs > 0 {
			if !rs[0].OK || rs[0].File == "" || rs[0].ActorName != "demi" || rs[0].Trigger != "manual" {
				t.Fatalf("run = %+v", rs[0])
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if runs == 0 {
		t.Fatal("backup never recorded")
	}
	last, _ := a.db.LastBackupRun(ctx)

	body = do(t, a, "GET", "/settings/backups", nil, ck).Body.String()
	if !strings.Contains(body, "/settings/backups/download/"+last.File) || !strings.Contains(body, "Succeeded") {
		t.Errorf("page doesn't list the snapshot:\n%s", body)
	}
	rec = do(t, a, "GET", "/settings/backups/download/"+last.File, nil, ck)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/gzip" ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") || rec.Body.Len() != int(last.Size) {
		t.Fatalf("download = %d %v (%d bytes, want %d)", rec.Code, rec.Header(), rec.Body.Len(), last.Size)
	}
	if rec := do(t, a, "GET", "/settings/backups/download/"+last.File, nil, nil); rec.Code != 303 {
		t.Errorf("signed-out download = %d", rec.Code)
	}
	for _, bad := range []string{"..%2Fatelier.db", "atelier.db", "atelier-20200101-000000.tar.gz"} {
		if rec := do(t, a, "GET", "/settings/backups/download/"+bad, nil, ck); rec.Code != 404 {
			t.Errorf("download %q = %d", bad, rec.Code)
		}
	}
}
