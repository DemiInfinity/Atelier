package depupdates

// Planning update pull requests: which updates go in which pull request,
// their branches, titles and bodies.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
)

// BranchRoot is the prefix of every update branch. Only branches under it,
// created by the updates bot, may be pushed to by the lockfile job.
const BranchRoot = "atelier-updates/"

var slugRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(s, "-"), "-.")
	for strings.Contains(s, "..") {
		s = strings.ReplaceAll(s, "..", ".")
	}
	if s == "" {
		s = "x"
	}
	return s
}

// scopePrefix is "atelier-updates/<manager>/" for the repository root and
// "atelier-updates/<manager>@<folder>/" for a subfolder (slashes in the
// folder become "+"), so prefixes of different folders never overlap.
func scopePrefix(manager, root string) string {
	if root == "" {
		return BranchRoot + manager + "/"
	}
	parts := strings.Split(root, "/")
	for i, p := range parts {
		parts[i] = slug(p)
	}
	return BranchRoot + manager + "@" + strings.Join(parts, "+") + "/"
}

// Folder is where a single dependency's pull request says it is: the
// manifest's folder, or the Gradle build's root for version catalogs and the
// wrapper (which live in its gradle/ folder).
func (d Dep) Folder() string {
	if d.Ecosystem == Gradle || d.Ecosystem == Maven && strings.Contains(d.Loc, ".") {
		return d.Root
	}
	return folderOf(d.Manifest)
}

// BranchPrefix is the prefix of one dependency's update branches
// ("atelier-updates/npm/left-pad/", "atelier-updates/gradle@android/androidx.core-core-ktx/").
func BranchPrefix(d Dep) string {
	return scopePrefix(d.Manager(), d.Folder()) + slug(d.Name) + "/"
}

// Branch is the branch of one dependency's update to version.
func Branch(d Dep, version string) string {
	return BranchPrefix(d) + slug(strings.TrimPrefix(version, "v"))
}

// GroupPrefix is the prefix of grouped update branches of a manager and
// folder. Package names can't start with "_" (npm, Go) or be slugged to it.
func GroupPrefix(manager, root string) string {
	return scopePrefix(manager, root) + "_group/"
}

// IsUpdateBranch reports whether a branch has the updates bot's prefix.
func IsUpdateBranch(branch string) bool {
	return strings.HasPrefix(branch, BranchRoot) && len(branch) > len(BranchRoot) && !strings.Contains(branch, "..")
}

// PR is one planned update pull request.
type PR struct {
	Manager string
	Root    string
	Group   bool     // a grouped minor-and-patch pull request
	Updates []Result // sorted by name
	Branch  string
	Prefix  string // finds the open pull request this one would duplicate
}

// PlanOptions say which updates get pull requests and how.
type PlanOptions struct {
	Group  bool               // group minor and patch updates per manager and folder
	Wanted func(Result) bool  // whether an update gets a pull request at all
	Skip   func(Result) bool  // already in an open pull request of its own
	Order  func(a, b PR) bool // optional
}

// Plan turns check results into pull requests: majors (and everything when
// grouping is off) one per dependency, minor and patch updates one per
// manager and folder when grouping is on. A group of one is a plain PR.
func Plan(results []Result, o PlanOptions) []PR {
	var out []PR
	groups := map[string][]Result{}
	var keys []string
	for _, r := range results {
		if r.Err != nil || r.Latest == "" || (o.Wanted != nil && !o.Wanted(r)) || (o.Skip != nil && o.Skip(r)) {
			continue
		}
		if o.Group && r.Kind != "major" {
			k := r.Manager() + "\x00" + r.Root
			if _, ok := groups[k]; !ok {
				keys = append(keys, k)
			}
			groups[k] = append(groups[k], r)
			continue
		}
		out = append(out, single(r))
	}
	for _, k := range keys {
		rs := groups[k]
		if len(rs) == 1 {
			out = append(out, single(rs[0]))
			continue
		}
		sort.SliceStable(rs, func(i, j int) bool {
			if rs[i].Name != rs[j].Name {
				return rs[i].Name < rs[j].Name
			}
			return rs[i].Manifest < rs[j].Manifest
		})
		h := sha256.New()
		for _, r := range rs {
			fmt.Fprintf(h, "%s\x00%s\x00%s\n", r.Manifest, r.Name, r.Latest)
		}
		pre := GroupPrefix(rs[0].Manager(), rs[0].Root)
		out = append(out, PR{Manager: rs[0].Manager(), Root: rs[0].Root, Group: true, Updates: rs,
			Prefix: pre, Branch: pre + hex.EncodeToString(h.Sum(nil))[:12]})
	}
	if o.Order != nil {
		sort.SliceStable(out, func(i, j int) bool { return o.Order(out[i], out[j]) })
	}
	return out
}

