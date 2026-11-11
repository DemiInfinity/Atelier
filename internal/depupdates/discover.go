package depupdates

// Finding manifests anywhere in a repository, npm/pnpm/yarn workspaces, and
// the packages a repository builds itself (which are never "updated" from a
// public registry).

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// MaxManifests caps how many manifests one check reads.
var MaxManifests = 60

// skipDirs are never searched: dependencies, build output, IDE folders and test data.
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "build": true, "dist": true, "out": true, "target": true, "bin": true, "obj": true,
	"bower_components": true, "jspm_packages": true, "testdata": true, "Pods": true, "Carthage": true, "DerivedData": true,
	"intermediates": true, "generated": true, "third_party": true, "__pycache__": true,
}

// SkipDir reports whether a folder name is never searched for manifests
// (also any hidden folder: .git, .gradle, .idea, .next, …).
func SkipDir(name string) bool {
	return skipDirs[name] || strings.HasPrefix(name, ".")
}

// Manifest is a dependency file found in the repository.
type Manifest struct {
	Path string // e.g. "mosaicplus-web/package.json"
	Kind string // gomod, npm, gradle-catalog, gradle-build, gradle-wrapper
	Root string // grouping folder ("" for the repository root)
}

func manifestKind(p string) string {
	base := path.Base(p)
	dir := path.Dir(p)
	switch {
	case base == "go.mod":
		return "gomod"
	case base == "package.json":
		return "npm"
	case base == "build.gradle" || base == "build.gradle.kts" || base == "settings.gradle" || base == "settings.gradle.kts":
		return "gradle-build"
	case strings.HasSuffix(base, ".versions.toml") && path.Base(dir) == "gradle":
		return "gradle-catalog"
	case base == "gradle-wrapper.properties" && strings.HasSuffix("/"+dir, "/gradle/wrapper"):
		return "gradle-wrapper"
	}
	return ""
}

func cleanDir(d string) string {
	if d == "." || d == "/" {
		return ""
	}
	return d
}

// Discover picks the manifests out of a repository's file list (as
// `git ls-tree -r --name-only` prints it), skipping vendored, generated and
// hidden folders. Gradle files are grouped under their build's root: the
// nearest folder with settings.gradle(.kts), or the folder holding gradle/.
func Discover(paths []string) []Manifest {
	files := map[string]bool{}
	var found []Manifest
	for _, p := range paths {
		p = strings.Trim(p, "/")
		if p == "" {
			continue
		}
		files[p] = true
		skip := false
		segs := strings.Split(p, "/")
		if len(segs) > 12 {
			continue
		}
		for _, s := range segs[:len(segs)-1] {
			// gradle/ itself is hidden-free; only skip the listed and dot folders.
			if SkipDir(s) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		if k := manifestKind(p); k != "" {
			found = append(found, Manifest{Path: p, Kind: k})
		}
	}
	gradleRoot := func(dir string) string {
		for d := dir; ; d = path.Dir(d) {
			cd := cleanDir(d)
			pre := ""
			if cd != "" {
				pre = cd + "/"
			}
			if files[pre+"settings.gradle"] || files[pre+"settings.gradle.kts"] {
				return cd
			}
			if cd == "" {
				return cleanDir(dir)
			}
		}
	}
	for i, m := range found {
		dir := cleanDir(path.Dir(m.Path))
		switch m.Kind {
		case "gradle-catalog":
			found[i].Root = cleanDir(path.Dir(dir)) // X/gradle/libs.versions.toml -> X
		case "gradle-wrapper":
			found[i].Root = cleanDir(path.Dir(path.Dir(dir))) // X/gradle/wrapper/… -> X
		case "gradle-build":
			found[i].Root = gradleRoot(dir)
		default:
			found[i].Root = dir
		}
	}
	sort.SliceStable(found, func(i, j int) bool {
		di, dj := strings.Count(found[i].Path, "/"), strings.Count(found[j].Path, "/")
		if di != dj {
			return di < dj // shallow manifests first: the cap drops the deepest
		}
		return found[i].Path < found[j].Path
	})
	if len(found) > MaxManifests {
		found = found[:MaxManifests]
	}
	return found
}

// ParseManifest reads one discovered manifest.
func ParseManifest(m Manifest, data []byte) ([]Dep, error) {
	var deps []Dep
	var err error
	switch m.Kind {
	case "gomod":
		deps, err = ParseGoMod(data)
	case "npm":
		deps, err = ParsePackageJSON(data)
	case "gradle-catalog":
		deps, err = ParseVersionCatalog(m.Path, data)
	case "gradle-build":
		deps, err = ParseGradleBuild(m.Path, data)
	case "gradle-wrapper":
		deps, err = ParseGradleWrapper(m.Path, data)
	default:
		return nil, fmt.Errorf("unknown manifest %s", m.Path)
	}
	for i := range deps {
		deps[i].Manifest, deps[i].Root = m.Path, m.Root
	}
	return deps, err
}

// ---------- workspaces and local packages ----------

// Local lists what a repository builds itself: npm package names from every
// package.json and module paths from every go.mod. Dependencies on them
// (workspace siblings, multi-module repositories) aren't looked up in public
// registries, where a same-named package could be someone else's.
type Local struct {
	Npm map[string]bool
	Go  map[string]bool
}

var goModuleRe = regexp.MustCompile(`(?m)^\s*module\s+("?)([^\s"]+)("?)\s*(//.*)?$`)

// AddManifest records the names a manifest defines.
func (l *Local) AddManifest(m Manifest, data []byte) {
	if l.Npm == nil {
		l.Npm, l.Go = map[string]bool{}, map[string]bool{}
	}
	switch m.Kind {
	case "npm":
		var pj struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &pj) == nil && pj.Name != "" {
			l.Npm[pj.Name] = true
		}
	case "gomod":
		if mm := goModuleRe.FindSubmatch(data); mm != nil {
			l.Go[string(mm[2])] = true
		}
	}
}

