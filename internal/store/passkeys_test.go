package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestPasskeys(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	id1, err := s.WebAuthnUserID(ctx)
	if err != nil || len(id1) != 32 {
		t.Fatalf("user id %x %v", id1, err)
	}
	id2, _ := s.WebAuthnUserID(ctx)
	if !bytes.Equal(id1, id2) {
		t.Fatal("WebAuthn user id is not stable")
	}

	p, err := s.AddPasskey(ctx, []byte{1, 2, 3}, "MacBook", []byte(`{"id":"AQID"}`), "deminimmo", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPasskey(ctx, []byte{1, 2, 3}, "dup", []byte(`{}`), "", ""); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("duplicate credential: %v", err)
	}
	got, err := s.PasskeyByCredentialID(ctx, []byte{1, 2, 3})
	if err != nil || got.ID != p.ID || string(got.Credential) != `{"id":"AQID"}` || got.Name != "MacBook" {
		t.Fatalf("by credential id: %+v %v", got, err)
	}
	if err := s.UsePasskey(ctx, p.ID, []byte(`{"id":"AQID","n":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.RenamePasskey(ctx, p.ID, "Work laptop"); err != nil {
		t.Fatal(err)
	}
	list, err := s.Passkeys(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "Work laptop" || list[0].LastUsedAt == nil || string(list[0].Credential) != `{"id":"AQID","n":2}` {
		t.Fatalf("Passkeys = %+v %v", list, err)
	}
	if err := s.RenamePasskey(ctx, 999, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename missing: %v", err)
	}
	if err := s.DeletePasskey(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePasskey(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}
