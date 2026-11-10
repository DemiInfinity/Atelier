package runner

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// fetchAtelierAction fetches an action that lives in a repository on the same
// Atelier server as the job (`uses: atelier:owner/repo@sha`). It uses the job
// token, so it can read the job's own repository and any public repository;
// private repositories other than the job's are refused by the server.
// Results are cached by sha like GitHub actions.
func (j *jobRun) fetchAtelierAction(ctx context.Context, owner, repo, sha string) (string, error) {
	_, cache := j.r.Config.Dirs()
	dest := filepath.Join(cache, "actions", "atelier", safeName(owner), safeName(repo), sha)
	if _, err := os.Stat(filepath.Join(dest, ".atelier-complete")); err == nil {
		return dest, nil
	}
	j.r.fetchMu.Lock()
	defer j.r.fetchMu.Unlock()
	if _, err := os.Stat(filepath.Join(dest, ".atelier-complete")); err == nil {
		return dest, nil
	}
	cloneURL, err := cloneURLFor(j.job.CloneURL, j.repoFull(), owner+"/"+repo)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".dl-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	env := osEnv()
	env.Set("GIT_TERMINAL_PROMPT", "0")
	env.Set("GCM_INTERACTIVE", "never")
	if j.job.JobToken != "" {
		auth := base64.StdEncoding.EncodeToString([]byte("atelier:" + j.job.JobToken))
		j.mask.Add(auth)
		env.Set("GIT_CONFIG_COUNT", "1")
		env.Set("GIT_CONFIG_KEY_0", "http."+urlOrigin(cloneURL)+"/.extraheader")
		env.Set("GIT_CONFIG_VALUE_0", "AUTHORIZATION: basic "+auth)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", tmp}, args...)...)
		cmd.Env = env.Slice()
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %v: %s", args[0], err, j.mask.Mask(strings.TrimSpace(string(out))))
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := git("init", "-q"); err != nil {
		return "", err
	}
	if _, err := git("fetch", "-q", "--depth", "1", cloneURL, sha); err != nil {
		return "", err
	}
	if _, err := git("-c", "advice.detachedHead=false", "checkout", "-q", "--detach", "FETCH_HEAD"); err != nil {
		return "", err
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(head, sha) {
		return "", fmt.Errorf("fetched %s but got commit %s", sha, head)
	}
	if err := os.RemoveAll(filepath.Join(tmp, ".git")); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, ".atelier-complete"), []byte(sha+"\n"), 0o644); err != nil {
		return "", err
	}
	_ = os.RemoveAll(dest)
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return dest, nil
}
