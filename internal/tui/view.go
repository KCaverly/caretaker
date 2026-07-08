package tui

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/KCaverly/caretaker/internal/session"
)

// Palette (gruvbox dark, medium contrast).
var (
	cAccent = lipgloss.Color("#83A598") // bright blue
	cPurple = lipgloss.Color("#D3869B") // bright purple
	cGreen  = lipgloss.Color("#B8BB26") // bright green
	cYellow = lipgloss.Color("#FABD2F") // bright yellow
	cRed    = lipgloss.Color("#FB4934") // bright red
	cFg     = lipgloss.Color("#EBDBB2") // fg1
	cDim    = lipgloss.Color("#928374") // gray
	cFaint  = lipgloss.Color("#665C54") // bg3
	cSelBg  = lipgloss.Color("#504945") // bg2 (selection)
	cInk    = lipgloss.Color("#1D2021") // bg0_h (hard)
)

var (
	barSep       = lipgloss.NewStyle().Foreground(cFaint)
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(cPurple)
	countStyle   = lipgloss.NewStyle().Foreground(cDim)
	repoHdrStyle = lipgloss.NewStyle().Bold(true).Foreground(cFg)
	repoStyle    = lipgloss.NewStyle().Foreground(cFg)
	nameStyle    = lipgloss.NewStyle().Foreground(cFg)
	dimStyle     = lipgloss.NewStyle().Foreground(cDim)
	liveStyle    = lipgloss.NewStyle().Foreground(cGreen)
	dirtyStyle   = lipgloss.NewStyle().Foreground(cYellow)
	recentStyle  = lipgloss.NewStyle().Foreground(cYellow)
	selStyle     = lipgloss.NewStyle().Bold(true).Foreground(cFg).Background(cSelBg)
	helpKeyStyle = lipgloss.NewStyle().Foreground(cAccent)
	helpStyle    = lipgloss.NewStyle().Foreground(cDim)
	errStyle     = lipgloss.NewStyle().Foreground(cRed)
)

// View implements tea.Model.
func (m Model) View() tea.View {
	w, h := m.width, m.height
	if w < 24 || h < 12 {
		v := tea.NewView("ct — please enlarge the terminal")
		v.AltScreen = true
		return v
	}

	chrome := m.renderBar()
	var body string
	var cursor *tea.Cursor
	switch {
	case m.screen == screenSetup:
		body = m.renderSetup(h - barHeight)
	case m.helpOpen:
		body = m.renderHelp(h - barHeight)
	case m.boardOpen:
		body = m.renderBoard(h - barHeight)
	case m.screen == screenPicker:
		body = m.renderDeck(h - barHeight)
	case m.screen == screenTerminal && m.current != nil && m.current.ws != nil:
		body, cursor = m.renderTermPanes(w, h-barHeight)
	default:
		if s := m.activeSession(); s != nil {
			body = s.Render()
			if x, y, visible := s.Cursor(); visible {
				cursor = tea.NewCursor(x, y+barHeight)
			}
		}
	}

	v := tea.NewView(chrome + "\n" + body)
	v.AltScreen = true
	v.Cursor = cursor
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// Tab glyphs (Nerd Font). Kept as named consts so they're easy to swap.
const (
	iconDeck   = "\U0000F4D8" // fa-seedling (U+F4D8) — the deck: a grove of worktrees
	iconEditor = ""          // fa-code (U+F121)     — nvim
	iconAgent  = "󰚩"          // md-robot (U+F06A9)   — claude
	iconTerm   = ""          // fa-terminal (U+F120) — term
)

// renderBar draws the pinned status bar plus a light separator directly
// beneath it (barHeight rows total). The four left icons (caretaker, nvim,
// claude, term) are bold Nerd Font glyphs evenly spaced: the caretaker is a
// stable seedling, lit yellow while the deck is active and dim once you drop
// into a session; the session icons glow in their own colour when active and
// dim otherwise (faint until a workspace exists). Agent attention lives in the
// "! N" badge, not the icons. The current repo / worktree sits on the right.
func (m Model) renderBar() string {
	left := "  "
	for i, z := range m.barZones() {
		if i > 0 {
			left += "   " // equidistant gap between icons
		}
		left += z.glyph
	}

	// Right side: notification zone (! N  * N) then the workspace context.
	right := ""
	if notif := m.renderNotifZone(); notif != "" {
		right += notif + "   "
	}
	right += m.barContextLabel()
	if right != "" {
		right += "  "
	}

	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	bar := left + strings.Repeat(" ", gap) + right
	sep := barSep.Render(strings.Repeat("─", max(1, m.width)))
	return bar + "\n" + sep
}

// renderNotifZone builds the right-side attention summary: "! N" (red) for
// worktrees where an agent is waiting on input, "* N" (green) for worktrees
// with unread completions, "@ N" (blue) for unread background-agent messages.
// Returns "" when nothing is pending. Clicking it opens the agent board.
func (m Model) renderNotifZone() string {
	waiting, done, msgs := m.attnSummary()
	var parts []string
	if waiting > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(cRed).Bold(true).Render("!")+
			" "+countStyle.Render(strconv.Itoa(waiting)))
	}
	if done > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(cGreen).Bold(true).Render("*")+
			" "+countStyle.Render(strconv.Itoa(done)))
	}
	if msgs > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(cAccent).Bold(true).Render("@")+
			" "+countStyle.Render(strconv.Itoa(msgs)))
	}
	return strings.Join(parts, "  ")
}

