package accounts

import (
	"errors"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "accounts.json")
	return NewStore(p), p
}

func TestUpsertFromLoginCreatesWhenPoolEmpty(t *testing.T) {
	s, path := newTestStore(t)

	acc, ok := s.UpsertFromLogin("tok-1", "dev-1", "inst-1")
	if !ok {
		t.Fatal("expected persisted=true")
	}
	if acc.Token != "tok-1" || acc.DeviceID != "dev-1" || acc.InstallID != "inst-1" {
		t.Fatalf("unexpected credentials on new account: %+v", acc)
	}
	if !acc.Enabled {
		t.Fatal("a web-login account must be enabled")
	}
	if acc.Status != StatusReady {
		t.Fatalf("status = %q, want %q", acc.Status, StatusReady)
	}
	if s.Len() != 1 {
		t.Fatalf("pool length = %d, want 1", s.Len())
	}

	// The whole point of the fix: the credential must survive a restart.
	reloaded := NewStore(path)
	if reloaded.Len() != 1 {
		t.Fatalf("reloaded pool length = %d, want 1", reloaded.Len())
	}
	got, found := reloaded.Get(acc.ID)
	if !found {
		t.Fatalf("account %s not persisted", acc.ID)
	}
	if got.Token != "tok-1" {
		t.Fatalf("persisted token = %q, want tok-1", got.Token)
	}
}

func TestUpsertFromLoginRenewsMatchingInstallID(t *testing.T) {
	s, _ := newTestStore(t)

	first, ok := s.UpsertFromLogin("tok-1", "dev-1", "inst-1")
	if !ok {
		t.Fatal("first upsert failed")
	}
	second, ok := s.UpsertFromLogin("tok-2", "dev-1", "inst-1")
	if !ok {
		t.Fatal("second upsert failed")
	}
	if first.ID != second.ID {
		t.Fatalf("expected the same account to be renewed, got %s then %s", first.ID, second.ID)
	}
	if s.Len() != 1 {
		t.Fatalf("pool length = %d, want 1 (no duplicate account)", s.Len())
	}
	got, _ := s.Get(first.ID)
	if got.Token != "tok-2" {
		t.Fatalf("token = %q, want tok-2", got.Token)
	}
}

func TestUpsertFromLoginRejectsBlankToken(t *testing.T) {
	s, _ := newTestStore(t)

	if _, ok := s.UpsertFromLogin("   ", "dev-1", "inst-1"); ok {
		t.Fatal("expected ok=false for a blank token")
	}
	if s.Len() != 0 {
		t.Fatalf("pool length = %d, want 0", s.Len())
	}
}

func TestUpsertFromLoginClearsAuthFailedState(t *testing.T) {
	s, _ := newTestStore(t)

	acc, _ := s.UpsertFromLogin("tok-1", "dev-1", "inst-1")
	s.MarkFailure(acc.ID, errors.New("401 from upstream"), true)
	if got, _ := s.Get(acc.ID); got.Status != StatusAuthFailed {
		t.Fatalf("precondition: status = %q, want %q", got.Status, StatusAuthFailed)
	}

	renewed, ok := s.UpsertFromLogin("tok-2", "dev-1", "inst-1")
	if !ok {
		t.Fatal("renewal upsert failed")
	}
	if renewed.Status != StatusReady {
		t.Fatalf("status = %q, want %q after renewal", renewed.Status, StatusReady)
	}
	if renewed.LastError != "" {
		t.Fatalf("LastError = %q, want empty", renewed.LastError)
	}
}
