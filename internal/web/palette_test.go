package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"atelier/internal/store"
)

type palEnv struct {
	snipEnv
	priv, pub store.Repo
}

// newPalEnv: zoe owns private zoe/demo (with files and an issue) and public
// zoe/open-tools; there is a private snippet and a public one; bob is a second
// person; acme is a company account with only private repositories.
func newPalEnv(t *testing.T) palEnv {
	t.Helper()
	e := newSnipEnv(t)
	a := e.a
	ctx := context.Background()
	zoe, _ := a.db.OwnerByName(ctx, "zoe")
	priv, _ := a.db.RepoByName(ctx, "zoe", "demo")

	// Files in the private repository.
	work := t.TempDir()
	runGit(t, work, "init", "--initial-branch=main", ".")
	for p, c := range map[string]string{"cmd/secretd/main.go": "package main\n", "internal/vault/keys.go": "package vault\n", "README.md": "# x\n"} {
		if err := os.MkdirAll(filepath.Join(work, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "init")
	runGit(t, work, "push", a.git.Path("zoe", "demo"), "main")

	if _, err := a.db.CreateIssue(ctx, priv.ID, "Leaky vault rotation", "", "owner", "zoe"); err != nil {
		t.Fatal(err)
	}
	if err := a.git.Init(ctx, "zoe", "open-tools"); err != nil {
		t.Fatal(err)
	}
	pub, err := a.db.CreateRepo(ctx, zoe.ID, "open-tools", "Handy public tools", false)
	if err != nil {
		t.Fatal(err)
	}
	acme, _ := a.db.CreateOwner(ctx, "acme", "Acme Secret Co", "company")
	if err := a.git.Init(ctx, "acme", "hidden"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.CreateRepo(ctx, acme.ID, "hidden", "", true); err != nil {
		t.Fatal(err)
	}
	e.create(t, snippetForm("zoe", "", "private", "vault-notes.txt", "secret"))
	e.create(t, snippetForm("zoe", "", "public", "vault-public.txt", "shared"))
	return palEnv{snipEnv: e, priv: priv, pub: pub}
}

func palGet(t *testing.T, a *App, q, repo string, hdr map[string]string) palResponse {
	t.Helper()
	target := "/-/palette?" + url.Values{"q": {q}, "repo": {repo}}.Encode()
	rec := do(t, a, "GET", target, nil, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d\n%s", target, rec.Code, rec.Body)
	}
	var res palResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func kinds(res palResponse) map[string]int {
	m := map[string]int{}
	for _, it := range res.Items {
		m[it.Kind]++
	}
	return m
}

func hasTitle(res palResponse, kind, title string) bool {
	for _, it := range res.Items {
		if it.Kind == kind && it.Title == title {
			return true
		}
	}
	return false
}

func TestPalettePrefixModes(t *testing.T) {
	e := newPalEnv(t)
	a := e.a

	// No query: repositories and commands.
	res := palGet(t, a, "", "", e.owner)
	if k := kinds(res); k["repo"] == 0 || k["command"] == 0 || len(res.Items) > paletteMax {
		t.Errorf("empty query kinds = %v (%d items)", k, len(res.Items))
	}
	// Mixed: repo first, then files of the current repo, issues, snippets, GitHub last.
	res = palGet(t, a, "vault", "zoe/demo", e.owner)
	if !hasTitle(res, "file", "internal/vault/keys.go") || !hasTitle(res, "issue", "Leaky vault rotation") ||
		!hasTitle(res, "snippet", "vault-notes.txt") {
		t.Errorf("mixed results = %+v", res.Items)
	}
	if last := res.Items[len(res.Items)-1]; last.Kind != "github" || last.URL != "/search?q=vault&src=github" {
		t.Errorf("last item = %+v, want Search GitHub", last)
	}
	res = palGet(t, a, "demo", "", e.owner)
	if len(res.Items) == 0 || res.Items[0].Kind != "repo" || res.Items[0].Title != "zoe/demo" || res.Items[0].URL != "/zoe/demo" {
		t.Errorf("repo jump = %+v", res.Items)
	}

	// > commands only, including repo commands in context.
	res = palGet(t, a, ">new", "zoe/demo", e.owner)
	if k := kinds(res); len(k) != 1 || k["command"] == 0 || !hasTitle(res, "command", "New repository") ||
		!hasTitle(res, "command", "New snippet") || !hasTitle(res, "command", "New issue in demo") {
		t.Errorf("> results = %+v", res.Items)
	}
	if res := palGet(t, a, ">backups", "", e.owner); !hasTitle(res, "command", "Settings: Backups and storage") {
		t.Errorf(">backups = %+v", res.Items)
	}
	// # issues: by number in the current repo, or by title.
	res = palGet(t, a, "#1", "zoe/demo", e.owner)
	if len(res.Items) != 1 || res.Items[0].Kind != "issue" || res.Items[0].URL != "/zoe/demo/issues/1" {
		t.Errorf("#1 = %+v", res.Items)
	}
	if res := palGet(t, a, "#leaky", "", e.owner); !hasTitle(res, "issue", "Leaky vault rotation") || len(kinds(res)) != 1 {
		t.Errorf("#leaky = %+v", res.Items)
	}
	// / files: fuzzy path match in the current repo.
	res = palGet(t, a, "/csm", "zoe/demo", e.owner)
	if len(res.Items) == 0 || res.Items[0].Title != "cmd/secretd/main.go" || res.Items[0].URL != "/zoe/demo/blob/main/cmd/secretd/main.go" {
		t.Errorf("/csm = %+v", res.Items)
	}
	if res := palGet(t, a, "/main", "", e.owner); len(res.Items) != 0 || res.Empty == "" {
		t.Errorf("/ without a repo = %+v", res)
	}
	// @ accounts.
	res = palGet(t, a, "@ac", "", e.owner)
	if k := kinds(res); len(k) != 1 || !hasTitle(res, "account", "Acme Secret Co (acme)") {
		t.Errorf("@ac = %+v", res.Items)
	}

	// The file list is cached per commit and refreshed after a push.
	if _, ok := palFilesCache.get(a.git.Path("zoe", "demo") + "@" + mustTip(t, a, "zoe", "demo")); !ok {
		t.Errorf("file list not cached")
	}
}

func mustTip(t *testing.T, a *App, owner, name string) string {
	t.Helper()
	g, err := a.git.Open(owner, name)
	if err != nil {
		t.Fatal(err)
	}
	c, err := g.ResolveCommit(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPalettePermissions(t *testing.T) {
	e := newPalEnv(t)
	a := e.a
	secrets := []string{"zoe/demo", "secretd", "vault/keys.go", "Leaky vault", "vault-notes", "acme", "Acme", "hidden"}
	for who, hdr := range map[string]map[string]string{"anonymous": nil, "bob": e.other} {
		for _, q := range []string{"", "vault", "demo", "#1", "#leaky", "/", "/csm", "/main", "@", "@ac", ">", ">new", "s", "e"} {
			rec := do(t, a, "GET", "/-/palette?"+url.Values{"q": {q}, "repo": {"zoe/demo"}}.Encode(), nil, hdr)
			if rec.Code != http.StatusOK {
				t.Errorf("%s %q = %d", who, q, rec.Code)
				continue
			}
			body := rec.Body.String()
			for _, s := range secrets {
				if strings.Contains(body, s) {
					t.Errorf("%s q=%q leaks %q:\n%s", who, q, s, body)
				}
			}
			var res palResponse
			_ = json.Unmarshal(rec.Body.Bytes(), &res)
			if res.Repo != "" {
				t.Errorf("%s q=%q got repo context %q", who, q, res.Repo)
			}
			for _, it := range res.Items {
				if (it.Kind == "command" || it.Kind == "github") && who == "anonymous" {
					t.Errorf("%s q=%q got command %+v", who, q, it)
				}
			}
		}
		// Public things are fine.
		if res := palGet(t, a, "open", "", hdr); !hasTitle(res, "repo", "zoe/open-tools") {
			t.Errorf("%s can't find the public repo: %+v", who, res.Items)
		}
		if res := palGet(t, a, "vault", "", hdr); !hasTitle(res, "snippet", "vault-public.txt") {
			t.Errorf("%s can't find the public snippet: %+v", who, res.Items)
		}
	}
	// A read-only token sees zoe's things; a bad token is refused.
	if res := palGet(t, a, "demo", "", map[string]string{"Authorization": "Bearer " + e.read}); !hasTitle(res, "repo", "zoe/demo") {
		t.Errorf("token palette = %+v", res.Items)
	}
	if rec := do(t, a, "GET", "/-/palette?q=demo", nil, map[string]string{"Authorization": "Bearer " + e.junk}); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token = %d", rec.Code)
	}
}

func TestPaletteFast(t *testing.T) {
	e := newPalEnv(t)
	a := e.a
	ctx := context.Background()
	zoe, _ := a.db.OwnerByName(ctx, "zoe")
	for i := 0; i < 40; i++ {
		name := "repo-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if _, err := a.db.CreateRepo(ctx, zoe.ID, name, "", i%2 == 0); err != nil {
			t.Fatal(err)
		}
	}
	palGet(t, a, "keys", "zoe/demo", e.owner) // warm the file cache
	start := time.Now()
	const n = 10
	for i := 0; i < n; i++ {
		palGet(t, a, "keys", "zoe/demo", e.owner)
	}
	if avg := time.Since(start) / n; avg > 100*time.Millisecond {
		t.Errorf("palette took %v per query with 40+ repos", avg)
	} else {
		t.Logf("palette: %v per query", avg)
	}
}

func TestFuzzyScore(t *testing.T) {
	if fuzzyScore("csm", "cmd/secretd/main.go") < 0 || fuzzyScore("xyz", "cmd/main.go") >= 0 {
		t.Error("subsequence matching is wrong")
	}
	if fuzzyScore("main", "cmd/main.go") <= fuzzyScore("main", "m/a/i/n.txt") {
		t.Error("contiguous match should beat a scattered one")
	}
	if fuzzyScore("", "anything") != 0 {
		t.Error("empty pattern should match with 0")
	}
}
