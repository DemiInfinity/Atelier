package web

import (
	"bytes"
	"html/template"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"

	"atelier/internal/tether"
)

// GitHub Markdown (READMEs, issues, comments, release notes) is rendered by
// Atelier's own goldmark setup: raw HTML is left out and dangerous link
// schemes are dropped, exactly as for local READMEs. On top of that:
//
//   - relative links point at the same file in /gh/<owner>/<repo>/blob/<ref>/…,
//     and links to github.com repositories, issues and pull requests open in
//     Atelier; other links open in a new tab with no referrer;
//   - images are only shown through the image proxy (/gh/-/img) and only from
//     GitHub's image hosts; any other image becomes its alt text;
//   - <img> tags in raw HTML (common in READMEs) are turned into ordinary
//     Markdown images and go through the same rules. Nothing else from raw
//     HTML is kept;
//   - #123 in issues and comments links to that issue in the repository.

// ghMD says where a piece of Markdown lives.
type ghMD struct {
	Repo   string // owner/repo
	Ref    string // branch, tag or commit for relative links
	Dir    string // directory of the file, "" at the root
	Issues bool   // link #123
}

const ghRawHost = "raw.githubusercontent.com"

// ghImageHosts may be fetched by the image proxy.
var ghImageHosts = map[string]bool{
	ghRawHost:                                   true,
	"user-images.githubusercontent.com":         true,
	"private-user-images.githubusercontent.com": true,
	"camo.githubusercontent.com":                true,
	"avatars.githubusercontent.com":             true,
	"github.com":                                true, // only /<owner>/<repo>/raw/… (see ghImageTarget)
}

func (a *App) ghMarkdown(src string, mc ghMD) template.HTML {
	if len(src) > 1<<20 {
		src = src[:1<<20]
	}
	source := []byte(src)
	doc := md.Parser().Parse(text.NewReader(source))
	ghLiftHTMLImages(doc, source)
	a.ghRewrite(doc, source, mc)
	if mc.Issues && tether.ValidRepo(mc.Repo) {
		linkIssueRefs(doc, source, ghLocalURL(mc.Repo), nil)
	}
	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, source, doc); err != nil {
		return template.HTML(template.HTMLEscapeString(src))
	}
	return template.HTML(buf.String()) // goldmark escaped every text and attribute
}

// ghRewrite fixes up links and images in place.
func (a *App) ghRewrite(doc ast.Node, source []byte, mc ghMD) {
	var links []*ast.Link
	var autos []*ast.AutoLink
	var imgs []*ast.Image
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Link:
			links = append(links, v)
		case *ast.AutoLink:
			autos = append(autos, v)
		case *ast.Image:
			imgs = append(imgs, v)
		}
		return ast.WalkContinue, nil
	})
	for _, l := range links {
		dest, external := a.ghLinkDest(string(l.Destination), mc)
		if dest == "" {
			// Not a link we'll keep: show its text.
			p := l.Parent()
			for c := l.FirstChild(); c != nil; {
				next := c.NextSibling()
				p.InsertBefore(p, l, c)
				c = next
			}
			p.RemoveChild(p, l)
			continue
		}
		l.Destination = []byte(dest)
		if external {
			l.SetAttributeString("rel", []byte("noopener noreferrer nofollow"))
			l.SetAttributeString("target", []byte("_blank"))
		}
	}
	for _, l := range autos {
		l.SetAttributeString("rel", []byte("noopener noreferrer nofollow"))
		l.SetAttributeString("target", []byte("_blank"))
	}
	for _, img := range imgs {
		src := a.ghImageSrc(string(img.Destination), mc)
		if src == "" {
			alt := ghAltText(img, source)
			p := img.Parent()
			if p == nil {
				continue
			}
			if alt == "" {
				p.RemoveChild(p, img)
				continue
			}
			p.ReplaceChild(p, img, ast.NewString([]byte(alt)))
			continue
		}
		img.Destination = []byte(src)
		img.Title = nil
		img.SetAttributeString("loading", []byte("lazy"))
		img.SetAttributeString("referrerpolicy", []byte("no-referrer"))
	}
}

