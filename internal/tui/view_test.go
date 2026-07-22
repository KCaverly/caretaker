package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/KCaverly/caretaker/internal/agent"
	"github.com/KCaverly/caretaker/internal/config"
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
	m := New(&Controller{cfg: config.Config{Keys: config.Default().Keys}}, session.NewManager())
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

func TestSelectionBarPreservesSemanticColors(t *testing.T) {
	for name, semantic := range map[string]string{
		"waiting": boldRed.Render("waiting · permission"),
		"ready":   boldGreen.Render("ready · new output"),
		"working": accentStyle.Render("working · 41s"),
	} {
		t.Run(name, func(t *testing.T) {
			selected := selBar("agent  "+semantic, 48)
			if !strings.Contains(ansi.Strip(selected), ansi.Strip(semantic)) {
				t.Fatalf("selection bar replaced %s semantic text:\n%s", name, selected)
			}
			// Every nested reset must immediately restore the selection style;
			// only selBar's final outer reset is allowed to leave it off.
			resets := strings.Count(selected, ansi.ResetStyle)
			restored := strings.Count(selected, ansi.ResetStyle+selANSI)
			if restored != resets-1 {
				t.Fatalf("selection background restored after %d/%d nested resets", restored, resets-1)
			}
			if w := lipgloss.Width(selected); w != 48 {
				t.Fatalf("selected row width = %d, want 48", w)
			}
		})
	}
}

// TestActiveRowStateCluster checks the right-aligned work-state cluster: ↑N
// when ahead, ↓M when behind, both when diverged, and a dim — when there is no
// base to compare against or the branch is level with it. Selected and
// unselected rows stay the same width so the cluster's column never shifts.
func TestActiveRowStateCluster(t *testing.T) {
	m := sampleModel()
	innerW := m.width - 4

	cases := []struct {
		name  string
		view  WorktreeView
		wants []string // each glyph is styled on its own, so match them separately
	}{
		{"ahead only", WorktreeView{WT: repo.Worktree{Name: "wt"}, HasBase: true, Ahead: 5}, []string{"↑5"}},
		{"behind only", WorktreeView{WT: repo.Worktree{Name: "wt"}, HasBase: true, Behind: 3}, []string{"↓3"}},
		{"diverged", WorktreeView{WT: repo.Worktree{Name: "wt"}, HasBase: true, Ahead: 5, Behind: 3}, []string{"↑5", "↓3"}},
		{"no base", WorktreeView{WT: repo.Worktree{Name: "wt"}}, []string{"—"}},
		{"level with main", WorktreeView{WT: repo.Worktree{Name: "wt"}, HasBase: true}, []string{"—"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			it := activeItem{repo: repo.Repo{Name: "r"}, view: tc.view}
			row := m.activeRow(it, false, innerW)
			sel := m.activeRow(it, true, innerW)
			for _, want := range tc.wants {
				if !strings.Contains(row, want) {
					t.Errorf("row missing cluster %q:\n%s", want, row)
				}
				if !strings.Contains(sel, want) {
					t.Errorf("selected row missing cluster %q:\n%s", want, sel)
				}
			}
			if w := lipgloss.Width(row); w != innerW {
				t.Errorf("unselected row width = %d, want %d", w, innerW)
			}
			if w := lipgloss.Width(sel); w != innerW {
				t.Errorf("selected row width = %d, want %d", w, innerW)
			}
		})
	}

	// A diverged row must not also show the — placeholder.
	it := activeItem{repo: repo.Repo{Name: "r"}, view: cases[2].view}
	if row := m.activeRow(it, false, innerW); strings.Contains(row, "—") {
		t.Errorf("diverged row should not show the — placeholder:\n%s", row)
	}
}

// TestActiveRowNarrowWidth drives the row and detail line through very narrow
// widths: the name truncates to keep the cluster on the row, and nothing panics.
func TestActiveRowNarrowWidth(t *testing.T) {
	m := sampleModel()
	it := activeItem{repo: repo.Repo{Name: "r"}, view: WorktreeView{
		WT:      repo.Worktree{Name: "a-very-long-worktree-name-indeed"},
		HasBase: true, Ahead: 12, Behind: 34, Dirty: true, Add: 100, Del: 200,
		Subject: "A long subject line that cannot possibly fit", CommitTime: 1,
	}}
	for _, innerW := range []int{1, 5, 10, 18, 24, 40} {
		_ = m.activeRow(it, false, innerW)
		_ = m.activeRow(it, true, innerW)
		_ = activeDetail(it.view, "", innerW)
	}
	// At a comfortable width the long name still yields a full-width row with
	// the cluster present.
	row := m.activeRow(it, false, 40)
	if !strings.Contains(row, "↑12") || !strings.Contains(row, "↓34") {
		t.Errorf("narrowed row lost its cluster:\n%s", row)
	}
	if w := lipgloss.Width(row); w != 40 {
		t.Errorf("row width = %d, want 40", w)
	}
}

