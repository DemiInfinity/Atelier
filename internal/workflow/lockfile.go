package workflow

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// LockfilePath is where a repository pins its actions.
const LockfilePath = ".atelier/actions.lock"

// Kinds of `uses:` references.
const (
	UsesLocal   = "local"   // ./path in the same repository
	UsesDocker  = "docker"  // docker://image
	UsesRemote  = "remote"  // owner/repo[/path]@ref on GitHub
	UsesAtelier = "atelier" // atelier:owner/repo[/path]@ref on this server
)

// ActionRef is a parsed `uses:` value.
type ActionRef struct {
	Raw   string
	Kind  string
	Owner string
	Repo  string
	Path  string // sub-directory inside the repository, if any
	Ref   string // tag, branch or commit
}

var (
	shaRe      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	usesNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	usesRefRe  = regexp.MustCompile(`^[A-Za-z0-9_./+-]{1,200}$`)
)

// IsSHA reports whether s is a full commit hash.
func IsSHA(s string) bool { return shaRe.MatchString(s) }

// ParseUses parses a step's uses value.
func ParseUses(s string) (ActionRef, error) {
	r := ActionRef{Raw: s}
	switch {
	case strings.HasPrefix(s, "./"):
		r.Kind = UsesLocal
		return r, nil
	case strings.HasPrefix(s, "docker://"):
		r.Kind = UsesDocker
		if len(s) == len("docker://") {
			return r, errors.New("docker:// needs an image")
		}
		return r, nil
	case strings.Contains(s, "${{"):
		return r, errors.New("uses can't contain expressions")
	}
	rest := s
	r.Kind = UsesRemote
	if after, ok := strings.CutPrefix(s, "atelier:"); ok {
		r.Kind, rest = UsesAtelier, after
	}
	name, ref, ok := strings.Cut(rest, "@")
	if !ok || ref == "" {
		return r, fmt.Errorf("%q needs a version, e.g. owner/repo@v4", s)
	}
	parts := strings.Split(name, "/")
	if len(parts) < 2 {
		return r, fmt.Errorf("%q must look like owner/repo@ref", s)
	}
	for _, p := range parts {
		if !usesNameRe.MatchString(p) || p == "." || p == ".." {
			return r, fmt.Errorf("%q has an invalid name", s)
		}
	}
	if !usesRefRe.MatchString(ref) || strings.Contains(ref, "..") || strings.HasPrefix(ref, "-") {
		return r, fmt.Errorf("%q has an invalid version", s)
	}
	r.Owner, r.Repo, r.Ref = parts[0], parts[1], ref
	r.Path = strings.Join(parts[2:], "/")
	return r, nil
}

// LockKey is the lockfile key: "owner/repo@ref" (or "atelier:owner/repo@ref").
func (r ActionRef) LockKey() string {
	k := r.Owner + "/" + r.Repo + "@" + r.Ref
	if r.Kind == UsesAtelier {
		return "atelier:" + k
	}
	return k
}

// Pinned is the uses value with the ref replaced by sha.
func (r ActionRef) Pinned(sha string) string {
	name := r.Owner + "/" + r.Repo
	if r.Path != "" {
		name += "/" + r.Path
	}
	if r.Kind == UsesAtelier {
		name = "atelier:" + name
	}
	return name + "@" + sha
}

// NeedsPin reports whether this reference has to be pinned to a commit.
func (r ActionRef) NeedsPin() bool {
	return (r.Kind == UsesRemote || r.Kind == UsesAtelier) && !IsSHA(r.Ref) && !r.IsBuiltin()
}

// Lockfile maps "owner/repo@tag" to a full commit sha.
type Lockfile map[string]string

// MaxLockfile bounds the lockfile size.
const MaxLockfile = 256 * 1024

// ParseLockfile reads a lockfile. Entries that aren't full shas are an error.
func ParseLockfile(b []byte) (Lockfile, error) {
	if len(b) > MaxLockfile {
		return nil, errors.New("the lockfile is too large")
	}
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("the lockfile isn't a JSON object of \"owner/repo@tag\": \"sha\": %v", err)
	}
	out := Lockfile{}
	for k, v := range raw {
		if !IsSHA(v) {
			return nil, fmt.Errorf("lockfile entry %q must be a full 40-character commit sha", k)
		}
		out[k] = v
	}
	return out, nil
}

