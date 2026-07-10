package tui

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/KCaverly/caretaker/internal/session"
	"github.com/KCaverly/caretaker/internal/usage"
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

	// Non-bold accent/purple foregrounds for the bar's volatile context
	// segments (pane indicator, agent position) and the accent split divider.
	accentStyle = lipgloss.NewStyle().Foreground(cAccent)
	purpleStyle = lipgloss.NewStyle().Foreground(cPurple)

	// Bold glyph styles, cached rather than rebuilt per frame: the status-bar
	// tab icons (a lit accent per tab plus the shared dim/faint states) and the
	// red/green attention markers reused across the bar, board, and deck.
	// lipgloss.Style is immutable (each method returns a copy) and View runs
	// only on the UI goroutine, so sharing these package-level styles is safe.
	boldDim    = lipgloss.NewStyle().Bold(true).Foreground(cDim)
	boldFaint  = lipgloss.NewStyle().Bold(true).Foreground(cFaint)
	boldGreen  = lipgloss.NewStyle().Bold(true).Foreground(cGreen)
	boldPurple = lipgloss.NewStyle().Bold(true).Foreground(cPurple)
	boldAccent = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	boldYellow = lipgloss.NewStyle().Bold(true).Foreground(cYellow)
	boldRed    = lipgloss.NewStyle().Bold(true).Foreground(cRed)

	// Bordered box frames for the deck sections and overlays: faint idle,
	// accent when focused. Rounded border + 0,1 padding match the old per-call
	// style exactly.
	boxStyleFaint   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cFaint).Padding(0, 1)
	boxStyleFocused = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cAccent).Padding(0, 1)
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
	case m.usageOpen:
		body = m.renderUsage(h - barHeight)
	case m.screen == screenPicker:
		body = m.renderDeck(h - barHeight)
	case m.screen == screenTerminal && m.current != nil && m.current.ws != nil:
		body, cursor = m.renderTermPanes(w, h-barHeight-m.sessionFooterH())
		body = m.appendSessionFooter(body)
	default:
		if s := m.activeSession(); s != nil {
			body = s.Render()
			if x, y, visible := s.Cursor(); visible {
				cursor = tea.NewCursor(x, y+barHeight)
			}
			body = m.appendSessionFooter(body)
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
	iconDeck   = "\U0000EDA7" // fa-seedling (U+EDA7) — the deck: a grove of worktrees
	iconEditor = ""          // fa-code (U+F121)     — nvim
	iconAgent  = "󰚩"          // md-robot (U+F06A9)   — claude
	iconTerm   = ""          // fa-terminal (U+F120) — term
	iconPanes  = "\U0000F009" // fa-th-large (U+F009) — the split-pane grid
	// Zoom toggle shown at the tail of the pane indicator (clickable): diagonal
	// arrows expanding out to maximize the active pane, collapsing in to restore
	// the split. Material Design Icons range — the classic Font Awesome
	// expand/compress glyphs (U+F065/F066) are absent from the bundled Nerd Font
	// symbols, same as the seedling's pre-v3 codepoint was.
	iconZoomIn  = "\U000F0616" // md-arrow-expand   (U+F0616) — maximize the active pane
	iconZoomOut = "\U000F0615" // md-arrow-collapse (U+F0615) — restore the split layout
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
// worktrees where an agent is waiting on input and "* N" (green) for worktrees
// with unread completions. Returns "" when nothing is pending. Clicking it
// opens the agent board.
func (m Model) renderNotifZone() string {
	waiting, done := m.attnSummary()
	var parts []string
	if waiting > 0 {
		parts = append(parts, boldRed.Render("!")+
			" "+countStyle.Render(strconv.Itoa(waiting)))
	}
	if done > 0 {
		parts = append(parts, boldGreen.Render("*")+
			" "+countStyle.Render(strconv.Itoa(done)))
	}
	return strings.Join(parts, "  ")
}

// barContextLabel builds the bar's right-side workspace context: the
// "repo / worktree" label, preceded by the agent pool position
// ("2/3 label ·") on the agent screen when the workspace has more than one
// agent, and the pane position (grid glyph + "2/3" + a clickable zoom toggle)
// on the terminal screen with splits. Each volatile segment shows only on the
// screen it steers, so the bar never advertises a position the current keys
// can't change. Both segments sit to the left because they hot-swap
// far more often than the worktree — keeping the repo / worktree label
// anchored at the right edge while agents and panes rotate. It surfaces
// state that is otherwise invisible — which agent is focused, how many exist,
// whether a pane is zoomed — and thereby advertises the prev/next-agent and
// zoom keys.
func (m Model) barContextLabel() string {
	if m.current == nil {
		return ""
	}
	s := dimStyle.Render(m.current.repo + " / " + m.current.worktree)
	ws := m.current.ws
	if ws == nil {
		return s
	}
	sep := dimStyle.Render(" · ")
	if seg, ok := m.paneSegment(); ok {
		s = accentStyle.Render(seg) + sep + s
	}
	if n := len(ws.Agents); m.screen == screenAgent && n > 1 {
		pos := fmt.Sprintf("%d/%d", clamp(ws.ActiveAgent, 0, n-1)+1, n)
		if a := ws.ActiveAgentSession(); a != nil {
			pos += " " + truncateTo(agentTitle(a.Title), 14)
		}
		s = purpleStyle.Render(pos) + sep + s
	}
	if seg, ok := m.usageSegment(); ok {
		w, _ := m.usageSnap.Binding()
		s = usageSeverity(w.Utilization).Render(seg) + sep + s
	}
	return s
}

// usageSegment returns the plan-usage gauge — pie glyph, the binding window's
// percent, and when it resets — and whether it applies: agent screen only,
// snapshot still fresh, and utilization at or past the configured threshold.
// Shared by barContextLabel (which styles and places it) and usageZoneAt
// (which measures it), so the drawn text and its click target can never
// drift. Like the other volatile segments it sits left of the stable
// repo / worktree label; it leads them because it hot-swaps with every poll.
func (m Model) usageSegment() (string, bool) {
	if m.screen != screenAgent || !m.usageFresh() {
		return "", false
	}
	w, limit := m.usageSnap.Binding()
	if w == nil || w.Utilization < float64(m.usageThreshold) {
		return "", false
	}
	seg := usagePie(w.Utilization)
	switch limit {
	case usage.LimitWeek:
		seg += " wk"
	case usage.LimitOpus:
		seg += " opus"
	}
	seg += fmt.Sprintf(" %d%%", int(w.Utilization+0.5))
	if !w.ResetsAt.IsZero() {
		if limit == usage.LimitSession {
			seg += " " + humanDur(time.Until(w.ResetsAt))
		} else {
			// A week-scale reset lands days out; the weekday says enough.
			seg += " " + strings.ToLower(w.ResetsAt.Local().Format("Mon"))
		}
	}
	return seg, true
}

// usageZoneAt reports whether bar coordinates land on the usage segment.
// It mirrors renderBar's right-side layout: the segment leads
// barContextLabel, so its left edge is where the context label begins.
func (m Model) usageZoneAt(x, y int) bool {
	if y != 0 {
		return false
	}
	seg, ok := m.usageSegment()
	if !ok {
		return false
	}
	prefix := ""
	if notif := m.renderNotifZone(); notif != "" {
		prefix = notif + "   "
	}
	right := prefix + m.barContextLabel() + "  "
	start := m.width - lipgloss.Width(right) + lipgloss.Width(prefix)
	return x >= start && x < start+lipgloss.Width(seg)
}

// usagePie maps a utilization percent to a pie glyph that fills as the
// window is consumed. Bands are centered so each glyph covers the quarter it
// depicts (e.g. ◑ spans 37.5–62.5).
func usagePie(util float64) string {
	switch {
	case util < 12.5:
		return "○"
	case util < 37.5:
		return "◔"
	case util < 62.5:
		return "◑"
	case util < 87.5:
		return "◕"
	default:
		return "●"
	}
}

// usageSeverity styles a utilization percent: dim while comfortable, yellow
// from 70, red and bold from 90. Deliberately independent of the visibility
// threshold, so a low threshold still colour-codes an urgent window.
func usageSeverity(util float64) lipgloss.Style {
	switch {
	case util >= 90:
		return lipgloss.NewStyle().Foreground(cRed).Bold(true)
	case util >= 70:
		return lipgloss.NewStyle().Foreground(cYellow)
	default:
		return dimStyle
	}
}

// paneSegment returns the terminal-screen pane indicator — grid glyph, pane
// position, and a trailing zoom toggle (iconZoomIn to maximize, iconZoomOut to
// restore) — and whether it applies (only on the terminal screen with more
// than one pane). Shared by barContextLabel (which styles and places it) and
// paneZoomAt (which measures it), so the drawn glyph and its click target can
// never drift. The zoom toggle is always the segment's final glyph.
func (m Model) paneSegment() (string, bool) {
	if m.current == nil || m.current.ws == nil {
		return "", false
	}
	ws := m.current.ws
	if m.screen != screenTerminal || len(ws.Terms) <= 1 {
		return "", false
	}
	pos := clamp(ws.ActiveTerm, 0, len(ws.Terms)-1) + 1
	return fmt.Sprintf("%s %d/%d %s", iconPanes, pos, len(ws.Terms), m.paneZoomIcon()), true
}

// paneZoomIcon is the zoom-toggle glyph reflecting the current pane state:
// inward arrows while a pane is maximized (click to restore), outward arrows
// otherwise (click to maximize the active pane).
func (m Model) paneZoomIcon() string {
	if m.current != nil && m.current.ws != nil && m.current.ws.TermZoomed {
		return iconZoomOut
	}
	return iconZoomIn
}

// paneZoomAt reports whether bar coordinates land on the pane indicator's zoom
// toggle. It mirrors renderBar's right-side layout to locate the segment, then
// targets its trailing icon (the toggle is always the segment's last glyph).
func (m Model) paneZoomAt(x, y int) bool {
	if y != 0 {
		return false
	}
	seg, ok := m.paneSegment()
	if !ok {
		return false
	}
	prefix := ""
	if notif := m.renderNotifZone(); notif != "" {
		prefix = notif + "   "
	}
	right := prefix + m.barContextLabel() + "  "
	labelStart := m.width - lipgloss.Width(right) + lipgloss.Width(prefix)
	iconW := lipgloss.Width(m.paneZoomIcon())
	iconStart := labelStart + lipgloss.Width(seg) - iconW
	// One column of slack on the left (the space before the icon) so the small
	// target is easy to hit.
	return x >= iconStart-1 && x < iconStart+iconW
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
	// colour (the lit style passed in), idle is dim, and disabled is faint until
	// a workspace exists. The styles are package-level, cached once, not rebuilt
	// per icon per frame.
	glyph := func(g string, lit lipgloss.Style, active, enabled bool) string {
		switch {
		case active:
			return lit.Render(g)
		case enabled:
			return boldDim.Render(g)
		default:
			return boldFaint.Render(g)
		}
	}

	// Caretaker: a stable seedling that follows the same lit-when-active rule as
	// the other tabs — yellow while the deck is active, dim otherwise (never
	// faint: the deck is always reachable). Agent attention lives in the ! badge
	// on the right, so the icon never reacts to agent status.
	ctStyle := boldDim
	if m.screen == screenPicker {
		ctStyle = boldYellow
	}
	ct := ctStyle.Render(iconDeck)

	agent := glyph(iconAgent, boldPurple, m.screen == screenAgent, has)

	return []barZone{
		{screenPicker, ct},
		{screenEditor, glyph(iconEditor, boldGreen, m.screen == screenEditor, has)},
		{screenAgent, agent},
		{screenTerminal, glyph(iconTerm, boldAccent, m.screen == screenTerminal, has)},
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
// label, and the right-aligned (truncated) status column.
func (m Model) boardAgentLine(r boardRow, innerW int) string {
	numCol := " "
	if r.num > 0 {
		numCol = strconv.Itoa(r.num)
	}
	glyph, glyphSt := " ", dimStyle
	switch r.attn {
	case attnWaiting:
		glyph, glyphSt = "!", boldRed
	case attnDone:
		glyph, glyphSt = "*", boldGreen
	}
	left := "   " + dimStyle.Render(numCol) + " " + glyphSt.Render(glyph) + " " + nameStyle.Render(r.label)
	status := truncateTo(r.status, max(0, innerW-lipgloss.Width(left)-2))
	right := dimStyle.Render(status)
	gap := max(2, innerW-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

// renderBoardForm draws the new-agent form: the prompt input plus the
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

// usageGaugeWidth is the overlay gauge's cell count — wide enough that one
// cell is a legible ~5.5%, narrow enough to fit the overlay's minimum width.
const usageGaugeWidth = 18

// usageBurnMinSpan is how much time the sample ring must cover before the
// overlay shows a burn rate; two adjacent polls would extrapolate noise.
const usageBurnMinSpan = 5 * time.Minute

// renderUsage draws the plan-usage overlay: one gauge per present limit
// window with its reset time, plus a burn-rate estimate once the sample ring
// spans enough time to be meaningful. Unlike the bar segment it ignores the
// threshold and staleness gates — when the user explicitly asks, they get
// whatever ct knows.
func (m Model) renderUsage(h int) string {
	innerW := clamp(m.width-8, 32, 56)
	rows := []string{header("usage", -1), ""}
	if !m.usageHave {
		rows = append(rows, dimStyle.Render("  no usage data"))
	} else {
		rows = append(rows, m.usageWindowRows()...)
		rows = append(rows, m.usageBurnRows()...)
	}
	rows = append(rows, "", "  "+keyhint("esc", "close"))
	boxStr := box(rows, innerW, len(rows), true)
	return centerBlock(boxStr, m.width, h)
}

// usageWindowRows renders a gauge line (and an indented reset line) per
// present window; absent windows are skipped outright — a placeholder row
// would imply a limit the plan doesn't have.
func (m Model) usageWindowRows() []string {
	var rows []string
	add := func(label string, w *usage.Window, session bool) {
		if w == nil {
			return
		}
		rows = append(rows, "  "+dimStyle.Render(padLine(label, 9))+usageGauge(w.Utilization)+
			usageSeverity(w.Utilization).Render(fmt.Sprintf(" %3d%%", int(w.Utilization+0.5))))
		if w.ResetsAt.IsZero() {
			return
		}
		reset := w.ResetsAt.Local()
		line := "resets " + strings.ToLower(reset.Format("Mon 3:04 PM"))
		if session {
			// The session window resets within hours; the countdown matters
			// more than the weekday.
			line = fmt.Sprintf("resets %s (%s)",
				strings.ToLower(reset.Format("3:04 PM")), untilPhrase(time.Until(reset)))
		}
		rows = append(rows, "  "+strings.Repeat(" ", 9)+dimStyle.Render(line))
	}
	add("session", m.usageSnap.FiveHour, true)
	add("week", m.usageSnap.SevenDay, false)
	add("opus", m.usageSnap.SevenDayOpus, false)
	return rows
}

// usageBurnRows estimates the five-hour window's burn rate from the sample
// ring: a sparkline of the between-poll deltas, the hourly rate, and — when
// the window would cap before it resets — the projected time it caps. Hidden
// until the ring spans usageBurnMinSpan with a rising trend, so a flat or
// draining window never shows a scary extrapolation, and the projection line
// is dropped when the reset lands first (the window frees up before it caps).
func (m Model) usageBurnRows() []string {
	hist := m.usageHist
	if len(hist) < 2 {
		return nil
	}
	first, last := hist[0], hist[len(hist)-1]
	span := last.at.Sub(first.at)
	if span < usageBurnMinSpan || last.util <= first.util {
		return nil
	}
	perHour := (last.util - first.util) / span.Hours()
	rows := []string{"  " + dimStyle.Render(padLine("burn", 9)) + usageSparkline(hist) +
		dimStyle.Render(fmt.Sprintf(" ~%d%%/hr", int(perHour+0.5)))}
	if w := m.usageSnap.FiveHour; w != nil && w.Utilization < 100 {
		caps := time.Now().Add(time.Duration((100 - w.Utilization) / perHour * float64(time.Hour)))
		if w.ResetsAt.IsZero() || caps.Before(w.ResetsAt) {
			rows = append(rows, "  "+strings.Repeat(" ", 9)+
				dimStyle.Render("at this pace: caps ~"+strings.ToLower(caps.Local().Format("3:04 PM"))))
		}
	}
	return rows
}

// usageSparkline draws the ring's between-sample utilization deltas as a
// block ramp: flat polls sit on the baseline, the steepest delta tops out.
// Only the newest deltas are drawn so the line always fits the overlay.
func usageSparkline(hist []usageSample) string {
	const maxCells = 24
	ramp := []rune("▁▂▃▄▅▆▇█")
	if len(hist) > maxCells+1 {
		hist = hist[len(hist)-maxCells-1:]
	}
	deltas := make([]float64, 0, len(hist)-1)
	maxD := 0.0
	for i := 1; i < len(hist); i++ {
		d := max(0, hist[i].util-hist[i-1].util)
		deltas = append(deltas, d)
		maxD = max(maxD, d)
	}
	var b strings.Builder
	for _, d := range deltas {
		idx := 0
		if maxD > 0 {
			idx = int(d / maxD * float64(len(ramp)-1))
		}
		b.WriteRune(ramp[idx])
	}
	return b.String()
}

// usageGauge renders a utilization percent as a fixed-width block gauge, the
// filled span coloured by the same severity ramp as the bar segment.
func usageGauge(util float64) string {
	filled := clamp(int(util/100*usageGaugeWidth+0.5), 0, usageGaugeWidth)
	return usageSeverity(util).Render(strings.Repeat("█", filled)) +
		dimStyle.Render(strings.Repeat("░", usageGaugeWidth-filled))
}

// untilPhrase formats a duration as the overlay's parenthetical countdown.
func untilPhrase(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
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
		notifSt = boldRed
	case attnDone:
		notifChar = "*"
		notifSt = boldGreen
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
		row("↑↓ ^p / j k", "move"),
		row("tab", "switch section"),
		row("enter", "open / create"),
		row("1 2 3", "open recent worktree"),
		row("d", "stop worktree"),
		row("x", "remove worktree (b keeps branch)"),
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
		row(m.keyUsage, "usage limits"),
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

// sessionFooterH is the number of rows reserved beneath a session body for the
// one-line help hint: one until the user's first keystroke into a session,
// zero after (see hintSeen). It is intentionally screen-independent so the
// reserved size is stable across every session view — sessionSize is queried
// before the target screen is even set.
func (m Model) sessionFooterH() int {
	if m.hintSeen {
		return 0
	}
	return 1
}

// appendSessionFooter tacks the help hint onto a session body while it is still
// reserved. The body was already rendered a row shorter (sessionSize subtracts
// sessionFooterH), so the combined height matches the non-hint case exactly.
func (m Model) appendSessionFooter(body string) string {
	if m.sessionFooterH() == 0 {
		return body
	}
	return body + "\n" + m.sessionFooter()
}

// sessionFooter builds the dim one-line hint shown beneath a session until the
// user first types. It always leads with the help key (which opens the full
// overlay) and, on the terminal screen, surfaces the pane-management keys the
// hint mainly exists to teach. Trailing hints are dropped rather than wrapped
// so the line always fits the single reserved row.
func (m Model) sessionFooter() string {
	hints := []string{keyhint(m.keyHelp, "help")}
	if m.screen == screenTerminal {
		hints = append(hints,
			keyhint(m.keyTermSplitV+" "+m.keyTermSplitH, "split"),
			keyhint(m.keyTermCycle, "cycle pane"),
			keyhint(m.keyTermZoom, "zoom"),
			keyhint(m.keyTermClose, "close"))
	} else {
		hints = append(hints,
			keyhint(m.keyCycle, "cycle view"),
			keyhint(m.keyPicker, "deck"))
	}
	sep := helpStyle.Render("  ·  ")
	for n := len(hints); n >= 1; n-- {
		line := "  " + strings.Join(hints[:n], sep)
		if lipgloss.Width(line) <= m.width {
			return line
		}
	}
	return "  " + hints[0]
}

// footerContent builds the two-row footer (status line + help line) before
// centering.
func (m Model) footerContent() string {
	switch m.mode {
	case modeCreateName:
		// A rejected name (client-side validation) sets a sticky error status;
		// style it red here so the hint stands out from ordinary form chrome.
		if m.statusLevel == statusError {
			return "\n" + errStyle.Render(m.status)
		}
		return "\n" + helpStyle.Render(m.status)
	case modeConfirmRemove, modeConfirmQuit, modeConfirmStop:
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

// dividerStyle returns the cached foreground style for a pane divider: lit
// (accent) when the divider borders the focused pane, faint otherwise. It maps
// the two colours paneAdjacentColor can return to their package-level styles so
// the divider style is never rebuilt per split per frame. barSep is exactly a
// faint foreground, so it doubles as the faint divider style.
func dividerStyle(c color.Color) lipgloss.Style {
	if c == cAccent {
		return accentStyle
	}
	return barSep
}

// joinVerticalSplit interleaves lines from left and right pane bodies with a
// single-column divider. leftWidth is the pane's column count — every left
// line is padded to that exact display width so the divider lands on a
// consistent column regardless of how much content the vt emulator rendered.
func joinVerticalSplit(left, right string, divColor color.Color, h, leftWidth int) string {
	leftLines := splitLines(left)
	rightLines := splitLines(right)
	div := dividerStyle(divColor).Render("│")
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
	div := dividerStyle(divColor).Render(strings.Repeat("─", max(1, w)))
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
	st := boxStyleFaint
	if focused {
		st = boxStyleFocused
	}
	return st.Render(strings.Join(rows, "\n"))
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