func ghAltText(n ast.Node, source []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := c.(type) {
		case *ast.Text:
			b.Write(v.Segment.Value(source))
		case *ast.String:
			b.Write(v.Value)
		}
		return ast.WalkContinue, nil
	})
	s := strings.TrimSpace(b.String())
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// ghRepoPath resolves a relative reference (already unescaped) against the
// file's directory. A leading "/" means the repository root. ok=false when it
// climbs out of the repository.
func ghRepoPath(dir, ref string) (string, bool) {
	var p string
	if strings.HasPrefix(ref, "/") {
		p = strings.TrimPrefix(path.Clean(ref), "/")
	} else {
		p = path.Clean(path.Join(dir, ref))
		if p == ".." || strings.HasPrefix(p, "../") {
			return "", false
		}
	}
	if p == "." {
		p = ""
	}
	return p, true
}

func ghEscPath(p string) string {
	if p == "" {
		return ""
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// ghLinkDest is where a Markdown link should go ("" to drop it) and whether
// it leaves Atelier.
func (a *App) ghLinkDest(dest string, mc ghMD) (string, bool) {
	dest = strings.TrimSpace(dest)
	switch {
	case dest == "":
		return "", false
	case strings.HasPrefix(dest, "#"):
		return dest, false
	}
	u, err := url.Parse(dest)
	if err != nil {
		return "", false
	}
	if u.Scheme != "" || u.Host != "" {
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
		case "mailto":
			return dest, true
		default:
			return "", false // javascript:, data:, vbscript:, file:…
		}
		if local := a.ghFromGitHubURL(u); local != "" {
			return local, false
		}
		return u.String(), true
	}
	if !tether.ValidRepo(mc.Repo) || mc.Ref == "" {
		return "", false
	}
	p, ok := ghRepoPath(mc.Dir, u.Path)
	if !ok {
		return "", false
	}
	out := ghLocalURL(mc.Repo) + "/blob/" + ghEscPath(mc.Ref)
	if p != "" {
		out += "/" + ghEscPath(p)
	} else {
		out = ghLocalURL(mc.Repo) + "/tree/" + ghEscPath(mc.Ref)
	}
	if u.Fragment != "" {
		out += "#" + url.PathEscape(u.Fragment)
	}
	return out, false
}

// ghFromGitHubURL maps a github.com URL onto the same page in /gh/ ("" if
// it isn't a page Atelier shows).
func (a *App) ghFromGitHubURL(u *url.URL) string {
	web, err := url.Parse(a.tether().web)
	if err != nil || !strings.EqualFold(u.Host, web.Host) || u.User != nil {
		return ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 2 || !tether.ValidRepo(segs[0]+"/"+segs[1]) {
		return ""
	}
	base := ghLocalURL(segs[0] + "/" + segs[1])
	rest := segs[2:]
	switch {
	case len(rest) == 0:
		return base
	case len(rest) == 1 && (rest[0] == "issues" || rest[0] == "pulls" || rest[0] == "releases" || rest[0] == "commits"):
		return base + "/" + rest[0]
	case len(rest) >= 2 && (rest[0] == "issues" || rest[0] == "pull") && ghNumberRe.MatchString(rest[1]):
		out := base + "/" + rest[0] + "/" + rest[1]
		if len(rest) == 3 && rest[0] == "pull" && rest[2] == "files" {
			out += "/files"
		}
		return out
	case len(rest) == 2 && rest[0] == "commit" && ghSHARe.MatchString(rest[1]):
		return base + "/commit/" + rest[1]
	case len(rest) >= 2 && (rest[0] == "blob" || rest[0] == "tree"):
		return base + "/" + rest[0] + "/" + ghEscPath(strings.Join(rest[1:], "/"))
	}
	return ""
}

var ghSHARe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// ghImageSrc is the proxied src for an image ("" to show its alt text).
func (a *App) ghImageSrc(dest string, mc ghMD) string {
	dest = strings.TrimSpace(dest)
	if dest == "" || len(dest) > 2048 {
		return ""
	}
	u, err := url.Parse(dest)
	if err != nil {
		return ""
	}
	var abs string
	switch {
	case u.Scheme == "" && u.Host == "":
		if !tether.ValidRepo(mc.Repo) || mc.Ref == "" {
			return ""
		}
		p, ok := ghRepoPath(mc.Dir, u.Path)
		if !ok || p == "" {
			return ""
		}
		abs = "https://" + ghRawHost + "/" + mc.Repo + "/" + ghEscPath(mc.Ref) + "/" + ghEscPath(p)
	default:
		// github.com/<o>/<r>/blob/<ref>/<path>?raw=true is the raw file too.
		if strings.EqualFold(u.Host, "github.com") {
			segs := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(segs) > 4 && segs[2] == "blob" {
				segs[2] = "raw"
				u.Path = "/" + strings.Join(segs, "/")
				u.RawPath = ""
				u.RawQuery = ""
			}
		}
		abs = u.String()
	}
	if _, err := ghImageTarget(abs); err != nil {
		return ""
	}
	return "/gh/-/img?" + url.Values{"u": {abs}}.Encode()
}

// ---------- <img> in raw HTML ----------

var (
	ghImgTagRe  = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	ghImgAttrRe = regexp.MustCompile(`(?is)\b(src|alt)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)
)

// ghLiftHTMLImages replaces raw HTML blocks and inline HTML that contain
// <img> tags with plain Markdown images (src and alt only). The rest of the
// raw HTML is dropped, as before.
func ghLiftHTMLImages(doc ast.Node, source []byte) {
	type hit struct {
		n    ast.Node
		html string
	}
	var hits []hit
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.HTMLBlock:
			var b strings.Builder
			for i := 0; i < v.Lines().Len(); i++ {
				seg := v.Lines().At(i)
				b.Write(seg.Value(source))
			}
			if v.HasClosure() {
				b.Write(v.ClosureLine.Value(source))
			}
			hits = append(hits, hit{v, b.String()})
			return ast.WalkSkipChildren, nil
		case *ast.RawHTML:
			var b strings.Builder
			for i := 0; i < v.Segments.Len(); i++ {
				seg := v.Segments.At(i)
				b.Write(seg.Value(source))
			}
			hits = append(hits, hit{v, b.String()})
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	for _, h := range hits {
		tags := ghImgTagRe.FindAllString(h.html, 20)
		if len(tags) == 0 {
			continue
		}
		var imgs []ast.Node
		for _, tag := range tags {
			var src, alt string
			for _, m := range ghImgAttrRe.FindAllStringSubmatch(tag, -1) {
				v := m[2] + m[3] + m[4]
				v = htmlUnescapeAttr(v)
				switch strings.ToLower(m[1]) {
				case "src":
					src = v
				case "alt":
					alt = v
				}
			}
			if src == "" {
				continue
			}
			img := ast.NewImage(ast.NewLink())
			img.Destination = []byte(src)
			if alt != "" {
				img.AppendChild(img, ast.NewString([]byte(alt)))
			}
			imgs = append(imgs, img)
		}
		if len(imgs) == 0 {
			continue
		}
		p := h.n.Parent()
		if p == nil {
			continue
		}
		if _, block := h.n.(*ast.HTMLBlock); block {
			para := ast.NewParagraph()
			for i, img := range imgs {
				if i > 0 {
					para.AppendChild(para, ast.NewString([]byte(" ")))
				}
				para.AppendChild(para, img)
			}
			p.ReplaceChild(p, h.n, para)
			continue
		}
		for _, img := range imgs {
			p.InsertBefore(p, h.n, img)
		}
		p.RemoveChild(p, h.n)
	}
}

// htmlUnescapeAttr undoes the few entities seen in attribute values.
func htmlUnescapeAttr(s string) string {
	r := strings.NewReplacer("&amp;", "&", "&quot;", `"`, "&#39;", "'", "&lt;", "<", "&gt;", ">")
	return r.Replace(s)
}
