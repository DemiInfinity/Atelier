package depupdates

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const catalog = `# Gradle version catalog
[versions]
agp = "9.4.1"
kotlin = "2.2.10" # shared by the plugin and the stdlib
coreKtx = "1.15.0"
compileSdk = "36"
strict = { strictly = "1.0" }

[libraries]
androidx-core-ktx = { group = "androidx.core", name = "core-ktx", version.ref = "coreKtx" }
kotlin-stdlib = { module = "org.jetbrains.kotlin:kotlin-stdlib", version.ref = "kotlin" }
junit = { module = "junit:junit", version = "4.13.2" }
robolectric = "org.robolectric:robolectric:4.16"
gson = "com.google.code.gson:gson"
dynamic = { module = "com.example:dyn", version = "1.+" }
rich = { module = "com.example:rich", version = { strictly = "2.0" } }

[bundles]
test = ["junit", "robolectric"]

[plugins]
android-application = { id = "com.android.application", version.ref = "agp" }
kotlin-android = { id = "org.jetbrains.kotlin.android", version.ref = "kotlin" }
ksp = { id = "com.google.devtools.ksp", version = "2.2.10-2.0.2" }
hilt = "com.google.dagger.hilt.android:2.60.1"
`

func depsByName(t *testing.T, deps []Dep) map[string]Dep {
	t.Helper()
	m := map[string]Dep{}
	for _, d := range deps {
		if _, dup := m[d.Name]; dup {
			t.Errorf("duplicate dependency %s", d.Name)
		}
		m[d.Name] = d
	}
	return m
}

func TestVersionCatalog(t *testing.T) {
	deps, err := ParseVersionCatalog("gradle/libs.versions.toml", []byte(catalog))
	if err != nil {
		t.Fatal(err)
	}
	by := depsByName(t, deps)
	if len(deps) != 7 {
		t.Errorf("deps = %+v", deps)
	}
	if d := by["kotlin"]; d.Loc != "versions.kotlin" || d.Version != "2.2.10" || len(d.Coords) != 2 ||
		d.Coords[0] != "org.jetbrains.kotlin:kotlin-stdlib" || d.Coords[1] != PluginMarker("org.jetbrains.kotlin.android") {
		t.Errorf("kotlin = %+v", d)
	}
	if d := by["androidx.core:core-ktx"]; d.Loc != "versions.coreKtx" || d.Spec != "1.15.0" {
		t.Errorf("core-ktx = %+v", d)
	}
	if d := by["com.android.application"]; d.Loc != "versions.agp" || d.Coords[0] != "com.android.application:com.android.application.gradle.plugin" {
		t.Errorf("agp = %+v", d)
	}
	if d := by["junit:junit"]; d.Loc != "libraries.junit" || d.Version != "4.13.2" {
		t.Errorf("junit = %+v", d)
	}
	if d := by["org.robolectric:robolectric"]; d.Loc != "libraries.robolectric" || d.Version != "4.16" {
		t.Errorf("robolectric = %+v", d)
	}
	if d := by["com.google.devtools.ksp"]; d.Loc != "plugins.ksp" || d.Version != "2.2.10-2.0.2" {
		t.Errorf("ksp = %+v", d)
	}
	if d := by["com.google.dagger.hilt.android"]; d.Loc != "plugins.hilt" || d.Version != "2.60.1" {
		t.Errorf("hilt = %+v", d)
	}
	for _, n := range []string{"com.example:dyn", "com.example:rich", "com.google.code.gson:gson", "compileSdk", "strict"} {
		if _, ok := by[n]; ok {
			t.Errorf("%s shouldn't be offered", n)
		}
	}

	// Edits change only the version text.
	for _, c := range []struct{ loc, old, new, wantLine string }{
		{"versions.kotlin", "2.2.10", "2.4.21", `kotlin = "2.4.21" # shared by the plugin and the stdlib`},
		{"libraries.junit", "4.13.2", "4.13.3", `junit = { module = "junit:junit", version = "4.13.3" }`},
		{"libraries.robolectric", "4.16", "4.17", `robolectric = "org.robolectric:robolectric:4.17"`},
		{"plugins.ksp", "2.2.10-2.0.2", "2.3.13", `ksp = { id = "com.google.devtools.ksp", version = "2.3.13" }`},
		{"plugins.hilt", "2.60.1", "2.61", `hilt = "com.google.dagger.hilt.android:2.61"`},
	} {
		out, err := UpdateVersionCatalog([]byte(catalog), c.loc, c.old, c.new)
		if err != nil {
			t.Fatalf("%s: %v", c.loc, err)
		}
		if !strings.Contains(string(out), c.wantLine) {
			t.Errorf("%s: wanted %q in\n%s", c.loc, c.wantLine, out)
		}
		if diff := lineDiff(catalog, string(out)); diff != 1 {
			t.Errorf("%s changed %d lines", c.loc, diff)
		}
	}
	if _, err := UpdateVersionCatalog([]byte(catalog), "versions.kotlin", "9.9.9", "10.0.0"); err == nil {
		t.Error("editing with the wrong old version succeeded")
	}
	if _, err := UpdateVersionCatalog([]byte(catalog), "versions.nope", "1", "2"); err == nil {
		t.Error("editing a missing key succeeded")
	}
}

