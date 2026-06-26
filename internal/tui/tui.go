// Package tui contains the Bubble Tea deck that powers the ct command.
package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/sahilm/fuzzy"

	"github.com/KCaverly/caretaker/internal/config"
	"github.com/KCaverly/caretaker/internal/repo"
	"github.com/KCaverly/caretaker/internal/session"
	"github.com/KCaverly/caretaker/internal/state"
)

// barHeight is the number of rows the top chrome occupies: the status bar and
// a light separator line directly beneath it.
const barHeight = 2

// Default reserved keys (not forwarded to embedded sessions); overridable via config.
const (
	defaultKeyCycle     = "ctrl+o" // cycle to the next session view
	defaultKeyPicker    = "ctrl+g" // return to the CT picker
	defaultKeyPalette   = "ctrl+a" // open the agent switcher
	defaultKeyNextAgent = "f4"     // focus the next agent in the pool
	defaultKeyPrevAgent = "f3"     // focus the previous agent in the pool
	defaultKeyHelp         = "f1" // toggle the help overlay
	defaultKeyGlobalConfig = "ctrl+h" // open home-directory workspace
)

// screen is the active view: the picker, one of the session views, or setup.
type screen int

const (
	screenPicker screen = iota
	screenEditor
	screenAgent
	screenTerminal
	screenSetup
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

// workspaceRef is the currently-activated workspace and its live sessions.
type workspaceRef struct {
	repo, worktree, key, path string
	ws                        *session.Workspace
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

// notifLevel ranks an unread notification for a worktree.
// Higher values take priority: waiting (needs action) beats done (informational).
type notifLevel int

const (
	notifNone    notifLevel = iota // nothing pending
	notifDone                     // busy → idle while unviewed
	notifWaiting                  // busy → waiting (needs input/permission)
)

// activeItem is a worktree shown in the "active" section, with its owning repo.
type activeItem struct {
	repo repo.Repo
	view WorktreeView
}

// Model is the ct UI: a pinned status bar plus the active screen (picker or an
// embedded nvim/claude/terminal session).
type Model struct {
	ctrl  *Controller
	mgr   *session.Manager
	state *state.State

	keyCycle, keyPicker                    string
	keyPalette, keyNextAgent, keyPrevAgent string
	keyHelp, keyGlobalConfig               string

	screen   screen
	current  *workspaceRef
	helpOpen bool

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
	recentRank   map[string]int // worktree key -> recency rank (1..3); absent = none

	// first-run setup
	configPath string

	// last screen visited per worktree key, so switching back lands where you left
	lastScreens map[string]screen

	// agent switcher overlay
	paletteOpen   bool
	paletteCursor int
	naming        bool // entering a label for a new agent
	agentName     textinput.Model
	rootInput     textinput.Model

	// live agent statuses from `claude agents --json`, keyed by pid
	agentStatus     map[int]AgentStatus
	agentPrevStatus map[int]string        // pid → status from previous poll, for transition detection
	unread          map[string]notifLevel // worktree key → highest unread notification level
	agentUnread     map[int]notifLevel    // pid → highest unread notification level (for palette)

	status        string
	statusAt      time.Time // when a transient status was set (for auto-expiry)
	width, height int
}

// transientStatusTTL is how long a non-error status message lingers before the
// status poll clears it.
const transientStatusTTL = 4 * time.Second

// New builds the model.
func New(ctrl *Controller, mgr *session.Manager) Model {
	filter := textinput.New()
	filter.Placeholder = "filter repos…"
	filter.Prompt = "› "
	filter.Focus()

	name := textinput.New()
	name.Placeholder = "branch-name"
	name.Prompt = "› "

	agentName := textinput.New()
	agentName.Placeholder = "task label (optional)"
	agentName.Prompt = "› "

	rootInput := textinput.New()
	rootInput.Placeholder = "~/repos"
	rootInput.Prompt = "› "

	cycle, picker := ctrl.Keys()
	if cycle == "" {
		cycle = defaultKeyCycle
	}
	if picker == "" {
		picker = defaultKeyPicker
	}
	palette, next, prev := ctrl.AgentKeys()
	if palette == "" {
		palette = defaultKeyPalette
	}
	if next == "" {
		next = defaultKeyNextAgent
	}
	if prev == "" {
		prev = defaultKeyPrevAgent
	}
	help := ctrl.HelpKey()
	if help == "" {
		help = defaultKeyHelp
	}
	globalConfig := ctrl.GlobalConfigKey()
	if globalConfig == "" {
		globalConfig = defaultKeyGlobalConfig
	}

	return Model{
		ctrl: ctrl, mgr: mgr, state: state.Load(),
		keyCycle: cycle, keyPicker: picker,
		keyPalette: palette, keyNextAgent: next, keyPrevAgent: prev,
		keyHelp: help, keyGlobalConfig: globalConfig,
		filter:  filter, nameInput: name, agentName: agentName, rootInput: rootInput,
		focus:           focusNew,
		agentPrevStatus: map[int]string{},
		unread:          map[string]notifLevel{},
		agentUnread:     map[int]notifLevel{},
	}
}

// EnterSetup switches the model into first-run setup mode, prompting the user
// to choose a repos root before caretaker starts normally.
func (m Model) EnterSetup(configPath string) Model {
	m.screen = screenSetup
	m.configPath = configPath
	m.rootInput.Focus()
	return m
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

// statusTickMsg fires on the status-poll timer; statusMsg carries the result of
// one `claude agents --json` poll.
type statusTickMsg struct{}

type statusMsg struct {
	byPid map[int]AgentStatus
	err   error
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	if m.screen == screenSetup {
		return tea.Batch(textinput.Blink, m.repaintCmd())
	}
	return tea.Batch(m.loadCmd(), textinput.Blink, m.repaintCmd(), m.pollStatusCmd())
}

// pollStatusCmd runs one `claude agents --json` poll off the UI goroutine.
func (m Model) pollStatusCmd() tea.Cmd {
	ctrl := m.ctrl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		st, err := ctrl.AgentStatuses(ctx)
		return statusMsg{byPid: st, err: err}
	}
}

// scheduleStatusTick re-arms the poll timer, polling faster while any agent is
// active (busy/waiting) and backing off when everything is idle.
func (m Model) scheduleStatusTick() tea.Cmd {
	interval := 5 * time.Second
	for _, st := range m.agentStatus {
		if st.Status == "busy" || st.Status == "waiting" {
			interval = 2 * time.Second
			break
		}
	}
	return tea.Tick(interval, func(time.Time) tea.Msg { return statusTickMsg{} })
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
	if m.current == nil || m.current.ws == nil {
		return nil
	}
	switch m.screen {
	case screenEditor:
		return m.current.ws.Editor
	case screenTerminal:
		return m.current.ws.Term
	case screenAgent:
		return m.current.ws.ActiveAgentSession()
	default:
		return nil
	}
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	return m.update(msg)
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		inputW := max(10, m.width-12)
		m.filter.SetWidth(inputW)
		m.nameInput.SetWidth(inputW)
		m.rootInput.SetWidth(clamp(m.width-14, 20, 52))
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
		m.computeRecentRanks()
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

	case statusTickMsg:
		m.maybeExpireStatus()
		return m, m.pollStatusCmd()

	case statusMsg:
		if msg.err == nil {
			bell := false
			for pid, st := range msg.byPid {
				prev := m.agentPrevStatus[pid]
				if prev == "busy" && (st.Status == "idle" || st.Status == "waiting") {
					if key, ok := m.pathToKey(st.Cwd); ok {
						level := notifDone
						if st.Status == "waiting" {
							level = notifWaiting
						}
						if level > m.unread[key] {
							m.unread[key] = level
							bell = true
						}
						if level > m.agentUnread[pid] {
							m.agentUnread[pid] = level
						}
					}
				}
			}
			if bell {
				fmt.Fprint(os.Stderr, "\a")
			}
			m.agentPrevStatus = make(map[int]string, len(msg.byPid))
			for pid, st := range msg.byPid {
				m.agentPrevStatus[pid] = st.Status
			}
			m.agentStatus = msg.byPid
		}
		m.maybeExpireStatus()
		return m, m.scheduleStatusTick()

	case tea.MouseClickMsg:
		return m.handleMouseClick(msg)
	case tea.MouseReleaseMsg:
		m.forwardMouse(msg)
		return m, nil
	case tea.MouseWheelMsg:
		m.forwardMouse(msg)
		return m, nil
	case tea.MouseMotionMsg:
		m.forwardMouse(msg)
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

// clearWorkspaceUnread deletes the workspace-level unread entry and clears the
// per-agent unread entries for every agent currently in the workspace. Both maps
// share the same lifecycle so callers always use this instead of bare deletes.
func (m Model) clearWorkspaceUnread() {
	if m.current == nil {
		return
	}
	delete(m.unread, m.current.key)
	if m.current.ws != nil {
		for _, a := range m.current.ws.Agents {
			if pid := a.Pid(); pid != 0 {
				delete(m.agentUnread, pid)
			}
		}
	}
}

// activateGlobalConfig opens the home-directory workspace (synthetic key "~/config"),
// starting it fresh on first use and resuming its agent pool on subsequent presses.
func (m Model) activateGlobalConfig() (tea.Model, tea.Cmd) {
	home, err := m.ctrl.GlobalConfigDir()
	if err != nil {
		m.status = "home dir error: " + err.Error()
		return m, nil
	}
	return m.activate("~", "config", home)
}

// activate ensures the workspace's sessions are running and switches to the
// last screen used in that worktree (editor on first open). A brand-new worktree
// starts one fresh claude session; reopening resumes the agent pool.
func (m Model) activate(repoName, wtName, dir string) (tea.Model, tea.Cmd) {
	key := repoName + "/" + wtName
	w, h := m.sessionSize()
	ws, err := m.mgr.Activate(key, dir, m.workspaceSpecs(key), w, h)
	if err != nil {
		m.status = "open error: " + err.Error()
		return m, m.loadCmd()
	}
	if saved := m.savedActiveAgent(key); saved < len(ws.Agents) {
		ws.ActiveAgent = saved
	}
	if m.current != nil && m.screen != screenPicker {
		if m.lastScreens == nil {
			m.lastScreens = map[string]screen{}
		}
		m.lastScreens[m.current.key] = m.screen
	}
	m.current = &workspaceRef{repo: repoName, worktree: wtName, key: key, path: dir, ws: ws}
	if s, ok := m.lastScreens[key]; ok {
		m.screen = s
	} else {
		m.screen = screenEditor
	}
	if m.screen == screenAgent {
		m.clearWorkspaceUnread()
	}
	m.status = ""
	if m.state != nil {
		m.state.Touch(key)
		m.persistAgents(key)
		_ = m.state.Save()
	}
	return m, m.loadCmd()
}

// workspaceSpecs builds the session set for activating key: nvim and a shell
// always, plus either the resumed agent pool persisted from key's last run or, if
// none, a single fresh claude session.
func (m Model) workspaceSpecs(key string) []session.Spec {
	specs := []session.Spec{m.ctrl.EditorSpec()}
	var saved []state.AgentState
	if m.state != nil {
		saved, _ = m.state.Agents(key)
	}
	if len(saved) == 0 {
		specs = append(specs, m.ctrl.NewAgentSpec(""))
	} else {
		for _, a := range saved {
			specs = append(specs, m.ctrl.AgentSpec(a.SessionID, a.Label))
		}
	}
	return append(specs, m.ctrl.TermSpec())
}

// savedActiveAgent returns the focused-agent index persisted for key, or 0.
func (m Model) savedActiveAgent(key string) int {
	if m.state == nil {
		return 0
	}
	_, active := m.state.Agents(key)
	return active
}

// persistAgents snapshots a live workspace's agent pool (resume ids, labels, and
// focused index) into ct's state so the next open can rebuild it. The caller is
// responsible for Save(); this only updates the in-memory state.
func (m *Model) persistAgents(key string) {
	if m.state == nil {
		return
	}
	ws, ok := m.mgr.Workspace(key)
	if !ok {
		return
	}
	agents := make([]state.AgentState, 0, len(ws.Agents))
	for _, s := range ws.Agents {
		if s.SessionID == "" {
			continue // not a ct-managed claude session; nothing to resume
		}
		agents = append(agents, state.AgentState{SessionID: s.SessionID, Label: s.Title})
	}
	m.state.SetAgents(key, agents, ws.ActiveAgent)
}

// saveAgents snapshots the agent pool for key and flushes state to disk. Call it
// after any change to the pool (spawn, close) or the focused agent.
func (m *Model) saveAgents(key string) {
	if m.state == nil {
		return
	}
	m.persistAgents(key)
	_ = m.state.Save()
}

// --- key handling ---

func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.screen == screenSetup {
		return m.handleSetupKey(msg)
	}
	// Help is modal and reachable from anywhere (including inside a session). Any
	// key dismisses it; the help key itself toggles it.
	if m.helpOpen {
		m.helpOpen = false
		return m, nil
	}
	if msg.String() == m.keyHelp {
		m.helpOpen = true
		return m, nil
	}
	if msg.String() == m.keyGlobalConfig {
		return m.activateGlobalConfig()
	}
	if m.screen != screenPicker {
		return m.handleSessionKey(msg)
	}
	return m.handlePicker(msg)
}

func (m Model) handleSetupKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		return m, tea.Quit
	case "enter":
		root := strings.TrimSpace(m.rootInput.Value())
		if root == "" {
			return m, nil
		}
		abs, err := config.ResolveRoot(root)
		if err != nil {
			m.status = err.Error()
			return m, nil
		}
		if err := config.Save(m.configPath, abs); err != nil {
			m.status = "save error: " + err.Error()
			return m, nil
		}
		m.ctrl.SetRoot(abs)
		m.screen = screenPicker
		m.flash("config saved — welcome to caretaker!")
		return m, tea.Batch(m.loadCmd(), m.pollStatusCmd())
	}
	var cmd tea.Cmd
	m.rootInput, cmd = m.rootInput.Update(msg)
	m.status = ""
	return m, cmd
}

