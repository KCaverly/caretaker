// Package tui contains the Bubble Tea deck that powers the ct command.
package tui

import (
	"context"
	"fmt"
	"math/rand"
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
	defaultKeyCycle        = "ctrl+o" // cycle to the next session view
	defaultKeyPicker       = "ctrl+g" // return to the CT picker
	defaultKeyPalette      = "ctrl+a" // open the agent board
	defaultKeyNextAgent    = "f4"     // focus the next agent in the pool
	defaultKeyPrevAgent    = "f3"     // focus the previous agent in the pool
	defaultKeyHelp         = "f1"     // toggle the help overlay
	defaultKeyGlobalConfig = "ctrl+h" // open home-directory workspace
	defaultKeyNotif        = "ctrl+n" // agent board alias (was the notification overlay)
	defaultKeyPrompt       = "ctrl+y" // new-agent form pre-set for a background home agent

	// Terminal pane management — only intercepted when the terminal screen is active.
	defaultKeyTermSplitV = "ctrl+\\" // new pane to the right
	defaultKeyTermSplitH = "ctrl+-"  // new pane below
	defaultKeyTermCycle  = "ctrl+w"  // cycle pane focus
	defaultKeyTermZoom   = "ctrl+f"  // toggle full-size
	defaultKeyTermClose  = "ctrl+x"  // close active pane
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

// attnLevel ranks an agent's attention state, highest first when sorting.
// attnWaiting is derived live from the polled agent status and never stored;
// attnDone and attnMessage are unread markers recorded when a transition is
// observed and cleared when the user views the workspace's agents.
type attnLevel int

const (
	attnNone    attnLevel = iota // nothing pending
	attnDone                     // busy → idle while unviewed (*)
	attnMessage                  // background home agent completed, preview attached (@)
	attnWaiting                  // live status "waiting" — needs input/permission (!)
)

// attnEntry is one stored unread marker (attnDone or attnMessage) for an agent,
// keyed by pid in Model.attention.
type attnEntry struct {
	level   attnLevel
	key     string // workspace key the agent belongs to
	preview string // attnMessage: last meaningful line scraped at completion
}

// boardRow is one row of the agent board: a non-navigable worktree group
// header, a selectable agent row, or the trailing "+ new agent" row.
type boardRow struct {
	isAgent bool // navigable agent row
	isNew   bool // navigable "+ new agent" row

	key      string // workspace key ("repo/branch")
	repo     string // for activate()
	worktree string
	path     string

	agentIdx int // index in ws.Agents
	pid      int
	label    string    // display name
	status   string    // right-hand status/preview column
	attn     attnLevel // includes derived waiting
	num      int       // 1-based quick-jump number (first 9 agent rows)
}

// Focus order of the new-agent form's fields.
const (
	formFieldLabel = iota
	formFieldPrompt
	formFieldWhere
	formFieldMode
	formFieldCount
)

// bgAgentMeta tracks a background home-worktree agent launched via the
// new-agent form, pending completion.
type bgAgentMeta struct {
	label    string
	homeKey  string
	homePath string
}

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
	keyHelp, keyGlobalConfig, keyNotif     string
	keyPrompt                              string

	keyTermSplitV, keyTermSplitH            string
	keyTermCycle, keyTermZoom, keyTermClose string

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

	// agent board overlay (unified agent switcher + notifications)
	boardOpen   bool
	boardCursor int // index into the board's navigable rows
	agentName   textinput.Model
	rootInput   textinput.Model

	// live agent statuses from `claude agents --json`, keyed by pid
	agentStatus     map[int]AgentStatus
	agentPrevStatus map[int]string // pid → status from previous poll, for transition detection

	// stored unread markers (done/message) keyed by agent pid; waiting badges
	// are derived live from agentStatus and never stored here
	attention map[int]attnEntry

	// prompt input for the board's new-agent form
	promptInput textinput.Model

	// background home-agent tracking
	bgAgentPIDs map[int]bgAgentMeta // PIDs of home-mode agents pending completion

	// home workspace path/key, cached on first open for pathToKey lookups
	homeWSPath string
	homeWSKey  string

	// new-agent form (sub-state of the agent board)
	formOpen       bool
	formFocus      int  // formFieldLabel..formFieldMode
	formLocation   int  // 0 = active worktree, 1 = home worktree
	formBackground bool // false = foreground (default), true = background

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

	promptInput := textinput.New()
	promptInput.Placeholder = "What should Claude do?"
	promptInput.Prompt = "› "

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
	notif := ctrl.NotifKey()
	if notif == "" {
		notif = defaultKeyNotif
	}
	prompt := ctrl.PromptKey()
	if prompt == "" {
		prompt = defaultKeyPrompt
	}
	termSplitV, termSplitH, termCycle, termZoom, termClose := ctrl.TermPaneKeys()
	if termSplitV == "" {
		termSplitV = defaultKeyTermSplitV
	}
	if termSplitH == "" {
		termSplitH = defaultKeyTermSplitH
	}
	if termCycle == "" {
		termCycle = defaultKeyTermCycle
	}
	if termZoom == "" {
		termZoom = defaultKeyTermZoom
	}
	if termClose == "" {
		termClose = defaultKeyTermClose
	}

	return Model{
		ctrl: ctrl, mgr: mgr, state: state.Load(),
		keyCycle: cycle, keyPicker: picker,
		keyPalette: palette, keyNextAgent: next, keyPrevAgent: prev,
		keyHelp: help, keyGlobalConfig: globalConfig, keyNotif: notif,
		keyPrompt:     prompt,
		keyTermSplitV: termSplitV, keyTermSplitH: termSplitH,
		keyTermCycle: termCycle, keyTermZoom: termZoom, keyTermClose: termClose,
		filter: filter, nameInput: name, agentName: agentName, rootInput: rootInput,
		promptInput:     promptInput,
		focus:           focusNew,
		agentPrevStatus: map[int]string{},
		attention:       map[int]attnEntry{},
		bgAgentPIDs:     make(map[int]bgAgentMeta),
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
	seq    uint64 // issue-time load generation; stale results are dropped
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
	if len(m.bgAgentPIDs) > 0 {
		interval = 2 * time.Second
	}
	return tea.Tick(interval, func(time.Time) tea.Msg { return statusTickMsg{} })
}

// loadCmd issues one deck refresh. The generation stamp is taken at issue
// time, so when several loads overlap (activate, then a quick stop, then a
// refresh) only the newest one's result is applied — a slow stale scan can't
// clobber fresher state.
func (m Model) loadCmd() tea.Cmd {
	ctrl := m.ctrl
	seq := ctrl.loadSeq.Add(1)
	return func() tea.Msg {
		g, err := ctrl.Load()
		return loadedMsg{groups: g, err: err, seq: seq}
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
		return m.current.ws.ActiveTermSession()
	case screenAgent:
		return m.current.ws.ActiveAgentSession()
	default:
		return nil
	}
}

// Update implements tea.Model. After every message it re-declares which
// sessions are on screen so the manager can drop repaint wakeups from
// invisible ones (background agents streaming output would otherwise trigger
// a full re-render each pty read).
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	mm, cmd := m.update(msg)
	if model, ok := mm.(Model); ok {
		model.syncVisible()
		return model, cmd
	}
	return mm, cmd
}

