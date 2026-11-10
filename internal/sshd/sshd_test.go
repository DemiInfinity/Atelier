package sshd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

func TestParseCommand(t *testing.T) {
	good := []struct {
		in, svc, owner, repo string
	}{
		{"git-upload-pack 'deminimmo/atelier.git'", "git-upload-pack", "deminimmo", "atelier"},
		{"git-receive-pack 'deminimmo/atelier.git'", "git-receive-pack", "deminimmo", "atelier"},
		{"git-upload-pack '/deminimmo/atelier.git'", "git-upload-pack", "deminimmo", "atelier"},
		{"git-upload-pack 'deminimmo/atelier'", "git-upload-pack", "deminimmo", "atelier"},
		{"git-upload-pack '/Elyndra/rebel-verify.GIT'", "git-upload-pack", "Elyndra", "rebel-verify"},
		{"git-upload-pack deminimmo/atelier.git", "git-upload-pack", "deminimmo", "atelier"},
		{"git-upload-pack 'a/b.c_d-e.git'", "git-upload-pack", "a", "b.c_d-e"},
	}
	for _, tc := range good {
		c, err := ParseCommand(tc.in)
		if err != nil {
			t.Errorf("ParseCommand(%q): %v", tc.in, err)
			continue
		}
		if string(c.Service) != tc.svc || c.Owner != tc.owner || c.Repo != tc.repo {
			t.Errorf("ParseCommand(%q) = %+v", tc.in, c)
		}
	}

	bad := []string{
		"",
		"git-upload-pack 'a/b.git'; rm -rf /",
		"git-upload-pack 'a/b.git' ; rm -rf /",
		"git-upload-pack 'a/b.git'\nrm -rf /",
		"git-upload-pack 'a/b.git' && id",
		"git-upload-pack 'a/b.git'|id",
		"git-upload-pack '$(id)/b.git'",
		"git-upload-pack '`id`/b.git'",
		"git-upload-pack 'a/b.git",    // unbalanced quote
		"git-upload-pack a/b.git'",    // unbalanced quote
		"git-upload-pack \"a/b.git\"", // double quotes
		"git-upload-pack '../etc/passwd'",
		"git-upload-pack '../../a/b.git'",
		"git-upload-pack 'a/../b.git'",
		"git-upload-pack 'a/b/c.git'",
		"git-upload-pack 'a'",
		"git-upload-pack '//a/b.git'",
		"git-upload-pack '.a/b.git'",
		"git-upload-pack 'a/.git'",
		"git-upload-pack 'a/b.git.git'",
		"git-upload-pack  'a/b.git'",
		" git-upload-pack 'a/b.git'",
		"git-upload-pack 'a/b.git' ",
		"git-upload-archive 'a/b.git'",
		"git upload-pack 'a/b.git'",
		"git-upload-pack --upload-pack=id 'a/b.git'",
		"git-upload-pack '-a/b.git'",
		"git-upload-pack 'a/b\x00.git'",
		"sh -c 'git-upload-pack a/b.git'",
		"GIT_DIR=/ git-upload-pack 'a/b.git'",
		"git-upload-pack '" + strings.Repeat("a", 101) + "/b.git'",
		"git-upload-pack 'a/" + strings.Repeat("b", 101) + "'",
		"git-upload-pack 'a/b c.git'",
		"git-receive-pack '~/b.git'",
	}
	for _, in := range bad {
		if c, err := ParseCommand(in); err == nil {
			t.Errorf("ParseCommand(%q) accepted: %+v", in, c)
		}
	}
}