func lineDiff(a, b string) int {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	if len(la) != len(lb) {
		return -1
	}
	n := 0
	for i := range la {
		if la[i] != lb[i] {
			n++
		}
	}
	return n
}

const ktsBuild = `plugins {
    id("com.android.application") version "9.4.1" apply false
    id("org.jetbrains.kotlin.android") version "2.2.10" apply false
    kotlin("plugin.serialization") version "2.0.21" apply false
    id("com.google.devtools.ksp") version "2.2.10-2.0.2" apply false
    id("com.example.local")
}

dependencies {
    val composeBom = platform("androidx.compose:compose-bom:2026.06.01")
    implementation(composeBom)
    implementation("androidx.compose.ui:ui")
    implementation("androidx.core:core-ktx:1.15.0")
    implementation("androidx.core:core-ktx:1.15.0") // twice: both edited
    implementation("com.android.billingclient:billing-ktx:7.1.1")
    // implementation("com.example:commented:1.0.0")
    /* implementation("com.example:block:1.0.0") */
    implementation("com.example:interp:$someVersion")
    testImplementation("org.jetbrains.kotlinx:kotlinx-coroutines-test:1.10.2")
}
`

const groovyBuild = `buildscript {
    dependencies {
        classpath 'com.android.tools.build:gradle:8.13.2'
    }
}
plugins {
    id 'com.google.gms.google-services' version '4.4.2' apply false
}
dependencies {
    implementation 'com.squareup.okhttp3:okhttp:4.12.0'
    implementation "com.google.android.gms:play-services-location:21.3.0@aar"
    implementation "org.example:ranged:[1.0,2.0)"
    implementation "org.example:dynamic:1.+"
}
`

func TestGradleScripts(t *testing.T) {
	deps, err := ParseGradleBuild("android/app/build.gradle.kts", []byte(ktsBuild))
	if err != nil {
		t.Fatal(err)
	}
	by := depsByName(t, deps)
	if len(deps) != 8 {
		t.Errorf("kts deps = %+v", deps)
	}
	if d := by["org.jetbrains.kotlin.plugin.serialization"]; d.Loc != "plugin" || d.Version != "2.0.21" {
		t.Errorf("kotlin(...) plugin = %+v", d)
	}
	if d := by["androidx.compose:compose-bom"]; d.Version != "2026.06.01" || d.Loc != "dep" {
		t.Errorf("bom = %+v", d)
	}
	for _, n := range []string{"com.example:commented", "com.example:block", "com.example:interp", "com.example.local", "androidx.compose.ui:ui"} {
		if _, ok := by[n]; ok {
			t.Errorf("%s shouldn't be offered", n)
		}
	}
	out, err := UpdateGradleBuild([]byte(ktsBuild), by["androidx.core:core-ktx"], "1.19.1")
	if err != nil || strings.Count(string(out), `"androidx.core:core-ktx:1.19.1"`) != 2 || lineDiff(ktsBuild, string(out)) != 2 {
		t.Errorf("core-ktx edit: %v\n%s", err, out)
	}
	out, err = UpdateGradleBuild([]byte(ktsBuild), by["com.android.application"], "9.5.1")
	if err != nil || !strings.Contains(string(out), `id("com.android.application") version "9.5.1" apply false`) || lineDiff(ktsBuild, string(out)) != 1 {
		t.Errorf("plugin edit: %v\n%s", err, out)
	}
	out, err = UpdateGradleBuild([]byte(ktsBuild), by["org.jetbrains.kotlin.plugin.serialization"], "2.4.21")
	if err != nil || !strings.Contains(string(out), `kotlin("plugin.serialization") version "2.4.21"`) {
		t.Errorf("kotlin() plugin edit: %v\n%s", err, out)
	}
	stale := by["androidx.core:core-ktx"]
	stale.Spec = "1.0.0"
	if _, err := UpdateGradleBuild([]byte(ktsBuild), stale, "1.19.1"); err == nil {
		t.Error("editing a version that isn't there succeeded")
	}

	deps, _ = ParseGradleBuild("build.gradle", []byte(groovyBuild))
	by = depsByName(t, deps)
	if len(deps) != 4 || by["com.google.gms.google-services"].Version != "4.4.2" || by["com.android.tools.build:gradle"].Version != "8.13.2" ||
		by["com.google.android.gms:play-services-location"].Version != "21.3.0" {
		t.Errorf("groovy deps = %+v", deps)
	}
	out, err = UpdateGradleBuild([]byte(groovyBuild), by["com.google.android.gms:play-services-location"], "21.4.0")
	if err != nil || !strings.Contains(string(out), `"com.google.android.gms:play-services-location:21.4.0@aar"`) {
		t.Errorf("groovy @aar edit: %v\n%s", err, out)
	}
	out, err = UpdateGradleBuild([]byte(groovyBuild), by["com.google.gms.google-services"], "4.4.3")
	if err != nil || !strings.Contains(string(out), `id 'com.google.gms.google-services' version '4.4.3' apply false`) {
		t.Errorf("groovy plugin edit: %v\n%s", err, out)
	}
}

