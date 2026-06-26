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

func TestActiveSessionByScreen(t *testing.T) {
	ed := &session.Session{}
	ag0 := &session.Session{}
	ag1 := &session.Session{}
	tm := &session.Session{}
	ws := &session.Workspace{Editor: ed, Term: tm, Agents: []*session.Session{ag0, ag1}, ActiveAgent: 1}

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

func TestRotateAgentWraps(t *testing.T) {
	m := modelWithAgents(3)

	m = m.rotateAgent(+1).(Model)
	if m.current.ws.ActiveAgent != 1 || m.screen != screenAgent {
		t.Fatalf("next: active=%d screen=%v", m.current.ws.ActiveAgent, m.screen)
	}
	m = m.rotateAgent(+1).(Model)
	m = m.rotateAgent(+1).(Model) // 2 -> wrap to 0
	if m.current.ws.ActiveAgent != 0 {
		t.Fatalf("expected wrap to 0, got %d", m.current.ws.ActiveAgent)
	}
	m = m.rotateAgent(-1).(Model) // 0 -> wrap to 2
	if m.current.ws.ActiveAgent != 2 {
		t.Fatalf("expected wrap to 2, got %d", m.current.ws.ActiveAgent)
	}

	// No agents: rotate is a no-op and doesn't switch the screen.
	empty := modelWithAgents(0)
	if got := empty.rotateAgent(+1).(Model); got.screen == screenAgent {
		t.Error("rotate with no agents should not switch to the agent view")
	}
}

func TestPaletteNavigateAndFocus(t *testing.T) {
	m := modelWithAgents(3)
	m.current.ws.ActiveAgent = 0

	m = m.openPalette().(Model)
	if !m.paletteOpen || m.paletteCursor != 0 {
		t.Fatalf("open: open=%v cursor=%d", m.paletteOpen, m.paletteCursor)
	}

	// Down moves the cursor; it can reach the trailing "+ new agent" row (index 3)
	// but no further.
	for i := 0; i < 5; i++ {
		mm, _ := m.handlePalette(tea.KeyPressMsg{Code: tea.KeyDown})
		m = mm.(Model)
	}
	if m.paletteCursor != 3 {
		t.Fatalf("cursor should clamp at the new-agent row (3), got %d", m.paletteCursor)
	}

	// Up back to agent 1, then enter focuses it and closes the palette.
	for i := 0; i < 2; i++ {
		mm, _ := m.handlePalette(tea.KeyPressMsg{Code: tea.KeyUp})
		m = mm.(Model)
	}
	mm, _ := m.handlePalette(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if m.paletteOpen {
		t.Error("enter should close the palette")
	}
	if m.current.ws.ActiveAgent != 1 || m.screen != screenAgent {
		t.Fatalf("enter should focus agent 1 on the agent screen, got active=%d screen=%v", m.current.ws.ActiveAgent, m.screen)
	}
}

func TestPaletteDigitJump(t *testing.T) {
	m := modelWithAgents(3)
	m.current.ws.ActiveAgent = 0
	m = m.openPalette().(Model)

	// "3" jumps straight to agent index 2, focuses it, and closes the palette.
	mm, _ := m.handlePalette(tea.KeyPressMsg{Code: '3', Text: "3"})
	m = mm.(Model)
	if m.current.ws.ActiveAgent != 2 || m.screen != screenAgent || m.paletteOpen {
		t.Fatalf("digit jump: active=%d screen=%v open=%v", m.current.ws.ActiveAgent, m.screen, m.paletteOpen)
	}

	// A digit past the pool size is ignored (palette stays open).
	m = m.openPalette().(Model)
	mm, _ = m.handlePalette(tea.KeyPressMsg{Code: '9', Text: "9"})
	m = mm.(Model)
	if !m.paletteOpen {
		t.Error("out-of-range digit should be a no-op (palette stays open)")
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
	m.status = "open error: boom"
	m.statusAt = m.statusAt.Add(-2 * transientStatusTTL)
	m.maybeExpireStatus()
	if m.status == "" {
		t.Error("error status should not auto-expire")
	}
}

func TestPaletteEnterNewRowStartsNaming(t *testing.T) {
	m := modelWithAgents(2)
	m = m.openPalette().(Model)
	m.paletteCursor = 2 // the "+ new agent" row

	mm, _ := m.handlePalette(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if !m.naming {
		t.Error("entering on the new-agent row should begin naming")
	}
}

func TestWorktreeStatusRollup(t *testing.T) {
	m := sampleModel()
	m.agentStatus = map[int]AgentStatus{
		1: {Status: "busy", Cwd: "/r/a"},
		2: {Status: "idle", Cwd: "/r/a"},
		3: {Status: "waiting", Cwd: "/r/b"},
	}

	if got := m.worktreeLevel("/r/a"); got != levelIdle {
		t.Errorf("/r/a should roll up to idle (worst of busy+idle), got %v", got)
	}
	if got := m.worktreeLevel("/r/b"); got != levelWaiting {
		t.Errorf("/r/b should roll up to waiting, got %v", got)
	}
	if got := m.worktreeLevel("/r/none"); got != levelNone {
		t.Errorf("unknown worktree should be levelNone, got %v", got)
	}

	// otherWorktreesLevel takes the worst across worktrees != current.
	m.active = []activeItem{
		{view: WorktreeView{WT: repo.Worktree{Path: "/r/a"}}},
		{view: WorktreeView{WT: repo.Worktree{Path: "/r/b"}}},
	}
	m.current = &workspaceRef{path: "/r/a"}
	if got := m.otherWorktreesLevel(); got != levelWaiting {
		t.Errorf("other worktrees should surface /r/b waiting, got %v", got)
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
	if m.current.ws.Editor == nil || m.current.ws.Term == nil || len(m.current.ws.Agents) != 1 {
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
