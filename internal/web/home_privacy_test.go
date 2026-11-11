package web

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The home page only shows repositories, activity and accounts the viewer
// may read: a second person never sees the owner's private repositories.
func TestHomeHidesPrivateActivity(t *testing.T) {
	f, _ := langFixture(t) // zoe with a private zoe/demo that has commits
	a := f.a
	ctx := context.Background()
	if _, err := a.db.CreateOwner(ctx, "acme", "Acme Secret Co", "company"); err != nil {
		t.Fatal(err)
	}
	sam, err := a.db.CreateOwner(ctx, "sam", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := a.db.CreateSessionFor(ctx, sam.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body := expectStatus(t, do(t, a, "GET", "/", nil, map[string]string{"Cookie": sessionCookie + "=" + sess}), 200, "sam home")
	for _, leak := range []string{"zoe/demo", "committed to", "Acme Secret Co"} {
		if strings.Contains(body, leak) {
			t.Errorf("sam's home page shows %q", leak)
		}
	}
	// The owner still sees everything.
	own, err := a.db.CreateSession(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body = expectStatus(t, do(t, a, "GET", "/", nil, map[string]string{"Cookie": sessionCookie + "=" + own}), 200, "owner home")
	if !strings.Contains(body, "zoe/demo") {
		t.Error("owner's home page lost their own repository")
	}
}
