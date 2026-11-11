package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestCreateFirstOwnerOnlyOnce(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	o, err := s.CreateFirstOwner(ctx, "zoe", "Zoe", "hash-1")
	if err != nil || o.Name != "zoe" || o.Kind != "user" {
		t.Fatalf("first = %+v, %v", o, err)
	}
	if _, err := s.CreateFirstOwner(ctx, "mallory", "", "hash-2"); !errors.Is(err, ErrAlreadySetUp) {
		t.Fatalf("second = %v, want ErrAlreadySetUp", err)
	}
	if h, _ := s.Setting(ctx, SettingPasswordHash); h != "hash-1" {
		t.Errorf("a refused setup changed the password hash to %q", h)
	}
	if _, err := s.OwnerByName(ctx, "mallory"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a refused setup created an owner: %v", err)
	}
}
