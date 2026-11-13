package runner

// Self-update. The server offers a newer build on the poll reply
// (actions.UpdateOffer). The runner stops taking jobs, lets the ones running
// finish, downloads the build, checks its size and SHA-256, makes sure it
// starts and reports the offered version, swaps it in for its own
// executable and asks to be restarted: Run returns ErrRestart and the
// command restarts the process (RestartSelf). A build that fails any check is
// reported back (PollRequest.UpdateFailed) so the server stops offering it,
// and the runner carries on as it was.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"atelier/internal/actions"
)

// ErrRestart is returned by Run after the runner replaced its executable:
// the caller should start it again (RestartSelf).
var ErrRestart = errors.New("atelier-runner was updated and needs to restart")

const maxUpdateSize = 256 << 20

// updateState: the offer being waited on, and the last version that failed.
type updateState struct {
	pending *actions.UpdateOffer
	failed  string
}

// selfExecutable is the running binary (tests swap it).
var selfExecutable = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if e, err := filepath.EvalSymlinks(exe); err == nil {
		exe = e
	}
	return exe, nil
}

// applyUpdate downloads, checks and installs offer. On success the new
// build is in place of the running executable.
func (r *Runner) applyUpdate(ctx context.Context, offer *actions.UpdateOffer) error {
	exe, err := selfExecutable()
	if err != nil {
		return fmt.Errorf("find own executable: %w", err)
	}
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".atelier-runner-update-*")
	if err != nil {
		return fmt.Errorf("can't write next to %s (%w); update it by hand", exe, err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	dctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	h := sha256.New()
	n, err := r.Client.Download(dctx, "/update", io.MultiWriter(tmp, h), maxUpdateSize)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if n != offer.Size {
		return fmt.Errorf("download is %d bytes, expected %d", n, offer.Size)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(sum, offer.SHA256) {
		return fmt.Errorf("checksum mismatch: got %s, expected %s", sum, offer.SHA256)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	// It must start and say it is the offered version.
	vctx, vcancel := context.WithTimeout(ctx, 30*time.Second)
	defer vcancel()
	out, err := exec.CommandContext(vctx, tmpPath, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("the new build doesn't start: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), " "+offer.Version+" ") {
		return fmt.Errorf("the new build says %q, expected version %s", strings.TrimSpace(string(out)), offer.Version)
	}
	if err := swapExecutable(exe, tmpPath); err != nil {
		return fmt.Errorf("replace %s: %w", exe, err)
	}
	keep = true
	return nil
}

// swapExecutable puts next in place of exe. Unix renames over the running
// file (the process keeps the old one open). Windows can't replace a running
// .exe but can rename it, so the old one moves aside to exe.old (removed on
// the next start, see cleanOldExecutable).
func swapExecutable(exe, next string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(next, exe)
	}
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(next, exe); err != nil {
		_ = os.Rename(old, exe)
		return err
	}
	return nil
}

// cleanOldExecutable removes what a Windows update moved aside.
func cleanOldExecutable() {
	if runtime.GOOS != "windows" {
		return
	}
	if exe, err := selfExecutable(); err == nil {
		_ = os.Remove(exe + ".old")
	}
}

// Download GETs an API path into w, refusing more than max bytes.
func (c *Client) Download(ctx context.Context, path string, w io.Writer, max int64) (int64, error) {
	u := strings.TrimRight(c.BaseURL, "/") + actions.APIPrefix + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "atelier-runner")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := *c.httpClient()
	hc.Timeout = 0 // the context bounds it; a build can take longer than a poll
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, &HTTPError{Status: resp.StatusCode, Body: string(b)}
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, max+1))
	if err == nil && n > max {
		err = fmt.Errorf("more than %d bytes", max)
	}
	return n, err
}