func (m Model) handleSessionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.paletteOpen {
		return m.handlePalette(msg)
	}
	switch msg.String() {
	case m.keyCycle:
		m.screen = m.screen.next()
		if m.screen == screenAgent && m.current != nil {
			m.clearWorkspaceUnread()
		}
		return m, nil
	case m.keyPicker:
		if m.current != nil {
			if m.lastScreens == nil {
				m.lastScreens = map[string]screen{}
			}
			m.lastScreens[m.current.key] = m.screen
		}
		m.screen = screenPicker
		if len(m.active) > 0 {
			m.focus = focusActive
		}
		return m, nil
	case m.keyPalette:
		return m.openPalette(), nil
	case m.keyNextAgent:
		return m.rotateAgent(+1), nil
	case m.keyPrevAgent:
		return m.rotateAgent(-1), nil
	}
	if s := m.activeSession(); s != nil {
		s.SendKey(toUVKey(msg))
	}
	return m, nil
}

// openPalette shows the agent switcher for the current workspace.
func (m Model) openPalette() tea.Model {
	if m.current == nil || m.current.ws == nil {
		return m
	}
	m.paletteOpen = true
	m.naming = false
	m.paletteCursor = m.current.ws.ActiveAgent
	return m
}

// rotateAgent moves the focused agent by delta (wrapping) and switches to the
// agent view. It's a no-op without at least two agents.
func (m Model) rotateAgent(delta int) tea.Model {
	if m.current == nil || m.current.ws == nil {
		return m
	}
	n := len(m.current.ws.Agents)
	if n == 0 {
		return m
	}
	m.current.ws.ActiveAgent = ((m.current.ws.ActiveAgent+delta)%n + n) % n
	m.screen = screenAgent
	m.clearWorkspaceUnread()
	m.saveAgents(m.current.key)
	return m
}

