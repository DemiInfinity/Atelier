package importer

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Creds authenticate git over HTTPS. They are passed to git through the
// environment (GIT_CONFIG_COUNT/KEY/VALUE → http.extraHeader), never on the
// command line or in a URL, so they don't show up in process lists or errors.
type Creds struct {
	Username string
	Token    string
}

func (c Creds) Empty() bool { return c.Token == "" }

// CleanURL validates a git URL and splits off any credentials in it.
// Allowed: https, http, git, ssh (keys must already be set up for the server
// user) and absolute local paths or file:// URLs.
func CleanURL(raw string) (string, Creds, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", Creds{}, errors.New("enter a git URL")
	}
	if strings.HasPrefix(raw, "-") {
		return "", Creds{}, errors.New("that doesn't look like a git URL")
	}
	if filepath.IsAbs(raw) {
		return raw, Creds{}, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return "", Creds{}, errors.New("use a full URL such as https://example.com/me/project.git")
	}
	var c Creds
	switch u.Scheme {
	case "https", "http":
		if u.Host == "" {
			return "", Creds{}, errors.New("the URL has no host")
		}
		if u.User != nil {
			c.Username = u.User.Username()
			c.Token, _ = u.User.Password()
			u.User = nil
		}
	case "git", "ssh":
		if u.Host == "" {
			return "", Creds{}, errors.New("the URL has no host")
		}
		if _, hasPass := u.User.Password(); hasPass {
			return "", Creds{}, errors.New("don't put a password in an ssh:// or git:// URL")
		}
	case "file":
		if u.Path == "" {
			return "", Creds{}, errors.New("the URL has no path")
		}
	default:
		return "", Creds{}, fmt.Errorf("%s:// URLs aren't supported; use https://", u.Scheme)
	}
	return u.String(), c, nil
}

// gitEnv builds a clean environment for git with optional credentials.
func gitEnv(c Creds) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") || strings.HasPrefix(kv, "GIT_CONFIG_KEY_") ||
			strings.HasPrefix(kv, "GIT_CONFIG_VALUE_") || strings.HasPrefix(kv, "GIT_ASKPASS=") ||
			strings.HasPrefix(kv, "SSH_ASKPASS=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GCM_INTERACTIVE=never")
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	// Credential helpers on the server must not supply or store anything.
	cfg := [][2]string{{"credential.helper", ""}}
	if !c.Empty() {
		user := c.Username
		if user == "" {
			user = "x-access-token"
		}
		basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + c.Token))
		cfg = append(cfg, [2]string{"http.extraHeader", "Authorization: Basic " + basic})
	}
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg)))
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}

// scrub removes credentials from text that may be shown or logged.
func scrub(s string, c Creds) string {
	if c.Token != "" {
		s = strings.ReplaceAll(s, c.Token, "***")
		user := c.Username
		if user == "" {
			user = "x-access-token"
		}
		s = strings.ReplaceAll(s, base64.StdEncoding.EncodeToString([]byte(user+":"+c.Token)), "***")
	}
	return s
}

func runGit(ctx context.Context, gitBin, dir string, c Creds, args ...string) ([]byte, error) {
	if gitBin == "" {
		gitBin = "git"
	}
	cmd := exec.CommandContext(ctx, gitBin, args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(c)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(scrub(stderr.String(), c))
		if len(msg) > 600 {
			msg = msg[len(msg)-600:]
		}
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("git %s: %s", args[0], msg)
	}
	return out, nil
}

// Mirror clones src as a bare mirror into dst (which must not exist), keeps
// only branches and tags, and enables pushes over HTTP. With keepRemote the
// "origin" remote stays (fetching heads and tags) for later syncing;
// otherwise it is removed so nothing points back at the source.
func Mirror(ctx context.Context, gitBin, src string, c Creds, dst string, keepRemote bool) error {
	if _, err := os.Stat(dst); err == nil {
		return errors.New("a repository already exists at that path")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if _, err := runGit(ctx, gitBin, filepath.Dir(dst), c, "clone", "--mirror", "--quiet", "--", src, dst); err != nil {
		os.RemoveAll(dst)
		return err
	}
	steps := [][]string{{"config", "http.receivepack", "true"}}
	if keepRemote {
		steps = append(steps,
			[]string{"config", "--unset-all", "remote.origin.fetch"},
			[]string{"config", "--add", "remote.origin.fetch", "+refs/heads/*:refs/heads/*"},
			[]string{"config", "--add", "remote.origin.fetch", "+refs/tags/*:refs/tags/*"},
		)
	} else {
		// Not `git remote remove`: with a mirror refspec it would delete every ref.
		steps = append(steps, []string{"config", "--remove-section", "remote.origin"})
	}
	for _, s := range steps {
		if _, err := runGit(ctx, gitBin, dst, Creds{}, s...); err != nil {
			os.RemoveAll(dst)
			return err
		}
	}
	if err := dropExtraRefs(ctx, gitBin, dst); err != nil {
		os.RemoveAll(dst)
		return err
	}
	return nil
}

// dropExtraRefs deletes refs other than branches and tags (e.g. GitHub's
// refs/pull/*), which Atelier doesn't use.
func dropExtraRefs(ctx context.Context, gitBin, dir string) error {
	out, err := runGit(ctx, gitBin, dir, Creds{}, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return err
	}
	var del strings.Builder
	for _, ref := range strings.Fields(string(out)) {
		if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
			fmt.Fprintf(&del, "delete %s\n", ref)
		}
	}
	if del.Len() == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, gitOr(gitBin), "update-ref", "--stdin")
	cmd.Dir = dir
	cmd.Env = gitEnv(Creds{})
	cmd.Stdin = strings.NewReader(del.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git update-ref: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gitOr(b string) string {
	if b == "" {
		return "git"
	}
	return b
}

// Fetch updates a mirror from its origin remote, pruning deleted branches.
func Fetch(ctx context.Context, gitBin, dir string, c Creds) error {
	_, err := runGit(ctx, gitBin, dir, c, "fetch", "--prune", "--quiet", "origin")
	return err
}

// Unmirror removes the origin remote from a repository that stops mirroring.
func Unmirror(ctx context.Context, gitBin, dir string) error {
	_, err := runGit(ctx, gitBin, dir, Creds{}, "config", "--remove-section", "remote.origin")
	return err
}
