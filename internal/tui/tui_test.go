package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/KCaverly/caretaker/internal/config"
	"github.com/KCaverly/caretaker/internal/repo"
	"github.com/KCaverly/caretaker/internal/session"
)

func ctrlKey(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl} }

func TestScreenCycle(t *testing.T) {
	if screenEditor.next() != screenAgent {
		t.Error("editor should cycle to agent")
	}
	if screenAgent.next() != screenTerminal {
		t.Error("agent should cycle to terminal")
	}
	if screenTerminal.next() != screenEditor {
		t.Error("terminal should wrap to editor")
	}
}

func TestSessionIndex(t *testing.T) {
	if sessionIndex(screenEditor) != 0 || sessionIndex(screenAgent) != 1 || sessionIndex(screenTerminal) != 2 {
		t.Error("unexpected session index mapping")
	}
	if sessionIndex(screenPicker) != -1 {
		t.Error("picker has no session index")
	}
}

func TestBarShowsWorkspaceWhenActive(t *testing.T) {
	m := sampleModel()

	// No active workspace: icon-only tabs present, no repo/worktree label.
	bar := m.renderBar()
	for _, want := range []string{iconDeck, iconEditor, iconAgent, iconTerm} {
		if !strings.Contains(bar, want) {
			t.Errorf("bar missing tab icon %q", want)
		}
	}

	// Active workspace: repo / worktree shown.
	m.current = &workspaceRef{repo: "caretaker", worktree: "feat-login", key: "caretaker/feat-login"}
	m.screen = screenEditor
	bar = m.renderBar()
	if !strings.Contains(bar, "caretaker / feat-login") {
		t.Errorf("bar should show active repo/worktree:\n%s", bar)
	}
	if testing.Verbose() {
		t.Logf("\n%s", bar)
	}
}

func TestTabAtMapsIcons(t *testing.T) {
	m := sampleModel()

	if _, ok := m.tabAt(2, 1); ok {
		t.Error("tabAt should ignore non-bar rows")
	}

	// Scanning the bar row should hit the four tabs left-to-right in order,
	// regardless of each glyph's rendered width.
	var seen []screen
	for x := 0; x < 80; x++ {
		if s, ok := m.tabAt(x, 0); ok {
			if len(seen) == 0 || seen[len(seen)-1] != s {
				seen = append(seen, s)
			}
		}
	}
	want := []screen{screenPicker, screenEditor, screenAgent, screenTerminal}
	if len(seen) != len(want) {
		t.Fatalf("expected tabs %v, got %v", want, seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("tab order mismatch: got %v want %v", seen, want)
		}
	}
}

func TestSelectTabGating(t *testing.T) {
	m := sampleModel() // default screen is the picker

	// Session tabs are ignored until a workspace is active.
	if got := m.selectTab(screenEditor).(Model); got.screen != screenPicker {
		t.Error("session tab should be ignored without an active workspace")
	}

	m.current = &workspaceRef{repo: "r", worktree: "w", key: "r/w"}
	if got := m.selectTab(screenEditor).(Model); got.screen != screenEditor {
		t.Error("session tab should switch when a workspace is active")
	}

	// The picker tab is always reachable.
	m.screen = screenTerminal
	if got := m.selectTab(screenPicker).(Model); got.screen != screenPicker {
		t.Error("picker tab should always be reachable")
	}
}

func TestActiveSortByRecency(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // hermetic, empty state

	m := New(&Controller{}, session.NewManager())
	m.groups = []Group{{
		Repo: repo.Repo{Name: "r"},
		Worktrees: []WorktreeView{
			{WT: repo.Worktree{Repo: "r", Name: "a"}, CommitTime: 10},
			{WT: repo.Worktree{Repo: "r", Name: "b"}, CommitTime: 30},
			{WT: repo.Worktree{Repo: "r", Name: "c"}, CommitTime: 20},
		},
	}}

	// "a" was opened in ct most recently; "b"/"c" never opened, so they fall
	// back to commit time (b=30 before c=20).
	m.state.LastOpened["r/a"] = 1000
	m.recomputeActive()

	var got []string
	for _, it := range m.active {
		got = append(got, it.view.WT.Name)
	}
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("recency order: got %v want %v", got, want)
		}
	}
}