// barContextLabel builds the bar's right-side workspace context: the
// "repo / worktree" label, extended with the agent pool position
// ("· 2/3 label") whenever the workspace has more than one agent, and the
// pane position ("· ⊞ 2/3", plus "zoom" while zoomed) on the terminal screen
// with splits. It surfaces state that is otherwise invisible — which agent
// is focused, how many exist, whether a pane is zoomed — and thereby
// advertises the prev/next-agent and zoom keys.
func (m Model) barContextLabel() string {
	if m.current == nil {
		return ""
	}
	s := lipgloss.NewStyle().Foreground(cDim).
		Render(m.current.repo + " / " + m.current.worktree)
	ws := m.current.ws
	if ws == nil {
		return s
	}
	sep := dimStyle.Render(" · ")
	if n := len(ws.Agents); n > 1 {
		pos := fmt.Sprintf("%d/%d", clamp(ws.ActiveAgent, 0, n-1)+1, n)
		if a := ws.ActiveAgentSession(); a != nil {
			pos += " " + truncateTo(agentTitle(a.Title), 14)
		}
		st := dimStyle
		if m.screen == screenAgent {
			st = lipgloss.NewStyle().Foreground(cPurple)
		}
		s += sep + st.Render(pos)
	}
	if m.screen == screenTerminal && len(ws.Terms) > 1 {
		pane := fmt.Sprintf("⊞ %d/%d", clamp(ws.ActiveTerm, 0, len(ws.Terms)-1)+1, len(ws.Terms))
		if ws.TermZoomed {
			pane += " zoom"
		}
		s += sep + lipgloss.NewStyle().Foreground(cAccent).Render(pane)
	}
	return s
}

// barZone is one clickable status-bar icon: its fully-rendered glyph (with any
// count/notification badge) and the screen it selects.
type barZone struct {
	s     screen
	glyph string
}

// barZones builds the ordered left-hand icons shared by renderBar (for drawing)
// and tabAt (for hit-testing), so their layouts can never drift apart.
func (m Model) barZones() []barZone {
	has := m.current != nil

	// All glyphs are bold (heaviest weight a cell allows); active gets its accent
	// colour, idle is dim, and disabled is faint until a workspace exists.
	glyph := func(g string, accent color.Color, active, enabled bool) string {
		st := lipgloss.NewStyle().Bold(true)
		switch {
		case active:
			return st.Foreground(accent).Render(g)
		case enabled:
			return st.Foreground(cDim).Render(g)
		default:
			return st.Foreground(cFaint).Render(g)
		}
	}

	// Caretaker: a stable seedling that follows the same lit-when-active rule as
	// the other tabs — yellow while the deck is active, dim otherwise (never
	// faint: the deck is always reachable). Agent attention lives in the ! badge
	// on the right, so the icon never reacts to agent status.
	ctColor := cDim
	if m.screen == screenPicker {
		ctColor = cYellow
	}
	ct := lipgloss.NewStyle().Bold(true).Foreground(ctColor).Render(iconDeck)

	agent := glyph(iconAgent, cPurple, m.screen == screenAgent, has)

	return []barZone{
		{screenPicker, ct},
		{screenEditor, glyph(iconEditor, cGreen, m.screen == screenEditor, has)},
		{screenAgent, agent},
		{screenTerminal, glyph(iconTerm, cAccent, m.screen == screenTerminal, has)},
	}
}

