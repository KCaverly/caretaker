// Package tui contains the Bubble Tea deck that powers the ct command.
package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/sahilm/fuzzy"

	"github.com/KCaverly/caretaker/internal/repo"
)

type focus int

const (
	focusNew focus = iota
	focusActive
)

type mode int

const (
	modeNormal mode = iota
	modeCreateName
	modeConfirmRemove
)

// activeItem is a worktree shown in the "active" section, with its owning repo.
type activeItem struct {
	repo repo.Repo
	view WorktreeView
}

// Model is the deck: a "new" repo finder on top and an "active" worktree
// navigator below.
type Model struct {
	ctrl   *Controller
	groups []Group

	focus focus
	mode  mode

	// "new" section
	filter      textinput.Model
	repoMatches []repo.Repo
	newCursor   int

	// create flow
	nameInput   textinput.Model
	pendingRepo repo.Repo

	// "active" section
	active       []activeItem
	activeCursor int

	status        string
	width, height int
}

// New builds the deck model.
func New(ctrl *Controller) Model {
	filter := textinput.New()
	filter.Placeholder = "filter repos…"
	filter.Prompt = "› "
	filter.Focus()

	name := textinput.New()
	name.Placeholder = "branch-name"
	name.Prompt = "› "

	return Model{ctrl: ctrl, filter: filter, nameInput: name, focus: focusNew}
}

// --- messages ---

type loadedMsg struct {
	groups []Group
	err    error
}

type createdMsg struct {
	wt  repo.Worktree
	err error
}

type ensuredMsg struct {
	wt  repo.Worktree
	err error
}

type attachedMsg struct{ err error }

type actionDoneMsg struct{ err error }

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.loadCmd(), textinput.Blink)
}

func (m Model) loadCmd() tea.Cmd {
	return func() tea.Msg {
		g, err := m.ctrl.Load()
		return loadedMsg{groups: g, err: err}
	}
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// Size the inputs so their placeholders render fully (the placeholder
		// buffer is sized to the input width).
		inputW := max(10, m.width-12)
		m.filter.SetWidth(inputW)
		m.nameInput.SetWidth(inputW)
		return m, nil

	case loadedMsg:
		if msg.err != nil {
			m.status = "load error: " + msg.err.Error()
			return m, nil
		}
		m.groups = msg.groups
		m.recomputeMatches()
		m.recomputeActive()
		return m, nil

	case createdMsg:
		if msg.err != nil {
			m.status = "create error: " + msg.err.Error()
			return m, m.loadCmd()
		}
		// Open the freshly created worktree.
		wt := msg.wt
		m.status = "opening " + wt.Name + "…"
		return m, func() tea.Msg { return ensuredMsg{wt: wt, err: m.ctrl.Ensure(wt)} }

	case ensuredMsg:
		if msg.err != nil {
			m.status = "open error: " + msg.err.Error()
			return m, m.loadCmd()
		}
		cmd, err := m.ctrl.AttachCmd(msg.wt)
		if err != nil {
			m.status = "attach error: " + err.Error()
			return m, nil
		}
		return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return attachedMsg{err} })

	case attachedMsg:
		if msg.err != nil {
			m.status = "session ended: " + msg.err.Error()
		}
		return m, m.loadCmd()

	case actionDoneMsg:
		if msg.err != nil {
			m.status = msg.err.Error()
		}
		return m, m.loadCmd()

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeCreateName:
		return m.handleCreateKey(msg)
	case modeConfirmRemove:
		return m.handleConfirmKey(msg)
	}

	// Normal mode.
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	if msg.String() == "tab" {
		return m.toggleFocus()
	}
	if m.focus == focusNew {
		return m.handleNewKey(msg)
	}
	return m.handleActiveKey(msg)
}

func (m Model) toggleFocus() (tea.Model, tea.Cmd) {
	if m.focus == focusNew {
		m.focus = focusActive
		m.filter.Blur()
		return m, nil
	}
	m.focus = focusNew
	return m, m.filter.Focus()
}

func (m Model) handleNewKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "ctrl+p":
		if m.newCursor > 0 {
			m.newCursor--
		}
		return m, nil
	case "down", "ctrl+n":
		if m.newCursor < len(m.repoMatches)-1 {
			m.newCursor++
		}
		return m, nil
	case "enter":
		if m.newCursor >= 0 && m.newCursor < len(m.repoMatches) {
			m.pendingRepo = m.repoMatches[m.newCursor]
			m.mode = modeCreateName
			m.filter.Blur()
			m.nameInput.SetValue("")
			m.status = "new worktree in " + m.pendingRepo.Name
			return m, m.nameInput.Focus()
		}
		return m, nil
	case "esc":
		m.filter.SetValue("")
		m.recomputeMatches()
		return m, nil
	}

	var cmd tea.Cmd
	m.filter, cmd = m.filter.Update(msg)
	m.recomputeMatches()
	return m, cmd
}

