package tui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/KCaverly/caretaker/internal/agent"
	"github.com/KCaverly/caretaker/internal/codex"
	"github.com/KCaverly/caretaker/internal/config"
	"github.com/KCaverly/caretaker/internal/repo"
	"github.com/KCaverly/caretaker/internal/session"
	"github.com/KCaverly/caretaker/internal/state"
)

func ctrlKey(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl} }

// altKey builds an alt-modified key press. Text is deliberately left empty so
// Key.String() falls through to the "alt+<r>" keystroke form (a non-empty Text
// would be returned verbatim instead).
func altKey(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Mod: tea.ModAlt} }

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

// TestScreenCyclePrev checks the reverse cycle wraps the other way and is the
// inverse of next across the three session views.
func TestScreenCyclePrev(t *testing.T) {
	if screenEditor.prev() != screenTerminal {
		t.Error("editor should wrap back to terminal")
	}
	if screenTerminal.prev() != screenAgent {
		t.Error("terminal should cycle back to agent")
	}
	if screenAgent.prev() != screenEditor {
		t.Error("agent should cycle back to editor")
	}
	// prev is the inverse of next over the session views.
	for _, s := range []screen{screenEditor, screenAgent, screenTerminal} {
		if got := s.next().prev(); got != s {
			t.Errorf("next then prev from %v = %v, want %v", s, got, s)
		}
		if got := s.prev().next(); got != s {
			t.Errorf("prev then next from %v = %v, want %v", s, got, s)
		}
	}
}

func TestActiveSessionByScreen(t *testing.T) {
	ed := &session.Session{}
	ag0 := &session.Session{}
	ag1 := &session.Session{}
	tm := &session.Session{}
	ws := &session.Workspace{Editor: ed, Terms: []*session.Session{tm}, TermLayout: &session.PaneNode{Idx: 0}, Agents: []*session.Session{ag0, ag1}, ActiveAgent: 1}

	m := sampleModel()
	m.current = &workspaceRef{repo: "r", worktree: "w", key: "r/w", ws: ws}

	m.screen = screenEditor
	if m.activeSession() != ed {
		t.Error("editor screen should resolve to the editor session")
	}
	m.screen = screenTerminal
	if m.activeSession() != tm {
		t.Error("terminal screen should resolve to the term session")
	}
	m.screen = screenAgent
	if m.activeSession() != ag1 {
		t.Error("agent screen should resolve to the active agent")
	}
	m.screen = screenPicker
	if m.activeSession() != nil {
		t.Error("picker has no session")
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

	tab := func(m Model, s screen) Model {
		mm, _ := m.selectTab(s)
		return mm.(Model)
	}

	// Session tabs are ignored until a workspace is active.
	if got := tab(m, screenEditor); got.screen != screenPicker {
		t.Error("session tab should be ignored without an active workspace")
	}

	m.current = &workspaceRef{repo: "r", worktree: "w", key: "r/w"}
	if got := tab(m, screenEditor); got.screen != screenEditor {
		t.Error("session tab should switch when a workspace is active")
	}

	// The picker tab is always reachable, and entering it refreshes the deck.
	m.screen = screenTerminal
	mm, cmd := m.selectTab(screenPicker)
	if mm.(Model).screen != screenPicker {
		t.Error("picker tab should always be reachable")
	}
	if cmd == nil {
		t.Error("entering the picker from a session should trigger a deck refresh")
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

func TestMostRecentWorktree(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	m := New(&Controller{}, session.NewManager())
	m.groups = []Group{
		{Repo: repo.Repo{Name: "r1"}, Worktrees: []WorktreeView{
			{WT: repo.Worktree{Repo: "r1", Name: "a", Path: "/p/a"}},
		}},
		{Repo: repo.Repo{Name: "r2"}, Worktrees: []WorktreeView{
			{WT: repo.Worktree{Repo: "r2", Name: "b", Path: "/p/b"}},
		}},
	}
	m.state.LastOpened["r1/a"] = 100
	m.state.LastOpened["r2/b"] = 200

	r, w, p, ok := m.mostRecentWorktree()
	if !ok || r != "r2" || w != "b" || p != "/p/b" {
		t.Fatalf("got %q/%q/%q ok=%v, want r2/b//p/b", r, w, p, ok)
	}

	// No history → not ok.
	empty := New(&Controller{}, session.NewManager())
	if _, _, _, ok := empty.mostRecentWorktree(); ok {
		t.Error("empty history should return ok=false")
	}
}

func TestPickerKeyJumpsToRecent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	keys := config.Default().Keys
	keys.Cycle = "ctrl+o"
	ctrl := &Controller{cfg: config.Config{
		Editor: "cat", Agent: "cat", Shell: "sh",
		Keys: keys,
	}}
	mgr := session.NewManager()
	defer mgr.CloseAll()

	m := New(ctrl, mgr)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(Model)
	m.groups = []Group{{Repo: repo.Repo{Name: "repo"}, Worktrees: []WorktreeView{
		{WT: repo.Worktree{Repo: "repo", Name: "wt", Path: t.TempDir()}},
	}}}
	m.state.LastOpened["repo/wt"] = 100

	// In the picker, the picker key jumps to the most recent worktree.
	mm, _ = m.handlePicker(ctrlKey('g'))
	m = mm.(Model)
	if m.screen != screenEditor {
		t.Fatalf("expected editor after ctrl+g, got %v", m.screen)
	}
	if m.current == nil || m.current.key != "repo/wt" {
		t.Fatalf("expected current repo/wt, got %+v", m.current)
	}
}

func TestDeckClickNewSection(t *testing.T) {
	m := sampleModel() // 72x24, focus defaults to focusNew

	L := m.deckLayout(m.height - barHeight)
	nStart, _ := windowBounds(len(m.repoMatches), m.newCursor, L.newRows)
	// Repo list begins at content line 4 of the NEW box (body row 1+4).
	yRepo := func(idx int) int { return barHeight + 1 + 4 + (idx - nStart) }

	if m.repoMatches[1].Name != "api" {
		t.Fatalf("expected api as repo index 1, got %q", m.repoMatches[1].Name)
	}
	mm, _, ok := m.deckClick(5, yRepo(1))
	m = mm.(Model)
	if !ok || m.newCursor != 1 {
		t.Fatalf("click should select repo 1: ok=%v cursor=%d", ok, m.newCursor)
	}
	// Clicking the already-selected repo starts the create flow.
	mm, _, ok = m.deckClick(5, yRepo(1))
	m = mm.(Model)
	if !ok || m.mode != modeCreateName || m.pendingRepo.Name != "api" {
		t.Fatalf("reselect should begin create in api: ok=%v mode=%v repo=%q", ok, m.mode, m.pendingRepo.Name)
	}
}

func TestDeckClickActiveSection(t *testing.T) {
	m := sampleModel() // focus starts on NEW

	L := m.deckLayout(m.height - barHeight)
	_, rowItem := m.activeDisplay(m.width - 4)
	start, _ := activeWindowStart(rowItem, m.activeCursor, L.activeRows)
	// Worktree rows begin at content line 2 of the ACTIVE box.
	yRow := func(di int) int { return barHeight + L.newOuterH + 1 + 2 + (di - start) }

	// rowItem = [-1 caretaker, 0 feat-login, 1 bugfix, -1 api, 2 spike].
	mm, _, ok := m.deckClick(5, yRow(1))
	m = mm.(Model)
	if !ok || m.focus != focusActive || m.activeCursor != 0 {
		t.Fatalf("click feat-login: ok=%v focus=%v cursor=%d", ok, m.focus, m.activeCursor)
	}
	// A repo-header row is not a selectable target.
	if _, _, ok := m.deckClick(5, yRow(0)); ok {
		t.Error("clicking a repo header should not be handled")
	}
	// Clicking another worktree moves the selection.
	mm, _, ok = m.deckClick(5, yRow(2))
	m = mm.(Model)
	if !ok || m.activeCursor != 1 {
		t.Fatalf("click bugfix: ok=%v cursor=%d", ok, m.activeCursor)
	}
}

// TestDeckClickDetailLineMisses proves the └ work-state detail line under the
// focused worktree row is not a click target: its rowItem entry is -1 (like a
// repo header), so a click on it neither selects nor activates anything.
func TestDeckClickDetailLineMisses(t *testing.T) {
	m := sampleModel()
	m.focus = focusActive
	m.activeCursor = 0
	// Give the cursor row work-state so its detail line actually renders.
	m.active[0].view.HasBase = true
	m.active[0].view.Ahead = 2

	L := m.deckLayout(m.height - barHeight)
	display, rowItem := m.activeDisplay(m.width - 4)
	start, _ := activeWindowStart(rowItem, m.activeCursor, L.activeRows)
	// display = [caretaker(-1), feat-login(0), └ detail(-1), bugfix(1), api(-1), spike(2)].
	if len(rowItem) != 6 || rowItem[2] != -1 || !strings.Contains(display[2], "└") {
		t.Fatalf("expected the detail line at display index 2 with rowItem -1, got %v", rowItem)
	}
	yRow := func(di int) int { return barHeight + L.newOuterH + 1 + 2 + (di - start) }

	// A click on the detail line is a miss: unhandled, cursor and focus intact.
	mm, _, ok := m.deckClick(5, yRow(2))
	m = mm.(Model)
	if ok {
		t.Error("clicking the detail line should not be handled")
	}
	if m.activeCursor != 0 || m.focus != focusActive || m.current != nil {
		t.Fatalf("detail-line click must not select or activate: cursor=%d focus=%v current=%+v",
			m.activeCursor, m.focus, m.current)
	}

	// The row beneath it (bugfix, shifted down by the detail line) still selects.
	mm, _, ok = m.deckClick(5, yRow(3))
	m = mm.(Model)
	if !ok || m.activeCursor != 1 {
		t.Fatalf("click below the detail line should select bugfix: ok=%v cursor=%d", ok, m.activeCursor)
	}
}

func TestDeckClickOpensSelectedWorktree(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	keys := config.Default().Keys
	keys.Cycle = "ctrl+o"
	ctrl := &Controller{cfg: config.Config{
		Editor: "cat", Agent: "cat", Shell: "sh",
		Keys: keys,
	}}
	mgr := session.NewManager()
	defer mgr.CloseAll()

	m := New(ctrl, mgr)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(Model)
	m.groups = []Group{{Repo: repo.Repo{Name: "repo"}, Worktrees: []WorktreeView{
		{WT: repo.Worktree{Repo: "repo", Name: "wt", Path: t.TempDir()}},
	}}}
	m.recomputeActive()
	m.focus, m.activeCursor = focusActive, 0

	L := m.deckLayout(m.height - barHeight)
	_, rowItem := m.activeDisplay(m.width - 4)
	start, _ := activeWindowStart(rowItem, m.activeCursor, L.activeRows)
	// display = [repo header(-1), wt(0)] → the worktree sits at display index 1.
	y := barHeight + L.newOuterH + 1 + 2 + (1 - start)

	mm, _, ok := m.deckClick(5, y) // already-selected row → open
	m = mm.(Model)
	if !ok || m.current == nil || m.current.key != "repo/wt" || m.screen != screenEditor {
		t.Fatalf("reselect-click should open the worktree: ok=%v current=%+v screen=%v", ok, m.current, m.screen)
	}
}

func TestHelpOverlayToggle(t *testing.T) {
	m := sampleModel()
	m.keys.Help = "f1"

	// "?" opens help from the deck.
	mm, _ := m.handleKey(tea.KeyPressMsg{Code: '?', Text: "?"})
	m = mm.(Model)
	if !m.helpOpen {
		t.Fatal("'?' should open the help overlay in the deck")
	}
	// Any key closes it.
	mm, _ = m.handleKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m = mm.(Model)
	if m.helpOpen {
		t.Fatal("any key should close the help overlay")
	}

	// The help key works from inside a session too (where "?" must reach the
	// program), and the overlay lists the configured session bindings + legend.
	m.current = &workspaceRef{repo: "r", worktree: "w", key: "r/w"}
	m.screen = screenEditor
	mm, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyF1})
	m = mm.(Model)
	if !m.helpOpen {
		t.Fatal("the help key should open help from a session")
	}
	out := m.renderHelp(m.height - barHeight)
	for _, want := range []string{"HELP", "Session", "Legend", m.keys.Cycle, m.keys.Picker, m.keys.Palette, "uncommitted"} {
		if !strings.Contains(out, want) {
			t.Errorf("help overlay missing %q:\n%s", want, out)
		}
	}
}

