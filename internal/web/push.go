package web

// Web Push: notifications on the phone (and any browser that supports push).
//
//   push.go          feature, settings page, subscriptions, the sender
//   push_crypto.go   RFC 8291 payload encryption and RFC 8292 VAPID tokens
//   store/push.go    push_subscriptions and push_prefs
//   static/push.js   permission prompt → pushManager.subscribe → POST
//   static/sw.js     shows the push and opens its URL (same origin only)
//
// pwa.go hands every event that notifies someone to pushHook once per
// recipient (never the person who acted). pushQueue looks up that person's
// devices and preferences and sends in the background, so event handling is
// never held up by a push service. Each device gets up to pushAttempts tries;
// a 404 or 410 means the browser dropped the subscription and the row goes.
//
// The VAPID key pair lives in <data>/push/vapid.pem (0600), created on first
// use. <data>/push is a registered backup data dir, so a restore keeps every
// existing subscription working (a new key would orphan them all).
//
// Endpoints are untrusted input: only https URLs on the known push services
// are accepted, and requests go through the same SSRF-guarded dialer as
// webhooks (apps_deliver.go), with no proxy and no redirects.

import (
	"bytes"
	"context"
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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"atelier/internal/backup"
	"atelier/internal/store"
)

const (
	pushDir          = "push"
	pushMaxDevices   = 20
	pushMaxBody      = 8 << 10
	pushTimeout      = 10 * time.Second
	pushTTL          = 24 * 60 * 60 // seconds a push service keeps an undelivered message
	pushMaxRespRead  = 4 << 10
	pushMaxEndpoint  = 1024
	pushTitleRunes   = 120
	pushBodyRunes    = 240
	pushMaxInFlight  = 4
	pushAttemptsBase = 3
)

func init() {
	backup.RegisterDataDir(pushDir)
	registerFeature(feature{
		name: "push",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /settings/notifications", a.authed(a.pushSettingsPage))
			m.HandleFunc("POST /settings/notifications/subscribe", a.passkeyAPI(a.pushSubscribe))
			m.HandleFunc("POST /settings/notifications/remove", a.authed(a.pushRemove))
			m.HandleFunc("POST /settings/notifications/test", a.authed(a.pushTest))
			m.HandleFunc("POST /settings/notifications/preferences", a.authed(a.pushSavePrefs))
		},
	})
	pushHook = pushQueue
}

// ---------- kinds and preferences ----------

// pushKind is one toggle under Settings → Notifications.
type pushKind struct {
	Key, Name, Hint string
	Default         bool
}

// pushKinds: the high-signal kinds are on until the person says otherwise.
var pushKinds = []pushKind{
	{"approvals", "Approvals waiting", "An Actions job or agent is waiting for you to approve it.", true},
	{"failures", "Actions failures", "Failed workflow runs, runners going offline, failed backups and imports.", true},
	{"security", "Security alerts", "Secrets found in pushes and other security findings.", true},
	{"pulls", "Pull requests and reviews", "Pull requests opened, reviewed, commented on, merged or closed.", true},
	{"mentions", "Issues, mentions and assignments", "Issues opened, commented on or closed, and anything that names or assigns you.", false},
	{"releases", "Releases and packages", "New releases and published packages.", false},
	{"tether", "Tether and GitHub activity", "Comments and activity mirrored from GitHub.", false},
}

// pushCategory maps an event to its toggle ("" = never pushed: routine
// events such as successful runs or secret edits stay in the inbox).
func pushCategory(kind, reason string) string {
	switch reason {
	case "mention", "mentioned", "assign", "assigned":
		return "mentions"
	case "review_requested":
		return "pulls"
	}
	subject, verb, _ := strings.Cut(kind, ".")
	switch {
	case kind == EventJobWaiting || verb == "waiting" || strings.Contains(kind, "approval"):
		return "approvals"
	case kind == EventRunFailed || kind == EventRunnerOffline || verb == "failed":
		return "failures"
	case kind == EventSecretDetected || subject == "security" || subject == "vulnerability" || subject == "advisory":
		return "security"
	case subject == "pull" || subject == "review":
		return "pulls"
	case subject == "issue":
		return "mentions"
	case subject == "release" || subject == "package":
		return "releases"
	case subject == "tether" || subject == "github":
		return "tether"
	}
	return ""
}

