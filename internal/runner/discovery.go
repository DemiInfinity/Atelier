package runner

import (
	"context"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"atelier/internal/actions"
)

const (
	discoveryInterval = 10 * time.Minute
	discoveryMaxDepth = 4
)

var discoverySkip = map[string]bool{"node_modules": true, "vendor": true, ".git": true, ".build": true, "Pods": true, ".venv": true, "target": true}

// Discover scans roots (max depth 4) for git repositories.
func Discover(ctx context.Context, roots []string) []actions.DiscoveredRepo {
	var out []actions.DiscoveredRepo
	seen := map[string]bool{}
	for _, root := range roots {
		root = filepath.Clean(root)
		if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
			continue
		}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return filepath.SkipAll
			}
			if err != nil || !d.IsDir() {
				return nil
			}
			if p != root && (discoverySkip[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
				if !seen[p] {
					seen[p] = true
					out = append(out, inspectRepo(ctx, p))
				}
				return filepath.SkipDir
			}
			rel, _ := filepath.Rel(root, p)
			if rel != "." && strings.Count(filepath.ToSlash(rel), "/")+1 >= discoveryMaxDepth {
				return filepath.SkipDir
			}
			return nil
		})
	}
	return out
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	b, err := cmd.Output()
	return strings.TrimSpace(string(b)), err
}

func inspectRepo(ctx context.Context, dir string) actions.DiscoveredRepo {
	abs, _ := filepath.Abs(dir)
	r := actions.DiscoveredRepo{Path: abs, Remotes: map[string]string{}}
	if out, err := gitOut(ctx, dir, "config", "--get-regexp", `^remote\..*\.url$`); err == nil {
		for _, l := range strings.Split(out, "\n") {
			k, v, ok := strings.Cut(l, " ")
			if !ok {
				continue
			}
			name := strings.TrimSuffix(strings.TrimPrefix(k, "remote."), ".url")
			r.Remotes[name] = StripCredentials(v)
		}
	}
	if b, err := gitOut(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD"); err == nil {
		r.Branch = b
	}
	if s, err := gitOut(ctx, dir, "status", "--porcelain", "--untracked-files=no"); err == nil && s != "" {
		r.Dirty = true
	}
	return r
}

// StripCredentials removes passwords and tokens from a remote URL. For http(s)
// all user info goes (tokens are often the "username"); ssh keeps the user.
func StripCredentials(raw string) string {
	if !strings.Contains(raw, "://") {
		return raw // scp-like git@host:path or a local path
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparseable remote URL]"
	}
	if u.User != nil {
		switch strings.ToLower(u.Scheme) {
		case "ssh", "git+ssh", "ssh+git":
			u.User = url.User(u.User.Username())
		default:
			u.User = nil
		}
	}
	q := u.Query()
	for k := range q {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "token") || strings.Contains(lk, "key") || strings.Contains(lk, "secret") || strings.Contains(lk, "password") {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// discoveryLoop reports repos now and every 10 minutes.
func (r *Runner) discoveryLoop(ctx context.Context) {
	roots := r.Config.DiscoveryPaths
	if len(roots) == 0 {
		roots = DefaultDiscoveryPaths()
	}
	interval := r.DiscoveryInterval
	if interval == 0 {
		interval = discoveryInterval
	}
	for {
		repos := Discover(ctx, roots)
		if ctx.Err() != nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		if err := r.Client.Discovery(cctx, actions.DiscoveryRequest{Repos: repos}); err != nil {
			r.logger().Warn("discovery report failed", "err", err)
		} else {
			r.logger().Info("reported repositories", "count", len(repos))
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
