package connect

// The only way Connect reaches other studios. Every connection is checked
// after DNS resolution (the address checked is the address dialled, so a
// second answer can't rebind it), redirects are re-checked, everything is
// HTTPS unless AllowLocal is set (tests and two studios on one machine), and
// responses are time- and size-limited.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Client fetches and delivers for one studio.
type Client struct {
	// AllowLocal permits http:// and loopback/private addresses. Never set it
	// in production: it's for tests and local development.
	AllowLocal bool
	Key        *Key
	KeyID      string // the key's id, used to sign GETs too (authorized fetch)
	UserAgent  string
	Timeout    time.Duration
	// Lookup and Dial can be replaced in tests.
	Lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	Dial   func(ctx context.Context, network, addr string) (net.Conn, error)
	Now    func() time.Time
}

// ErrBlocked marks a request to an address Connect may not reach.
var ErrBlocked = errors.New("blocked address")

var (
	metadataAddrs = []netip.Addr{
		netip.MustParseAddr("169.254.169.254"),
		netip.MustParseAddr("169.254.170.2"),
		netip.MustParseAddr("100.100.100.200"),
		netip.MustParseAddr("fd00:ec2::254"),
	}
	reservedPrefixes = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("64:ff9b::/96"),
	}
)

// BlockedAddr says why ip may not be reached ("" if it may).
func BlockedAddr(ip netip.Addr, allowLocal bool) string {
	ip = ip.Unmap()
	for _, m := range metadataAddrs {
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
	for _, p := range reservedPrefixes {
		if p.Contains(ip) {
			return "a reserved address"
		}
	}
	return ""
}

// CheckURL validates a URL Connect is about to use.
func (c *Client) CheckURL(raw string) (*url.URL, error) {
	if len(raw) > 2048 {
		return nil, errors.New("URL too long")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil {
		return nil, errors.New("not a usable URL")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && c.AllowLocal:
	default:
		return nil, errors.New("other studios must be reached over HTTPS")
	}
	h := strings.ToLower(u.Hostname())
	if !c.AllowLocal && (h == "localhost" || strings.HasSuffix(h, ".localhost")) {
		return nil, fmt.Errorf("%w: %s is this machine", ErrBlocked, h)
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		if why := BlockedAddr(ip, c.AllowLocal); why != "" {
			return nil, fmt.Errorf("%w: %s is %s", ErrBlocked, ip, why)
		}
	}
	return u, nil
}

func (c *Client) dialer() func(ctx context.Context, network, addr string) (net.Conn, error) {
	lookup := c.Lookup
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	dial := c.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: 5 * time.Second}
		dial = d.DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var ips []netip.Addr
		if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
			ips = []netip.Addr{ip}
		} else if ips, err = lookup(ctx, host); err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("%s has no addresses", host)
		}
		for _, ip := range ips {
			if why := BlockedAddr(ip, c.AllowLocal); why != "" {
				return nil, fmt.Errorf("%w: %s resolves to %s (%s)", ErrBlocked, host, ip.Unmap(), why)
			}
		}
		return dial(ctx, network, net.JoinHostPort(ips[0].Unmap().String(), port))
	}
}

// HTTP is the guarded http.Client.
func (c *Client) HTTP() *http.Client {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	tr := &http.Transport{
		Proxy:                 nil, // never through a proxy: the guard must see the real address
		DialContext:           c.dialer(),
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true, // a new client per call: don't leave idle connections behind
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if _, err := c.CheckURL(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// StatusError is a non-2xx answer.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Code, e.Body)
	}
	return fmt.Sprintf("HTTP %d", e.Code)
}

// Get fetches a JSON document (ActivityPub or JRD) and decodes it strictly.
// GETs are signed when the client has a key, as servers with authorized
// fetch require.
func (c *Client) Get(ctx context.Context, raw, accept string) (Doc, error) {
	u, err := c.CheckURL(raw)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", c.UserAgent)
	if c.Key != nil && c.KeyID != "" {
		if err := SignRequest(req, c.KeyID, c.Key, nil, c.now()); err != nil {
			return nil, err
		}
	}
	resp, err := c.HTTP().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &StatusError{Code: resp.StatusCode, Body: Clip(PlainText(string(body)), 200)}
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "json") {
		return nil, fmt.Errorf("expected JSON, got %q", Clip(ct, 60))
	}
	return DecodeJSON(body, MaxBody)
}

// Post delivers a signed activity to an inbox. It returns the HTTP status.
func (c *Client) Post(ctx context.Context, inbox string, body []byte) (int, error) {
	u, err := c.CheckURL(inbox)
	if err != nil {
		return 0, err
	}
	if c.Key == nil || c.KeyID == "" {
		return 0, errors.New("no signing key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", ContentType)
	req.Header.Set("Accept", ContentType)
	req.Header.Set("User-Agent", c.UserAgent)
	if err := SignRequest(req, c.KeyID, c.Key, body, c.now()); err != nil {
		return 0, err
	}
	// Signed requests aren't replayed across redirects.
	hc := c.HTTP()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, &StatusError{Code: resp.StatusCode, Body: Clip(PlainText(string(msg)), 200)}
	}
	return resp.StatusCode, nil
}