func single(r Result) PR {
	return PR{Manager: r.Manager(), Root: r.Root, Updates: []Result{r}, Branch: Branch(r.Dep, r.Latest), Prefix: BranchPrefix(r.Dep)}
}

func folderOf(manifest string) string {
	return cleanDir(path.Dir(manifest))
}

// Title is the pull request's title. Paths are named when not at the root.
func (p PR) Title() string {
	if p.Group {
		t := "Update minor and patch dependencies (" + p.Manager + ")"
		if p.Root != "" {
			t += " in " + p.Root
		}
		return t
	}
	r := p.Updates[0]
	t := fmt.Sprintf("Bump %s from %s to %s", r.Name, r.Spec, r.NewSpec(r.Latest))
	if dir := r.Folder(); dir != "" {
		t += " in " + dir
	}
	return t
}

// CommitMessage is the message of the commit editing one manifest.
func (p PR) CommitMessage(manifest string) string {
	if !p.Group {
		return p.Title()
	}
	var names []string
	for _, r := range p.Updates {
		if r.Manifest == manifest {
			names = append(names, fmt.Sprintf("%s to %s", r.Name, r.NewSpec(r.Latest)))
		}
	}
	return "Update " + strings.Join(names, ", ") + " in " + manifest
}

func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.ReplaceAll(s, "\n", " ")
}

// PackageURL links to a dependency's page on its registry.
func PackageURL(d Dep) string {
	switch d.Ecosystem {
	case Go:
		return "https://pkg.go.dev/" + d.Name
	case Npm:
		return "https://www.npmjs.com/package/" + d.Name
	case Gradle:
		return "https://docs.gradle.org/current/release-notes.html"
	case Maven:
		if len(d.Coords) == 0 {
			return ""
		}
		if id, ok := IsPluginMarker(d.Coords[0]); ok {
			if IsGoogleGroup(id) {
				return "https://developer.android.com/build/releases/gradle-plugin"
			}
			return "https://plugins.gradle.org/plugin/" + id
		}
		g, a, _ := strings.Cut(d.Coords[0], ":")
		if IsGoogleGroup(g) {
			return "https://maven.google.com/web/index.html#" + d.Coords[0]
		}
		return "https://central.sonatype.com/artifact/" + g + "/" + a
	}
	return ""
}

// AdvisoryURL links to an OSV advisory.
func AdvisoryURL(id string) string { return "https://osv.dev/vulnerability/" + url.PathEscape(id) }

// BodyOptions are the parts of a pull request body that depend on the repository.
type BodyOptions struct {
	Lockfiles  bool // the repository regenerates lockfiles on update branches
	Supersedes int64
}