// TestActiveDetailSegments checks that the └ line adds context not already
// visible in the row: the uncommitted diffstat, quoted subject, and age. Branch
// divergence stays exclusively in the row's ↑N/↓N cluster.
func TestActiveDetailSegments(t *testing.T) {
	now := time.Now().Add(-2 * time.Hour).Unix()
	cases := []struct {
		name    string
		view    WorktreeView
		want    []string
		notWant []string
	}{
		{
			"full",
			WorktreeView{HasBase: true, Ahead: 2, Dirty: true, Add: 614, Del: 12,
				Subject: "Add plasma panel", CommitTime: now},
			[]string{"└", "+614 −12 uncommitted", `"Add plasma panel"`, "2h00m"},
			[]string{"ahead", "behind"},
		},
		{
			"divergence omitted",
			WorktreeView{HasBase: true, Ahead: 2, Behind: 3},
			nil,
			[]string{"ahead", "behind"},
		},
		{
			"no base omits divergence entirely",
			WorktreeView{Subject: "init", CommitTime: now},
			[]string{`"init"`},
			[]string{"ahead", "behind", "no commits yet"},
		},
		{
			"clean tree omits diffstat",
			WorktreeView{HasBase: true, Ahead: 1, Dirty: false, Add: 9, Del: 9},
			nil,
			[]string{"ahead", "uncommitted"},
		},
		{
			"dirty with zero lines omits diffstat",
			WorktreeView{HasBase: true, Ahead: 1, Dirty: true},
			nil,
			[]string{"ahead", "uncommitted"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := activeDetail(tc.view, "", 80)
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("detail missing %q:\n%s", want, out)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(out, notWant) {
					t.Errorf("detail should not contain %q:\n%s", notWant, out)
				}
			}
		})
	}

	// Every segment empty → no line at all.
	if out := activeDetail(WorktreeView{}, "", 68); out != "" {
		t.Errorf("empty view should yield no detail line, got:\n%s", out)
	}

	// A long subject flexes: it is the truncated segment, the age survives.
	long := WorktreeView{HasBase: true, Ahead: 2, CommitTime: now,
		Subject: strings.Repeat("wide subject ", 20)}
	out := activeDetail(long, "", 48)
	if w := lipgloss.Width(out); w > 48 {
		t.Errorf("detail with long subject overflows: width %d", w)
	}
	if !strings.Contains(out, "…") || !strings.Contains(out, "2h") {
		t.Errorf("long subject should truncate with … and keep the age:\n%s", out)
	}
}

// TestActiveDisplayDetailLine checks the structural rule: the └ line appears
// only beneath the focused cursor row, and its rowItem entry is -1 so the
// click hit-test and windowing treat it like a repo header.
func TestActiveDisplayDetailLine(t *testing.T) {
	m := sampleModel()
	innerW := m.width - 4
	// Give the cursor row (feat-login, active index 0) some work-state so the
	// detail line has content.
	m.active[0].view.HasBase = true
	m.active[0].view.Ahead = 2
	m.active[0].view.Subject = "Add login flow"

	// Focus elsewhere: no detail line anywhere.
	m.focus = focusNew
	_, rowItem := m.activeDisplay(innerW)
	base := len(rowItem)
	// rowItem = [-1 caretaker, 0 feat-login, 1 bugfix, -1 api, 2 spike].
	if base != 5 {
		t.Fatalf("unfocused display should have 5 lines, got %d", base)
	}

	// Focused on feat-login: exactly one extra line, directly beneath the
	// cursor row, mapped to -1.
	m.focus = focusActive
	m.activeCursor = 0
	display, rowItem := m.activeDisplay(innerW)
	if len(rowItem) != base+1 {
		t.Fatalf("focused display should add one detail line: got %d, want %d", len(rowItem), base+1)
	}
	if rowItem[1] != 0 || rowItem[2] != -1 {
		t.Fatalf("detail line should follow the cursor row with rowItem -1, got %v", rowItem)
	}
	if !strings.Contains(display[2], "└") || !strings.Contains(display[2], `"Add login flow"`) || strings.Contains(display[2], "ahead") {
		t.Errorf("detail line content wrong:\n%s", display[2])
	}
	// No second detail line anywhere else.
	for i, ln := range display {
		if i != 2 && strings.Contains(ln, "└") {
			t.Errorf("unexpected extra detail line at display index %d:\n%s", i, ln)
		}
	}

	// Moving the cursor to a worktree with no state at all drops the line.
	m.activeCursor = 1 // bugfix: zero work-state
	_, rowItem = m.activeDisplay(innerW)
	if len(rowItem) != base {
		t.Errorf("cursor on a stateless worktree should add no detail line, got %d lines", len(rowItem))
	}
}