// workspaceWith builds a model whose current workspace has n agents.
func modelWithAgents(n int) Model {
	agents := make([]*session.Session, n)
	for i := range agents {
		agents[i] = &session.Session{}
	}
	m := sampleModel()
	ws := &session.Workspace{Agents: agents}
	m.current = &workspaceRef{repo: "r", worktree: "w", key: "r/w", path: "/r/w", ws: ws}
	m.screen = screenEditor
	return m
}

func TestStatusTickInterval(t *testing.T) {
	// Nothing hosted and nothing tracked: slow idle watch.
	m := sampleModel()
	if got := m.statusTickInterval(); got != 30*time.Second {
		t.Errorf("empty deck interval = %v, want 30s", got)
	}

	// A busy agent (even one started outside ct) forces the fast cadence.
	m.agentStatus = map[int]AgentStatus{7: {Status: "busy"}}
	if got := m.statusTickInterval(); got != 2*time.Second {
		t.Errorf("busy interval = %v, want 2s", got)
	}

	// Idle agents with a live workspace: the medium cadence.
	m.agentStatus = map[int]AgentStatus{7: {Status: "idle"}}
	if _, err := m.mgr.Activate("r/w", t.TempDir(),
		[]session.Spec{{Kind: session.Terminal, Argv: []string{"sleep", "5"}}}, 80, 24); err != nil {
		t.Fatal(err)
	}
	defer m.mgr.CloseAll()
	if got := m.statusTickInterval(); got != 5*time.Second {
		t.Errorf("live-workspace idle interval = %v, want 5s", got)
	}
}

func TestPickerKeyRefreshesDeck(t *testing.T) {
	m := modelWithAgents(1)
	m.screen = screenEditor

	mm, cmd := m.handleKey(ctrlKey('g'))
	if mm.(Model).screen != screenPicker {
		t.Fatal("picker key should return to the deck")
	}
	if cmd == nil {
		t.Error("returning to the deck should trigger a refresh")
	}
}

func TestStaleLoadDropped(t *testing.T) {
	m := sampleModel()
	first := m.ctrl.loadSeq.Add(1)  // an older in-flight load
	latest := m.ctrl.loadSeq.Add(1) // superseded by this newer one

	fresh := []Group{{Repo: repo.Repo{Name: "fresh"}}}
	mm, _ := m.update(loadedMsg{groups: fresh, seq: latest})
	m = mm.(Model)
	if len(m.groups) != 1 || m.groups[0].Repo.Name != "fresh" {
		t.Fatalf("latest load should apply, got %+v", m.groups)
	}

	// The older load finishes late; its result must not roll the deck back.
	stale := []Group{{Repo: repo.Repo{Name: "stale"}}}
	mm, _ = m.update(loadedMsg{groups: stale, seq: first})
	m = mm.(Model)
	if m.groups[0].Repo.Name != "fresh" {
		t.Fatal("stale load result clobbered newer state")
	}
}

func TestRotateAgentWraps(t *testing.T) {
	m := modelWithAgents(3)

	rotate := func(m Model, delta int) Model {
		mm, _ := m.rotateAgent(delta)
		return mm.(Model)
	}

	m = rotate(m, +1)
	if m.current.ws.ActiveAgent != 1 || m.screen != screenAgent {
		t.Fatalf("next: active=%d screen=%v", m.current.ws.ActiveAgent, m.screen)
	}
	m = rotate(m, +1)
	m = rotate(m, +1) // 2 -> wrap to 0
	if m.current.ws.ActiveAgent != 0 {
		t.Fatalf("expected wrap to 0, got %d", m.current.ws.ActiveAgent)
	}
	m = rotate(m, -1) // 0 -> wrap to 2
	if m.current.ws.ActiveAgent != 2 {
		t.Fatalf("expected wrap to 2, got %d", m.current.ws.ActiveAgent)
	}

	// No agents: rotate is a no-op and doesn't switch the screen.
	if got := rotate(modelWithAgents(0), +1); got.screen == screenAgent {
		t.Error("rotate with no agents should not switch to the agent view")
	}
}

