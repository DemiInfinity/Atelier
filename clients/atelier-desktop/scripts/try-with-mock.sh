#!/bin/sh
# Runs the debug build of Atelier Desktop against the mock studio, in a
# throwaway home: its own config, token file, SSH folder, clone root and
# fake runner socket. The real keychain, ~/.ssh, ~/Atelier, your studio and
# the installed runner are never touched.
#
#   scripts/try-with-mock.sh            # look around by hand
#   scripts/try-with-mock.sh selftest   # scripted walk-through, then quit
#   scripts/try-with-mock.sh selftest atl
#       the same, but starting as someone who already signed in with `atl`:
#       no watch folders, an empty clone folder, the repositories in
#       ~/Projects (one through a remote called `atelier`, one only on GitHub)
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
# Atelier also has the branch of pull request 14, pushed. The studio's copy
# then gets one more commit on it, standing in for what "Update from main"
# adds there, so a pull afterwards really has something to bring in. The
# folder goes back to main.
copy="$world/home/Atelier/DemiInfinity/Atelier"
g -C "$copy" switch --quiet -c feature/tunnel-ui
printf 'The switch lives under Open studio.\n' > "$copy/tunnel.md"
g -C "$copy" add tunnel.md && g -C "$copy" commit --quiet -m "Remote access switch"
g -C "$copy" push --quiet -u local feature/tunnel-ui
g clone --quiet -b feature/tunnel-ui "$world/studio/Atelier.git" "$world/studio/merge-tmp"
printf 'From main.\n' > "$world/studio/merge-tmp/from-main.md"
g -C "$world/studio/merge-tmp" add from-main.md
g -C "$world/studio/merge-tmp" commit --quiet -m "Merge main into feature/tunnel-ui"
g -C "$world/studio/merge-tmp" push --quiet origin feature/tunnel-ui
rm -rf "$world/studio/merge-tmp"
g -C "$copy" switch --quiet main
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

if [ "${2:-}" = "atl" ]; then
  mv "$world/home/Atelier/DemiInfinity/Atelier" "$world/home/Atelier/DemiInfinity/Marbl" "$world/home/Projects/"
  rmdir "$world/home/Atelier/DemiInfinity"
  g -C "$world/home/Projects/Atelier" remote rename origin atelier
  g init --quiet -b main "$world/home/Projects/loopkit"
  echo loop > "$world/home/Projects/loopkit/README.md"
  g -C "$world/home/Projects/loopkit" add . && g -C "$world/home/Projects/loopkit" commit --quiet -m "Start"
  g -C "$world/home/Projects/loopkit" remote add origin https://github.com/maralind/loopkit.git
  # What `atl login` leaves behind: the studio and the device, nothing else.
  printf 'studio = "%s"\ndevice_id = 7\ndevice_name = "MacBook"\n' "$studio" > "$world/config/config.toml"
  printf '{"%s": "atd_mockmockmockmockmockmockmockmockmockmock"}\n' "$studio" > "$world/config/tokens.json"
  chmod 600 "$world/config/tokens.json"
fi

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
  admin=0
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
    # The walk-through asks for the "may rename and delete" switch of
    # Settings → Devices to be turned on, after it has seen the refusal.
    if [ $admin -eq 0 ] && grep -q "ready-for-admin" "$log" 2>/dev/null; then
      curl -s -X POST "$studio/_mock/admin/on" > /dev/null && admin=1
    fi
    if grep -q "\[selftest\] done\|\[selftest\] FAIL [^st]" "$log" 2>/dev/null; then break; fi
    kill -0 $app_pid 2>/dev/null || break
    sleep 0.2; i=$((i + 1))
  done
  [ -n "${ATELIER_TRY_HOLD:-}" ] && sleep "$ATELIER_TRY_HOLD"
  kill $app_pid 2>/dev/null || true
  echo "--- app log ($log)"; cat "$log"
  echo "--- mock log"; cat "$world/mock.log"
  # After the walk: the folders are all still there, and the one that was
  # renamed in the studio points at its new address.
  echo "--- folders afterwards"
  for copy in "$world"/home/Atelier/*/* "$world"/home/Projects/*; do
    [ -d "$copy/.git" ] || continue
    echo "$copy: origin=$(g -C "$copy" remote get-url origin 2>/dev/null || echo none) files=$(ls "$copy" | wc -l | tr -d ' ')"
  done
  echo "--- [desktop] in config.toml"; sed -n '/^\[desktop\]/,$p' "$world/config/config.toml"
  grep -q "\[selftest\] done" "$log" && ! grep -q "\[selftest\] FAIL" "$log"
else
  "$app"
fi
