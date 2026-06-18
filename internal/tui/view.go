package tui

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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
	if m.screen == screenPicker {
		body = m.renderDeck(h - barHeight)
	} else if s := m.activeSession(); s != nil {
		body = s.Render()
		if x, y, visible := s.Cursor(); visible {
			cursor = tea.NewCursor(x, y+barHeight)
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
	iconDeck   = "" // fa-smile (U+F118)    — tending the deck (picker)
	iconAway   = "󰚌" // md-skull (U+F068C)   — away in a session
	iconEditor = "" // fa-code (U+F121)     — nvim
	iconAgent  = "󰚩" // md-robot (U+F06A9)   — claude
	iconTerm   = "" // fa-terminal (U+F120) — term
)

// renderBar draws the pinned status bar plus a light separator directly
// beneath it (barHeight rows total). The four left icons (caretaker, nvim,
// claude, term) are bold Nerd Font glyphs evenly spaced: the caretaker shows a
// yellow smiley while you tend the deck and a red skull once you drop into a
// session; the session icons glow in their own colour when active and dim
// otherwise (faint until a workspace exists). The current repo / worktree sits
// on the right.
func (m Model) renderBar() string {
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

	// Caretaker: smiley (tending the deck) vs skull (away in a session).
	ct := lipgloss.NewStyle().Bold(true).Foreground(cYellow).Render(iconDeck)
	if m.screen != screenPicker {
		ct = lipgloss.NewStyle().Bold(true).Foreground(cRed).Render(iconAway)
	}

	// All four left icons share the same gap so they're equidistant.
	left := "  " + strings.Join([]string{
		ct,
		glyph(iconEditor, cGreen, m.screen == screenEditor, has),
		glyph(iconAgent, cPurple, m.screen == screenAgent, has),
		glyph(iconTerm, cAccent, m.screen == screenTerminal, has),
	}, "   ")

	right := ""
	if has {
		right = lipgloss.NewStyle().Bold(true).Foreground(cPurple).
			Render(m.current.repo+" / "+m.current.worktree) + "  "
	}

	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	bar := left + strings.Repeat(" ", gap) + right
	sep := barSep.Render(strings.Repeat("─", max(1, m.width)))
	return bar + "\n" + sep
}

// tabAt maps bar coordinates to the tab/screen under them, if a click landed on
// one of the four left icons. It mirrors renderBar's layout exactly: a 2-column
// lead-in, each icon, and a 3-column gap between them; each icon's hit target
// includes one column of slack on each side. Only the bar row (y == 0) counts.
func (m Model) tabAt(x, y int) (screen, bool) {
	if y != 0 {
		return 0, false
	}
	caretaker := iconDeck
	if m.screen != screenPicker {
		caretaker = iconAway
	}
	zones := []struct {
		glyph string
		s     screen
	}{
		{caretaker, screenPicker},
		{iconEditor, screenEditor},
		{iconAgent, screenAgent},
		{iconTerm, screenTerminal},
	}
	col := 2 // leading "  " in renderBar
	for _, z := range zones {
		w := lipgloss.Width(z.glyph)
		if x >= col-1 && x < col+w+1 {
			return z.s, true
		}
		col += w + 3 // glyph + the 3-space Join separator
	}
	return 0, false
}

// renderDeck draws the picker (NEW + ACTIVE sections) into h rows beneath the bar.
func (m Model) renderDeck(h int) string {
	w := m.width
	footer := m.renderFooter()
	bodyH := h - lipgloss.Height(footer)

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
	innerW := w - 4 // border (2) + horizontal padding (2)

	newContentH := newOuterH - 2
	activeContentH := activeOuterH - 2

	newRows := max(0, newContentH-4)       // header + blank + input + blank
	activeRows := max(0, activeContentH-2) // header + blank

	newBox := box(m.renderNew(innerW, newRows), innerW, newContentH, m.focus == focusNew)
	activeBox := box(m.renderActive(innerW, activeRows), innerW, activeContentH, m.focus == focusActive)

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
		return append(lines, dimStyle.Render("   no repos under root"))
	}

	start, end := windowBounds(len(m.repoMatches), m.newCursor, rows)
	for i := start; i < end; i++ {
		name := m.repoMatches[i].Name
		if i == m.newCursor && m.focus == focusNew {
			lines = append(lines, selBar(" ▸ "+name, innerW))
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

// renderActive builds the bottom navigator: worktrees grouped under their repo.
func (m Model) renderActive(innerW, rows int) []string {
	lines := []string{header("active", len(m.active)), ""}

	if len(m.active) == 0 {
		return append(lines, dimStyle.Render("no workspaces yet — pick a repo above to create one"))
	}

	// Build display lines (repo headers + indented worktree rows), tracking the
	// display index of the cursor so the window keeps it visible.
	var display []string
	cursorAt, lastRepo := 0, ""
	for i, it := range m.active {
		if it.repo.Name != lastRepo {
			display = append(display, repoHdrStyle.Render(it.repo.Name))
			lastRepo = it.repo.Name
		}
		if i == m.activeCursor {
			cursorAt = len(display)
		}
		display = append(display, m.activeRow(it, i == m.activeCursor && m.focus == focusActive, innerW))
	}

	start, end := windowBounds(len(display), cursorAt, rows)
	return append(lines, display[start:end]...)
}

func (m Model) activeRow(it activeItem, highlight bool, innerW int) string {
	dotChar := "○"
	if it.view.Live {
		dotChar = "●"
	}
	dirtyChar := " "
	if it.view.Dirty {
		dirtyChar = "✷"
	}

	if highlight {
		return selBar(fmt.Sprintf("  ▸ %s %s %s", dotChar, dirtyChar, it.view.WT.Name), innerW)
	}

	dot := dimStyle.Render(dotChar)
	if it.view.Live {
		dot = liveStyle.Render(dotChar)
	}
	dirty := " "
	if it.view.Dirty {
		dirty = dirtyStyle.Render(dirtyChar)
	}
	return "    " + dot + " " + dirty + " " + nameStyle.Render(it.view.WT.Name)
}

func (m Model) renderFooter() string {
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
			keyhint("enter", "create"), keyhint("tab", "active"), keyhint("ctrl+c", "quit"),
		}
	} else {
		hints = []string{
			keyhint("↑↓", "move"), keyhint("enter", "open"),
			keyhint("d", "stop"), keyhint("x", "remove"),
			keyhint("tab", "new"), keyhint("q", "quit"),
		}
	}
	help := strings.Join(hints, helpStyle.Render("  ·  "))

	if m.status != "" {
		style := helpStyle
		if strings.Contains(m.status, "error") {
			style = errStyle
		}
		return style.Render(m.status) + "\n" + help
	}
	return "\n" + help
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
