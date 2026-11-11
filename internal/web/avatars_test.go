package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"atelier/internal/backup"
	"atelier/internal/store"
)

// ---------- image helpers ----------

func testImage(w, h int) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{uint8(x * 255 / w), uint8(y * 255 / h), 120, 255})
		}
	}
	return img
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, testImage(w, h)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// pngChunk builds a PNG chunk with a correct CRC.
func pngChunk(typ string, data []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(len(data)))
	b.WriteString(typ)
	b.Write(data)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	return b.Bytes()
}

// withPNGText inserts a tEXt chunk (metadata) before IEND.
func withPNGText(b []byte, text string) []byte {
	iend := bytes.Index(b, []byte("IEND")) - 4
	out := append([]byte{}, b[:iend]...)
	out = append(out, pngChunk("tEXt", []byte("Comment\x00"+text))...)
	return append(out, b[iend:]...)
}

// pngBomb claims to be 50000 × 50000 pixels (2.5 gigapixels) in a few bytes.
func pngBomb(t *testing.T) []byte {
	b := testPNG(t, 2, 2)
	ihdr := b[8+8 : 8+8+13]
	binary.BigEndian.PutUint32(ihdr[0:], 50000)
	binary.BigEndian.PutUint32(ihdr[4:], 50000)
	binary.BigEndian.PutUint32(b[8+8+13:], crc32.ChecksumIEEE(b[8+4:8+8+13]))
	return b
}

func testJPEG(t *testing.T, exif string) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, testImage(120, 90), &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	j := b.Bytes()
	if exif == "" {
		return j
	}
	payload := append([]byte("Exif\x00\x00"), exif...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	out := append([]byte{}, j[:2]...)
	out = append(out, seg...)
	out = append(out, payload...)
	return append(out, j[2:]...)
}

// A 1 × 1 lossless WebP.
func testWebP(t *testing.T) []byte {
	b, err := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func uploadPicture(t *testing.T, a *App, cookie map[string]string, account, filename, ctype string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("account", account)
	mw.WriteField("action", "upload")
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="picture"; filename="` + filename + `"`}
	h["Content-Type"] = []string{ctype}
	part, _ := mw.CreatePart(h)
	part.Write(data)
	mw.Close()
	req := httptest.NewRequest("POST", "/settings/profile/picture?account="+url.QueryEscape(account), &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range cookie {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	return rec
}

// ---------- tests ----------

// processAvatar keeps only what it decoded: valid PNG / JPEG / WebP become
// fresh square PNGs without metadata; anything else is refused.
func TestAvatarProcessing(t *testing.T) {
	for name, in := range map[string][]byte{
		"png":  withPNGText(testPNG(t, 300, 200), "SECRET-GPS 51.5,-0.1"),
		"jpeg": testJPEG(t, "SECRET-GPS 51.5,-0.1"),
		"webp": testWebP(t),
	} {
		out, hash, err := processAvatar(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(hash) != 16 {
			t.Errorf("%s: hash %q", name, hash)
		}
		for _, n := range avatarSizes {
			cfg, err := png.DecodeConfig(bytes.NewReader(out[n]))
			if err != nil || cfg.Width != n || cfg.Height != n {
				t.Errorf("%s %d: %v %dx%d", name, n, err, cfg.Width, cfg.Height)
			}
			if bytes.Contains(out[n], []byte("SECRET-GPS")) || bytes.Contains(out[n], []byte("tEXt")) || bytes.Contains(out[n], []byte("Exif")) {
				t.Errorf("%s %d: metadata survived", name, n)
			}
			if bytes.Equal(out[n], in) {
				t.Errorf("%s: the uploaded bytes came back unchanged", name)
			}
		}
	}

	zip := []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00evil.jsp")
	refused := map[string]struct {
		b    []byte
		want error
	}{
		"empty":            {nil, errAvatarTooTiny},
		"too big":          {append(testPNG(t, 4, 4), make([]byte, avatarMaxBytes)...), errAvatarTooBig},
		"gif":              {[]byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"), errAvatarType},
		"svg":              {[]byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`), errAvatarType},
		"html":             {[]byte("<html><script>alert(1)</script></html>"), errAvatarType},
		"png + html":       {append(testPNG(t, 4, 4), "<html><script>alert(1)</script>"...), errAvatarExtra},
		"jpeg + zip":       {append(testJPEG(t, ""), zip...), errAvatarExtra},
		"webp + js":        {append(testWebP(t), "alert(1)//"...), errAvatarExtra},
		"bomb":             {pngBomb(t), errAvatarHuge},
		"truncated png":    {testPNG(t, 40, 40)[:60], errAvatarBroken},
		"png magic + junk": {append([]byte("\x89PNG\r\n\x1a\n"), "not really"...), errAvatarBroken},
	}
	for name, c := range refused {
		if _, _, err := processAvatar(c.b); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}

