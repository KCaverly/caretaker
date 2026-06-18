// Package tui contains the Bubble Tea deck that powers the ct command.
package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/sahilm/fuzzy"

	"github.com/KCaverly/caretaker/internal/repo"
	"github.com/KCaverly/caretaker/internal/session"
)

// barHeight is the number of rows the top chrome occupies: the status bar, a
// light separator line, and a blank spacing row beneath it.
const barHeight = 3

// Default reserved keys (not forwarded to embedded sessions); overridable via config.
const (
	defaultKeyCycle  = "ctrl+o" // cycle to the next session view
	defaultKeyPicker = "ctrl+g" // return to the CT picker
)

// screen is the active view: the picker or one of the session views.
type screen int

const (
	screenPicker screen = iota
	screenEditor
	screenAgent
	screenTerminal
)

// next cycles among the session views (editor → agent → terminal → editor).
func (s screen) next() screen {
	switch s {
	case screenEditor:
		return screenAgent
	case screenAgent:
		return screenTerminal
	default:
		return screenEditor
	}
}

// sessionIndex maps a session screen to its index in a workspace's sessions.
func sessionIndex(s screen) int {
	switch s {
	case screenEditor:
		return 0
	case screenAgent:
		return 1
	case screenTerminal:
		return 2
	default:
		return -1
	}
}

// workspaceRef is the currently-activated workspace and its live sessions.
type workspaceRef struct {
	repo, worktree, key string
	sessions            []*session.Session
}

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

// Model is the ct UI: a pinned status bar plus the active screen (picker or an
// embedded nvim/claude/terminal session).
type Model struct {
	ctrl *Controller
	mgr  *session.Manager

	keyCycle, keyPicker string

	screen  screen
	current *workspaceRef

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

// New builds the model.
func New(ctrl *Controller, mgr *session.Manager) Model {
	filter := textinput.New()
	filter.Placeholder = "filter repos…"
	filter.Prompt = "› "
	filter.Focus()

	name := textinput.New()
	name.Placeholder = "branch-name"
	name.Prompt = "› "

	cycle, picker := ctrl.Keys()
	if cycle == "" {
		cycle = defaultKeyCycle
	}
	if picker == "" {
		picker = defaultKeyPicker
	}

	return Model{
		ctrl: ctrl, mgr: mgr,
		keyCycle: cycle, keyPicker: picker,
		filter: filter, nameInput: name, focus: focusNew,
	}
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

type actionDoneMsg struct{ err error }

type dirtyMsg struct{}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.loadCmd(), textinput.Blink, m.repaintCmd())
}

func (m Model) loadCmd() tea.Cmd {
	return func() tea.Msg {
		g, err := m.ctrl.Load()
		return loadedMsg{groups: g, err: err}
	}
}

// repaintCmd blocks until a session's screen changes, then asks for a re-render.
func (m Model) repaintCmd() tea.Cmd {
	mgr := m.mgr
	return func() tea.Msg {
		<-mgr.Dirty()
		return dirtyMsg{}
	}
}

func (m Model) sessionSize() (int, int) {
	return m.width, max(1, m.height-barHeight)
}

func (m Model) activeSession() *session.Session {
	if m.current == nil {
		return nil
	}
	i := sessionIndex(m.screen)
	if i < 0 || i >= len(m.current.sessions) {
		return nil
	}
	return m.current.sessions[i]
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		inputW := max(10, m.width-12)
		m.filter.SetWidth(inputW)
		m.nameInput.SetWidth(inputW)
		w, h := m.sessionSize()
		m.mgr.Resize(w, h)
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
		return m.activate(msg.wt.Repo, msg.wt.Name, msg.wt.Path)

	case actionDoneMsg:
		if msg.err != nil {
			m.status = msg.err.Error()
		}
		return m, m.loadCmd()

	case dirtyMsg:
		return m, m.repaintCmd()

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

// activate ensures the workspace's sessions are running and switches to the
// editor view.
func (m Model) activate(repoName, wtName, dir string) (tea.Model, tea.Cmd) {
	key := repoName + "/" + wtName
	w, h := m.sessionSize()
	ss, err := m.mgr.Activate(key, dir, m.ctrl.Specs(), w, h)
	if err != nil {
		m.status = "open error: " + err.Error()
		return m, m.loadCmd()
	}
	m.current = &workspaceRef{repo: repoName, worktree: wtName, key: key, sessions: ss}
	m.screen = screenEditor
	m.status = ""
	return m, m.loadCmd()
}

// --- key handling ---

func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.screen != screenPicker {
		return m.handleSessionKey(msg)
	}
	return m.handlePicker(msg)
}

func (m Model) handleSessionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case m.keyCycle:
		m.screen = m.screen.next()
		return m, nil
	case m.keyPicker:
		m.screen = screenPicker
		return m, nil
	}
	if s := m.activeSession(); s != nil {
		s.SendKey(toUVKey(msg))
	}
	return m, nil
}

func (m Model) handlePicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeCreateName:
		return m.handleCreateKey(msg)
	case modeConfirmRemove:
		return m.handleConfirmKey(msg)
	}
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
			return m.activate(it.repo.Name, it.view.WT.Name, it.view.WT.Path)
		}
	case "d":
		if it, ok := m.selectedActive(); ok {
			m.stopWorkspace(wsKey(it.repo.Name, it.view.WT.Name))
			m.status = "stopped " + it.view.WT.Name
			return m, m.loadCmd()
		}
	case "x":
		if it, ok := m.selectedActive(); ok && !it.view.WT.IsMain {
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
		m.stopWorkspace(wsKey(it.repo.Name, it.view.WT.Name))
		r, wt := it.repo, it.view.WT
		m.status = "removing " + wt.Name + "…"
		return m, func() tea.Msg { return actionDoneMsg{err: m.ctrl.Remove(r, wt)} }
	}
	m.mode = modeNormal
	m.status = ""
	return m, nil
}

// stopWorkspace closes a workspace's sessions and, if it was current, returns to
// the picker.
func (m *Model) stopWorkspace(key string) {
	m.mgr.Close(key)
	if m.current != nil && m.current.key == key {
		m.current = nil
		m.screen = screenPicker
	}
}

func wsKey(repoName, wtName string) string { return repoName + "/" + wtName }

// toUVKey converts a Bubble Tea key event into the ultraviolet key event the
// emulator expects (the field layouts are identical).
func toUVKey(k tea.KeyPressMsg) uv.KeyPressEvent {
	return uv.KeyPressEvent{
		Text:        k.Text,
		Mod:         uv.KeyMod(k.Mod),
		Code:        k.Code,
		ShiftedCode: k.ShiftedCode,
		BaseCode:    k.BaseCode,
	}
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
			if m.mgr != nil {
				wv.Live = m.mgr.Has(wsKey(g.Repo.Name, wv.WT.Name))
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
