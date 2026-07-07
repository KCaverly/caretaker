package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"

	"github.com/KCaverly/caretaker/internal/session"
)

// renderToTerminal writes styled frame content into a real terminal emulator
// (the same vt engine ct uses to host sessions) and returns the visible
// screen as plain text — the closest test approximation of what a user sees,
// with ANSI styling and layout applied. Run tests with -v to eyeball the
// rendered frames.
func renderToTerminal(t *testing.T, content string, w, h int) string {
	t.Helper()
	emu := vt.NewEmulator(max(1, w), max(1, h))
	// Place each frame row with absolute cursor positioning, as Bubble Tea's
	// renderer does — writing full-width rows with bare newlines would let
	// autowrap scroll the frame out of the emulator.
	for i, line := range strings.Split(content, "\n") {
		_, _ = fmt.Fprintf(emu, "\x1b[%d;1H%s", i+1, line)
	}
	screen := ansi.Strip(emu.Render())
	if testing.Verbose() {
		t.Logf("rendered screen (%dx%d):\n%s", w, h, screen)
	}
	return screen
}

// screenText renders the model's full View. Only usable when every visible
// session is real (or none is visible, e.g. picker/board/help screens).
func screenText(t *testing.T, m Model) string {
	t.Helper()
	return renderToTerminal(t, m.View().Content, m.width, m.height)
}

// barLine renders just the status bar row through the terminal emulator.
func barLine(t *testing.T, m Model) string {
	t.Helper()
	return strings.Split(renderToTerminal(t, m.renderBar(), m.width, barHeight), "\n")[0]
}

func TestBarShowsAgentPoolPosition(t *testing.T) {
	m := modelWithAgents(3)
	m.current.ws.Agents[1].Title = "refactor-auth"
	m.current.ws.ActiveAgent = 1
	m.screen = screenAgent

	bar := barLine(t, m)
	if !strings.Contains(bar, "2/3") {
		t.Errorf("bar should show the agent pool position 2/3:\n%s", bar)
	}
	if !strings.Contains(bar, "refactor-auth") {
		t.Errorf("bar should show the focused agent's label:\n%s", bar)
	}

	// The position stays visible from other screens (it advertises the
	// prev/next-agent keys), still labelled with the focused agent.
	m.screen = screenEditor
	if bar := barLine(t, m); !strings.Contains(bar, "2/3") {
		t.Errorf("agent position should stay visible on the editor screen:\n%s", bar)
	}

	// A single-agent pool renders no position — nothing to rotate through.
	single := modelWithAgents(1)
	single.screen = screenAgent
	if bar := barLine(t, single); strings.Contains(bar, "1/1") {
		t.Errorf("bar should not show a position for a single agent:\n%s", bar)
	}
}

func TestBarShowsPanePositionAndZoom(t *testing.T) {
	m := modelWithAgents(1)
	ws := m.current.ws
	ws.Terms = []*session.Session{{}, {}, {}}
	ws.TermLayout = &session.PaneNode{Dir: session.SplitV,
		A: &session.PaneNode{Idx: 0},
		B: &session.PaneNode{Dir: session.SplitH, A: &session.PaneNode{Idx: 1}, B: &session.PaneNode{Idx: 2}},
	}
	ws.ActiveTerm = 1
	m.screen = screenTerminal

	bar := barLine(t, m)
	if !strings.Contains(bar, "2/3") {
		t.Errorf("bar should show the pane position 2/3 on the terminal screen:\n%s", bar)
	}
	if strings.Contains(bar, "zoom") {
		t.Errorf("bar should not show zoom while unzoomed:\n%s", bar)
	}

	ws.TermZoomed = true
	if bar := barLine(t, m); !strings.Contains(bar, "zoom") {
		t.Errorf("bar should flag the zoomed pane state:\n%s", bar)
	}

	// Pane position is terminal-screen context; other screens omit it.
	m.screen = screenEditor
	if bar := barLine(t, m); strings.Contains(bar, "⊞") {
		t.Errorf("pane indicator should not appear off the terminal screen:\n%s", bar)
	}
}

