// Package tunneltest has stand-ins for the Tailscale binaries, for tests in
// several packages. Nothing here talks to Tailscale.
package tunneltest

import (
	"os"
	"path/filepath"
	"strings"
)

// Tailnet is the fake tailnet's DNS suffix.
const Tailnet = "tail1234.ts.net"

// FakeTailscale writes shell scripts named tailscale and tailscaled into
// dir/bin and returns their paths. They keep their state in files in dir:
//
//	state         BackendState; absent means NeedsLogin
//	hostname      the node's name
//	caps          the inside of Self.CapMap, e.g. "https":null,"funnel":null
//	goodkey       the auth key `tailscale up` accepts
//	serve.fail    if present, serve/funnel prints it and exits 1
//
// and record what they were run with:
//
//	daemon.args, daemon.env, daemon.pid, daemon.child
//	up.args, up.env, up.keyseen, up.keyfile, up.keymode, up.runs
//	serve.args, serve.env, serve.child, serve.runs, set.args, reset, loggedout
func FakeTailscale(dir string) (bin, daemonBin string, err error) {
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", "", err
	}
	bin, daemonBin = filepath.Join(binDir, "tailscale"), filepath.Join(binDir, "tailscaled")
	for path, script := range map[string]string{bin: fakeCLI, daemonBin: fakeDaemon} {
		if err := os.WriteFile(path, []byte(strings.ReplaceAll(script, "__DIR__", dir)), 0o755); err != nil {
			return "", "", err
		}
	}
	return bin, daemonBin, nil
}

const fakeDaemon = `#!/bin/sh
D="__DIR__"
echo "$@" > "$D/daemon.args"
env > "$D/daemon.env"
echo $$ > "$D/daemon.pid"
echo "2026/11/11 09:00:00 Program starting: v1.104.6" >&2
sleep 600 &
echo $! > "$D/daemon.child"
wait
`

const fakeCLI = `#!/bin/sh
D="__DIR__"
case "$1" in --socket=*) echo "$1" > "$D/socket"; shift ;; esac
cmd="$1"; shift
bump() { n=$(cat "$D/$1" 2>/dev/null || echo 0); echo $((n+1)) > "$D/$1"; }
daemon_up() { [ -f "$D/daemon.pid" ] && kill -0 "$(cat "$D/daemon.pid")" 2>/dev/null; }
if ! daemon_up; then
  echo "failed to connect to local tailscaled; it doesn't appear to be running" >&2
  exit 1
fi
state=$(cat "$D/state" 2>/dev/null || echo NeedsLogin)
host=$(cat "$D/hostname" 2>/dev/null || echo fake-host)
case "$cmd" in
status)
  if [ "$state" = Running ]; then
    printf '{"BackendState":"Running","Self":{"HostName":"%s","DNSName":"%s.tail1234.ts.net.","CapMap":{%s}},"CertDomains":null}\n' "$host" "$host" "$(cat "$D/caps" 2>/dev/null)"
  else
    printf '{"BackendState":"%s","Self":{"HostName":"%s","DNSName":""},"CertDomains":null}\n' "$state" "$host"
  fi
  ;;
up)
  bump up.runs
  echo "$@" > "$D/up.args"
  env > "$D/up.env"
  key=""; newhost=""
  for a in "$@"; do
    case "$a" in
      --auth-key=file:*) f="${a#--auth-key=file:}"; key=$(cat "$f"); echo "$f" > "$D/up.keyfile"; ls -l "$f" | cut -c1-10 > "$D/up.keymode" ;;
      --hostname=*) newhost="${a#--hostname=}" ;;
    esac
  done
  printf '%s' "$key" > "$D/up.keyseen"
  if [ -n "$key" ] && [ "$key" = "$(cat "$D/goodkey" 2>/dev/null)" ]; then
    echo Running > "$D/state"
    [ -n "$newhost" ] && echo "$newhost" > "$D/hostname"
    exit 0
  fi
  echo "backend error: invalid key: API key does not exist" >&2
  exit 1
  ;;
set)
  echo "$@" > "$D/set.args"
  for a in "$@"; do case "$a" in --hostname=*) echo "${a#--hostname=}" > "$D/hostname" ;; esac; done
  ;;
logout)
  rm -f "$D/state"
  touch "$D/loggedout"
  ;;
serve|funnel)
  if [ "$1" = reset ]; then touch "$D/reset"; exit 0; fi
  bump serve.runs
  echo "$cmd $@" > "$D/serve.args"
  env > "$D/serve.env"
  if [ -f "$D/serve.fail" ]; then cat "$D/serve.fail" >&2; exit 1; fi
  if [ "$cmd" = funnel ]; then echo "Available on the internet:"; else echo "Available within your tailnet:"; fi
  echo
  echo "https://$host.tail1234.ts.net/"
  for a in "$@"; do last="$a"; done
  echo "|-- proxy $last"
  echo
  echo "Press Ctrl+C to exit."
  sleep 600 &
  echo $! > "$D/serve.child"
  wait
  ;;
*)
  echo "unknown subcommand: $cmd" >&2
  exit 2
  ;;
esac
`