// Settings → Profile → Picture: uploads, refusals, serving, and who may.
func TestAvatarUploadAndServe(t *testing.T) {
	f := newGitFixture(t)
	a := f.a
	ctx := context.Background()
	zoe, _ := a.db.OwnerByName(ctx, "zoe")
	co, _ := a.db.CreateOwner(ctx, "elyndrastudios", "Elyndra Studios LTD", "company")
	zc := cookieFor(t, a, zoe.ID)

	// Every kind of bad file is refused with a message, and nothing is stored.
	bad := map[string]struct {
		file, ctype string
		b           []byte
		msg         string
	}{
		"svg renamed .png":   {"me.png", "image/png", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "Use a PNG, JPEG or WebP"},
		"text":               {"me.jpg", "image/jpeg", []byte("hello"), "Use a PNG, JPEG or WebP"},
		"polyglot":           {"me.png", "image/png", append(testPNG(t, 8, 8), "<?php system($_GET['c']); ?>"...), "something extra"},
		"decompression bomb": {"bomb.png", "image/png", pngBomb(t), "too large"},
		"just over 5 MB":     {"big.png", "image/png", append(testPNG(t, 8, 8), make([]byte, avatarMaxBytes)...), "over 5 MB"},
		"far over 5 MB":      {"huge.png", "image/png", make([]byte, 8<<20), "over 5 MB"},
	}
	for name, c := range bad {
		rec := uploadPicture(t, a, zc, "zoe", c.file, c.ctype, c.b)
		body := expectStatus(t, rec, http.StatusUnprocessableEntity, name)
		mustContain(t, body, name, c.msg)
	}
	if _, err := a.db.AccountAvatar(ctx, zoe.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused upload left a picture: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(a.cfg.DataDir, avatarDir)); len(entries) != 0 {
		t.Errorf("refused uploads left files: %v", entries)
	}

	// A good one.
	expectStatus(t, uploadPicture(t, a, zc, "zoe", "me.jpg", "image/jpeg", testJPEG(t, "SECRET-GPS")), http.StatusSeeOther, "upload")
	av, err := a.db.AccountAvatar(ctx, zoe.ID)
	if err != nil || av.Source != store.AvatarUpload {
		t.Fatalf("avatar = %+v, %v", av, err)
	}
	for _, n := range avatarSizes {
		b, err := os.ReadFile(a.avatarPath(zoe.ID, n))
		if err != nil || bytes.Contains(b, []byte("SECRET-GPS")) {
			t.Fatalf("stored %d: %v", n, err)
		}
	}

	// Served with the right headers, cacheable by ETag.
	rec := do(t, a, "GET", "/-/avatar/zoe/160.png", nil, zc)
	expectStatus(t, rec, 200, "serve")
	h := rec.Header()
	for k, want := range map[string]string{
		"Content-Type": "image/png", "X-Content-Type-Options": "nosniff", "Cache-Control": "private, max-age=86400",
		"ETag": `"` + av.Hash + `-160"`, "Content-Security-Policy": "default-src 'none'; sandbox",
	} {
		if h.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, h.Get(k), want)
		}
	}
	if cfg, err := png.DecodeConfig(rec.Body); err != nil || cfg.Width != 160 {
		t.Errorf("served picture: %v %d", err, cfg.Width)
	}
	rec = do(t, a, "GET", "/-/avatar/zoe/160.png", nil, map[string]string{"Cookie": zc["Cookie"], "If-None-Match": `"` + av.Hash + `-160"`})
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("If-None-Match = %d (%d bytes)", rec.Code, rec.Body.Len())
	}
	for _, p := range []string{"/-/avatar/zoe/65.png", "/-/avatar/zoe/64.jpg", "/-/avatar/zoe/../64.png", "/-/avatar/nobody/64.png", "/-/avatar/elyndrastudios/64.png"} {
		if rec := do(t, a, "GET", p, nil, zc); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", p, rec.Code)
		}
	}
	if rec := do(t, a, "POST", "/-/avatar/zoe/64.png", url.Values{}, zc); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST avatar = %d", rec.Code)
	}

	// Anonymous visitors and a second person can't fetch the picture of an
	// account with nothing public.
	sam, _ := a.db.CreateOwner(ctx, "sam", "", "user")
	sc := cookieFor(t, a, sam.ID)
	for who, ck := range map[string]map[string]string{"anonymous": nil, "sam": sc} {
		if rec := do(t, a, "GET", "/-/avatar/zoe/64.png", nil, ck); rec.Code != http.StatusNotFound {
			t.Errorf("%s fetching zoe's picture = %d", who, rec.Code)
		}
	}
	// … nor change anyone else's.
	for _, acct := range []string{"zoe", "elyndrastudios"} {
		if rec := uploadPicture(t, a, sc, acct, "x.png", "image/png", testPNG(t, 10, 10)); rec.Code != http.StatusForbidden {
			t.Errorf("sam uploading for %s = %d", acct, rec.Code)
		}
	}

	// Companies get their own; WebP works.
	expectStatus(t, uploadPicture(t, a, zc, "elyndrastudios", "logo.webp", "image/webp", testWebP(t)), http.StatusSeeOther, "company upload")
	if _, err := a.db.AccountAvatar(ctx, co.ID); err != nil {
		t.Fatal(err)
	}

	// Shown in the top bar, the profile, the account menu and the edit page.
	src := `/-/avatar/zoe/64.png?v=` + av.Hash
	body := expectStatus(t, do(t, a, "GET", "/zoe", nil, zc), 200, "profile")
	mustContain(t, body, "profile", `class="acct-pic" src="`+src, `/-/avatar/zoe/160.png?v=`+av.Hash, `/-/avatar/elyndrastudios/64.png?v=`)
	body = expectStatus(t, do(t, a, "GET", "/settings/profile", nil, zc), 200, "edit page")
	mustContain(t, body, "edit page", "Uploaded", "Remove picture", `enctype="multipart/form-data"`)

	// Remove: back to the initial.
	rec = do(t, a, "POST", "/settings/profile/picture", url.Values{"account": {"zoe"}, "action": {"remove"}}, zc)
	expectStatus(t, rec, http.StatusSeeOther, "remove")
	a.avatarIndexDirty()
	body = expectStatus(t, do(t, a, "GET", "/zoe", nil, zc), 200, "profile after remove")
	mustNotContain(t, body, "after remove", "/-/avatar/zoe/")
	if rec := do(t, a, "GET", "/-/avatar/zoe/64.png", nil, zc); rec.Code != http.StatusNotFound {
		t.Errorf("removed picture still served: %d", rec.Code)
	}
}