// syncVisible pushes the currently visible session set to the manager.
func (m Model) syncVisible() {
	if m.mgr == nil {
		return
	}
	m.mgr.SetVisible(m.visibleSessions()...)
}

// visibleSessions returns the sessions whose output is currently drawn:
// nothing while the picker, setup, or a full-body overlay (help, board) is
// shown; the editor or focused agent on their screens; and on the terminal
// screen either the focused pane (zoomed/single) or every pane in the split
// layout. Mirrors the branch structure of View.
func (m Model) visibleSessions() []*session.Session {
	if m.helpOpen || m.boardOpen || m.screen == screenPicker || m.screen == screenSetup {
		return nil
	}
	if m.current == nil || m.current.ws == nil {
		return nil
	}
	ws := m.current.ws
	switch m.screen {
	case screenEditor:
		return []*session.Session{ws.Editor}
	case screenAgent:
		return []*session.Session{ws.ActiveAgentSession()}
	case screenTerminal:
		if ws.TermZoomed || len(ws.Terms) == 1 || ws.TermLayout == nil {
			return []*session.Session{ws.ActiveTermSession()}
		}
		return ws.Terms
	}
	return nil
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		inputW := max(10, m.width-12)
		m.filter.SetWidth(inputW)
		m.nameInput.SetWidth(inputW)
		m.rootInput.SetWidth(clamp(m.width-14, 20, 52))
		m.promptInput.SetWidth(clamp(m.width-16, 20, 52))
		w, h := m.sessionSize()
		m.mgr.Resize(w, h)
		if m.current != nil {
			m.mgr.ResizeTermPanes(m.current.key, w, h)
		}
		return m, nil

	case loadedMsg:
		if msg.seq != m.ctrl.loadSeq.Load() {
			return m, nil // superseded by a newer in-flight load
		}
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
				if _, isBg := m.bgAgentPIDs[pid]; isBg {
					continue // background completions are handled below
				}
				prev := m.agentPrevStatus[pid]
				if prev != "busy" || (st.Status != "idle" && st.Status != "waiting") {
					continue
				}
				// Skip the bell/marker if the user is already watching this agent.
				if m.watchingAgent(pid) {
					continue
				}
				if st.Status == "waiting" {
					bell = true // the ! badge is derived live from agentStatus
					continue
				}
				if key, ok := m.pathToKey(st.Cwd); ok {
					m.recordAttention(pid, attnDone, key, "")
					bell = true
				}
			}
			// Mark non-bg agents that disappeared between polls while busy
			// (e.g. the process crashed or was killed). Use the last-known Cwd so
			// we can map the pid to its workspace.
			for pid, prev := range m.agentPrevStatus {
				if _, stillTracked := msg.byPid[pid]; stillTracked {
					continue
				}
				if _, isBg := m.bgAgentPIDs[pid]; isBg {
					continue // handled in the bg loop below
				}
				if prev != "busy" {
					continue
				}
				if last, ok := m.agentStatus[pid]; ok {
					if key, ok := m.pathToKey(last.Cwd); ok {
						m.recordAttention(pid, attnDone, key, "")
						bell = true
					}
				}
			}
			// Detect background home-agent completions.
			for pid, meta := range m.bgAgentPIDs {
				newSt, stillTracked := msg.byPid[pid]
				prev := m.agentPrevStatus[pid]
				completed := false
				if stillTracked && newSt.Status == "idle" && (prev == "busy" || prev == "waiting") {
					completed = true
				} else if !stillTracked && (prev == "busy" || prev == "waiting") {
					// Agent disappeared from claude agents list — treat as completed.
					completed = true
				}
				if completed {
					preview := m.scrapeBgAgentPreview(pid, meta)
					m.attention[pid] = attnEntry{level: attnMessage, key: meta.homeKey, preview: preview}
					delete(m.bgAgentPIDs, pid)
					bell = true
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

// watchingAgent reports whether the user is on the agent screen with the agent
// running as pid focused.
func (m Model) watchingAgent(pid int) bool {
	return m.screen == screenAgent && m.current != nil && m.current.ws != nil &&
		m.current.ws.ActiveAgentSession() != nil &&
		m.current.ws.ActiveAgentSession().Pid() == pid
}

// recordAttention stores an unread marker for pid unless an equal or higher one
// is already recorded.
func (m Model) recordAttention(pid int, level attnLevel, key, preview string) {
	if cur, ok := m.attention[pid]; ok && cur.level >= level {
		return
	}
	m.attention[pid] = attnEntry{level: level, key: key, preview: preview}
}

// clearWorkspaceAttention drops the stored unread markers for every agent in
// the current workspace. Live waiting badges are derived from agentStatus and
// clear themselves once the agent's polled status changes.
func (m Model) clearWorkspaceAttention() {
	if m.current == nil {
		return
	}
	for pid, e := range m.attention {
		if e.key == m.current.key {
			delete(m.attention, pid)
		}
	}
}

// agentAttn returns pid's effective attention level: a live "waiting" status
// wins, then any stored unread marker.
func (m Model) agentAttn(pid int) attnLevel {
	if pid == 0 {
		return attnNone
	}
	if m.agentStatus[pid].Status == "waiting" {
		return attnWaiting
	}
	return m.attention[pid].level
}

// worktreeAttn returns the highest attention level among key's agents, for the
// deck's per-worktree badge: live-waiting agents mapped to key via their Cwd,
// then stored unread markers.
func (m Model) worktreeAttn(key string) attnLevel {
	level := attnNone
	for pid, st := range m.agentStatus {
		if st.Status != "waiting" || m.watchingAgent(pid) {
			continue
		}
		if k, ok := m.pathToKey(st.Cwd); ok && k == key {
			level = attnWaiting
		}
	}
	for _, e := range m.attention {
		if e.key == key && e.level > level {
			level = e.level
		}
	}
	return level
}

// attnSummary counts, for the bar badge: worktrees with a live-waiting agent,
// worktrees with unread completions, and unread background messages.
func (m Model) attnSummary() (waiting, done, msgs int) {
	waitKeys := map[string]bool{}
	for pid, st := range m.agentStatus {
		if st.Status != "waiting" || m.watchingAgent(pid) {
			continue
		}
		if key, ok := m.pathToKey(st.Cwd); ok {
			waitKeys[key] = true
		}
	}
	doneKeys := map[string]bool{}
	for _, e := range m.attention {
		switch e.level {
		case attnDone:
			doneKeys[e.key] = true
		case attnMessage:
			msgs++
		}
	}
	return len(waitKeys), len(doneKeys), msgs
}

// buildBoard constructs the agent board: every agent of every open workspace,
// grouped under non-navigable worktree header rows, with worktrees (and agents
// within them) needing attention sorted first and a trailing "+ new agent" row.
// nav maps cursor positions to indices in rows.
func (m Model) buildBoard() (rows []boardRow, nav []int) {
	type group struct {
		key, repo, worktree, path string
		agents                    []boardRow
		attn                      attnLevel
		opened                    int64
	}

	seen := map[string]bool{}
	var groups []group
	addGroup := func(key, repoName, wtName, path string) {
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		ws, ok := m.mgr.Workspace(key)
		if !ok {
			if m.current == nil || m.current.key != key || m.current.ws == nil {
				return
			}
			ws = m.current.ws
		}
		g := group{key: key, repo: repoName, worktree: wtName, path: path}
		if m.state != nil {
			g.opened = m.state.Opened(key)
		}
		for i, a := range ws.Agents {
			pid := a.Pid()
			attn := m.agentAttn(pid)
			g.agents = append(g.agents, boardRow{
				isAgent: true, key: key, repo: repoName, worktree: wtName, path: path,
				agentIdx: i, pid: pid, label: agentTitle(a.Title),
				status: m.boardStatus(pid, attn), attn: attn,
			})
			if attn > g.attn {
				g.attn = attn
			}
		}
		if len(g.agents) == 0 {
			return
		}
		sort.SliceStable(g.agents, func(i, j int) bool { return g.agents[i].attn > g.agents[j].attn })
		groups = append(groups, g)
	}

	if m.current != nil {
		addGroup(m.current.key, m.current.repo, m.current.worktree, m.current.path)
	}
	for _, it := range m.active {
		addGroup(wsKey(it.repo.Name, it.view.WT.Name), it.repo.Name, it.view.WT.Name, it.view.WT.Path)
	}
	// The home workspace isn't a git worktree, so it never appears in m.active.
	if m.homeWSKey != "" {
		addGroup(m.homeWSKey, "~", "config", m.homeWSPath)
	}

	isCurrent := func(key string) bool { return m.current != nil && m.current.key == key }
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].attn != groups[j].attn {
			return groups[i].attn > groups[j].attn // attention floats to the top
		}
		if ci, cj := isCurrent(groups[i].key), isCurrent(groups[j].key); ci != cj {
			return ci
		}
		if groups[i].opened != groups[j].opened {
			return groups[i].opened > groups[j].opened // then most recently opened
		}
		return groups[i].key < groups[j].key
	})

	num := 0
	for _, g := range groups {
		rows = append(rows, boardRow{key: g.key, repo: g.repo, worktree: g.worktree, path: g.path, attn: g.attn})
		for _, r := range g.agents {
			num++
			if num <= 9 {
				r.num = num
			}
			nav = append(nav, len(rows))
			rows = append(rows, r)
		}
	}
	rows = append(rows, boardRow{isNew: true})
	nav = append(nav, len(rows)-1)
	return rows, nav
}