const wrapperProps = "#Sat Oct 17 23:42:33 BST 2026\ndistributionBase=GRADLE_USER_HOME\ndistributionPath=wrapper/dists\n" +
	"distributionUrl=https\\://services.gradle.org/distributions/gradle-9.8.0-bin.zip\nzipStoreBase=GRADLE_USER_HOME\nzipStorePath=wrapper/dists\n"

func TestGradleWrapper(t *testing.T) {
	deps, err := ParseGradleWrapper("android/gradle/wrapper/gradle-wrapper.properties", []byte(wrapperProps))
	if err != nil || len(deps) != 1 || deps[0].Version != "9.8.0" || deps[0].Dist != "bin" || deps[0].PinsChecksum || deps[0].Ecosystem != Gradle {
		t.Fatalf("wrapper = %+v, %v", deps, err)
	}
	out, err := UpdateGradleWrapper([]byte(wrapperProps), "9.8.0", "9.9.0", "")
	if err != nil || string(out) != strings.Replace(wrapperProps, "gradle-9.8.0-bin.zip", "gradle-9.9.0-bin.zip", 1) {
		t.Errorf("wrapper edit: %v\n%s", err, out)
	}
	sum := strings.Repeat("ab", 32)
	pinned := strings.Replace(wrapperProps, "-bin.zip", "-all.zip", 1) + "distributionSha256Sum=" + strings.Repeat("0", 64) + "\n"
	deps, _ = ParseGradleWrapper("gradle/wrapper/gradle-wrapper.properties", []byte(pinned))
	if !deps[0].PinsChecksum || deps[0].Dist != "all" {
		t.Errorf("pinned wrapper = %+v", deps)
	}
	if _, err := UpdateGradleWrapper([]byte(pinned), "9.8.0", "9.9.0", ""); err == nil {
		t.Error("updating a pinned wrapper without a checksum succeeded")
	}
	out, err = UpdateGradleWrapper([]byte(pinned), "9.8.0", "9.9.0", sum)
	if err != nil || !strings.Contains(string(out), "gradle-9.9.0-all.zip") || !strings.Contains(string(out), "distributionSha256Sum="+sum+"\n") {
		t.Errorf("pinned edit: %v\n%s", err, out)
	}
	if _, err := ParseGradleWrapper("x", []byte("distributionUrl=https\\://example.com/custom.zip\n")); err == nil {
		t.Error("a custom distribution was parsed")
	}
}

func TestMavenVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.10.0", "1.9.0", 1}, {"1.0", "1.0.0", 0}, {"1.0", "1.0-rc1", 1}, {"1.0-beta1", "1.0-alpha9", 1},
		{"2.0.0-RC1", "2.0.0-Beta2", 1}, {"33.0.0-jre", "32.1.3-jre", 1}, {"2026.09.00", "2026.06.01", 1},
		{"2.3.13", "2.2.10-2.0.2", 1}, {"4.17", "4.16", 1}, {"1.0.0-SNAPSHOT", "1.0.0", -1},
	} {
		if got := CompareMaven(c.a, c.b); got != c.want {
			t.Errorf("CompareMaven(%s, %s) = %d", c.a, c.b, got)
		}
	}
	for v, stable := range map[string]bool{
		"1.13.1": true, "33.0.0-jre": true, "6.4.0.Final": true, "2.2.10-2.0.2": true, "2026.09.00": true,
		"1.0.0-alpha05": false, "2.0.0-RC1": false, "1.9.0-Beta": false, "1.0-SNAPSHOT": false, "1.6.0-M1": false,
		"2.1.0-dev-1234": false, "8.6.0-rc01": false, "1.0.0-milestone-2": false, "3.0.0-preview1": false, "1.0.0.Beta2": false,
	} {
		if IsStableMaven(v) != stable {
			t.Errorf("IsStableMaven(%s) = %v", v, !stable)
		}
	}
	vs := []string{"1.15.0", "1.16.0-alpha01", "1.17.0", "1.19.1", "1.20.0-beta02", "1.20.0-rc01", "1.20.0-SNAPSHOT", "1.18.0"}
	if l := LatestMaven("1.15.0", vs); l != "1.19.1" {
		t.Errorf("stable latest = %s", l)
	}
	if l := LatestMaven("1.20.0-alpha01", vs); l != "1.20.0-rc01" {
		t.Errorf("pre-release latest = %s", l)
	}
	if l := LatestMaven("32.1.3-jre", []string{"33.0.0-android", "33.0.0-jre", "33.1.0-android"}); l != "33.0.0-jre" {
		t.Errorf("flavoured latest = %s", l)
	}
	if l := LatestMaven("1.19.1", vs); l != "" {
		t.Errorf("up to date = %s", l)
	}
	if k := KindMaven("2.2.10-2.0.2", "2.3.13"); k != "minor" {
		t.Errorf("ksp kind = %s", k)
	}
	if k := KindMaven("7.1.1", "9.1.0"); k != "major" {
		t.Errorf("billing kind = %s", k)
	}
	if k := KindMaven("9.8.0", "9.8.1"); k != "patch" {
		t.Errorf("gradle kind = %s", k)
	}
	if k := KindMaven("4.16", "4.17"); k != "minor" {
		t.Errorf("robolectric kind = %s", k)
	}
}