// pushWants applies a person's stored choices over the defaults.
func pushWants(prefs map[string]bool, category string) bool {
	if on, ok := prefs[category]; ok {
		return on
	}
	for _, k := range pushKinds {
		if k.Key == category {
			return k.Default
		}
	}
	return false
}

func pushUrgency(category string) string {
	switch category {
	case "approvals", "failures", "security":
		return "high"
	}
	return "normal"
}

// ---------- state ----------

type pushState struct {
	mu   sync.Mutex
	keys *vapidKeys

	// guard holds the SSRF guard's lookup and dial hooks (tests replace them).
	guard *appsState
	// transport replaces the guarded transport in tests (a fake push service).
	transport http.RoundTripper
	// extraHosts lets tests accept another endpoint host.
	extraHosts func(host string) bool

	attempts int
	backoff  func(n int) time.Duration
	now      func() time.Time

	sem chan struct{}
	wg  sync.WaitGroup
}

var pushStates sync.Map // *App → *pushState

func (a *App) push() *pushState {
	if st, ok := pushStates.Load(a); ok {
		return st.(*pushState)
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	st := &pushState{
		guard: &appsState{
			lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			},
			dial: d.DialContext,
		},
		attempts: pushAttemptsBase,
		backoff:  func(n int) time.Duration { return time.Duration(n*n) * 5 * time.Second }, // 5s, 20s
		now:      time.Now,
		sem:      make(chan struct{}, pushMaxInFlight),
	}
	got, _ := pushStates.LoadOrStore(a, st)
	return got.(*pushState)
}

// vapid loads (or on first use creates) the VAPID key pair.
func (a *App) vapid() (*vapidKeys, error) {
	st := a.push()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.keys != nil {
		return st.keys, nil
	}
	k, err := loadOrCreateVAPID(filepath.Join(a.cfg.DataDir, pushDir))
	if err != nil {
		return nil, err
	}
	st.keys = k
	return k, nil
}

// vapidSubject is the contact in VAPID tokens: the public https URL when
// there is one (push services reject localhost contacts).
func (a *App) vapidSubject() string {
	if u := a.cfg.PublicURL; strings.HasPrefix(u, "https://") {
		return u
	}
	return "mailto:atelier@example.invalid"
}

// ---------- endpoints ----------

// pushServiceHost says whether host belongs to a known browser push service.
func pushServiceHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	switch host {
	case "fcm.googleapis.com", "updates.push.services.mozilla.com", "push.services.mozilla.com":
		return true
	}
	return strings.HasSuffix(host, ".push.apple.com") || strings.HasSuffix(host, ".notify.windows.com")
}

// pushCheckEndpoint validates a subscription endpoint: https, default port,
// no credentials, on a known push service.
func (st *pushState) checkEndpoint(raw string) (*url.URL, error) {
	if raw == "" || len(raw) > pushMaxEndpoint {
		return nil, errors.New("That push endpoint isn't valid.")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" || u.Opaque != "" {
		return nil, errors.New("That push endpoint isn't a plain https:// URL.")
	}
	if p := u.Port(); p != "" && p != "443" {
		return nil, errors.New("That push endpoint uses an unexpected port.")
	}
	host := u.Hostname()
	if _, err := netip.ParseAddr(host); err == nil {
		return nil, errors.New("That push endpoint isn't on a known push service.")
	}
	if !pushServiceHost(host) && (st.extraHosts == nil || !st.extraHosts(host)) {
		return nil, errors.New("That push endpoint isn't on a known push service (Apple, Google, Mozilla or Microsoft).")
	}
	return u, nil
}

