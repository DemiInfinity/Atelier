package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"atelier/internal/store"
)

// Webhook delivery: a queue in the database (apps_deliveries) drained by the
// apps feature's start loop. Each attempt is signed, sent through a dialer
// that refuses private and metadata addresses (checked after DNS resolution,
// for every connection including redirects), time-limited and size-capped,
// and logged with secrets redacted. Failed attempts retry with backoff.

const (
	appsDeliverTimeout = 10 * time.Second
	appsMaxPayload     = 1 << 20  // bytes we're willing to send
	appsMaxRespRead    = 64 << 10 // bytes read from a receiver
	appsMaxRespStore   = 16 << 10 // bytes kept in the log
	appsMaxRedirects   = 3
	appsKeepDeliveries = 100 // per hook
	appsUserAgent      = "Atelier-Hookshot/1.0"
)

// appsState is the apps feature's per-App state. Tests replace the hooks.
type appsState struct {
	wake chan struct{}
	// lookup resolves a host name (net.DefaultResolver unless a test replaces it).
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// dial connects to an address that already passed the guard.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// backoff is the wait before retry number n (n ≥ 1).
	backoff     func(n int) time.Duration
	maxAttempts int
	now         func() time.Time

	refMu sync.Mutex     // one ref scan at a time
	bg    sync.WaitGroup // Tether calls in flight
}

var appsStates sync.Map // *App → *appsState

func (a *App) apps() *appsState {
	if st, ok := appsStates.Load(a); ok {
		return st.(*appsState)
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	st := &appsState{
		wake: make(chan struct{}, 1),
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dial: d.DialContext,
		backoff: func(n int) time.Duration {
			// 30s, 2m, 8m, 32m…
			d := 30 * time.Second
			for i := 1; i < n && d < 6*time.Hour; i++ {
				d *= 4
			}
			return d
		},
		maxAttempts: 5,
		now:         time.Now,
	}
	got, _ := appsStates.LoadOrStore(a, st)
	return got.(*appsState)
}

func (st *appsState) signal() {
	select {
	case st.wake <- struct{}{}:
	default:
	}
}

// ---------- SSRF guard ----------

var (
	appsMetadataAddrs = []netip.Addr{
		netip.MustParseAddr("169.254.169.254"), // AWS, GCP, Azure, DigitalOcean…
		netip.MustParseAddr("169.254.170.2"),   // ECS task metadata
		netip.MustParseAddr("100.100.100.200"), // Alibaba
		netip.MustParseAddr("fd00:ec2::254"),   // AWS IPv6
	}
	appsLocalPrefixes = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can reach IPv4 private space
	}
)

// errAppsBlocked marks a delivery the guard refused; it is never retried.
var errAppsBlocked = errors.New("blocked address")

// appsBlockedAddr says why an address may not receive webhooks ("" if it may).
// Metadata, unspecified, multicast and broadcast addresses are always refused;
// loopback, link-local and private ranges unless allowLocal is set.
func appsBlockedAddr(ip netip.Addr, allowLocal bool) string {
	ip = ip.Unmap()
	for _, m := range appsMetadataAddrs {
		if ip == m {
			return "a cloud metadata address"
		}
	}
	if ip.IsUnspecified() || ip.IsMulticast() || ip == netip.MustParseAddr("255.255.255.255") {
		return "not a unicast address"
	}
	if allowLocal {
		return ""
	}
	switch {
	case ip.IsLoopback():
		return "a loopback address"
	case ip.IsLinkLocalUnicast():
		return "a link-local address"
	case ip.IsPrivate():
		return "a private network address"
	}
	for _, p := range appsLocalPrefixes {
		if p.Contains(ip) {
			return "a reserved address"
		}
	}
	return ""
}

// appsGuardedDial resolves, checks every answer, then connects to the first:
// the address checked is the one dialled, so a second DNS answer can't
// rebind the request to a private address.
func (st *appsState) guardedDial(allowLocal bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var ips []netip.Addr
		if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
			ips = []netip.Addr{ip}
		} else {
			if ips, err = st.lookup(ctx, host); err != nil {
				return nil, err
			}
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("%s has no addresses", host)
		}
		for _, ip := range ips {
			if why := appsBlockedAddr(ip, allowLocal); why != "" {
				return nil, fmt.Errorf("%w: %s resolves to %s (%s)", errAppsBlocked, host, ip.Unmap(), why)
			}
		}
		return st.dial(ctx, network, net.JoinHostPort(ips[0].Unmap().String(), port))
	}
}