func TestMavenURLs(t *testing.T) {
	u, err := MetadataURL(DefaultGoogleMavenURL, "androidx.core:core-ktx")
	if err != nil || u != "https://dl.google.com/android/maven2/androidx/core/core-ktx/maven-metadata.xml" {
		t.Errorf("Google Maven URL = %s, %v", u, err)
	}
	u, _ = MetadataURL(DefaultMavenCentralURL+"/", "org.jetbrains.kotlinx:kotlinx-coroutines-test")
	if u != "https://repo1.maven.org/maven2/org/jetbrains/kotlinx/kotlinx-coroutines-test/maven-metadata.xml" {
		t.Errorf("Central URL = %s", u)
	}
	for _, bad := range []string{"noartifact", "a:b:c", "../x:y", "a/b:c", ":x"} {
		if _, err := MetadataURL("https://x", bad); err == nil {
			t.Errorf("MetadataURL(%q) succeeded", bad)
		}
	}
	m := MavenRepos{Central: "C", Google: "G", PluginPortal: "P"}
	for coord, want := range map[string]string{
		"androidx.core:core-ktx":                        "G,C",
		"com.android.billingclient:billing-ktx":         "G,C",
		"com.google.android.gms:play-services-location": "G,C",
		"com.squareup.okhttp3:okhttp":                   "C",
		PluginMarker("com.android.application"):         "G,P",
		PluginMarker("com.google.devtools.ksp"):         "P,C",
	} {
		if got := strings.Join(m.reposFor(coord), ","); got != want {
			t.Errorf("reposFor(%s) = %s, want %s", coord, got, want)
		}
	}
}

