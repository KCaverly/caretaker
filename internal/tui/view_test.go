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

func TestRenderHelpKeys(t *testing.T) {
	// Defaults: the cycle fwd/back and goto rows show, and the retired notif
	// alias + pane-cycle rows are omitted.
	m := sampleModel()
	out := m.renderHelp(m.height - barHeight)
	for _, want := range []string{"alt+]", "alt+[", "alt+1", "alt+h", "alt+v", "cycle view"} {
		if !strings.Contains(out, want) {
			t.Errorf("help overlay missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "agent board (alias)") {
		t.Error("retired notif alias row should be hidden when keyNotif is empty")
	}
	if strings.Contains(out, "cycle pane focus") {
		t.Error("retired pane-cycle row should be hidden when keyTermCycle is empty")
	}

	// When a user re-enables the aliases, their rows reappear.
	m.keyNotif = "ctrl+n"
	m.keyTermCycle = "ctrl+w"
	out = m.renderHelp(m.height - barHeight)
	if !strings.Contains(out, "agent board (alias)") || !strings.Contains(out, "cycle pane focus") {
		t.Errorf("rebound alias rows should appear:\n%s", out)
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

	// A live-waiting agent elsewhere shows "!", a stored unread marker shows "*".
	// (The waiting agent is in another worktree so it maps via m.active.)
	m.active = []activeItem{
		{repo: repo.Repo{Name: "other"}, view: WorktreeView{WT: repo.Worktree{Name: "wt", Path: "/other/wt"}}},
	}
	m.agentStatus = map[int]AgentStatus{7: {Status: "waiting", Cwd: "/other/wt"}}
	m.attention[8] = attnEntry{level: attnDone, key: "r/w"}
	bar = m.renderBar()
	if !strings.Contains(bar, "!") {
		t.Errorf("bar should show ! for a live-waiting agent:\n%s", bar)
	}
	if !strings.Contains(bar, "*") {
		t.Errorf("bar should show * for an unread completion:\n%s", bar)
	}

	// Board header should show the agent count when open.
	m.boardOpen = true
	m.boardCursor = 0
	board := m.renderBoard(m.height - barHeight)
	if !strings.Contains(board, "2") {
		t.Errorf("board header should show pool count 2:\n%s", board)
	}
}

func TestBoardRender(t *testing.T) {
	m := modelWithAgents(2)
	mm, _ := m.openBoard()
	m = mm.(Model)
	out := m.renderBoard(m.height - barHeight)
	for _, want := range []string{"AGENTS", "r/w", "claude", "new agent", "focus", "current"} {
		if !strings.Contains(out, want) {
			t.Errorf("board missing %q:\n%s", want, out)
		}
	}

	// The form renders the prompt input and both toggles.
	m = m.openNewAgentForm().(Model)
	out = m.renderBoard(m.height - barHeight)
	for _, want := range []string{"NEW AGENT", "prompt", "active worktree", "background", "launch"} {
		if !strings.Contains(out, want) {
			t.Errorf("form missing %q:\n%s", want, out)
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