// boardStatus renders the right-hand column for an agent row: the message
// preview for background completions, otherwise the live polled status.
func (m Model) boardStatus(pid int, attn attnLevel) string {
	if attn == attnMessage {
		if p := m.attention[pid].preview; p != "" {
			return p
		}
		return "done"
	}
	st := m.agentStatus[pid]
	switch st.Status {
	case "busy":
		return "working"
	case "waiting":
		if st.WaitingFor != "" {
			return "waiting: " + st.WaitingFor
		}
		return "waiting"
	case "idle":
		if attn == attnDone {
			return "done"
		}
		return "idle"
	default:
		return ""
	}
}

// openBoard toggles the agent board. The cursor starts on the first row needing
// attention, falling back to the current workspace's focused agent.
func (m Model) openBoard() (tea.Model, tea.Cmd) {
	if m.boardOpen {
		m.boardOpen = false
		m.formOpen = false
		return m, nil
	}
	m.boardOpen = true
	m.formOpen = false
	m.boardCursor = 0
	rows, nav := m.buildBoard()
	for i, ri := range nav {
		if rows[ri].isAgent && rows[ri].attn > attnNone {
			m.boardCursor = i
			return m, nil
		}
	}
	for i, ri := range nav {
		r := rows[ri]
		if r.isAgent && m.current != nil && r.key == m.current.key &&
			m.current.ws != nil && r.agentIdx == m.current.ws.ActiveAgent {
			m.boardCursor = i
			break
		}
	}
	return m, nil
}