// tabAt maps bar coordinates to the tab/screen under them, if a click landed on
// one of the left icons. It walks the same barZones renderBar draws: a 2-column
// lead-in, each (possibly badged) icon, and a 3-column gap between them; each
// icon's hit target includes one column of slack on each side. Only the bar row
// (y == 0) counts.
func (m Model) tabAt(x, y int) (screen, bool) {
	if y != 0 {
		return 0, false
	}
	col := 2 // leading "  " in renderBar
	for _, z := range m.barZones() {
		w := lipgloss.Width(z.glyph)
		if x >= col-1 && x < col+w+1 {
			return z.s, true
		}
		col += w + 3 // glyph + the 3-space gap
	}
	return 0, false
}

// notifZoneAt reports whether bar coordinates (x, y) land on the notification
// zone. It mirrors renderBar's right-side layout to locate the zone's x bounds.
func (m Model) notifZoneAt(x, y int) bool {
	if y != 0 {
		return false
	}
	notif := m.renderNotifZone()
	if notif == "" {
		return false
	}
	right := notif + "   " + m.barContextLabel() + "  "
	start := m.width - lipgloss.Width(right)
	end := start + lipgloss.Width(notif)
	return x >= start && x < end
}

// renderBoard draws the agent board overlay: every open workspace's agents
// grouped under worktree header rows, attention sorted to the top, plus the
// trailing "+ new agent" row. Delegates to renderBoardForm in form state.
func (m Model) renderBoard(h int) string {
	innerW := clamp(m.width-8, 32, 64)
	if m.formOpen {
		return m.renderBoardForm(h, innerW)
	}

	rows, nav := m.buildBoard()
	selRow := -1
	if m.boardCursor >= 0 && m.boardCursor < len(nav) {
		selRow = nav[m.boardCursor]
	}
	agentCount := 0
	for _, r := range rows {
		if r.isAgent {
			agentCount++
		}
	}

	lines := []string{header("agents", agentCount), ""}
	for i, r := range rows {
		switch {
		case r.isNew:
			if agentCount > 0 {
				lines = append(lines, "")
			}
			if i == selRow {
				lines = append(lines, selBar("  + new agent…", innerW))
			} else {
				lines = append(lines, dimStyle.Render("  + new agent…"))
			}
		case r.isAgent:
			content := m.boardAgentLine(r, innerW)
			if i == selRow {
				lines = append(lines, selBar(content, innerW))
			} else {
				lines = append(lines, content)
			}
		default: // worktree group header
			if i > 0 {
				lines = append(lines, "")
			}
			left := dimStyle.Render("  " + r.key)
			if m.current != nil && r.key == m.current.key {
				right := helpKeyStyle.Render("current")
				gap := max(2, innerW-lipgloss.Width(left)-lipgloss.Width(right))
				left += strings.Repeat(" ", gap) + right
			}
			lines = append(lines, left)
		}
	}

	lines = append(lines, "", "  "+strings.Join([]string{
		keyhint("↑↓", "move"), keyhint("1-9", "jump"), keyhint("enter", "focus"),
		keyhint("n", "new"), keyhint("d", "close"), keyhint("esc", "close"),
	}, helpStyle.Render("  ·  ")))

	boxStr := box(lines, innerW, len(lines), true)
	return centerBlock(boxStr, m.width, h)
}

