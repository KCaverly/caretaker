package tui

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/KCaverly/caretaker/internal/agent"
	"github.com/KCaverly/caretaker/internal/config"
	"github.com/KCaverly/caretaker/internal/repo"
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
	cSelBg  = lipgloss.Color("#665C54") // bg3 (strong selection)
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

	// Deck work-state cluster: commits the branch carries beyond main (green,
	// the accent's healthy sibling) and commits it trails behind (yellow, the
	// same caution hue as the dirty marker). Hoisted like the other row styles.
	aheadStyle   = lipgloss.NewStyle().Foreground(cGreen)
	behindStyle  = lipgloss.NewStyle().Foreground(cYellow)
	selStyle     = lipgloss.NewStyle().Bold(true).Foreground(cFg).Background(cSelBg)
	selANSI      = ansi.NewStyle().Bold().ForegroundColor(cFg).BackgroundColor(cSelBg).String()
	helpKeyStyle = lipgloss.NewStyle().Foreground(cAccent)
	helpStyle    = lipgloss.NewStyle().Foreground(cDim)
	errStyle     = lipgloss.NewStyle().Foreground(cRed)
	// stackWaitStyle colours the deck's "checks pending" stack glyph (yellow),
	// kept distinct from the ahead/behind hues it sits beside in the row cluster.
	stackWaitStyle = lipgloss.NewStyle().Foreground(cYellow)

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

	// Diff viewer line styles, hoisted like the deck's so they're never rebuilt
	// per frame: added lines green, removed lines red, hunk headers in the
	// accent, `diff --git` file headers bold, and section rules / the truncation
	// notice in the faint style.
	diffAddStyle  = lipgloss.NewStyle().Foreground(cGreen)
	diffDelStyle  = lipgloss.NewStyle().Foreground(cRed)
	diffHunkStyle = lipgloss.NewStyle().Foreground(cAccent)
	diffFileStyle = lipgloss.NewStyle().Bold(true).Foreground(cFg)
	diffRuleStyle = lipgloss.NewStyle().Foreground(cFaint)

	// Bordered box frames for the deck sections and overlays: faint idle,
	// accent when focused. Rounded border + 0,1 padding match the old per-call
	// style exactly.
	boxStyleFaint   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cFaint).Padding(0, 1)
	boxStyleFocused = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cAccent).Padding(0, 1)
)

