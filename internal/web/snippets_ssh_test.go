package web

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"atelier/internal/config"
	"atelier/internal/sshd"
)

// sshTestKey writes an OpenSSH private key for the ssh binary and registers
// its public half for person (added_by decides whose key it is).
func sshTestKey(t *testing.T, a *App, person string) string {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(priv, person)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_"+person)
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, _ := ssh.NewSignerFromKey(priv)
	pk, err := sshd.ParsePublicKey(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.AddSSHKey(context.Background(), person, pk.Type, pk.Fingerprint, pk.Normalized, person, "owner"); err != nil {
		t.Fatal(err)
	}
	return path
}

// startSnippetSSH runs the SSH server wired the way the "ssh" feature wires it.
func startSnippetSSH(t *testing.T, a *App) string {
	t.Helper()
	srv, err := sshd.New(a.db, a.git, filepath.Join(t.TempDir(), "host_key"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Logf = t.Logf
	srv.Allow = a.sshAllow
	srv.Create = a.sshPushCreate
	srv.Snippet = a.sshSnippet
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("ssh server did not stop")
		}
	})
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

func sshGit(t *testing.T, key, port, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+fmt.Sprintf(
		"ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes -p %s", key, port))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestSnippetGitOverSSH(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh client not installed")
	}
	e := newSnipEnv(t)
	a := e.a
	ctx := context.Background()
	sn := e.create(t, snippetForm("zoe", "", "private", "hello.txt", "hello\n"))
	zoeKey := sshTestKey(t, a, "zoe")
	bobKey := sshTestKey(t, a, "bob")
	port := startSnippetSSH(t, a)
	remote := "ssh://git@127.0.0.1:" + port + "/snippets/" + sn.UID + ".git"

	// The owner clones and pushes, like over HTTPS.
	work := filepath.Join(t.TempDir(), "work")
	if out, err := sshGit(t, zoeKey, port, t.TempDir(), "clone", remote, work); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "hello.txt")); string(b) != "hello\n" {
		t.Fatalf("cloned hello.txt = %q", b)
	}
	if err := os.WriteFile(filepath.Join(work, "ssh.md"), []byte("# Over SSH\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "Add ssh.md")
	if out, err := sshGit(t, zoeKey, port, work, "push", "origin", "main"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	sn2, _ := a.db.SnippetByUID(ctx, sn.UID)
	if strings.Join(sn2.Files, ",") != "hello.txt,ssh.md" {
		t.Errorf("files after SSH push = %v (the file list wasn't refreshed)", sn2.Files)
	}

	// Someone who can't read a private snippet is told it doesn't exist.
	if out, err := sshGit(t, bobKey, port, t.TempDir(), "clone", remote, "x"); err == nil || !strings.Contains(out, "not found") {
		t.Errorf("bob cloned zoe's private snippet: %v\n%s", err, out)
	}
	// A public snippet can be cloned by anyone with a key, but only pushed by
	// those with write access.
	if err := a.db.UpdateSnippet(ctx, sn.ID, "", false); err != nil {
		t.Fatal(err)
	}
	bobWork := filepath.Join(t.TempDir(), "bob")
	if out, err := sshGit(t, bobKey, port, t.TempDir(), "clone", remote, bobWork); err != nil {
		t.Fatalf("bob clone of a public snippet: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(bobWork, "bob.txt"), []byte("bob\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, bobWork, "add", ".")
	runGit(t, bobWork, "commit", "-m", "bob")
	if out, err := sshGit(t, bobKey, port, bobWork, "push", "origin", "main"); err == nil || !strings.Contains(out, "permission") {
		t.Errorf("bob pushed to zoe's snippet: %v\n%s", err, out)
	}
	// Unknown snippets, and names that aren't snippet ids, are not found.
	for _, p := range []string{"/snippets/0000000000000000dead.git", "/snippets/not-a-uid.git"} {
		if out, err := sshGit(t, zoeKey, port, t.TempDir(), "clone", "ssh://git@127.0.0.1:"+port+p, "y"); err == nil || !strings.Contains(out, "not found") {
			t.Errorf("clone %s: %v\n%s", p, err, out)
		}
	}
}

// The snippet page shows an SSH clone URL when SSH is on, and none when off.
func TestSnippetPageSSHCloneURL(t *testing.T) {
	e := newSnipEnv(t)
	sn := e.create(t, snippetForm("zoe", "", "private", "hello.txt", "hello\n"))
	page := func() string {
		return do(t, e.a, "GET", "/snippets/"+sn.UID, nil, e.owner).Body.String()
	}
	if strings.Contains(page(), "ssh://git@") {
		t.Error("SSH clone URL shown while SSH is off")
	}
	e.a.cfg = withSSH(e.a.cfg)
	body := page()
	want := "ssh://git@example.com:2223/snippets/" + sn.UID + ".git"
	if !strings.Contains(body, want) {
		t.Errorf("snippet page lacks %q", want)
	}
}

func withSSH(c config.Config) config.Config {
	c.SSHListen = ":2223"
	c.PublicURL = "https://example.com"
	return c
}
