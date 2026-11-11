package web

// Remote pull requests: another studio offers a branch of one of its public
// repositories to one of yours (a ForgeFed-style Offer of a Ticket with a
// Branch attachment). This studio fetches the branch over HTTPS from the
// sender's public clone URL, through the same address checks as every other
// Connect request (the checked address is pinned for git with
// http.curloptResolve), into a quarantine ref (refs/connect/offers/<n>), then
// copies it to a branch under connect/<studio>/… and opens a normal pull
// request authored by "connect:@name@studio". Like any pull request not
// opened by you, its Actions wait for your approval.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"atelier/internal/connect"
	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

const connectFetchTimeout = 2 * time.Minute

// connectOfferSubmit offers one of your branches to a repository on another studio.
func (a *App) connectOfferSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	base := a.connectActive(ctx)
	if base == "" {
		a.connectFail(w, r, http.StatusConflict, connectErrText(errConnectOff))
		return
	}
	owner, name, _ := strings.Cut(r.FormValue("repo"), "/")
	rp, ok := a.connectPublicRepo(ctx, owner, name)
	if !ok {
		a.connectFail(w, r, http.StatusBadRequest, "Pick one of your public repositories.")
		return
	}
	branch := strings.TrimSpace(r.FormValue("branch"))
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if _, err := g.BranchSHA(ctx, branch); err != nil {
		a.connectFail(w, r, http.StatusBadRequest, "That branch doesn't exist in "+rp.FullName()+".")
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		title = branch
	}
	if err := a.connectOffer(ctx, base, rp, branch, r.FormValue("target"), connect.Clip(title, 200), connect.Clip(r.FormValue("body"), 8000)); err != nil {
		a.connectFail(w, r, http.StatusBadRequest, connectErrText(err))
		return
	}
	connectDone(w, r, "offer")
}

func (a *App) connectOffer(ctx context.Context, base string, rp store.Repo, branch, target, title, body string) error {
	them, err := a.connectResolve(ctx, base, target)
	if err != nil {
		return err
	}
	if them.Type != "Repository" {
		return errors.New("That address isn't a repository.")
	}
	me := connectActorURI(base, rp.Owner)
	myRepo := connectRepoURI(base, rp)
	ticket := map[string]any{
		"id": base + "/connect/offers/" + store.RandomToken(8), "type": "Ticket", "attributedTo": me, "context": them.URI,
		"name": title, "content": body, "mediaType": "text/markdown",
		"attachment": map[string]any{"type": "Branch", "name": branch, "ref": "refs/heads/" + branch, "context": myRepo},
	}
	act := connect.Activity(connectNewID(base), "Offer", me, ticket, []string{them.URI}, nil, time.Now())
	act["target"] = them.URI
	act["summary"] = "offered " + branch + " from " + rp.FullName()
	return a.connectQueue(ctx, act, []string{them.Inbox}, rp.ID)
}

// connectReceiveOffer accepts an offered branch for one of your public repositories.
func (a *App) connectReceiveOffer(ctx context.Context, base string, actor store.ConnectActor, doc connect.Doc) error {
	rp, ok := a.connectRepoFromURI(ctx, base, doc.ID("target"))
	if !ok {
		return inboxFail(http.StatusNotFound, "no such public repository here")
	}
	t := doc.Obj("object")
	if t == nil || t.Type() != "Ticket" {
		return inboxFail(http.StatusBadRequest, "an offer needs a Ticket")
	}
	att := t.Obj("attachment")
	if att == nil || att.Type() != "Branch" {
		return inboxFail(http.StatusBadRequest, "the ticket needs a Branch attachment")
	}
	branch := att.Str("name")
	src := att.ID("context")
	if !gitrepo.ValidBranchName(branch) || connect.HostOf(src) != actor.Host {
		return inboxFail(http.StatusBadRequest, "the branch must be on the sender's own studio")
	}
	title := connect.Clip(connect.PlainText(t.Str("name")), 200)
	if title == "" {
		title = branch
	}
	id, fresh, err := a.db.AddConnectOffer(ctx, store.ConnectOffer{ActivityID: doc.Str("id"), ActorURI: actor.URI, RepoID: rp.ID,
		Branch: branch, CloneURI: src, Title: title, State: "fetching"})
	if err != nil || !fresh {
		return err
	}
	body := connect.Clip(t.Str("content"), 8000)
	st := a.connect()
	st.bg.Add(1)
	go func() {
		defer st.bg.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), connectFetchTimeout+30*time.Second)
		defer cancel()
		pull, ref, err := a.connectOpenOffer(ctx, base, actor, rp, id, src, branch, title, body)
		if err != nil {
			log.Printf("connect: offer from %s to %s: %v", actor.Handle(), rp.FullName(), err)
			_ = a.db.FinishConnectOffer(ctx, id, 0, ref, "failed", err.Error())
			return
		}
		_ = a.db.FinishConnectOffer(ctx, id, pull.ID, ref, "opened", "")
	}()
	return nil
}