// handlePalette routes keys while the agent switcher is open. The list has one
// row per agent plus a trailing "new agent" row.
func (m Model) handlePalette(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	ws := m.current.ws
	if m.naming {
		return m.handleAgentName(msg)
	}
	newRow := len(ws.Agents) // index of the "+ new agent" row
	switch msg.String() {
	case "esc", m.keyPalette:
		m.paletteOpen = false
		return m, nil
	case "up", "k", "ctrl+p":
		if m.paletteCursor > 0 {
			m.paletteCursor--
		}
		return m, nil
	case "down", "j", "ctrl+n":
		if m.paletteCursor < newRow {
			m.paletteCursor++
		}
		return m, nil
	case "n":
		return m.beginNaming(), nil
	case "d":
		if m.paletteCursor < newRow {
			m.mgr.CloseAgent(m.current.key, m.paletteCursor)
			m.paletteCursor = clamp(m.paletteCursor, 0, max(0, len(ws.Agents)-1))
			m.saveAgents(m.current.key)
		}
		return m, nil
	case "enter":
		if m.paletteCursor == newRow {
			return m.beginNaming(), nil
		}
		ws.ActiveAgent = m.paletteCursor
		m.screen = screenAgent
		m.paletteOpen = false
		m.clearWorkspaceUnread()
		m.saveAgents(m.current.key)
		return m, nil
	}
	// A digit jumps straight to that agent (matching the 1..n labels shown).
	if s := msg.String(); len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
		if idx := int(s[0] - '1'); idx < len(ws.Agents) {
			ws.ActiveAgent = idx
			m.screen = screenAgent
			m.paletteOpen = false
			m.clearWorkspaceUnread()
			m.saveAgents(m.current.key)
		}
	}
	return m, nil
}

