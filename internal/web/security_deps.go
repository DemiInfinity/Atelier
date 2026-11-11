package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"atelier/internal/depupdates"
	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// Dependency updates: a weekly check (and "Check now") reads go.mod and
// package.json at the root of the default branch, asks the module proxy /
// npm registry for newer versions and OSV for known vulnerabilities, and,
// when the repository allows it, opens one pull request per dependency
// authored by the atelier-updates bot. Only the manifest is edited; lockfiles
// (go.sum, package-lock.json) are left for CI.

const (
	settingSecGoProxy     = "security.goproxy"      // GOPROXY-style list for update checks
	settingSecNpmRegistry = "security.npm_registry" // npm registry when Atelier's upstream cache is off
	defaultSecGoProxy     = "https://proxy.golang.org"
	defaultSecNpmRegistry = "https://registry.npmjs.org"

	secBotName   = "atelier-updates"
	secBotBranch = "atelier-updates/"
)

var (
	secOSVURL      = depupdates.DefaultOSVURL
	secHTTPClient  = &http.Client{Timeout: 20 * time.Second}
	secDepsEvery   = 7 * 24 * time.Hour
	secDepsTimeout = 5 * time.Minute
	secBot         = Actor{Kind: "bot", Name: secBotName}
)

func (a *App) secGoProxySetting(ctx context.Context) string {
	if v := strings.TrimSpace(os.Getenv("ATELIER_GOPROXY")); v != "" {
		return v
	}
	if v, err := a.db.Setting(ctx, settingSecGoProxy); err == nil && v != "" {
		return v
	}
	return defaultSecGoProxy
}

func (a *App) secNpmRegistrySetting(ctx context.Context) string {
	if v, err := a.db.Setting(ctx, settingSecNpmRegistry); err == nil && v != "" {
		return v
	}
	return defaultSecNpmRegistry
}

func (a *App) secSaveRegistries(ctx context.Context, goproxy, npm string) error {
	goproxy = strings.TrimSpace(goproxy)
	if len(depupdates.ParseGoProxy(goproxy)) == 0 {
		goproxy = ""
	}
	npm = strings.TrimRight(strings.TrimSpace(npm), "/")
	if u, err := url.Parse(npm); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		npm = ""
	}
	return errors.Join(a.db.SetSetting(ctx, settingSecGoProxy, goproxy), a.db.SetSetting(ctx, settingSecNpmRegistry, npm))
}

// secHost is the host name this Atelier serves Go modules under.
func (a *App) secHost(ctx context.Context) string {
	base := a.cfg.PublicURL
	if base == "" {
		base, _ = a.db.Setting(ctx, settingSecurityBaseURL)
	}
	if u, err := url.Parse(base); err == nil {
		return strings.ToLower(u.Host)
	}
	return ""
}

// secChecker builds the version and advisory sources: Atelier's own Go proxy
// and npm registry first, then the configured public ones.
func (a *App) secChecker(ctx context.Context) depupdates.Checker {
	host := a.secHost(ctx)
	owner, _ := a.db.PersonalOwner(ctx)
	reader := personActor(owner)
	goLocal := func(ctx context.Context, mod string) ([]string, bool) {
		if host == "" || !strings.HasPrefix(strings.ToLower(mod), host+"/") {
			return nil, false
		}
		m, ok := a.goResolve(ctx, reader, mod)
		if !ok {
			return nil, false
		}
		return a.goVersions(ctx, m), true
	}
	upstream, viaAtelier := a.npmUpstream(ctx)
	npmDoc := func(ctx context.Context, name string) ([]byte, bool, error) {
		if scope, bare := npmScope(name); scope != "" {
			if o, err := a.db.OwnerByName(ctx, scope); err == nil {
				// One of Atelier's own scopes: never ask a public registry.
				p, err := a.db.Package(ctx, o.ID, store.PkgNpm, bare)
				if err != nil {
					return nil, false, fmt.Errorf("%s isn't in Atelier's npm registry", name)
				}
				vers, err := a.db.Versions(ctx, p.ID)
				if err != nil {
					return nil, false, err
				}
				var b strings.Builder
				b.WriteString(`{"versions":{`)
				for i, v := range vers {
					if i > 0 {
						b.WriteString(",")
					}
					fmt.Fprintf(&b, "%q:{}", v.Version)
				}
				b.WriteString("}}")
				return []byte(b.String()), true, nil
			}
		}
		if viaAtelier {
			body, err := a.npmUpstreamDoc(ctx, upstream, name)
			return body, err == nil, err
		}
		return nil, false, nil
	}
	return depupdates.Checker{
		Go:  depupdates.GoProxy{URLs: depupdates.ParseGoProxy(a.secGoProxySetting(ctx)), Client: secHTTPClient, Local: goLocal},
		Npm: depupdates.NpmRegistry{URL: a.secNpmRegistrySetting(ctx), Client: secHTTPClient, Doc: npmDoc},
		OSV: &depupdates.OSV{URL: secOSVURL, Client: secHTTPClient},
	}
}

