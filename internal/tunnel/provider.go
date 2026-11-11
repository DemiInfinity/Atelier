// Package tunnel runs the helper process behind Atelier's remote access (a
// "tunnel"): an outbound connection to a relay that gives the home server a
// public HTTPS address without port forwarding.
//
// The web package decides whether the tunnel is on (the setting lives in the
// database and defaults off) and serves the requests that come through it on
// a dedicated loopback listener. This package only knows how to start, watch
// and kill the helper.
//
// # Providers
//
// A Provider turns the owner's settings into a command line and reads the
// helper's output. Cloudflare Tunnel (cloudflared) is the only provider today,
// in two modes:
//
//   - quick: `cloudflared tunnel --url http://127.0.0.1:3001`, no account. The
//     random https://<words>.trycloudflare.com address is read from the log
//     and changes every start.
//   - named: `cloudflared tunnel run` with the tunnel token from the Cloudflare
//     dashboard (passed in the TUNNEL_TOKEN environment variable, never on the
//     command line). The public hostname is whatever the owner configured
//     there, so it is part of the settings rather than parsed.
//
// Tailscale Funnel would fit the same interface as a second provider: Command
// runs `tailscaled --tun=userspace-networking --statedir=<data>/tailscale`
// plus `tailscale up --authkey=$TS_AUTHKEY` and `tailscale funnel --bg
// http://127.0.0.1:3001` (two processes, so the provider would return a small
// wrapper script or the supervisor would gain a "sidecar" list), Address
// parses the https://<machine>.<tailnet>.ts.net URL that `tailscale funnel
// status` prints, and Ready waits for "Available on the internet". The auth
// key would be sealed exactly like the Cloudflare token. Funnel connections
// arrive with the real client address in Tailscale-Ingress headers rather
// than CF-Connecting-IP, so the provider would also name its client-IP header.
package tunnel

import (
	"errors"
	"regexp"
	"strings"
)

// Modes.
const (
	ModeQuick = "quick" // Cloudflare quick tunnel: random trycloudflare.com address
	ModeNamed = "named" // Cloudflare named tunnel: token + the owner's hostname
)

// Settings are what the owner chose, as the provider needs them.
type Settings struct {
	Mode     string
	Origin   string // where the helper forwards to, e.g. http://127.0.0.1:3001
	Token    string // named tunnels: the tunnel token (secret)
	Hostname string // named tunnels: the public hostname set up in the dashboard
}

// Command is how to run the helper.
type Command struct {
	Args []string // after the binary
	Env  []string // extra KEY=value entries (secrets go here, never in Args)
	// Address is known up front for named tunnels; quick tunnels learn it
	// from the log (Provider.Address).
	Address string
}

// Provider knows one kind of tunnel helper.
type Provider interface {
	Name() string
	Command(s Settings) (Command, error)
	// Address finds the public address in a line of the helper's output.
	Address(line string) (string, bool)
	// Ready reports whether a line says the tunnel is connected to the edge.
	Ready(line string) bool
	// ClientIPHeader names the header that carries the visitor's real IP.
	ClientIPHeader() string
}

// Cloudflare is the cloudflared provider.
type Cloudflare struct{}

func (Cloudflare) Name() string           { return "cloudflare" }
func (Cloudflare) ClientIPHeader() string { return "CF-Connecting-IP" }

var (
	errNoToken    = errors.New("a named tunnel needs its tunnel token")
	errNoHostname = errors.New("a named tunnel needs the public hostname you set up in the Cloudflare dashboard")
	errNoOrigin   = errors.New("no origin to forward to")
	errMode       = errors.New("unknown tunnel mode")
)

func (Cloudflare) Command(s Settings) (Command, error) {
	if s.Origin == "" {
		return Command{}, errNoOrigin
	}
	switch s.Mode {
	case ModeQuick:
		return Command{Args: []string{"tunnel", "--no-autoupdate", "--url", s.Origin}}, nil
	case ModeNamed:
		if strings.TrimSpace(s.Token) == "" {
			return Command{}, errNoToken
		}
		host, ok := NormalizeHostname(s.Hostname)
		if !ok {
			return Command{}, errNoHostname
		}
		return Command{
			Args:    []string{"tunnel", "--no-autoupdate", "run"},
			Env:     []string{"TUNNEL_TOKEN=" + strings.TrimSpace(s.Token)},
			Address: "https://" + host,
		}, nil
	}
	return Command{}, errMode
}

// quickURL matches the address cloudflared prints for a quick tunnel, e.g.
// "|  https://quiet-fox-lamp-river.trycloudflare.com  |".
var quickURL = regexp.MustCompile(`(https://[a-z0-9]+(?:-[a-z0-9]+)*\.trycloudflare\.com)(?:[^a-z0-9.:/-]|$)`)

func (Cloudflare) Address(line string) (string, bool) {
	var m string
	if sm := quickURL.FindStringSubmatch(strings.ToLower(line)); sm != nil {
		m = sm[1]
	}
	// "api.trycloudflare.com" appears in error lines when the quick-tunnel
	// service itself can't be reached; it is never a tunnel address.
	if m == "" || m == "https://api.trycloudflare.com" {
		return "", false
	}
	return m, true
}

func (Cloudflare) Ready(line string) bool {
	return strings.Contains(line, "Registered tunnel connection")
}

// hostnameRe is a DNS name with at least one dot (no ports, no schemes).
var hostnameRe = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,62}$`)

// NormalizeHostname accepts "git.example.com" or "https://git.example.com/"
// and returns "git.example.com".
func NormalizeHostname(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimRight(s, "/")
	if len(s) > 253 || !hostnameRe.MatchString(s) {
		return "", false
	}
	return s, true
}