// fakeMaven serves Maven Central, Google Maven, the Plugin Portal, Gradle's
// services and OSV under one test server, recording every request.
func fakeMaven(t *testing.T) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	var hits []string
	meta := func(vs ...string) string {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><metadata><groupId>x</groupId><versioning><versions>`)
		for _, v := range vs {
			b.WriteString("<version>" + v + "</version>")
		}
		b.WriteString("</versions></versioning></metadata>")
		return b.String()
	}
	files := map[string]string{
		"/google/androidx/core/core-ktx/maven-metadata.xml":                                                  meta("1.15.0", "1.17.0", "1.19.1", "1.20.0-alpha01"),
		"/google/com/android/application/com.android.application.gradle.plugin/maven-metadata.xml":           meta("9.4.1", "9.5.1", "9.6.0-alpha02"),
		"/google/com/android/billingclient/billing-ktx/maven-metadata.xml":                                   meta("7.1.1", "8.0.0", "9.1.0"),
		"/central/org/jetbrains/kotlin/kotlin-stdlib/maven-metadata.xml":                                     meta("2.2.10", "2.4.20", "2.4.21", "2.5.0-Beta1"),
		"/portal/org/jetbrains/kotlin/android/org.jetbrains.kotlin.android.gradle.plugin/maven-metadata.xml": meta("2.2.10", "2.4.20"),
		"/central/com/fasterxml/jackson/core/jackson-databind/maven-metadata.xml":                            meta("2.15.0", "2.15.4"),
		"/portal/com/google/devtools/ksp/com.google.devtools.ksp.gradle.plugin/maven-metadata.xml":           meta("2.2.10-2.0.2", "2.3.13"),
		"/gradle/distributions/gradle-9.9.0-all.zip.sha256":                                                  strings.Repeat("cd", 32),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /gradle/versions/all", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"version": "9.10.0-20261101000000+0000", "nightly": true},
			{"version": "9.10.0-rc-1", "rcFor": "9.10.0"},
			{"version": "9.10.0-milestone-1", "milestoneFor": "9.10.0"},
			{"version": "9.9.1", "broken": true},
			{"version": "9.9.0"}, {"version": "9.8.0"},
		})
	})
	mux.HandleFunc("POST /osv", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Queries []struct {
				Package struct{ Name, Ecosystem string }
				Version string
			}
		}
		json.NewDecoder(r.Body).Decode(&req)
		type vuln struct {
			ID string `json:"id"`
		}
		var res struct {
			Results []map[string][]vuln `json:"results"`
		}
		for _, q := range req.Queries {
			mu.Lock()
			hits = append(hits, "osv "+q.Package.Ecosystem+":"+q.Package.Name+"@"+q.Version)
			mu.Unlock()
			m := map[string][]vuln{}
			if q.Package.Ecosystem == "Maven" && q.Package.Name == "com.fasterxml.jackson.core:jackson-databind" && q.Version == "2.15.0" {
				m["vulns"] = []vuln{{"GHSA-maven-0001"}}
			}
			res.Results = append(res.Results, m)
		}
		json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		if body, ok := files[r.URL.Path]; ok {
			w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestCheckGradleAgainstFakeRepositories(t *testing.T) {
	srv, hits := fakeMaven(t)
	deps := []Dep{
		{Ecosystem: Maven, Name: "androidx.core:core-ktx", Spec: "1.15.0", Version: "1.15.0", Coords: []string{"androidx.core:core-ktx"}, Loc: "dep"},
		{Ecosystem: Maven, Name: "kotlin", Spec: "2.2.10", Version: "2.2.10", Loc: "versions.kotlin",
			Coords: []string{"org.jetbrains.kotlin:kotlin-stdlib", PluginMarker("org.jetbrains.kotlin.android")}},
		{Ecosystem: Maven, Name: "com.android.application", Spec: "9.4.1", Version: "9.4.1", Coords: []string{PluginMarker("com.android.application")}, Loc: "plugin"},
		{Ecosystem: Maven, Name: "com.google.devtools.ksp", Spec: "2.2.10-2.0.2", Version: "2.2.10-2.0.2", Coords: []string{PluginMarker("com.google.devtools.ksp")}, Loc: "plugin"},
		{Ecosystem: Maven, Name: "com.android.billingclient:billing-ktx", Spec: "7.1.1", Version: "7.1.1", Coords: []string{"com.android.billingclient:billing-ktx"}, Loc: "dep"},
		{Ecosystem: Maven, Name: "com.fasterxml.jackson.core:jackson-databind", Spec: "2.15.0", Version: "2.15.0", Coords: []string{"com.fasterxml.jackson.core:jackson-databind"}, Loc: "dep"},
		{Ecosystem: Maven, Name: "com.example:missing", Spec: "1.0", Version: "1.0", Coords: []string{"com.example:missing"}, Loc: "dep"},
		{Ecosystem: Gradle, Name: "gradle", Spec: "9.8.0", Version: "9.8.0", Loc: "wrapper", Dist: "all", PinsChecksum: true},
	}
	c := Checker{
		Maven:  MavenRepos{Central: srv.URL + "/central", Google: srv.URL + "/google", PluginPortal: srv.URL + "/portal"},
		Gradle: GradleReleases{URL: srv.URL + "/gradle"},
		OSV:    &OSV{URL: srv.URL + "/osv"},
	}
	res, err := c.Check(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Result{}
	for _, r := range res {
		by[r.Name] = r
	}
	if r := by["androidx.core:core-ktx"]; r.Latest != "1.19.1" || r.Kind != "minor" || r.Err != nil {
		t.Errorf("core-ktx = %+v", r)
	}
	// A shared catalog version only moves to a version every artifact has.
	if r := by["kotlin"]; r.Latest != "2.4.20" || r.Kind != "minor" {
		t.Errorf("kotlin = %+v", r)
	}
	if r := by["com.android.application"]; r.Latest != "9.5.1" {
		t.Errorf("agp = %+v", r)
	}
	if r := by["com.google.devtools.ksp"]; r.Latest != "2.3.13" {
		t.Errorf("ksp = %+v", r)
	}
	if r := by["com.android.billingclient:billing-ktx"]; r.Latest != "9.1.0" || r.Kind != "major" {
		t.Errorf("billing = %+v", r)
	}
	if r := by["com.fasterxml.jackson.core:jackson-databind"]; r.Latest != "2.15.4" || len(r.Vulns) != 1 || r.Vulns[0] != "GHSA-maven-0001" || r.Fixes != 1 {
		t.Errorf("jackson = %+v", r)
	}
	if r := by["com.example:missing"]; r.Err == nil {
		t.Errorf("missing = %+v", r)
	}
	if r := by["gradle"]; r.Latest != "9.9.0" || r.Kind != "minor" || r.Checksum != strings.Repeat("cd", 32) {
		t.Errorf("gradle = %+v", r)
	}
	joined := strings.Join(*hits, "\n")
	for _, want := range []string{
		"/google/androidx/core/core-ktx/maven-metadata.xml",
		"osv Maven:com.fasterxml.jackson.core:jackson-databind@2.15.0",
		"osv Maven:com.fasterxml.jackson.core:jackson-databind@2.15.4",
		"osv Maven:org.jetbrains.kotlin:kotlin-stdlib@2.2.10",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing request %s in\n%s", want, joined)
		}
	}
	for _, never := range []string{"/central/androidx/", "osv Maven:com.android.application:com.android.application.gradle.plugin", "osv npm:gradle"} {
		if strings.Contains(joined, never) {
			t.Errorf("unexpected request %s in\n%s", never, joined)
		}
	}
}

func TestDiscover(t *testing.T) {
	paths := []string{
		"go.mod", "README.md", "tools/go.mod", "tools/testdata/go.mod", "vendor/x/go.mod",
		"package.json", "mosaicplus-web/package.json", "mosaicplus-web/node_modules/svelte/package.json",
		"mosaicplus-web/dist/package.json", ".github/package.json",
		"android/settings.gradle.kts", "android/build.gradle.kts", "android/app/build.gradle.kts",
		"android/app/build/generated/build.gradle", "android/.gradle/x/build.gradle",
		"android/gradle/wrapper/gradle-wrapper.properties", "android/gradle/wrapper/gradle-wrapper.jar",
		"android/gradle/libs.versions.toml", "other.versions.toml",
		"standalone/build.gradle",
	}
	got := map[string]Manifest{}
	for _, m := range Discover(paths) {
		got[m.Path] = m
	}
	want := map[string][2]string{
		"go.mod": {"gomod", ""}, "tools/go.mod": {"gomod", "tools"},
		"package.json": {"npm", ""}, "mosaicplus-web/package.json": {"npm", "mosaicplus-web"},
		"android/settings.gradle.kts": {"gradle-build", "android"}, "android/build.gradle.kts": {"gradle-build", "android"},
		"android/app/build.gradle.kts":                     {"gradle-build", "android"},
		"android/gradle/wrapper/gradle-wrapper.properties": {"gradle-wrapper", "android"},
		"android/gradle/libs.versions.toml":                {"gradle-catalog", "android"},
		"standalone/build.gradle":                          {"gradle-build", "standalone"},
	}
	if len(got) != len(want) {
		t.Errorf("found %d manifests: %+v", len(got), got)
	}
	for p, w := range want {
		if m := got[p]; m.Kind != w[0] || m.Root != w[1] {
			t.Errorf("%s = %+v, want %v", p, m, w)
		}
	}
	old := MaxManifests
	MaxManifests = 2
	defer func() { MaxManifests = old }()
	if ms := Discover(paths); len(ms) != 2 || ms[0].Path != "go.mod" || ms[1].Path != "package.json" {
		t.Errorf("capped = %+v", ms)
	}
}

func TestWorkspaces(t *testing.T) {
	if g := Workspaces([]byte(`{"workspaces":["packages/*","apps/**","!packages/skip"]}`), nil); strings.Join(g, ",") != "packages/*,apps/**,!packages/skip" {
		t.Errorf("npm workspaces = %v", g)
	}
	if g := Workspaces([]byte(`{"workspaces":{"packages":["libs/*"],"nohoist":["**/x"]}}`), nil); strings.Join(g, ",") != "libs/*" {
		t.Errorf("yarn workspaces = %v", g)
	}
	pnpm := "packages:\n  - 'packages/*'\n  - \"web\" # the site\n  - '!**/test/**'\ncatalog:\n  react: ^18.3.1\n"
	if g := Workspaces(nil, []byte(pnpm)); strings.Join(g, ",") != "packages/*,web,!**/test/**" {
		t.Errorf("pnpm workspaces = %v", g)
	}
	roots := map[string][]string{"": {"packages/*", "apps/**", "!packages/skip"}, "site": {"web"}}
	for dir, want := range map[string]string{"packages/ui": "", "apps/a/b": "", "site/web": "site"} {
		if got, ok := WorkspaceRoot(dir, roots); !ok || got != want {
			t.Errorf("WorkspaceRoot(%s) = %q, %v", dir, got, ok)
		}
	}
	for _, dir := range []string{"packages/skip", "packages/ui/nested", "elsewhere"} {
		if _, ok := WorkspaceRoot(dir, roots); ok {
			t.Errorf("%s is in a workspace", dir)
		}
	}
	var l Local
	l.AddManifest(Manifest{Path: "packages/ui/package.json", Kind: "npm"}, []byte(`{"name":"@me/ui","version":"1.0.0"}`))
	l.AddManifest(Manifest{Path: "tools/go.mod", Kind: "gomod"}, []byte("// tools\nmodule example.com/app/tools\n\ngo 1.22\n"))
	app, _ := ParsePackageJSON([]byte(`{"dependencies":{"@me/ui":"^1.0.0","left-pad":"^1.0.0","@me/core":"workspace:*"}}`))
	var kept []string
	for _, d := range app {
		if !l.Has(d) {
			kept = append(kept, d.Name)
		}
	}
	if strings.Join(kept, ",") != "left-pad" {
		t.Errorf("kept = %v", kept)
	}
	if !l.Has(Dep{Ecosystem: Go, Name: "example.com/app/tools"}) || l.Has(Dep{Ecosystem: Go, Name: "example.com/other"}) {
		t.Error("Local Go modules")
	}
}

func TestPlanGroupingAndBranches(t *testing.T) {
	res := func(eco, name, manifest, root, from, to, kind string) Result {
		d := Dep{Ecosystem: eco, Name: name, Manifest: manifest, Root: root, Spec: from, Version: from}
		if eco == Maven {
			d.Coords, d.Loc = []string{name}, "dep"
		}
		if eco == Npm {
			d.Op, d.Spec = "^", strings.TrimPrefix(from, "v")
		}
		return Result{Dep: d, Latest: to, Kind: kind}
	}
	results := []Result{
		res(Npm, "svelte", "mosaicplus-web/package.json", "mosaicplus-web", "v5.57.0", "v5.57.3", "patch"),
		res(Npm, "vite", "mosaicplus-web/package.json", "mosaicplus-web", "v8.2.2", "v8.3.7", "minor"),
		res(Npm, "left-pad", "package.json", "", "v1.2.0", "v2.0.0", "major"),
		res(Maven, "androidx.core:core-ktx", "android/app/build.gradle.kts", "android", "1.15.0", "1.19.1", "minor"),
		res(Maven, "org.robolectric:robolectric", "android/app/build.gradle.kts", "android", "4.16", "4.17", "minor"),
		res(Maven, "com.android.billingclient:billing-ktx", "android/app/build.gradle.kts", "android", "7.1.1", "9.1.0", "major"),
		{Dep: Dep{Ecosystem: Gradle, Name: "gradle", Manifest: "android/gradle/wrapper/gradle-wrapper.properties", Root: "android", Spec: "9.8.0", Version: "9.8.0", Loc: "wrapper"}, Latest: "9.9.0", Kind: "minor"},
		res(Go, "example.com/one", "tools/go.mod", "tools", "v1.2.3", "v1.3.0", "minor"),
		{Dep: Dep{Ecosystem: Npm, Name: "broken"}, Latest: "v1.0.0", Err: errors.New("x")},
		{Dep: Dep{Ecosystem: Npm, Name: "current"}},
	}
	byBranch := func(prs []PR) map[string]PR {
		m := map[string]PR{}
		for _, p := range prs {
			m[p.Branch] = p
		}
		return m
	}
	// Without grouping: one per dependency, paths in branches and titles.
	prs := Plan(results, PlanOptions{})
	m := byBranch(prs)
	if len(prs) != 8 {
		t.Errorf("ungrouped = %d PRs", len(prs))
	}
	for branch, title := range map[string]string{
		"atelier-updates/npm@mosaicplus-web/svelte/5.57.3":                 "Bump svelte from 5.57.0 to ^5.57.3 in mosaicplus-web",
		"atelier-updates/npm/left-pad/2.0.0":                               "Bump left-pad from 1.2.0 to ^2.0.0",
		"atelier-updates/gradle@android+app/androidx.core-core-ktx/1.19.1": "Bump androidx.core:core-ktx from 1.15.0 to 1.19.1 in android/app",
		"atelier-updates/gradle@android/gradle/9.9.0":                      "Bump gradle from 9.8.0 to 9.9.0 in android",
		"atelier-updates/go@tools/example.com-one/1.3.0":                   "Bump example.com/one from v1.2.3 to v1.3.0 in tools",
	} {
		if p, ok := m[branch]; !ok || p.Title() != title {
			t.Errorf("%s: %+v (title %q)", branch, ok, p.Title())
		}
	}
	// With grouping: minor and patch per manager and folder; majors alone.
	prs = Plan(results, PlanOptions{Group: true})
	var groups []PR
	singles := map[string]bool{}
	for _, p := range prs {
		if p.Group {
			groups = append(groups, p)
		} else {
			singles[p.Updates[0].Name] = true
		}
	}
	if len(groups) != 2 || len(prs) != 5 || !singles["left-pad"] || !singles["com.android.billingclient:billing-ktx"] || !singles["example.com/one"] {
		t.Fatalf("grouped = %+v", prs)
	}
	for _, g := range groups {
		switch g.Manager {
		case Npm:
			if g.Root != "mosaicplus-web" || len(g.Updates) != 2 || !strings.HasPrefix(g.Branch, "atelier-updates/npm@mosaicplus-web/_group/") ||
				g.Title() != "Update minor and patch dependencies (npm) in mosaicplus-web" {
				t.Errorf("npm group = %+v %q", g, g.Title())
			}
		case Gradle:
			if g.Root != "android" || len(g.Updates) != 3 || g.Prefix != "atelier-updates/gradle@android/_group/" {
				t.Errorf("gradle group = %+v", g)
			}
			body := g.Body(BodyOptions{Lockfiles: true})
			for _, want := range []string{"| Package | From | To | Type | Advisories | Links |", "| `androidx.core:core-ktx` in `android/app/build.gradle.kts` | `1.15.0` | `1.19.1` | minor | – | [Google Maven](https://maven.google.com/web/index.html#androidx.core:core-ktx) |",
				"[Maven Central](https://central.sonatype.com/artifact/org.robolectric/robolectric)", "`gradle` in `android/gradle/wrapper/gradle-wrapper.properties`", "atelier/update-lockfiles"} {
				if !strings.Contains(body, want) {
					t.Errorf("gradle body lacks %q:\n%s", want, body)
				}
			}
		}
	}
	// The group branch depends on its contents, so the same set plans the same branch.
	again := Plan(results, PlanOptions{Group: true})
	if byBranch(again)[groups[0].Branch].Branch == "" {
		t.Error("group branch isn't stable")
	}
	results[1].Latest = "v8.4.0"
	if _, same := byBranch(Plan(results, PlanOptions{Group: true}))[groups[0].Branch]; same && groups[0].Manager == Npm {
		t.Error("group branch didn't change with its contents")
	}
	// Skip and Wanted filter; a group of one is a plain PR.
	prs = Plan(results, PlanOptions{Group: true, Skip: func(r Result) bool { return r.Name == "vite" }, Wanted: func(r Result) bool { return r.Manager() == Npm }})
	if len(prs) != 2 || prs[0].Group || prs[1].Group {
		t.Errorf("filtered = %+v", prs)
	}
	// Advisories link to OSV.
	r := results[2]
	r.Vulns, r.Fixes = []string{"GHSA-xxxx-yyyy-zzzz"}, 1
	body := Plan([]Result{r}, PlanOptions{})[0].Body(BodyOptions{})
	if !strings.Contains(body, "[GHSA-xxxx-yyyy-zzzz](https://osv.dev/vulnerability/GHSA-xxxx-yyyy-zzzz) (fixes 1)") || !strings.Contains(body, "package-lock.json") {
		t.Errorf("body:\n%s", body)
	}
	if IsUpdateBranch("main") || IsUpdateBranch("atelier-updates/") || !IsUpdateBranch("atelier-updates/npm/x/1.0.0") || IsUpdateBranch("atelier-updates/../main") {
		t.Error("IsUpdateBranch")
	}
}

func TestLockfileCommands(t *testing.T) {
	files := []string{
		"package.json", "package-lock.json", "web/package.json", "web/pnpm-lock.yaml", "y/package.json", "y/yarn.lock", "y/.yarnrc.yml",
		"orphan/package-lock.json", "node_modules/x/package.json", "node_modules/x/package-lock.json",
		"go.mod", "go.sum", "tools/go.mod",
		"android/gradlew", "android/settings.gradle.kts", "android/app/gradle.lockfile", "android/gradle.lockfile", "android/gradle/verification-metadata.xml",
		"nowrapper/gradle.lockfile",
	}
	var got []string
	for _, c := range LockfileCommands(files) {
		got = append(got, c.Dir+": "+strings.Join(c.Argv, " "))
	}
	want := []string{
		"android: ./gradlew --no-daemon dependencies --write-locks",
		"android: ./gradlew --no-daemon --write-verification-metadata sha256 help",
		": go mod tidy",
		": npm install --package-lock-only --ignore-scripts --no-audit --no-fund",
		"tools: go mod tidy",
		"web: pnpm install --lockfile-only --ignore-scripts",
		"y: yarn install --mode update-lockfile",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands:\n%s", strings.Join(got, "\n"))
	}
	for p, ok := range map[string]bool{"package-lock.json": true, "web/pnpm-lock.yaml": true, "go.sum": true, "tools/go.mod": true,
		"android/app/gradle.lockfile": true, "android/gradle/verification-metadata.xml": true, "gradle/dependency-locks/x.lockfile": true,
		"package.json": false, "android/gradlew": false, "android/gradle/wrapper/gradle-wrapper.jar": false, "src/main.go": false, ".atelier/workflows/x.yml": false} {
		if IsLockfile(p) != ok {
			t.Errorf("IsLockfile(%s) = %v", p, !ok)
		}
	}
}

func TestLockfilePushPolicy(t *testing.T) {
	const old, new = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	zero := strings.Repeat("0", 40)
	base := func() LockfilePush {
		return LockfilePush{
			Branch: "atelier-updates/npm@web/_group/abc", BotPR: true, Enabled: true,
			Updates:   []RefUpdate{{Old: old, New: new, Ref: "refs/heads/atelier-updates/npm@web/_group/abc"}},
			FastFwd:   func(a, b string) (bool, error) { return true, nil },
			Merges:    func(a, b string) (bool, error) { return false, nil },
			ChangedIn: func(a, b string) ([]string, error) { return []string{"web/package-lock.json"}, nil },
		}
	}
	if msg, err := LockfilePushAllowed(base()); msg != "" || err != nil {
		t.Fatalf("allowed push refused: %q %v", msg, err)
	}
	cases := map[string]func(*LockfilePush){
		"off":            func(p *LockfilePush) { p.Enabled = false },
		"main":           func(p *LockfilePush) { p.Branch, p.Updates[0].Ref = "main", "refs/heads/main" },
		"no bot PR":      func(p *LockfilePush) { p.BotPR = false },
		"other branch":   func(p *LockfilePush) { p.Updates[0].Ref = "refs/heads/atelier-updates/npm/other/1.0.0" },
		"default branch": func(p *LockfilePush) { p.Updates[0].Ref = "refs/heads/main" },
		"tag":            func(p *LockfilePush) { p.Updates[0].Ref = "refs/tags/v1" },
		"two refs": func(p *LockfilePush) {
			p.Updates = append(p.Updates, RefUpdate{Old: zero, New: new, Ref: "refs/heads/x"})
		},
		"delete": func(p *LockfilePush) { p.Updates[0].New = zero },
		"create": func(p *LockfilePush) { p.Updates[0].Old = zero },
		"force":  func(p *LockfilePush) { p.FastFwd = func(a, b string) (bool, error) { return false, nil } },
		"merge":  func(p *LockfilePush) { p.Merges = func(a, b string) (bool, error) { return true, nil } },
		"source file": func(p *LockfilePush) {
			p.ChangedIn = func(a, b string) ([]string, error) { return []string{"web/package-lock.json", "web/src/app.js"}, nil }
		},
		"workflow": func(p *LockfilePush) {
			p.ChangedIn = func(a, b string) ([]string, error) { return []string{".atelier/workflows/ci.yml"}, nil }
		},
		"wrapper jar": func(p *LockfilePush) {
			p.ChangedIn = func(a, b string) ([]string, error) { return []string{"android/gradle/wrapper/gradle-wrapper.jar"}, nil }
		},
	}
	for name, mutate := range cases {
		p := base()
		mutate(&p)
		if msg, err := LockfilePushAllowed(p); msg == "" || err != nil {
			t.Errorf("%s: allowed (%v)", name, err)
		}
	}
}