// handleBoard routes key events while the agent board is open.
func (m Model) handleBoard(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.formOpen {
		return m.handleBoardForm(msg)
	}
	rows, nav := m.buildBoard()
	rowAt := func(i int) (boardRow, bool) {
		if i < 0 || i >= len(nav) {
			return boardRow{}, false
		}
		return rows[nav[i]], true
	}
	switch msg.String() {
	case "esc", "q", m.keyPalette, m.keyNotif:
		m.boardOpen = false
		return m, nil
	case m.keyPrompt:
		return m.openQuickPrompt()
	case "up", "k", "ctrl+p":
		if m.boardCursor > 0 {
			m.boardCursor--
		}
		return m, nil
	case "down", "j":
		if m.boardCursor < len(nav)-1 {
			m.boardCursor++
		}
		return m, nil
	case "n":
		return m.openNewAgentForm(), nil
	case "d":
		if r, ok := rowAt(m.boardCursor); ok && r.isAgent {
			m.mgr.CloseAgent(r.key, r.agentIdx)
			delete(m.attention, r.pid)
			save := m.saveAgents(r.key)
			_, nav = m.buildBoard()
			m.boardCursor = clamp(m.boardCursor, 0, max(0, len(nav)-1))
			return m, save
		}
		return m, nil
	case "enter":
		if r, ok := rowAt(m.boardCursor); ok {
			if r.isNew {
				return m.openNewAgentForm(), nil
			}
			return m.focusBoardAgent(r)
		}
		return m, nil
	}
	// A digit jumps straight to that agent (matching the numbers shown).
	if s := msg.String(); len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
		want := int(s[0] - '0')
		for _, ri := range nav {
			if rows[ri].isAgent && rows[ri].num == want {
				return m.focusBoardAgent(rows[ri])
			}
		}
	}
	return m, nil
}

