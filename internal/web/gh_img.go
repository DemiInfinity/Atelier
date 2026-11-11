package web

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"atelier/internal/tether"
)

// The image proxy: /gh/-/img?u=<https URL>. The app's CSP only allows images
// from Atelier itself (and GitHub avatars), so README and comment images are
// fetched here and served from Atelier's origin.
//
//   - Only https:// URLs on GitHub's image hosts (ghImageHosts) are fetched;
//     github.com only for /<owner>/<repo>/raw/… (rewritten to the raw host).
//   - Host names are resolved and every address is checked: private,
//     loopback, link-local and metadata addresses are refused (the same guard
//     as webhook delivery). Redirects may only stay on the same host.
//   - The GitHub token is never sent to an image host. Files of a private
//     repository the person can read are fetched through the API instead
//     (the token only ever goes to the API).
//   - At most ghImgMax bytes; only image types. SVG is served as an
//     attachment with a sandbox CSP, so it can't run anything even if opened
//     directly. Answers are cached briefly, per person.

const (
	ghImgMax      = 5 << 20
	ghImgTTL      = 10 * time.Minute
	ghImgCacheMax = 64 << 20
	ghImgTimeout  = 15 * time.Second
)

type ghImgEntry struct {
	body []byte
	ct   string
	at   time.Time
}

type ghImgState struct {
	// lookup and dial are net.DefaultResolver and a net.Dialer; tests replace them.
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
	tls    *tls.Config // nil = the system's roots

	mu    sync.Mutex
	cache map[string]ghImgEntry
	size  int
}

func newGHImgState() *ghImgState {
	d := &net.Dialer{Timeout: 5 * time.Second}
	return &ghImgState{
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dial:  d.DialContext,
		cache: map[string]ghImgEntry{},
	}
}

var errGHImgBlocked = errors.New("blocked")

// guardedDial resolves, refuses non-public addresses, then dials the address it checked.
func (st *ghImgState) guardedDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		ips = []netip.Addr{ip}
	} else if ips, err = st.lookup(ctx, host); err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%s has no addresses", host)
	}
	for _, ip := range ips {
		if why := appsBlockedAddr(ip, false); why != "" {
			return nil, fmt.Errorf("%w: %s resolves to %s", errGHImgBlocked, host, why)
		}
	}
	return st.dial(ctx, network, net.JoinHostPort(ips[0].Unmap().String(), port))
}

func (st *ghImgState) client() *http.Client {
	tr := &http.Transport{
		Proxy:                  nil, // a proxy would bypass the guard
		DialContext:            st.guardedDial,
		TLSClientConfig:        st.tls,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  ghImgTimeout,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 32 << 10,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   ghImgTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 3 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
				return fmt.Errorf("%w: redirect to another host", errGHImgBlocked)
			}
			req.Header.Del("Authorization")
			return nil
		},
	}
}

func (st *ghImgState) get(key string) (ghImgEntry, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	e, ok := st.cache[key]
	if ok && time.Since(e.at) > ghImgTTL {
		delete(st.cache, key)
		st.size -= len(e.body)
		return e, false
	}
	return e, ok
}

func (st *ghImgState) put(key string, e ghImgEntry) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if old, ok := st.cache[key]; ok {
		st.size -= len(old.body)
	}
	for st.size+len(e.body) > ghImgCacheMax && len(st.cache) > 0 {
		var oldest string
		var at time.Time
		for k, v := range st.cache {
			if oldest == "" || v.at.Before(at) {
				oldest, at = k, v.at
			}
		}
		st.size -= len(st.cache[oldest].body)
		delete(st.cache, oldest)
	}
	st.cache[key] = e
	st.size += len(e.body)
}

// ghImgTarget is a vetted image URL.
type ghImgTarget struct {
	URL *url.URL
	// For raw.githubusercontent.com: the repository and "<ref>/<path>".
	Repo, RefPath string
}

// ghImageTarget checks an image URL against the allow-list.
func ghImageTarget(raw string) (ghImgTarget, error) {
	var t ghImgTarget
	if len(raw) > 2048 {
		return t, errors.New("too long")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" {
		return t, errors.New("only https URLs")
	}
	if p := u.Port(); p != "" && p != "443" {
		return t, errors.New("only the standard port")
	}
	host := strings.ToLower(u.Hostname())
	if !ghImageHosts[host] {
		return t, errors.New("not a GitHub image host")
	}
	u.Host = host
	u.Fragment = ""
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch host {
	case "github.com":
		// /<owner>/<repo>/raw/<ref>/<path…> is a raw file.
		if len(segs) < 5 || segs[2] != "raw" || !tether.ValidRepo(segs[0]+"/"+segs[1]) {
			return t, errors.New("only raw files from github.com")
		}
		u = &url.URL{Scheme: "https", Host: ghRawHost, Path: "/" + segs[0] + "/" + segs[1] + "/" + strings.Join(segs[3:], "/")}
		segs = append(segs[:2:2], segs[3:]...)
		fallthrough
	case ghRawHost:
		if len(segs) < 4 || !tether.ValidRepo(segs[0]+"/"+segs[1]) {
			return t, errors.New("not a raw file")
		}
		for _, s := range segs[2:] {
			if s == "" || s == "." || s == ".." {
				return t, errors.New("bad path")
			}
		}
		t.Repo, t.RefPath = segs[0]+"/"+segs[1], strings.Join(segs[2:], "/")
		u.RawQuery = "" // tokens in raw URLs are never forwarded
	}
	t.URL = u
	return t, nil
}