// checkKeys validates the browser's p256dh (an uncompressed P-256 point) and
// auth secret (16 bytes).
func pushCheckKeys(p256dh, auth string) (ua, sec []byte, err error) {
	ua, err = pushB64(p256dh)
	if err != nil || len(ua) != 65 || ua[0] != 4 {
		return nil, nil, errors.New("The browser's push key isn't valid.")
	}
	sec, err = pushB64(auth)
	if err != nil || len(sec) != 16 {
		return nil, nil, errors.New("The browser's push secret isn't valid.")
	}
	if _, err := pushEncrypt([]byte("{}"), ua, sec); err != nil {
		return nil, nil, errors.New("The browser's push key isn't on the P-256 curve.")
	}
	return ua, sec, nil
}

func (st *pushState) client() *http.Client {
	tr := st.transport
	if tr == nil {
		tr = &http.Transport{
			Proxy:                  nil, // a proxy would bypass the guard
			DialContext:            st.guard.guardedDial(false),
			TLSHandshakeTimeout:    5 * time.Second,
			ResponseHeaderTimeout:  pushTimeout,
			DisableKeepAlives:      true,
			MaxResponseHeaderBytes: 32 << 10,
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   pushTimeout,
		// Push services answer directly; a redirect is never followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// ---------- sending ----------

// pushPayload is what static/sw.js receives. No secrets; kept short.
type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
	Badge *int   `json:"badge,omitempty"`
}

func pushRunes(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

func pushPayloadFor(m PushMessage, unread int) pushPayload {
	u := safeLocalURL(m.URL)
	if u == "" {
		u = "/notifications"
	}
	body := pushRunes(m.Body, pushBodyRunes)
	if m.RepoFullName != "" {
		if body != "" {
			body = m.RepoFullName + ": " + body
		} else {
			body = m.RepoFullName
		}
		body = pushRunes(body, pushBodyRunes)
	}
	subject, _, _ := strings.Cut(m.Kind, ".")
	tag := subject + ":" + u
	if len(tag) > 120 {
		sum := sha256.Sum256([]byte(tag))
		tag = subject + ":" + hex.EncodeToString(sum[:12])
	}
	p := pushPayload{Title: pushRunes(m.Title, pushTitleRunes), Body: body, URL: u, Tag: tag}
	if unread >= 0 {
		p.Badge = &unread
	}
	return p
}

// pushTopic is the Topic header: push services keep only the newest pending
// message per topic (≤ 32 URL-safe characters).
func pushTopic(tag string) string {
	sum := sha256.Sum256([]byte(tag))
	return b64url.EncodeToString(sum[:])[:32]
}

// pushQueue is pushHook: it returns at once and sends in the background.
func pushQueue(ctx context.Context, a *App, m PushMessage) {
	st := a.push()
	st.wg.Add(1)
	go func() {
		defer st.wg.Done()
		ctx := context.WithoutCancel(ctx)
		subs, err := a.db.PushSubscriptions(ctx, m.PersonID)
		if err != nil || len(subs) == 0 {
			return
		}
		category := pushCategory(m.Kind, m.Reason)
		if category == "" {
			return
		}
		prefs, err := a.db.PushPrefs(ctx, m.PersonID)
		if err != nil || !pushWants(prefs, category) {
			return
		}
		unread, err := a.db.UnreadNotificationCount(ctx, m.PersonID)
		if err != nil {
			unread = -1
		}
		payload := pushPayloadFor(m, unread)
		body, _ := json.Marshal(payload)
		topic := pushTopic(payload.Tag)
		for _, sub := range subs {
			st.wg.Add(1)
			go func(sub store.PushSubscription) {
				defer st.wg.Done()
				st.sem <- struct{}{}
				defer func() { <-st.sem }()
				_, _ = a.pushDeliver(ctx, sub, body, pushUrgency(category), topic, st.attempts)
			}(sub)
		}
	}()
}

// pushWait blocks until every queued push has finished (tests, shutdown).
func (a *App) pushWait() { a.push().wg.Wait() }

// errPushGone marks a subscription the push service no longer knows.
var errPushGone = errors.New("subscription expired")

// pushDeliver sends one payload to one device, retrying network errors, 429
// and 5xx. It updates the device's row and returns the last status.
func (a *App) pushDeliver(ctx context.Context, sub store.PushSubscription, payload []byte, urgency, topic string, attempts int) (int, error) {
	st := a.push()
	var status int
	var err error
	for n := 1; n <= attempts; n++ {
		if n > 1 {
			select {
			case <-ctx.Done():
				return status, ctx.Err()
			case <-time.After(st.backoff(n - 1)):
			}
		}
		var retry bool
		status, retry, err = a.pushOnce(ctx, sub, payload, urgency, topic)
		if err == nil || !retry {
			break
		}
	}
	switch {
	case err == nil:
		_ = a.db.PushDelivered(ctx, sub.ID, st.now())
	case errors.Is(err, errPushGone):
		_ = a.db.DeletePushSubscriptionByID(ctx, sub.ID)
		log.Printf("push: device %d of person %d unsubscribed (HTTP %d); removed", sub.ID, sub.PersonID, status)
	default:
		_ = a.db.PushFailed(ctx, sub.ID, err.Error())
		log.Printf("push: device %d of person %d: %v", sub.ID, sub.PersonID, err)
	}
	return status, err
}

// pushOnce makes one attempt. Errors never include the endpoint path, keys or
// the VAPID token.
func (a *App) pushOnce(ctx context.Context, sub store.PushSubscription, payload []byte, urgency, topic string) (status int, retry bool, err error) {
	st := a.push()
	u, err := st.checkEndpoint(sub.Endpoint)
	if err != nil {
		return 0, false, errors.New("endpoint no longer allowed")
	}
	ua, sec, err := pushCheckKeys(sub.P256dh, sub.Auth)
	if err != nil {
		return 0, false, errors.New("device keys are invalid")
	}
	keys, err := a.vapid()
	if err != nil {
		return 0, false, errors.New("VAPID keys unavailable")
	}
	body, err := pushEncrypt(payload, ua, sec)
	if err != nil {
		return 0, false, errors.New("could not encrypt the payload")
	}
	auth, err := keys.authorization("https://"+u.Host, a.vapidSubject(), st.now())
	if err != nil {
		return 0, false, errors.New("could not sign the VAPID token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, false, errors.New("bad endpoint")
	}
	h := req.Header
	h.Set("Authorization", auth)
	h.Set("Content-Encoding", "aes128gcm")
	h.Set("Content-Type", "application/octet-stream")
	h.Set("TTL", strconv.Itoa(pushTTL))
	h.Set("Urgency", urgency)
	if topic != "" {
		h.Set("Topic", topic)
	}
	resp, err := st.client().Do(req)
	if err != nil {
		if errors.Is(err, errAppsBlocked) {
			return 0, false, fmt.Errorf("%s refused: blocked address", u.Hostname())
		}
		return 0, true, fmt.Errorf("%s unreachable", u.Hostname())
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, pushMaxRespRead))
	switch s := resp.StatusCode; {
	case s >= 200 && s < 300:
		return s, false, nil
	case s == http.StatusNotFound || s == http.StatusGone:
		return s, false, errPushGone
	case s == http.StatusTooManyRequests || s >= 500:
		return s, true, fmt.Errorf("%s answered HTTP %d", u.Hostname(), s)
	default:
		return s, false, fmt.Errorf("%s answered HTTP %d", u.Hostname(), s)
	}
}

// ---------- settings page ----------

type pushDeviceView struct {
	ID           int64
	Label        string
	Hash         string
	CreatedAt    time.Time
	LastSuccess  *time.Time
	FailureCount int
	LastError    string
}

// pushEndpointHash identifies a device to static/push.js (which hashes its
// own endpoint the same way) without putting endpoints in the page.
func pushEndpointHash(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:16])
}

var pushMessages = map[string]string{
	"added":   "Notifications are on for this device.",
	"removed": "Device removed. It won't get notifications any more.",
	"saved":   "Notification preferences saved.",
	"sent":    "Test notification sent. It should appear in a few seconds.",
}

func (a *App) pushSettingsPage(w http.ResponseWriter, r *http.Request) {
	a.renderPushSettings(w, r, http.StatusOK, "")
}

func (a *App) renderPushSettings(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	ctx := r.Context()
	who := a.actor(r)
	subs, err := a.db.PushSubscriptions(ctx, who.PersonID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	prefs, err := a.db.PushPrefs(ctx, who.PersonID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	keys, err := a.vapid()
	if err != nil {
		a.fail(w, r, fmt.Errorf("push keys: %w", err))
		return
	}
	devices := make([]pushDeviceView, 0, len(subs))
	for _, s := range subs {
		devices = append(devices, pushDeviceView{ID: s.ID, Label: s.Label, Hash: pushEndpointHash(s.Endpoint), CreatedAt: s.CreatedAt,
			LastSuccess: s.LastSuccess, FailureCount: s.FailureCount, LastError: s.LastError})
	}
	type kindView struct {
		pushKind
		On bool
	}
	kinds := make([]kindView, 0, len(pushKinds))
	for _, k := range pushKinds {
		kinds = append(kinds, kindView{k, pushWants(prefs, k.Key)})
	}
	scheme, host := a.external(r)
	if a.cfg.PublicURL != "" {
		if u, err := url.Parse(a.cfg.PublicURL); err == nil && u.Host != "" {
			scheme, host = u.Scheme, u.Host
		}
	}
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	secure := scheme == "https" || h == "localhost" || strings.HasSuffix(h, ".localhost") || h == "127.0.0.1" || h == "::1"
	msg := pushMessages[r.URL.Query().Get("m")]
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	a.render(w, r, "push_settings", page{Title: "Notifications", Error: errMsg, Data: map[string]any{
		"PublicKey": keys.PublicKey(),
		"Devices":   devices,
		"Kinds":     kinds,
		"Secure":    secure,
		"Origin":    scheme + "://" + host,
		"Message":   msg,
		"Max":       pushMaxDevices,
	}})
}

// pushWantsJSON: the page's script asks for JSON; plain forms get redirects.
func pushWantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func pushJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// pushDeviceLabel names a device from its User-Agent ("iPhone · Home Screen app").
func pushDeviceLabel(ua string, standalone bool) string {
	device := "Browser"
	switch {
	case strings.Contains(ua, "iPhone"):
		device = "iPhone"
	case strings.Contains(ua, "iPad"):
		device = "iPad"
	case strings.Contains(ua, "Android"):
		device = "Android"
	case strings.Contains(ua, "Macintosh"):
		device = "Mac"
	case strings.Contains(ua, "Windows"):
		device = "Windows"
	case strings.Contains(ua, "CrOS"):
		device = "ChromeOS"
	case strings.Contains(ua, "Linux"):
		device = "Linux"
	}
	browser := ""
	switch {
	case standalone:
		browser = "Home Screen app"
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "Firefox/") || strings.Contains(ua, "FxiOS"):
		browser = "Firefox"
	case strings.Contains(ua, "Chrome/") || strings.Contains(ua, "CriOS"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	}
	if browser == "" {
		return device
	}
	return device + " · " + browser
}

func (a *App) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	who := a.actor(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, pushMaxBody+1))
	if err != nil || len(body) > pushMaxBody {
		pushJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "Request too large."})
		return
	}
	var in struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
		Standalone bool `json:"standalone"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		pushJSON(w, http.StatusBadRequest, map[string]string{"error": "That isn't a push subscription."})
		return
	}
	st := a.push()
	if _, err := st.checkEndpoint(in.Endpoint); err != nil {
		pushJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if _, _, err := pushCheckKeys(in.Keys.P256dh, in.Keys.Auth); err != nil {
		pushJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx := r.Context()
	n, err := a.db.CountPushSubscriptions(ctx, who.PersonID)
	if err != nil {
		pushJSON(w, http.StatusInternalServerError, map[string]string{"error": "Couldn't save the device."})
		return
	}
	existing := false
	if subs, err := a.db.PushSubscriptions(ctx, who.PersonID); err == nil {
		for _, s := range subs {
			existing = existing || s.Endpoint == in.Endpoint
		}
	}
	if n >= pushMaxDevices && !existing {
		pushJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("You already have %d devices. Remove one first.", pushMaxDevices)})
		return
	}
	label := pushDeviceLabel(r.UserAgent(), in.Standalone)
	id, err := a.db.SavePushSubscription(ctx, store.PushSubscription{PersonID: who.PersonID, Endpoint: in.Endpoint,
		P256dh: in.Keys.P256dh, Auth: in.Keys.Auth, Label: label})
	if err != nil {
		pushJSON(w, http.StatusInternalServerError, map[string]string{"error": "Couldn't save the device."})
		return
	}
	pushJSON(w, http.StatusOK, map[string]any{"id": id, "label": label, "redirect": "/settings/notifications?m=added"})
}

func pushFormID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	return id
}

func (a *App) pushRemove(w http.ResponseWriter, r *http.Request) {
	who := a.actor(r)
	err := a.db.DeletePushSubscription(r.Context(), who.PersonID, pushFormID(r))
	if pushWantsJSON(r) {
		if err != nil {
			pushJSON(w, http.StatusNotFound, map[string]string{"error": "That device isn't yours or is already gone."})
			return
		}
		pushJSON(w, http.StatusOK, map[string]string{"redirect": "/settings/notifications?m=removed"})
		return
	}
	if err != nil {
		a.renderPushSettings(w, r, http.StatusNotFound, "That device isn't yours or is already gone.")
		return
	}
	http.Redirect(w, r, "/settings/notifications?m=removed", http.StatusSeeOther)
}

// pushTest sends a test notification to one of the person's devices and
// waits for the push service's answer.
func (a *App) pushTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := a.actor(r)
	sub, err := a.db.PushSubscription(ctx, who.PersonID, pushFormID(r))
	fail := func(status int, msg string) {
		if pushWantsJSON(r) {
			pushJSON(w, status, map[string]string{"error": msg})
			return
		}
		a.renderPushSettings(w, r, status, msg)
	}
	if err != nil {
		fail(http.StatusNotFound, "That device isn't yours or is already gone.")
		return
	}
	unread, err := a.db.UnreadNotificationCount(ctx, who.PersonID)
	if err != nil {
		unread = -1
	}
	p := pushPayloadFor(PushMessage{PersonID: who.PersonID, Kind: "test.sent", Title: "Atelier test notification",
		Body: "Notifications work on " + sub.Label + ".", URL: "/settings/notifications"}, unread)
	body, _ := json.Marshal(p)
	status, err := a.pushDeliver(ctx, sub, body, "high", "", 1)
	switch {
	case errors.Is(err, errPushGone):
		fail(http.StatusGone, "The push service says this device unsubscribed, so it was removed. Turn notifications on again from that device.")
		return
	case err != nil:
		fail(http.StatusBadGateway, "The test didn't go through: "+err.Error()+".")
		return
	}
	if pushWantsJSON(r) {
		pushJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status, "message": pushMessages["sent"]})
		return
	}
	http.Redirect(w, r, "/settings/notifications?m=sent", http.StatusSeeOther)
}

func (a *App) pushSavePrefs(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.renderPushSettings(w, r, http.StatusBadRequest, "That form didn't arrive intact.")
		return
	}
	prefs := map[string]bool{}
	for _, k := range pushKinds {
		prefs[k.Key] = r.PostForm.Get("kind_"+k.Key) != ""
	}
	if err := a.db.SetPushPrefs(r.Context(), a.actor(r).PersonID, prefs); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/notifications?m=saved", http.StatusSeeOther)
}
