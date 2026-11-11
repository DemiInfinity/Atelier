package runner

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// atelier/deploy-pages is built into the runner (like actions/checkout): it
// packs a folder of the workspace into a tar.gz and uploads it to the
// server's POST /api/pages/deploy with the job token. The server only
// accepts it for the job's own repository, on a push to the site's branch.
//
//	- uses: atelier/deploy-pages@v1
//	  with:
//	    path: dist        # default _site
//
// Output: page_url, the live address.

const deployPagesAction = "atelier/deploy-pages"

func isDeployPages(u string) bool {
	spec, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(u)), "@")
	return spec == deployPagesAction
}

func (j *jobRun) deployPages(ctx context.Context, sc *scope, with map[string]string) (map[string]string, int, error) {
	step := sc.logStep
	rel := strings.TrimSpace(with["path"])
	if rel == "" {
		rel = "_site"
	}
	dir := filepath.Join(j.ws, filepath.FromSlash(rel))
	if r, err := filepath.Rel(j.ws, dir); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return nil, 1, fmt.Errorf("deploy-pages path %q is outside the workspace", rel)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, 1, fmt.Errorf("deploy-pages: %s is not a folder (build the site first)", rel)
	}
	if j.job.JobToken == "" {
		return nil, 1, fmt.Errorf("deploy-pages needs a job token")
	}
	tmp, err := os.CreateTemp(j.temp, "pages-*.tar.gz")
	if err != nil {
		return nil, 1, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	files, size, skipped, err := packSite(dir, tmp)
	if err != nil {
		return nil, 1, fmt.Errorf("deploy-pages: packing %s: %w", rel, err)
	}
	if skipped > 0 {
		j.log.System(step, fmt.Sprintf("Skipped %d symlinks or special files (Pages publishes regular files only)", skipped))
	}
	j.log.System(step, fmt.Sprintf("Uploading %d files (%d bytes) from %s", files, size, rel))
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, 1, err
	}

	server := strings.TrimRight(j.r.Config.ServerURL, "/")
	q := url.Values{"repo": {j.repoFull()}, "sha": {j.job.SHA}}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/api/pages/deploy?"+q.Encode(), tmp)
	if err != nil {
		return nil, 1, err
	}
	req.Header.Set("Authorization", "Bearer "+j.job.JobToken)
	req.Header.Set("Content-Type", "application/gzip")
	client := &http.Client{}
	if j.r.Client != nil && j.r.Client.HTTP != nil {
		client = &http.Client{Transport: j.r.Client.HTTP.Transport}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 1, fmt.Errorf("deploy-pages: upload: %s", j.mask.Mask(err.Error()))
	}
	defer resp.Body.Close()
	var out struct {
		URL   string `json:"url"`
		Files int    `json:"files"`
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out)
	if resp.StatusCode != http.StatusCreated {
		msg := out.Error
		if msg == "" {
			msg = resp.Status
		}
		return nil, 1, fmt.Errorf("deploy-pages: server refused the upload: %s", msg)
	}
	j.log.System(step, "Deployed to "+out.URL)
	return map[string]string{"page_url": out.URL}, 0, nil
}

// packSite writes dir as a tar.gz to w: regular files and folders only.
func packSite(dir string, w io.Writer) (files int, size int64, skipped int, err error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(rel)
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755, ModTime: info.ModTime()})
		case info.Mode().IsRegular():
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return err
			}
			n, err := io.Copy(tw, f)
			files++
			size += n
			return err
		default:
			skipped++
			return nil
		}
	})
	if err != nil {
		return
	}
	if err = tw.Close(); err != nil {
		return
	}
	err = gz.Close()
	return
}
