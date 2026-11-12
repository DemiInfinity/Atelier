#!/bin/sh
# Builds a throwaway repository for working on the git client's screens:
#
#   scripts/git-fixture.sh <folder>
#
#   <folder>                              on feature/tunnel-ui: 5 commits that
#                                         aren't pushed, 3 changed files
#                                         (2 staged, 1 not), 2 put-aside entries
#   <folder>.worktrees/fix-setup-race     a linked worktree on fix/setup-race
#   <folder>.remotes/studio.git           the remote `studio` (bare)
#   <folder>.remotes/github.git           the remote `github` (bare)
#
# Branches: main (tag v0.9, v0.8), feature/tunnel-ui (current, follows
# studio/feature/tunnel-ui, 5 ahead), feature/backoff-rework (conflicts
# with feature/tunnel-ui in supervisor.go and tray.go: merge it, or rebase
# onto it, to get a conflict in 2 files), feature/ssh (follows
# github/feature/ssh, 1 ahead and 1 behind), fix/setup-race (in the worktree).
#
# Nothing outside <folder>, <folder>.worktrees and <folder>.remotes is
# touched; the real git configuration and its hooks never apply. Used by
# try-with-mock.sh and by the Rust tests (tests/common: World::git_fixture).
set -eu

[ $# -eq 1 ] || { echo "usage: $0 <folder>" >&2; exit 2; }
repo=$1
case "$repo" in /*) ;; *) repo="$(pwd)/$repo" ;; esac
[ ! -e "$repo/.git" ] || { echo "$repo is already a repository" >&2; exit 1; }

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE || true
g() { git -C "$repo" "$@"; }
# Commits a day apart, the last one a few minutes ago.
now=$(date +%s)
n=0
commit() {
  n=$((n + 1))
  when=$((now - (20 - n) * 86400 / 4))
  GIT_AUTHOR_DATE="@$when +0000" GIT_COMMITTER_DATE="@$when +0000" g commit --quiet "$@"
}

mkdir -p "$repo" "$repo.remotes"
git init --quiet --bare -b main "$repo.remotes/studio.git"
git init --quiet --bare -b main "$repo.remotes/github.git"
g init --quiet -b main
g config user.name Demi
g config user.email demi@example.com

mkdir -p "$repo/cmd/atelier-desktop" "$repo/internal/tunnel" "$repo/internal/setup"
printf '# Atelier\n\nA studio of your own.\n' > "$repo/README.md"
cat > "$repo/internal/tunnel/supervisor.go" <<'GO'
package tunnel

import (
	"context"
	"log"
	"time"
)

// Supervisor keeps the tunnel up.
type Supervisor struct {
	cfg   Config
	state State
}

// Run dials until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		err := s.dial(ctx)
		if err != nil {
			log.Printf("tunnel: %v", err)
		}
		time.Sleep(backoff)
	}
}

// The lines below are padding, so that changes at the top and at the
// bottom of this file land in separate chunks.
//
//   1
//   2
//   3
//   4
//   5
//   6

func (s *Supervisor) setState(st State) {
	s.state = st
}
GO
cat > "$repo/cmd/atelier-desktop/tray.go" <<'GO'
package main

// buildMenu puts the tray menu together.
func buildMenu() []Item {
	return []Item{
		{Label: "Open studio"},
		{Label: "Quit"},
	}
}
GO
printf 'package setup\n\nfunc Run() error {\n\treturn migrate()\n}\n' > "$repo/internal/setup/setup.go"
g add . && commit -m "Tunnel: supervisor skeleton"
g tag v0.8
printf '\nRemote access works with one-time passkey links.\n' >> "$repo/README.md"
g add . && commit -m "Remote access: one-time passkey links"
g tag --annotate v0.9 -m "Remote access"
printf '\nPages can live on your own domain.\n' >> "$repo/README.md"
g add . && commit -m "Merge pull request 13: Pages custom domains"

g remote add studio "$repo.remotes/studio.git"
g remote add github "$repo.remotes/github.git"
g push --quiet -u studio main
g push --quiet github main
g push --quiet studio v0.9

# feature/ssh: on GitHub, one commit ahead here and one behind.
g switch --quiet -c feature/ssh
printf 'package setup\n\n// AcceptKey takes ed25519 keys from the phone app.\nfunc AcceptKey() {}\n' > "$repo/internal/setup/ssh.go"
g add . && commit -m "SSH: accept ed25519 keys from the phone app"
g push --quiet -u github feature/ssh
git clone --quiet -b feature/ssh "$repo.remotes/github.git" "$repo.remotes/tmp"
git -C "$repo.remotes/tmp" config user.name rhea-okafor
git -C "$repo.remotes/tmp" config user.email rhea@example.com
printf '\nOn Linux, install with the package manager.\n' >> "$repo.remotes/tmp/README.md"
git -C "$repo.remotes/tmp" commit --quiet -am "Docs: fix the install command for Linux"
git -C "$repo.remotes/tmp" push --quiet origin feature/ssh
rm -rf "$repo.remotes/tmp"
g fetch --quiet github
printf '// more\n' >> "$repo/internal/setup/ssh.go"
commit -am "SSH: reject keys without a comment"

# feature/backoff-rework: touches the same lines feature/tunnel-ui will.
g switch --quiet -c feature/backoff-rework main
sed -i.bak 's/backoff := time.Second/backoff := 2 * time.Second \/\/ reworked/' "$repo/internal/tunnel/supervisor.go"
sed -i.bak 's/{Label: "Open studio"},/{Label: "Open your studio"},/' "$repo/cmd/atelier-desktop/tray.go"
rm -f "$repo/internal/tunnel/supervisor.go.bak" "$repo/cmd/atelier-desktop/tray.go.bak"
commit -am "Backoff: start at two seconds"

# fix/setup-race: lives in a worktree of its own.
g switch --quiet -c fix/setup-race main
printf 'package setup\n\nfunc Run() error {\n\twaitForDatabase()\n\treturn migrate()\n}\n' > "$repo/internal/setup/setup.go"
commit -am "Setup: wait for the database before first run"

# feature/tunnel-ui: five commits the studio doesn't have yet.
g switch --quiet -c feature/tunnel-ui main
g push --quiet -u studio feature/tunnel-ui
printf 'package tunnel\n\nfunc debugPackets() {}\n' > "$repo/internal/tunnel/debug.go"
g add . && commit -m "debug: log every tunnel packet"
sed -i.bak 's/backoff := time.Second/backoff := s.cfg.MinBackoff/' "$repo/internal/tunnel/supervisor.go"
rm -f "$repo/internal/tunnel/supervisor.go.bak"
commit -am "Supervisor: reconnect with backoff"
sed -i.bak 's/time.Sleep(backoff)/time.Sleep(min(backoff, 30*time.Second))/' "$repo/internal/tunnel/supervisor.go"
rm -f "$repo/internal/tunnel/supervisor.go.bak"
commit -am "Supervisor: cap the backoff at 30 seconds"
sed -i.bak 's/{Label: "Open studio"},/{Label: "Open studio"},\
		{Label: "Who is connected"},/' "$repo/cmd/atelier-desktop/tray.go"
rm -f "$repo/cmd/atelier-desktop/tray.go.bak"
commit -am "Tray: show who's connected"
sed -i.bak 's/{Label: "Open studio"},/{Label: "Open studio", Remote: true},/' "$repo/cmd/atelier-desktop/tray.go"
rm -f "$repo/cmd/atelier-desktop/tray.go.bak"
commit -am "Add remote access switch to the tray menu"

g worktree add --quiet "$repo.worktrees/fix-setup-race" fix/setup-race

# Two put-aside entries: one from here, one from the worktree.
printf '\n// badge: half done\n' >> "$repo/cmd/atelier-desktop/tray.go"
printf 'draft\n' > "$repo/badge-notes.txt"
g stash push --quiet --include-untracked -m "Half-done tunnel status badge"
printf '// try a faster lock\n' >> "$repo.worktrees/fix-setup-race/internal/setup/setup.go"
git -C "$repo.worktrees/fix-setup-race" stash push --quiet -m "Try a faster setup lock"

# Three changed files: two staged, one not (with two chunks).
sed -i.bak 's/{Label: "Quit"},/{Label: "Remote sessions"},\
		{Label: "Quit"},/' "$repo/cmd/atelier-desktop/tray.go"
printf 'package main\n\n// toggleRemote flips remote access on the studio.\nfunc toggleRemote() {}\n' > "$repo/cmd/atelier-desktop/remote.go"
sed -i.bak 's/\tstate State/\tstate State\
	log   Logger/; s/\ts.state = st/\tchanged := s.state != st\
	s.state = st\
	if changed {\
		s.notify(st)\
	}/' "$repo/internal/tunnel/supervisor.go"
rm -f "$repo/cmd/atelier-desktop/tray.go.bak" "$repo/internal/tunnel/supervisor.go.bak"
g add cmd/atelier-desktop/tray.go cmd/atelier-desktop/remote.go

echo "fixture: $repo"
echo "  branch:    $(g symbolic-ref --short HEAD) ($(g rev-list --count '@{upstream}..HEAD') not pushed)"
echo "  changes:   $(g status --porcelain | tr '\n' ';')"
echo "  put aside: $(g stash list | wc -l | tr -d ' ')"
echo "  worktree:  $repo.worktrees/fix-setup-race"
