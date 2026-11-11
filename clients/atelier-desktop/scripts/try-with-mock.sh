#!/bin/sh
# Runs the debug build of Atelier Desktop against the mock studio, in a
# throwaway home: its own config, token file, SSH folder, clone root and
# fake runner socket. The real keychain, ~/.ssh, ~/Atelier, your studio and
# the installed runner are never touched.
#
#   scripts/try-with-mock.sh            # look around by hand
#   scripts/try-with-mock.sh selftest   # scripted walk-through, then quit
#
# Build first: (cd .. && cargo build -p atelier-desktop --example mock_studio)
# and `npm run tauri build -- --debug`.
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
target="$here/../target/debug"
app="$target/bundle/macos/Atelier.app/Contents/MacOS/atelier-desktop"
[ -x "$app" ] || app="$target/atelier-desktop"
mock="$target/examples/mock_studio"
port=${ATELIER_MOCK_PORT:-4780}
studio="http://127.0.0.1:$port"

world=${ATELIER_TRY_DIR:-$(mktemp -d /tmp/atelier-try.XXXXXX)}
mkdir -p "$world"
world=$(cd "$world" && pwd -P)
# The fake runner's socket gets a short path of its own: Unix sockets can't
# have long ones.
sockdir=$(mktemp -d /tmp/atl.XXXXXX)
log="$world/app.log"
mkdir -p "$world/home/Atelier/DemiInfinity" "$world/home/Projects" "$world/config" "$world/ssh" "$world/run" "$world/studio"

g() { GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_AUTHOR_NAME=Demi GIT_AUTHOR_EMAIL=demi@example.com \
      GIT_COMMITTER_NAME=Demi GIT_COMMITTER_EMAIL=demi@example.com git "$@"; }

# Two working copies the mock studio knows. `origin` is the studio's address
# (so the app matches them); the branch follows a local bare repository
# through the remote `local`, so fetch, pull and push really work.
make_copy() {
  name=$1
  bare="$world/studio/$name.git"
  copy="$world/home/Atelier/DemiInfinity/$name"
  g init --quiet --bare -b main "$bare"
  g init --quiet -b main "$copy"
  printf '# %s\n' "$name" > "$copy/README.md"
  mkdir -p "$copy/cmd/app"
  printf 'package main\n\nfunc main() {\n\tprintln("hello")\n}\n' > "$copy/cmd/app/main.go"
  g -C "$copy" add . && g -C "$copy" commit --quiet -m "Start $name"
  g -C "$copy" remote add origin "$studio/DemiInfinity/$name.git"
  g -C "$copy" remote add local "$bare"
  g -C "$copy" push --quiet -u local main
  g -C "$copy" config user.name Demi && g -C "$copy" config user.email demi@example.com
}
make_copy Atelier
make_copy Marbl
# Atelier: a pushed commit, an unpushed one, and three uncommitted changes.
copy="$world/home/Atelier/DemiInfinity/Atelier"
printf 'package main\n\nfunc main() {\n\tprintln("hello, studio")\n\tprintln("remote access")\n}\n' > "$copy/cmd/app/main.go"
g -C "$copy" commit --quiet -am "Tray: show who's connected"
printf '# Atelier\n\nA studio of your own.\n' > "$copy/README.md"
printf 'package main\n\n// toggleRemote flips remote access on the studio.\nfunc toggleRemote() {}\n' > "$copy/cmd/app/remote.go"
printf 'The menu shows what is running.\n' > "$copy/notes.md"
# Marbl: two commits to push.
copy="$world/home/Atelier/DemiInfinity/Marbl"
echo one > "$copy/one.txt" && g -C "$copy" add . && g -C "$copy" commit --quiet -m "One"
echo two > "$copy/two.txt" && g -C "$copy" add . && g -C "$copy" commit --quiet -m "Two"

"$mock" --port "$port" --runner-socket "$sockdir/control.sock" > "$world/mock.log" 2>&1 &
mock_pid=$!
trap 'kill $mock_pid 2>/dev/null || true; rm -rf "$sockdir"' EXIT INT TERM
sleep 1

export HOME="$world/home"
export ATELIER_CONFIG_DIR="$world/config"
export ATELIER_TOKEN_STORE=file
export ATELIER_SSH_DIR="$world/ssh"
export ATELIER_RUNNER_SOCKET="$sockdir/control.sock"
export ATELIER_NO_BROWSER=1
export ATELIER_DESKTOP_DRY_RUN=1
export ATELIER_MESSAGE_SEED=0
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM=1
unset ATELIER_STUDIO ATELIER_TOKEN || true

echo "world:  $world"
echo "studio: $studio (mock; sign in by typing that address, it approves itself)"
echo "notify: curl -X POST $studio/_mock/notify"

if [ "${1:-}" = "selftest" ]; then
  ATELIER_DESKTOP_SELFTEST="$studio" "$app" > "$log" 2>&1 &
  app_pid=$!
  trap 'kill $mock_pid $app_pid 2>/dev/null || true; rm -rf "$sockdir"' EXIT INT TERM
  notified=0
  shots=0
  i=0
  while [ $i -lt 600 ]; do
    # ATELIER_TRY_SHOTS=<folder>: a picture of the app's windows each time
    # the self-test says it is holding still (needs screen recording allowed).
    if [ -n "${ATELIER_TRY_SHOTS:-}" ]; then
      n=$(grep -c "\[selftest\] shot " "$log" 2>/dev/null || true)
      if [ "${n:-0}" -gt "$shots" ]; then
        shots=$n
        name=$(grep "\[selftest\] shot " "$log" | tail -1 | sed 's/.*shot //')
        mkdir -p "$ATELIER_TRY_SHOTS"
        for id in $("$ATELIER_TRY_WINDOWS" $app_pid 2>/dev/null); do
          screencapture -x -o -l "$id" "$ATELIER_TRY_SHOTS/$name-$id.png" 2>/dev/null || true
        done
      fi
    fi
    if [ $notified -eq 0 ] && grep -q "ready-for-notify" "$log" 2>/dev/null; then
      curl -s -X POST "$studio/_mock/notify" > /dev/null && notified=1
    fi
    if grep -q "\[selftest\] done\|\[selftest\] FAIL [^st]" "$log" 2>/dev/null; then break; fi
    kill -0 $app_pid 2>/dev/null || break
    sleep 0.2; i=$((i + 1))
  done
  [ -n "${ATELIER_TRY_HOLD:-}" ] && sleep "$ATELIER_TRY_HOLD"
  kill $app_pid 2>/dev/null || true
  echo "--- app log ($log)"; cat "$log"
  echo "--- mock log"; cat "$world/mock.log"
  grep -q "\[selftest\] done" "$log" && ! grep -q "\[selftest\] FAIL" "$log"
else
  "$app"
fi