// appsCheckURL validates a webhook URL when it's saved. It checks literal
// addresses only; host names are checked on every delivery.
func appsCheckURL(raw string, allowLocal bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("Enter a full http:// or https:// URL.")
	}
	if len(raw) > 2048 {
		return nil, errors.New("That URL is too long.")
	}
	if u.User != nil {
		return nil, errors.New("Leave user names and passwords out of the URL; use the secret to authenticate deliveries.")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		if why := appsBlockedAddr(ip, allowLocal); why != "" {
			msg := "That URL points at " + why + "."
			if !allowLocal && appsBlockedAddr(ip, true) == "" {
				msg += " Tick \"Allow local network\" to send there."
			}
			return nil, errors.New(msg)
		}
	}
	if h := strings.ToLower(u.Hostname()); h == "localhost" || strings.HasSuffix(h, ".localhost") {
		if !allowLocal {
			return nil, errors.New("That URL points at this machine. Tick \"Allow local network\" to send there.")
		}
	}
	return u, nil
}

func (st *appsState) client(allowLocal bool) *http.Client {
	tr := &http.Transport{
		Proxy:                  nil, // a proxy would bypass the guard
		DialContext:            st.guardedDial(allowLocal),
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  appsDeliverTimeout,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 32 << 10,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   appsDeliverTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > appsMaxRedirects {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("%w: redirect to a %s URL", errAppsBlocked, req.URL.Scheme)
			}
			// The dialer checks the new host when it connects.
			return nil
		},
	}
}

// ---------- signing ----------

// appsSign is GitHub's X-Hub-Signature-256 value for body.
func appsSign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// AppsVerifySignature is how a receiver checks a delivery (constant time).
func AppsVerifySignature(secret string, body []byte, header string) bool {
	want := appsSign(secret, body)
	return hmac.Equal([]byte(want), []byte(strings.TrimSpace(header)))
}

func appsGUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// ---------- queueing ----------

// appsQueue queues event for every active hook on repo that wants it.
func (a *App) appsQueue(ctx context.Context, repo store.Repo, event, action string, payload map[string]any) {
	hooks, err := a.db.HooksFor(ctx, repo)
	if err != nil {
		log.Printf("apps: listing webhooks for %s: %v", repo.FullName(), err)
		return
	}
	queued := false
	for _, h := range hooks {
		if !h.Wants(event) || event == "ping" {
			continue
		}
		if err := a.appsQueueOne(ctx, h, event, action, payload, ""); err != nil {
			log.Printf("apps: queueing %s for hook %d: %v", event, h.ID, err)
			continue
		}
		queued = true
	}
	if queued {
		a.apps().signal()
	}
}

func (a *App) appsQueueOne(ctx context.Context, h store.Webhook, event, action string, payload map[string]any, guid string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if len(body) > appsMaxPayload {
		return fmt.Errorf("payload is %d bytes, over the %d byte limit", len(body), appsMaxPayload)
	}
	redelivery := guid != ""
	if guid == "" {
		guid = appsGUID()
	}
	_, err = a.db.QueueDelivery(ctx, store.Delivery{HookID: h.ID, GUID: guid, Event: event, Action: action,
		Redelivery: redelivery, Payload: string(body)})
	return err
}

// appsRedeliver queues a copy of a delivery (same GUID, like GitHub).
func (a *App) appsRedeliver(ctx context.Context, h store.Webhook, d store.Delivery) error {
	_, err := a.db.QueueDelivery(ctx, store.Delivery{HookID: h.ID, GUID: d.GUID, Event: d.Event, Action: d.Action,
		Redelivery: true, Payload: d.Payload})
	if err == nil {
		a.apps().signal()
	}
	return err
}

// ---------- the loop ----------

func (a *App) appsDeliveryLoop(ctx context.Context) {
	st := a.apps()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-st.wake:
		case <-prune.C:
			if err := a.db.PruneDeliveries(ctx, appsKeepDeliveries); err != nil {
				log.Printf("apps: pruning deliveries: %v", err)
			}
			continue
		}
		a.appsDeliverDue(ctx)
	}
}

// appsDeliverDue sends every delivery that's due, four at a time, and
// returns when they're done.
func (a *App) appsDeliverDue(ctx context.Context) int {
	st := a.apps()
	total := 0
	for i := 0; i < 10 && ctx.Err() == nil; i++ {
		due, err := a.db.DueDeliveries(ctx, st.now(), 16, 2*appsDeliverTimeout*time.Duration(appsMaxRedirects+1))
		if err != nil {
			log.Printf("apps: reading delivery queue: %v", err)
			return total
		}
		if len(due) == 0 {
			return total
		}
		sem := make(chan struct{}, 4)
		var wg sync.WaitGroup
		for _, d := range due {
			wg.Add(1)
			sem <- struct{}{}
			go func(d store.Delivery) {
				defer wg.Done()
				defer func() { <-sem }()
				a.appsAttempt(ctx, d)
			}(d)
		}
		wg.Wait()
		total += len(due)
	}
	return total
}