func TestBoardNavigateAndFocus(t *testing.T) {
	m := modelWithAgents(3)
	m.current.ws.ActiveAgent = 0

	mm, _ := m.openBoard()
	m = mm.(Model)
	if !m.boardOpen || m.boardCursor != 0 {
		t.Fatalf("open: open=%v cursor=%d", m.boardOpen, m.boardCursor)
	}

	// Down moves the cursor; it can reach the trailing "+ new agent" row (nav
	// index 3) but no further.
	for i := 0; i < 5; i++ {
		mm, _ := m.handleBoard(tea.KeyPressMsg{Code: tea.KeyDown})
		m = mm.(Model)
	}
	if m.boardCursor != 3 {
		t.Fatalf("cursor should clamp at the new-agent row (3), got %d", m.boardCursor)
	}

	// Up back to agent 1, then enter focuses it and closes the board.
	for i := 0; i < 2; i++ {
		mm, _ := m.handleBoard(tea.KeyPressMsg{Code: tea.KeyUp})
		m = mm.(Model)
	}
	mm, _ = m.handleBoard(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if m.boardOpen {
		t.Error("enter should close the board")
	}
	if m.current.ws.ActiveAgent != 1 || m.screen != screenAgent {
		t.Fatalf("enter should focus agent 1 on the agent screen, got active=%d screen=%v", m.current.ws.ActiveAgent, m.screen)
	}
}

func TestBoardDigitJump(t *testing.T) {
	m := modelWithAgents(3)
	m.current.ws.ActiveAgent = 0
	mm, _ := m.openBoard()
	m = mm.(Model)

	// "3" jumps straight to the third agent, focuses it, and closes the board.
	mm, _ = m.handleBoard(tea.KeyPressMsg{Code: '3', Text: "3"})
	m = mm.(Model)
	if m.current.ws.ActiveAgent != 2 || m.screen != screenAgent || m.boardOpen {
		t.Fatalf("digit jump: active=%d screen=%v open=%v", m.current.ws.ActiveAgent, m.screen, m.boardOpen)
	}

	// A digit past the pool size is ignored (board stays open).
	mm, _ = m.openBoard()
	m = mm.(Model)
	mm, _ = m.handleBoard(tea.KeyPressMsg{Code: '9', Text: "9"})
	m = mm.(Model)
	if !m.boardOpen {
		t.Error("out-of-range digit should be a no-op (board stays open)")
	}
}

func TestBoardOpensFromPicker(t *testing.T) {
	m := sampleModel() // picker, no current workspace

	mm, _ := m.handleKey(altKey('a')) // palette
	m = mm.(Model)
	if !m.boardOpen {
		t.Fatal("alt+a should open the board from the picker")
	}
	// With no open workspaces the board still offers the "+ new agent" row.
	rows, nav := m.buildBoard()
	if len(nav) != 1 || !rows[nav[0]].isNew {
		t.Fatalf("empty board should hold only the new-agent row, got rows=%d nav=%d", len(rows), len(nav))
	}
	// Inside an open board ctrl+n is list-down: it keeps the board open. The
	// primary palette key closes.
	mm, _ = m.handleKey(ctrlKey('n'))
	m = mm.(Model)
	if !m.boardOpen {
		t.Fatal("ctrl+n inside the board should navigate, not close it")
	}
	mm, _ = m.handleKey(altKey('a'))
	m = mm.(Model)
	if m.boardOpen {
		t.Fatal("alt+a should close the board")
	}
}

func TestQuickPromptOpensForm(t *testing.T) {
	m := sampleModel()

	mm, _ := m.handleKey(altKey('y'))
	m = mm.(Model)
	if !m.boardOpen || !m.formOpen {
		t.Fatalf("alt+y should open the new-agent form: board=%v form=%v", m.boardOpen, m.formOpen)
	}
	if m.formLocation != 1 || !m.formBackground || m.formFocus != formFieldPrompt {
		t.Fatalf("quick prompt should preselect home+background with prompt focused: loc=%d bg=%v focus=%d",
			m.formLocation, m.formBackground, m.formFocus)
	}
}

func TestBoardFormPromptSupportsMultipleLines(t *testing.T) {
	m := modelWithAgents(1).openNewAgentForm().(Model)

	for _, key := range []tea.KeyPressMsg{
		{Code: 'f', Text: "f"},
		{Code: tea.KeyEnter},
		{Code: 's', Text: "s"},
	} {
		mm, _ := m.handleBoardForm(key)
		m = mm.(Model)
	}
	if got, want := m.promptInput.Value(), "f\ns"; got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
	if m.formFocus != formFieldPrompt || !m.formOpen {
		t.Fatalf("enter should edit the prompt without leaving the form: focus=%d open=%v", m.formFocus, m.formOpen)
	}
	if m.promptInput.Height() < 3 {
		t.Fatalf("multi-line prompt should retain its expanded input area, height=%d", m.promptInput.Height())
	}
}

func TestBoardFormFieldCycleAndToggles(t *testing.T) {
	m := modelWithAgents(1)
	m = m.openNewAgentForm().(Model)
	if m.formFocus != formFieldPrompt || m.formLocation != 0 || m.formBackground {
		t.Fatalf("form defaults: focus=%d loc=%d bg=%v", m.formFocus, m.formLocation, m.formBackground)
	}

	// Tab cycles prompt → where → mode → prompt.
	for _, want := range []int{formFieldWhere, formFieldMode, formFieldPrompt} {
		mm, _ := m.handleBoardForm(tea.KeyPressMsg{Code: tea.KeyTab})
		m = mm.(Model)
		if m.formFocus != want {
			t.Fatalf("tab: focus=%d want %d", m.formFocus, want)
		}
	}

	// Space on the where/mode rows flips the toggles.
	mm, _ := m.handleBoardForm(tea.KeyPressMsg{Code: tea.KeyTab}) // prompt → where
	m = mm.(Model)
	mm, _ = m.handleBoardForm(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	m = mm.(Model)
	if m.formLocation != 1 {
		t.Fatalf("space should flip location, got %d", m.formLocation)
	}
	mm, _ = m.handleBoardForm(tea.KeyPressMsg{Code: tea.KeyTab}) // → mode
	m = mm.(Model)
	mm, _ = m.handleBoardForm(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	m = mm.(Model)
	if !m.formBackground {
		t.Fatal("space should flip mode to background")
	}

	// Esc returns to the board list without closing the overlay.
	mm, _ = m.handleBoardForm(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = mm.(Model)
	if m.formOpen || !m.boardOpen {
		t.Fatalf("esc should return to the board list: form=%v board=%v", m.formOpen, m.boardOpen)
	}
}

func mixedProviderModel(defaultProvider agent.Provider) Model {
	cfg := config.Default()
	cfg.Agents.Enabled = []agent.Provider{agent.Claude, agent.Codex}
	cfg.Agents.Default = defaultProvider
	cfg.Agents.Claude.Command = "/usr/bin/true"
	cfg.Agents.Codex.Command = "/usr/bin/true"
	ctrl := NewController(cfg)
	ctrl.startCodex = func(context.Context, codex.Config) (codexRuntime, error) {
		return newFakeCodexRuntime("unix:///tmp/caretaker-fake-codex.sock"), nil
	}
	return New(ctrl, session.NewManager())
}

type fakeCodexRuntime struct {
	remote string
	events chan agent.Event
	once   sync.Once
	closed atomic.Int32
}

func newFakeCodexRuntime(remote string) *fakeCodexRuntime {
	return &fakeCodexRuntime{remote: remote, events: make(chan agent.Event, 8)}
}

func (f *fakeCodexRuntime) Close() error {
	f.once.Do(func() {
		f.closed.Add(1)
		close(f.events)
	})
	return nil
}

func (f *fakeCodexRuntime) Remote() string                  { return f.remote }
func (f *fakeCodexRuntime) EventStream() <-chan agent.Event { return f.events }

func TestPrepareAgentSpecPlacesRemoteAfterBaseArgs(t *testing.T) {
	cfg := config.Default()
	cfg.Agents.Enabled = []agent.Provider{agent.Codex}
	cfg.Agents.Default = agent.Codex
	cfg.Agents.Codex = config.AgentProvider{Command: "codex-test", Args: []string{"--base", "value"}}
	ctrl := NewController(cfg)
	runtime := newFakeCodexRuntime("unix:///tmp/fake-runtime.sock")
	ctrl.startCodex = func(_ context.Context, got codex.Config) (codexRuntime, error) {
		if got.Command != "codex-test" || !equalStrings(got.Args, []string{"--base", "value"}) || got.Dir != "/repo" {
			t.Fatalf("runtime config = %+v", got)
		}
		return runtime, nil
	}

	spec, err := ctrl.NewProviderAgentSpec(agent.Codex, "jade-otter", "inspect", AgentBackground)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := ctrl.PrepareAgentSpec(context.Background(), "/repo", spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"codex-test", "--base", "value", "--remote", "unix:///tmp/fake-runtime.sock",
		"--sandbox", "workspace-write", "--ask-for-approval", "never", "inspect",
	}
	if !equalStrings(prepared.Argv, want) {
		t.Fatalf("prepared argv = %v, want %v", prepared.Argv, want)
	}
	if prepared.Companion != runtime || prepared.Events == nil {
		t.Fatalf("prepared runtime wiring = companion %T events nil=%v", prepared.Companion, prepared.Events == nil)
	}
	_ = prepared.Companion.Close()
	_ = prepared.Companion.Close()
	if got := runtime.closed.Load(); got != 1 {
		t.Fatalf("runtime close count = %d, want 1", got)
	}
}

func TestPrepareWorkspaceSpecsCleansEarlierCompanionOnFailure(t *testing.T) {
	m := mixedProviderModel(agent.Codex)
	var runtimes []*fakeCodexRuntime
	m.ctrl.startCodex = func(context.Context, codex.Config) (codexRuntime, error) {
		runtime := newFakeCodexRuntime("unix:///tmp/partial-runtime.sock")
		runtimes = append(runtimes, runtime)
		return runtime, nil
	}
	valid, err := m.ctrl.NewProviderAgentSpec(agent.Codex, "one", "", AgentForeground)
	if err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.Argv = []string{"wrong-command"}
	_, err = m.prepareWorkspaceSpecs(t.TempDir(), []session.Spec{valid, invalid})
	if err == nil {
		t.Fatal("expected mismatched Codex command to fail preparation")
	}
	if len(runtimes) != 1 || runtimes[0].closed.Load() != 1 {
		t.Fatalf("started runtimes = %d, first close count = %d", len(runtimes), runtimes[0].closed.Load())
	}
}

func TestBoardFormProviderChoiceAndDefaultReset(t *testing.T) {
	m := mixedProviderModel(agent.Codex)
	m.current = &workspaceRef{key: "r/w", path: t.TempDir(), ws: &session.Workspace{}}
	m = m.openNewAgentForm().(Model)
	if m.formProvider != agent.Codex || m.promptInput.Placeholder != "What should Codex do?" {
		t.Fatalf("default provider form state = %q / %q", m.formProvider, m.promptInput.Placeholder)
	}

	// With two providers the row participates in focus order.
	mm, _ := m.handleBoardForm(tea.KeyPressMsg{Code: tea.KeyTab})
	m = mm.(Model)
	if m.formFocus != formFieldProvider {
		t.Fatalf("first tab focus = %d, want provider", m.formFocus)
	}
	mm, _ = m.handleBoardForm(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	m = mm.(Model)
	if m.formProvider != agent.Claude || m.promptInput.Placeholder != "What should Claude do?" {
		t.Fatalf("toggled provider form state = %q / %q", m.formProvider, m.promptInput.Placeholder)
	}

	// A fresh form always returns to the configured default.
	m = m.openNewAgentForm().(Model)
	if m.formProvider != agent.Codex {
		t.Fatalf("reopened form provider = %q, want codex", m.formProvider)
	}
}

func TestWorkspaceSpecsPreserveProviders(t *testing.T) {
	m := mixedProviderModel(agent.Codex)
	m.state = &state.State{
		LastOpened: map[string]int64{},
		Workspaces: map[string]*state.WorkspaceState{
			"r/w": {Agents: []state.AgentState{
				{Provider: agent.Claude, SessionID: "claude-id", Label: "one"},
				{Provider: agent.Codex, SessionID: "codex-id", Label: "two"},
			}},
		},
	}
	specs, err := m.workspaceSpecs("r/w", true)
	if err != nil {
		t.Fatal(err)
	}
	var got []agent.Provider
	for _, spec := range specs {
		if spec.Kind == session.Agent {
			got = append(got, spec.Provider)
		}
	}
	if len(got) != 2 || got[0] != agent.Claude || got[1] != agent.Codex {
		t.Fatalf("restored providers = %v", got)
	}
}

func TestFirstHomeLaunchCreatesOnlySelectedAgent(t *testing.T) {
	m := mixedProviderModel(agent.Codex)
	m.state = &state.State{LastOpened: map[string]int64{}, Workspaces: map[string]*state.WorkspaceState{}}
	m.formLocation = 1
	m.formProvider = agent.Codex
	m.promptInput.SetValue("inspect the project")
	mm, _ := m.launchAgent()
	m = mm.(Model)
	defer m.mgr.CloseAll()
	ws, ok := m.mgr.Workspace("~/config")
	if !ok {
		t.Fatal("home workspace was not activated")
	}
	if len(ws.Agents) != 1 {
		t.Fatalf("home agent count = %d, want exactly the selected agent", len(ws.Agents))
	}
	if ws.Agents[0].Provider != agent.Codex {
		t.Fatalf("home agent provider = %q, want codex", ws.Agents[0].Provider)
	}
	if ws.Agents[0].Events == nil {
		t.Fatal("home Codex launch did not attach the observer event stream")
	}
}

func TestPersistAgentsRecordsProvider(t *testing.T) {
	m := mixedProviderModel(agent.Claude)
	m.state = &state.State{LastOpened: map[string]int64{}, Workspaces: map[string]*state.WorkspaceState{}}
	dir := t.TempDir()
	spec := session.Spec{
		Kind: session.Agent, Title: "jade-otter", Argv: []string{"/usr/bin/true"},
		Provider: agent.Codex, SessionID: "thread-123",
	}
	ws, err := m.mgr.Activate("r/w", dir, []session.Spec{spec}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer m.mgr.CloseAll()
	m.current = &workspaceRef{repo: "r", worktree: "w", key: "r/w", path: dir, ws: ws}
	m.persistAgents("r/w")
	saved, _ := m.state.Agents("r/w")
	if len(saved) != 1 || saved[0].Provider != agent.Codex || saved[0].SessionID != "thread-123" {
		t.Fatalf("persisted agents = %+v", saved)
	}
}

func TestBoardRestartPreservesProviderAndPoolPosition(t *testing.T) {
	m := mixedProviderModel(agent.Claude)
	m.state = &state.State{LastOpened: map[string]int64{}, Workspaces: map[string]*state.WorkspaceState{}}
	dir := t.TempDir()
	specs := []session.Spec{
		{Kind: session.Agent, Title: "one", Argv: []string{"/usr/bin/true"}, Provider: agent.Claude, SessionID: "claude-id"},
		{Kind: session.Agent, Title: "two", Argv: []string{"/usr/bin/true"}, Provider: agent.Codex, SessionID: "codex-id"},
	}
	ws, err := m.mgr.Activate("r/w", dir, specs, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer m.mgr.CloseAll()
	ws.ActiveAgent = 1
	old := ws.Agents[1]
	m.current = &workspaceRef{repo: "r", worktree: "w", key: "r/w", path: dir, ws: ws}
	m.boardOpen = true
	m.boardCursor = 1
	mm, _ := m.handleBoard(tea.KeyPressMsg{Code: 'r', Text: "r"})
	m = mm.(Model)
	if ws.Agents[1] == old {
		t.Fatal("restart did not replace the selected session")
	}
	if ws.ActiveAgent != 1 || len(ws.Agents) != 2 {
		t.Fatalf("restart changed pool shape/focus: len=%d active=%d", len(ws.Agents), ws.ActiveAgent)
	}
	if got := ws.Agents[1]; got.Provider != agent.Codex || got.SessionID != "codex-id" {
		t.Fatalf("replacement metadata = provider %q id %q", got.Provider, got.SessionID)
	}
	if ws.Agents[1].Events == nil {
		t.Fatal("restarted Codex session did not attach a fresh observer event stream")
	}
}

func TestCodexTranscriptWheel(t *testing.T) {
	tests := []struct {
		button tea.MouseButton
		want   string
		ok     bool
	}{
		{tea.MouseWheelUp, "\x1b[1;2A", true},
		{tea.MouseWheelDown, "\x1b[1;2B", true},
		{tea.MouseLeft, "", false},
	}
	for _, tt := range tests {
		got, ok := codexTranscriptWheel(tt.button)
		if got != tt.want || ok != tt.ok {
			t.Errorf("codexTranscriptWheel(%v) = (%q, %t), want (%q, %t)", tt.button, got, ok, tt.want, tt.ok)
		}
	}
}

func TestCodexAgentEventsPersistThreadAndSurviveClaudeSnapshot(t *testing.T) {
	m := mixedProviderModel(agent.Claude)
	m.state = &state.State{LastOpened: map[string]int64{}, Workspaces: map[string]*state.WorkspaceState{}}
	dir := t.TempDir()
	events := make(chan agent.Event)
	ws, err := m.mgr.Activate("r/w", dir, []session.Spec{{
		Kind: session.Agent, Title: "jade-otter", Argv: []string{"sleep", "5"},
		Provider: agent.Codex, Events: events,
	}}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer m.mgr.CloseAll()
	sess := ws.Agents[0]
	pid := sess.Pid()
	m.current = &workspaceRef{repo: "r", worktree: "w", key: "r/w", path: dir, ws: ws}

	apply := func(event agent.Event) {
		t.Helper()
		mm, _ := m.update(agentEventMsg{session: sess, event: event, open: true})
		m = mm.(Model)
	}
	apply(agent.Event{Kind: agent.ThreadStarted, ThreadID: "thread-123"})
	if sess.SessionID != "thread-123" {
		t.Fatalf("live session ID = %q", sess.SessionID)
	}
	saved, _ := m.state.Agents("r/w")
	if len(saved) != 1 || saved[0].Provider != agent.Codex || saved[0].SessionID != "thread-123" {
		t.Fatalf("thread start did not persist Codex state: %+v", saved)
	}

	apply(agent.Event{Kind: agent.TurnStarted, ThreadID: "thread-123", TurnID: "turn-1"})
	if got := m.agentStatus[pid]; got.Provider != agent.Codex || got.Status != "busy" {
		t.Fatalf("turn start status = %+v", got)
	}
	apply(agent.Event{Kind: agent.ThreadStatusChanged, ThreadID: "thread-123", Status: "active", WaitingOnApproval: true})
	if got := m.agentStatus[pid]; got.Status != "waiting" || got.WaitingFor != "permission prompt" {
		t.Fatalf("approval status = %+v", got)
	}
	apply(agent.Event{Kind: agent.ThreadStatusChanged, ThreadID: "thread-123", Status: "active", WaitingOnUserInput: true})
	if got := m.agentStatus[pid]; got.Status != "waiting" || got.WaitingFor != "input needed" {
		t.Fatalf("input status = %+v", got)
	}
	apply(agent.Event{Kind: agent.TurnCompleted, ThreadID: "thread-123", TurnID: "turn-1", Status: "completed"})
	if got := m.agentStatus[pid]; got.Status != "idle" || got.WaitingFor != "" {
		t.Fatalf("turn complete status = %+v", got)
	}

	const claudePID = 424242
	mm, _ := m.update(statusMsg{byPid: map[int]AgentStatus{
		claudePID: {Status: "busy", Cwd: dir, StartedAt: 10},
	}})
	m = mm.(Model)
	if got := m.agentStatus[pid]; got.Provider != agent.Codex || got.Status != "idle" {
		t.Fatalf("Claude snapshot replaced Codex status: %+v", got)
	}
	if got := m.agentStatus[claudePID]; got.Provider != agent.Claude || got.Status != "busy" {
		t.Fatalf("Claude snapshot status = %+v", got)
	}
}

func TestOverlayClicksDoNotReachBar(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Model)
	}{
		{"help", func(m *Model) { m.helpOpen = true }},
		{"board", func(m *Model) { m.boardOpen = true }},
		{"usage", func(m *Model) { m.usageOpen = true }},
		{"palette", func(m *Model) { m.paletteOpen = true }},
		{"diff", func(m *Model) { m.diffOpen = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := modelWithAgents(1)
			m.screen = screenEditor
			tc.set(&m)
			// The agent tab normally occupies this bar zone.
			var x int
			for _, z := range m.barZones() {
				if z.s == screenAgent {
					break
				}
				x += lipgloss.Width(z.glyph) + 3
			}
			x += 2
			mm, _ := m.handleMouseClick(tea.MouseClickMsg{X: x, Y: 0, Button: tea.MouseLeft})
			if got := mm.(Model).screen; got != screenEditor {
				t.Fatalf("overlay click changed screen to %d", got)
			}
		})
	}
}

func TestCommandPaletteToggle(t *testing.T) {
	m := sampleModel()

	// alt+p opens the palette from the deck.
	mm, _ := m.handleKey(altKey('p'))
	m = mm.(Model)
	if !m.paletteOpen {
		t.Fatal("alt+p should open the command palette")
	}
	// alt+p again closes it.
	mm, _ = m.handleKey(altKey('p'))
	m = mm.(Model)
	if m.paletteOpen {
		t.Fatal("alt+p should close the command palette")
	}
	// esc closes it too.
	mm, _ = m.handleKey(altKey('p'))
	m = mm.(Model)
	mm, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = mm.(Model)
	if m.paletteOpen {
		t.Fatal("esc should close the command palette")
	}
}

// TestCommandPaletteClosesOtherOverlays: opening the palette must never stack on
// top of the board or usage overlays.
func TestCommandPaletteClosesOtherOverlays(t *testing.T) {
	m := sampleModel()
	m.boardOpen = true
	m.usageOpen = true
	mm, _ := m.openPalette()
	m = mm.(Model)
	if !m.paletteOpen || m.boardOpen || m.usageOpen {
		t.Fatalf("opening the palette should close other overlays: palette=%v board=%v usage=%v",
			m.paletteOpen, m.boardOpen, m.usageOpen)
	}
}

// TestCommandPaletteBlockedDuringConfirm: a picker modal confirm owns its keys,
// so alt+p must not open over it.
func TestCommandPaletteBlockedDuringConfirm(t *testing.T) {
	m := sampleModel() // screen is the picker
	m.mode = modeConfirmRemove
	mm, _ := m.handleKey(altKey('p'))
	m = mm.(Model)
	if m.paletteOpen {
		t.Fatal("alt+p must not open over a picker modal confirm")
	}
}

func TestCommandPaletteFilter(t *testing.T) {
	m := sampleModel()
	mm, _ := m.openPalette()
	m = mm.(Model)

	all := len(m.filteredPaletteCommands())
	m.paletteInput.SetValue("help")
	got := m.filteredPaletteCommands()
	if len(got) == 0 || len(got) >= all {
		t.Fatalf("query should narrow the list: all=%d got=%d", all, len(got))
	}
	found := false
	for _, c := range got {
		if c.title == "help" {
			found = true
		}
	}
	if !found {
		t.Error("query 'help' should still match the help row")
	}
}

// TestCommandPaletteCursorResetsOnQueryChange: typing that changes the query
// snaps the cursor back to the top of the (re-filtered) list.
func TestCommandPaletteCursorResetsOnQueryChange(t *testing.T) {
	m := sampleModel()
	mm, _ := m.openPalette()
	m = mm.(Model)

	mm, _ = m.handlePalette(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mm.(Model)
	mm, _ = m.handlePalette(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mm.(Model)
	if m.paletteCursor == 0 {
		t.Fatal("down should have moved the cursor off the top")
	}

	mm, _ = m.handlePalette(tea.KeyPressMsg{Code: 'h', Text: "h"})
	m = mm.(Model)
	if m.paletteCursor != 0 {
		t.Fatalf("cursor should reset to 0 on query change, got %d", m.paletteCursor)
	}
}

// TestCommandPaletteCursorClamped: the cursor never runs off either end of the
// filtered list.
func TestCommandPaletteCursorClamped(t *testing.T) {
	m := sampleModel()
	mm, _ := m.openPalette()
	m = mm.(Model)
	n := len(m.filteredPaletteCommands())

	// Up at the top stays at 0.
	mm, _ = m.handlePalette(tea.KeyPressMsg{Code: tea.KeyUp})
	m = mm.(Model)
	if m.paletteCursor != 0 {
		t.Fatalf("up at the top should stay at 0, got %d", m.paletteCursor)
	}
	// Down well past the end clamps at n-1.
	for i := 0; i < n+5; i++ {
		mm, _ = m.handlePalette(tea.KeyPressMsg{Code: tea.KeyDown})
		m = mm.(Model)
	}
	if m.paletteCursor != n-1 {
		t.Fatalf("down should clamp at %d, got %d", n-1, m.paletteCursor)
	}
}

// TestCommandPaletteAvailability: view-navigation rows only appear with an
// active workspace.
func TestCommandPaletteAvailability(t *testing.T) {
	m := sampleModel() // no current workspace
	for _, c := range m.paletteCommands() {
		if c.title == "go to editor" {
			t.Fatal("no workspace should not offer 'go to editor'")
		}
	}

	m2 := modelWithAgents(1)
	found := false
	for _, c := range m2.paletteCommands() {
		if c.title == "go to editor" {
			found = true
		}
	}
	if !found {
		t.Fatal("an active workspace should offer 'go to editor'")
	}
}

// TestCommandPaletteRunGotoEditor: enter on the "go to editor" row closes the
// palette and switches the screen.
func TestCommandPaletteRunGotoEditor(t *testing.T) {
	m := modelWithAgents(1)
	m.screen = screenTerminal
	mm, _ := m.openPalette()
	m = mm.(Model)

	m.paletteInput.SetValue("editor") // only "go to editor" matches
	m.paletteCursor = 0
	mm, _ = m.handlePalette(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if m.paletteOpen {
		t.Error("enter should close the palette")
	}
	if m.screen != screenEditor {
		t.Fatalf("enter on 'go to editor' should switch to editor, got %v", m.screen)
	}
}

// TestCommandPaletteRunOpenWorktree: enter on an "open <repo>/<wt>" row runs
// activate against a real (cheap-child) manager.
func TestCommandPaletteRunOpenWorktree(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	keys := config.Default().Keys
	keys.Cycle = "ctrl+o"
	ctrl := &Controller{cfg: config.Config{
		Editor: "cat", Agent: "cat", Shell: "sh",
		Keys: keys,
	}}
	mgr := session.NewManager()
	defer mgr.CloseAll()

	m := New(ctrl, mgr)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(Model)
	dir := t.TempDir()
	m.groups = []Group{{Repo: repo.Repo{Name: "repo"}, Worktrees: []WorktreeView{
		{WT: repo.Worktree{Repo: "repo", Name: "wt", Path: dir}},
	}}}
	m.recomputeActive()

	mm, _ = m.openPalette()
	m = mm.(Model)
	m.paletteInput.SetValue("open repo/wt")
	m.paletteCursor = 0
	mm, _ = m.handlePalette(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if m.current == nil || m.current.key != "repo/wt" {
		t.Fatalf("enter on the open row should activate repo/wt, got %+v", m.current)
	}
	if m.paletteOpen {
		t.Error("enter should close the palette")
	}
}

// TestCommandPaletteSwallowsSessionKeys: while the palette is open no session is
// visible and a stray letter key lands in the query, never in the session.
func TestCommandPaletteSwallowsSessionKeys(t *testing.T) {
	m := modelWithAgents(1)
	m.screen = screenEditor
	mm, _ := m.openPalette()
	m = mm.(Model)

	if m.visibleSessions() != nil {
		t.Error("no session should be visible while the palette is open")
	}
	mm, _ = m.handleKey(tea.KeyPressMsg{Code: 'z', Text: "z"})
	m = mm.(Model)
	if !m.paletteOpen {
		t.Error("a letter key should not close the palette")
	}
	if m.paletteInput.Value() != "z" {
		t.Fatalf("letter should type into the query, got %q", m.paletteInput.Value())
	}
}

func TestStatusAutoExpires(t *testing.T) {
	m := sampleModel()

	m.flash("stopped feat-login")
	m.maybeExpireStatus() // fresh — should stay
	if m.status == "" {
		t.Fatal("a fresh transient status should not expire")
	}

	// Backdate it past the TTL: the next poll clears it.
	m.statusAt = m.statusAt.Add(-2 * transientStatusTTL)
	m.maybeExpireStatus()
	if m.status != "" {
		t.Fatalf("stale transient status should clear, got %q", m.status)
	}

	// Errors are sticky regardless of age.
	m.setError("open error: boom")
	m.statusAt = time.Now().Add(-2 * transientStatusTTL)
	m.maybeExpireStatus()
	if m.status == "" {
		t.Error("error status should not auto-expire")
	}

	// A transient status that merely mentions "error" in a name still expires
	// — expiry keys off the typed level, not the text.
	m.flash("stopped error-handling")
	m.statusAt = m.statusAt.Add(-2 * transientStatusTTL)
	m.maybeExpireStatus()
	if m.status != "" {
		t.Fatalf("info status naming 'error' should still expire, got %q", m.status)
	}
}

func TestBoardEnterNewRowOpensForm(t *testing.T) {
	m := modelWithAgents(2)
	mm, _ := m.openBoard()
	m = mm.(Model)
	m.boardCursor = 2 // the "+ new agent" row

	mm, _ = m.handleBoard(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if !m.formOpen {
		t.Error("entering on the new-agent row should open the new-agent form")
	}
}

func TestAttentionTransitionDetection(t *testing.T) {
	m := sampleModel()
	m.active = []activeItem{
		{repo: repo.Repo{Name: "r"}, view: WorktreeView{WT: repo.Worktree{Name: "a", Path: "/r/a"}}},
		{repo: repo.Repo{Name: "r"}, view: WorktreeView{WT: repo.Worktree{Name: "b", Path: "/r/b"}}},
	}
	m.agentPrevStatus = map[int]string{1: "busy", 2: "busy"}

	// pid 1 finishes (busy → idle), pid 2 gets blocked (busy → waiting)
	result, _ := m.update(statusMsg{byPid: map[int]AgentStatus{
		1: {Status: "idle", Cwd: "/r/a"},
		2: {Status: "waiting", Cwd: "/r/b"},
	}})
	m2 := result.(Model)

	if e := m2.attention[1]; e.level != attnDone || e.key != wsKey("r", "a") {
		t.Errorf("pid 1: want stored attnDone for r/a after busy→idle, got %+v", e)
	}
	// Waiting is derived live from the polled status, not stored.
	if _, ok := m2.attention[2]; ok {
		t.Error("pid 2: waiting must not be stored as an unread marker")
	}
	if got := m2.agentAttn(2); got != attnWaiting {
		t.Errorf("pid 2: agentAttn should derive waiting, got %v", got)
	}
	if got := m2.worktreeAttn(wsKey("r", "b")); got != attnWaiting {
		t.Errorf("r/b: worktreeAttn should derive waiting, got %v", got)
	}

	// A pid that was idle (not busy) going idle again should not fire.
	m3 := sampleModel()
	m3.active = m.active
	m3.agentPrevStatus = map[int]string{1: "idle"}
	result3, _ := m3.update(statusMsg{byPid: map[int]AgentStatus{
		1: {Status: "idle", Cwd: "/r/a"},
	}})
	m4 := result3.(Model)
	if len(m4.attention) != 0 {
		t.Errorf("idle→idle should not produce a marker, got %v", m4.attention)
	}
}

func TestAttentionPrecedence(t *testing.T) {
	m := sampleModel()
	m.active = []activeItem{
		{repo: repo.Repo{Name: "r"}, view: WorktreeView{WT: repo.Worktree{Name: "a", Path: "/r/a"}}},
	}
	// A live-waiting agent outranks another agent's unread completion for the
	// worktree badge.
	m.agentStatus = map[int]AgentStatus{1: {Status: "waiting", Cwd: "/r/a"}}
	m.attention[2] = attnEntry{level: attnDone, key: wsKey("r", "a")}
	if got := m.worktreeAttn(wsKey("r", "a")); got != attnWaiting {
		t.Errorf("waiting should outrank done, got %v", got)
	}

	// recordAttention never downgrades a stored marker.
	m.attention[3] = attnEntry{level: attnWaiting, key: "~/config"}
	m.recordAttention(3, attnDone, "~/config", 0)
	if e := m.attention[3]; e.level != attnWaiting {
		t.Errorf("recordAttention must not downgrade waiting → done, got %+v", e)
	}
}

func TestAttentionClearedOnAgentView(t *testing.T) {
	m := sampleModel()
	m.attention = map[int]attnEntry{
		1: {level: attnDone, key: "r/a"},
		2: {level: attnDone, key: "r/b"},
	}
	ws := &session.Workspace{Agents: []*session.Session{{}, {}}}
	m.current = &workspaceRef{key: "r/a", path: "/r/a", ws: ws}
	m.screen = screenEditor // start on editor, cycle to agent

	// Cycling to the agent screen (keyCycle) clears markers for the current workspace.
	result, _ := m.update(altKey(']'))
	m2 := result.(Model)

	if m2.screen != screenAgent {
		t.Fatalf("expected screenAgent after cycle, got %v", m2.screen)
	}
	if _, ok := m2.attention[1]; ok {
		t.Error("r/a marker should clear when cycling to the agent screen")
	}
	if e := m2.attention[2]; e.level != attnDone {
		t.Errorf("r/b marker should be unaffected, got %+v", e)
	}

	// selectTab(screenAgent) also clears.
	m3 := sampleModel()
	m3.attention = map[int]attnEntry{
		1: {level: attnDone, key: "r/a"},
		2: {level: attnDone, key: "r/b"},
	}
	m3.current = &workspaceRef{key: "r/a", path: "/r/a", ws: ws}
	mm3, _ := m3.selectTab(screenAgent)
	m4 := mm3.(Model)
	if _, ok := m4.attention[1]; ok {
		t.Error("r/a marker should clear on selectTab(agent)")
	}
	if e := m4.attention[2]; e.level != attnDone {
		t.Errorf("r/b marker should be unaffected after selectTab, got %+v", e)
	}

	// Being on screenAgent during an unrelated message does NOT clear.
	m5 := sampleModel()
	m5.attention = map[int]attnEntry{1: {level: attnDone, key: "r/a"}}
	m5.current = &workspaceRef{key: "r/a", path: "/r/a", ws: ws}
	m5.screen = screenAgent
	result5, _ := m5.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m6 := result5.(Model)
	if e := m6.attention[1]; e.level != attnDone {
		t.Errorf("unrelated message on agent screen should NOT clear markers, got %+v", e)
	}
}

// waitForSession polls a session's rendered screen until it contains want.
func waitForSession(t *testing.T, s *session.Session, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(s.Render(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q; screen was:\n%s", want, s.Render())
}

// pasteModel activates one real workspace (cheap cat/sh children) on the
// editor screen and returns the model plus its active session. cat echoes its
// stdin, so anything routed to the session shows up on the session's screen.
func pasteModel(t *testing.T) (Model, *session.Session) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	keys := config.Default().Keys
	keys.Cycle = "ctrl+o"
	ctrl := &Controller{cfg: config.Config{
		Editor: "cat", Agent: "cat", Shell: "sh",
		Keys: keys,
	}}
	mgr := session.NewManager()
	t.Cleanup(mgr.CloseAll)

	m := New(ctrl, mgr)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(Model)
	mm, _ = m.activate("repo", "wt", t.TempDir())
	m = mm.(Model)
	return m, m.activeSession()
}

// TestPasteReachesSession: a paste on a bare session screen is delivered to
// the active program (and, like typed input, retires the help hint).
func TestPasteReachesSession(t *testing.T) {
	m, sess := pasteModel(t)
	if sess == nil {
		t.Fatal("expected an active session")
	}
	mm, _ := m.Update(tea.PasteMsg{Content: "pasted-into-session"})
	m = mm.(Model)
	if !m.hintSeen {
		t.Error("pasting into a session should dismiss the help hint like typed input")
	}
	waitForSession(t, sess, "pasted-into-session")
}

// TestPasteReachesPickerFilter: a paste on the picker still lands in the
// focused filter input (the pre-existing textinput behavior is preserved).
func TestPasteReachesPickerFilter(t *testing.T) {
	m := sampleModel() // picker, NEW section, filter focused
	if !m.filter.Focused() {
		t.Fatal("precondition: the picker filter should be focused")
	}
	mm, _ := m.Update(tea.PasteMsg{Content: "api"})
	m = mm.(Model)
	if got := m.filter.Value(); got != "api" {
		t.Fatalf("paste should reach the filter input, got %q", got)
	}
}

// TestPasteBlockedByOverlay: a paste while an overlay is open must not leak
// into the session drawn beneath it.
func TestPasteBlockedByOverlay(t *testing.T) {
	m, sess := pasteModel(t)
	if sess == nil {
		t.Fatal("expected an active session")
	}
	m.helpOpen = true
	mm, _ := m.Update(tea.PasteMsg{Content: "leaked-through-overlay"})
	m = mm.(Model)

	// Close the overlay and paste a value that IS allowed through. Messages are
	// processed in order, so once the allowed paste is on screen the blocked one
	// would already be visible too — its absence proves it was swallowed.
	m.helpOpen = false
	mm, _ = m.Update(tea.PasteMsg{Content: "allowed-after-close"})
	m = mm.(Model)
	waitForSession(t, sess, "allowed-after-close")
	if strings.Contains(sess.Render(), "leaked-through-overlay") {
		t.Error("paste while an overlay was open leaked into the session")
	}
}

// TestPasteIntoPaletteOnly: a paste while the command palette is open lands in
// the palette's query input and nowhere else — in particular not in the deck
// filter, which stays focused across screens and would otherwise receive a
// duplicate through the focus-routed path.
func TestPasteIntoPaletteOnly(t *testing.T) {
	m := sampleModel() // picker, NEW section, filter focused
	mm, _ := m.openPalette()
	m = mm.(Model)
	m.paletteCursor = 2
	mm, _ = m.Update(tea.PasteMsg{Content: "usage"})
	m = mm.(Model)
	if got := m.paletteInput.Value(); got != "usage" {
		t.Fatalf("paste should reach the palette input, got %q", got)
	}
	if got := m.filter.Value(); got != "" {
		t.Errorf("paste leaked into the deck filter: %q", got)
	}
	if m.paletteCursor != 0 {
		t.Errorf("paste changed the query, so the cursor should reset to 0, got %d", m.paletteCursor)
	}
}

func TestToUVKey(t *testing.T) {
	uvk := toUVKey(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if uvk.Code != 'a' || uvk.Text != "a" {
		t.Fatalf("toUVKey mapped wrong: %+v", uvk)
	}

	// Kitty-protocol terminals report shift+a as Code='a', ShiftedCode='A',
	// Mod=ModShift. The vt emulator's SendKey only writes Code when Mod==0, so
	// we must collapse the pair into Code='A', Mod=0 before forwarding.
	uvk = toUVKey(tea.KeyPressMsg{Code: 'a', ShiftedCode: 'A', Mod: tea.ModShift, Text: "A"})
	if uvk.Code != 'A' || uvk.Mod != 0 || uvk.Text != "A" {
		t.Fatalf("toUVKey should collapse shift+letter to uppercase: %+v", uvk)
	}
}

// TestActivateFlow drives activate → cycle → return → re-activate with cheap
// child commands (no real nvim/claude needed) and a real session manager.
func TestActivateFlow(t *testing.T) {
	keys := config.Default().Keys
	keys.Cycle = "ctrl+o"
	ctrl := &Controller{cfg: config.Config{
		Editor: "cat", Agent: "cat", Shell: "sh",
		Keys: keys,
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
	if m.current == nil || m.current.ws == nil {
		t.Fatalf("expected an active workspace, got %+v", m.current)
	}
	if m.current.ws.Editor == nil || len(m.current.ws.Terms) == 0 || len(m.current.ws.Agents) != 1 {
		t.Fatalf("expected editor+term+1 agent, got %+v", m.current.ws)
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
	before := m.current.ws.Editor
	mm, _ = m.activate("repo", "wt", t.TempDir())
	m = mm.(Model)
	if m.current.ws.Editor != before {
		t.Fatal("re-activate should reuse existing sessions")
	}
}

// TestMouseClickFocusesTermPane splits a terminal into two panes and verifies a
// left click over the non-focused pane moves pane focus onto it.
func TestMouseClickFocusesTermPane(t *testing.T) {
	keys := config.Default().Keys
	keys.Cycle = "ctrl+o"
	ctrl := &Controller{cfg: config.Config{
		Editor: "cat", Agent: "cat", Shell: "sh",
		Keys: keys,
	}}
	mgr := session.NewManager()
	defer mgr.CloseAll()

	m := New(ctrl, mgr)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(Model)

	mm, _ = m.activate("repo", "wt", t.TempDir())
	m = mm.(Model)
	m.screen = screenTerminal

	// Split vertically → left 0 | right 1, with the new pane 1 focused.
	w, h := m.sessionSize()
	if _, err := mgr.SplitTermPane("repo/wt", t.TempDir(), ctrl.TermSpec(), session.SplitV, w, h); err != nil {
		t.Fatal(err)
	}
	ws := m.current.ws
	if ws.ActiveTerm != 1 {
		t.Fatalf("after split active=%d, want 1", ws.ActiveTerm)
	}

	// A left click well left of the divider, below the bar, focuses pane 0.
	m.handleMouseClick(tea.MouseClickMsg{X: 5, Y: barHeight + 3, Button: tea.MouseLeft})
	if ws.ActiveTerm != 0 {
		t.Fatalf("click in left pane should focus 0, got %d", ws.ActiveTerm)
	}
}

// TestBoardSortsAttentionFirst activates two real workspaces and checks that
// the one with an unread marker sorts above the current one, and that the
// board cursor opens on it.
func TestBoardSortsAttentionFirst(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	keys := config.Default().Keys
	keys.Cycle = "ctrl+o"
	ctrl := &Controller{cfg: config.Config{
		Editor: "cat", Agent: "cat", Shell: "sh",
		Keys: keys,
	}}
	mgr := session.NewManager()
	defer mgr.CloseAll()

	m := New(ctrl, mgr)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(Model)

	dirA, dirB := t.TempDir(), t.TempDir()
	mm, _ = m.activate("repo", "a", dirA)
	m = mm.(Model)
	mm, _ = m.activate("repo", "b", dirB)
	m = mm.(Model) // current is now repo/b
	m.active = []activeItem{
		{repo: repo.Repo{Name: "repo"}, view: WorktreeView{WT: repo.Worktree{Name: "a", Path: dirA}}},
		{repo: repo.Repo{Name: "repo"}, view: WorktreeView{WT: repo.Worktree{Name: "b", Path: dirB}}},
	}

	wsA, _ := mgr.Workspace("repo/a")
	pid := wsA.Agents[0].Pid()
	m.attention[pid] = attnEntry{level: attnDone, key: "repo/a"}

	rows, nav := m.buildBoard()
	if len(rows) == 0 || rows[0].isAgent || rows[0].key != "repo/a" {
		t.Fatalf("worktree with attention should sort first, got %+v", rows[0])
	}
	// nav: repo/a's agent, repo/b's agent, new row.
	if len(nav) != 3 {
		t.Fatalf("expected 3 navigable rows, got %d", len(nav))
	}
	if got := rows[nav[0]]; got.key != "repo/a" || got.attn != attnDone {
		t.Fatalf("first navigable row should be repo/a's unread agent, got %+v", got)
	}

	// openBoard lands the cursor on the attention row.
	mm2, _ := m.openBoard()
	m2 := mm2.(Model)
	if m2.boardCursor != 0 {
		t.Fatalf("cursor should start on the attention row, got %d", m2.boardCursor)
	}

	// Focusing it clears repo/a's marker and switches workspaces.
	mm2, _ = m2.handleBoard(tea.KeyPressMsg{Code: tea.KeyEnter})
	m2 = mm2.(Model)
	if m2.current == nil || m2.current.key != "repo/a" || m2.screen != screenAgent {
		t.Fatalf("enter should switch to repo/a's agent screen, got %+v screen=%v", m2.current, m2.screen)
	}
	if _, ok := m2.attention[pid]; ok {
		t.Error("focusing the agent should clear its unread marker")
	}
}

// TestAttentionSweepOnPidReuse verifies that when a polled pid reports a
// StartedAt different from a stored attention entry's known startedAt, the
// pid has been recycled to a new process and the stale marker is dropped.
// An entry whose StartedAt matches the poll is left alone.
func TestAttentionSweepOnPidReuse(t *testing.T) {
	m := sampleModel()
	m.attention[42] = attnEntry{level: attnDone, key: "r/w", startedAt: 111}
	m.attention[43] = attnEntry{level: attnDone, key: "r/w", startedAt: 111}

	result, _ := m.update(statusMsg{byPid: map[int]AgentStatus{
		42: {Status: "idle", StartedAt: 222}, // recycled pid: different, nonzero StartedAt
		43: {Status: "idle", StartedAt: 111}, // same process: StartedAt matches
	}})
	m2 := result.(Model)

	if _, ok := m2.attention[42]; ok {
		t.Error("stale marker for a recycled pid should be swept away")
	}
	if e, ok := m2.attention[43]; !ok || e.level != attnDone {
		t.Errorf("marker for a pid whose StartedAt matches should survive, got %+v ok=%v", e, ok)
	}
}

// TestSessionHelpHint covers the one-line help hint shown beneath a session
// until the user's first keystroke: it reserves a row, names the help and
// pane keys, and is retired (row reclaimed) once a key reaches the session.
func TestSessionHelpHint(t *testing.T) {
	m := modelWithAgents(1)
	m.screen = screenTerminal

	// Fresh session: the hint reserves exactly one row of the viewport.
	if got := m.sessionFooterH(); got != 1 {
		t.Fatalf("fresh session should reserve one hint row, got %d", got)
	}
	if _, h := m.sessionSize(); h != m.height-barHeight-1 {
		t.Errorf("session height should drop the hint row, got %d want %d", h, m.height-barHeight-1)
	}

	// On the terminal screen it leads with the help key and surfaces panelling.
	foot := m.sessionFooter()
	for _, want := range []string{m.keys.Help, "help", "split", "zoom"} {
		if !strings.Contains(foot, want) {
			t.Errorf("terminal footer missing %q:\n%s", want, foot)
		}
	}

	// appendSessionFooter adds exactly the reserved row while the hint is live.
	body := "a\nb"
	if got, want := strings.Count(m.appendSessionFooter(body), "\n"), strings.Count(body, "\n")+1; got != want {
		t.Errorf("appended footer newline count = %d, want %d", got, want)
	}

	// First keystroke into the session retires the hint and reclaims the row.
	m2 := m.dismissHint()
	if got := m2.sessionFooterH(); got != 0 {
		t.Errorf("dismissed hint should reserve no rows, got %d", got)
	}
	if _, h := m2.sessionSize(); h != m2.height-barHeight {
		t.Errorf("session should reclaim the row after dismiss, got %d want %d", h, m2.height-barHeight)
	}
	if got := m2.appendSessionFooter(body); got != body {
		t.Errorf("dismissed hint should append nothing, got %q", got)
	}
}

// isQuit reports whether cmd is tea.Quit (i.e. running it yields a QuitMsg).
// Safe to call only on commands that don't have side effects.
func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// busyGuardModel activates one real workspace ("repo/a") the manager hosts and
// exposes it via m.active on the picker, so the quit/stop guards can map a busy
// agent's Cwd to a live workspace. Returns the model and the workspace path.
func busyGuardModel(t *testing.T) (Model, string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	keys := config.Default().Keys
	keys.Cycle = "ctrl+o"
	ctrl := &Controller{cfg: config.Config{
		Editor: "cat", Agent: "cat", Shell: "sh",
		Keys: keys,
	}}
	mgr := session.NewManager()
	t.Cleanup(mgr.CloseAll)
	m := New(ctrl, mgr)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(Model)

	dir := t.TempDir()
	mm, _ = m.activate("repo", "a", dir)
	m = mm.(Model)
	m.active = []activeItem{
		{repo: repo.Repo{Name: "repo"}, view: WorktreeView{WT: repo.Worktree{Name: "a", Path: dir}}},
	}
	m.screen = screenPicker
	m.focus = focusActive
	return m, dir
}

func TestQuitGuardNoBusyQuitsImmediately(t *testing.T) {
	m, dir := busyGuardModel(t)
	m.agentStatus = map[int]AgentStatus{1: {Status: "idle", Cwd: dir}}

	mm, cmd := m.handlePicker(ctrlKey('c'))
	m = mm.(Model)
	if m.mode != modeNormal {
		t.Fatalf("no busy agent should not open a confirm prompt, got mode %v", m.mode)
	}
	if !isQuit(cmd) {
		t.Fatal("ctrl+c with no busy agent should quit immediately")
	}
}

func TestQuitGuardBusyConfirms(t *testing.T) {
	m, dir := busyGuardModel(t)
	m.agentStatus = map[int]AgentStatus{1: {Status: "busy", Cwd: dir}}

	mm, cmd := m.handlePicker(ctrlKey('c'))
	m = mm.(Model)
	if m.mode != modeConfirmQuit {
		t.Fatalf("busy agent should enter modeConfirmQuit, got %v", m.mode)
	}
	if isQuit(cmd) {
		t.Fatal("ctrl+c with a busy agent must not quit yet")
	}
	if !strings.Contains(m.status, "quit anyway") {
		t.Fatalf("expected a quit-confirm prompt, got %q", m.status)
	}

	// 'y' quits.
	_, cmd = m.handlePicker(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if !isQuit(cmd) {
		t.Fatal("'y' should quit from the confirm prompt")
	}

	// Any other key cancels back to normal without quitting.
	mm, cmd = m.handlePicker(tea.KeyPressMsg{Code: 'n', Text: "n"})
	m = mm.(Model)
	if m.mode != modeNormal {
		t.Fatalf("'n' should cancel the quit, got mode %v", m.mode)
	}
	if isQuit(cmd) {
		t.Fatal("'n' must not quit")
	}
}

func TestStopGuardNoBusyStopsImmediately(t *testing.T) {
	m, dir := busyGuardModel(t)
	m.agentStatus = map[int]AgentStatus{1: {Status: "idle", Cwd: dir}}

	mm, _ := m.handleActiveKey(tea.KeyPressMsg{Code: 'd', Text: "d"})
	m = mm.(Model)
	if m.mode != modeNormal {
		t.Fatalf("no busy agent should stop without a prompt, got mode %v", m.mode)
	}
	if m.mgr.Has("repo/a") {
		t.Fatal("'d' with no busy agent should stop the workspace immediately")
	}
}

func TestStopGuardBusyConfirms(t *testing.T) {
	m, dir := busyGuardModel(t)
	m.agentStatus = map[int]AgentStatus{1: {Status: "busy", Cwd: dir}}

	mm, _ := m.handleActiveKey(tea.KeyPressMsg{Code: 'd', Text: "d"})
	m = mm.(Model)
	if m.mode != modeConfirmStop {
		t.Fatalf("busy agent should enter modeConfirmStop, got %v", m.mode)
	}
	if !m.mgr.Has("repo/a") {
		t.Fatal("workspace must not be stopped before confirmation")
	}
	if !strings.Contains(m.status, "stop anyway") {
		t.Fatalf("expected a stop-confirm prompt, got %q", m.status)
	}

	// A non-'y' key cancels, leaving the workspace running.
	mm, _ = m.handlePicker(tea.KeyPressMsg{Code: 'n', Text: "n"})
	cancel := mm.(Model)
	if cancel.mode != modeNormal {
		t.Fatalf("'n' should cancel the stop, got mode %v", cancel.mode)
	}
	if !cancel.mgr.Has("repo/a") {
		t.Fatal("'n' must leave the workspace running")
	}

	// 'y' stops the workspace.
	mm, _ = m.handlePicker(tea.KeyPressMsg{Code: 'y', Text: "y"})
	confirmed := mm.(Model)
	if confirmed.mode != modeNormal {
		t.Fatalf("'y' should return to normal mode, got %v", confirmed.mode)
	}
	if confirmed.mgr.Has("repo/a") {
		t.Fatal("'y' should stop the workspace")
	}
}

// removePromptModel builds a picker model whose active list holds one dirty and
// one clean worktree, so the remove prompt's dirty-awareness can be exercised
// without touching git.
func removePromptModel() Model {
	m := sampleModel()
	m.screen = screenPicker
	m.focus = focusActive
	m.active = []activeItem{
		{repo: repo.Repo{Name: "r"}, view: WorktreeView{WT: repo.Worktree{Repo: "r", Name: "dirtywt", Branch: "dirtywt"}, Dirty: true}},
		{repo: repo.Repo{Name: "r"}, view: WorktreeView{WT: repo.Worktree{Repo: "r", Name: "cleanwt", Branch: "cleanwt"}}},
	}
	return m
}

func TestRemovePromptDirtyAware(t *testing.T) {
	// Dirty worktree: the prompt must warn that uncommitted work is lost.
	m := removePromptModel()
	m.activeCursor = 0
	mm, _ := m.handleActiveKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m = mm.(Model)
	if m.mode != modeConfirmRemove {
		t.Fatalf("'x' should enter modeConfirmRemove, got %v", m.mode)
	}
	if !strings.Contains(m.status, "UNCOMMITTED") {
		t.Fatalf("dirty worktree prompt must mention uncommitted changes, got %q", m.status)
	}
	if !strings.Contains(m.status, "keep branch") {
		t.Fatalf("prompt should offer the keep-branch choice, got %q", m.status)
	}

	// Clean worktree: same choices, but no data-loss warning.
	m = removePromptModel()
	m.activeCursor = 1
	mm, _ = m.handleActiveKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m = mm.(Model)
	if m.mode != modeConfirmRemove {
		t.Fatalf("'x' should enter modeConfirmRemove, got %v", m.mode)
	}
	if strings.Contains(m.status, "UNCOMMITTED") {
		t.Fatalf("clean worktree prompt must not warn of uncommitted changes, got %q", m.status)
	}
	if !strings.Contains(m.status, "keep branch") {
		t.Fatalf("prompt should offer the keep-branch choice, got %q", m.status)
	}
}

func TestRemovePromptCancels(t *testing.T) {
	m := removePromptModel()
	m.activeCursor = 0
	mm, _ := m.handleActiveKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m = mm.(Model)
	// Any key that isn't y/b cancels without removing anything.
	mm, cmd := m.handleConfirmKey(tea.KeyPressMsg{Code: 'n', Text: "n"})
	m = mm.(Model)
	if m.mode != modeNormal {
		t.Fatalf("'n' should cancel back to normal mode, got %v", m.mode)
	}
	if m.status != "" {
		t.Fatalf("cancel should clear the prompt, got %q", m.status)
	}
	if cmd != nil {
		t.Fatal("cancel must not schedule a removal command")
	}
}

// removeGitModel sets up a real repo with one worktree on branch "feat" and a
// picker model whose selected active item points at it, so the y/b removal
// paths can be verified against actual git state. Returns the model, repo dir,
// and branch name.
func removeGitModel(t *testing.T) (Model, string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	repoDir := filepath.Join(root, "demo")
	if err := os.Mkdir(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.email", "t@t.t")
	runGit("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repoDir, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "init")

	r := repo.Repo{Name: "demo", Path: repoDir}
	wt, err := repo.CreateWorktree(r, ".worktrees/feat", "feat", "")
	if err != nil {
		t.Fatal(err)
	}

	m := sampleModel()
	m.screen = screenPicker
	m.focus = focusActive
	m.active = []activeItem{{repo: r, view: WorktreeView{WT: wt}}}
	m.activeCursor = 0
	return m, repoDir, "feat"
}

// branchExists reports whether branch is present in repoDir.
func branchExists(t *testing.T, repoDir, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "branch", "--list", branch)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --list: %v\n%s", err, out)
	}
	return strings.Contains(string(out), branch)
}

// actionDoneFromBatch runs cmd — which may be a single command or a tea.Batch —
// and returns the actionDoneMsg it produces. It stops at the first actionDoneMsg
// so it never executes a batched flash-expiry tick (which would sleep).
func actionDoneFromBatch(t *testing.T, cmd tea.Cmd) actionDoneMsg {
	t.Helper()
	switch msg := cmd().(type) {
	case actionDoneMsg:
		return msg
	case tea.BatchMsg:
		for _, c := range msg {
			if c == nil {
				continue
			}
			if done, ok := c().(actionDoneMsg); ok {
				return done
			}
		}
	}
	t.Fatal("command did not yield an actionDoneMsg")
	return actionDoneMsg{}
}

func TestRemoveConfirmYDeletesBranch(t *testing.T) {
	m, repoDir, branch := removeGitModel(t)

	mm, _ := m.handleActiveKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m = mm.(Model)
	_, cmd := m.handleConfirmKey(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if cmd == nil {
		t.Fatal("'y' should schedule a removal command")
	}
	// The removal is batched with the "removing…" flash-expiry tick; unwrap it.
	msg := actionDoneFromBatch(t, cmd)
	if msg.err != nil {
		t.Fatalf("remove failed: %v", msg.err)
	}
	if branchExists(t, repoDir, branch) {
		t.Fatalf("'y' should delete branch %q", branch)
	}
}

func TestRemoveConfirmBKeepsBranch(t *testing.T) {
	m, repoDir, branch := removeGitModel(t)

	mm, _ := m.handleActiveKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m = mm.(Model)
	_, cmd := m.handleConfirmKey(tea.KeyPressMsg{Code: 'b', Text: "b"})
	if cmd == nil {
		t.Fatal("'b' should schedule a removal command")
	}
	msg := actionDoneFromBatch(t, cmd)
	if msg.err != nil {
		t.Fatalf("remove failed: %v", msg.err)
	}
	if !branchExists(t, repoDir, branch) {
		t.Fatalf("'b' should keep branch %q", branch)
	}
	// The working tree itself must still be gone.
	if wts, _ := repo.ListWorktrees(repo.Repo{Name: "demo", Path: repoDir}); len(wts) != 1 {
		t.Fatalf("expected only main worktree after remove, got %+v", wts)
	}
}

// TestFlashClearsViaDedicatedTick covers the idle-deck stale-flash fix: with no
// hosted workspaces the agent-status poll is 30s away, so a flash must be
// cleared by its own one-shot expiry tick, not the poll.
func TestFlashClearsViaDedicatedTick(t *testing.T) {
	m := sampleModel()

	// sampleModel hosts no workspaces, so the only *other* expiry path — the
	// agent-status poll — is 30s out. Guard the premise so the test still means
	// something if the cadence changes.
	if iv := m.statusTickInterval(); iv < 30*time.Second {
		t.Fatalf("expected the 30s idle-deck poll cadence, got %v", iv)
	}

	tick := m.flashCmd("creating foo…")
	if tick == nil {
		t.Fatal("flashCmd should return an expiry tick command")
	}
	if m.status != "creating foo…" {
		t.Fatalf("flashCmd should set the status, got %q", m.status)
	}

	// Simulate the tick firing once the TTL has elapsed (backdate rather than
	// sleep). The tick is stamped with the current statusAt, so it matches and
	// clears — exactly what the dedicated timer guarantees on an idle deck.
	m.statusAt = time.Now().Add(-2 * transientStatusTTL)
	mm, _ := m.Update(statusExpireMsg{at: m.statusAt})
	m = mm.(Model)
	if m.status != "" {
		t.Fatalf("dedicated tick should clear the stale flash, got %q", m.status)
	}
}

// TestFlashTickDoesNotClearNewerFlash covers the staleness guard: a tick armed
// by an earlier flash must not clear a newer flash that replaced it.
func TestFlashTickDoesNotClearNewerFlash(t *testing.T) {
	m := sampleModel()

	m.flash("first")
	staleAt := m.statusAt.Add(-time.Second) // the first flash's tick, armed earlier
	m.flash("second")                       // a newer flash supersedes it
	// Age the newer flash past its own TTL so that, absent the guard, the stale
	// tick would wrongly clear it.
	m.statusAt = time.Now().Add(-2 * transientStatusTTL)

	mm, _ := m.Update(statusExpireMsg{at: staleAt})
	m = mm.(Model)
	if m.status != "second" {
		t.Fatalf("a stale tick must not clear a newer flash, got %q", m.status)
	}

	// The newer flash's *own* tick still expires it.
	mm, _ = m.Update(statusExpireMsg{at: m.statusAt})
	m = mm.(Model)
	if m.status != "" {
		t.Fatalf("the newer flash's own tick should clear it, got %q", m.status)
	}
}

// TestHandleCreateKeyValidation checks that an invalid worktree name is rejected
// inline without dispatching a create, while a valid name (including interior
// slashes) proceeds.
func TestHandleCreateKeyValidation(t *testing.T) {
	m := sampleModel()
	m.pendingRepo = repo.Repo{Name: "api", Path: t.TempDir()}
	m.mode = modeCreateName
	m.nameInput.Focus()

	// Invalid: rejected inline. Error status, form stays open, no create cmd.
	m.nameInput.SetValue("../escape")
	mm, cmd := m.handleCreateKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if cmd != nil {
		t.Fatal("invalid name should not dispatch a create command")
	}
	if m.mode != modeCreateName {
		t.Fatalf("invalid name should keep the create form open, got mode %v", m.mode)
	}
	if m.statusLevel != statusError || m.status == "" {
		t.Fatalf("invalid name should set an error status, got %q level=%v", m.status, m.statusLevel)
	}

	// Valid (interior slash namespacing): form closes, progress flashes, and a
	// create command is dispatched.
	m.nameInput.SetValue("feature/foo")
	mm, cmd = m.handleCreateKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if cmd == nil {
		t.Fatal("valid name should dispatch a create command")
	}
	if m.mode != modeNormal {
		t.Fatalf("valid name should close the form, got mode %v", m.mode)
	}
	if m.statusLevel != statusInfo || m.status != "creating feature/foo…" {
		t.Fatalf("valid name should flash progress, got %q level=%v", m.status, m.statusLevel)
	}
}

// TestListNavDialect: the shared list-nav dialect — ctrl+p (up) / ctrl+n (down)
// — moves the cursor in the ACTIVE and NEW deck lists (called directly, since at
// the default binding ctrl+n is intercepted upstream to open the board).
func TestListNavDialect(t *testing.T) {
	m := sampleModel()
	m.focus = focusActive
	if len(m.active) < 2 {
		t.Fatalf("precondition: need >=2 active items, got %d", len(m.active))
	}
	m.activeCursor = 0
	mm, _ := m.handleActiveKey(ctrlKey('n')) // down
	m = mm.(Model)
	if m.activeCursor != 1 {
		t.Fatalf("ctrl+n should move the ACTIVE cursor down, got %d", m.activeCursor)
	}
	mm, _ = m.handleActiveKey(ctrlKey('p')) // up
	m = mm.(Model)
	if m.activeCursor != 0 {
		t.Fatalf("ctrl+p should move the ACTIVE cursor up, got %d", m.activeCursor)
	}

	m2 := sampleModel() // focusNew, filter focused
	if len(m2.repoMatches) < 2 {
		t.Fatalf("precondition: need >=2 repo matches, got %d", len(m2.repoMatches))
	}
	m2.newCursor = 0
	mm, _ = m2.handleNewKey(ctrlKey('n'))
	m2 = mm.(Model)
	if m2.newCursor != 1 {
		t.Fatalf("ctrl+n should move the NEW cursor down, got %d", m2.newCursor)
	}
	mm, _ = m2.handleNewKey(ctrlKey('p'))
	m2 = mm.(Model)
	if m2.newCursor != 0 {
		t.Fatalf("ctrl+p should move the NEW cursor up, got %d", m2.newCursor)
	}
}

// TestDeckCtrlNMovesCursor: ctrl+n in the deck is a pure list-down key — it
// moves the ACTIVE cursor and never opens the agent board.
func TestDeckCtrlNMovesCursor(t *testing.T) {
	m := sampleModel()
	m.focus = focusActive
	if len(m.active) < 2 {
		t.Fatalf("precondition: need >=2 active items, got %d", len(m.active))
	}
	m.activeCursor = 0
	mm, _ := m.handleKey(ctrlKey('n'))
	m = mm.(Model)
	if m.boardOpen {
		t.Fatal("ctrl+n in the deck must not open the board")
	}
	if m.activeCursor != 1 {
		t.Fatalf("ctrl+n should move the ACTIVE cursor down, got %d", m.activeCursor)
	}
}

// TestBoardCtrlNNavigates: inside an open board ctrl+n/ctrl+p navigate and do not
// close it, while ctrl+a and esc still close.
func TestBoardCtrlNNavigates(t *testing.T) {
	m := modelWithAgents(3)
	m.current.ws.ActiveAgent = 0
	mm, _ := m.openBoard()
	m = mm.(Model)
	start := m.boardCursor

	mm, _ = m.handleBoard(ctrlKey('n'))
	m = mm.(Model)
	if !m.boardOpen {
		t.Fatal("ctrl+n should not close the board (it navigates instead)")
	}
	if m.boardCursor != start+1 {
		t.Fatalf("ctrl+n should move the board cursor down, got %d want %d", m.boardCursor, start+1)
	}
	mm, _ = m.handleBoard(ctrlKey('p'))
	m = mm.(Model)
	if m.boardCursor != start {
		t.Fatalf("ctrl+p should move the board cursor back up, got %d", m.boardCursor)
	}

	// alt+a (palette) still closes.
	mm, _ = m.handleBoard(altKey('a'))
	m = mm.(Model)
	if m.boardOpen {
		t.Fatal("alt+a should still close the board")
	}

	// esc still closes.
	mm, _ = m.openBoard()
	m = mm.(Model)
	mm, _ = m.handleBoard(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = mm.(Model)
	if m.boardOpen {
		t.Fatal("esc should still close the board")
	}
}

// TestGotoScreenKeys: the direct-jump keys (alt+1/2/3) switch straight to the
// editor/agent/terminal views from a session screen and from the picker when a
// workspace is active, clear attention on landing on the agent screen, and
// no-op when no workspace is active.
func TestGotoScreenKeys(t *testing.T) {
	ws := &session.Workspace{
		Editor: &session.Session{},
		Terms:  []*session.Session{{}},
		Agents: []*session.Session{{}},
	}

	// From a session screen: alt+3 jumps to the terminal, alt+1 back to editor.
	m := sampleModel()
	m.current = &workspaceRef{repo: "r", worktree: "w", key: "r/w", path: "/r/w", ws: ws}
	m.screen = screenEditor
	mm, _ := m.handleKey(altKey('3'))
	m = mm.(Model)
	if m.screen != screenTerminal {
		t.Fatalf("alt+3 should jump to the terminal, got %v", m.screen)
	}
	mm, _ = m.handleKey(altKey('1'))
	m = mm.(Model)
	if m.screen != screenEditor {
		t.Fatalf("alt+1 should jump to the editor, got %v", m.screen)
	}

	// alt+2 lands on the agent screen and clears the workspace's markers.
	m.attention = map[int]attnEntry{1: {level: attnDone, key: "r/w"}}
	mm, _ = m.handleKey(altKey('2'))
	m = mm.(Model)
	if m.screen != screenAgent {
		t.Fatalf("alt+2 should jump to the agent screen, got %v", m.screen)
	}
	if _, ok := m.attention[1]; ok {
		t.Error("landing on the agent screen should clear the workspace's markers")
	}

	// From the picker with a workspace active: the jump still works.
	m.screen = screenPicker
	mm, _ = m.handleKey(altKey('3'))
	m = mm.(Model)
	if m.screen != screenTerminal {
		t.Fatalf("alt+3 should jump from the picker too, got %v", m.screen)
	}

	// With no active workspace the jump is a swallowed no-op (stays on picker).
	n := sampleModel() // picker, current == nil
	mm, _ = n.handleKey(altKey('2'))
	n = mm.(Model)
	if n.screen != screenPicker || n.current != nil {
		t.Fatalf("goto with no workspace should no-op, screen=%v current=%v", n.screen, n.current)
	}
}

// TestHelpReservedKeyReDispatches: a reserved action key pressed while help is
// open closes the overlay AND performs its action in one press.
func TestHelpReservedKeyReDispatches(t *testing.T) {
	m := sampleModel()
	m.current = &workspaceRef{repo: "r", worktree: "w", key: "r/w"}
	m.screen = screenEditor
	m.helpOpen = true

	mm, _ := m.handleKey(altKey(']')) // default keyCycle == alt+]
	m = mm.(Model)
	if m.helpOpen {
		t.Fatal("a reserved key should still close the help overlay")
	}
	if m.screen != screenAgent {
		t.Fatalf("the cycle key should also advance editor→agent, got %v", m.screen)
	}
}

// TestHelpPlainKeyDoesNotLeak: a non-reserved key dismissing help must never
// leak into the embedded session drawn beneath the overlay.
func TestHelpPlainKeyDoesNotLeak(t *testing.T) {
	m, sess := pasteModel(t)
	if sess == nil {
		t.Fatal("expected an active session")
	}
	m.helpOpen = true
	mm, _ := m.handleKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m = mm.(Model)
	if m.helpOpen {
		t.Fatal("a plain key should close the help overlay")
	}
	// Once an allowed keystroke sent afterward shows on screen, a leaked 'x'
	// would already be visible too — its absence proves it was swallowed.
	mm, _ = m.handleKey(tea.KeyPressMsg{Code: 'Z', Text: "Z"})
	m = mm.(Model)
	waitForSession(t, sess, "Z")
	if strings.Contains(sess.Render(), "x") {
		t.Error("a plain key dismissing help leaked into the session")
	}
}

// --- diff viewer ---

// diffKey builds a KeyPressMsg for a single rune (Text set so Key.String()
// returns the rune, as the diff routing keys off msg.String()).
func diffKey(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Text: string(r)} }

// openSampleDiff opens the diff for active[idx] of a sampleModel and delivers a
// diffMsg with the given payload, returning the model with content built. The
// selected worktree is given a base branch (so the vs-base section renders) and
// marked ahead/dirty per the flags.
func openSampleDiff(t *testing.T, idx int, committed, uncommitted []repo.FileStat, cbody, ubody string, untracked []string, dirty bool) Model {
	t.Helper()
	m := sampleModel()
	m.focus = focusActive
	m.activeCursor = idx
	m.active[idx].view.BaseBranch = "main"
	m.active[idx].view.Ahead = 3
	m.active[idx].view.Dirty = dirty
	mm, cmd := m.openDiff(m.active[idx])
	m = mm.(Model)
	if !m.diffOpen || !m.diffView.loading || cmd == nil {
		t.Fatalf("openDiff: open=%v loading=%v cmd=%v", m.diffOpen, m.diffView.loading, cmd != nil)
	}
	res, _ := m.update(diffMsg{
		key:             m.diffView.key,
		committedBody:   cbody,
		committedStat:   committed,
		uncommittedBody: ubody,
		uncommittedStat: uncommitted,
		untracked:       untracked,
	})
	return res.(Model)
}

func manyLineDiff(n int) string {
	var b strings.Builder
	b.WriteString("diff --git a/f.go b/f.go\n@@ -1,1 +1,")
	b.WriteString(strconv.Itoa(n))
	b.WriteString(" @@\n")
	for i := 0; i < n; i++ {
		b.WriteString("+line ")
		b.WriteString(strconv.Itoa(i))
		b.WriteString("\n")
	}
	return b.String()
}

func TestDiffOpensFromDeckKey(t *testing.T) {
	m := sampleModel()
	m.focus = focusActive
	m.activeCursor = 0
	mm, cmd := m.handleActiveKey(diffKey('v'))
	m = mm.(Model)
	if !m.diffOpen || !m.diffView.loading {
		t.Fatalf("v should open the diff in the loading state: open=%v loading=%v", m.diffOpen, m.diffView.loading)
	}
	if cmd == nil {
		t.Error("opening the diff should return a fetch command")
	}
	it, _ := m.selectedActive()
	if m.diffView.key != wsKey(it.repo.Name, it.view.WT.Name) {
		t.Errorf("diff target key = %q, want %q", m.diffView.key, wsKey(it.repo.Name, it.view.WT.Name))
	}
}

func TestDiffMsgRendersAndStaleDropped(t *testing.T) {
	stat := []repo.FileStat{{Path: "main.go", Add: 4, Del: 2}}
	m := openSampleDiff(t, 0, stat, nil,
		"diff --git a/main.go b/main.go\n@@ -1,2 +1,2 @@\n-old\n+new\n", "", nil, false)
	if m.diffView.loading {
		t.Fatal("diffMsg arrival should clear the loading state")
	}
	if len(m.diffView.full.lines) == 0 {
		t.Fatal("content should be built on diffMsg arrival")
	}
	out := m.renderDiff(m.height - barHeight)
	for _, want := range []string{"vs main", "main.go", "[all]", "scroll"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered diff missing %q:\n%s", want, out)
		}
	}

	// A diffMsg carrying a different (stale) key is dropped: content unchanged.
	before := len(m.diffView.full.lines)
	res, _ := m.update(diffMsg{key: "someone/else", committedBody: "junk"})
	m = res.(Model)
	if len(m.diffView.full.lines) != before {
		t.Error("a stale-key diffMsg should be dropped, not applied")
	}
}

func TestDiffErrorClosesOverlay(t *testing.T) {
	m := sampleModel()
	m.focus = focusActive
	m.activeCursor = 0
	mm, _ := m.openDiff(m.active[0])
	m = mm.(Model)
	res, _ := m.update(diffMsg{key: m.diffView.key, err: errTest})
	m = res.(Model)
	if m.diffOpen {
		t.Error("a diff error should close the overlay")
	}
	if m.statusLevel != statusError || !strings.Contains(m.status, "diff error") {
		t.Errorf("a diff error should set a sticky error status, got level=%v status=%q", m.statusLevel, m.status)
	}
}

var errTest = &testErr{}

type testErr struct{}

func (*testErr) Error() string { return "boom" }

func TestDiffScrollClamping(t *testing.T) {
	m := openSampleDiff(t, 0, []repo.FileStat{{Path: "f.go", Add: 40}}, nil, manyLineDiff(40), "", nil, false)
	avail := m.diffBodyHeight()
	maxOff := max(0, len(m.diffView.full.lines)-avail)
	if maxOff == 0 {
		t.Fatal("test needs content taller than the viewport")
	}

	// Up at the top stays at 0.
	mm, _ := m.handleDiff(diffKey('k'))
	m = mm.(Model)
	if m.diffView.offset != 0 {
		t.Fatalf("up at the top should stay at 0, got %d", m.diffView.offset)
	}
	// G jumps to the bottom (maxOff); further down clamps there.
	mm, _ = m.handleDiff(tea.KeyPressMsg{Code: 'G', Text: "G"})
	m = mm.(Model)
	if m.diffView.offset != maxOff {
		t.Fatalf("G should land on maxOff %d, got %d", maxOff, m.diffView.offset)
	}
	for i := 0; i < 5; i++ {
		mm, _ = m.handleDiff(diffKey('j'))
		m = mm.(Model)
	}
	if m.diffView.offset != maxOff {
		t.Fatalf("down past the bottom should clamp at %d, got %d", maxOff, m.diffView.offset)
	}
	// g returns to the top.
	mm, _ = m.handleDiff(diffKey('g'))
	m = mm.(Model)
	if m.diffView.offset != 0 {
		t.Fatalf("g should return to the top, got %d", m.diffView.offset)
	}
}

func TestDiffScopeToggleResetsOffset(t *testing.T) {
	m := openSampleDiff(t, 0,
		[]repo.FileStat{{Path: "committed.go", Add: 40}}, []repo.FileStat{{Path: "wip.go", Add: 1}},
		manyLineDiff(40), "diff --git a/wip.go b/wip.go\n@@ -1,1 +1,1 @@\n+wip\n", nil, true)

	// Scroll down in the full scope, then toggle to uncommitted.
	mm, _ := m.handleDiff(tea.KeyPressMsg{Code: 'G', Text: "G"})
	m = mm.(Model)
	if m.diffView.offset == 0 {
		t.Fatal("precondition: expected a non-zero offset in the full scope")
	}
	mm, _ = m.handleDiff(diffKey('u'))
	m = mm.(Model)
	if !m.diffView.scopeUncommitted {
		t.Fatal("u should switch to the uncommitted scope")
	}
	if m.diffView.offset != 0 {
		t.Fatalf("scope toggle should reset the offset to 0, got %d", m.diffView.offset)
	}
	// The uncommitted scope is shorter than the full scope and mentions wip.go.
	if len(m.diffView.uncommitted.lines) >= len(m.diffView.full.lines) {
		t.Error("the uncommitted scope should be a subset of the full scope")
	}
	out := m.renderDiff(m.height - barHeight)
	if !strings.Contains(out, "[uncommitted]") || !strings.Contains(out, "wip.go") {
		t.Errorf("uncommitted scope render missing content:\n%s", out)
	}
}

func TestDiffFileJump(t *testing.T) {
	// Two files, each tall enough that the content overflows the viewport so the
	// second file's header sits at a scrollable offset (jumps clamp to the
	// bottom when everything already fits).
	fileBody := func(name string) string {
		var b strings.Builder
		b.WriteString("diff --git a/" + name + " b/" + name + "\n@@ -1,1 +1,30 @@\n")
		for i := 0; i < 30; i++ {
			b.WriteString("+line\n")
		}
		return b.String()
	}
	body := fileBody("one.go") + fileBody("two.go")
	m := openSampleDiff(t, 0, []repo.FileStat{{Path: "one.go", Add: 30}, {Path: "two.go", Add: 30}}, nil, body, "", nil, false)
	fl := m.diffView.full.fileLines
	if len(fl) < 2 {
		t.Fatalf("expected at least 2 file-header lines, got %d", len(fl))
	}
	// J from the top lands on the first file header past offset 0.
	mm, _ := m.handleDiff(tea.KeyPressMsg{Code: 'J', Text: "J"})
	m = mm.(Model)
	if !contains(fl, m.diffView.offset) {
		t.Fatalf("J should land on a file-header line, got offset %d (fileLines %v)", m.diffView.offset, fl)
	}
	landed := m.diffView.offset
	// K from there lands on the previous file header (or the top).
	mm, _ = m.handleDiff(tea.KeyPressMsg{Code: 'K', Text: "K"})
	m = mm.(Model)
	if m.diffView.offset >= landed {
		t.Fatalf("K should move to an earlier file header, got %d (was %d)", m.diffView.offset, landed)
	}
}

func contains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func TestDiffXEntersRemoveFlow(t *testing.T) {
	m := openSampleDiff(t, 0, []repo.FileStat{{Path: "f.go", Add: 1}}, nil,
		"diff --git a/f.go b/f.go\n@@ -1,1 +1,1 @@\n+x\n", "", nil, true)
	mm, _ := m.handleDiff(diffKey('x'))
	m = mm.(Model)
	if m.diffOpen {
		t.Error("x should close the diff")
	}
	if m.mode != modeConfirmRemove {
		t.Fatalf("x should enter the remove confirm, got mode %v", m.mode)
	}
	if !strings.Contains(m.status, "UNCOMMITTED CHANGES") || !strings.Contains(m.status, "view diff") {
		t.Errorf("dirty remove prompt should warn and offer view diff, got %q", m.status)
	}
}

func TestDiffFromRemovePrompt(t *testing.T) {
	m := sampleModel()
	m.focus = focusActive
	m.activeCursor = 0
	m.mode = modeConfirmRemove
	m.status = "remove worktree …"
	mm, cmd := m.handleConfirmKey(diffKey('v'))
	m = mm.(Model)
	if m.mode != modeNormal {
		t.Errorf("v should exit the confirm mode, got %v", m.mode)
	}
	if !m.diffOpen || !m.diffView.loading {
		t.Fatalf("v from the remove prompt should open the diff: open=%v loading=%v", m.diffOpen, m.diffView.loading)
	}
	if cmd == nil {
		t.Error("opening the diff should return a fetch command")
	}
}

func TestPaletteViewDiffRow(t *testing.T) {
	m := sampleModel()
	m.focus = focusActive
	m.activeCursor = 0

	// The row exists for a chosen worktree (not active[0]) and moving to it must
	// move the active cursor onto that worktree.
	target := m.active[len(m.active)-1]
	wantTitle := "view diff: " + target.repo.Name + "/" + target.view.WT.Name
	var run func(Model) (tea.Model, tea.Cmd)
	for _, c := range m.paletteCommands() {
		if c.title == wantTitle {
			run = c.run
		}
	}
	if run == nil {
		t.Fatalf("palette should contain %q", wantTitle)
	}
	mm, cmd := run(m)
	m = mm.(Model)
	if !m.diffOpen || m.diffView.loading == false || cmd == nil {
		t.Fatalf("running the view-diff row should open the diff: open=%v loading=%v", m.diffOpen, m.diffView.loading)
	}
	if m.screen != screenPicker || m.focus != focusActive {
		t.Errorf("view-diff row should return to the picker's active section: screen=%v focus=%v", m.screen, m.focus)
	}
	if m.diffView.key != wsKey(target.repo.Name, target.view.WT.Name) {
		t.Errorf("view-diff row opened the wrong worktree: %q", m.diffView.key)
	}
	if it, _ := m.selectedActive(); wsKey(it.repo.Name, it.view.WT.Name) != m.diffView.key {
		t.Error("active cursor should have moved onto the diff's worktree")
	}
}

func TestDiffWheelScrolls(t *testing.T) {
	m := openSampleDiff(t, 0, []repo.FileStat{{Path: "f.go", Add: 40}}, nil, manyLineDiff(40), "", nil, false)
	mm, _ := m.diffWheel(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	m = mm.(Model)
	if m.diffView.offset != 3 {
		t.Fatalf("a wheel-down notch should scroll 3 lines, got %d", m.diffView.offset)
	}
	mm, _ = m.diffWheel(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	m = mm.(Model)
	if m.diffView.offset != 0 {
		t.Fatalf("a wheel-up notch should scroll back, got %d", m.diffView.offset)
	}
}

func TestDiffSwallowsUnknownKeys(t *testing.T) {
	m := openSampleDiff(t, 0, []repo.FileStat{{Path: "f.go", Add: 40}}, nil, manyLineDiff(40), "", nil, false)
	m.diffView.offset = 5
	mm, _ := m.handleDiff(diffKey('z'))
	m = mm.(Model)
	if !m.diffOpen {
		t.Error("an unknown key should not close the diff")
	}
	if m.diffView.offset != 5 {
		t.Errorf("an unknown key should not move the offset, got %d", m.diffView.offset)
	}
	if m.mode != modeNormal {
		t.Errorf("an unknown key should not change the mode, got %v", m.mode)
	}
	// esc / q close.
	mm, _ = m.handleDiff(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = mm.(Model)
	if m.diffOpen {
		t.Error("esc should close the diff")
	}
}