// Body is the pull request description: a table of the updates, advisories,
// and what happens to lockfiles.
func (p PR) Body(o BodyOptions) string {
	var b strings.Builder
	if p.Group {
		fmt.Fprintf(&b, "Updates %d %s dependencies", len(p.Updates), p.Manager)
		if p.Root != "" {
			fmt.Fprintf(&b, " in `%s`", p.Root)
		}
		b.WriteString(" to their newest minor or patch versions.\n\n")
	} else {
		r := p.Updates[0]
		fmt.Fprintf(&b, "Bumps `%s` from `%s` to `%s` (%s update) in `%s`.\n\n", r.Name, r.Spec, r.NewSpec(r.Latest), r.Kind, r.Manifest)
	}
	b.WriteString("| Package | From | To | Type | Advisories | Links |\n|---|---|---|---|---|---|\n")
	var vulnerable []Result
	for _, r := range p.Updates {
		adv := "–"
		if len(r.Vulns) > 0 {
			var ls []string
			for _, id := range r.Vulns {
				ls = append(ls, fmt.Sprintf("[%s](%s)", mdCell(id), AdvisoryURL(id)))
			}
			adv = strings.Join(ls, ", ")
			if r.Fixes > 0 {
				adv += fmt.Sprintf(" (fixes %d)", r.Fixes)
			}
			vulnerable = append(vulnerable, r)
		}
		name := "`" + mdCell(r.Name) + "`"
		if len(r.Coords) > 1 {
			name += " (" + mdCell(strings.Join(r.Coords, ", ")) + ")"
		}
		if p.Group {
			name += " in `" + mdCell(r.Manifest) + "`"
		}
		links := "–"
		if u := PackageURL(r.Dep); u != "" {
			links = fmt.Sprintf("[%s](%s)", registryLabel(r.Dep), u)
		}
		fmt.Fprintf(&b, "| %s | `%s` | `%s` | %s | %s | %s |\n", name, mdCell(r.Spec), mdCell(r.NewSpec(r.Latest)), r.Kind, adv, links)
	}
	b.WriteString("\n")
	if len(vulnerable) > 0 {
		var fixes int
		for _, r := range vulnerable {
			fixes += r.Fixes
		}
		fmt.Fprintf(&b, "**Security:** %d of these packages have known advisories in the current version", len(vulnerable))
		if fixes > 0 {
			fmt.Fprintf(&b, "; this update fixes %d of them", fixes)
		}
		b.WriteString(".\n\n")
	}
	if o.Supersedes > 0 {
		fmt.Fprintf(&b, "Supersedes #%d.\n\n", o.Supersedes)
	}
	b.WriteString(p.lockfileNote(o.Lockfiles))
	b.WriteString("\nOpened by atelier-updates from the repository's Security settings.")
	return b.String()
}

func registryLabel(d Dep) string {
	switch d.Ecosystem {
	case Go:
		return "pkg.go.dev"
	case Npm:
		return "npm"
	case Gradle:
		return "release notes"
	}
	if len(d.Coords) > 0 {
		if id, ok := IsPluginMarker(d.Coords[0]); ok && !IsGoogleGroup(id) {
			return "Plugin Portal"
		}
		if g, _, _ := strings.Cut(d.Coords[0], ":"); IsGoogleGroup(g) {
			return "Google Maven"
		}
	}
	return "Maven Central"
}

func (p PR) lockfileNote(regenerated bool) string {
	var lock string
	switch p.Manager {
	case Go:
		lock = "`go.sum`"
	case Npm:
		lock = "the lockfile (`package-lock.json`, `pnpm-lock.yaml` or `yarn.lock`)"
	case Gradle:
		lock = "Gradle lockfiles (`gradle.lockfile`) and the wrapper JAR"
	}
	if regenerated {
		return "Only the manifest was edited here. A workflow using `atelier/update-lockfiles` regenerates " + lock +
			" on this branch and pushes the result; CI then runs on that commit.\n"
	}
	switch p.Manager {
	case Go:
		return "Only `go.mod` was changed: `go.sum` was not regenerated. Let CI run (or run `go mod tidy` and push) before merging.\n"
	case Npm:
		return "Only `package.json` was changed: " + lock + " was not regenerated. Let CI run (or run your package manager's install and push) before merging.\n"
	}
	return "Only the version was changed: " + lock + " were not regenerated. Let CI run (or run `./gradlew dependencies --write-locks` / `./gradlew wrapper` and push) before merging.\n"
}

// Edit applies an update to its manifest's content.
func (r Result) Edit(data []byte) ([]byte, error) {
	if r.Latest == "" {
		return nil, fmt.Errorf("%s has no update", r.Name)
	}
	switch r.Ecosystem {
	case Go:
		return UpdateGoMod(data, r.Name, r.Latest)
	case Npm:
		return UpdatePackageJSON(data, r.Name, r.Spec, r.NewSpec(r.Latest))
	case Gradle:
		return UpdateGradleWrapper(data, r.Spec, r.Latest, r.Checksum)
	case Maven:
		if strings.Contains(r.Loc, ".") {
			return UpdateVersionCatalog(data, r.Loc, r.Spec, r.Latest)
		}
		return UpdateGradleBuild(data, r.Dep, r.Latest)
	}
	return nil, fmt.Errorf("unknown ecosystem %q", r.Ecosystem)
}
