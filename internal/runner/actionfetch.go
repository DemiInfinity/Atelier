package runner

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const maxActionSize = 512 << 20 // uncompressed bytes per action

// fetchAction returns the directory of owner/repo at sha, downloading the
// codeload tarball into <cache>/actions/<owner>/<repo>/<sha> the first time.
// The tarball must really be that commit: its pax header comment (written by
// git archive) or its top-level directory name must carry the sha.
func (r *Runner) fetchAction(ctx context.Context, owner, repo, sha string) (string, error) {
	_, cache := r.Config.Dirs()
	dest := filepath.Join(cache, "actions", safeName(owner), safeName(repo), sha)
	if fi, err := os.Stat(filepath.Join(dest, ".atelier-complete")); err == nil && !fi.IsDir() {
		return dest, nil
	}
	r.fetchMu.Lock()
	defer r.fetchMu.Unlock()
	if _, err := os.Stat(filepath.Join(dest, ".atelier-complete")); err == nil {
		return dest, nil
	}
	base := r.Config.CodeloadURL
	if base == "" {
		base = DefaultCodeloadURL
	}
	u := fmt.Sprintf("%s/%s/%s/tar.gz/%s", strings.TrimRight(base, "/"), owner, repo, sha)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "atelier-runner")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".dl-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := extractActionTarball(resp.Body, tmp, sha); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, ".atelier-complete"), []byte(sha+"\n"), 0o644); err != nil {
		return "", err
	}
	_ = os.RemoveAll(dest)
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == 0 {
			return '_'
		}
		return r
	}, strings.TrimLeft(s, "."))
}

// extractActionTarball unpacks a codeload tarball (one top-level directory)
// into dest, stripping that directory, refusing paths that escape dest, and
// verifying the commit sha.
func extractActionTarball(r io.Reader, dest, sha string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("not a gzip tarball: %w", err)
	}
	tr := tar.NewReader(gz)
	verified := false
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tarball: %w", err)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			if c, ok := h.PAXRecords["comment"]; ok {
				if !strings.EqualFold(strings.TrimSpace(c), sha) {
					return fmt.Errorf("downloaded commit %s, expected %s", c, sha)
				}
				verified = true
			}
			continue
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		top, rest, _ := strings.Cut(name, "/")
		if !verified {
			if !strings.HasSuffix(strings.ToLower(top), "-"+sha) {
				return fmt.Errorf("tarball is not commit %s (top directory %q)", sha, top)
			}
			verified = true
		}
		if rest == "" || rest == "." {
			continue
		}
		if path.IsAbs(rest) || rest == ".." || strings.HasPrefix(rest, "../") {
			return fmt.Errorf("unsafe path %q in tarball", h.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(rest))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if total > maxActionSize {
				return errors.New("action is too large")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, h.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			// keep only relative links that stay inside the action
			l := h.Linkname
			if path.IsAbs(l) || strings.Contains(l, "\\") {
				continue
			}
			resolved := path.Clean(path.Join(path.Dir(rest), l))
			if resolved == ".." || strings.HasPrefix(resolved, "../") {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Symlink(filepath.FromSlash(l), target)
		}
	}
	if !verified {
		return errors.New("empty tarball")
	}
	return nil
}
