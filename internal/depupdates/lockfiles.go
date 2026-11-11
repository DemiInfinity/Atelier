package depupdates

// Lockfile regeneration. The server never runs package managers. Instead a
// workflow step, `uses: atelier/update-lockfiles@v1` (built into the
// runner), regenerates lockfiles on a runner and pushes one commit to the
// update branch with the job's token. Three checks keep that token narrow:
//
//  1. The runner action only acts on atelier-updates/* branches.
//  2. The server lets a job token push only when the run is for a branch
//     the updates bot created and still has an open pull request for, and
//     the repository turned on "Regenerate lockfiles" (web access policy).
//  3. The pre-receive hook accepts a job's push only as a fast-forward of
//     exactly that branch, changing nothing but lockfiles (LockfilePushAllowed).

import (
	"path"
	"sort"
	"strings"
)

// lockfileNames are the files the lockfile job may change.
var lockfileNames = map[string]bool{
	"package-lock.json": true, "npm-shrinkwrap.json": true, "pnpm-lock.yaml": true, "yarn.lock": true,
	"go.sum": true, "go.work.sum": true,
	"gradle.lockfile": true, "settings-gradle.lockfile": true, "buildscript-gradle.lockfile": true,
}

// IsLockfile reports whether the lockfile job may change a path. go.mod is
// included because `go mod tidy` adds the indirect requirements a new
// version needs; gradle/verification-metadata.xml holds dependency checksums.
// Wrapper scripts and JARs are not: they are code.
func IsLockfile(p string) bool {
	base := path.Base(p)
	switch {
	case lockfileNames[base], base == "go.mod":
		return true
	case strings.HasSuffix(base, ".lockfile") && strings.Contains("/"+p, "/gradle/dependency-locks/"):
		return true
	case p == "gradle/verification-metadata.xml" || strings.HasSuffix(p, "/gradle/verification-metadata.xml"):
		return true
	}
	return false
}

// LockfileCommand is one regeneration a runner performs in Dir.
type LockfileCommand struct {
	Dir  string   // relative to the workspace ("" for the root)
	Argv []string // never through a shell
	Why  string   // the lockfile that asked for it
}

// LockfileCommands plans the regeneration for the files in a checkout
// (paths relative to its root): npm, pnpm and yarn lockfiles, go.sum next
// to a go.mod, and Gradle dependency locks or verification metadata in a
// build with a gradlew script. Install scripts are never run.
func LockfileCommands(files []string) []LockfileCommand {
	have := map[string]bool{}
	for _, f := range files {
		have[strings.Trim(f, "/")] = true
	}
	join := func(dir, name string) string {
		if dir == "" {
			return name
		}
		return dir + "/" + name
	}
	var out []LockfileCommand
	gradleDone := map[string]bool{}
	var sorted []string
	for f := range have {
		sorted = append(sorted, f)
	}
	sort.Strings(sorted)
	for _, f := range sorted {
		skip := false
		for _, s := range strings.Split(f, "/")[:strings.Count(f, "/")] {
			if SkipDir(s) && s != ".gradle" {
				skip = true
			}
		}
		if skip {
			continue
		}
		dir, base := cleanDir(path.Dir(f)), path.Base(f)
		switch {
		case base == "package-lock.json" && have[join(dir, "package.json")]:
			out = append(out, LockfileCommand{Dir: dir, Why: f, Argv: []string{"npm", "install", "--package-lock-only", "--ignore-scripts", "--no-audit", "--no-fund"}})
		case base == "pnpm-lock.yaml" && have[join(dir, "package.json")]:
			out = append(out, LockfileCommand{Dir: dir, Why: f, Argv: []string{"pnpm", "install", "--lockfile-only", "--ignore-scripts"}})
		case base == "yarn.lock" && have[join(dir, "package.json")]:
			if have[join(dir, ".yarnrc.yml")] {
				out = append(out, LockfileCommand{Dir: dir, Why: f, Argv: []string{"yarn", "install", "--mode", "update-lockfile"}})
			} else {
				out = append(out, LockfileCommand{Dir: dir, Why: f, Argv: []string{"yarn", "install", "--ignore-scripts", "--non-interactive"}})
			}
		case base == "go.mod":
			// go.sum is created when missing: a module with requirements needs one.
			out = append(out, LockfileCommand{Dir: dir, Why: f, Argv: []string{"go", "mod", "tidy"}})
		case base == "gradle.lockfile" || strings.HasSuffix(f, ".lockfile") && strings.Contains("/"+f, "/gradle/dependency-locks/") ||
			base == "verification-metadata.xml" && path.Base(dir) == "gradle":
			root := gradleWrapperRoot(dir, have)
			if root == "-" || gradleDone[root+base] {
				continue
			}
			gradleDone[root+base] = true
			if base == "verification-metadata.xml" {
				out = append(out, LockfileCommand{Dir: root, Why: f, Argv: []string{"./gradlew", "--no-daemon", "--write-verification-metadata", "sha256", "help"}})
			} else if !gradleDone[root] {
				gradleDone[root] = true
				out = append(out, LockfileCommand{Dir: root, Why: f, Argv: []string{"./gradlew", "--no-daemon", "dependencies", "--write-locks"}})
			}
		}
	}
	return out
}

