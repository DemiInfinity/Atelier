package gitrepo

import (
	"net/http"
	"net/http/cgi"
	"strings"
)

// IsGitPath reports whether a request path under /{owner}/{repo}.git/ is a
// smart-HTTP git endpoint, and returns owner, repo (without .git) and the
// remaining git path.
func IsGitPath(urlPath string) (owner, repo, rest string, ok bool) {
	parts := strings.SplitN(strings.TrimPrefix(urlPath, "/"), "/", 3)
	if len(parts) < 2 || !strings.HasSuffix(strings.ToLower(parts[1]), ".git") {
		return "", "", "", false
	}
	owner = parts[0]
	repo = parts[1][:len(parts[1])-4]
	if len(parts) == 3 {
		rest = parts[2]
	}
	switch {
	case rest == "info/refs", rest == "git-upload-pack", rest == "git-receive-pack", rest == "HEAD",
		strings.HasPrefix(rest, "objects/"):
		return owner, repo, rest, true
	}
	return owner, repo, rest, false
}

// IsWrite reports whether the request pushes data.
func IsWrite(r *http.Request, rest string) bool {
	return rest == "git-receive-pack" || r.URL.Query().Get("service") == "git-receive-pack"
}

// ServeHTTP hands the request to git's own http-backend. The caller must
// already have authorised it; remoteUser is recorded in the reflog.
func (s *Store) ServeHTTP(w http.ResponseWriter, r *http.Request, owner, name, rest, remoteUser string) {
	h := &cgi.Handler{
		Path: s.Git,
		Args: []string{"http-backend"},
		Dir:  s.Root,
		Env: []string{
			"GIT_PROJECT_ROOT=" + s.Root,
			"GIT_HTTP_EXPORT_ALL=1",
			"REMOTE_USER=" + remoteUser,
		},
	}
	// Rewrite to the canonical on-disk path so case differences in the URL don't matter.
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + s.RelPath(owner, name) + "/" + rest
	// git sends packs larger than http.postBuffer (1 MiB) with chunked
	// encoding. Go's net/http has already de-chunked the body, but net/http/cgi
	// refuses any request still marked chunked with a 400. Clearing the marker
	// leaves CONTENT_LENGTH unset, so git http-backend reads stdin to EOF.
	r2.TransferEncoding = nil
	h.ServeHTTP(w, r2)
}
