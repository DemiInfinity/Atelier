// Package sshd is Atelier's git-over-SSH server. It speaks just enough SSH to
// run git-upload-pack (clone/fetch) and git-receive-pack (push) for keys the
// owner has added in Settings → SSH keys. There is no shell, no PTY and no
// port forwarding.
package sshd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// Server serves git over SSH.
type Server struct {
	db      *store.Store
	git     *gitrepo.Store
	config  *ssh.ServerConfig
	hostKey ssh.Signer
	Logf    func(format string, args ...any)

	// Allow decides whether the person behind an SSH key may read (write=false)
	// or push to (write=true) a repository. Atelier's web package sets it to its
	// shared permission check. When nil, every stored key has full access.
	Allow func(ctx context.Context, keyID int64, repo store.Repo, write bool) bool

	// Create makes a missing repository when someone pushes to it (push to
	// create). It returns an error when that isn't allowed. Nil disables it.
	Create func(ctx context.Context, keyID int64, owner, name string) (store.Repo, error)

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

const (
	extKeyID   = "atelier-key-id"
	extKeyName = "atelier-key-name"
)

// New creates a server whose ed25519 host key is kept at hostKeyPath
// (generated on first start).
func New(db *store.Store, git *gitrepo.Store, hostKeyPath string) (*Server, error) {
	signer, err := LoadOrCreateHostKey(hostKeyPath)
	if err != nil {
		return nil, err
	}
	s := &Server{db: db, git: git, hostKey: signer, Logf: log.Printf, conns: map[net.Conn]struct{}{}}
	s.config = &ssh.ServerConfig{
		PublicKeyCallback: s.authKey,
		MaxAuthTries:      6,
		ServerVersion:     "SSH-2.0-Atelier",
		BannerCallback:    nil,
	}
	s.config.AddHostKey(signer)
	return s, nil
}

// HostKey is the server's public host key.
func (s *Server) HostKey() ssh.PublicKey { return s.hostKey.PublicKey() }

// LoadOrCreateHostKey reads an OpenSSH-format private key, or generates and
// saves a new ed25519 key (mode 0600) when the file doesn't exist.
func LoadOrCreateHostKey(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("ssh host key %s: %w", path, err)
		}
		return signer, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "atelier host key")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) { // another process won the race
			return LoadOrCreateHostKey(path)
		}
		return nil, err
	}
	if err := pem.Encode(f, block); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}

// authKey accepts only public keys stored in ssh_keys. It may be called for
// key "queries" before a signature is checked, so it doesn't record use.
func (s *Server) authKey(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	k, err := s.db.SSHKeyByFingerprint(ctx, ssh.FingerprintSHA256(key))
	if err != nil {
		return nil, errors.New("unknown public key")
	}
	stored, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k.PublicKey))
	if err != nil || string(stored.Marshal()) != string(key.Marshal()) || k.Kind != "owner" {
		return nil, errors.New("unknown public key")
	}
	return &ssh.Permissions{Extensions: map[string]string{
		extKeyID:   strconv.FormatInt(k.ID, 10),
		extKeyName: k.Name,
	}}, nil
}

// ListenAndServe listens on addr and serves until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve accepts connections on ln until ctx is done, then closes the listener
// and every open connection and waits for their handlers to return.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	stop := context.AfterFunc(ctx, func() {
		ln.Close()
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
	})
	defer stop()
	defer s.wg.Wait()
	var delay time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				delay = min(max(delay*2, 5*time.Millisecond), time.Second)
				time.Sleep(delay)
				continue
			}
			return err
		}
		delay = 0
		s.mu.Lock()
		if ctx.Err() != nil {
			s.mu.Unlock()
			c.Close()
			return nil
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
				c.Close()
			}()
			s.handleConn(ctx, c)
		}()
	}
}

func (s *Server) handleConn(ctx context.Context, c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(30 * time.Second)) // handshake must finish promptly
	conn, chans, reqs, err := ssh.NewServerConn(c, s.config)
	if err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	defer conn.Close()

	keyID, _ := strconv.ParseInt(conn.Permissions.Extensions[extKeyID], 10, 64)
	_ = s.db.TouchSSHKey(context.WithoutCancel(ctx), keyID)

	go ssh.DiscardRequests(reqs) // refuses tcpip-forward and other global requests
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(ctx, conn, ch, creqs)
	}
}

// session state for one channel.
func (s *Server) handleSession(ctx context.Context, conn *ssh.ServerConn, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	keyName := conn.Permissions.Extensions[extKeyName]
	keyID, _ := strconv.ParseInt(conn.Permissions.Extensions[extKeyID], 10, 64)
	var gitProtocol string
	for req := range reqs {
		switch req.Type {
		case "env":
			var kv struct{ Name, Value string }
			if ssh.Unmarshal(req.Payload, &kv) == nil && kv.Name == "GIT_PROTOCOL" && validGitProtocol(kv.Value) {
				gitProtocol = kv.Value
				_ = req.Reply(true, nil)
				continue
			}
			_ = req.Reply(false, nil)
		case "exec":
			var p struct{ Command string }
			if err := ssh.Unmarshal(req.Payload, &p); err != nil {
				_ = req.Reply(false, nil)
				return
			}
			_ = req.Reply(true, nil)
			code := s.runGit(ctx, ch, p.Command, gitProtocol, keyID, keyName, conn.RemoteAddr())
			s.exit(ch, code)
			return
		case "shell":
			_ = req.Reply(true, nil)
			fmt.Fprintf(ch.Stderr(), "Hi! You've authenticated with the key %q, but Atelier does not provide shell access.\n", keyName)
			s.exit(ch, 1)
			return
		default: // pty-req, subsystem, x11-req, auth-agent-req…
			_ = req.Reply(false, nil)
		}
	}
}

