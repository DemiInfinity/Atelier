package depupdates

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const goMod = `module example.com/app

go 1.22

require (
	github.com/a/one v1.2.3
	github.com/b/two v0.4.0 // indirect
	example.com/local v1.0.0
	golang.org/x/text v0.14.0 // a comment
)

replace example.com/local => ../local
`

func TestParseManifests(t *testing.T) {
	deps, err := ParseGoMod([]byte(goMod))
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 2 || deps[0].Name != "github.com/a/one" || deps[1].Name != "golang.org/x/text" || deps[1].Version != "v0.14.0" {
		t.Errorf("go deps = %+v", deps)
	}
	pj := `{"name":"x","dependencies":{"left-pad":"^1.2.0","lodash":"~4.17.1","react":"18.2.0","weird":"git+https://x","tagged":"latest","comp":">=1 <2"},
	"devDependencies":{"vitest":">=1.0.0","left-pad":"^9.0.0"}}`
	deps, err = ParsePackageJSON([]byte(pj))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Dep{}
	for _, d := range deps {
		got[d.Name] = d
	}
	if len(deps) != 4 || got["left-pad"].Op != "^" || got["left-pad"].Version != "v1.2.0" || got["lodash"].Op != "~" ||
		got["react"].Op != "" || got["vitest"].Op != ">=" {
		t.Errorf("npm deps = %+v", deps)
	}
}

func TestSemverHelpers(t *testing.T) {
	cases := []struct {
		op, base, v string
		want        bool
	}{
		{"^", "v1.2.3", "v1.9.0", true}, {"^", "v1.2.3", "v2.0.0", false}, {"^", "v0.2.3", "v0.2.9", true},
		{"^", "v0.2.3", "v0.3.0", false}, {"^", "v0.0.3", "v0.0.4", false}, {"~", "v1.2.3", "v1.2.9", true},
		{"~", "v1.2.3", "v1.3.0", false}, {"", "v1.2.3", "v1.2.4", false}, {">=", "v1.0.0", "v5.0.0", true}, {"^", "v1.2.3", "v1.2.2", false},
	}
	for _, c := range cases {
		if got := Satisfies(c.op, c.base, c.v); got != c.want {
			t.Errorf("Satisfies(%s%s, %s) = %v", c.op, c.base, c.v, got)
		}
	}
	if Kind("v1.2.3", "v2.0.0") != "major" || Kind("v1.2.3", "v1.3.0") != "minor" || Kind("v1.2.3", "v1.2.4") != "patch" {
		t.Error("Kind")
	}
	if l := Latest(Go, "v1.2.3", []string{"v1.2.4", "v1.3.0-rc.1", "v2.0.0+incompatible", "v1.10.0", "v0.9.0", "v1.11.0-0.20240101000000-abcdefabcdef"}); l != "v1.10.0" {
		t.Errorf("Go latest = %s", l)
	}
	if l := Latest(Npm, "v1.2.3", []string{"1.2.4", "2.0.0", "3.0.0-beta.1", "garbage"}); l != "v2.0.0" {
		t.Errorf("npm latest = %s", l)
	}
	if l := Latest(Npm, "v2.0.0", []string{"1.0.0", "2.0.0"}); l != "" {
		t.Errorf("up to date = %s", l)
	}
	if got := ParseGoProxy("https://atelier.test/go/,https://proxy.golang.org|direct,off,ftp://x"); len(got) != 2 || got[0] != "https://atelier.test/go" {
		t.Errorf("ParseGoProxy = %v", got)
	}
	if d := (Dep{Ecosystem: Npm, Op: "^"}); d.NewSpec("v2.1.0") != "^2.1.0" {
		t.Error("NewSpec npm")
	}
}

func TestEditManifests(t *testing.T) {
	out, err := UpdateGoMod([]byte(goMod), "golang.org/x/text", "v0.15.0")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "golang.org/x/text v0.15.0 // a comment") || !strings.Contains(s, "github.com/a/one v1.2.3") || !strings.Contains(s, "replace example.com/local => ../local") {
		t.Errorf("go.mod after edit:\n%s", s)
	}
	if _, err := UpdateGoMod([]byte(goMod), "nope.example/x", "v1.0.0"); err == nil {
		t.Error("editing a missing module succeeded")
	}
	pj := "{\n  \"dependencies\": {\n    \"left-pad\": \"^1.2.0\",\n    \"@scope/pkg\": \"~2.0.0\"\n  }\n}\n"
	out, err = UpdatePackageJSON([]byte(pj), "@scope/pkg", "~2.0.0", "~2.3.1")
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(pj, `"~2.0.0"`, `"~2.3.1"`, 1); string(out) != want {
		t.Errorf("package.json after edit:\n%s", out)
	}
	if _, err := UpdatePackageJSON([]byte(pj), "left-pad", "^9", "^10"); err == nil {
		t.Error("editing a missing spec succeeded")
	}
}