// Pictures beside comments, commits (matched by email) and in the palette;
// the initial when there's no picture or no matching account.
func TestAvatarsShownBesideAuthors(t *testing.T) {
	f := newGitFixture(t)
	a := f.a
	ctx := context.Background()
	zoe, _ := a.db.OwnerByName(ctx, "zoe")
	zc := cookieFor(t, a, zoe.ID)
	expectStatus(t, uploadPicture(t, a, zc, "zoe", "me.png", "image/png", testPNG(t, 64, 64)), http.StatusSeeOther, "upload")
	av, _ := a.db.AccountAvatar(ctx, zoe.ID)
	pic := `<img class="avatar avatar-sm avatar-pic" src="/-/avatar/zoe/64.png?v=` + av.Hash + `"`

	demo, _ := a.db.RepoByName(ctx, "zoe", "demo")
	is, err := a.db.CreateIssue(ctx, demo.ID, "Crash on start", "It crashes.", ActorOwner, "zoe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.AddIssueComment(ctx, is.ID, "Fixed it.", ActorOwner, "zoe"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.AddIssueComment(ctx, is.ID, "Thanks!", "import", "github:alice"); err != nil {
		t.Fatal(err)
	}
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/issues/1", nil, zc), 200, "issue")
	if n := strings.Count(body, pic); n < 3 { // author, comment, participants
		t.Errorf("issue page shows zoe's picture %d times, want at least 3", n)
	}
	mustContain(t, body, "issue", `<span class="avatar avatar-sm" aria-hidden="true">G</span>`) // github:alice keeps her initial

	// Commits by an email zoe listed show her picture.
	rec := do(t, a, "POST", "/settings/profile/links", url.Values{"account": {"zoe"}, "emails": {"Zoe@Example.com\nbad"}}, zc)
	expectStatus(t, rec, http.StatusUnprocessableEntity, "bad email")
	rec = do(t, a, "POST", "/settings/profile/links", url.Values{"account": {"zoe"}, "emails": {"Zoe@Example.com"}}, zc)
	expectStatus(t, rec, http.StatusSeeOther, "emails")
	if err := a.git.Init(ctx, "zoe", "open"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.CreateRepo(ctx, zoe.ID, "open", "", false); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	runGit(t, work, "init", "-q", "--initial-branch=main", ".")
	runGit(t, work, "commit", "-q", "--allow-empty", "--author=Zoe Q <zoe@example.com>", "-m", "first")
	runGit(t, work, "commit", "-q", "--allow-empty", "--author=Stranger <who@else.example>", "-m", "second")
	runGit(t, work, "push", "-q", a.git.Path("zoe", "open"), "main")
	body = expectStatus(t, do(t, a, "GET", "/zoe/open/commits/main", nil, zc), 200, "commits")
	if strings.Count(body, pic) != 1 {
		t.Errorf("commits page: zoe's picture %d times, want 1 (her commit only)", strings.Count(body, pic))
	}
	mustContain(t, body, "commits", `<span class="avatar avatar-sm" aria-hidden="true">S</span>`)

	// Palette results carry it too.
	rec = do(t, a, "GET", "/-/palette?q=%40zoe", nil, zc)
	var res palResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || len(res.Items) == 0 || !strings.HasPrefix(res.Items[0].Avatar, "/-/avatar/zoe/64.png") {
		t.Errorf("palette: %v %+v", err, res.Items)
	}
}

// Avatars are part of every backup.
func TestBackupIncludesAvatars(t *testing.T) {
	f := newGitFixture(t)
	a := f.a
	ctx := context.Background()
	zoe, _ := a.db.OwnerByName(ctx, "zoe")
	zc := cookieFor(t, a, zoe.ID)
	expectStatus(t, uploadPicture(t, a, zc, "zoe", "me.png", "image/png", testPNG(t, 50, 50)), http.StatusSeeOther, "upload")
	res, err := backup.Create(ctx, backup.Options{DB: a.db.DB(), RepoRoot: a.git.Root, DataDir: a.cfg.DataDir, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	m, err := backup.ReadManifest(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, fe := range m.Files {
		got[fe.Path] = true
	}
	for _, n := range []string{"64", "160", "460"} {
		p := "avatars/" + strconv.FormatInt(zoe.ID, 10) + "/" + n + ".png"
		if !got[p] {
			t.Errorf("backup lacks %s", p)
		}
	}
}
