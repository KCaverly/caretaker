package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/KCaverly/caretaker/internal/config"
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
