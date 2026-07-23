package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"

	"github.com/KCaverly/caretaker/internal/agent"
	"github.com/KCaverly/caretaker/internal/config"
	"github.com/KCaverly/caretaker/internal/repo"
	"github.com/KCaverly/caretaker/internal/session"
	"github.com/KCaverly/caretaker/internal/usage"
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

func assertFrameFits(t *testing.T, m Model) {
	t.Helper()
	content := m.View().Content
	lines := strings.Split(content, "\n")
	if len(lines) > m.height {
		t.Errorf("frame height = %d, viewport = %d", len(lines), m.height)
	}
	for i, line := range lines {
		if got := lipgloss.Width(line); got > m.width {
			t.Errorf("line %d width = %d, viewport = %d: %q", i, got, m.width, ansi.Strip(line))
		}
	}
}

func TestResponsiveSurfaceMatrix(t *testing.T) {
	surfaces := []struct {
		name  string
		apply func(*Model)
	}{
		{"deck", func(m *Model) {}},
		{"agent board", func(m *Model) { m.boardOpen = true }},
		{"agent form", func(m *Model) { m.boardOpen, m.formOpen = true, true }},
		{"command palette", func(m *Model) { m.paletteOpen = true }},
		{"usage", func(m *Model) { m.usageOpen = true }},
		{"confirmation", func(m *Model) {
			m.mode = modeConfirmStop
			m.confirm = confirmState{title: "STOP WORKSPACE", context: []string{"repo / worktree"},
				options: []confirmOption{{label: "cancel", key: "esc"}, {label: "stop anyway", key: "y", danger: true}}}
		}},
		{"help", func(m *Model) { m.helpOpen = true }},
		{"setup", func(m *Model) { m.screen = screenSetup; m.configPath = "/a/very/long/config/path/config.toml" }},
		{"diff", func(m *Model) { m.diffOpen = true; m.diffView.loading = true }},
		{"stack", func(m *Model) {
			m.stackOpen = true
			m.stackView = stackView{repoName: "repo", wtName: "worktree", working: true}
		}},
	}
	sizes := []struct{ w, h int }{{24, 16}, {32, 18}, {48, 20}, {80, 24}}
	for _, size := range sizes {
		for _, surface := range surfaces {
			t.Run(fmt.Sprintf("%s/%dx%d", surface.name, size.w, size.h), func(t *testing.T) {
				m := sampleModel()
				mm, _ := m.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
				m = mm.(Model)
				surface.apply(&m)
				assertFrameFits(t, m)
			})
		}
	}
}

func TestBelowViableSizeShowsOnlyResizeInstruction(t *testing.T) {
	for _, size := range []struct{ w, h int }{{minViableWidth - 1, minViableHeight}, {minViableWidth, minViableHeight - 1}} {
		m := sampleModel()
		mm, _ := m.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
		m = mm.(Model)
		if got := m.View().Content; got != "ct — please enlarge the terminal" {
			t.Errorf("%dx%d rendered %q", size.w, size.h, got)
		}
	}
}

func TestNarrowBarPreservesActiveDestinationAndContext(t *testing.T) {
	m := sampleModel()
	m.current = &workspaceRef{repo: "caretaker", worktree: "polish", key: "caretaker/polish", ws: &session.Workspace{}}
	m.screen = screenTerminal
	mm, _ := m.Update(tea.WindowSizeMsg{Width: minViableWidth, Height: minViableHeight})
	m = mm.(Model)
	bar := m.renderBar()
	if !strings.Contains(bar, iconTerm) || !strings.Contains(bar, "caretaker / polish") {
		t.Errorf("narrow bar lost active destination or context:\n%s", bar)
	}
	if strings.Contains(bar, iconDeck) || strings.Contains(bar, iconEditor) || strings.Contains(bar, iconAgent) {
		t.Errorf("inactive destinations should yield on a narrow bar:\n%s", bar)
	}
	for i, line := range strings.Split(bar, "\n") {
		if width := lipgloss.Width(line); width > m.width {
			t.Errorf("bar line %d width = %d, viewport = %d", i, width, m.width)
		}
	}
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
	// The agent segment sits to the left of "repo / worktree" so the label
	// stays put at the right edge while agents hot-swap.
	if strings.Index(bar, "2/3") > strings.Index(bar, "r / w") {
		t.Errorf("agent position should precede the repo / worktree label:\n%s", bar)
	}

	// The position is agent-screen context (it advertises the prev/next-agent
	// keys); other screens omit it so the bar never shows a position the
	// current keys can't change.
	m.screen = screenEditor
	if bar := barLine(t, m); strings.Contains(bar, "2/3") {
		t.Errorf("agent position should not appear off the agent screen:\n%s", bar)
	}

	// A single-agent pool renders no position — nothing to rotate through.
	single := modelWithAgents(1)
	single.screen = screenAgent
	if bar := barLine(t, single); strings.Contains(bar, "1/1") {
		t.Errorf("bar should not show a position for a single agent:\n%s", bar)
	}
}