// beginNaming switches the palette into its new-agent label sub-state.
func (m Model) beginNaming() tea.Model {
	m.naming = true
	m.agentName.SetValue("")
	m.agentName.Focus()
	return m
}

// handleAgentName drives the label field for spawning a new agent.
func (m Model) handleAgentName(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.naming = false
		m.agentName.Blur()
		return m, nil
	case "enter":
		label := strings.TrimSpace(m.agentName.Value())
		m.naming = false
		m.agentName.Blur()
		w, h := m.sessionSize()
		if _, err := m.mgr.SpawnAgent(m.current.key, m.current.path, m.ctrl.NewAgentSpec(label), w, h); err != nil {
			m.status = "spawn error: " + err.Error()
			return m, nil
		}
		m.saveAgents(m.current.key)
		m.paletteCursor = m.current.ws.ActiveAgent
		m.screen = screenAgent
		m.paletteOpen = false
		m.clearWorkspaceUnread()
		return m, nil
	}
	var cmd tea.Cmd
	m.agentName, cmd = m.agentName.Update(msg)
	return m, cmd
}

// handleMouseClick switches tabs when a left-click lands on a bar icon, and
// otherwise forwards the click to the active session.
func (m Model) handleMouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	mo := msg.Mouse()
	if mo.Button == tea.MouseLeft {
		if s, ok := m.tabAt(mo.X, mo.Y); ok {
			return m.selectTab(s), nil
		}
		if m.screen == screenPicker && !m.helpOpen {
			if mm, cmd, ok := m.deckClick(mo.X, mo.Y); ok {
				return mm, cmd
			}
		}
	}
	m.forwardMouse(msg)
	return m, nil
}

