package depupdates

// Maven and Gradle versions: maven-metadata.xml from Maven Central, Google
// Maven (dl.google.com/android/maven2) and the Gradle Plugin Portal, and
// Gradle releases from services.gradle.org.

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"unicode"
)

const (
	DefaultMavenCentralURL = "https://repo1.maven.org/maven2"
	DefaultGoogleMavenURL  = "https://dl.google.com/android/maven2"
	DefaultPluginPortalURL = "https://plugins.gradle.org/m2"
	DefaultGradleURL       = "https://services.gradle.org"
)

// PluginMarker is the Maven coordinate of a Gradle plugin's marker artifact.
func PluginMarker(id string) string { return id + ":" + id + ".gradle.plugin" }

// IsPluginMarker reports whether coord is a plugin marker (and its plugin id).
func IsPluginMarker(coord string) (string, bool) {
	g, a, ok := strings.Cut(coord, ":")
	return g, ok && a == g+".gradle.plugin"
}

// googleGroups are published on Google Maven rather than Maven Central.
var googleGroups = []string{"androidx.", "android.arch.", "com.android.", "com.google.android.", "com.google.firebase",
	"com.google.gms", "com.google.mlkit", "com.google.ar", "com.google.testing.platform", "com.crashlytics."}

// IsGoogleGroup reports whether a group (or plugin id) lives on Google Maven.
func IsGoogleGroup(group string) bool {
	for _, p := range googleGroups {
		if strings.HasPrefix(group, p) || group+"." == p || group == strings.TrimSuffix(p, ".") {
			return true
		}
	}
	return false
}

// MavenRepos lists versions of Maven artifacts. Google groups are looked up
// on Google Maven first, plugin markers on the Gradle Plugin Portal, and
// everything else on Maven Central; the first repository that knows the
// artifact answers.
type MavenRepos struct {
	Central, Google, PluginPortal string // base URLs; empty: the defaults
	Client                        *http.Client
}