func (m Model) handleActiveKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, tea.Quit
	case "up", "k":
		if m.activeCursor > 0 {
			m.activeCursor--
		}
	case "down", "j":
		if m.activeCursor < len(m.active)-1 {
			m.activeCursor++
		}
	case "r":
		m.status = ""
		return m, m.loadCmd()
	case "enter":
		if it, ok := m.selectedActive(); ok {
			wt := it.view.WT
			m.status = "opening " + wt.Name + "…"
			return m, func() tea.Msg { return ensuredMsg{wt: wt, err: m.ctrl.Ensure(wt)} }
		}
	case "a":
		if it, ok := m.selectedActive(); ok {
			wt := it.view.WT
			return m, func() tea.Msg { return actionDoneMsg{err: m.ctrl.AddAgent(wt)} }
		}
	case "t":
		if it, ok := m.selectedActive(); ok {
			wt := it.view.WT
			return m, func() tea.Msg { return actionDoneMsg{err: m.ctrl.AddTerminal(wt)} }
		}
	case "d":
		if it, ok := m.selectedActive(); ok {
			wt := it.view.WT
			m.status = "archiving " + wt.Name + "…"
			return m, func() tea.Msg { return actionDoneMsg{err: m.ctrl.Archive(wt)} }
		}
	case "x":
		if it, ok := m.selectedActive(); ok {
			m.mode = modeConfirmRemove
			m.status = fmt.Sprintf("remove worktree %q and its branch? (y/n)", it.view.WT.Name)
		}
	}
	return m, nil
}

func (m Model) handleCreateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.nameInput.Blur()
		m.status = ""
		return m, m.filter.Focus()
	case "enter":
		name := strings.TrimSpace(m.nameInput.Value())
		m.mode = modeNormal
		m.nameInput.Blur()
		r := m.pendingRepo
		focusCmd := m.filter.Focus()
		if name == "" {
			m.status = ""
			return m, focusCmd
		}
		m.status = "creating " + name + "…"
		return m, tea.Batch(focusCmd, func() tea.Msg {
			wt, err := m.ctrl.Create(r, name, "")
			return createdMsg{wt: wt, err: err}
		})
	}
	var cmd tea.Cmd
	m.nameInput, cmd = m.nameInput.Update(msg)
	return m, cmd
}

func (m Model) handleConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "y" {
		it, ok := m.selectedActive()
		m.mode = modeNormal
		if !ok {
			return m, nil
		}
		r, wt := it.repo, it.view.WT
		m.status = "removing " + wt.Name + "…"
		return m, func() tea.Msg { return actionDoneMsg{err: m.ctrl.Remove(r, wt)} }
	}
	m.mode = modeNormal
	m.status = ""
	return m, nil
}

// --- derived state ---

func (m *Model) recomputeMatches() {
	repos := make([]repo.Repo, 0, len(m.groups))
	names := make([]string, 0, len(m.groups))
	for _, g := range m.groups {
		repos = append(repos, g.Repo)
		names = append(names, g.Repo.Name)
	}

	q := strings.TrimSpace(m.filter.Value())
	if q == "" {
		m.repoMatches = repos
	} else {
		res := fuzzy.Find(q, names)
		m.repoMatches = make([]repo.Repo, 0, len(res))
		for _, mt := range res {
			m.repoMatches = append(m.repoMatches, repos[mt.Index])
		}
	}
	m.newCursor = clamp(m.newCursor, 0, max(0, len(m.repoMatches)-1))
}

func (m *Model) recomputeActive() {
	var items []activeItem
	for _, g := range m.groups {
		for _, wv := range g.Worktrees {
			if wv.WT.IsMain {
				continue
			}
			items = append(items, activeItem{repo: g.Repo, view: wv})
		}
	}
	m.active = items
	m.activeCursor = clamp(m.activeCursor, 0, max(0, len(items)-1))
}

func (m Model) selectedActive() (activeItem, bool) {
	if m.activeCursor < 0 || m.activeCursor >= len(m.active) {
		return activeItem{}, false
	}
	return m.active[m.activeCursor], true
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
