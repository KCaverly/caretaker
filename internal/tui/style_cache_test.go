package tui

import (
	"testing"

	"charm.land/lipgloss/v2"
)

// TestCachedStylesMatchInline pins the per-frame style hoist to byte identity:
// each package-level cached style must render exactly what the pre-change
// inline lipgloss.NewStyle()… expression produced. This is the before/after
// golden for the style caching — if a reorder or a dropped attribute ever
// changed the emitted ANSI, the rendered bytes here would diverge.
func TestCachedStylesMatchInline(t *testing.T) {
	cases := []struct {
		name   string
		cached lipgloss.Style
		inline lipgloss.Style
	}{
		// Bar tab glyphs (barZones): lit accents + shared dim/faint.
		{"boldDim", boldDim, lipgloss.NewStyle().Bold(true).Foreground(cDim)},
		{"boldFaint", boldFaint, lipgloss.NewStyle().Bold(true).Foreground(cFaint)},
		{"boldGreen(tab)", boldGreen, lipgloss.NewStyle().Bold(true).Foreground(cGreen)},
		{"boldPurple", boldPurple, lipgloss.NewStyle().Bold(true).Foreground(cPurple)},
		{"boldAccent", boldAccent, lipgloss.NewStyle().Bold(true).Foreground(cAccent)},
		{"boldYellow", boldYellow, lipgloss.NewStyle().Bold(true).Foreground(cYellow)},
		// Attention markers (renderNotifZone / boardAgentLine / activeRow) built
		// as Foreground(...).Bold(true) inline; caching flips the setter order,
		// which must not change output.
		{"boldRed", boldRed, lipgloss.NewStyle().Foreground(cRed).Bold(true)},
		{"boldGreen(notif)", boldGreen, lipgloss.NewStyle().Foreground(cGreen).Bold(true)},
		// barContextLabel volatile segments and the accent split divider.
		{"accentStyle", accentStyle, lipgloss.NewStyle().Foreground(cAccent)},
		{"purpleStyle", purpleStyle, lipgloss.NewStyle().Foreground(cPurple)},
		{"dimStyle(context)", dimStyle, lipgloss.NewStyle().Foreground(cDim)},
		// Faint split divider reuses barSep (a faint foreground).
		{"barSep(divider)", barSep, lipgloss.NewStyle().Foreground(cFaint)},
		// box() frames.
		{"boxStyleFaint", boxStyleFaint,
			lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cFaint).Padding(0, 1)},
		{"boxStyleFocused", boxStyleFocused,
			lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cAccent).Padding(0, 1)},
	}
	for _, c := range cases {
		if got, want := c.cached.Render("Xy"), c.inline.Render("Xy"); got != want {
			t.Errorf("%s: cached %q != inline %q", c.name, got, want)
		}
	}

	// dividerStyle must select the accent style for the focused-adjacent colour
	// and the faint style otherwise — the two colours paneAdjacentColor returns.
	if got, want := dividerStyle(cAccent).Render("│"), accentStyle.Render("│"); got != want {
		t.Errorf("dividerStyle(cAccent) = %q, want %q", got, want)
	}
	if got, want := dividerStyle(cFaint).Render("│"), lipgloss.NewStyle().Foreground(cFaint).Render("│"); got != want {
		t.Errorf("dividerStyle(cFaint) = %q, want %q", got, want)
	}
}