// focusBoardAgent navigates directly to an agent's pane: it activates the
// agent's workspace (when it isn't already current), switches to the agent
// screen, focuses the agent, and clears the workspace's unread markers.
func (m Model) focusBoardAgent(r boardRow) (tea.Model, tea.Cmd) {
	if m.current != nil && m.current.key == r.key {
		m.boardOpen = false
		m.screen = screenAgent
		if m.current.ws != nil && r.agentIdx >= 0 && r.agentIdx < len(m.current.ws.Agents) {
			m.current.ws.ActiveAgent = r.agentIdx
		}
		m.clearWorkspaceAttention()
		return m, m.saveAgents(r.key)
	}
	mm, cmd := m.activate(r.repo, r.worktree, r.path)
	model := mm.(Model)
	model.boardOpen = false
	if model.current != nil && model.current.key == r.key {
		model.screen = screenAgent
		if model.current.ws != nil && r.agentIdx >= 0 && r.agentIdx < len(model.current.ws.Agents) {
			model.current.ws.ActiveAgent = r.agentIdx
		}
		model.clearWorkspaceAttention()
		cmd = tea.Batch(cmd, model.saveAgents(r.key))
	}
	return model, cmd
}

// activateGlobalConfig opens the home-directory workspace (synthetic key "~/config"),
// starting it fresh on first use and resuming its agent pool on subsequent presses.
func (m Model) activateGlobalConfig() (tea.Model, tea.Cmd) {
	home, err := m.ctrl.GlobalConfigDir()
	if err != nil {
		m.status = "home dir error: " + err.Error()
		return m, nil
	}
	m.homeWSPath = home
	m.homeWSKey = "~/config"
	return m.activate("~", "config", home)
}