func TestBarShowsSingleAgentProviderIdentity(t *testing.T) {
	m := modelWithAgents(1)
	m.current.ws.Agents[0].Provider = agent.Codex
	m.current.ws.Agents[0].Title = "jade-otter"
	m.screen = screenAgent

	bar := barLine(t, m)
	if !strings.Contains(bar, "codex · jade-otter") {
		t.Errorf("bar should identify the focused provider and agent:\n%s", bar)
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
	// Like the agent segment, the pane segment sits to the left of the
	// right-anchored "repo / worktree" label.
	if strings.Index(bar, iconPanes) > strings.Index(bar, "r / w") {
		t.Errorf("pane position should precede the repo / worktree label:\n%s", bar)
	}
	// Unzoomed: the toggle offers "maximize" (zoom-in), never the restore icon.
	if !strings.Contains(bar, iconZoomIn) {
		t.Errorf("bar should show the zoom-in toggle while unzoomed:\n%s", bar)
	}
	if strings.Contains(bar, iconZoomOut) {
		t.Errorf("bar should not show the restore icon while unzoomed:\n%s", bar)
	}

	// Zoomed: the toggle flips to the restore (zoom-out) icon.
	ws.TermZoomed = true
	if bar := barLine(t, m); !strings.Contains(bar, iconZoomOut) {
		t.Errorf("bar should show the restore toggle while zoomed:\n%s", bar)
	}

	// Pane position is terminal-screen context; other screens omit it.
	m.screen = screenEditor
	if bar := barLine(t, m); strings.Contains(bar, iconPanes) {
		t.Errorf("pane indicator should not appear off the terminal screen:\n%s", bar)
	}
}

// leftClickAt is a left mouse-button click at bar coordinates (x, y).
func leftClickAt(x, y int) tea.MouseClickMsg {
	return tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}
}