// deckClick maps a left-click in the deck to the NEW repo row or ACTIVE worktree
// row under it. A click selects (and focuses) the row; clicking the
// already-selected row activates it — opening a worktree or starting the create
// flow, mirroring enter. handled is false when the click misses every row.
func (m Model) deckClick(x, y int) (tea.Model, tea.Cmd, bool) {
	by := y - barHeight // body-relative row (bar + separator sit above)
	if by < 0 {
		return m, nil, false
	}
	L := m.deckLayout(m.height - barHeight)

	// NEW box: content rows are body [1, 1+newContentH); the repo list begins at
	// content line 4 (header, blank, input, blank).
	if by >= 1 && by < 1+L.newContentH {
		if m.mode == modeCreateName {
			return m, nil, false
		}
		cl := by - 1
		if cl < 4 {
			return m, nil, false
		}
		start, end := windowBounds(len(m.repoMatches), m.newCursor, L.newRows)
		idx := start + (cl - 4)
		if idx < start || idx >= end || idx >= len(m.repoMatches) {
			return m, nil, false
		}
		reselect := m.focus == focusNew && m.newCursor == idx
		m.newCursor = idx
		if reselect {
			mm, cmd := m.beginCreate()
			return mm, cmd, true
		}
		var cmd tea.Cmd
		if m.focus != focusNew {
			m.focus = focusNew
			cmd = m.filter.Focus()
		}
		return m, cmd, true
	}

	// ACTIVE box: content rows are body [newOuterH+1, …); worktree rows begin at
	// content line 2 (header, blank).
	top := L.newOuterH
	if by >= top+1 && by < top+1+L.activeContentH {
		cl := by - (top + 1)
		if cl < 2 {
			return m, nil, false
		}
		display, rowItem := m.activeDisplay(m.width - 4)
		start, end := activeWindowStart(rowItem, m.activeCursor, L.activeRows)
		di := start + (cl - 2)
		if di < start || di >= end || di >= len(display) {
			return m, nil, false
		}
		item := rowItem[di]
		if item < 0 {
			return m, nil, false // a repo header row
		}
		reselect := m.focus == focusActive && m.activeCursor == item
		m.focus = focusActive
		m.activeCursor = item
		m.filter.Blur()
		if reselect {
			if it, ok := m.selectedActive(); ok {
				mm, cmd := m.activate(it.repo.Name, it.view.WT.Name, it.view.WT.Path)
				return mm, cmd, true
			}
		}
		return m, nil, true
	}

	return m, nil, false
}