func TestBoardShowsElapsedBusyTime(t *testing.T) {
	m := modelWithAgents(2)
	pid1, pid2 := 101, 102
	// Sessions constructed without processes report pid 0; wire statuses via
	// the model's maps directly and read boardStatus, which the board renders.
	m.agentStatus = map[int]AgentStatus{
		pid1: {Status: "busy"},
		pid2: {Status: "busy"},
	}
	m.busySince = map[int]time.Time{
		pid1: time.Now().Add(-3 * time.Minute),
		// pid2 just started; below the 5s display threshold.
		pid2: time.Now(),
	}

	if got := m.boardStatus(pid1, attnNone); got != "working · 3m" {
		t.Errorf("boardStatus = %q, want %q", got, "working · 3m")
	}
	if got := m.boardStatus(pid2, attnNone); got != "working" {
		t.Errorf("fresh busy agent = %q, want plain %q", got, "working")
	}
}

func TestDeckLoadingState(t *testing.T) {
	// A fresh model has issued its first scan but received nothing yet: the
	// deck must say it's scanning, not claim the root is empty.
	m := New(&Controller{}, session.NewManager())
	mm, _ := m.Update(testWindowSize())
	m = mm.(Model)

	screen := screenText(t, m)
	if !strings.Contains(screen, "scanning") {
		t.Errorf("deck should show a scanning state before the first load:\n%s", screen)
	}
	if strings.Contains(screen, "no repos under root") {
		t.Errorf("deck must not claim an empty root before the first load:\n%s", screen)
	}

	// The scan lands (genuinely empty root): now the empty message is true.
	mm, _ = m.update(loadedMsg{groups: nil})
	m = mm.(Model)
	screen = screenText(t, m)
	if !strings.Contains(screen, "no repos under root") {
		t.Errorf("deck should report an empty root after the load:\n%s", screen)
	}
}

func TestDeckWheelScrolls(t *testing.T) {
	m := sampleModel()
	L := m.deckLayout(m.height - barHeight)

	// Wheel-down over the ACTIVE box moves its cursor and focuses the section.
	y := barHeight + L.newOuterH + 1 + 2 // first worktree row
	mm, _ := m.update(tea.MouseWheelMsg{X: 4, Y: y, Button: tea.MouseWheelDown})
	m = mm.(Model)
	if m.focus != focusActive || m.activeCursor != 1 {
		t.Fatalf("wheel over ACTIVE: focus=%v cursor=%d, want focusActive/1", m.focus, m.activeCursor)
	}

	// Wheel-up scrolls back and clamps at the top.
	for i := 0; i < 3; i++ {
		mm, _ = m.update(tea.MouseWheelMsg{X: 4, Y: y, Button: tea.MouseWheelUp})
		m = mm.(Model)
	}
	if m.activeCursor != 0 {
		t.Fatalf("wheel-up should clamp at 0, got %d", m.activeCursor)
	}

	// Wheel over the NEW box switches focus there and moves the repo cursor.
	mm, _ = m.update(tea.MouseWheelMsg{X: 4, Y: barHeight + 4, Button: tea.MouseWheelDown})
	m = mm.(Model)
	if m.focus != focusNew || m.newCursor != 1 {
		t.Fatalf("wheel over NEW: focus=%v cursor=%d, want focusNew/1", m.focus, m.newCursor)
	}
}

func TestRankedActiveLookup(t *testing.T) {
	m := sampleModel()
	key := wsKey(m.active[1].repo.Name, m.active[1].view.WT.Name)
	m.recentRank = map[string]int{key: 2}

	it, ok := m.rankedActive(2)
	if !ok || wsKey(it.repo.Name, it.view.WT.Name) != key {
		t.Fatalf("rankedActive(2) = %+v ok=%v, want %s", it, ok, key)
	}
	if _, ok := m.rankedActive(1); ok {
		t.Error("rankedActive(1) should miss when no worktree holds rank 1")
	}

	// The footer advertises the digit shortcut in the ACTIVE section.
	m.focus = focusActive
	if footer := renderToTerminal(t, m.renderFooter(), m.width, 3); !strings.Contains(footer, "1-3") {
		t.Errorf("ACTIVE footer should hint the 1-3 shortcut:\n%s", footer)
	}
}

func testWindowSize() tea.WindowSizeMsg { return tea.WindowSizeMsg{Width: 72, Height: 24} }

func TestHumanDur(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{42 * time.Second, "42s"},
		{89 * time.Second, "89s"},
		{4 * time.Minute, "4m"},
		{75 * time.Minute, "75m"},
		{3*time.Hour + 5*time.Minute, "3h05m"},
	}
	for _, c := range cases {
		if got := humanDur(c.d); got != c.want {
			t.Errorf("humanDur(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