// openNewAgentForm switches the board into the new-agent form with defaults:
// active worktree (home when no workspace is active), foreground, focus on the
// label field.
func (m Model) openNewAgentForm() tea.Model {
	m.boardOpen = true
	m.formOpen = true
	m.formFocus = formFieldLabel
	m.formLocation = 0
	if m.current == nil {
		m.formLocation = 1
	}
	m.formBackground = false
	m.agentName.SetValue("")
	m.promptInput.SetValue("")
	m.promptInput.Blur()
	m.agentName.Focus()
	return m
}

// openQuickPrompt opens the new-agent form pre-set for a background home
// worktree agent with the prompt field focused (the ctrl+y shortcut).
func (m Model) openQuickPrompt() (tea.Model, tea.Cmd) {
	mm := m.openNewAgentForm().(Model)
	mm.formLocation = 1
	mm.formBackground = true
	mm.formFocus = formFieldPrompt
	mm.agentName.Blur()
	return mm, mm.promptInput.Focus()
}

// setFormFocus moves the new-agent form's focus to field f (wrapping), keeping
// the text inputs' focus state in sync.
func (m Model) setFormFocus(f int) (Model, tea.Cmd) {
	m.formFocus = ((f % formFieldCount) + formFieldCount) % formFieldCount
	m.agentName.Blur()
	m.promptInput.Blur()
	switch m.formFocus {
	case formFieldLabel:
		return m, m.agentName.Focus()
	case formFieldPrompt:
		return m, m.promptInput.Focus()
	}
	return m, nil
}

// handleBoardForm drives the new-agent form: tab/shift+tab (or ↑↓) move between
// the label, prompt, where, and mode fields; space or ←/→ flip the focused
// toggle; enter launches — except on the label field, where it advances to the
// prompt so the old label→prompt→launch muscle memory still works.
func (m Model) handleBoardForm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.formOpen = false
		m.agentName.Blur()
		m.promptInput.Blur()
		return m, nil
	case "tab", "down":
		return m.setFormFocus(m.formFocus + 1)
	case "shift+tab", "up":
		return m.setFormFocus(m.formFocus - 1)
	case "enter":
		if m.formFocus == formFieldLabel {
			return m.setFormFocus(formFieldPrompt)
		}
		return m.launchAgent()
	}
	switch m.formFocus {
	case formFieldLabel:
		var cmd tea.Cmd
		m.agentName, cmd = m.agentName.Update(msg)
		return m, cmd
	case formFieldPrompt:
		var cmd tea.Cmd
		m.promptInput, cmd = m.promptInput.Update(msg)
		return m, cmd
	}
	switch msg.String() {
	case "left", "right", "h", "l", "space":
		if m.formFocus == formFieldWhere {
			m.formLocation = 1 - m.formLocation
		} else {
			m.formBackground = !m.formBackground
		}
	}
	return m, nil
}

var promptAdjectives = []string{
	"amber", "bold", "calm", "deft", "eager", "fast", "grand", "hazy",
	"idle", "jade", "keen", "lazy", "mild", "nimble", "odd", "proud",
	"quick", "rare", "shy", "tame", "umber", "vivid", "warm", "young",
	"zany", "brave", "crisp", "dark", "fleet", "grey",
}

var promptNouns = []string{
	"badger", "cedar", "drift", "ember", "falcon", "grove", "haven",
	"inlet", "jasper", "kelp", "lantern", "moth", "nebula", "otter",
	"pine", "quartz", "raven", "stone", "tide", "vale", "willow",
	"fox", "creek", "dune", "fern", "gust", "hawk", "iris", "juniper",
	"kestrel", "larch",
}

func randomAgentTitle() string {
	adj := promptAdjectives[rand.Intn(len(promptAdjectives))]
	noun := promptNouns[rand.Intn(len(promptNouns))]
	return adj + "-" + noun
}

// scrapeBgAgentPreview extracts the last meaningful line(s) from a background
// agent's terminal screen to use as a notification preview.
func (m Model) scrapeBgAgentPreview(pid int, meta bgAgentMeta) string {
	ws, ok := m.mgr.Workspace(meta.homeKey)
	if !ok {
		return ""
	}
	for _, a := range ws.Agents {
		if a.Pid() == pid {
			return lastMeaningfulLines(a.Render())
		}
	}
	return ""
}