// selectTab activates a clicked bar tab: the picker is always reachable; the
// session tabs only switch when a workspace is active.
func (m Model) selectTab(s screen) tea.Model {
	if s == screenPicker || m.current != nil {
		m.screen = s
		if s == screenAgent && m.current != nil {
			m.clearWorkspaceUnread()
		}
	}
	return m
}

// forwardMouse relays a mouse event to the active session (translated below the
// bar). The emulator only encodes it if the program has enabled mouse reporting.
func (m Model) forwardMouse(msg tea.MouseMsg) {
	if m.screen == screenPicker {
		return
	}
	s := m.activeSession()
	if s == nil || msg.Mouse().Y < barHeight {
		return
	}
	shift := func(mo tea.Mouse) uv.Mouse {
		return uv.Mouse{X: mo.X, Y: mo.Y - barHeight, Button: mo.Button, Mod: uv.KeyMod(mo.Mod)}
	}
	switch e := msg.(type) {
	case tea.MouseClickMsg:
		s.SendMouse(uv.MouseClickEvent(shift(e.Mouse())))
	case tea.MouseReleaseMsg:
		s.SendMouse(uv.MouseReleaseEvent(shift(e.Mouse())))
	case tea.MouseWheelMsg:
		s.SendMouse(uv.MouseWheelEvent(shift(e.Mouse())))
	case tea.MouseMotionMsg:
		s.SendMouse(uv.MouseMotionEvent(shift(e.Mouse())))
	}
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
	// "?" is a convenient deck-only alias for the help key; the picker owns its
	// input, so intercepting it before the filter is safe.
	if msg.String() == "?" {
		m.helpOpen = true
		return m, nil
	}
	// The picker key jumps straight to the most recently activated worktree, so
	// it toggles: session -> picker -> back to the most recent work.
	if msg.String() == m.keyPicker {
		if r, w, p, ok := m.mostRecentWorktree(); ok {
			return m.activate(r, w, p)
		}
		return m, nil
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
		return m.beginCreate()
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

// beginCreate enters the new-worktree naming form for the repo under newCursor.
func (m Model) beginCreate() (tea.Model, tea.Cmd) {
	if m.newCursor < 0 || m.newCursor >= len(m.repoMatches) {
		return m, nil
	}
	m.pendingRepo = m.repoMatches[m.newCursor]
	m.mode = modeCreateName
	m.filter.Blur()
	m.nameInput.SetValue("")
	m.status = "new worktree in " + m.pendingRepo.Name
	return m, m.nameInput.Focus()
}

func (m Model) handleActiveKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
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
			m.flash("stopped " + it.view.WT.Name)
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
		m.flash("creating " + name + "…")
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
		m.flash("removing " + wt.Name + "…")
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

// flash sets a transient status message that the status poll auto-clears after
// transientStatusTTL. Error statuses are set on m.status directly so they stick.
func (m *Model) flash(s string) {
	m.status = s
	m.statusAt = time.Now()
}

// maybeExpireStatus clears a transient (non-error) status once it has been shown
// for transientStatusTTL. It's driven by the status poll, which always
// reschedules itself, so the clear lands within a poll interval.
func (m *Model) maybeExpireStatus() {
	if m.status == "" || m.mode != modeNormal || m.statusAt.IsZero() {
		return
	}
	if strings.Contains(m.status, "error") {
		return
	}
	if time.Since(m.statusAt) > transientStatusTTL {
		m.status = ""
		m.statusAt = time.Time{}
	}
}

// toUVKey converts a Bubble Tea key event into the ultraviolet key event the
// emulator expects (the field layouts are identical).
func toUVKey(k tea.KeyPressMsg) uv.KeyPressEvent {
	code := k.Code
	mod := uv.KeyMod(k.Mod)
	// The vt SendKey default branch only encodes key.Code when Mod==0, so it
	// silently drops shift+letter pairs from kitty-protocol terminals. Collapse
	// them: if shift is the only modifier and there is a known shifted rune,
	// use it directly and clear the shift bit.
	if mod == uv.ModShift && k.ShiftedCode != 0 {
		code = k.ShiftedCode
		mod = 0
	}
	return uv.KeyPressEvent{
		Text:        k.Text,
		Mod:         mod,
		Code:        code,
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
		var views []WorktreeView
		for _, wv := range g.Worktrees {
			if wv.WT.IsMain {
				continue
			}
			if m.mgr != nil {
				wv.Live = m.mgr.Has(wsKey(g.Repo.Name, wv.WT.Name))
			}
			views = append(views, wv)
		}
		m.sortByRecency(g.Repo.Name, views)
		for _, wv := range views {
			items = append(items, activeItem{repo: g.Repo, view: wv})
		}
	}
	m.active = items
	m.activeCursor = clamp(m.activeCursor, 0, max(0, len(items)-1))
}

// mostRecentWorktree returns the worktree opened most recently in ct (across
// all repos), or ok=false if none has ever been opened.
func (m Model) mostRecentWorktree() (repoName, wtName, path string, ok bool) {
	if m.state == nil {
		return
	}
	var best int64
	for _, g := range m.groups {
		for _, wv := range g.Worktrees {
			if wv.WT.IsMain {
				continue
			}
			if t := m.state.Opened(wsKey(g.Repo.Name, wv.WT.Name)); t > best {
				best = t
				repoName, wtName, path, ok = g.Repo.Name, wv.WT.Name, wv.WT.Path, true
			}
		}
	}
	return
}

// computeRecentRanks finds the three worktrees opened most recently in ct
// (across all repos) and maps their keys to ranks 1..3 for badge display.
func (m *Model) computeRecentRanks() {
	m.recentRank = nil
	if m.state == nil {
		return
	}
	type wr struct {
		key string
		t   int64
	}
	var ws []wr
	for _, g := range m.groups {
		for _, wv := range g.Worktrees {
			if wv.WT.IsMain {
				continue
			}
			key := wsKey(g.Repo.Name, wv.WT.Name)
			if t := m.state.Opened(key); t > 0 {
				ws = append(ws, wr{key, t})
			}
		}
	}
	sort.SliceStable(ws, func(i, j int) bool { return ws[i].t > ws[j].t })

	ranks := make(map[string]int, 3)
	for i, w := range ws {
		if i >= 3 {
			break
		}
		ranks[w.key] = i + 1
	}
	m.recentRank = ranks
}

// sortByRecency orders a repo's worktrees most-recent-first: by when ct last
// opened them, then (for never-opened ones) by HEAD commit time, then by name.
func (m *Model) sortByRecency(repoName string, views []WorktreeView) {
	opened := func(name string) int64 {
		if m.state == nil {
			return 0
		}
		return m.state.Opened(wsKey(repoName, name))
	}
	sort.SliceStable(views, func(i, j int) bool {
		if oi, oj := opened(views[i].WT.Name), opened(views[j].WT.Name); oi != oj {
			return oi > oj
		}
		if views[i].CommitTime != views[j].CommitTime {
			return views[i].CommitTime > views[j].CommitTime
		}
		return views[i].WT.Name < views[j].WT.Name
	})
}

// pathToKey maps a session's working directory to its workspace key. Checks the
// currently active workspace first (covering synthetic workspaces like ~/config
// that don't appear in m.active), then walks the active list. Paths are cleaned
// before comparison to absorb trailing slashes and minor OS-level variation.
func (m Model) pathToKey(path string) (string, bool) {
	path = filepath.Clean(path)
	if m.current != nil && filepath.Clean(m.current.path) == path {
		return m.current.key, true
	}
	for _, it := range m.active {
		if filepath.Clean(it.view.WT.Path) == path {
			return wsKey(it.repo.Name, it.view.WT.Name), true
		}
	}
	return "", false
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