// secStartDepsCheck starts a background check; done names the page flash.
func (a *App) secStartDepsCheck(ctx context.Context, repo store.Repo) (string, error) {
	if !a.secHasManifest(ctx, repo) {
		return "nodeps", nil
	}
	ok, err := a.db.StartSecurityScan(ctx, repo.ID, "deps", secDepsTimeout+time.Minute)
	if err != nil {
		return "", err
	}
	if !ok {
		return "checking", nil
	}
	ctx = context.WithoutCancel(ctx)
	secGo(func() { a.secFinishDepsCheck(ctx, repo) })
	return "deps", nil
}

func (a *App) secFinishDepsCheck(ctx context.Context, repo store.Repo) {
	ctx, cancel := context.WithTimeout(ctx, secDepsTimeout)
	defer cancel()
	sc := store.SecurityScan{RepoID: repo.ID, Kind: "deps"}
	if err := a.secCheckDeps(ctx, repo); err != nil {
		sc.Error = err.Error()
	}
	if err := a.db.FinishSecurityScan(context.WithoutCancel(ctx), sc); err != nil {
		log.Printf("security: dependency check of %s: %v", repo.FullName(), err)
	}
}

var secManifests = []string{"go.mod", "package.json"}

func (a *App) secHasManifest(ctx context.Context, repo store.Repo) bool {
	g, err := a.git.Open(repo.Owner, repo.Name)
	if err != nil || g.IsEmpty(ctx) {
		return false
	}
	tip, err := g.BranchSHA(ctx, g.DefaultBranch(ctx))
	if err != nil {
		return false
	}
	for _, m := range secManifests {
		if g.ObjectType(ctx, tip, m) == "blob" {
			return true
		}
	}
	return false
}

