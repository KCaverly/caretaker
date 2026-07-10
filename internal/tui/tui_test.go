package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/KCaverly/caretaker/internal/config"
	"github.com/KCaverly/caretaker/internal/repo"
	"github.com/KCaverly/caretaker/internal/session"
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

	ctrl := &Controller{cfg: config.Config{
		Editor: "cat", Agent: "cat", Shell: "sh",
		Keys: config.Keys{Cycle: "ctrl+o", Picker: "ctrl+g"},
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

func TestDeckClickOpensSelectedWorktree(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctrl := &Controller{cfg: config.Config{
		Editor: "cat", Agent: "cat", Shell: "sh",
		Keys: config.Keys{Cycle: "ctrl+o", Picker: "ctrl+g"},
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
	m.keyHelp = "f1"

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
	for _, want := range []string{"HELP", "Session", "Legend", m.keyCycle, m.keyPicker, m.keyPalette, "uncommitted"} {
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
	// Inside an open board ctrl+n is list-down (the notif close alias is retired):
	// it keeps the board open. The primary palette key closes.
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
	ctrl := &Controller{cfg: config.Config{
		Editor: "cat", Agent: "cat", Shell: "sh",
		Keys: config.Keys{Cycle: "ctrl+o", Picker: "ctrl+g"},
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

// TestBoardSortsAttentionFirst activates two real workspaces and checks that
// the one with an unread marker sorts above the current one, and that the
// board cursor opens on it.
func TestBoardSortsAttentionFirst(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctrl := &Controller{cfg: config.Config{
		Editor: "cat", Agent: "cat", Shell: "sh",
		Keys: config.Keys{Cycle: "ctrl+o", Picker: "ctrl+g"},
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
	for _, want := range []string{m.keyHelp, "help", "split", "zoom"} {
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
	ctrl := &Controller{cfg: config.Config{
		Editor: "cat", Agent: "cat", Shell: "sh",
		Keys: config.Keys{Cycle: "ctrl+o", Picker: "ctrl+g"},
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

// TestDeckCtrlNMovesCursor: with the notif alias retired (empty default), ctrl+n
// in the deck is a pure list-down key — it moves the ACTIVE cursor and never
// opens the agent board (handleKey no longer intercepts it).
func TestDeckCtrlNMovesCursor(t *testing.T) {
	m := sampleModel()
	if m.keyNotif != "" {
		t.Fatalf("precondition: notif should default empty, got %q", m.keyNotif)
	}
	m.focus = focusActive
	if len(m.active) < 2 {
		t.Fatalf("precondition: need >=2 active items, got %d", len(m.active))
	}
	m.activeCursor = 0
	mm, _ := m.handleKey(ctrlKey('n'))
	m = mm.(Model)
	if m.boardOpen {
		t.Fatal("ctrl+n in the deck must not open the board once notif is retired")
	}
	if m.activeCursor != 1 {
		t.Fatalf("ctrl+n should move the ACTIVE cursor down, got %d", m.activeCursor)
	}
}

// TestBoardCtrlNNavigates: inside an open board ctrl+n/ctrl+p navigate and do not
// close it (nav wins over the legacy close alias), while ctrl+a and esc still
// close.
func TestBoardCtrlNNavigates(t *testing.T) {
	m := modelWithAgents(3)
	m.current.ws.ActiveAgent = 0
	mm, _ := m.openBoard()
	m = mm.(Model)
	start := m.boardCursor

	mm, _ = m.handleBoard(ctrlKey('n'))
	m = mm.(Model)
	if !m.boardOpen {
		t.Fatal("ctrl+n should not close the board (nav wins over the close alias)")
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

// TestKeyNotifReboundFreesCtrlN: when the user rebinds keyNotif off ctrl+n, the
// rebound key closes the board and ctrl+n becomes an ordinary list-down key both
// in the board and in the deck.
func TestKeyNotifReboundFreesCtrlN(t *testing.T) {
	m := modelWithAgents(3)
	m.keyNotif = "ctrl+b"
	m.current.ws.ActiveAgent = 0
	mm, _ := m.openBoard()
	m = mm.(Model)
	start := m.boardCursor

	mm, _ = m.handleBoard(ctrlKey('n'))
	m = mm.(Model)
	if !m.boardOpen || m.boardCursor != start+1 {
		t.Fatalf("rebound: ctrl+n should navigate the board, open=%v cursor=%d", m.boardOpen, m.boardCursor)
	}
	mm, _ = m.handleBoard(ctrlKey('b'))
	m = mm.(Model)
	if m.boardOpen {
		t.Fatal("the rebound keyNotif should close the board")
	}

	// In the deck ctrl+n is now free and reaches the ACTIVE list as list-down.
	d := sampleModel()
	d.keyNotif = "ctrl+b"
	d.focus = focusActive
	d.activeCursor = 0
	mm, _ = d.handleKey(ctrlKey('n'))
	d = mm.(Model)
	if d.boardOpen {
		t.Fatal("rebound: ctrl+n must not open the board")
	}
	if d.activeCursor != 1 {
		t.Fatalf("rebound: ctrl+n should move the ACTIVE cursor, got %d", d.activeCursor)
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