// appsAttempt makes one delivery attempt and records it.
func (a *App) appsAttempt(ctx context.Context, d store.Delivery) {
	st := a.apps()
	h, err := a.db.WebhookByID(ctx, d.HookID)
	if err != nil {
		return // the hook was deleted (its deliveries go with it)
	}
	res := store.DeliveryResult{Status: "failed"}
	retry := false
	defer func() {
		if retry && d.Attempts+1 < st.maxAttempts && h.Active {
			res.Status = "pending"
			res.NextAt = st.now().Add(st.backoff(d.Attempts + 1))
		}
		if err := a.db.FinishAttempt(context.WithoutCancel(ctx), d, res); err != nil {
			log.Printf("apps: recording delivery %d: %v", d.ID, err)
		}
	}()

	secret, err := a.db.WebhookSecret(ctx, a.secretSealer(), h.ID)
	if err != nil {
		res.Error = "The webhook secret can't be decrypted (was the secret key changed?). Save the secret again."
		return
	}
	if _, err := appsCheckURL(h.URL, h.AllowLocal); err != nil {
		res.Error = err.Error()
		return
	}
	body := []byte(d.Payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		res.Error = "Bad URL: " + err.Error()
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", appsUserAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-GitHub-Event", d.Event)
	req.Header.Set("X-Atelier-Event", d.Event)
	req.Header.Set("X-GitHub-Delivery", d.GUID)
	req.Header.Set("X-Atelier-Delivery", d.GUID)
	req.Header.Set("X-GitHub-Hook-ID", fmt.Sprint(h.ID))
	req.Header.Set("X-Atelier-Hook-ID", fmt.Sprint(h.ID))
	if h.RepoID != 0 {
		req.Header.Set("X-GitHub-Hook-Installation-Target-Type", "repository")
		req.Header.Set("X-GitHub-Hook-Installation-Target-ID", fmt.Sprint(h.RepoID))
	} else {
		req.Header.Set("X-GitHub-Hook-Installation-Target-Type", "organization")
		req.Header.Set("X-GitHub-Hook-Installation-Target-ID", fmt.Sprint(h.OwnerID))
	}
	if secret != "" {
		sig := appsSign(secret, body)
		req.Header.Set("X-Hub-Signature-256", sig)
		req.Header.Set("X-Atelier-Signature-256", sig)
	}
	res.ReqHeaders = appsHeaderJSON(req.Header)

	start := time.Now()
	resp, err := st.client(h.AllowLocal).Do(req)
	res.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = appsErrText(err)
		retry = !errors.Is(err, errAppsBlocked)
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, appsMaxRespRead))
	if len(data) > appsMaxRespStore {
		data = append(data[:appsMaxRespStore], []byte("\n… (truncated)")...)
	}
	res.RespStatus = resp.StatusCode
	res.RespHeaders = appsHeaderJSON(resp.Header)
	res.RespBody = strings.ToValidUTF8(string(data), "�")
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		res.Status = "ok"
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		res.Error = "The receiver answered with a redirect Atelier didn't follow."
	default:
		res.Error = fmt.Sprintf("The receiver answered %d.", resp.StatusCode)
		retry = resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout
	}
}

func appsErrText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	s := err.Error()
	if errors.Is(err, errAppsBlocked) {
		return "Refused: " + s + ". Tick \"Allow local network\" if this receiver is on your own network."
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(s, "Client.Timeout") {
		return fmt.Sprintf("Timed out after %s.", appsDeliverTimeout)
	}
	return s
}

// appsRedactedHeaders never show their values in the delivery log.
var appsRedactedHeaders = map[string]bool{
	"Authorization": true, "Proxy-Authorization": true, "Cookie": true, "Set-Cookie": true,
	"X-Hub-Signature-256": true, "X-Atelier-Signature-256": true, "X-Hub-Signature": true,
}

func appsHeaderJSON(h http.Header) string {
	m := map[string]string{}
	for k, vs := range h {
		k = http.CanonicalHeaderKey(k)
		v := strings.Join(vs, ", ")
		if appsRedactedHeaders[k] {
			v = "[redacted]"
		}
		if len(v) > 1024 {
			v = v[:1024] + "…"
		}
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

type appsHeader struct{ Name, Value string }

func appsHeaderList(js string) []appsHeader {
	var m map[string]string
	if json.Unmarshal([]byte(js), &m) != nil {
		return nil
	}
	var out []appsHeader
	for k, v := range m {
		out = append(out, appsHeader{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// appsPretty indents a JSON payload for the delivery page.
func appsPretty(s string) string {
	var buf bytes.Buffer
	if json.Indent(&buf, []byte(s), "", "  ") != nil {
		return s
	}
	return buf.String()
}
