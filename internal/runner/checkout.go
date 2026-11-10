package runner

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// checkout is the built-in replacement for actions/checkout. The token is
// passed to git as an http.extraheader through GIT_CONFIG_* environment
// variables (and, with persist-credentials, an included config file), never
// on the command line.
func (j *jobRun) checkout(ctx context.Context, sc *scope, with map[string]string) (int, error) {
	repo := strings.TrimSpace(with["repository"])
	if repo == "" {
		repo = j.repoFull()
	}
	cloneURL, err := cloneURLFor(j.job.CloneURL, j.repoFull(), repo)
	if err != nil {
		return 1, err
	}
	token := with["token"]
	if token == "" {
		token = j.job.JobToken
	} else {
		j.mask.Add(token)
	}
	dest := j.ws
	if p := strings.TrimSpace(with["path"]); p != "" {
		dest = filepath.Join(j.ws, filepath.FromSlash(p))
		if rel, err := filepath.Rel(j.ws, dest); err != nil || strings.HasPrefix(rel, "..") {
			return 1, fmt.Errorf("checkout path %q is outside the workspace", p)
		}
	}
	depth := 1
	if d := strings.TrimSpace(with["fetch-depth"]); d != "" {
		if depth, err = strconv.Atoi(d); err != nil || depth < 0 {
			return 1, fmt.Errorf("invalid fetch-depth %q", d)
		}
	}
	persist := !strings.EqualFold(strings.TrimSpace(with["persist-credentials"]), "false")

	ref := strings.TrimSpace(with["ref"])
	sameRepo := strings.EqualFold(repo, j.repoFull())
	branch := ""
	if ref == "" && sameRepo {
		ref = j.job.SHA
		if strings.HasPrefix(j.job.Ref, "refs/heads/") {
			branch = strings.TrimPrefix(j.job.Ref, "refs/heads/")
		}
	}

	env := osEnv()
	env.Set("GIT_TERMINAL_PROMPT", "0")
	env.Set("GCM_INTERACTIVE", "never")
	header := ""
	if token != "" {
		auth := base64.StdEncoding.EncodeToString([]byte("atelier:" + token))
		j.mask.Add(auth)
		header = "AUTHORIZATION: basic " + auth
		env.Set("GIT_CONFIG_COUNT", "1")
		env.Set("GIT_CONFIG_KEY_0", "http."+urlOrigin(cloneURL)+"/.extraheader")
		env.Set("GIT_CONFIG_VALUE_0", header)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return 1, err
	}
	step := sc.logStep
	j.log.System(step, fmt.Sprintf("Checking out %s@%s into %s", repo, orDefault(ref, "default branch"), dest))
	git := func(args ...string) (int, error) {
		return j.run(ctx, step, append([]string{"git"}, args...), "", dest, env, newCmdState())
	}
	steps := [][]string{
		{"init", "-q", dest},
		{"-C", dest, "remote", "add", "origin", cloneURL},
		{"-C", dest, "config", "--local", "gc.auto", "0"},
	}
	for _, a := range steps {
		if code, err := git(a...); code != 0 || err != nil {
			return failCode(code), err
		}
	}
	fetch := []string{"-C", dest, "-c", "protocol.version=2", "fetch", "--no-tags", "--prune", "--no-recurse-submodules", "--progress"}
	if depth > 0 {
		fetch = append(fetch, "--depth="+strconv.Itoa(depth))
	}
	isSHA := shaRe.MatchString(strings.ToLower(ref))
	target := ""
	switch {
	case depth == 0:
		code, err := git(append(fetch, "origin", "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*")...)
		if code != 0 || err != nil {
			return failCode(code), err
		}
		target = ref
		if !isSHA && ref != "" {
			if code, _ := git("-C", dest, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+ref); code == 0 {
				branch, target = ref, "refs/remotes/origin/"+ref
			} else {
				target = ref
			}
		}
		if ref == "" {
			target = "FETCH_HEAD"
		}
	case isSHA:
		if code, _ := git(append(fetch, "origin", ref)...); code != 0 {
			// servers that refuse fetching by sha: fetch the ref, then the sha must be in it
			fallback := j.job.Ref
			if fallback == "" {
				return 1, fmt.Errorf("could not fetch %s", ref)
			}
			args := []string{"-C", dest, "-c", "protocol.version=2", "fetch", "--no-tags", "--prune", "origin", fallback}
			if code, err := git(args...); code != 0 || err != nil {
				return failCode(code), err
			}
		}
		target = ref
	case ref == "":
		if code, err := git(append(fetch, "origin", "HEAD")...); code != 0 || err != nil {
			return failCode(code), err
		}
		target = "FETCH_HEAD"
	default:
		name := strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/")
		if !strings.HasPrefix(ref, "refs/tags/") {
			if code, _ := git(append(fetch, "origin", "+refs/heads/"+name+":refs/remotes/origin/"+name)...); code == 0 {
				branch, target = name, "refs/remotes/origin/"+name
				break
			}
		}
		if code, err := git(append(fetch, "origin", "+refs/tags/"+name+":refs/tags/"+name)...); code != 0 || err != nil {
			return failCode(code), fmt.Errorf("ref %q not found", ref)
		}
		target = "refs/tags/" + name
	}
	var co []string
	if branch != "" {
		co = []string{"-C", dest, "checkout", "--force", "-B", branch, target}
	} else {
		co = []string{"-C", dest, "checkout", "--force", "--detach", target}
	}
	if code, err := git(co...); code != 0 || err != nil {
		return failCode(code), err
	}
	if persist && header != "" {
		gitDir := filepath.Join(dest, ".git")
		cfg := fmt.Sprintf("[http %q]\n\textraheader = %s\n", urlOrigin(cloneURL)+"/", header)
		if err := os.WriteFile(filepath.Join(gitDir, "atelier-credentials.config"), []byte(cfg), 0o600); err != nil {
			return 1, err
		}
		if code, err := git("-C", dest, "config", "--local", "include.path", "atelier-credentials.config"); code != 0 || err != nil {
			return failCode(code), err
		}
	}
	_, _ = git("-C", dest, "log", "-1", "--format=HEAD is now at %h %s")
	return 0, nil
}

func failCode(c int) int {
	if c == 0 {
		return 1
	}
	return c
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// urlOrigin is scheme://host[:port] of a URL (no credentials).
func urlOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Scheme + "://" + u.Host
}

// cloneURLFor derives the clone URL of another repository on the same server.
func cloneURLFor(base, baseRepo, repo string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || base == "" {
		return "", fmt.Errorf("the job has no usable clone_url")
	}
	u.User = nil
	if strings.EqualFold(repo, baseRepo) {
		return u.String(), nil
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.Contains(repo, "..") {
		return "", fmt.Errorf("invalid repository %q", repo)
	}
	p := u.Path
	suffix := ""
	if strings.HasSuffix(p, ".git") {
		suffix = ".git"
	}
	i := strings.LastIndex(strings.ToLower(p), strings.ToLower(baseRepo))
	if i < 0 {
		return "", fmt.Errorf("cannot derive the clone URL for %s", repo)
	}
	u.Path = p[:i] + repo + suffix
	return u.String(), nil
}