func TestParsePublicKey(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sp, _ := ssh.NewPublicKey(pub)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp)))

	k, err := ParsePublicKey("  " + line + "  me@laptop \n")
	if err != nil {
		t.Fatal(err)
	}
	if k.Type != "ssh-ed25519" || k.Comment != "me@laptop" || k.Normalized != line || k.Fingerprint != ssh.FingerprintSHA256(sp) {
		t.Fatalf("unexpected %+v", k)
	}
	if !strings.HasPrefix(k.Fingerprint, "SHA256:") {
		t.Fatalf("fingerprint %q", k.Fingerprint)
	}

	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	smallPub, _ := ssh.NewPublicKey(&small.PublicKey)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	privPEM, _ := ssh.MarshalPrivateKey(priv, "")

	for name, in := range map[string]string{
		"empty":       "",
		"garbage":     "hello world",
		"private key": string(pem.EncodeToMemory(privPEM)),
		"two keys":    line + "\n" + line,
		"options":     `command="rm -rf /" ` + line,
		"small rsa":   string(ssh.MarshalAuthorizedKey(smallPub)),
		"truncated":   line[:40],
	} {
		if _, err := ParsePublicKey(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHostKeyPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "ssh_host_ed25519_key")
	a, err := LoadOrCreateHostKey(p)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("host key mode %v", fi.Mode().Perm())
	}
	b, err := LoadOrCreateHostKey(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.PublicKey().Marshal(), b.PublicKey().Marshal()) {
		t.Fatal("host key changed between loads")
	}
	if a.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Fatalf("host key type %s", a.PublicKey().Type())
	}
}

// ---------- end to end ----------

type fixture struct {
	db      *store.Store
	git     *gitrepo.Store
	srv     *Server
	addr    string
	port    string
	keyPath string
	signer  ssh.Signer
	repo    store.Repo
	dir     string
}

func isolateGit(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("GIT_AUTHOR_NAME", "Test Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "author@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test Author")
	t.Setenv("GIT_COMMITTER_EMAIL", "author@example.com")
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	isolateGit(t)
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "atelier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	g, err := gitrepo.New(filepath.Join(dir, "repos"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	o, err := db.CreateOwner(ctx, "deminimmo", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := db.CreateRepo(ctx, o.ID, "Atelier", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Init(ctx, repo.Owner, repo.Name); err != nil {
		t.Fatal(err)
	}

	// The client's key, stored in OpenSSH format for the ssh binary.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(priv, "test")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, _ := ssh.NewSignerFromKey(priv)
	pk, err := ParsePublicKey(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddSSHKey(ctx, "laptop", pk.Type, pk.Fingerprint, pk.Normalized, "deminimmo", "owner"); err != nil {
		t.Fatal(err)
	}

	srv, err := New(db, g, filepath.Join(dir, "ssh_host_ed25519_key"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Logf = t.Logf
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("server did not stop")
		}
	})
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return &fixture{db: db, git: g, srv: srv, addr: ln.Addr().String(), port: port, keyPath: keyPath, signer: signer, repo: repo, dir: dir}
}

func (f *fixture) client(t *testing.T, signer ssh.Signer) (*ssh.Client, error) {
	t.Helper()
	return ssh.Dial("tcp", f.addr, &ssh.ClientConfig{
		User:            "git",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(f.srv.HostKey()),
		Timeout:         5 * time.Second,
	})
}

// run executes one exec request and returns stderr and the exit status.
func run(t *testing.T, c *ssh.Client, cmd string) (string, int) {
	t.Helper()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var stderr bytes.Buffer
	s.Stderr = &stderr
	err = s.Run(cmd)
	var ee *ssh.ExitError
	switch {
	case err == nil:
		return stderr.String(), 0
	case errors.As(err, &ee):
		return stderr.String(), ee.ExitStatus()
	}
	t.Fatalf("run %q: %v", cmd, err)
	return "", -1
}