func (a *App) connectOpenOffer(ctx context.Context, base string, actor store.ConnectActor, rp store.Repo, offerID int64,
	srcURI, branch, title, body string) (store.Pull, string, error) {
	src, _, err := a.connectRemoteActor(ctx, base, srcURI, true)
	if err != nil {
		return store.Pull{}, "", err
	}
	if src.Type != "Repository" || src.CloneURI == "" || src.Host != actor.Host {
		return store.Pull{}, "", errors.New("the offered repository has no clone address on the sender's studio")
	}
	quarantine := "refs/connect/offers/" + strconv.FormatInt(offerID, 10)
	if err := a.connectFetch(ctx, rp, src.CloneURI, branch, quarantine); err != nil {
		return store.Pull{}, quarantine, err
	}
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		return store.Pull{}, quarantine, err
	}
	sha, err := g.ResolveCommit(ctx, quarantine)
	if err != nil {
		return store.Pull{}, quarantine, err
	}
	host := strings.NewReplacer(":", "-").Replace(actor.Host)
	head := "connect/" + host + "/" + strconv.FormatInt(offerID, 10) + "-" + branch
	if !gitrepo.ValidBranchName(head) {
		head = "connect/" + host + "/" + strconv.FormatInt(offerID, 10)
	}
	if err := a.connectGit(ctx, rp, "update-ref", "refs/heads/"+head, sha, ""); err != nil {
		return store.Pull{}, quarantine, err
	}
	who := "connect:" + actor.Handle()
	note := fmt.Sprintf("\n\n---\nOffered through Atelier Connect by **%s** from [%s](%s) (branch `%s`).",
		actor.Handle(), src.Label(), src.URL, branch)
	if src.URL == "" || !connect.SafeLink(src.URL) {
		note = fmt.Sprintf("\n\n---\nOffered through Atelier Connect by **%s** (branch `%s`).", actor.Handle(), branch)
	}
	p, err := a.db.CreatePull(ctx, store.NewPull{RepoID: rp.ID, Title: title, Body: strings.TrimSpace(body) + note,
		Head: head, Base: g.DefaultBranch(ctx), HeadSHA: sha, AuthorKind: "connect", AuthorName: who})
	if err != nil {
		return store.Pull{}, quarantine, err
	}
	a.emit(ctx, Event{Kind: EventPullOpened, Repo: rp, RepoID: rp.ID, Number: p.Number, Title: p.Title,
		URL: fmt.Sprintf("/%s/pulls/%d", rp.FullName(), p.Number), Actor: Actor{Kind: "connect", Name: who},
		Detail: eventExcerpt(body, 140)})
	return p, quarantine, nil
}

// connectFetch fetches one branch from a clone URL into a quarantine ref.
// The host is resolved and checked here, and git is pinned to the checked
// address, so a DNS answer can't steer it to a private one.
func (a *App) connectFetch(ctx context.Context, rp store.Repo, cloneURL, branch, ref string) error {
	st := a.connect()
	cl := &connect.Client{AllowLocal: st.allowLocal}
	u, err := cl.CheckURL(cloneURL)
	if err != nil {
		return err
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	var ip netip.Addr
	if parsed, err := netip.ParseAddr(host); err == nil {
		ip = parsed
	} else {
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return fmt.Errorf("couldn't resolve %s", host)
		}
		for _, x := range ips {
			if why := connect.BlockedAddr(x, st.allowLocal); why != "" {
				return fmt.Errorf("%w: %s resolves to %s", connect.ErrBlocked, host, why)
			}
		}
		ip = ips[0].Unmap()
	}
	if why := connect.BlockedAddr(ip, st.allowLocal); why != "" {
		return fmt.Errorf("%w: %s is %s", connect.ErrBlocked, host, why)
	}
	resolve := host + ":" + port + ":" + ip.String()
	if ip.Is6() {
		resolve = host + ":" + port + ":[" + ip.String() + "]"
	}
	allow := []string{"-c", "protocol.allow=never", "-c", "protocol.https.allow=always"}
	if st.allowLocal {
		allow = append(allow, "-c", "protocol.http.allow=always")
	}
	args := append(allow, "-c", "http.curloptResolve="+resolve, "-c", "http.followRedirects=false",
		"-c", "credential.helper=", "-c", "http.proxy=",
		"fetch", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--quiet",
		u.String(), "+refs/heads/"+branch+":"+ref)
	fctx, cancel := context.WithTimeout(ctx, connectFetchTimeout)
	defer cancel()
	return a.connectGitArgs(fctx, rp, args)
}

func (a *App) connectGit(ctx context.Context, rp store.Repo, args ...string) error {
	return a.connectGitArgs(ctx, rp, args)
}

func (a *App) connectGitArgs(ctx context.Context, rp store.Repo, args []string) error {
	cmd := exec.CommandContext(ctx, a.git.Git, args...)
	cmd.Dir = a.git.Path(rp.Owner, rp.Name)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "HTTPS_PROXY=", "HTTP_PROXY=", "ALL_PROXY=", "https_proxy=", "http_proxy=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %v: %s", args[len(args)-1], err, connect.Clip(strings.TrimSpace(stderr.String()), 300))
	}
	return nil
}
