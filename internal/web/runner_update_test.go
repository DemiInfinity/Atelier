package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"atelier/internal/actions"
)

func (fr *fakeRunner) pollUpdate(version, failed string) actions.PollResponse {
	fr.t.Helper()
	var resp actions.PollResponse
	if code := fr.call("/poll", actions.PollRequest{Labels: fr.labels, Capabilities: fr.caps, Version: version, UpdateFailed: failed}, &resp); code != 200 {
		fr.t.Fatalf("poll = %d", code)
	}
	return resp
}

func TestRunnerUpdates(t *testing.T) {
	f := newActFixture(t)
	f.a.actions().pollWait = 50e6 // 50ms
	dir := t.TempDir()
	build := []byte("new runner build")
	os.WriteFile(filepath.Join(dir, "atelier-runner-linux-amd64"), build, 0o755)
	os.WriteFile(filepath.Join(dir, "VERSION"), []byte("v2\n"), 0o644)
	f.a.cfg.RunnerDir = dir
	sum := sha256.Sum256(build)

	lx := f.registerRunner(t, "Linux box", []string{"linux"}, []string{"jobs"})
	if r := lx.pollUpdate("v1", ""); r.Update != nil {
		t.Fatalf("offered an update nobody asked for: %+v", r.Update)
	}
	body := expectStatus(t, do(t, f.a, "GET", "/settings/runners", nil, f.ck), 200, "runners")
	mustContain(t, body, "runners", "atelier-runner <span class=\"mono\">v1</span>", "Update available", "atelier-runner v2", "Update runners automatically")
	rns, _ := f.a.db.ActionsRunners(t.Context())
	id := itoa(rns[len(rns)-1].ID)

	// Update pressed: the next poll carries the offer and no job.
	expectStatus(t, do(t, f.a, "POST", "/settings/runners/"+id+"/update", url.Values{}, f.ck), 303, "update")
	mustContain(t, expectStatus(t, do(t, f.a, "GET", "/settings/runners", nil, f.ck), 200, "runners"), "updating", "Updating to")
	r := lx.pollUpdate("v1", "")
	if r.Update == nil || r.Update.Version != "v2" || r.Update.SHA256 != hex.EncodeToString(sum[:]) || r.Update.Size != int64(len(build)) || r.Job != nil {
		t.Fatalf("poll = %+v", r)
	}

	// The download is the build, for this runner only.
	req, _ := http.NewRequest("GET", f.srv.URL+actions.APIPrefix+"/update", nil)
	req.Header.Set("Authorization", "Bearer "+lx.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(got) != string(build) {
		t.Errorf("download = %d %q", resp.StatusCode, got)
	}
	resp, _ = http.Get(f.srv.URL + actions.APIPrefix + "/update")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("download without a token = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A build that failed on the runner isn't offered again.
	if r := lx.pollUpdate("v1", "v2"); r.Update != nil {
		t.Errorf("offered a failed build again")
	}
	// Once it reports v2, it is up to date and the request is gone.
	if r := lx.pollUpdate("v2", ""); r.Update != nil {
		t.Errorf("offered v2 to a v2 runner")
	}
	if f.a.runnerUpdateRequested(t.Context(), rns[len(rns)-1].ID) {
		t.Error("request not cleared")
	}
	body = expectStatus(t, do(t, f.a, "GET", "/settings/runners", nil, f.ck), 200, "runners")
	mustContain(t, body, "runners", "Up to date")

	// Automatic updates: an older runner is offered without pressing Update.
	expectStatus(t, do(t, f.a, "POST", "/settings/runners/auto-update", url.Values{"auto": {"1"}}, f.ck), 303, "auto")
	if r := lx.pollUpdate("v1", ""); r.Update == nil {
		t.Error("auto-update didn't offer")
	}
	// No build for its system: nothing to offer, and the button refuses.
	mac := f.registerRunner(t, "Mac", []string{"mac"}, []string{"jobs"})
	if r := mac.pollUpdate("v1", ""); r.Update != nil {
		t.Error("offered a build that doesn't exist")
	}
	if strings.Contains(f.a.runnerLatest(rns[0]), "v") && rns[0].OS == "darwin" {
		t.Error("latest for a system without a build")
	}
}
