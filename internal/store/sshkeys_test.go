package store

import (
	"context"
	"errors"
	"testing"
)

func TestSSHKeys(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	k, err := s.AddSSHKey(ctx, "laptop", "ssh-ed25519", "SHA256:abc", "ssh-ed25519 AAAA", "deminimmo", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if k.ID == 0 || k.Name != "laptop" || k.Kind != "owner" || k.LastUsedAt != nil || k.AddedBy != "deminimmo" {
		t.Fatalf("unexpected key %+v", k)
	}
	if _, err := s.AddSSHKey(ctx, "again", "ssh-ed25519", "SHA256:abc", "ssh-ed25519 AAAA", "", ""); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("duplicate fingerprint: %v", err)
	}
	if _, err := s.AddSSHKey(ctx, "phone", "ecdsa-sha2-nistp256", "SHA256:def", "ecdsa-sha2-nistp256 AAAA", "", ""); err != nil {
		t.Fatal(err)
	}
	keys, err := s.SSHKeys(ctx)
	if err != nil || len(keys) != 2 || keys[0].Name != "phone" {
		t.Fatalf("SSHKeys = %+v, %v", keys, err)
	}

	got, err := s.SSHKeyByFingerprint(ctx, "SHA256:abc")
	if err != nil || got.ID != k.ID {
		t.Fatalf("by fingerprint: %+v %v", got, err)
	}
	if _, err := s.SSHKeyByFingerprint(ctx, "SHA256:nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing fingerprint: %v", err)
	}
	if err := s.TouchSSHKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.SSHKeyByFingerprint(ctx, "SHA256:abc")
	if got.LastUsedAt == nil {
		t.Fatal("last used not recorded")
	}
	if err := s.DeleteSSHKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSSHKey(ctx, k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if keys, _ := s.SSHKeys(ctx); len(keys) != 1 {
		t.Fatalf("after delete: %+v", keys)
	}
}