// lastMeaningfulLines walks a rendered terminal screen backwards and returns
// the last non-empty line, truncated to 120 chars.
func lastMeaningfulLines(rendered string) string {
	lines := splitLines(rendered)
	for i := len(lines) - 1; i >= 0; i-- {
		if ln := strings.TrimSpace(lines[i]); ln != "" {
			if len(ln) > 120 {
				ln = ln[:120] + "…"
			}
			return ln
		}
	}
	return ""
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
		m.clearWorkspaceAttention()
	}
	m.status = ""
	var save tea.Cmd
	if m.state != nil {
		m.state.Touch(key)
		m.persistAgents(key)
		save = m.writeStateCmd()
	}
	return m, tea.Batch(m.loadCmd(), save)
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

// saveAgents snapshots the agent pool for key and returns a command that
// flushes state to disk off the UI goroutine. Call it after any change to the
// pool (spawn, close) or the focused agent, and return the command.
func (m *Model) saveAgents(key string) tea.Cmd {
	if m.state == nil {
		return nil
	}
	m.persistAgents(key)
	return m.writeStateCmd()
}

// writeStateCmd marshals the state in memory (cheap, on the UI goroutine) and
// returns a command that writes it to disk in the background. Superseded
// snapshots are dropped by Snapshot's sequence guard, so rapid-fire saves
// can't roll the file back; a final synchronous flush runs on exit.
func (m Model) writeStateCmd() tea.Cmd {
	if m.state == nil {
		return nil
	}
	sn, ok := m.state.Snapshot()
	if !ok {
		return nil
	}
	return func() tea.Msg {
		_ = sn.Write()
		return nil
	}
}