// fakeRegistries serves a Go module proxy, an npm registry and OSV.
func fakeRegistries(t *testing.T) (*httptest.Server, *[]string) {
	var osvBodies []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /go/github.com/a/one/@v/list", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("v1.2.3\nv1.2.4\nv1.3.0\nv2.0.0-rc.1\n"))
	})
	mux.HandleFunc("GET /go/golang.org/x/text/@v/list", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("GET /go/golang.org/x/text/@latest", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Version":"v0.14.1-0.20240101000000-abcdefabcdef"}`))
	})
	mux.HandleFunc("GET /npm/left-pad", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), "application/vnd.npm.install-v1+json") {
			t.Error("npm request without the abbreviated Accept header")
		}
		w.Write([]byte(`{"name":"left-pad","versions":{"1.2.0":{},"1.3.0":{},"2.0.0":{"deprecated":"don't"},"1.4.0":{"deprecated":""}}}`))
	})
	mux.HandleFunc("POST /osv", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Queries []struct {
				Package struct{ Name, Ecosystem string }
				Version string
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		type vuln struct {
			ID string `json:"id"`
		}
		var res struct {
			Results []map[string][]vuln `json:"results"`
		}
		for _, q := range req.Queries {
			osvBodies = append(osvBodies, q.Package.Ecosystem+":"+q.Package.Name+"@"+q.Version)
			m := map[string][]vuln{}
			if q.Package.Name == "github.com/a/one" && q.Version == "v1.2.3" {
				m["vulns"] = []vuln{{"GO-2024-0001"}, {"GHSA-aaaa-bbbb-cccc"}}
			}
			if q.Package.Name == "github.com/a/one" && q.Version == "v1.3.0" {
				m["vulns"] = []vuln{{"GHSA-aaaa-bbbb-cccc"}}
			}
			res.Results = append(res.Results, m)
		}
		json.NewEncoder(w).Encode(res)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &osvBodies
}

func TestCheckAgainstFakeRegistries(t *testing.T) {
	srv, osv := fakeRegistries(t)
	goDeps, _ := ParseGoMod([]byte(goMod))
	npmDeps, _ := ParsePackageJSON([]byte(`{"dependencies":{"left-pad":"^1.2.0","missing":"1.0.0"}}`))
	local := func(ctx context.Context, mod string) ([]string, bool) {
		if mod == "atelier.test/zoe/lib" {
			return []string{"v1.0.0", "v1.1.0"}, true
		}
		return nil, false
	}
	deps := append(append(goDeps, Dep{Ecosystem: Go, Name: "atelier.test/zoe/lib", Spec: "v1.0.0", Version: "v1.0.0"}), npmDeps...)
	c := Checker{
		Go:  GoProxy{URLs: []string{srv.URL + "/missing", srv.URL + "/go"}, Local: local},
		Npm: NpmRegistry{URL: srv.URL + "/npm"},
		OSV: &OSV{URL: srv.URL + "/osv"},
	}
	res, err := c.Check(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Result{}
	for _, r := range res {
		by[r.Name] = r
	}
	one := by["github.com/a/one"]
	if one.Latest != "v1.3.0" || one.Kind != "minor" || len(one.Vulns) != 2 || one.Fixes != 1 {
		t.Errorf("a/one = %+v", one)
	}
	if r := by["golang.org/x/text"]; r.Latest != "" || r.Err != nil {
		t.Errorf("x/text (pseudo-version only) = %+v", r)
	}
	if r := by["atelier.test/zoe/lib"]; r.Latest != "v1.1.0" {
		t.Errorf("local module = %+v", r)
	}
	lp := by["left-pad"]
	if lp.Latest != "v1.4.0" || !lp.InRange || lp.Kind != "minor" || lp.NewSpec(lp.Latest) != "^1.4.0" {
		t.Errorf("left-pad = %+v", lp)
	}
	if r := by["missing"]; r.Err == nil {
		t.Errorf("missing package = %+v", r)
	}
	joined := strings.Join(*osv, " ")
	if !strings.Contains(joined, "Go:github.com/a/one@v1.2.3") || !strings.Contains(joined, "npm:left-pad@1.2.0") {
		t.Errorf("OSV queries = %s", joined)
	}
}
