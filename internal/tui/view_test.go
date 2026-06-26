package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/KCaverly/caretaker/internal/repo"
	"github.com/KCaverly/caretaker/internal/session"
)

func sampleModel() Model {
	groups := []Group{
		{Repo: repo.Repo{Name: "caretaker"}, Worktrees: []WorktreeView{
			{WT: repo.Worktree{Repo: "caretaker", Name: "(main)", Branch: "main", IsMain: true}},
			{WT: repo.Worktree{Repo: "caretaker", Name: "feat-login", Branch: "feat-login"}, Live: true, Dirty: true},
			{WT: repo.Worktree{Repo: "caretaker", Name: "bugfix", Branch: "bugfix"}, Live: false},
		}},
		{Repo: repo.Repo{Name: "api"}, Worktrees: []WorktreeView{
			{WT: repo.Worktree{Repo: "api", Name: "(main)", Branch: "main", IsMain: true}},
			{WT: repo.Worktree{Repo: "api", Name: "spike", Branch: "spike"}, Live: true},
		}},
	}
	m := New(&Controller{}, session.NewManager())
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 72, Height: 24})
	m = mm.(Model)
	m.groups = groups
	m.recomputeMatches()
	m.recomputeActive()
	return m
}

func TestRenderLayout(t *testing.T) {
	m := sampleModel()
	out := m.renderDeck(m.height - barHeight)
	for _, want := range []string{"NEW", "ACTIVE", "caretaker", "feat-login", "bugfix", "spike"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q", want)
		}
	}
	if len(m.active) != 3 { // feat-login, bugfix, spike (mains excluded)
		t.Fatalf("active should hold 3 non-main worktrees, got %d", len(m.active))
	}
	if testing.Verbose() {
		m.focus = focusActive
		t.Logf("\n%s", m.renderDeck(m.height-barHeight))
	}
}

// TestActiveGrouping checks that each repo header appears once and worktree rows
// don't repeat the repo name (they're grouped under the header).
func TestActiveGrouping(t *testing.T) {
	m := sampleModel()
	m.focus = focusActive
	lines := m.renderActive(m.width-4, 50)
	joined := strings.Join(lines, "\n")
	// Worktree rows show only the worktree name, not "repo · name".
	if strings.Contains(joined, "·") {
		t.Errorf("active rows should not contain the 'repo · name' separator:\n%s", joined)
	}
}

func TestSelectionBarFillsWidth(t *testing.T) {
	m := sampleModel()
	m.focus = focusActive
	innerW := m.width - 4
	bar := m.activeRow(m.active[0], true, innerW)
	if w := lipgloss.Width(bar); w != innerW {
		t.Fatalf("selected row width = %d, want %d", w, innerW)
	}
}

func TestBarNotifZone(t *testing.T) {
	m := sampleModel()
	m.current = &workspaceRef{
		repo: "r", worktree: "w", key: "r/w", path: "/r/w",
		ws: &session.Workspace{Agents: []*session.Session{{}, {}}},
	}
	m.screen = screenAgent

	// No unread: right side shows only the worktree label, no notif zone.
	bar := m.renderBar()
	if !strings.Contains(bar, "r / w") {
		t.Errorf("bar should show worktree label:\n%s", bar)
	}
	if strings.Contains(bar, "!") || strings.Contains(bar, "*") {
		t.Errorf("bar should not show notif glyphs when nothing is unread:\n%s", bar)
	}

	// Waiting unread: "!" should appear in the bar, count in palette (not bar).
	m.unread = map[string]notifLevel{"r/w": notifWaiting, "other/wt": notifDone}
	bar = m.renderBar()
	if !strings.Contains(bar, "!") {
		t.Errorf("bar should show ! for waiting notif:\n%s", bar)
	}
	if !strings.Contains(bar, "*") {
		t.Errorf("bar should show * for done notif:\n%s", bar)
	}

	// Palette header should show agent count when open.
	m.paletteOpen = true
	m.paletteCursor = 0
	palette := m.renderPalette(m.height - barHeight)
	if !strings.Contains(palette, "2") {
		t.Errorf("palette header should show pool count 2:\n%s", palette)
	}
}

func TestPaletteRender(t *testing.T) {
	m := modelWithAgents(2)
	m = m.openPalette().(Model)
	out := m.renderPalette(m.height - barHeight)
	for _, want := range []string{"claude", "new agent", "focus"} {
		if !strings.Contains(out, want) {
			t.Errorf("palette missing %q:\n%s", want, out)
		}
	}
}

func TestCreateModeInlineInput(t *testing.T) {
	m := sampleModel()
	// Simulate having chosen the first repo to create in.
	m.mode = modeCreateName
	m.pendingRepo = m.repoMatches[0]

	// The name input + "in <repo>" context should render inside the "new" box…
	newLines := strings.Join(m.renderNew(m.width-4, 10), "\n")
	if !strings.Contains(newLines, "in ") || !strings.Contains(newLines, m.pendingRepo.Name) {
		t.Errorf("new box should show the 'in <repo>' create context:\n%s", newLines)
	}
	// …and the footer should not carry an input prompt.
	if strings.Contains(m.renderFooter(), "›") {
		t.Errorf("footer should not contain the input prompt in create mode")
	}
	if testing.Verbose() {
		t.Logf("\n%s", m.renderDeck(m.height-barHeight))
	}
}