// View implements tea.Model.
func (m Model) View() tea.View {
	w, h := m.width, m.height
	if w < minViableWidth || h < minViableHeight {
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
	case m.paletteOpen:
		body = m.renderPalette(h - barHeight)
	case m.confirmActive():
		// Confirmations can originate from either the deck or the agent board;
		// render them above their originating surface in both cases.
		body = m.renderConfirm(h - barHeight)
	case m.boardOpen:
		body = m.renderBoard(h - barHeight)
	case m.usageOpen:
		body = m.renderUsage(h - barHeight)
	case m.diffOpen:
		body = m.renderDiff(h - barHeight)
	case m.stackOpen:
		body = m.renderStack(h - barHeight)
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

const (
	minViableWidth  = 24
	minViableHeight = 16
)

func (m Model) destinationGlyph(s screen) string {
	mode := m.iconMode
	if mode == "" {
		mode = config.IconsNerd
	}
	if mode == config.IconsText {
		switch s {
		case screenPicker:
			return "ct"
		case screenEditor:
			return "edit"
		case screenAgent:
			return "agent"
		default:
			return "term"
		}
	}
	if mode == config.IconsASCII {
		switch s {
		case screenPicker:
			return "C"
		case screenEditor:
			return "E"
		case screenAgent:
			return "A"
		default:
			return "T"
		}
	}
	switch s {
	case screenPicker:
		return iconDeck
	case screenEditor:
		return iconEditor
	case screenAgent:
		return iconAgent
	default:
		return iconTerm
	}
}

func (m Model) paneGridGlyph() string {
	switch m.iconMode {
	case config.IconsText:
		return "panes"
	case config.IconsASCII:
		return "#"
	default:
		return iconPanes
	}
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
	left := m.renderBarLeft()

	// Right side: notification zone (! N  ✓ N) then the workspace context. Its
	// layout (and the click targets within it) is derived once by barRightZones.
	right := m.barRightZones().text

	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	bar := ansi.Truncate(left+strings.Repeat(" ", gap)+right, m.width, "")
	sep := barSep.Render(strings.Repeat("─", max(1, m.width)))
	return bar + "\n" + sep
}

func (m Model) renderBarLeft() string {
	left := "  "
	for i, z := range m.barZones() {
		if i > 0 {
			left += "   " // equidistant gap between icons
		}
		left += z.glyph
	}
	return left
}

// renderNotifZone builds the right-side attention summary: "! N" (red) for
// worktrees where an agent is waiting on input and "✓ N" (green) for worktrees
// with unread completions. Returns "" when nothing is pending. Clicking it
// jumps straight to the agent needing attention (the attention-jump chord's
// destination), cycling to the next on each click.
func (m Model) renderNotifZone() string {
	waiting, done := m.attnSummary()
	var parts []string
	if waiting > 0 {
		parts = append(parts, boldRed.Render("!")+
			" "+countStyle.Render(strconv.Itoa(waiting)))
	}
	if done > 0 {
		parts = append(parts, boldGreen.Render("✓")+
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
	if n := len(ws.Agents); m.screen == screenAgent && n > 0 {
		if a := ws.ActiveAgentSession(); a != nil {
			provider := normalizedProvider(a.Provider)
			identity := provider.String()
			if title := agentTitle(provider, a.Title); title != provider.String() {
				identity += " · " + title
			}
			if n > 1 {
				identity = fmt.Sprintf("%d/%d ", clamp(ws.ActiveAgent, 0, n-1)+1, n) + identity
			}
			s = purpleStyle.Render(truncateTo(identity, 32)) + sep + s
		}
	}
	if seg, ok := m.usageSegment(); ok {
		provider, _ := m.activeUsageProvider()
		snap, _, _ := m.usageState(provider)
		binding, _ := snap.BindingWindow()
		s = usageSeverity(binding.Window.Utilization).Render(seg) + sep + s
	}
	return s
}

func (m Model) activeUsageProvider() (agent.Provider, bool) {
	if m.current == nil || m.current.ws == nil {
		return "", false
	}
	session := m.current.ws.ActiveAgentSession()
	if session == nil {
		return "", false
	}
	provider := normalizedProvider(session.Provider)
	return provider, providerIn(provider, m.agentProviders)
}

// usageSegment returns the plan-usage gauge — pie glyph, the binding window's
// percent, and when it resets — and whether it applies: agent screen only,
// snapshot still fresh, and utilization at or past the configured threshold.
// Shared by barContextLabel (which styles and places it) and usageZoneAt
// (which measures it), so the drawn text and its click target can never
// drift. Like the other volatile segments it sits left of the stable
// repo / worktree label; it leads them because it hot-swaps with every poll.
func (m Model) usageSegment() (string, bool) {
	provider, ok := m.activeUsageProvider()
	if !ok || m.screen != screenAgent || !m.usageFresh(provider) {
		return "", false
	}
	snap, _, _ := m.usageState(provider)
	binding, ok := snap.BindingWindow()
	if !ok || binding.Window.Utilization < float64(m.usageThreshold) {
		return "", false
	}
	w := binding.Window
	seg := usagePie(w.Utilization)
	if binding.ShortLabel != "" {
		seg += " " + binding.ShortLabel
	}
	seg += fmt.Sprintf(" %d%%", int(w.Utilization+0.5))
	if !w.ResetsAt.IsZero() {
		if binding.Session {
			seg += " " + humanDur(time.Until(w.ResetsAt))
		} else {
			// A week-scale reset lands days out; the weekday says enough.
			seg += " " + strings.ToLower(w.ResetsAt.Local().Format("Mon"))
		}
	}
	return seg, true
}

// usageZoneAt reports whether bar coordinates land on the usage segment, using
// the shared barRightZones layout so the click target tracks what renderBar drew.
func (m Model) usageZoneAt(x, y int) bool {
	if y != 0 {
		return false
	}
	z := m.barRightZones()
	if z.usage == "" {
		return false
	}
	return x >= z.usageStart && x < z.usageStart+lipgloss.Width(z.usage)
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
	return fmt.Sprintf("%s %d/%d %s", m.paneGridGlyph(), pos, len(ws.Terms), m.paneZoomIcon()), true
}

// paneZoomIcon is the zoom-toggle glyph reflecting the current pane state:
// inward arrows while a pane is maximized (click to restore), outward arrows
// otherwise (click to maximize the active pane).
func (m Model) paneZoomIcon() string {
	if m.iconMode == config.IconsText {
		if m.current != nil && m.current.ws != nil && m.current.ws.TermZoomed {
			return "restore"
		}
		return "zoom"
	}
	if m.iconMode == config.IconsASCII {
		if m.current != nil && m.current.ws != nil && m.current.ws.TermZoomed {
			return "-"
		}
		return "+"
	}
	if m.current != nil && m.current.ws != nil && m.current.ws.TermZoomed {
		return iconZoomOut
	}
	return iconZoomIn
}

// paneZoomAt reports whether bar coordinates land on the pane indicator's zoom
// toggle. It uses the shared barRightZones layout to locate the segment, then
// targets its trailing icon (the toggle is always the segment's last glyph).
func (m Model) paneZoomAt(x, y int) bool {
	if y != 0 {
		return false
	}
	z := m.barRightZones()
	if z.pane == "" {
		return false
	}
	iconW := lipgloss.Width(m.paneZoomIcon())
	iconStart := z.paneStart + lipgloss.Width(z.pane) - iconW
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
	ct := ctStyle.Render(m.destinationGlyph(screenPicker))

	agent := glyph(m.destinationGlyph(screenAgent), boldPurple, m.screen == screenAgent, has)

	zones := []barZone{
		{screenPicker, ct},
		{screenEditor, glyph(m.destinationGlyph(screenEditor), boldGreen, m.screen == screenEditor, has)},
		{screenAgent, agent},
		{screenTerminal, glyph(m.destinationGlyph(screenTerminal), boldAccent, m.screen == screenTerminal, has)},
	}
	// On narrow terminals, optional inactive destinations yield before the
	// anchored workspace identity. Every destination remains keyboard-reachable.
	if m.width < 48 {
		for _, z := range zones {
			if z.s == m.screen {
				return []barZone{z}
			}
		}
	}
	return zones
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

// barRight holds the bar's fully-assembled right side plus the start columns of
// its clickable zones, so drawing (renderBar) and hit-testing (notifZoneAt /
// usageZoneAt / paneZoomAt) share one layout and can never drift apart —
// mirroring how barZones unifies the left icons. Zone strings are "" when the
// zone isn't shown; their start columns are only meaningful when non-empty.
type barRight struct {
	text       string // the right side exactly as drawn
	notif      string // notification zone ("! N  ✓ N")
	notifStart int
	usage      string // plan-usage segment (unstyled)
	usageStart int
	pane       string // terminal pane indicator (unstyled)
	paneStart  int
}

// barRightZones assembles the bar's right side and locates its clickable zones.
// The right side is the notification zone (with a 3-column gap) followed by the
// workspace context label, with a 2-column trailing pad when anything is shown —
// exactly what renderBar draws. The usage and pane segments each lead the context
// label on their own screen (they never coexist), so both begin where the label
// does.
func (m Model) barRightZones() barRight {
	z := barRight{notif: m.renderNotifZone()}
	label := m.barContextLabel()
	fullLabel := label
	available := max(0, m.width-lipgloss.Width(m.renderBarLeft())-1)
	assemble := func() string {
		text := ""
		if z.notif != "" {
			text = z.notif + "   "
		}
		text += label
		if text != "" {
			text += "  "
		}
		return text
	}
	if lipgloss.Width(assemble()) > available && m.current != nil {
		// Volatile usage/agent/pane facts disappear before stable identity.
		label = dimStyle.Render(m.current.repo + " / " + m.current.worktree)
	}
	if lipgloss.Width(assemble()) > available {
		z.notif = ""
	}
	if lipgloss.Width(assemble()) > available {
		label = ansi.Truncate(label, max(0, available-2), "…")
	}
	z.text = assemble()

	z.notifStart = m.width - lipgloss.Width(z.text)
	labelStart := z.notifStart
	if z.notif != "" {
		labelStart += lipgloss.Width(z.notif + "   ")
	}
	if label == fullLabel {
		if seg, ok := m.usageSegment(); ok {
			z.usage, z.usageStart = seg, labelStart
		}
		if seg, ok := m.paneSegment(); ok {
			z.pane, z.paneStart = seg, labelStart
		}
	}
	return z
}

// notifZoneAt reports whether bar coordinates (x, y) land on the notification
// zone, using the shared barRightZones layout to locate the zone's x bounds.
func (m Model) notifZoneAt(x, y int) bool {
	if y != 0 {
		return false
	}
	z := m.barRightZones()
	if z.notif == "" {
		return false
	}
	return x >= z.notifStart && x < z.notifStart+lipgloss.Width(z.notif)
}

// renderBoard draws the agent board overlay: every open workspace's agents
// grouped under worktree header rows, attention sorted to the top, plus the
// trailing "+ new agent" row. Delegates to renderBoardForm in form state.
func (m Model) renderBoard(h int) string {
	if m.formOpen {
		return m.renderBoardForm(h, panelInnerWidth(m.width, 84))
	}
	// Match the help overlay's readable maximum while allowing every agent row
	// and its selection bar to use the full width inside the panel.
	innerW := panelInnerWidth(m.width, 72)

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

	separator := helpStyle.Render("  ·  ")
	lines = append(lines, "",
		"  "+strings.Join([]string{
			keyhint("↑↓", "move"), keyhint("1-9", "jump"), keyhint("enter", "focus"),
		}, separator),
		"  "+strings.Join([]string{
			keyhint("n", "new"), keyhint("r", "restart agent"), keyhint("d", "remove agent"), keyhint("esc", "close"),
		}, separator),
	)

	return renderPanel(lines, innerW, m.width, h)
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
		glyph, glyphSt = "✓", boldGreen
	}
	chip := helpKeyStyle.Render(normalizedProvider(r.provider).String())
	left := "   " + dimStyle.Render(numCol) + " " + glyphSt.Render(glyph) + " " + chip +
		dimStyle.Render(" · ") + nameStyle.Render(r.label)
	status := truncateTo(r.status, max(0, innerW-lipgloss.Width(left)-2))
	statusSt := dimStyle
	switch {
	case r.attn == attnWaiting:
		statusSt = boldRed
	case r.attn == attnDone:
		statusSt = boldGreen
	case strings.HasPrefix(status, "working"):
		statusSt = accentStyle
	}
	right := statusSt.Render(status)
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
	selectedToggle := lipgloss.NewStyle().Bold(true).Foreground(cInk).Background(cAccent)
	toggle := func(options [2]string, sel int) string {
		var parts [2]string
		for i, o := range options {
			if i == sel {
				parts[i] = selectedToggle.Render(" " + o + " ")
			} else {
				parts[i] = dimStyle.Render(" " + o + " ")
			}
		}
		return parts[0] + helpStyle.Render(" ") + parts[1]
	}
	toggleRow := func(field int, label string, options [2]string, sel int) string {
		line := "  " + fieldName(field, label) + " " + toggle(options, sel)
		if m.formFocus == field {
			line += helpStyle.Render("   ← → change")
		}
		return line
	}
	rows := []string{
		header("new agent", -1),
		"",
		"  " + fieldName(formFieldPrompt, "What should "+providerName(m.formProvider)+" do?"),
	}
	for _, line := range strings.Split(m.promptInput.View(), "\n") {
		rows = append(rows, "  "+line)
	}
	rows = append(rows, "")
	if len(m.agentProviders) > 1 {
		providerIdx := 0
		for i, provider := range m.agentProviders {
			if provider == m.formProvider {
				providerIdx = i
			}
		}
		rows = append(rows, toggleRow(formFieldProvider, "provider",
			[2]string{m.agentProviders[0].String(), m.agentProviders[1].String()}, providerIdx))
	}
	currentLocation := "current worktree"
	if m.current != nil && m.current.worktree != "" {
		currentLocation = "current: " + truncateTo(m.current.worktree, 24)
	}
	rows = append(rows,
		toggleRow(formFieldWhere, "where", [2]string{currentLocation, "home"}, m.formLocation),
		"",
		"  "+strings.Join([]string{
			keyhint("ctrl+enter", "launch"), keyhint("tab", "field"),
			keyhint("←→", "change"), keyhint("esc", "back"),
		}, helpStyle.Render("  ·  ")),
	)
	return renderPanel(rows, innerW, m.width, h)
}

// renderPalette draws the command-palette overlay: a titled box holding the
// fuzzy query input and the filtered command rows, each with its live keybinding
// right-aligned in a faint style. The row list is windowed to the available
// height (as the deck lists are) with the selection drawn as a full-width bar.
func (m Model) renderPalette(h int) string {
	innerW := panelInnerWidth(m.width, 72)
	cmds := m.filteredPaletteCommands()

	// header, blank, input, blank up top; blank + footer legend at the bottom.
	lines := []string{header("commands", -1), "", "  " + m.paletteInput.View(), ""}
	rowsAvail := max(1, h-8)
	if len(cmds) == 0 {
		lines = append(lines, dimStyle.Render("  no matching commands"))
	} else {
		start, end := windowBounds(len(cmds), m.paletteCursor, rowsAvail)
		for i := start; i < end; i++ {
			lines = append(lines, m.paletteLine(cmds[i], i == m.paletteCursor, innerW))
		}
	}

	lines = append(lines, "", "  "+strings.Join([]string{
		keyhint("↑↓", "move"), keyhint("enter", "run"), keyhint("esc", "close"),
	}, helpStyle.Render("  ·  ")))

	return renderPanel(lines, innerW, m.width, h)
}

// paletteLine renders one command row: the verb-phrase title on the left and the
// live keybinding hint right-aligned in a faint style, drawn as a full-width
// selection bar when it is the cursor row. Mirrors boardAgentLine's layout.
func (m Model) paletteLine(c paletteCmd, selected bool, innerW int) string {
	left := "  " + nameStyle.Render(c.title)
	content := left
	if c.hint != "" {
		right := helpStyle.Render(c.hint)
		gap := max(2, innerW-lipgloss.Width(left)-lipgloss.Width(right))
		content = left + strings.Repeat(" ", gap) + right
	}
	if selected {
		return selBar(content, innerW)
	}
	return content
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
	innerW := panelInnerWidth(m.width, 56)
	rows := []string{header("usage", -1), ""}
	for i, provider := range m.agentProviders {
		if i > 0 {
			rows = append(rows, "")
		}
		rows = append(rows, repoHdrStyle.Render("  "+providerName(provider)))
		snap, have, hist := m.usageState(provider)
		if !have {
			rows = append(rows, dimStyle.Render("  no usage data"))
			continue
		}
		windowRows := usageWindowRows(snap)
		if len(windowRows) == 0 {
			rows = append(rows, dimStyle.Render("  no usage data"))
			continue
		}
		rows = append(rows, windowRows...)
		rows = append(rows, usageBurnRows(snap, hist)...)
	}
	rows = append(rows, "", "  "+keyhint("esc", "close"))
	return renderPanel(rows, innerW, m.width, h)
}

// usageWindowRows renders a gauge line (and an indented reset line) per
// present window; absent windows are skipped outright — a placeholder row
// would imply a limit the plan doesn't have.
func usageWindowRows(snap usage.Snapshot) []string {
	var rows []string
	add := func(named usage.NamedWindow) {
		label, w, session := named.Label, named.Window, named.Session
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
	for _, named := range snap.Windows() {
		add(named)
	}
	return rows
}

// usageBurnRows estimates the five-hour window's burn rate from the sample
// ring: a sparkline of the between-poll deltas, the hourly rate, and — when
// the window would cap before it resets — the projected time it caps. Hidden
// until the ring spans usageBurnMinSpan with a rising trend, so a flat or
// draining window never shows a scary extrapolation, and the projection line
// is dropped when the reset lands first (the window frees up before it caps).
func usageBurnRows(snap usage.Snapshot, hist []usageSample) []string {
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
	if w := sessionUsageWindow(snap); w != nil && w.Utilization < 100 {
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

// --- diff viewer ---

// diffLineCap bounds a scope's pre-rendered line count so a monster diff can't
// blow up memory or the render loop; the excess is replaced by a single dim
// truncation notice.
const diffLineCap = 20000

// renderDiff draws the read-only diff viewer full-body: a one-line header (bold
// repo/worktree, then the dim summary and the active scope label), a faint rule,
// the windowed content lines, and a one-line footer legend. loading and
// genuinely-empty diffs show a centered dim notice instead. Every line is
// width-clamped (ANSI-aware) so long diff lines can't wrap and break the layout.
func (m Model) renderDiff(h int) string {
	d := m.diffView
	if d.loading {
		return centerBlock(dimStyle.Render("loading diff…"), m.width, h)
	}
	sc := d.active()
	if sc.files == 0 {
		var msg string
		switch {
		case d.scopeUncommitted:
			msg = "nothing to show — no uncommitted changes"
		case d.base != "":
			msg = "nothing to show — branch is level with " + d.base + " and clean"
		default:
			msg = "nothing to show — clean"
		}
		return centerBlock(dimStyle.Render(msg), m.width, h)
	}

	scopeLabel := "[all]"
	if d.scopeUncommitted {
		scopeLabel = "[uncommitted]"
	}
	summary := fmt.Sprintf("↑%d · %d files · +%d −%d", d.ahead, sc.files, sc.add, sc.del)
	header := "  " + repoHdrStyle.Render(d.repoName+"/"+d.wtName) + "  " +
		dimStyle.Render(summary) + " " + helpKeyStyle.Render(scopeLabel)
	rule := diffRuleStyle.Render(strings.Repeat("─", max(1, m.width)))

	avail := max(1, h-diffChromeRows)
	start := clamp(d.offset, 0, max(0, len(sc.lines)-1))
	end := min(len(sc.lines), start+avail)

	out := make([]string, 0, avail+diffChromeRows)
	out = append(out, ansi.Truncate(header, m.width, ""), rule)
	for i := start; i < end; i++ {
		out = append(out, ansi.Truncate(sc.lines[i], m.width, ""))
	}
	for i := end - start; i < avail; i++ {
		out = append(out, "") // pad so the footer sits at the bottom edge
	}
	footer := "  " + strings.Join([]string{
		keyhint("↑↓ / j k / ^n ^p", "scroll"), keyhint("J/K", "file"),
		keyhint("u", "scope"), keyhint("x", "remove"), keyhint("esc", "back"),
	}, helpStyle.Render(" · "))
	out = append(out, ansi.Truncate(footer, m.width, ""))
	return strings.Join(out, "\n")
}

// diffBuilder accumulates a scope's styled lines while recording the indices of
// the file-header lines (the `diff --git` rows) for the J/K jumps.
type diffBuilder struct {
	lines     []string
	fileLines []int
}

func (b *diffBuilder) add(line string) { b.lines = append(b.lines, line) }

func (b *diffBuilder) addFile(line string) {
	b.fileLines = append(b.fileLines, len(b.lines))
	b.lines = append(b.lines, line)
}

// buildDiffContent renders both scopes into d from the fetched msg. The full
// scope (the default) shows the vs-base section — present only when the branch
// has a base to compare against — followed by the uncommitted section; the
// uncommitted scope shows just the latter, so `u` narrows the view. Lines are
// pre-styled here once (renderDiff only windows and width-clamps them). width
// sizes the section rules; the diff bodies reflow via the render-time clamp, so
// a later resize only leaves the rules slightly long/short (cosmetic).
func buildDiffContent(d *diffState, msg diffMsg, width int) {
	// Uncommitted section, shared by both scopes.
	var uncB diffBuilder
	uf, ua, ud := appendDiffSection(&uncB, "uncommitted", "",
		msg.uncommittedStat, msg.untracked, msg.uncommittedBody, width)
	d.uncommitted = finishScope(uncB, uf, ua, ud)

	// Full scope: the vs-base section (when a base was available) then the same
	// uncommitted section.
	var fullB diffBuilder
	files, add, del := 0, 0, 0
	if d.base != "" {
		cf, ca, cd := appendDiffSection(&fullB, "vs "+d.base, pluralize(d.ahead, "commit"),
			msg.committedStat, nil, msg.committedBody, width)
		files, add, del = cf, ca, cd
		fullB.add("")
	}
	uf2, ua2, ud2 := appendDiffSection(&fullB, "uncommitted", "",
		msg.uncommittedStat, msg.untracked, msg.uncommittedBody, width)
	files, add, del = files+uf2, add+ua2, del+ud2
	d.full = finishScope(fullB, files, add, del)
}

// appendDiffSection appends one titled section to b — a faint `── <title>
// (<meta>) ──` rule, the file index, a blank line, then the styled unified-diff
// body — and returns the section's file count and +/− totals for the header
// summary. Untracked paths are listed in the index as `?? path  new` (they carry
// no diff body). A section with no files still draws its rule and a dim
// "(nothing)" so the scope is never blank mid-view.
func appendDiffSection(b *diffBuilder, title, meta string, stat []repo.FileStat, untracked []string, body string, width int) (files, add, del int) {
	b.add(diffRule(title, meta, width))
	for _, fs := range stat {
		b.add(diffIndexLine(fs))
		files++
		add += fs.Add
		del += fs.Del
	}
	for _, p := range untracked {
		b.add(dimStyle.Render("  ?? "+p+"  ") + diffAddStyle.Render("new"))
		files++
	}
	if files == 0 {
		b.add(dimStyle.Render("  (nothing)"))
	}
	b.add("")
	appendDiffBody(b, body)
	return files, add, del
}

// appendDiffBody styles a unified-diff body line by line into b: `diff --git`
// rows are bold file headers (and recorded for J/K), `@@` rows are hunk headers,
// `+`/`−` rows are coloured, and context lines pass through plain.
func appendDiffBody(b *diffBuilder, body string) {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "diff --git"):
			b.addFile(diffFileStyle.Render(line))
		case strings.HasPrefix(line, "@@"):
			b.add(diffHunkStyle.Render(line))
		case strings.HasPrefix(line, "+"):
			b.add(diffAddStyle.Render(line))
		case strings.HasPrefix(line, "-"):
			b.add(diffDelStyle.Render(line))
		default:
			b.add(line)
		}
	}
}

// diffIndexLine renders one file's index row: a dim `M` marker, the path, and
// either the +/− line counts or a dim `binary` for files git couldn't diff.
func diffIndexLine(fs repo.FileStat) string {
	left := dimStyle.Render("  M ") + nameStyle.Render(fs.Path)
	if fs.Binary {
		return left + "  " + dimStyle.Render("binary")
	}
	return left + "  " + diffAddStyle.Render("+"+strconv.Itoa(fs.Add)) +
		" " + diffDelStyle.Render("−"+strconv.Itoa(fs.Del))
}

// diffRule builds a section rule `── <title> (<meta>) ─────…` filled to width in
// the faint style; an empty meta drops the parenthetical.
func diffRule(title, meta string, width int) string {
	head := "── " + title
	if meta != "" {
		head += " (" + meta + ")"
	}
	head += " "
	if fill := width - lipgloss.Width(head); fill > 0 {
		head += strings.Repeat("─", fill)
	}
	return diffRuleStyle.Render(head)
}

// finishScope packages a builder into a scopeContent with its summary counts,
// enforcing diffLineCap: lines past the cap are dropped (along with their
// file-header indices) and replaced by a single dim truncation notice.
func finishScope(b diffBuilder, files, add, del int) scopeContent {
	sc := scopeContent{lines: b.lines, fileLines: b.fileLines, files: files, add: add, del: del}
	if len(sc.lines) > diffLineCap {
		more := len(sc.lines) - diffLineCap
		sc.lines = sc.lines[:diffLineCap]
		var fl []int
		for _, i := range sc.fileLines {
			if i < diffLineCap {
				fl = append(fl, i)
			}
		}
		sc.fileLines = fl
		sc.lines = append(sc.lines, dimStyle.Render(fmt.Sprintf("… diff truncated (%d more lines)", more)))
	}
	return sc
}

// pluralize renders "1 commit" / "3 commits" for the section-rule meta.
func pluralize(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
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

// plasmaWidth returns the plasma panel's column count for the current
// terminal size: the configured percent of the width, or 0 when the panel is
// disabled or the terminal is too narrow to split — the repo/worktree lists
// keep priority over ambience.
func (m Model) plasmaWidth() int {
	if m.plasma == nil || m.plasmaWidthPct <= 0 {
		return 0
	}
	w := m.width * m.plasmaWidthPct / 100
	if w < 16 || m.width-w < 48 {
		return 0
	}
	return w
}

// renderDeck draws the picker into h rows beneath the bar: the NEW + ACTIVE
// sections stacked on the left, and (given the room) the ambient plasma panel
// filling a full-height box on the right.
func (m Model) renderDeck(h int) string {
	plasmaW := m.plasmaWidth()
	innerW := m.width - plasmaW - 4 // border (2) + horizontal padding (2)
	footer := m.renderFooter()
	L := m.deckLayout(h)

	newBox := box(m.renderNew(innerW, L.newRows), innerW, L.newContentH, m.focus == focusNew)
	activeBox := box(m.renderActive(innerW, L.activeRows), innerW, L.activeContentH, m.focus == focusActive)
	body := lipgloss.JoinVertical(lipgloss.Left, newBox, activeBox)

	if plasmaW > 0 {
		// Span both left boxes exactly: their outer heights are contentH+2 each.
		ph := L.newContentH + L.activeContentH + 2
		body = lipgloss.JoinHorizontal(lipgloss.Top, body,
			box(m.plasma.Render(plasmaW-4, ph), plasmaW-4, ph, false))
	}

	return lipgloss.JoinVertical(lipgloss.Left, body, footer)
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
// each repo's first worktree, then one row per worktree, plus a "└" detail line
// beneath the focused worktree) alongside a parallel slice mapping each display
// line back to its m.active index (-1 for header and detail lines). Shared by
// renderActive and the click hit-test so their row layout stays identical; the
// detail line's -1 makes it non-navigable, so a click on it misses and the
// windowing centres on the worktree row, not the detail beneath it.
func (m Model) activeDisplay(innerW int) (lines []string, rowItem []int) {
	lastRepo := ""
	for i, it := range m.active {
		if it.repo.Name != lastRepo {
			lines = append(lines, repoHdrStyle.Render(it.repo.Name))
			rowItem = append(rowItem, -1)
			lastRepo = it.repo.Name
		}
		selected := i == m.activeCursor && m.focus == focusActive
		lines = append(lines, m.activeRow(it, selected, innerW))
		rowItem = append(rowItem, i)
		if selected {
			stackSeg := m.stackDetailSeg(wsKey(it.repo.Name, it.view.WT.Name))
			if detail := activeDetail(it.view, stackSeg, innerW); detail != "" {
				lines = append(lines, detail)
				rowItem = append(rowItem, -1)
			}
		}
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

// activeRowPrefixW is the fixed column count the row draws before the worktree
// name: "  " + rank + "   " + live + " " + notif + " " + dirty + " ". Keeping it
// a constant lets the name-truncation and cluster-alignment maths stay in step
// with the format string that lays the glyphs out.
const activeRowPrefixW = 12

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
		notifChar = "✓"
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

	// Right-aligned work-state cluster (↑ahead / ↓behind / —). The name flexes:
	// it is truncated so the cluster keeps its column at the row's right edge, on
	// selected and unselected rows alike.
	cluster, clusterW := stateCluster(it.view)
	// Append the stack glyph (when the cache has one) after the ahead/behind
	// cluster, widening the reserved column so the name math stays in step. With
	// no cache entry the width is 0 and the row is byte-identical to before.
	if gl, glW := m.stackGlyph(key); glW > 0 {
		cluster = cluster + " " + gl
		clusterW += 1 + glW
	}
	// On the focused row, identity outranks optional right-side facts. Drop the
	// cluster before truncating the selected worktree name.
	if highlight && lipgloss.Width(it.view.WT.Name) > innerW-activeRowPrefixW-clusterW-1 {
		cluster, clusterW = "", 0
	}
	nameMax := max(1, innerW-activeRowPrefixW-clusterW-1)
	name := truncateTo(it.view.WT.Name, nameMax)
	gap := max(1, innerW-activeRowPrefixW-lipgloss.Width(name)-clusterW)

	if highlight {
		left := fmt.Sprintf("  %s   %s %s %s %s", rankCh, liveChar, notifChar, dirtyChar, name)
		return selBar(left+strings.Repeat(" ", gap)+cluster, innerW)
	}

	rankCol := " "
	if rank > 0 {
		rankCol = recentStyle.Render(rankCh)
	}
	dirty := " "
	if it.view.Dirty {
		dirty = dirtyStyle.Render(dirtyChar)
	}
	left := "  " + rankCol + "   " + liveSt.Render(liveChar) + " " + notifSt.Render(notifChar) + " " + dirty + " " + nameStyle.Render(name)
	return left + strings.Repeat(" ", gap) + cluster
}

// stateCluster renders a worktree's right-aligned work-state indicator: "↑N"
// (green) when the branch is ahead of main, "↓M" (yellow) when behind, both
// (space-separated) when it has diverged in both directions, and a dim "—" when
// there's nothing to show — no base branch to compare against, or level with it.
// Returns the styled string plus its display width so the row can right-align it.
func stateCluster(v WorktreeView) (string, int) {
	if !v.HasBase || (v.Ahead == 0 && v.Behind == 0) {
		return dimStyle.Render("—"), 1
	}
	var parts []string
	plain := ""
	if v.Ahead > 0 {
		s := "↑" + strconv.Itoa(v.Ahead)
		parts = append(parts, aheadStyle.Render(s))
		plain += s
	}
	if v.Behind > 0 {
		s := "↓" + strconv.Itoa(v.Behind)
		parts = append(parts, behindStyle.Render(s))
		if plain != "" {
			plain += " "
		}
		plain += s
	}
	return strings.Join(parts, " "), lipgloss.Width(plain)
}

// activeDetail builds the dim "└" line shown directly beneath the focused
// worktree row. Branch divergence already lives in the row's compact ↑N/↓N
// cluster, so the detail line only reveals new context: uncommitted diffstat,
// the last-commit subject (in quotes), its age, and stack state. Each segment is
// omitted when it doesn't apply and joined by " · ". The subject is the segment
// that flexes: it is truncated so the whole line fits innerW. Returns "" when
// every segment is empty, so activeDisplay drops the line entirely.
func activeDetail(v WorktreeView, stackSeg string, innerW int) string {
	const prefix = "      └ "

	// head holds the segments left of the subject (diffstat); tail
	// holds those to its right (age). The subject slots between them.
	var head, tail []string
	if v.Dirty && v.Add+v.Del > 0 {
		head = append(head, fmt.Sprintf("+%d −%d uncommitted", v.Add, v.Del))
	}
	if v.CommitTime > 0 {
		tail = append(tail, humanDur(time.Since(time.Unix(v.CommitTime, 0))))
	}
	// The stack segment (when the cache has one) sits right of the subject with
	// the age; the subject budget below already accounts for the whole tail, and
	// the final truncate keeps it graceful on narrow widths.
	if stackSeg != "" {
		tail = append(tail, stackSeg)
	}

	subject := ""
	if v.Subject != "" {
		// Budget the subject against everything else already claiming the line:
		// the prefix, the fixed segments, the separators joining the subject in,
		// and the two quote marks around it.
		used := lipgloss.Width(prefix)
		if fixed := append(append([]string{}, head...), tail...); len(fixed) > 0 {
			used += lipgloss.Width(strings.Join(fixed, " · "))
		}
		seps := 0
		if len(head) > 0 {
			seps++
		}
		if len(tail) > 0 {
			seps++
		}
		used += seps * lipgloss.Width(" · ")
		if budget := innerW - used - 2; budget >= 1 {
			subject = `"` + truncateTo(v.Subject, budget) + `"`
		}
	}

	segs := append([]string{}, head...)
	if subject != "" {
		segs = append(segs, subject)
	}
	segs = append(segs, tail...)
	if len(segs) == 0 {
		return ""
	}
	// A final truncate guards the narrow-width case where even the budgeted line
	// overshoots (e.g. the subject was dropped but head+tail still overflow).
	return dimStyle.Render(truncateTo(prefix+strings.Join(segs, " · "), innerW))
}

// renderConfirm draws the centered panel that replaces the old status-line
// prompts for the three destructive picker modes. It reuses the board/help
// overlays' visual language: a pink header title, the pre-styled context lines,
// a blank spacer, then one arrow-selectable row per option — the cursor row
// gets the selection bar, danger rows render red, and every row shows its
// mnemonic key right-aligned and dim. A footer legend advertises the keys.
func (m Model) renderConfirm(h int) string {
	c := m.confirm
	innerW := panelInnerWidth(m.width, 56)

	lines := []string{header(c.title, -1), ""}
	for _, ctx := range c.context {
		lines = append(lines, "  "+ctx)
	}
	lines = append(lines, "")
	for i, opt := range c.options {
		lines = append(lines, m.confirmOptionLine(opt, i == c.cursor, innerW))
	}
	lines = append(lines, "", "  "+strings.Join([]string{
		keyhint("↑↓", "move"), keyhint("enter", "confirm"), keyhint("esc", "cancel"),
	}, helpStyle.Render("  ·  ")))

	return renderPanel(lines, innerW, m.width, h)
}

// confirmOptionLine renders one option row: the label on the left and its
// mnemonic key right-aligned dim on the right. Danger rows paint both red; the
// cursor row gets the full-width selection bar (matching the board's rows).
func (m Model) confirmOptionLine(opt confirmOption, selected bool, innerW int) string {
	labelSt, keySt := nameStyle, dimStyle
	if opt.danger {
		labelSt, keySt = errStyle, errStyle
	}
	left := "  " + labelSt.Render(opt.label)
	right := keySt.Render(opt.key)
	gap := max(2, innerW-lipgloss.Width(left)-lipgloss.Width(right)-2)
	row := left + strings.Repeat(" ", gap) + right + "  "
	if selected {
		return selBar(row, innerW)
	}
	return row
}

// removeConfirmContext builds the context lines shown atop the remove-worktree
// panel: the repo / worktree identity (bright), a one-line summary of the
// branch's divergence from its base and its uncommitted diffstat (reusing the
// deck detail line's data sources — Ahead/Behind and the vs-HEAD Add/Del), and
// — only when the tree is dirty — a red warning that the uncommitted work will
// be lost. Pieces that aren't available are omitted rather than triggering a
// fresh git call to fill them in.
func removeConfirmContext(it activeItem) []string {
	v := it.view
	lines := []string{repoHdrStyle.Render(it.repo.Name + " / " + v.WT.Name)}

	var summary []string
	if v.HasBase && (v.Ahead > 0 || v.Behind > 0) {
		summary = append(summary, divergencePhrase(v))
	}
	if v.Dirty && v.Add+v.Del > 0 {
		summary = append(summary, fmt.Sprintf("+%d −%d uncommitted", v.Add, v.Del))
	}
	if len(summary) > 0 {
		lines = append(lines, dimStyle.Render(strings.Join(summary, " · ")))
	}
	if v.Dirty {
		lines = append(lines, errStyle.Render("✷ uncommitted changes will be lost"))
	}
	return lines
}

// divergencePhrase spells out a branch's divergence from its base for the
// remove panel, e.g. "↑23 ahead of main" — naming the base when known and
// falling back to the bare count otherwise. The caller gates it on HasBase and
// a non-zero divergence.
func divergencePhrase(v WorktreeView) string {
	switch {
	case v.Ahead > 0 && v.Behind > 0:
		return fmt.Sprintf("↑%d ahead, ↓%d behind", v.Ahead, v.Behind)
	case v.Ahead > 0:
		if v.BaseBranch != "" {
			return fmt.Sprintf("↑%d ahead of %s", v.Ahead, v.BaseBranch)
		}
		return fmt.Sprintf("↑%d ahead", v.Ahead)
	default:
		if v.BaseBranch != "" {
			return fmt.Sprintf("↓%d behind %s", v.Behind, v.BaseBranch)
		}
		return fmt.Sprintf("↓%d behind", v.Behind)
	}
}

// renderHelp draws the key + legend overlay, centered in the body area. The
// session bindings are read from the model so the overlay can never drift from
// the real (configurable) keys.
func (m Model) renderHelp(h int) string {
	innerW := panelInnerWidth(m.width, 72)

	row := func(key, desc string) string {
		k := padLine(key, 12)
		if lipgloss.Width(key) > 12 {
			k = key + " " // guarantee a gap when the key column overflows
		}
		return "  " + helpKeyStyle.Render(k) + helpStyle.Render(desc)
	}

	rows := []string{header("help", -1), ""}
	rows = append(rows,
		repoHdrStyle.Render("  Deck"),
		row("↑↓ ^p / j k", "move"),
		row("tab", "switch section"),
		row("enter", "open / create"),
		row("1 2 3", "open recent worktree"),
		row("d", "stop worktree"),
		row("v", "view diff (deck)"),
		row("s", "stack screen (↑↓ / j k / ^n ^p move · s submit · R restack · v diff · o open PR)"),
		row("x", "remove worktree (b keeps branch)"),
		row("r", "refresh"),
		row("ctrl+c", "quit"),
		"",
		repoHdrStyle.Render("  Session"),
		row(m.keys.Cycle+" / "+m.keys.CycleBack, "cycle view (next / prev)"),
		row(m.keys.GotoEditor+" "+m.keys.GotoAgent+" "+m.keys.GotoTerm, "go to editor / agent / term"),
		row(m.keys.Picker, "back to the deck"),
		row(m.keys.Back, "return to previous location"),
		row(m.keys.GlobalConfig, "open home workspace (~)"),
		row(m.keys.Palette, "agent board"),
		row(m.keys.CommandPalette, "command palette (every action)"),
	)
	rows = append(rows,
		row(m.keys.Attention, "jump to agent needing attention"),
		row(m.keys.PrevAgent+" / "+m.keys.NextAgent, "prev / next agent"),
	)
	if m.usageEnabled() {
		rows = append(rows, row(m.keys.Usage, "usage limits"))
	}
	rows = append(rows,
		"",
		repoHdrStyle.Render("  Terminal panes"),
		row(m.keys.TermSplitV, "vertical split"),
		row(m.keys.TermSplitH, "horizontal split"),
		row(m.keys.TermFocusLeft+" "+m.keys.TermFocusDown+" "+m.keys.TermFocusUp+" "+m.keys.TermFocusRight, "focus left / down / up / right"),
		row(m.keys.TermZoom, "zoom / restore pane"),
		row(m.keys.TermClose, "close pane"),
	)
	rows = append(rows,
		"",
		repoHdrStyle.Render("  Navigation"),
		"  "+m.navigationLegend(),
		"",
		repoHdrStyle.Render("  Legend"),
		"  "+statusLegend(),
		"  "+markLegend(),
		"  "+stackLegend(),
		"",
		"  "+strings.Join([]string{
			keyhint("j/k", "scroll"), keyhint("esc", "close"),
			helpStyle.Render("toggle ") + helpKeyStyle.Render(m.keys.Help),
		}, helpStyle.Render(" · ")),
	)

	// Help is the one panel whose complete content routinely exceeds a normal
	// terminal. Window its middle while keeping title and navigation anchored.
	footer := rows[len(rows)-1]
	body := rows[2 : len(rows)-2]
	visibleH := max(1, h-6) // borders + title/blank + blank/footer
	maxOffset := max(0, len(body)-visibleH)
	start := clamp(m.helpOffset, 0, maxOffset)
	end := min(len(body), start+visibleH)
	visible := []string{rows[0], ""}
	visible = append(visible, body[start:end]...)
	visible = append(visible, "", footer)
	return renderPanel(visible, innerW, m.width, h)
}

func (m Model) navigationLegend() string {
	parts := []string{
		m.destinationGlyph(screenPicker) + " deck",
		m.destinationGlyph(screenEditor) + " editor",
		m.destinationGlyph(screenAgent) + " agent",
		m.destinationGlyph(screenTerminal) + " terminal",
	}
	return helpStyle.Render(strings.Join(parts, " · "))
}

// statusLegend / markLegend explain the deck's status glyphs, split across two
// lines so they stay within the overlay's width.
func statusLegend() string {
	return strings.Join([]string{
		liveStyle.Render("●") + helpStyle.Render(" live"),
		dimStyle.Render("○") + helpStyle.Render(" stopped"),
		lipgloss.NewStyle().Foreground(cRed).Render("!") + helpStyle.Render(" waiting"),
		lipgloss.NewStyle().Foreground(cGreen).Render("✓") + helpStyle.Render(" new output"),
	}, helpStyle.Render("   "))
}

func markLegend() string {
	return strings.Join([]string{
		dirtyStyle.Render("✷") + helpStyle.Render(" uncommitted"),
		recentStyle.Render("1 2 3") + helpStyle.Render(" recently opened"),
	}, helpStyle.Render("   "))
}

// stackLegend explains the deck's per-worktree stack glyphs (drawn after the
// ahead/behind cluster) shown once cached `ct stack status` data is available.
func stackLegend() string {
	return strings.Join([]string{
		aheadStyle.Render(stackGlyphReady) + helpStyle.Render(" passing"),
		stackWaitStyle.Render(stackGlyphPending) + helpStyle.Render(" pending"),
		errStyle.Render(stackGlyphAttention) + helpStyle.Render(" attention"),
		stackWaitStyle.Render(stackGlyphRestack) + helpStyle.Render(" restack"),
		dimStyle.Render(stackGlyphInactive) + helpStyle.Render(" inactive/draft"),
	}, helpStyle.Render("   "))
}

// renderSetup draws the first-run setup overlay centered in the body area.
func (m Model) renderSetup(h int) string {
	innerW := panelInnerWidth(m.width, 60)

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

	return renderPanel(rows, innerW, m.width, h)
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

// panelInnerWidth keeps a rounded, horizontally padded panel inside the
// viewport. The frame consumes four columns: two borders and two padding cells.
func panelInnerWidth(viewport, preferred int) int {
	if viewport <= 0 {
		return preferred
	}
	return max(1, min(preferred, viewport-4))
}

// renderPanel fits a centered panel to the available body. On short terminals,
// stable orientation (title) and the action footer survive while middle detail
// yields, with an explicit omission marker rather than clipped borders.
func renderPanel(rows []string, innerW, viewportW, viewportH int) string {
	contentH := max(1, viewportH-2) // rounded top and bottom borders
	fitted := rows
	if len(fitted) > contentH {
		switch contentH {
		case 1:
			fitted = rows[:1]
		case 2:
			fitted = []string{rows[0], rows[len(rows)-1]}
		default:
			middle := contentH - 2
			fitted = append([]string{rows[0]}, rows[1:1+middle]...)
			if middle > 0 {
				fitted[len(fitted)-1] = dimStyle.Render("  …")
			}
			fitted = append(fitted, rows[len(rows)-1])
		}
	}
	for i := range fitted {
		fitted[i] = ansi.Truncate(fitted[i], innerW, "…")
	}
	return centerBlock(box(fitted, innerW, len(fitted), true), viewportW, viewportH)
}

func (m Model) renderFooter() string {
	return m.centerFooter(m.footerContent())
}

// sessionFooterH is the number of rows reserved beneath a session body for the
// two-line help hint: two until the user's first keystroke into a session,
// zero after (see hintSeen). It is intentionally screen-independent so the
// reserved size is stable across every session view — sessionSize is queried
// before the target screen is even set.
func (m Model) sessionFooterH() int {
	if m.hintSeen {
		return 0
	}
	return 2
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

// sessionFooter builds the dim two-line hint shown beneath a session until the
// user first types. The help key gets its own row; the second row surfaces
// navigation or pane-management keys. Trailing hints are dropped so each row
// stays within the viewport.
func (m Model) sessionFooter() string {
	first := "  " + keyhint(m.keys.Help, "help")
	var hints []string
	if m.screen == screenTerminal {
		hints = []string{
			keyhint(m.keys.TermSplitV+" "+m.keys.TermSplitH, "split"),
			keyhint(m.keys.TermZoom, "zoom"),
			keyhint(m.keys.TermClose, "close")}
	} else {
		hints = []string{
			keyhint(m.keys.Cycle, "cycle view"),
			keyhint(m.keys.Picker, "deck")}
	}
	if m.returnLocation != nil {
		hints = append([]string{keyhint(m.keys.Back, "return")}, hints...)
	}
	sep := helpStyle.Render("  ·  ")
	for n := len(hints); n >= 1; n-- {
		second := "  " + strings.Join(hints[:n], sep)
		if lipgloss.Width(second) <= m.width {
			return first + "\n" + second
		}
	}
	return first + "\n"
}

// footerContent builds the two-row footer (status line + help line) before
// centering. The destructive-confirm modes never reach here — they replace the
// whole deck body with their own panel (see View) — so only the create form
// needs its own status-styled footer.
func (m Model) footerContent() string {
	switch m.mode {
	case modeCreateName:
		// A rejected name (client-side validation) sets a sticky error status;
		// style it red here so the hint stands out from ordinary form chrome.
		if m.statusLevel == statusError {
			return "\n" + errStyle.Render(m.status)
		}
		return "\n" + helpStyle.Render(m.status)
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
			keyhint("1-3", "recent"), keyhint("s", "stack"), keyhint("d", "stop"), keyhint("x", "remove"),
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
		ln = ansi.Truncate(ln, m.width, "")
		if pad := (m.width - lipgloss.Width(ln)) / 2; pad > 0 {
			ln = strings.Repeat(" ", pad) + ln
		}
		lines[i] = ln
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

// selBar renders text as a solid full-width selection bar. Nested styles in a
// row (provider, label, status) emit full ANSI resets, so reapply the selection
// style after each one; otherwise the background stops at the first styled
// segment instead of continuing through the title and status columns.
func selBar(text string, innerW int) string {
	text = strings.ReplaceAll(padLine(text, innerW), ansi.ResetStyle, ansi.ResetStyle+selANSI)
	return selStyle.Render(text)
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