// TestPaneZoomToggleClick checks the bar's zoom toggle is hit-tested at the
// tail of the pane segment and that clicking it toggles the pane zoom state.
func TestPaneZoomToggleClick(t *testing.T) {
	m := sampleModel()
	dir := t.TempDir()
	sleep := []string{"sleep", "5"}
	ws, err := m.mgr.Activate("r/w", dir,
		[]session.Spec{{Kind: session.Terminal, Argv: sleep}}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer m.mgr.CloseAll()
	if _, err := m.mgr.SplitTermPane("r/w", dir,
		session.Spec{Kind: session.Terminal, Argv: sleep}, session.SplitV, 80, 24); err != nil {
		t.Fatal(err)
	}
	m.current = &workspaceRef{repo: "r", worktree: "w", key: "r/w", path: dir, ws: ws}
	m.screen = screenTerminal

	// Locate the toggle's column: the pane segment leads barContextLabel, which
	// is right-anchored, and the icon is its final glyph.
	seg, ok := m.paneSegment()
	if !ok {
		t.Fatal("pane segment should apply on the terminal screen with 2 panes")
	}
	iconW := lipgloss.Width(m.paneZoomIcon())
	labelStart := m.width - lipgloss.Width(m.barContextLabel()+"  ")
	iconX := labelStart + lipgloss.Width(seg) - iconW

	if !m.paneZoomAt(iconX, 0) {
		t.Fatalf("zoom toggle should be hit-tested at column %d", iconX)
	}
	if m.paneZoomAt(labelStart, 0) {
		t.Error("the grid glyph at the segment head should not hit the zoom toggle")
	}

	// Clicking the toggle maximizes the active pane; clicking again restores it.
	if _, _ = m.handleMouseClick(leftClickAt(iconX, 0)); !ws.TermZoomed {
		t.Error("clicking the zoom toggle should maximize the pane")
	}
	if _, _ = m.handleMouseClick(leftClickAt(iconX, 0)); ws.TermZoomed {
		t.Error("clicking the restore toggle should return to the split layout")
	}
}

func TestCaretakerSeedlingTab(t *testing.T) {
	m := sampleModel()
	m.active = []activeItem{
		{repo: repo.Repo{Name: "other"}, view: WorktreeView{WT: repo.Worktree{Name: "wt", Path: "/other/wt"}}},
	}

	// On the picker the caretaker tab shows the seedling.
	if bar := barLine(t, m); !strings.Contains(bar, iconDeck) {
		t.Errorf("picker bar should show the seedling:\n%s", bar)
	}

	// Dropping into a session keeps the same glyph — only the colour changes,
	// which we can't observe after ansi.Strip, so assert glyph stability.
	m.current = &workspaceRef{repo: "r", worktree: "w", key: "r/w", path: "/r/w"}
	m.screen = screenEditor
	if bar := barLine(t, m); !strings.Contains(bar, iconDeck) {
		t.Errorf("session bar should still show the seedling:\n%s", bar)
	}

	// An agent elsewhere waiting on input must NOT change the icon: the "!"
	// badge carries the signal, the seedling stays put.
	m.agentStatus = map[int]AgentStatus{7: {Status: "waiting", Cwd: "/other/wt"}}
	bar := barLine(t, m)
	if !strings.Contains(bar, iconDeck) {
		t.Errorf("waiting agent must not change the caretaker icon:\n%s", bar)
	}
	if !strings.Contains(bar, "!") {
		t.Errorf("waiting agent should raise the ! badge:\n%s", bar)
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

func TestBoardAttentionStatusVocabulary(t *testing.T) {
	m := modelWithAgents(1)
	pid := m.current.ws.Agents[0].Pid()
	m.agentStatus = map[int]AgentStatus{}

	m.agentStatus[pid] = AgentStatus{Status: "waiting", WaitingFor: "permission"}
	if got, want := m.boardStatus(pid, attnWaiting), "waiting · permission"; got != want {
		t.Fatalf("waiting status = %q, want %q", got, want)
	}
	m.agentStatus[pid] = AgentStatus{Status: "idle"}
	if got, want := m.boardStatus(pid, attnDone), "ready · new output"; got != want {
		t.Fatalf("unread status = %q, want %q", got, want)
	}
	if got, want := m.boardStatus(pid, attnNone), "idle"; got != want {
		t.Fatalf("acknowledged status = %q, want %q", got, want)
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

// usageModel returns an agent-screen model with a fresh fabricated snapshot:
// a five-hour window at `five` percent (resetting in ~2h40m) and a seven-day
// window at `week` percent. No network is involved anywhere in these tests.
func usageModel(five, week float64) Model {
	m := modelWithAgents(1)
	m.screen = screenAgent
	m.usageThreshold = 50
	now := time.Now()
	m.usageSnap = usage.Snapshot{
		FiveHour:  &usage.Window{Utilization: five, ResetsAt: now.Add(2*time.Hour + 40*time.Minute + 30*time.Second)},
		SevenDay:  &usage.Window{Utilization: week, ResetsAt: now.Add(72 * time.Hour)},
		FetchedAt: now,
	}
	m.usageHave = true
	return m
}

func TestBarUsageSegmentGating(t *testing.T) {
	m := usageModel(68, 40)

	bar := barLine(t, m)
	if !strings.Contains(bar, "◕ 68% 2h40m") {
		t.Errorf("bar should show the session gauge with pie, percent, and reset:\n%s", bar)
	}
	// The gauge is the leftmost volatile segment: it hot-swaps every poll, so
	// it sits furthest from the right-anchored repo / worktree label.
	if strings.Index(bar, "68%") > strings.Index(bar, "r / w") {
		t.Errorf("usage segment should precede the repo / worktree label:\n%s", bar)
	}

	// Below the threshold the gauge stays out of the bar entirely.
	m.usageSnap.FiveHour.Utilization = 42
	if bar := barLine(t, m); strings.Contains(bar, "42%") {
		t.Errorf("segment should hide below the threshold:\n%s", bar)
	}

	// The gate is at-or-above: exactly the threshold shows.
	m.usageSnap.FiveHour.Utilization = 50
	if bar := barLine(t, m); !strings.Contains(bar, "50%") {
		t.Errorf("segment should show at exactly the threshold:\n%s", bar)
	}

	// Off the agent screen the gauge never appears, however hot the window —
	// the deck and every other screen stay untouched.
	m.usageSnap.FiveHour.Utilization = 95
	for _, s := range []screen{screenEditor, screenTerminal, screenPicker} {
		m.screen = s
		if bar := barLine(t, m); strings.Contains(bar, "95%") {
			t.Errorf("segment should not appear on screen %d:\n%s", s, bar)
		}
	}

	// A stale snapshot (older than 5 minutes) hides the segment: the number
	// may no longer be true, and a wrong gauge is worse than none.
	m.screen = screenAgent
	m.usageSnap.FetchedAt = time.Now().Add(-6 * time.Minute)
	if bar := barLine(t, m); strings.Contains(bar, "95%") {
		t.Errorf("a stale snapshot should hide the segment:\n%s", bar)
	}
}

func codexUsageSnapshot(session, week float64) usage.Snapshot {
	now := time.Now()
	return usage.Snapshot{
		Named: []usage.NamedWindow{
			{Label: "session", Session: true, Window: &usage.Window{Utilization: session, ResetsAt: now.Add(90 * time.Minute)}},
			{Label: "week", ShortLabel: "wk", Window: &usage.Window{Utilization: week, ResetsAt: now.Add(5 * 24 * time.Hour)}},
		},
		FetchedAt: now,
	}
}

func TestUsageBarMatchesActiveAgentProvider(t *testing.T) {
	cfg := config.Default()
	cfg.Agents.Enabled = []agent.Provider{agent.Claude, agent.Codex}
	cfg.Agents.Codex.Command = "/usr/bin/true"
	m := New(NewController(cfg), session.NewManager())
	m.width, m.height = 72, 24
	m.screen = screenAgent
	m.current = &workspaceRef{repo: "r", worktree: "w", ws: &session.Workspace{
		Agents: []*session.Session{
			{Provider: agent.Claude, Title: "amber-fox"},
			{Provider: agent.Codex, Title: "jade-otter"},
		},
	}}
	m.usageSnap = usageModel(80, 20).usageSnap
	m.usageHave = true
	m.codexUsageSnap = codexUsageSnapshot(60, 25)
	m.codexUsageHave = true

	// The Claude agent sees only the Claude estimate, even though a fresh Codex
	// estimate also exists.
	m.current.ws.ActiveAgent = 0
	if bar := barLine(t, m); !strings.Contains(bar, "80%") || strings.Contains(bar, "60%") {
		t.Errorf("Claude agent bar should show only Claude usage:\n%s", bar)
	}

	// Switching agents switches the estimate source; the stale provider's
	// notification must not remain in the bar.
	m.current.ws.ActiveAgent = 1
	if bar := barLine(t, m); !strings.Contains(bar, "60%") || strings.Contains(bar, "80%") {
		t.Errorf("Codex agent bar should show only Codex usage:\n%s", bar)
	}
}

func TestCodexOnlyExposesCodexUsage(t *testing.T) {
	cfg := config.Default()
	cfg.Agents.Enabled = []agent.Provider{agent.Codex}
	cfg.Agents.Default = agent.Codex
	cfg.Agents.Codex.Command = "/usr/bin/true"
	m := New(NewController(cfg), session.NewManager())
	m.width, m.height = 72, 24
	m.screen = screenAgent
	m.current = &workspaceRef{repo: "r", worktree: "w", ws: &session.Workspace{
		Agents: []*session.Session{{Provider: agent.Codex, Title: "jade-otter"}},
	}}
	m.codexUsageSnap = codexUsageSnapshot(72, 20)
	m.codexUsageHave = true

	if bar := barLine(t, m); !strings.Contains(bar, "72%") {
		t.Errorf("Codex-only bar should show Codex usage:\n%s", bar)
	}
	if out := renderToTerminal(t, m.renderUsage(m.height-barHeight), m.width, m.height-barHeight); !strings.Contains(out, "Codex") || strings.Contains(out, "Claude") {
		t.Errorf("Codex-only panel should show only Codex usage:\n%s", out)
	}
	if help := renderAllHelp(m); !strings.Contains(help, "usage limits") {
		t.Errorf("Codex-only help should expose usage:\n%s", help)
	}
}

func TestBarUsageSegmentWeekBinding(t *testing.T) {
	// The seven-day window binds (84 > 60): the segment flips to the wk label
	// with the reset weekday instead of the session countdown.
	m := usageModel(60, 84)
	bar := barLine(t, m)
	if !strings.Contains(bar, "wk 84%") {
		t.Errorf("bar should show the binding week window as wk:\n%s", bar)
	}
	weekday := strings.ToLower(m.usageSnap.SevenDay.ResetsAt.Local().Format("Mon"))
	if !strings.Contains(bar, weekday) {
		t.Errorf("week binding should show the reset weekday %q:\n%s", weekday, bar)
	}
	if strings.Contains(bar, "2h40m") {
		t.Errorf("week binding should not show the session countdown:\n%s", bar)
	}
}

func TestUsageSegmentClickOpensOverlay(t *testing.T) {
	m := usageModel(68, 40)
	seg, ok := m.usageSegment()
	if !ok {
		t.Fatal("usage segment should apply on the agent screen with a fresh snapshot")
	}
	// The segment leads the right-anchored context label; hit both edges.
	start := m.width - lipgloss.Width(m.barContextLabel()+"  ")
	for _, x := range []int{start, start + lipgloss.Width(seg) - 1} {
		if !m.usageZoneAt(x, 0) {
			t.Errorf("usage zone should be hit-tested at column %d", x)
		}
	}
	if m.usageZoneAt(start+lipgloss.Width(seg), 0) {
		t.Error("the separator after the segment should not hit the usage zone")
	}
	mm, _ := m.handleMouseClick(leftClickAt(start, 0))
	if !mm.(Model).usageOpen {
		t.Error("clicking the usage segment should open the usage overlay")
	}
}

func TestUsageOverlayRows(t *testing.T) {
	m := usageModel(68, 84)
	// A rising sample ring spanning >5 minutes: the burn row appears with a
	// rate and, since capping lands before the reset, the projection line.
	now := time.Now()
	m.usageHist = []usageSample{
		{at: now.Add(-10 * time.Minute), util: 60},
		{at: now.Add(-5 * time.Minute), util: 64},
		{at: now, util: 68},
	}

	out := renderToTerminal(t, m.renderUsage(m.height-barHeight), m.width, m.height-barHeight)
	for _, want := range []string{"USAGE", "session", "68%", "week", "84%", "resets", "%/hr", "at this pace", "esc"} {
		if !strings.Contains(out, want) {
			t.Errorf("overlay missing %q:\n%s", want, out)
		}
	}
	// No opus window on this plan: the row is skipped, not placeholdered.
	if strings.Contains(out, "opus") {
		t.Errorf("overlay should skip the absent opus window:\n%s", out)
	}

	// Only the five-hour window present: week vanishes too.
	m.usageSnap.SevenDay = nil
	m.usageHist = nil
	out = renderToTerminal(t, m.renderUsage(m.height-barHeight), m.width, m.height-barHeight)
	if strings.Contains(out, "week") {
		t.Errorf("overlay should skip the absent week window:\n%s", out)
	}

	// Before any snapshot has ever arrived the overlay says so.
	empty := modelWithAgents(1)
	empty.screen = screenAgent
	out = renderToTerminal(t, empty.renderUsage(empty.height-barHeight), empty.width, empty.height-barHeight)
	if !strings.Contains(out, "no usage data") {
		t.Errorf("overlay should report missing data before the first poll:\n%s", out)
	}
}

func TestUsageOverlayIncludesEveryEnabledProvider(t *testing.T) {
	m := usageModel(68, 84)
	m.agentProviders = []agent.Provider{agent.Claude, agent.Codex}
	m.codexUsageSnap = codexUsageSnapshot(31, 55)
	m.codexUsageHave = true

	out := renderToTerminal(t, m.renderUsage(m.height-barHeight), m.width, m.height-barHeight)
	for _, want := range []string{"Claude", "Codex", "68%", "84%", "31%", "55%"} {
		if !strings.Contains(out, want) {
			t.Errorf("multi-provider overlay missing %q:\n%s", want, out)
		}
	}
}

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