// gradleWrapperRoot is the nearest folder at or above dir with a gradlew
// script, or "-" when there is none.
func gradleWrapperRoot(dir string, have map[string]bool) string {
	for d := dir; ; d = cleanDir(path.Dir(d)) {
		p := "gradlew"
		if d != "" {
			p = d + "/gradlew"
		}
		if have[p] {
			return d
		}
		if d == "" {
			return "-"
		}
	}
}

// RefUpdate is one line of a pre-receive hook's input.
type RefUpdate struct{ Old, New, Ref string }

// LockfilePush is what the pre-receive hook knows about a job's push.
type LockfilePush struct {
	Branch     string      // the run's branch (refs/heads/ stripped)
	BotPR      bool        // the updates bot has an open pull request from Branch
	Enabled    bool        // the repository turned on lockfile regeneration
	Updates    []RefUpdate // what the push changes
	FastFwd    func(old, new string) (bool, error)
	Merges     func(old, new string) (bool, error)     // whether old..new has merge commits
	ChangedIn  func(old, new string) ([]string, error) // every path any commit in old..new touches
	MaxChanged int                                     // default 200
}

// LockfilePushAllowed returns why a job's push is refused ("" when it's fine):
// exactly one fast-forward update of the run's own atelier-updates branch,
// which the bot opened a pull request from, changing only lockfiles.
func LockfilePushAllowed(p LockfilePush) (string, error) {
	switch {
	case !p.Enabled:
		return "lockfile regeneration is off for this repository (Security settings).", nil
	case !IsUpdateBranch(p.Branch):
		return "Actions jobs can only push to atelier-updates/* branches.", nil
	case !p.BotPR:
		return p.Branch + " has no open pull request from atelier-updates.", nil
	case len(p.Updates) != 1:
		return "an Actions job may update exactly one branch per push.", nil
	}
	u := p.Updates[0]
	zero := func(s string) bool { return strings.Trim(s, "0") == "" }
	switch {
	case u.Ref != "refs/heads/"+p.Branch:
		return "this job may only push to " + p.Branch + ", not " + u.Ref + ".", nil
	case zero(u.Old) || zero(u.New):
		return "Actions jobs can't create or delete branches.", nil
	}
	ff, err := p.FastFwd(u.Old, u.New)
	if err != nil {
		return "", err
	}
	if !ff {
		return "Actions jobs can't force-push " + p.Branch + ".", nil
	}
	if p.Merges != nil {
		merges, err := p.Merges(u.Old, u.New)
		if err != nil {
			return "", err
		}
		if merges {
			return "Actions jobs can't push merge commits to update branches.", nil
		}
	}
	changed, err := p.ChangedIn(u.Old, u.New)
	if err != nil {
		return "", err
	}
	max := p.MaxChanged
	if max <= 0 {
		max = 200
	}
	if len(changed) > max {
		return "the push changes too many files for a lockfile update.", nil
	}
	for _, f := range changed {
		if !IsLockfile(f) {
			return "Actions jobs may only change lockfiles on update branches, not " + f + ".", nil
		}
	}
	return "", nil
}