// boardAgentLine renders one agent row: quick-jump number, attention glyph,
// label, and the right-aligned (truncated) status/preview column.
func (m Model) boardAgentLine(r boardRow, innerW int) string {
	numCol := " "
	if r.num > 0 {
		numCol = strconv.Itoa(r.num)
	}
	glyph, glyphSt := " ", dimStyle
	switch r.attn {
	case attnWaiting:
		glyph, glyphSt = "!", lipgloss.NewStyle().Foreground(cRed).Bold(true)
	case attnMessage:
		glyph, glyphSt = "@", lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	case attnDone:
		glyph, glyphSt = "*", lipgloss.NewStyle().Foreground(cGreen).Bold(true)
	}
	left := "   " + dimStyle.Render(numCol) + " " + glyphSt.Render(glyph) + " " + nameStyle.Render(r.label)
	status := truncateTo(r.status, max(0, innerW-lipgloss.Width(left)-2))
	right := dimStyle.Render(status)
	gap := max(2, innerW-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

// renderBoardForm draws the new-agent form: label and prompt inputs plus the
// where/mode toggles, with the focused field's name highlighted.
func (m Model) renderBoardForm(h, innerW int) string {
	fieldName := func(f int, name string) string {
		st := dimStyle
		if m.formFocus == f {
			st = helpKeyStyle
		}
		return st.Render(padLine(name, 8))
	}
	toggle := func(options [2]string, sel int) string {
		var parts [2]string
		for i, o := range options {
			if i == sel {
				parts[i] = lipgloss.NewStyle().Bold(true).Foreground(cFg).Render("[" + o + "]")
			} else {
				parts[i] = dimStyle.Render(" " + o + " ")
			}
		}
		return parts[0] + " " + parts[1]
	}
	bgIdx := 0
	if m.formBackground {
		bgIdx = 1
	}
	rows := []string{
		header("new agent", -1),
		"",
		"  " + fieldName(formFieldLabel, "label") + m.agentName.View(),
		"  " + fieldName(formFieldPrompt, "prompt") + m.promptInput.View(),
		"",
		"  " + fieldName(formFieldWhere, "where") + toggle([2]string{"active worktree", "home worktree"}, m.formLocation),
		"  " + fieldName(formFieldMode, "mode") + toggle([2]string{"foreground", "background"}, bgIdx),
		"",
		"  " + strings.Join([]string{
			keyhint("enter", "launch"), keyhint("tab", "field"),
			keyhint("space", "toggle"), keyhint("esc", "back"),
		}, helpStyle.Render("  ·  ")),
	}
	boxStr := box(rows, innerW, len(rows), true)
	return centerBlock(boxStr, m.width, h)
}

// truncateTo shortens s to at most w display columns, appending "…" when it
// had to cut.
func truncateTo(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	if w <= 1 {
		return ""
	}
	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes))+1 > w {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

// deckLayout captures the deck's vertical geometry, shared by renderDeck (to
// draw) and deckClick (to hit-test) so the two can never drift apart. bodyH is
// the row count beneath the bar (m.height - barHeight).
type deckLayout struct {
	newOuterH      int // rows in the NEW box, border included
	newContentH    int // inner rows of the NEW box
	newRows        int // repo-list rows inside the NEW box
	activeContentH int // inner rows of the ACTIVE box
	activeRows     int // worktree rows inside the ACTIVE box
}

func (m Model) deckLayout(bodyH int) deckLayout {
	bodyH -= lipgloss.Height(m.renderFooter())

	// Size the NEW box to its content (header, blank, input, blank, then repos),
	// capped at half the body so ACTIVE always keeps room.
	var newContent int
	if m.mode == modeCreateName {
		newContent = 7 // header, blank, label, blank, input, blank, hint
	} else {
		newContent = 4 + min(max(len(m.repoMatches), 1), 6)
	}
	newOuterH := clamp(newContent+2, 7, max(7, bodyH/2))
	activeOuterH := bodyH - newOuterH
	return deckLayout{
		newOuterH:      newOuterH,
		newContentH:    newOuterH - 2,
		newRows:        max(0, (newOuterH-2)-4), // header + blank + input + blank
		activeContentH: activeOuterH - 2,
		activeRows:     max(0, (activeOuterH-2)-2), // header + blank
	}
}

// renderDeck draws the picker (NEW + ACTIVE sections) into h rows beneath the bar.
func (m Model) renderDeck(h int) string {
	innerW := m.width - 4 // border (2) + horizontal padding (2)
	footer := m.renderFooter()
	L := m.deckLayout(h)

	newBox := box(m.renderNew(innerW, L.newRows), innerW, L.newContentH, m.focus == focusNew)
	activeBox := box(m.renderActive(innerW, L.activeRows), innerW, L.activeContentH, m.focus == focusActive)

	return lipgloss.JoinVertical(lipgloss.Left, newBox, activeBox, footer)
}

// renderNew builds the top "new" repo finder. In create mode it becomes a
// roomy form for naming the new worktree, co-located with the repo header.
func (m Model) renderNew(innerW, rows int) []string {
	if m.mode == modeCreateName {
		return m.renderCreateForm()
	}

	// header, blank, input, blank, then the repo list.
	lines := []string{header("new", -1), "", m.filter.View(), ""}

	if len(m.repoMatches) == 0 {
		// Before the first scan lands, an empty list means "still looking",
		// not "nothing there" — don't claim the root is empty.
		if !m.groupsLoaded {
			return append(lines, dimStyle.Render("   scanning repos…"))
		}
		return append(lines, dimStyle.Render("   no repos under root"))
	}

	start, end := windowBounds(len(m.repoMatches), m.newCursor, rows)
	for i := start; i < end; i++ {
		name := m.repoMatches[i].Name
		if i == m.newCursor && m.focus == focusNew {
			lines = append(lines, selBar("   "+name, innerW))
		} else {
			lines = append(lines, repoStyle.Render("   "+name))
		}
	}
	return lines
}

// renderCreateForm draws the new-worktree naming form inside the NEW box.
func (m Model) renderCreateForm() []string {
	label := dimStyle.Render("new worktree in ") + repoHdrStyle.Render(m.pendingRepo.Name)
	hint := keyhint("enter", "create") + helpStyle.Render("   ·   ") + keyhint("esc", "cancel")
	return []string{
		header("new", -1),
		"",
		"  " + label,
		"",
		"  " + m.nameInput.View(),
		"",
		"  " + hint,
	}
}

// activeDisplay builds the ACTIVE section's display lines (a repo header before
// each repo's first worktree, then one row per worktree) alongside a parallel
// slice mapping each display line back to its m.active index (-1 for header
// lines). Shared by renderActive and the click hit-test so their row layout
// stays identical.
func (m Model) activeDisplay(innerW int) (lines []string, rowItem []int) {
	lastRepo := ""
	for i, it := range m.active {
		if it.repo.Name != lastRepo {
			lines = append(lines, repoHdrStyle.Render(it.repo.Name))
			rowItem = append(rowItem, -1)
			lastRepo = it.repo.Name
		}
		lines = append(lines, m.activeRow(it, i == m.activeCursor && m.focus == focusActive, innerW))
		rowItem = append(rowItem, i)
	}
	return
}

// activeWindowStart returns the first display index shown for a window of `rows`
// rows, keeping the cursor's worktree visible.
func activeWindowStart(rowItem []int, cursor, rows int) (start, end int) {
	cursorAt := 0
	for di, it := range rowItem {
		if it == cursor {
			cursorAt = di
		}
	}
	return windowBounds(len(rowItem), cursorAt, rows)
}

// renderActive builds the bottom navigator: worktrees grouped under their repo.
func (m Model) renderActive(innerW, rows int) []string {
	lines := []string{header("active", len(m.active)), ""}

	if len(m.active) == 0 {
		if !m.groupsLoaded {
			return append(lines, dimStyle.Render("scanning…"))
		}
		return append(lines, dimStyle.Render("no workspaces yet — pick a repo above to create one"))
	}

	display, rowItem := m.activeDisplay(innerW)
	start, end := activeWindowStart(rowItem, m.activeCursor, rows)
	return append(lines, display[start:end]...)
}

func (m Model) activeRow(it activeItem, highlight bool, innerW int) string {
	key := wsKey(it.repo.Name, it.view.WT.Name)

	// Live/dead indicator: filled circle when sessions are running, hollow otherwise.
	liveChar := "○"
	liveSt := dimStyle
	if it.view.Live {
		liveChar = "●"
		liveSt = liveStyle
	}

	// Attention indicator: matches the right-bar glyphs so the user can
	// scan the list for the same symbol they saw in the bar.
	notifChar := " "
	notifSt := dimStyle
	switch m.worktreeAttn(key) {
	case attnWaiting:
		notifChar = "!"
		notifSt = lipgloss.NewStyle().Foreground(cRed).Bold(true)
	case attnMessage:
		notifChar = "@"
		notifSt = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	case attnDone:
		notifChar = "*"
		notifSt = lipgloss.NewStyle().Foreground(cGreen).Bold(true)
	}

	dirtyChar := " "
	if it.view.Dirty {
		dirtyChar = "✷"
	}

	// Leading rank column (1..3) for the worktrees most recently opened in ct,
	// blank otherwise. A fixed-width gutter keeps selected/unselected rows aligned.
	rank := m.recentRank[key]
	rankCh := " "
	if rank > 0 {
		rankCh = strconv.Itoa(rank)
	}

	if highlight {
		return selBar(fmt.Sprintf("  %s   %s %s %s %s", rankCh, liveChar, notifChar, dirtyChar, it.view.WT.Name), innerW)
	}

	rankCol := " "
	if rank > 0 {
		rankCol = recentStyle.Render(rankCh)
	}
	dirty := " "
	if it.view.Dirty {
		dirty = dirtyStyle.Render(dirtyChar)
	}
	return "  " + rankCol + "   " + liveSt.Render(liveChar) + " " + notifSt.Render(notifChar) + " " + dirty + " " + nameStyle.Render(it.view.WT.Name)
}

// renderHelp draws the key + legend overlay, centered in the body area. The
// session bindings are read from the model so the overlay can never drift from
// the real (configurable) keys.
func (m Model) renderHelp(h int) string {
	innerW := clamp(m.width-8, 28, 72)

	row := func(key, desc string) string {
		return "  " + helpKeyStyle.Render(padLine(key, 12)) + helpStyle.Render(desc)
	}

	rows := []string{header("help", -1), ""}
	rows = append(rows,
		repoHdrStyle.Render("  Deck"),
		row("↑↓ / j k", "move"),
		row("tab", "switch section"),
		row("enter", "open / create"),
		row("1 2 3", "open recent worktree"),
		row("d", "stop worktree"),
		row("x", "remove worktree"),
		row("r", "refresh"),
		row("ctrl+c", "quit"),
		"",
		repoHdrStyle.Render("  Session"),
		row(m.keyCycle, "cycle view (nvim → claude → term)"),
		row(m.keyPicker, "back to the deck"),
		row(m.keyGlobalConfig, "open home workspace (~)"),
		row(m.keyPrompt, "quick background agent (home)"),
		row(m.keyPalette, "agent board"),
		row(m.keyNotif, "agent board (alias)"),
		row(m.keyPrevAgent+" / "+m.keyNextAgent, "prev / next agent"),
		"",
		repoHdrStyle.Render("  Terminal panes"),
		row(m.keyTermSplitV, "vertical split"),
		row(m.keyTermSplitH, "horizontal split"),
		row(m.keyTermCycle, "cycle pane focus"),
		row(m.keyTermZoom, "zoom / restore pane"),
		row(m.keyTermClose, "close pane"),
		"",
		repoHdrStyle.Render("  Legend"),
		"  "+statusLegend(),
		"  "+markLegend(),
		"",
		"  "+helpStyle.Render("toggle with ")+helpKeyStyle.Render(m.keyHelp)+
			helpStyle.Render(" (or ")+helpKeyStyle.Render("?")+
			helpStyle.Render(" in the deck) · any key closes"),
	)

	boxStr := box(rows, innerW, len(rows), true)
	return centerBlock(boxStr, m.width, h)
}

// statusLegend / markLegend explain the deck's status glyphs, split across two
// lines so they stay within the overlay's width.
func statusLegend() string {
	return strings.Join([]string{
		liveStyle.Render("●") + helpStyle.Render(" live"),
		dimStyle.Render("○") + helpStyle.Render(" stopped"),
		lipgloss.NewStyle().Foreground(cRed).Render("!") + helpStyle.Render(" waiting"),
		lipgloss.NewStyle().Foreground(cGreen).Render("*") + helpStyle.Render(" done"),
		lipgloss.NewStyle().Foreground(cAccent).Render("@") + helpStyle.Render(" message"),
	}, helpStyle.Render("   "))
}

func markLegend() string {
	return strings.Join([]string{
		dirtyStyle.Render("✷") + helpStyle.Render(" uncommitted"),
		recentStyle.Render("1 2 3") + helpStyle.Render(" recently opened"),
	}, helpStyle.Render("   "))
}

// renderSetup draws the first-run setup overlay centered in the body area.
func (m Model) renderSetup(h int) string {
	innerW := clamp(m.width-8, 32, 60)

	rows := []string{
		header("setup", -1),
		"",
		dimStyle.Render("  no config found — let's get started"),
		"",
		dimStyle.Render("  config will be saved to:"),
		"  " + helpKeyStyle.Render(m.configPath),
		"",
		dimStyle.Render("  directory containing your git repos"),
		"",
		"  " + m.rootInput.View(),
		"",
	}
	if m.status != "" {
		rows = append(rows, "  "+errStyle.Render(m.status), "")
	}
	rows = append(rows, "  "+keyhint("enter", "confirm")+"   "+keyhint("esc", "quit"))

	boxStr := box(rows, innerW, len(rows), true)
	return centerBlock(boxStr, m.width, h)
}

// centerBlock centers a rendered block within w×h by padding above and to the
// left (lines wider/taller than the area are left as-is).
func centerBlock(block string, w, h int) string {
	lines := strings.Split(block, "\n")
	bw := 0
	for _, ln := range lines {
		if lw := lipgloss.Width(ln); lw > bw {
			bw = lw
		}
	}
	prefix := strings.Repeat(" ", max(0, (w-bw)/2))

	var out []string
	for i := 0; i < max(0, (h-len(lines))/2); i++ {
		out = append(out, "")
	}
	for _, ln := range lines {
		out = append(out, prefix+ln)
	}
	return strings.Join(out, "\n")
}

func (m Model) renderFooter() string {
	return m.centerFooter(m.footerContent())
}

// footerContent builds the two-row footer (status line + help line) before
// centering.
func (m Model) footerContent() string {
	switch m.mode {
	case modeCreateName:
		return "\n" + helpStyle.Render(m.status)
	case modeConfirmRemove:
		return "\n" + errStyle.Render(m.status)
	}

	var hints []string
	if m.focus == focusNew {
		hints = []string{
			keyhint("type", "filter"), keyhint("↑↓", "select"),
			keyhint("enter", "create"), keyhint("tab", "active"),
			keyhint("?", "help"), keyhint("ctrl+c", "quit"),
		}
	} else {
		hints = []string{
			keyhint("↑↓", "move"), keyhint("enter", "open"),
			keyhint("1-3", "recent"), keyhint("d", "stop"), keyhint("x", "remove"),
			keyhint("tab", "new"), keyhint("?", "help"), keyhint("ctrl+c", "quit"),
		}
	}
	help := strings.Join(hints, helpStyle.Render("  ·  "))

	if m.status != "" {
		style := helpStyle
		if m.statusLevel == statusError {
			style = errStyle
		}
		return style.Render(m.status) + "\n" + help
	}
	return "\n" + help
}

// centerFooter horizontally centers each footer row within the deck width by
// left-padding. Lines wider than the deck are left as-is (no wrapping), so the
// footer keeps its row count.
func (m Model) centerFooter(content string) string {
	if m.width <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	for i, ln := range lines {
		if pad := (m.width - lipgloss.Width(ln)) / 2; pad > 0 {
			lines[i] = strings.Repeat(" ", pad) + ln
		}
	}
	return strings.Join(lines, "\n")
}

// --- terminal pane rendering ---

// renderTermPanes renders the terminal screen: either a single full-size pane,
// a zoomed pane, or a split layout assembled from the pane tree.
func (m Model) renderTermPanes(w, h int) (string, *tea.Cursor) {
	ws := m.current.ws
	if len(ws.Terms) == 0 {
		return "", nil
	}
	if ws.TermZoomed || len(ws.Terms) == 1 || ws.TermLayout == nil {
		s := ws.Terms[ws.ActiveTerm]
		body := s.Render()
		var cursor *tea.Cursor
		if x, y, visible := s.Cursor(); visible {
			cursor = tea.NewCursor(x, y+barHeight)
		}
		return body, cursor
	}
	return m.renderPaneNode(ws.TermLayout, 0, 0, w, h, ws)
}

// renderPaneNode recursively renders a split subtree into (x, y, w, h) of the
// body, inserting styled dividers between panes.
func (m Model) renderPaneNode(node *session.PaneNode, x, y, w, h int, ws *session.Workspace) (string, *tea.Cursor) {
	if node == nil || w < 1 || h < 1 {
		return strings.Repeat(" ", w) + strings.Repeat("\n"+strings.Repeat(" ", w), h-1), nil
	}
	if node.Dir == session.SplitNone {
		if node.Idx >= len(ws.Terms) {
			return "", nil
		}
		s := ws.Terms[node.Idx]
		body := s.Render()
		var cursor *tea.Cursor
		if node.Idx == ws.ActiveTerm {
			if cx, cy, visible := s.Cursor(); visible {
				cursor = tea.NewCursor(x+cx, y+barHeight+cy)
			}
		}
		return body, cursor
	}

	if node.Dir == session.SplitV {
		if w < 3 {
			return m.renderPaneNode(node.A, x, y, w, h, ws)
		}
		aW := max(1, int(node.Ratio*float64(w-1)))
		bW := w - aW - 1
		if bW < 1 {
			bW, aW = 1, w-2
		}
		aBody, aCur := m.renderPaneNode(node.A, x, y, aW, h, ws)
		bBody, bCur := m.renderPaneNode(node.B, x+aW+1, y, bW, h, ws)
		divColor := m.paneAdjacentColor(node, ws.ActiveTerm)
		body := joinVerticalSplit(aBody, bBody, divColor, h, aW)
		if aCur != nil {
			return body, aCur
		}
		return body, bCur
	}

	// SplitH
	if h < 3 {
		return m.renderPaneNode(node.A, x, y, w, h, ws)
	}
	aH := max(1, int(node.Ratio*float64(h-1)))
	bH := h - aH - 1
	if bH < 1 {
		bH, aH = 1, h-2
	}
	aBody, aCur := m.renderPaneNode(node.A, x, y, w, aH, ws)
	bBody, bCur := m.renderPaneNode(node.B, x, y+aH+1, w, bH, ws)
	divColor := m.paneAdjacentColor(node, ws.ActiveTerm)
	body := joinHorizontalSplit(aBody, bBody, divColor, w, aH)
	if aCur != nil {
		return body, aCur
	}
	return body, bCur
}

// paneAdjacentColor returns cAccent if the active pane is a direct child of
// this split node (i.e., the divider directly borders the focused pane), or
// cFaint otherwise.
func (m Model) paneAdjacentColor(node *session.PaneNode, activeTerm int) color.Color {
	if (node.A.Dir == session.SplitNone && node.A.Idx == activeTerm) ||
		(node.B.Dir == session.SplitNone && node.B.Idx == activeTerm) {
		return cAccent
	}
	return cFaint
}

// joinVerticalSplit interleaves lines from left and right pane bodies with a
// single-column divider. leftWidth is the pane's column count — every left
// line is padded to that exact display width so the divider lands on a
// consistent column regardless of how much content the vt emulator rendered.
func joinVerticalSplit(left, right string, divColor color.Color, h, leftWidth int) string {
	leftLines := splitLines(left)
	rightLines := splitLines(right)
	div := lipgloss.NewStyle().Foreground(divColor).Render("│")
	rows := make([]string, h)
	for i := range rows {
		l, r := "", ""
		if i < len(leftLines) {
			l = leftLines[i]
		}
		if i < len(rightLines) {
			r = rightLines[i]
		}
		// Pad left line to leftWidth so the divider is always at the same column.
		if lw := lipgloss.Width(l); lw < leftWidth {
			l += strings.Repeat(" ", leftWidth-lw)
		}
		rows[i] = l + div + r
	}
	return strings.Join(rows, "\n")
}

// joinHorizontalSplit stacks top and bottom pane bodies with a single-row
// divider. topHeight is the expected row count for the top pane — the block
// is padded to that many rows so the divider always starts at the right row.
func joinHorizontalSplit(top, bottom string, divColor color.Color, w, topHeight int) string {
	topLines := splitLines(top)
	rows := make([]string, topHeight)
	for i := range rows {
		if i < len(topLines) {
			rows[i] = topLines[i]
		}
	}
	div := lipgloss.NewStyle().Foreground(divColor).Render(strings.Repeat("─", max(1, w)))
	return strings.Join(rows, "\n") + "\n" + div + "\n" + bottom
}

// splitLines splits a vt-emulator output string into lines, normalising \r\n
// and stripping any trailing \r so callers get clean line strings.
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, "\r")
	}
	return lines
}

