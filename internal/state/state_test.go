package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KCaverly/caretaker/internal/agent"
)

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

func TestAgentsRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	s := Load()
	if got, active := s.Agents("repo/wt"); got != nil || active != 0 {
		t.Fatalf("unseen key should have no agents, got %v active=%d", got, active)
	}

	want := []AgentState{
		{Provider: agent.Claude, SessionID: "id-1", Label: "claude"},
		{Provider: agent.Codex, SessionID: "id-2", Label: "refactor auth"},
	}
	s.SetAgents("repo/wt", want, 1)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, active := Load().Agents("repo/wt")
	if active != 1 || len(got) != len(want) {
		t.Fatalf("round-trip mismatch: active=%d agents=%v", active, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("agent %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// An empty pool clears the entry so a later open starts fresh.
	s.SetAgents("repo/wt", nil, 0)
	if got, _ := s.Agents("repo/wt"); got != nil {
		t.Errorf("cleared pool should be nil, got %v", got)
	}
}

func TestLoadLegacyAgentDefaultsProviderToClaude(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	stateDir := filepath.Join(root, "ct")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"workspaces":{"repo/wt":{"agents":[{"session_id":"old-id","label":"old agent"}],"active_agent":0}}}`
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	got, _ := Load().Agents("repo/wt")
	if len(got) != 1 || got[0].Provider != agent.Claude {
		t.Fatalf("legacy agents = %+v, want Claude provider", got)
	}
}

func TestSetAgentsDefaultsMissingProviderToClaude(t *testing.T) {
	s := &State{Workspaces: map[string]*WorkspaceState{}}
	s.SetAgents("repo/wt", []AgentState{{SessionID: "id"}}, 0)
	got, _ := s.Agents("repo/wt")
	if len(got) != 1 || got[0].Provider != agent.Claude {
		t.Fatalf("agents = %+v, want Claude provider", got)
	}
}

func TestStaleSnapshotSkipped(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	s := Load()
	s.Touch("a/b")
	older, ok := s.Snapshot()
	if !ok {
		t.Fatal("snapshot should be available")
	}
	s.Touch("c/d")
	newer, _ := s.Snapshot()

	// The newer snapshot lands first; the stale one must not roll it back.
	if err := newer.Write(); err != nil {
		t.Fatal(err)
	}
	if err := older.Write(); err != nil {
		t.Fatal(err)
	}
	if got := Load(); got.Opened("c/d") == 0 {
		t.Fatal("stale snapshot write rolled back newer state")
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