// secCheckDeps checks the default branch's manifests, stores the results and
// opens update pull requests as the repository's settings allow.
func (a *App) secCheckDeps(ctx context.Context, repo store.Repo) error {
	g, err := a.git.Open(repo.Owner, repo.Name)
	if err != nil {
		return err
	}
	if g.IsEmpty(ctx) {
		return a.db.ReplaceDepUpdates(ctx, repo.ID, nil)
	}
	base := g.DefaultBranch(ctx)
	tip, err := g.BranchSHA(ctx, base)
	if err != nil {
		return err
	}
	var deps []depupdates.Dep
	var problems []string
	for _, m := range secManifests {
		b, err := g.Blob(ctx, tip, m, 1<<20)
		if err != nil || b.TooLarge || b.Binary {
			continue
		}
		var ds []depupdates.Dep
		if m == "go.mod" {
			ds, err = depupdates.ParseGoMod(b.Data)
		} else {
			ds, err = depupdates.ParsePackageJSON(b.Data)
		}
		if err != nil {
			problems = append(problems, m+" couldn't be read: "+err.Error())
			continue
		}
		deps = append(deps, ds...)
	}
	results, err := a.secChecker(ctx).Check(ctx, deps)
	if err != nil {
		problems = append(problems, err.Error())
	}
	settings, serr := a.db.SecuritySettings(ctx, repo.ID)
	if serr != nil {
		return serr
	}
	failed := 0
	var ups []store.DepUpdate
	for _, r := range results {
		if r.Err != nil {
			failed++
			continue
		}
		if r.Latest == "" && len(r.Vulns) == 0 {
			continue
		}
		u := store.DepUpdate{RepoID: repo.ID, Ecosystem: r.Ecosystem, Name: r.Name, Manifest: r.Manifest, Current: r.Spec,
			Kind: r.Kind, Vulns: len(r.Vulns), VulnIDs: strings.Join(r.Vulns, ","), Fixes: r.Fixes}
		if r.Latest != "" {
			u.Latest, u.NewSpec = r.Latest, r.NewSpec(r.Latest)
			if r.Ecosystem == depupdates.Npm {
				u.Latest = strings.TrimPrefix(r.Latest, "v")
			}
		}
		existing, err := a.secBotPR(ctx, repo.ID, r.Dep, "")
		if err != nil {
			return err
		}
		if existing != nil && existing.IsOpen() {
			u.PRNumber = existing.Number
		} else if r.Latest != "" && (settings.AllPRs || settings.SecurityPRs && len(r.Vulns) > 0) {
			n, err := a.secOpenUpdatePR(ctx, repo, g, base, tip, r)
			if err != nil {
				problems = append(problems, fmt.Sprintf("pull request for %s: %v", r.Name, err))
			}
			u.PRNumber = n
		}
		ups = append(ups, u)
	}
	if failed > 0 {
		problems = append(problems, fmt.Sprintf("%d of %d dependencies couldn't be looked up", failed, len(results)))
	}
	if err := a.db.ReplaceDepUpdates(ctx, repo.ID, ups); err != nil {
		return err
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

var secSlugRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// secBranchPrefix is the branch prefix of a dependency's update PRs
// ("atelier-updates/npm/scope-pkg/").
func secBranchPrefix(d depupdates.Dep) string {
	slug := strings.Trim(secSlugRe.ReplaceAllString(d.Name, "-"), "-.")
	for strings.Contains(slug, "..") {
		slug = strings.ReplaceAll(slug, "..", ".")
	}
	return secBotBranch + d.Ecosystem + "/" + slug + "/"
}

// secBotPR finds the bot's pull request for a dependency: the open one for
// any version, or (when branch is set) any PR from exactly that branch.
func (a *App) secBotPR(ctx context.Context, repoID int64, d depupdates.Dep, branch string) (*store.Pull, error) {
	prs, err := a.db.PullsByAuthorPrefix(ctx, repoID, secBot.Kind, secBot.Name, secBranchPrefix(d))
	if err != nil {
		return nil, err
	}
	for i := range prs {
		if prs[i].IsOpen() {
			return &prs[i], nil
		}
	}
	for i := range prs {
		if branch != "" && prs[i].Head == branch {
			return &prs[i], nil
		}
	}
	return nil, nil
}

// secOpenUpdatePR creates the branch, the commit and the pull request for one
// update, unless the bot already has one. It returns the PR number.
func (a *App) secOpenUpdatePR(ctx context.Context, repo store.Repo, g *gitrepo.Repo, base, tip string, r depupdates.Result) (int64, error) {
	version := strings.TrimPrefix(r.Latest, "v")
	branch := secBranchPrefix(r.Dep) + version
	if pr, err := a.secBotPR(ctx, repo.ID, r.Dep, branch); err != nil || pr != nil {
		if pr != nil {
			return pr.Number, nil // never a second PR for the same update, even if the first was closed
		}
		return 0, err
	}
	head, err := g.BranchSHA(ctx, branch)
	if err != nil {
		blob, err := g.Blob(ctx, tip, r.Manifest, 1<<20)
		if err != nil || blob.TooLarge {
			return 0, fmt.Errorf("can't read %s", r.Manifest)
		}
		var edited []byte
		if r.Ecosystem == depupdates.Go {
			edited, err = depupdates.UpdateGoMod(blob.Data, r.Name, r.Latest)
		} else {
			edited, err = depupdates.UpdatePackageJSON(blob.Data, r.Name, r.Spec, r.NewSpec(r.Latest))
		}
		if err != nil {
			return 0, err
		}
		sig := gitrepo.Signature{Name: secBotName, Email: secBotName + "@atelier.invalid", When: time.Now()}
		msg := fmt.Sprintf("Bump %s from %s to %s", r.Name, r.Spec, r.NewSpec(r.Latest))
		sha, err := g.CommitFile(ctx, gitrepo.FileCommit{Parent: tip, Path: r.Manifest, Content: edited, Message: msg, Author: sig})
		if err != nil {
			return 0, err
		}
		if err := g.CreateBranch(ctx, branch, sha, sig); err != nil && !errors.Is(err, gitrepo.ErrBranchExists) {
			return 0, err
		}
		if head, err = g.BranchSHA(ctx, branch); err != nil {
			return 0, err
		}
	}
	title := fmt.Sprintf("Bump %s from %s to %s", r.Name, r.Spec, r.NewSpec(r.Latest))
	pr, err := a.db.CreatePull(ctx, store.NewPull{RepoID: repo.ID, Title: title, Body: secPRBody(r), Head: branch, Base: base,
		HeadSHA: head, AuthorKind: secBot.Kind, AuthorName: secBot.Name})
	if err != nil {
		return 0, err
	}
	a.emit(ctx, Event{Kind: EventPullOpened, Repo: repo, RepoID: repo.ID, Number: pr.Number, Title: pr.Title,
		URL: fmt.Sprintf("/%s/pulls/%d", repo.FullName(), pr.Number), Actor: secBot, Detail: eventExcerpt(pr.Body, 140)})
	return pr.Number, nil
}

func secPRBody(r depupdates.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Bumps `%s` from `%s` to `%s` (%s update) in `%s`.\n\n", r.Name, r.Spec, r.NewSpec(r.Latest), r.Kind, r.Manifest)
	if len(r.Vulns) > 0 {
		fmt.Fprintf(&b, "**Security:** the current version is affected by %s.", strings.Join(r.Vulns, ", "))
		if r.Fixes > 0 {
			fmt.Fprintf(&b, " This update fixes %d of them.", r.Fixes)
		}
		b.WriteString("\n\n")
	}
	if r.Ecosystem == depupdates.Go {
		b.WriteString("Only `go.mod` was changed: `go.sum` was not regenerated. Let CI run (or run `go mod tidy` and push) before merging.\n")
	} else {
		b.WriteString("Only `package.json` was changed: the lockfile (`package-lock.json`, `yarn.lock` or `pnpm-lock.yaml`) was not regenerated. Let CI run (or run your package manager's install and push) before merging.\n")
	}
	b.WriteString("\nOpened by atelier-updates from the repository's Security settings.")
	return b.String()
}

// secDepsScheduler checks each repository with a manifest about once a week.
func (a *App) secDepsScheduler(ctx context.Context) {
	timer := time.NewTimer(3 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		repos, err := a.db.Repos(ctx)
		if err == nil {
			for _, rp := range repos {
				if ctx.Err() != nil {
					return
				}
				if sc, err := a.db.SecurityScanState(ctx, rp.ID, "deps"); err == nil && time.Since(sc.StartedAt) < secDepsEvery {
					continue
				}
				if !a.secHasManifest(ctx, rp) {
					continue
				}
				if ok, err := a.db.StartSecurityScan(ctx, rp.ID, "deps", secDepsTimeout+time.Minute); err == nil && ok {
					a.secFinishDepsCheck(ctx, rp) // one at a time
				}
			}
		}
		timer.Reset(time.Hour)
	}
}
