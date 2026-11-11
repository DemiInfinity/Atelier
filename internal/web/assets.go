package web

import (
	"io/fs"
	"regexp"
	"sync"
	"testing/fstest"
)

// Static assets carry the build version in their URLs (/static/x.css?v=…),
// so a deploy shows new styles and scripts at once while browsers may cache
// versioned files for a year. Templates are rewritten as they load: every
// href or src pointing at /static/ without a query gets ?v=<buildVersion>.

var staticRefRe = regexp.MustCompile(`((?:href|src)=")(/static/[^"?{}\s]+)(")`)

var (
	versionedTemplatesOnce sync.Once
	versionedTemplates     fs.FS
)

// versionedTemplateFS is templateFS with static URLs versioned.
func versionedTemplateFS() fs.FS {
	versionedTemplatesOnce.Do(func() {
		m := fstest.MapFS{}
		files, _ := fs.Glob(templateFS, "templates/*.html")
		v := "?v=" + buildVersion()
		for _, f := range files {
			b, err := fs.ReadFile(templateFS, f)
			if err != nil {
				continue
			}
			m[f] = &fstest.MapFile{Data: staticRefRe.ReplaceAll(b, []byte("${1}${2}"+v+"${3}")), Mode: 0o444}
		}
		versionedTemplates = m
	})
	return versionedTemplates
}
