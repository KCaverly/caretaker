package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/KCaverly/caretaker/internal/config"
	"github.com/KCaverly/caretaker/internal/repo"
	"github.com/KCaverly/caretaker/internal/session"
)

// plasmaModel builds a deck model with the plasma panel enabled at 40% and
// the same sample groups as sampleModel, sized w×h.
func plasmaModel(t *testing.T, w, h int) Model {
	t.Helper()
	ctrl := &Controller{cfg: config.Config{Plasma: config.Plasma{
		Pattern: "classic", Palette: "aurora", Charset: "dots", Speed: 0.3, Width: 40,
	}}}
	m := New(ctrl, session.NewManager())
	mm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = mm.(Model)
	m.groups = []Group{
		{Repo: repo.Repo{Name: "caretaker"}, Worktrees: []WorktreeView{
			{WT: repo.Worktree{Repo: "caretaker", Name: "(main)", Branch: "main", IsMain: true}},
			{WT: repo.Worktree{Repo: "caretaker", Name: "feat-login", Branch: "feat-login"}, Live: true},
			{WT: repo.Worktree{Repo: "caretaker", Name: "bugfix", Branch: "bugfix"}},
		}},
	}
	m.groupsLoaded = true
	m.recomputeMatches()
	m.recomputeActive()
	return m
}

func TestDeckPlasmaPanelLayout(t *testing.T) {
	m := plasmaModel(t, 100, 30)
	if got := m.plasmaWidth(); got != 40 {
		t.Fatalf("plasmaWidth at 100 cols = %d, want 40", got)
	}

	// Three box frames: NEW and ACTIVE on the left, the panel on the right —
	// and on screen the NEW and panel frames share the top row.
	if deck := m.renderDeck(m.height - barHeight); strings.Count(deck, "╭") != 3 {
		t.Errorf("wide deck should draw three boxes:\n%s", deck)
	}
	screen := screenText(t, m)
	top := strings.Split(screen, "\n")[barHeight]
	if strings.Count(top, "╭") != 2 || strings.Count(top, "╮") != 2 {
		t.Errorf("deck top row should show two box frames side by side, got:\n%s", top)
	}

	// Too narrow to split: the panel yields the full width to the lists.
	m = plasmaModel(t, 70, 24)
	if got := m.plasmaWidth(); got != 0 {
		t.Fatalf("plasmaWidth at 70 cols = %d, want 0 (lists keep priority)", got)
	}
	if deck := m.renderDeck(m.height - barHeight); strings.Count(deck, "╭") != 2 {
		t.Errorf("narrow deck should draw only the two list boxes:\n%s", deck)
	}
}

func TestDeckPlasmaPanelClicksSelectNothing(t *testing.T) {
	m := plasmaModel(t, 100, 30)
	L := m.deckLayout(m.height - barHeight)
	// First worktree row in ACTIVE: content lines run header, blank, then the
	// display window, whose first row is the repo header — the worktree sits
	// one below it.
	y := barHeight + L.newOuterH + 1 + 3

	// A click on that row inside the left column selects the worktree…
	mm, _, handled := m.deckClick(4, y)
	if !handled || mm.(Model).focus != focusActive {
		t.Fatalf("left-column click should select the worktree row (handled=%v)", handled)
	}
	// …but the same row inside the plasma panel does nothing.
	if _, _, handled = m.deckClick(70, y); handled {
		t.Fatal("clicks on the plasma panel must not hit list rows")
	}
}

func TestPlasmaTickLifecycle(t *testing.T) {
	// The first Update on the deck arms the tick.
	m := plasmaModel(t, 100, 30)
	if !m.plasmaTicking {
		t.Fatal("plasma tick should arm once the deck is visible")
	}

	// On the deck a tick advances the field and re-arms.
	mm, cmd := m.update(plasmaTickMsg{})
	m = mm.(Model)
	if cmd == nil {
		t.Fatal("on-deck plasma tick should schedule the next frame")
	}

	// Off the deck the tick goes dormant instead of re-arming.
	m.screen = screenAgent
	mm, cmd = m.update(plasmaTickMsg{})
	m = mm.(Model)
	if cmd != nil || m.plasmaTicking {
		t.Fatalf("off-deck plasma tick should disarm (cmd=%v ticking=%v)", cmd, m.plasmaTicking)
	}
}