// --- helpers ---

func header(label string, count int) string {
	s := headerStyle.Render(strings.ToUpper(label))
	if count >= 0 {
		s += "  " + countStyle.Render(strconv.Itoa(count))
	}
	return s
}

func keyhint(key, desc string) string {
	return helpKeyStyle.Render(key) + helpStyle.Render(" "+desc)
}

// selBar renders text as a solid full-width selection bar by padding the plain
// string to innerW before styling, so the background spans the whole row.
func selBar(text string, innerW int) string {
	return selStyle.Render(padLine(text, innerW))
}

// box draws content inside a rounded, padded frame of a fixed inner size. Lines
// are pre-padded to innerW (and to contentH rows) so the border never re-pads
// them — which would otherwise strip selection-bar backgrounds.
func box(lines []string, innerW, contentH int, focused bool) string {
	rows := make([]string, contentH)
	for i := range rows {
		if i < len(lines) {
			rows[i] = padLine(lines[i], innerW)
		} else {
			rows[i] = strings.Repeat(" ", innerW)
		}
	}
	color := cFaint
	if focused {
		color = cAccent
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(color).
		Padding(0, 1).
		Render(strings.Join(rows, "\n"))
}

// padLine right-pads s with plain spaces to w display columns.
func padLine(s string, w int) string {
	if diff := w - lipgloss.Width(s); diff > 0 {
		return s + strings.Repeat(" ", diff)
	}
	return s
}

// windowBounds returns [start,end) of a scrolling window of `height` rows that
// keeps `cursor` visible within a list of `n` items.
func windowBounds(n, cursor, height int) (int, int) {
	if height <= 0 || n == 0 {
		return 0, 0
	}
	if n <= height {
		return 0, n
	}
	start := cursor - height/2
	if start < 0 {
		start = 0
	}
	if start+height > n {
		start = n - height
	}
	return start, start + height
}