func TestRenderHelpKeys(t *testing.T) {
	// The cycle fwd/back and goto rows show; the removed alias rows never do.
	m := sampleModel()
	out := m.renderHelp(m.height - barHeight)
	for _, want := range []string{"alt+]", "alt+[", "alt+1", "alt+h", "alt+v", "cycle view"} {
		if !strings.Contains(out, want) {
			t.Errorf("help overlay missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "agent board (alias)") {
		t.Error("removed notif alias row should never appear")
	}
	if strings.Contains(out, "cycle pane focus") {
		t.Error("removed pane-cycle row should never appear")
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
	if strings.Contains(bar, "!") || strings.Contains(bar, "✓") {
		t.Errorf("bar should not show notif glyphs when nothing is unread:\n%s", bar)
	}

	// A live-waiting agent elsewhere shows "!", a stored unread marker shows "✓".
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
	if !strings.Contains(bar, "✓") {
		t.Errorf("bar should show ✓ for unread output:\n%s", bar)
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
	for _, want := range []string{"NEW AGENT", "What should Claude do?", "current: w", "home", "interactive", "background", "launch"} {
		if !strings.Contains(out, want) {
			t.Errorf("form missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "prompt") {
		t.Errorf("form should not render a redundant prompt label or placeholder:\n%s", out)
	}
}

func TestNewAgentFormIsWideWritingSurface(t *testing.T) {
	m := modelWithAgents(1)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = mm.(Model).openNewAgentForm().(Model)

	// Width reports the writable content after the border and horizontal
	// padding; SetWidth(80) leaves 76 cells for prompt text.
	if got, want := m.promptInput.Width(), 76; got != want {
		t.Fatalf("prompt content width = %d, want %d", got, want)
	}
	out := m.renderBoard(m.height - barHeight)
	if !strings.Contains(ansi.Strip(out), "╭"+strings.Repeat("─", 78)+"╮") {
		t.Fatalf("new-agent prompt should render as a bordered 80-column writing surface:\n%s", out)
	}
}

func TestBoardUsesFullPanelWidth(t *testing.T) {
	m := modelWithAgents(2)
	m.width, m.height = 120, 30
	mm, _ := m.openBoard()
	m = mm.(Model)

	out := m.renderBoard(m.height - barHeight)
	lines := strings.Split(out, "\n")
	frameWidth := 0
	for _, line := range lines {
		frameWidth = max(frameWidth, lipgloss.Width(strings.TrimLeft(ansi.Strip(line), " ")))
	}
	// 72 content cells plus two horizontal padding cells and two borders,
	// matching the help overlay's maximum width.
	if want := 76; frameWidth != want {
		t.Fatalf("rendered board frame width = %d, want %d", frameWidth, want)
	}

	rows, nav := m.buildBoard()
	if len(nav) == 0 || !rows[nav[0]].isAgent {
		t.Fatal("test board has no selectable agent row")
	}
	if got, want := lipgloss.Width(m.boardAgentLine(rows[nav[0]], 72)), 72; got != want {
		t.Fatalf("agent row width = %d, want full panel width %d", got, want)
	}
}

func TestProviderRowOnlyAppearsForMixedConfig(t *testing.T) {
	claudeOnly := modelWithAgents(1).openNewAgentForm().(Model)
	out := claudeOnly.renderBoard(claudeOnly.height - barHeight)
	if strings.Contains(out, "provider") || strings.Contains(out, "codex") {
		t.Errorf("single-provider form should hide provider choice:\n%s", out)
	}

	mixed := mixedProviderModel(agent.Codex)
	mixed.width, mixed.height = 72, 24
	mixed = mixed.openNewAgentForm().(Model)
	out = mixed.renderBoard(mixed.height - barHeight)
	for _, want := range []string{"provider", "claude", "codex"} {
		if !strings.Contains(out, want) {
			t.Errorf("mixed-provider form missing %q:\n%s", want, out)
		}
	}
}

func TestMixedProviderBoardChips(t *testing.T) {
	m := mixedProviderModel(agent.Claude)
	m.width, m.height = 72, 24
	m.current = &workspaceRef{repo: "r", worktree: "w", key: "r/w", path: "/r/w", ws: &session.Workspace{
		Agents: []*session.Session{
			{Provider: agent.Claude, Title: "amber-fox"},
			{Provider: agent.Codex, Title: "jade-otter"},
		},
	}}
	m.boardOpen = true
	out := m.renderBoard(m.height - barHeight)
	for _, want := range []string{"claude", "amber-fox", "codex", "jade-otter", "restart"} {
		if !strings.Contains(out, want) {
			t.Errorf("mixed board missing %q:\n%s", want, out)
		}
	}
}

func TestRenderCommandPalette(t *testing.T) {
	m := modelWithAgents(1) // an active workspace, so view-nav rows show
	mm, _ := m.openPalette()
	m = mm.(Model)
	out := m.renderPalette(m.height - barHeight)
	// The long list windows to the visible height, so assert on rows near the top
	// plus the title, input placeholder, and footer legend.
	for _, want := range []string{"COMMANDS", "go to editor", "back to deck", "open agent board", "run", "close"} {
		if !strings.Contains(out, want) {
			t.Errorf("palette render missing %q:\n%s", want, out)
		}
	}

	// Each row shows its live keybinding right-aligned; the "go to editor" row
	// carries the goto-editor key.
	if !strings.Contains(out, m.keys.GotoEditor) {
		t.Errorf("palette should show the goto-editor row's live key %q:\n%s", m.keys.GotoEditor, out)
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
