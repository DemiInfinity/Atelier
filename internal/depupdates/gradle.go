package depupdates

// Gradle manifests: version catalogs (gradle/libs.versions.toml), build
// scripts (build.gradle, build.gradle.kts, settings.gradle[.kts]) and the
// wrapper (gradle/wrapper/gradle-wrapper.properties). Parsing is line based
// and edits only ever replace the version text, so formatting, comments and
// ordering stay as written.

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// plainMavenVersion is a fixed version: no ranges ([1,2)), dynamic versions
// (1.+, latest.release) or interpolation ($kotlin).
var plainMavenVersion = regexp.MustCompile(`^[0-9][0-9A-Za-z._\-]*$`)

func isPlainMavenVersion(v string) bool {
	return plainMavenVersion.MatchString(v) && !strings.HasSuffix(v, "+")
}

var (
	tomlSectionRe = regexp.MustCompile(`^\s*\[\s*([A-Za-z0-9_.\-]+)\s*\]\s*(#.*)?$`)
	tomlKeyRe     = regexp.MustCompile(`^\s*("?)([A-Za-z0-9_.\-]+)("?)\s*=\s*(.*)$`)
	tomlStringRe  = regexp.MustCompile(`^"([^"\\]*)"\s*(#.*)?$`)
	tomlFieldRe   = func(name string) *regexp.Regexp {
		return regexp.MustCompile(`(?:^|[{,\s])` + regexp.QuoteMeta(name) + `\s*=\s*"([^"\\]*)"`)
	}
	catModuleRe   = tomlFieldRe("module")
	catGroupRe    = tomlFieldRe("group")
	catNameRe     = tomlFieldRe("name")
	catIDRe       = tomlFieldRe("id")
	catVersionRe  = tomlFieldRe("version")
	catVerRefRe   = tomlFieldRe("version.ref")
	catVerRefTbRe = regexp.MustCompile(`(?:^|[{,\s])version\s*=\s*\{\s*ref\s*=\s*"([^"\\]*)"\s*\}`)
)

type catalogLine struct {
	section, key, value string // value: the text after "="
	line                int
}

// catalogLines walks a TOML catalog's "key = value" lines with their section.
func catalogLines(data []byte) []catalogLine {
	var out []catalogLine
	section := ""
	for i, raw := range strings.Split(string(data), "\n") {
		l := strings.TrimRight(raw, "\r")
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if m := tomlSectionRe.FindStringSubmatch(l); m != nil {
			section = m[1]
			continue
		}
		if m := tomlKeyRe.FindStringSubmatch(l); m != nil && m[1] == m[3] {
			out = append(out, catalogLine{section: section, key: m[2], value: strings.TrimSpace(m[4]), line: i})
		}
	}
	return out
}

type catalogUse struct {
	alias, coord, version, ref string // coord: group:artifact (plugin marker for plugins)
	plugin                     bool
}

func catalogEntry(section string, cl catalogLine) (catalogUse, bool) {
	u := catalogUse{alias: cl.key, plugin: section == "plugins"}
	if m := tomlStringRe.FindStringSubmatch(cl.value); m != nil {
		// "group:artifact:version" or "plugin.id:version"
		parts := strings.Split(m[1], ":")
		switch {
		case !u.plugin && len(parts) == 3:
			u.coord, u.version = parts[0]+":"+parts[1], parts[2]
		case !u.plugin && len(parts) == 2:
			u.coord = m[1]
		case u.plugin && len(parts) == 2:
			u.coord, u.version = PluginMarker(parts[0]), parts[1]
		default:
			return u, false
		}
		return u, true
	}
	v := cl.value
	if !strings.HasPrefix(v, "{") {
		return u, false
	}
	if u.plugin {
		m := catIDRe.FindStringSubmatch(v)
		if m == nil {
			return u, false
		}
		u.coord = PluginMarker(m[1])
	} else if m := catModuleRe.FindStringSubmatch(v); m != nil {
		u.coord = m[1]
	} else {
		g, n := catGroupRe.FindStringSubmatch(v), catNameRe.FindStringSubmatch(v)
		if g == nil || n == nil {
			return u, false
		}
		u.coord = g[1] + ":" + n[1]
	}
	if m := catVerRefRe.FindStringSubmatch(v); m != nil {
		u.ref = m[1]
	} else if m := catVerRefTbRe.FindStringSubmatch(v); m != nil {
		u.ref = m[1]
	} else if m := catVersionRe.FindStringSubmatch(v); m != nil {
		u.version = m[1]
	}
	return u, true
}