func TestRecentRanks(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	m := New(&Controller{}, session.NewManager())
	m.groups = []Group{
		{Repo: repo.Repo{Name: "r1"}, Worktrees: []WorktreeView{
			{WT: repo.Worktree{Repo: "r1", Name: "a"}},
			{WT: repo.Worktree{Repo: "r1", Name: "b"}},
		}},
		{Repo: repo.Repo{Name: "r2"}, Worktrees: []WorktreeView{
			{WT: repo.Worktree{Repo: "r2", Name: "c"}},
			{WT: repo.Worktree{Repo: "r2", Name: "d"}}, // never opened
		}},
	}
	m.state.LastOpened["r2/c"] = 300
	m.state.LastOpened["r1/b"] = 200
	m.state.LastOpened["r1/a"] = 100
	m.computeRecentRanks()

	for key, want := range map[string]int{"r2/c": 1, "r1/b": 2, "r1/a": 3} {
		if got := m.recentRank[key]; got != want {
			t.Errorf("rank %q: got %d want %d", key, got, want)
		}
	}
	if _, ok := m.recentRank["r2/d"]; ok {
		t.Error("never-opened worktree should not be ranked")
	}

	// The rank-1 worktree's row should show a leading "1".
	m.recomputeActive()
	row := m.activeRow(activeItem{repo: repo.Repo{Name: "r2"}, view: WorktreeView{WT: repo.Worktree{Repo: "r2", Name: "c"}}}, false, 40)
	if !strings.Contains(row, "1") {
		t.Errorf("rank-1 row should show 1: %q", row)
	}
}

func TestToUVKey(t *testing.T) {
	uvk := toUVKey(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if uvk.Code != 'a' || uvk.Text != "a" {
		t.Fatalf("toUVKey mapped wrong: %+v", uvk)
	}
}

// TestActivateFlow drives activate → cycle → return → re-activate with cheap
// child commands (no real nvim/claude needed) and a real session manager.
func TestActivateFlow(t *testing.T) {
	ctrl := &Controller{cfg: config.Config{
		Editor: "cat", Agent: "cat", Shell: "sh",
		Keys: config.Keys{Cycle: "ctrl+o", Picker: "ctrl+g"},
	}}
	mgr := session.NewManager()
	defer mgr.CloseAll()

	m := New(ctrl, mgr)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(Model)

	// Activate a workspace → lands in the editor view with 3 live sessions.
	mm, _ = m.activate("repo", "wt", t.TempDir())
	m = mm.(Model)
	if m.screen != screenEditor {
		t.Fatalf("activate should land in editor, got %v", m.screen)
	}
	if m.current == nil || len(m.current.sessions) != 3 {
		t.Fatalf("expected 3 sessions, got %+v", m.current)
	}
	if !mgr.Has("repo/wt") {
		t.Fatal("manager should track the activated workspace")
	}

	// Cycle right: editor → agent → terminal → editor.
	for _, want := range []screen{screenAgent, screenTerminal, screenEditor} {
		mm, _ = m.handleSessionKey(ctrlKey('o'))
		m = mm.(Model)
		if m.screen != want {
			t.Fatalf("cycle: got %v want %v", m.screen, want)
		}
	}

	// Return to picker; sessions persist.
	mm, _ = m.handleSessionKey(ctrlKey('g'))
	m = mm.(Model)
	if m.screen != screenPicker {
		t.Fatalf("expected picker, got %v", m.screen)
	}
	if m.current == nil || !mgr.Has("repo/wt") {
		t.Fatal("sessions should persist after returning to picker")
	}

	// Re-activating reuses the same sessions (no relaunch).
	before := m.current.sessions[0]
	mm, _ = m.activate("repo", "wt", t.TempDir())
	m = mm.(Model)
	if m.current.sessions[0] != before {
		t.Fatal("re-activate should reuse existing sessions")
	}
}