func (s *Server) exit(ch ssh.Channel, code int) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
	_ = ch.CloseWrite()
}

var gitProtocolRe = regexp.MustCompile(`^[A-Za-z0-9=:.,_-]{1,64}$`)

func validGitProtocol(v string) bool { return gitProtocolRe.MatchString(v) }

// runGit runs one git service for the session and returns its exit code.
func (s *Server) runGit(ctx context.Context, ch ssh.Channel, command, gitProtocol string, keyID int64, keyName string, remote net.Addr) int {
	stderr := ch.Stderr()
	cmd, err := ParseCommand(command)
	if err != nil {
		fmt.Fprintf(stderr, "atelier: %v\n", err)
		return 128
	}
	repo, err := s.db.RepoByName(ctx, cmd.Owner, cmd.Repo)
	if err != nil && cmd.Service == ReceivePack && s.Create != nil {
		// Push to create: a push to a missing repository creates it (private).
		if created, cerr := s.Create(ctx, keyID, cmd.Owner, cmd.Repo); cerr == nil {
			repo, err = created, nil
			fmt.Fprintf(stderr, "atelier: created private repository %s\n", repo.FullName())
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "atelier: repository '%s/%s' not found\n", cmd.Owner, cmd.Repo)
		return 128
	}
	dir := s.git.Path(repo.Owner, repo.Name)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		fmt.Fprintf(stderr, "atelier: repository '%s' not found\n", repo.FullName())
		return 128
	}
	if s.Allow != nil {
		write := cmd.Service == ReceivePack
		if !s.Allow(ctx, keyID, repo, write) {
			if write && s.Allow(ctx, keyID, repo, false) {
				fmt.Fprintf(stderr, "atelier: you don't have permission to push to '%s'\n", repo.FullName())
			} else {
				// Don't reveal that a repository exists to someone who can't see it.
				fmt.Fprintf(stderr, "atelier: repository '%s/%s' not found\n", cmd.Owner, cmd.Repo)
			}
			return 128
		}
	}

	c := exec.CommandContext(ctx, s.git.Git, cmd.Service.subcommand(), dir)
	c.Dir = dir
	c.Env = append(os.Environ(), "REMOTE_USER=ssh:"+keyName, "REMOTE_ADDR="+hostOnly(remote))
	if gitProtocol != "" {
		c.Env = append(c.Env, "GIT_PROTOCOL="+gitProtocol)
	}
	c.Stdout = ch
	c.Stderr = stderr
	// A manual stdin pipe: exec would otherwise wait on a goroutine stuck
	// reading the channel after git has finished.
	stdin, err := c.StdinPipe()
	if err != nil {
		fmt.Fprintln(stderr, "atelier: internal error")
		return 1
	}
	if err := c.Start(); err != nil {
		s.Logf("ssh: start %s: %v", cmd.Service, err)
		fmt.Fprintln(stderr, "atelier: internal error")
		return 1
	}
	go func() {
		_, _ = io.Copy(stdin, ch)
		stdin.Close()
	}()
	err = c.Wait()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() >= 0 {
			code = ee.ExitCode()
		} else {
			code = 1
		}
	}
	if cmd.Service == ReceivePack {
		_ = s.db.TouchRepo(context.WithoutCancel(ctx), repo.ID)
	}
	s.Logf("ssh: %s %s by key %q from %s exited %d", cmd.Service, repo.FullName(), keyName, hostOnly(remote), code)
	return code
}

func hostOnly(a net.Addr) string {
	if a == nil {
		return ""
	}
	if h, _, err := net.SplitHostPort(a.String()); err == nil {
		return h
	}
	return a.String()
}

// ---------- command parsing ----------

type Service string

const (
	UploadPack  Service = "git-upload-pack"
	ReceivePack Service = "git-receive-pack"
)

func (s Service) subcommand() string { return strings.TrimPrefix(string(s), "git-") }

// Command is a parsed git SSH exec request.
type Command struct {
	Service Service
	Owner   string
	Repo    string // without .git
}

var commandRe = regexp.MustCompile(`^(git-upload-pack|git-receive-pack) ('?)/?([A-Za-z0-9][A-Za-z0-9._-]{0,99})/([A-Za-z0-9][A-Za-z0-9._-]{0,103})('?)$`)

// ParseCommand strictly parses "git-upload-pack 'owner/repo.git'" or
// "git-receive-pack '…'". The path may start with "/" and may omit ".git";
// quotes are optional but must be balanced. Anything else is refused.
func ParseCommand(command string) (Command, error) {
	m := commandRe.FindStringSubmatch(command)
	if m == nil || m[2] != m[5] {
		return Command{}, errors.New("only git clone, fetch and push are supported over SSH")
	}
	owner, repo := m[3], m[4]
	if len(repo) > 4 && strings.EqualFold(repo[len(repo)-4:], ".git") {
		repo = repo[:len(repo)-4]
	}
	if !gitrepo.ValidName(owner) || !gitrepo.ValidName(repo) {
		return Command{}, errors.New("invalid repository path")
	}
	return Command{Service: Service(m[1]), Owner: owner, Repo: repo}, nil
}
