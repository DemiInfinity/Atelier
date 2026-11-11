package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUseSecretBypassesAllOrNothing(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	o, _ := s.CreateOwner(ctx, "zoe", "", "user")
	r, err := s.CreateRepo(ctx, o.ID, "demo", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSecretBypass(ctx, r.ID, "fp-a", "zoe", time.Hour); err != nil {
		t.Fatal(err)
	}
	// fp-b has no bypass: nothing is used, fp-a stays available.
	if ok, err := s.UseSecretBypasses(ctx, r.ID, []string{"fp-a", "fp-b"}); ok || err != nil {
		t.Fatalf("partial = %v, %v", ok, err)
	}
	if ok, _ := s.HasSecretBypass(ctx, r.ID, "fp-a"); !ok {
		t.Fatal("a refused use consumed fp-a")
	}
	if ok, err := s.UseSecretBypasses(ctx, r.ID, []string{"fp-a", "fp-a"}); !ok || err != nil {
		t.Fatalf("use = %v, %v", ok, err)
	}
	if ok, _ := s.HasSecretBypass(ctx, r.ID, "fp-a"); ok {
		t.Fatal("bypass not used up")
	}
	// One-time: a second (e.g. racing) push can't use it again.
	if ok, _ := s.UseSecretBypasses(ctx, r.ID, []string{"fp-a"}); ok {
		t.Fatal("bypass used twice")
	}
}