// FlushState synchronously writes ct's state to disk. main calls it once after
// the program exits so a pending background write can't be lost.
func (m Model) FlushState() {
	if m.state != nil {
		_ = m.state.Save()
	}
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
	if m.boardOpen {
		return m.handleBoard(msg)
	}
	// keyNotif is a legacy alias for the board (it replaced the notification
	// overlay), kept so existing muscle memory and configs work.
	if msg.String() == m.keyPalette || msg.String() == m.keyNotif {
		return m.openBoard()
	}
	if msg.String() == m.keyPrompt {
		return m.openQuickPrompt()
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
	switch msg.String() {
	case m.keyCycle:
		m.screen = m.screen.next()
		if m.screen == screenAgent && m.current != nil {
			m.clearWorkspaceAttention()
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
	case m.keyNextAgent:
		return m.rotateAgent(+1)
	case m.keyPrevAgent:
		return m.rotateAgent(-1)
	}
	if m.screen == screenTerminal && m.current != nil {
		key := m.current.key
		w, h := m.sessionSize()
		switch msg.String() {
		case m.keyTermSplitV:
			_, _ = m.mgr.SplitTermPane(key, m.current.path, m.ctrl.TermSpec(), session.SplitV, w, h)
			m.mgr.ResizeTermPanes(key, w, h)
			return m, nil
		case m.keyTermSplitH:
			_, _ = m.mgr.SplitTermPane(key, m.current.path, m.ctrl.TermSpec(), session.SplitH, w, h)
			m.mgr.ResizeTermPanes(key, w, h)
			return m, nil
		case m.keyTermCycle:
			m.mgr.CycleTermPane(key)
			return m, nil
		case m.keyTermZoom:
			m.mgr.ZoomTermPane(key)
			m.mgr.ResizeTermPanes(key, w, h)
			return m, nil
		case m.keyTermClose:
			_ = m.mgr.CloseTermPane(key)
			m.mgr.ResizeTermPanes(key, w, h)
			return m, nil
		}
	}

	if s := m.activeSession(); s != nil {
		s.SendKey(toUVKey(msg))
	}
	return m, nil
}

// rotateAgent moves the focused agent by delta (wrapping) and switches to the
// agent view. It's a no-op without at least two agents.
func (m Model) rotateAgent(delta int) (tea.Model, tea.Cmd) {
	if m.current == nil || m.current.ws == nil {
		return m, nil
	}
	n := len(m.current.ws.Agents)
	if n == 0 {
		return m, nil
	}
	m.current.ws.ActiveAgent = ((m.current.ws.ActiveAgent+delta)%n + n) % n
	m.screen = screenAgent
	m.clearWorkspaceAttention()
	return m, m.saveAgents(m.current.key)
}

// launchAgent spawns a new agent from the board's new-agent form, honouring the
// selected location (active/home worktree) and mode (foreground/background).
func (m Model) launchAgent() (tea.Model, tea.Cmd) {
	prompt := strings.TrimSpace(m.promptInput.Value())
	label := strings.TrimSpace(m.agentName.Value())
	if label == "" {
		label = randomAgentTitle()
	}
	m.formOpen = false
	m.boardOpen = false
	m.agentName.Blur()
	m.promptInput.Blur()
	w, h := m.sessionSize()

	if m.formLocation == 0 {
		// Active worktree.
		if m.current == nil {
			m.flash("no active workspace")
			return m, nil
		}
		spec := m.ctrl.NewAgentSpec(label)
		sess, err := m.mgr.SpawnAgent(m.current.key, m.current.path, spec, w, h)
		if err != nil {
			m.status = "spawn error: " + err.Error()
			return m, nil
		}
		if prompt != "" {
			_, _ = sess.WriteInput([]byte(prompt + "\n"))
		}
		save := m.saveAgents(m.current.key)
		if m.formBackground {
			m.flash("agent launched in background")
			return m, save
		}
		m.screen = screenAgent
		m.clearWorkspaceAttention()
		m.flash("agent launched")
		return m, save
	}

	// Home worktree.
	home, err := m.ctrl.GlobalConfigDir()
	if err != nil {
		m.status = "home dir error: " + err.Error()
		return m, nil
	}
	homeKey := "~/config"
	m.homeWSPath = home
	m.homeWSKey = homeKey
	if _, err := m.mgr.Activate(homeKey, home, m.workspaceSpecs(homeKey), w, h); err != nil {
		m.status = "open error: " + err.Error()
		return m, nil
	}
	if err := m.ctrl.EnsureHomeDirTrusted(); err != nil {
		m.status = "trust setup error: " + err.Error()
		return m, nil
	}

	if m.formBackground {
		spec := m.ctrl.PromptAgentSpec(label)
		sess, err := m.mgr.SpawnAgent(homeKey, home, spec, w, h)
		if err != nil {
			m.status = "spawn error: " + err.Error()
			return m, nil
		}
		if prompt != "" {
			_, _ = sess.WriteInput([]byte(prompt + "\n"))
		}
		m.bgAgentPIDs[sess.Pid()] = bgAgentMeta{label: label, homeKey: homeKey, homePath: home}
		var save tea.Cmd
		if m.state != nil {
			m.state.Touch(homeKey)
			save = m.saveAgents(homeKey)
		}
		m.flash("background agent launched")
		return m, save
	}

	// Home + foreground: spawn an interactive agent and navigate there.
	spec := m.ctrl.NewAgentSpec(label)
	sess, err := m.mgr.SpawnAgent(homeKey, home, spec, w, h)
	if err != nil {
		m.status = "spawn error: " + err.Error()
		return m, nil
	}
	if prompt != "" {
		_, _ = sess.WriteInput([]byte(prompt + "\n"))
	}
	// Persist the new active agent before activate() reads saved state.
	m.persistAgents(homeKey)
	mm, cmd := m.activate("~", "config", home)
	model := mm.(Model)
	model.screen = screenAgent
	model.flash("agent launched")
	return model, cmd
}

// handleMouseClick switches tabs when a left-click lands on a bar icon, and
// otherwise forwards the click to the active session.
func (m Model) handleMouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	mo := msg.Mouse()
	if mo.Button == tea.MouseLeft {
		if s, ok := m.tabAt(mo.X, mo.Y); ok {
			return m.selectTab(s), nil
		}
		if m.notifZoneAt(mo.X, mo.Y) {
			return m.openBoard()
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
			m.clearWorkspaceAttention()
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
	case "down":
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

// stopWorkspace closes a workspace's sessions (dropping its stale attention
// markers) and, if it was current, returns to the picker.
func (m *Model) stopWorkspace(key string) {
	m.mgr.Close(key)
	for pid, e := range m.attention {
		if e.key == key {
			delete(m.attention, pid)
		}
	}
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
	if m.homeWSPath != "" && filepath.Clean(m.homeWSPath) == path {
		return m.homeWSKey, true
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