// ParseVersionCatalog lists a Gradle version catalog's dependencies. A
// [versions] entry used by libraries or plugins is one dependency (its
// Coords are every artifact sharing it); a library or plugin with its own
// version is another. Rich versions ({ strictly = … }), ranges and dynamic
// versions are left alone.
func ParseVersionCatalog(path string, data []byte) ([]Dep, error) {
	lines := catalogLines(data)
	versions := map[string]string{}
	var order []string
	for _, cl := range lines {
		if cl.section != "versions" {
			continue
		}
		if m := tomlStringRe.FindStringSubmatch(cl.value); m != nil {
			versions[cl.key] = m[1]
			order = append(order, cl.key)
		}
	}
	refs := map[string][]string{}
	var out []Dep
	seen := map[string]bool{}
	for _, cl := range lines {
		if cl.section != "libraries" && cl.section != "plugins" {
			continue
		}
		u, ok := catalogEntry(cl.section, cl)
		if !ok || u.coord == "" {
			continue
		}
		if u.ref != "" {
			if !containsStr(refs[u.ref], u.coord) {
				refs[u.ref] = append(refs[u.ref], u.coord)
			}
			continue
		}
		if !isPlainMavenVersion(u.version) {
			continue
		}
		name := u.coord
		if id, ok := IsPluginMarker(u.coord); ok {
			name = id
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, Dep{Ecosystem: Maven, Name: name, Manifest: path, Spec: u.version, Version: u.version,
			Coords: []string{u.coord}, Loc: cl.section + "." + u.alias})
	}
	for _, key := range order {
		coords, v := refs[key], versions[key]
		if len(coords) == 0 || !isPlainMavenVersion(v) {
			continue
		}
		name := key
		if len(coords) == 1 {
			name = coords[0]
			if id, ok := IsPluginMarker(name); ok {
				name = id
			}
		}
		if seen[name] {
			name = "versions." + key
		}
		seen[name] = true
		out = append(out, Dep{Ecosystem: Maven, Name: name, Manifest: path, Spec: v, Version: v, Coords: coords, Loc: "versions." + key})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// UpdateVersionCatalog sets the version at loc ("versions.kotlin",
// "libraries.core-ktx", "plugins.agp") from old to new, changing nothing else.
func UpdateVersionCatalog(data []byte, loc, old, new string) ([]byte, error) {
	section, key, ok := strings.Cut(loc, ".")
	if !ok || !isPlainMavenVersion(new) {
		return nil, fmt.Errorf("can't update %s", loc)
	}
	lines := strings.Split(string(data), "\n")
	for _, cl := range catalogLines(data) {
		if cl.section != section || cl.key != key {
			continue
		}
		l := lines[cl.line]
		eq := strings.Index(l, "=")
		head, val := l[:eq+1], l[eq+1:]
		var re *regexp.Regexp
		var repl string
		switch {
		case section == "versions":
			re, repl = regexp.MustCompile(`^(\s*)"`+regexp.QuoteMeta(old)+`"`), `${1}"`+new+`"`
		case tomlStringRe.MatchString(strings.TrimSpace(val)):
			re, repl = regexp.MustCompile(`:`+regexp.QuoteMeta(old)+`"`), `:`+new+`"`
		default:
			re, repl = regexp.MustCompile(`((?:^|[{,\s])version\s*=\s*)"`+regexp.QuoteMeta(old)+`"`), `${1}"`+new+`"`
		}
		if !re.MatchString(val) {
			return nil, fmt.Errorf("%s isn't %s in the version catalog", loc, old)
		}
		lines[cl.line] = head + re.ReplaceAllString(val, repl)
		return []byte(strings.Join(lines, "\n")), nil
	}
	return nil, fmt.Errorf("%s isn't in the version catalog", loc)
}

// ---------- build scripts ----------

var (
	// "group:artifact:version" or 'group:artifact:version', optionally @aar.
	gradleCoordRe = regexp.MustCompile(`(?:"|')([A-Za-z0-9_.\-]+):([A-Za-z0-9_.\-]+):([0-9][0-9A-Za-z._\-]*)(@[A-Za-z]+)?(?:"|')`)
	// id("x") version "y", id 'x' version 'y', kotlin("android") version "y"
	gradlePluginRe = regexp.MustCompile(`\b(id|kotlin)\s*\(?\s*["']([A-Za-z0-9_.\-]+)["']\s*\)?\s*version\s*\(?\s*["']([0-9][0-9A-Za-z._\-]*)["']`)
)

// gradleCode blanks // and /* */ comments (keeping offsets) so commented-out
// dependencies aren't offered.
func gradleCode(data []byte) []byte {
	b := bytes.Clone(data)
	inStr := byte(0)
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case inStr != 0:
			if c == '\\' {
				i++
			} else if c == inStr || c == '\n' {
				inStr = 0
			}
		case c == '"' || c == '\'':
			inStr = c
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for ; i < len(b) && b[i] != '\n'; i++ {
				b[i] = ' '
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			for ; i < len(b) && !(b[i] == '*' && i+1 < len(b) && b[i+1] == '/'); i++ {
				if b[i] != '\n' {
					b[i] = ' '
				}
			}
			if i+1 < len(b) {
				b[i], b[i+1] = ' ', ' '
				i++
			}
		}
	}
	return b
}

func pluginID(fn, id string) string {
	if fn == "kotlin" {
		return "org.jetbrains.kotlin." + id
	}
	return id
}

// ParseGradleBuild lists string coordinates ("g:a:v") and versioned plugins
// in a build or settings script, Groovy or Kotlin DSL. Versions written as
// variables ($kotlin_version) aren't offered.
func ParseGradleBuild(path string, data []byte) ([]Dep, error) {
	code := gradleCode(data)
	var out []Dep
	seen := map[string]bool{}
	for _, m := range gradlePluginRe.FindAllSubmatch(code, -1) {
		id, v := pluginID(string(m[1]), string(m[2])), string(m[3])
		if seen[id] || !isPlainMavenVersion(v) {
			continue
		}
		seen[id] = true
		out = append(out, Dep{Ecosystem: Maven, Name: id, Manifest: path, Spec: v, Version: v, Coords: []string{PluginMarker(id)}, Loc: "plugin"})
	}
	for _, m := range gradleCoordRe.FindAllSubmatch(code, -1) {
		coord, v := string(m[1])+":"+string(m[2]), string(m[3])
		if seen[coord] || !isPlainMavenVersion(v) {
			continue
		}
		seen[coord] = true
		out = append(out, Dep{Ecosystem: Maven, Name: coord, Manifest: path, Spec: v, Version: v, Coords: []string{coord}, Loc: "dep"})
	}
	return out, nil
}

// UpdateGradleBuild changes one dependency's or plugin's version in a build
// script: every occurrence written with the old version.
func UpdateGradleBuild(data []byte, d Dep, new string) ([]byte, error) {
	if !isPlainMavenVersion(new) {
		return nil, fmt.Errorf("%q isn't a plain version", new)
	}
	var re *regexp.Regexp
	var repl string
	if d.Loc == "plugin" {
		id := d.Name
		short, isKotlin := strings.CutPrefix(id, "org.jetbrains.kotlin.")
		alt := `id\s*\(?\s*["']` + regexp.QuoteMeta(id) + `["']`
		if isKotlin {
			alt = `(?:` + alt + `|kotlin\s*\(\s*["']` + regexp.QuoteMeta(short) + `["']\s*\))`
		}
		re = regexp.MustCompile(`(\b` + alt + `\s*\)?\s*version\s*\(?\s*["'])` + regexp.QuoteMeta(d.Spec) + `(["'])`)
		repl = "${1}" + new + "${2}"
	} else {
		re = regexp.MustCompile(`(["'])` + regexp.QuoteMeta(d.Name+":"+d.Spec) + `((?:@[A-Za-z]+)?["'])`)
		repl = "${1}" + d.Name + ":" + new + "${2}"
	}
	if !re.Match(data) {
		return nil, fmt.Errorf("%s %s isn't in %s", d.Name, d.Spec, d.Manifest)
	}
	return re.ReplaceAll(data, []byte(repl)), nil
}

// ---------- wrapper ----------

var (
	wrapperURLRe = regexp.MustCompile(`(?m)^(\s*distributionUrl\s*[=:]\s*\S*?gradle-)([0-9][0-9A-Za-z.\-]*?)-(bin|all)(\.zip\s*)$`)
	wrapperSumRe = regexp.MustCompile(`(?m)^(\s*distributionSha256Sum\s*[=:]\s*)([0-9A-Fa-f]*)(\s*)$`)
)

// ParseGradleWrapper reads Gradle's version from gradle-wrapper.properties.
func ParseGradleWrapper(path string, data []byte) ([]Dep, error) {
	m := wrapperURLRe.FindSubmatch(data)
	if m == nil {
		return nil, errors.New("no gradle-N-bin.zip or -all.zip distributionUrl")
	}
	v := string(m[2])
	return []Dep{{Ecosystem: Gradle, Name: "gradle", Manifest: path, Spec: v, Version: v, Loc: "wrapper",
		Dist: string(m[3]), PinsChecksum: wrapperSumRe.Match(data)}}, nil
}

// UpdateGradleWrapper points distributionUrl at another version and, when
// the file pins distributionSha256Sum, replaces the checksum.
func UpdateGradleWrapper(data []byte, old, new, sum string) ([]byte, error) {
	m := wrapperURLRe.FindSubmatchIndex(data)
	if m == nil || string(data[m[4]:m[5]]) != old {
		return nil, fmt.Errorf("gradle-wrapper.properties isn't on Gradle %s", old)
	}
	if !isPlainMavenVersion(new) {
		return nil, fmt.Errorf("%q isn't a Gradle version", new)
	}
	out := append(append(append([]byte{}, data[:m[4]]...), new...), data[m[5]:]...)
	if wrapperSumRe.Match(out) {
		if !sha256Re.MatchString(sum) {
			return nil, errors.New("the wrapper pins a checksum but the new one is unknown")
		}
		out = wrapperSumRe.ReplaceAll(out, []byte("${1}"+sum+"${3}"))
	}
	return out, nil
}