// ghImgTypes are the image types served (SVG is handled separately).
var ghImgTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/avif": true,
	"image/bmp": true, "image/x-icon": true, "image/vnd.microsoft.icon": true,
}

// ghImgType decides what an image is from its declared type, its name and
// its bytes ("" = refuse).
func ghImgType(declared, name string, body []byte) string {
	mt, _, _ := mime.ParseMediaType(declared)
	mt = strings.ToLower(mt)
	sniffed, _, _ := mime.ParseMediaType(http.DetectContentType(body))
	looksSVG := func() bool {
		head := bytes.ToLower(body[:min(len(body), 4096)])
		return bytes.Contains(head, []byte("<svg"))
	}
	switch {
	case mt == "image/svg+xml" || (mt == "" || mt == "application/octet-stream" || mt == "text/plain") && strings.HasSuffix(strings.ToLower(name), ".svg"):
		if looksSVG() {
			return "image/svg+xml"
		}
		return ""
	case ghImgTypes[sniffed] && (mt == "" || mt == "application/octet-stream" || ghImgTypes[mt]):
		return sniffed
	case mt == "image/avif" && sniffed == "application/octet-stream":
		return "image/avif"
	}
	return ""
}

// ghImage serves one proxied image.
func (a *App) ghImage(w http.ResponseWriter, r *http.Request, g *ghReq) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	t, err := ghImageTarget(r.URL.Query().Get("u"))
	if err != nil {
		http.Error(w, "image refused: "+err.Error(), http.StatusBadRequest)
		return
	}
	st := a.gh().img
	key := fmt.Sprintf("%d\x00%s", g.who.PersonID, t.URL.String())
	e, ok := st.get(key)
	if !ok {
		e, err = a.ghFetchImage(r.Context(), g, t)
		if err != nil {
			http.Error(w, "image unavailable: "+err.Error(), http.StatusBadGateway)
			return
		}
		e.at = time.Now()
		st.put(key, e)
	}
	h.Set("Content-Type", e.ct)
	h.Set("Cache-Control", "private, max-age=600")
	if e.ct == "image/svg+xml" {
		h.Set("Content-Disposition", `attachment; filename="image.svg"`)
	}
	_, _ = w.Write(e.body)
}

func (a *App) ghFetchImage(ctx context.Context, g *ghReq, t ghImgTarget) (ghImgEntry, error) {
	// A private repository's file: through the API with the person's token.
	if t.Repo != "" {
		if rp, err := g.c.Repo(ctx, t.Repo); err == nil && rp.Private {
			return a.ghFetchPrivateImage(ctx, g, t)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, ghImgTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL.String(), nil)
	if err != nil {
		return ghImgEntry{}, errors.New("bad URL")
	}
	req.Header.Set("User-Agent", "Atelier-Image-Proxy")
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/svg+xml,image/*;q=0.8")
	resp, err := a.gh().img.client().Do(req)
	if err != nil {
		if errors.Is(err, errGHImgBlocked) {
			return ghImgEntry{}, errors.New("that address isn't allowed")
		}
		return ghImgEntry{}, errors.New("couldn't fetch it")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ghImgEntry{}, fmt.Errorf("the image host answered %d", resp.StatusCode)
	}
	if resp.ContentLength > ghImgMax {
		return ghImgEntry{}, errors.New("too large")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, ghImgMax+1))
	if err != nil {
		return ghImgEntry{}, errors.New("couldn't read it")
	}
	if len(body) > ghImgMax {
		return ghImgEntry{}, errors.New("too large")
	}
	ct := ghImgType(resp.Header.Get("Content-Type"), t.URL.Path, body)
	if ct == "" {
		return ghImgEntry{}, errors.New("not an image")
	}
	return ghImgEntry{body: body, ct: ct}, nil
}

func (a *App) ghFetchPrivateImage(ctx context.Context, g *ghReq, t ghImgTarget) (ghImgEntry, error) {
	ref, p := a.ghSplitRef(ctx, g, t.Repo, t.RefPath)
	if p == "" {
		return ghImgEntry{}, errors.New("not a file")
	}
	_, f, err := g.c.Contents(ctx, t.Repo, ref, p)
	if err != nil || f == nil || f.Type != "file" {
		return ghImgEntry{}, errors.New("couldn't fetch it")
	}
	body, ok := f.Decoded()
	if !ok || len(body) > ghImgMax {
		return ghImgEntry{}, errors.New("too large")
	}
	ct := ghImgType("", p, body)
	if ct == "" {
		return ghImgEntry{}, errors.New("not an image")
	}
	return ghImgEntry{body: body, ct: ct}, nil
}
