package web

import (
	"io"
	"net/http"
	"strings"
)

// The Pages host's own pages: a welcome at the bare address and a friendly
// "no site here" page, in place of plain-text answers. They're static and
// self-contained (inline style and logo) because the Pages host serves none of
// the app's files, and they say nothing about which repositories exist. A
// site's own 404.html still wins inside that site.

func pagesWelcome(w http.ResponseWriter, r *http.Request) {
	pagesOwnPage(w, r, http.StatusOK, "Atelier Pages",
		`<h1>Atelier Pages</h1>
<p class="lead">Websites published straight from repositories in an Atelier studio.</p>
<div class="card">
  <p>Every site lives at an address like</p>
  <p><code>/<em>owner</em>/<em>repository</em>/</code></p>
  <p class="small">and an account's own site, from a repository named <code><em>owner</em>.pages</code>, at <code>/<em>owner</em>/</code>.</p>
</div>
<p class="small">Publishing happens in the studio: open a repository's <strong>Settings → Pages</strong> and choose a branch and folder, or deploy from an Actions workflow with <code>atelier/deploy-pages</code>.</p>`)
}

func pagesPlain404(w http.ResponseWriter, r *http.Request) {
	pagesOwnPage(w, r, http.StatusNotFound, "No site here",
		`<p class="code">404</p>
<h1>There isn't a site here</h1>
<p class="lead">Nothing has been published at this address.</p>
<p class="small">If you just published it, give it a moment and refresh. Sites are only published from public repositories, so a site goes offline when its repository is made private.</p>
<p><a href="/">About Atelier Pages</a></p>`)
}

func pagesOwnPage(w http.ResponseWriter, r *http.Request, status int, title, body string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, strings.NewReplacer("{{title}}", title, "{{body}}", body).Replace(pagesOwnPageHTML))
}

const pagesOwnPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{title}}</title>
<style>
:root{--bg:#131211;--card:#1a1917;--line:#2c2a27;--ink:#ece6dc;--muted:#a39b8f;--amber:#e8a33d}
@media (prefers-color-scheme: light){:root{--bg:#f7f4ef;--card:#fffdf9;--line:#e4ddd2;--ink:#1f1c18;--muted:#6b645a;--amber:#b5761a}}
*{box-sizing:border-box}
html,body{margin:0;background:var(--bg);color:var(--ink);font:16px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",system-ui,sans-serif}
main{max-width:560px;margin:0 auto;padding:max(12vh,48px) 20px 48px}
.mark{display:flex;align-items:center;gap:10px;color:var(--amber);font-weight:600;letter-spacing:.01em;margin-bottom:28px}
h1{font-size:30px;line-height:1.2;margin:0 0 10px;font-weight:650}
.lead{font-size:18px;color:var(--muted);margin:0 0 24px}
.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:16px 18px;margin:0 0 20px}
.card p{margin:0 0 6px}.card p:last-child{margin:0}
.small{font-size:14px;color:var(--muted)}
.code{font:600 14px ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--amber);margin:0 0 8px}
code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.92em;background:var(--line);padding:1px 6px;border-radius:5px}
em{font-style:normal;color:var(--amber)}
a{color:var(--amber)}
footer{margin-top:40px;font-size:13px;color:var(--muted)}
</style>
</head>
<body>
<main>
<div class="mark"><svg width="28" height="28" viewBox="0 0 28 28" fill="none" aria-hidden="true"><rect x="2" y="2" width="24" height="24" rx="7" stroke="currentColor" stroke-width="2"/><path d="M9 19 L14 8 L19 19 M11 15 H17" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg><span>Atelier Pages</span></div>
{{body}}
<footer>Served by Atelier, a self-hosted studio for code.</footer>
</main>
</body>
</html>
`