// MetadataURL is the maven-metadata.xml address of group:artifact under base
// (the group's dots become path segments, as every Maven layout needs).
func MetadataURL(base, coord string) (string, error) {
	g, a, ok := strings.Cut(coord, ":")
	if !ok || g == "" || a == "" || strings.Contains(a, ":") || strings.ContainsAny(coord, "/\\ ") || strings.Contains(coord, "..") {
		return "", fmt.Errorf("%q isn't a group:artifact coordinate", coord)
	}
	return strings.TrimRight(base, "/") + "/" + strings.ReplaceAll(g, ".", "/") + "/" + a + "/maven-metadata.xml", nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// reposFor is the lookup order for a coordinate.
func (m MavenRepos) reposFor(coord string) []string {
	central := orDefault(m.Central, DefaultMavenCentralURL)
	google := orDefault(m.Google, DefaultGoogleMavenURL)
	portal := orDefault(m.PluginPortal, DefaultPluginPortalURL)
	g, _, _ := strings.Cut(coord, ":")
	if id, ok := IsPluginMarker(coord); ok {
		if IsGoogleGroup(id) {
			return []string{google, portal}
		}
		return []string{portal, central}
	}
	if IsGoogleGroup(g) {
		return []string{google, central}
	}
	return []string{central}
}

type mavenMetadata struct {
	Versioning struct {
		Versions []string `xml:"versions>version"`
	} `xml:"versioning"`
}

// ParseMavenMetadata reads the version list of a maven-metadata.xml.
func ParseMavenMetadata(data []byte) ([]string, error) {
	var md mavenMetadata
	if err := xml.Unmarshal(data, &md); err != nil {
		return nil, err
	}
	out := md.Versioning.Versions[:0]
	for _, v := range md.Versioning.Versions {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out, nil
}

// Versions lists the versions of one group:artifact.
func (m MavenRepos) Versions(ctx context.Context, coord string) ([]string, error) {
	c := m.Client
	if c == nil {
		c = DefaultClient
	}
	var lastErr error = errNotFound
	for _, base := range m.reposFor(coord) {
		u, err := MetadataURL(base, coord)
		if err != nil {
			return nil, err
		}
		body, err := get(ctx, c, u, "application/xml", 8<<20)
		if err != nil {
			lastErr = err
			continue
		}
		return ParseMavenMetadata(body)
	}
	if errors.Is(lastErr, errNotFound) {
		return nil, fmt.Errorf("%s isn't on Maven Central, Google Maven or the Gradle Plugin Portal", coord)
	}
	return nil, lastErr
}

// GradleReleases lists Gradle versions from services.gradle.org.
type GradleReleases struct {
	URL    string // default DefaultGradleURL
	Client *http.Client
}

type gradleVersion struct {
	Version        string `json:"version"`
	Snapshot       bool   `json:"snapshot"`
	Nightly        bool   `json:"nightly"`
	ReleaseNightly bool   `json:"releaseNightly"`
	RcFor          string `json:"rcFor"`
	MilestoneFor   string `json:"milestoneFor"`
	Broken         bool   `json:"broken"`
}

// Versions lists released (not snapshot, nightly, RC, milestone or broken)
// Gradle versions.
func (g GradleReleases) Versions(ctx context.Context) ([]string, error) {
	c := g.Client
	if c == nil {
		c = DefaultClient
	}
	var all []gradleVersion
	if err := getJSON(ctx, c, strings.TrimRight(orDefault(g.URL, DefaultGradleURL), "/")+"/versions/all", "application/json", 8<<20, &all); err != nil {
		return nil, err
	}
	var out []string
	for _, v := range all {
		if v.Snapshot || v.Nightly || v.ReleaseNightly || v.RcFor != "" || v.MilestoneFor != "" || v.Broken || v.Version == "" {
			continue
		}
		out = append(out, v.Version)
	}
	return out, nil
}

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Checksum is the SHA-256 of a Gradle distribution (dist is bin or all).
func (g GradleReleases) Checksum(ctx context.Context, version, dist string) (string, error) {
	c := g.Client
	if c == nil {
		c = DefaultClient
	}
	if dist != "all" {
		dist = "bin"
	}
	body, err := get(ctx, c, fmt.Sprintf("%s/distributions/gradle-%s-%s.zip.sha256", strings.TrimRight(orDefault(g.URL, DefaultGradleURL), "/"), version, dist), "", 1024)
	if err != nil {
		return "", err
	}
	sum := strings.ToLower(strings.TrimSpace(string(body)))
	if !sha256Re.MatchString(sum) {
		return "", errors.New("the Gradle checksum isn't a SHA-256")
	}
	return sum, nil
}

// ---------- Maven versions ----------

// unstableRe matches pre-release qualifiers: alpha, beta, rc, cr, dev,
// snapshot, milestone (M1), preview, eap, ea, nightly.
var unstableRe = regexp.MustCompile(`(?i)(^|[.\-_+])(alpha|a|beta|b|rc|cr|dev|snapshot|milestone|m|preview|pre|eap|ea|nightly)[.\-_]?\d*([.\-_+]|$)`)

// neverRe are builds never offered, even to pre-release users.
var neverRe = regexp.MustCompile(`(?i)(snapshot|nightly|[.\-_+]dev([.\-_+]|\d|$))`)

// IsStableMaven reports whether a Maven version is a release.
func IsStableMaven(v string) bool {
	return !unstableRe.MatchString(v)
}

// mavenFlavor is a trailing non-numeric qualifier that marks a variant
// rather than a pre-release ("33.0.0-jre", "33.0.0-android", "6.4.0.Final").
func mavenFlavor(v string) string {
	i := strings.LastIndexAny(v, ".-")
	if i < 0 {
		return ""
	}
	q := strings.ToLower(v[i+1:])
	if q == "" || strings.IndexFunc(q, unicode.IsDigit) >= 0 || !IsStableMaven(v) {
		return ""
	}
	return q
}

type mvTok struct {
	num *big.Int
	q   string
}

func mavenTokens(v string) []mvTok {
	var out []mvTok
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		s := cur.String()
		cur.Reset()
		if n, ok := new(big.Int).SetString(s, 10); ok {
			out = append(out, mvTok{num: n})
		} else {
			out = append(out, mvTok{q: strings.ToLower(s)})
		}
	}
	prevDigit := false
	for i, r := range v {
		if r == '.' || r == '-' || r == '_' || r == '+' {
			flush()
			continue
		}
		d := unicode.IsDigit(r)
		if i > 0 && cur.Len() > 0 && d != prevDigit {
			flush()
		}
		cur.WriteRune(r)
		prevDigit = d
	}
	flush()
	return out
}

var qualRank = map[string]int{"alpha": 1, "a": 1, "beta": 2, "b": 2, "milestone": 3, "m": 3, "rc": 4, "cr": 4, "preview": 4, "pre": 4,
	"eap": 4, "ea": 4, "dev": 0, "snapshot": 5, "": 6, "ga": 6, "final": 6, "release": 6, "sp": 7}

func qRank(q string) int {
	if r, ok := qualRank[q]; ok {
		return r
	}
	return 6 // unknown qualifiers (jre, android) sort with releases, then by name
}

// CompareMaven orders two Maven versions roughly like Maven's
// ComparableVersion: numbers numerically, then pre-release qualifiers
// (alpha < beta < milestone < rc < snapshot < release < sp).
func CompareMaven(a, b string) int {
	ta, tb := mavenTokens(a), mavenTokens(b)
	for i := 0; i < len(ta) || i < len(tb); i++ {
		var x, y mvTok
		if i < len(ta) {
			x = ta[i]
		} else if i < len(tb) && tb[i].num != nil {
			x = mvTok{num: big.NewInt(0)}
		}
		if i < len(tb) {
			y = tb[i]
		} else if i < len(ta) && ta[i].num != nil {
			y = mvTok{num: big.NewInt(0)}
		}
		switch {
		case x.num != nil && y.num != nil:
			if c := x.num.Cmp(y.num); c != 0 {
				return c
			}
		case x.num != nil: // number vs qualifier: a number is newer
			return 1
		case y.num != nil:
			return -1
		default:
			rx, ry := qRank(x.q), qRank(y.q)
			if rx != ry {
				if rx < ry {
					return -1
				}
				return 1
			}
			if c := strings.Compare(x.q, y.q); c != 0 {
				return c
			}
		}
	}
	return 0
}

// mavenNums are the leading numeric components ("1.13.1-jre" -> 1, 13, 1).
func mavenNums(v string) []string {
	var out []string
	for _, t := range mavenTokens(v) {
		if t.num == nil {
			break
		}
		out = append(out, t.num.String())
	}
	return out
}

// KindMaven classifies a Maven version change as major, minor or patch.
func KindMaven(from, to string) string {
	a, b := mavenNums(from), mavenNums(to)
	at := func(s []string, i int) string {
		if i < len(s) {
			return s[i]
		}
		return "0"
	}
	switch {
	case at(a, 0) != at(b, 0):
		return "major"
	case at(a, 1) != at(b, 1):
		return "minor"
	}
	return "patch"
}

// LatestMaven picks the newest version above current. Pre-releases count
// only when current is one, and a variant qualifier (-jre, -android) must
// match current's.
func LatestMaven(current string, versions []string) string {
	pre := !IsStableMaven(current)
	flavor := mavenFlavor(current)
	best := ""
	for _, v := range versions {
		v = strings.TrimSpace(v)
		if v == "" || strings.ContainsAny(v, " $[]()*,") {
			continue
		}
		if !pre && !IsStableMaven(v) || neverRe.MatchString(v) {
			continue
		}
		if IsStableMaven(v) && mavenFlavor(v) != flavor {
			continue
		}
		if CompareMaven(v, current) > 0 && (best == "" || CompareMaven(v, best) > 0) {
			best = v
		}
	}
	return best
}

// intersect keeps the versions present in every list (catalog versions
// shared by several artifacts must exist for all of them).
func intersect(lists [][]string) []string {
	if len(lists) == 0 {
		return nil
	}
	count := map[string]int{}
	for _, l := range lists {
		seen := map[string]bool{}
		for _, v := range l {
			if !seen[v] {
				seen[v] = true
				count[v]++
			}
		}
	}
	var out []string
	for _, v := range lists[0] {
		if count[v] == len(lists) {
			out = append(out, v)
			count[v] = 0
		}
	}
	return out
}
