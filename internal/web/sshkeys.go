package web

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"atelier/internal/sshd"
	"atelier/internal/store"
)

// SSH: the git SSH server (started in the background) and Settings → SSH keys.
func init() {
	registerFeature(feature{
		name: "ssh",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /settings/keys", a.authed(a.sshKeysPage))
			m.HandleFunc("POST /settings/keys", a.authed(a.sshKeysAdd))
			// Literal paths only (id in the form): a wildcard here could clash with
			// repository routes like /{owner}/{repo}/x/{id}.
			m.HandleFunc("POST /settings/keys/delete", a.authed(a.sshKeysDelete))
			m.HandleFunc("GET /settings/keys/clone.json", a.authed(a.sshCloneInfo))
		},
		start: func(ctx context.Context, a *App) {
			if !a.cfg.SSHEnabled() {
				return
			}
			srv, err := sshd.New(a.db, a.git, a.cfg.SSHHostKeyPath())
			if err != nil {
				log.Printf("ssh: disabled: %v", err)
				return
			}
			srv.Allow = a.sshAllow
			srv.Create = a.sshPushCreate
			srv.Snippet = a.sshSnippet // snippets_git.go
			log.Printf("ssh: git over SSH listening on %s (clone port %d, host key %s)",
				a.cfg.SSHListen, a.cfg.SSHPort(), ssh.FingerprintSHA256(srv.HostKey()))
			if err := srv.ListenAndServe(ctx, a.cfg.SSHListen); err != nil {
				log.Printf("ssh: stopped: %v", err)
			}
		},
	})
}

// sshHost is the host name people use for SSH: the public URL's host, else
// the host the browser used, without any port.
func (a *App) sshHost(r *http.Request) string {
	host := ""
	if a.cfg.PublicURL != "" {
		if u, err := url.Parse(a.cfg.PublicURL); err == nil {
			host = u.Host
		}
	}
	if host == "" {
		_, host = a.external(r)
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.Trim(host, "[]")
}

// sshCloneBase is "ssh://git@host:port", or "" when SSH is off.
func (a *App) sshCloneBase(r *http.Request) string {
	if !a.cfg.SSHEnabled() {
		return ""
	}
	host := a.sshHost(r)
	if strings.Contains(host, ":") { // IPv6 literal
		host = "[" + host + "]"
	}
	return "ssh://git@" + host + ":" + strconv.Itoa(a.cfg.SSHPort())
}

// hostKeyFingerprint reads the persisted host key, if it exists yet.
func (a *App) hostKeyFingerprint() string {
	data, err := os.ReadFile(a.cfg.SSHHostKeyPath())
	if err != nil {
		return ""
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(signer.PublicKey())
}

func (a *App) sshKeysPage(w http.ResponseWriter, r *http.Request) {
	a.renderSSHKeys(w, r, http.StatusOK, "", r.URL.Query().Get("saved") != "", url.Values{})
}

func (a *App) renderSSHKeys(w http.ResponseWriter, r *http.Request, status int, errMsg string, saved bool, form url.Values) {
	keys, err := a.db.SSHKeys(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	notice := ""
	if saved {
		notice = "saved"
	}
	me, _ := a.db.PersonalOwner(r.Context())
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	a.render(w, r, "sshkeys_settings", page{Title: "SSH keys", Error: errMsg, Notice: notice, Data: map[string]any{
		"Keys":      keys,
		"Enabled":   a.cfg.SSHEnabled(),
		"Host":      a.sshHost(r),
		"Port":      a.cfg.SSHPort(),
		"CloneBase": a.sshCloneBase(r),
		"HostKey":   a.hostKeyFingerprint(),
		"User":      me.Name,
		"Name":      form.Get("name"),
		"Key":       form.Get("key"),
	}})
}

func (a *App) sshKeysAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.renderSSHKeys(w, r, http.StatusBadRequest, "That form couldn't be read.", false, url.Values{})
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	pk, err := sshd.ParsePublicKey(r.PostForm.Get("key"))
	if err != nil {
		a.renderSSHKeys(w, r, http.StatusUnprocessableEntity, sshCapitalise(err.Error())+".", false, r.PostForm)
		return
	}
	if name == "" {
		name = pk.Comment
	}
	if name == "" {
		name = "SSH key"
	}
	if len(name) > 80 {
		name = name[:80]
	}
	act := a.actor(r)
	if _, err := a.db.AddSSHKey(r.Context(), name, pk.Type, pk.Fingerprint, pk.Normalized, act.Name, act.Kind); err != nil {
		if errors.Is(err, store.ErrKeyExists) {
			a.renderSSHKeys(w, r, http.StatusConflict, "That key has already been added.", false, r.PostForm)
			return
		}
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/keys?saved=1", http.StatusSeeOther)
}

func (a *App) sshKeysDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	if err := a.db.DeleteSSHKey(r.Context(), id); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/keys?saved=1", http.StatusSeeOther)
}

// sshCloneInfo tells the clone box (static/sshkeys.js) how to build SSH URLs.
func (a *App) sshCloneInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"enabled": a.cfg.SSHEnabled(),
		"base":    a.sshCloneBase(r),
	})
}

func sshCapitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// sshAllow routes SSH access through the same permission check as the web and
// git over HTTPS. An SSH key acts as the person it belongs to.
func (a *App) sshAllow(ctx context.Context, keyID int64, repo store.Repo, write bool) bool {
	personID, err := a.db.SSHKeyPerson(ctx, keyID)
	if err != nil {
		return false
	}
	p, err := a.db.Person(ctx, personID)
	if err != nil {
		return false
	}
	need := PermRead
	if write {
		need = PermPush
	}
	return a.can(ctx, Actor{Kind: ActorOwner, Name: p.Name, PersonID: p.ID}, repo, need)
}
