package state

import (
	"fmt"
	"testing"
)

// benchState builds a representative state: a dozen opened worktrees, three
// persisted agents each.
func benchState(b *testing.B) *State {
	b.Setenv("XDG_STATE_HOME", b.TempDir())
	s := Load()
	for i := 0; i < 12; i++ {
		key := fmt.Sprintf("repo-%d/feature-branch-%d", i, i)
		s.Touch(key)
		s.SetAgents(key, []AgentState{
			{SessionID: "aaaaaaaa-bbbb-cccc-dddd-000000000001", Label: "refactor auth"},
			{SessionID: "aaaaaaaa-bbbb-cccc-dddd-000000000002", Label: "write tests"},
			{SessionID: "aaaaaaaa-bbbb-cccc-dddd-000000000003", Label: ""},
		}, 1)
	}
	return s
}

// BenchmarkSaveSync measures the full synchronous save (marshal + write +
// rename) — the cost the UI goroutine used to pay inline on every agent
// rotate/focus/close keystroke.
func BenchmarkSaveSync(b *testing.B) {
	s := benchState(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Save(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSnapshot measures the in-memory marshal only — the cost those
// keystrokes pay now; the disk write happens on a background goroutine.
func BenchmarkSnapshot(b *testing.B) {
	s := benchState(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := s.Snapshot(); !ok {
			b.Fatal("snapshot unavailable")
		}
	}
}