// Marshal writes the lockfile with sorted keys and a trailing newline.
func (l Lockfile) Marshal() []byte {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteString("{\n")
	for i, k := range keys {
		kj, _ := json.Marshal(k)
		vj, _ := json.Marshal(l[k])
		b.WriteString("  " + string(kj) + ": " + string(vj))
		if i < len(keys)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return b.Bytes()
}

// ResolveFunc finds the commit a reference points at.
type ResolveFunc func(ctx context.Context, r ActionRef) (string, error)

// PinResult is the outcome of pinning a workflow's actions.
type PinResult struct {
	Pins      map[string]string // uses as written → pinned uses
	Tags      map[string]string // uses as written → the ref it was pinned from
	Resolved  Lockfile          // entries resolved without the lockfile
	Generated Lockfile          // the lockfile to commit (existing + resolved)
	Errors    []string
}

// Pin pins every uses value to a commit: from the lockfile when it has the
// entry, otherwise through resolve (unless requireLock is set, which makes a
// missing entry an error).
func Pin(ctx context.Context, uses []string, lock Lockfile, resolve ResolveFunc, requireLock bool) PinResult {
	res := PinResult{Pins: map[string]string{}, Tags: map[string]string{}, Resolved: Lockfile{}, Generated: Lockfile{}}
	for k, v := range lock {
		res.Generated[k] = v
	}
	for _, u := range uses {
		r, err := ParseUses(u)
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
			continue
		}
		if !r.NeedsPin() {
			res.Pins[u] = u
			continue
		}
		key := r.LockKey()
		if sha, ok := lock[key]; ok {
			res.Pins[u], res.Tags[u] = r.Pinned(sha), r.Ref
			continue
		}
		if sha, ok := res.Resolved[key]; ok {
			res.Pins[u], res.Tags[u] = r.Pinned(sha), r.Ref
			continue
		}
		if requireLock {
			res.Errors = append(res.Errors, fmt.Sprintf("%s isn't in %s, and this repository requires a lockfile.", key, LockfilePath))
			continue
		}
		if resolve == nil {
			res.Errors = append(res.Errors, fmt.Sprintf("Can't resolve %s.", key))
			continue
		}
		sha, err := resolve(ctx, r)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("Can't resolve %s: %v", key, err))
			continue
		}
		res.Resolved[key], res.Generated[key] = sha, sha
		res.Pins[u], res.Tags[u] = r.Pinned(sha), r.Ref
	}
	return res
}

// ---------- ls-remote ----------

// LsRemote resolves owner/repo@ref with `git ls-remote <Base>/owner/repo`.
// Results are cached for CacheFor.
type LsRemote struct {
	Git      string        // git binary; "git" if empty
	Base     string        // e.g. https://github.com (no trailing slash)
	Timeout  time.Duration // per lookup; 20s if zero
	CacheFor time.Duration // 10m if zero

	mu    sync.Mutex
	cache map[string]lsEntry
}

type lsEntry struct {
	sha string
	at  time.Time
}

// Resolve implements ResolveFunc for remote references.
func (l *LsRemote) Resolve(ctx context.Context, r ActionRef) (string, error) {
	if IsSHA(r.Ref) {
		return r.Ref, nil
	}
	url := strings.TrimRight(l.Base, "/") + "/" + r.Owner + "/" + r.Repo
	key := url + "@" + r.Ref
	ttl := l.CacheFor
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	l.mu.Lock()
	if e, ok := l.cache[key]; ok && time.Since(e.at) < ttl {
		l.mu.Unlock()
		return e.sha, nil
	}
	l.mu.Unlock()

	sha, err := LsRemoteRef(ctx, l.Git, url, r.Ref, l.Timeout)
	if err != nil {
		return "", err
	}
	l.mu.Lock()
	if l.cache == nil {
		l.cache = map[string]lsEntry{}
	}
	l.cache[key] = lsEntry{sha: sha, at: time.Now()}
	l.mu.Unlock()
	return sha, nil
}

// LsRemoteRef runs git ls-remote and picks the commit for ref: a peeled tag
// first, then a tag, then a branch.
func LsRemoteRef(ctx context.Context, gitBin, url, ref string, timeout time.Duration) (string, error) {
	if gitBin == "" {
		gitBin = "git"
	}
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitBin, "ls-remote", "--", url, "refs/tags/"+ref, "refs/tags/"+ref+"^{}", "refs/heads/"+ref)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return "", fmt.Errorf("git ls-remote %s: %v %s", url, err, msg)
	}
	found := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && IsSHA(f[0]) {
			found[f[1]] = f[0]
		}
	}
	for _, k := range []string{"refs/tags/" + ref + "^{}", "refs/tags/" + ref, "refs/heads/" + ref} {
		if sha, ok := found[k]; ok {
			return sha, nil
		}
	}
	return "", fmt.Errorf("no tag or branch %q in %s", ref, url)
}
