package web

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestStaticAssetsAreVersioned(t *testing.T) {
	a := newTestApp(t)
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest("GET", "/setup", nil))
	body := rec.Body.String()
	v := "?v=" + buildVersion()
	if !strings.Contains(body, `/static/atelier.css`+v) {
		t.Fatalf("stylesheet link not versioned:\n%.800s", body)
	}
	if m := regexp.MustCompile(`(?:href|src)="/static/[^"?]+"`).FindString(body); m != "" {
		t.Errorf("unversioned static link: %s", m)
	}
	rec = httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest("GET", "/static/atelier.css"+v, nil))
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("versioned asset = %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	rec = httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest("GET", "/static/atelier.css", nil))
	if !strings.Contains(rec.Header().Get("Cache-Control"), "max-age=3600") {
		t.Errorf("plain asset cache = %q", rec.Header().Get("Cache-Control"))
	}
}