// Has reports whether a dependency is built in this repository.
func (l Local) Has(d Dep) bool {
	switch d.Ecosystem {
	case Npm:
		return l.Npm[d.Name]
	case Go:
		return l.Go[d.Name]
	}
	return false
}

// Workspaces reads the workspace globs of a folder: package.json
// "workspaces" (npm, yarn: a list or {"packages": [...]}) and
// pnpm-workspace.yaml "packages".
func Workspaces(packageJSON, pnpmWorkspace []byte) []string {
	var out []string
	if len(packageJSON) > 0 {
		var pj struct {
			Workspaces json.RawMessage `json:"workspaces"`
		}
		if json.Unmarshal(packageJSON, &pj) == nil && len(pj.Workspaces) > 0 {
			var list []string
			if json.Unmarshal(pj.Workspaces, &list) != nil {
				var obj struct {
					Packages []string `json:"packages"`
				}
				if json.Unmarshal(pj.Workspaces, &obj) == nil {
					list = obj.Packages
				}
			}
			out = append(out, list...)
		}
	}
	if len(pnpmWorkspace) > 0 {
		in := false
		for _, raw := range strings.Split(string(pnpmWorkspace), "\n") {
			l := strings.TrimRight(raw, "\r")
			t := strings.TrimSpace(l)
			switch {
			case t == "" || strings.HasPrefix(t, "#"):
			case !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") && !strings.HasPrefix(t, "-"):
				in = strings.HasPrefix(t, "packages:")
			case in && strings.HasPrefix(t, "-"):
				g := strings.TrimSpace(strings.TrimPrefix(t, "-"))
				if i := strings.Index(g, " #"); i >= 0 {
					g = strings.TrimSpace(g[:i])
				}
				out = append(out, strings.Trim(g, `"'`))
			}
		}
	}
	return out
}

// InWorkspace reports whether dir (relative to the workspace root) matches
// the workspace globs ("packages/*", "apps/**", "!packages/skip").
func InWorkspace(dir string, globs []string) bool {
	in := false
	for _, g := range globs {
		neg := strings.HasPrefix(g, "!")
		g = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(g, "!"), "./"), "/")
		if globMatch(g, dir) {
			in = !neg
		}
	}
	return in
}

func globMatch(glob, p string) bool {
	gs, ps := strings.Split(glob, "/"), strings.Split(p, "/")
	var match func(i, j int) bool
	match = func(i, j int) bool {
		if i == len(gs) {
			return j == len(ps)
		}
		if gs[i] == "**" {
			for k := j; k <= len(ps); k++ {
				if match(i+1, k) {
					return true
				}
			}
			return false
		}
		if j == len(ps) {
			return false
		}
		ok, err := path.Match(gs[i], ps[j])
		return err == nil && ok && match(i+1, j+1)
	}
	return match(0, 0)
}

// WorkspaceRoot finds the folder whose workspace globs include dir (the
// folder of a package.json), or "" with ok=false. roots maps a folder to its
// globs (see Workspaces).
func WorkspaceRoot(dir string, roots map[string][]string) (string, bool) {
	best, found := "", false
	for root, globs := range roots {
		rel := dir
		if root != "" {
			var ok bool
			if rel, ok = strings.CutPrefix(dir, root+"/"); !ok {
				continue
			}
		}
		if InWorkspace(rel, globs) && (!found || len(root) > len(best)) {
			best, found = root, true
		}
	}
	return best, found
}