func TestSSHRefusals(t *testing.T) {
	f := newFixture(t)

	// Unknown key.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := ssh.NewSignerFromKey(other)
	if c, err := f.client(t, otherSigner); err == nil {
		c.Close()
		t.Fatal("unknown key was accepted")
	}

	c, err := f.client(t, f.signer)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for _, cmd := range []string{
		"git-upload-pack 'deminimmo/atelier.git'; touch /tmp/atelier-pwned",
		"ls -la",
		"git-upload-archive 'deminimmo/atelier.git'",
	} {
		msg, code := run(t, c, cmd)
		if code == 0 || !strings.Contains(msg, "only git clone, fetch and push") {
			t.Errorf("%q: code %d, stderr %q", cmd, code, msg)
		}
	}
	if _, err := os.Stat("/tmp/atelier-pwned"); err == nil {
		t.Fatal("injected command ran")
	}

	msg, code := run(t, c, "git-upload-pack 'deminimmo/missing.git'")
	if code == 0 || !strings.Contains(msg, "not found") {
		t.Errorf("missing repo: code %d, stderr %q", code, msg)
	}

	// No PTY, and a shell only prints a greeting.
	s, _ := c.NewSession()
	if err := s.RequestPty("xterm", 40, 80, ssh.TerminalModes{}); err == nil {
		t.Error("pty request accepted")
	}
	var out bytes.Buffer
	s.Stderr = &out
	if err := s.Shell(); err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(); err == nil || !strings.Contains(out.String(), "does not provide shell access") {
		t.Errorf("shell: %v %q", err, out.String())
	}
	s.Close()

	// No port forwarding, either direction.
	if _, err := c.Dial("tcp", "127.0.0.1:1"); err == nil {
		t.Error("direct-tcpip accepted")
	}
	if ln, err := c.Listen("tcp", "127.0.0.1:0"); err == nil {
		ln.Close()
		t.Error("tcpip-forward accepted")
	}

	// A deleted key stops working.
	keys, _ := f.db.SSHKeys(context.Background())
	if len(keys) != 1 || keys[0].LastUsedAt == nil {
		t.Fatalf("expected the key to be marked used: %+v", keys)
	}
	if err := f.db.DeleteSSHKey(context.Background(), keys[0].ID); err != nil {
		t.Fatal(err)
	}
	if c2, err := f.client(t, f.signer); err == nil {
		c2.Close()
		t.Fatal("deleted key was accepted")
	}
}

func gitCmd(t *testing.T, env []string, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestSSHCloneAndPush(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh client not installed")
	}
	f := newFixture(t)
	before := f.repo.UpdatedAt
	sshCmd := fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes -p %s", f.keyPath, f.port)
	env := []string{"GIT_SSH_COMMAND=" + sshCmd}
	work := t.TempDir()

	// Clone the empty repo (no leading slash form via scp-like URL is not portable with a port, so use ssh://).
	url := "ssh://git@127.0.0.1:" + f.port + "/deminimmo/atelier.git"
	if out, err := gitCmd(t, env, work, "clone", url, "one"); err != nil {
		t.Fatalf("clone empty: %v\n%s", err, out)
	}
	one := filepath.Join(work, "one")
	if err := os.WriteFile(filepath.Join(one, "README.md"), []byte("# Atelier\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "README.md"},
		{"commit", "-m", "First commit"},
		{"branch", "-M", "main"},
	} {
		if out, err := gitCmd(t, env, one, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	time.Sleep(1100 * time.Millisecond) // updated_at has one-second resolution
	if out, err := gitCmd(t, env, one, "push", "-u", "origin", "main"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}

	r, err := f.db.RepoByName(context.Background(), "deminimmo", "atelier")
	if err != nil {
		t.Fatal(err)
	}
	if !r.UpdatedAt.After(before) {
		t.Errorf("updated_at not touched after push: %v -> %v", before, r.UpdatedAt)
	}

	// Clone again with the path variants (no .git, mixed case) and protocol v2.
	url2 := "ssh://git@127.0.0.1:" + f.port + "/DemiNimmo/Atelier"
	if out, err := gitCmd(t, append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=protocol.version", "GIT_CONFIG_VALUE_0=2"), work, "clone", url2, "two"); err != nil {
		t.Fatalf("second clone: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(work, "two", "README.md"))
	if err != nil || string(data) != "# Atelier\n" {
		t.Fatalf("cloned README: %q %v", data, err)
	}

	// Pushing to a repo that doesn't exist fails cleanly.
	if out, err := gitCmd(t, env, one, "push", "ssh://git@127.0.0.1:"+f.port+"/deminimmo/nope.git", "main"); err == nil || !strings.Contains(out, "not found") {
		t.Fatalf("push to missing repo: %v\n%s", err, out)
	}
}
