package state

import "testing"

func TestStateRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	s := Load()
	if got := s.Opened("repo/wt"); got != 0 {
		t.Fatalf("unseen key should be 0, got %d", got)
	}

	s.Touch("repo/wt")
	if s.Opened("repo/wt") == 0 {
		t.Fatal("Touch should record a non-zero time")
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A fresh load sees the persisted value.
	if got := Load().Opened("repo/wt"); got != s.Opened("repo/wt") {
		t.Fatalf("round-trip mismatch: %d vs %d", got, s.Opened("repo/wt"))
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := Load()
	if s.LastOpened == nil {
		t.Fatal("LastOpened should be initialised")
	}
	if len(s.LastOpened) != 0 {
		t.Fatalf("expected empty state, got %v", s.LastOpened)
	}
}
