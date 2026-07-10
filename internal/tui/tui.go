// Package tui contains the Bubble Tea deck that powers the ct command.
package tui

import (
	"context"
	"errors"
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
	"github.com/KCaverly/caretaker/internal/usage"
)

// barHeight is the number of rows the top chrome occupies: the status bar and
// a light separator line directly beneath it.
const barHeight = 2

// Default reserved keys (not forwarded to embedded sessions); overridable via config.
const (
	defaultKeyCycle        = "alt+]"  // cycle to the next session view
	defaultKeyCycleBack    = "alt+["  // cycle to the previous session view
	defaultKeyGotoEditor   = "alt+1"  // jump to the editor view
	defaultKeyGotoAgent    = "alt+2"  // jump to the agent view
	defaultKeyGotoTerm     = "alt+3"  // jump to the terminal view
	defaultKeyPicker       = "ctrl+g" // return to the CT picker
	defaultKeyPalette      = "alt+a"  // open the agent board
	defaultKeyNextAgent    = "f4"     // focus the next agent in the pool
	defaultKeyPrevAgent    = "f3"     // focus the previous agent in the pool
	defaultKeyHelp         = "f1"     // toggle the help overlay
	defaultKeyGlobalConfig = "alt+g"  // open home-directory workspace
	defaultKeyPrompt       = "alt+y"  // new-agent form pre-set for a background home agent
	defaultKeyUsage        = "alt+u"  // usage overlay (agent screen only)

	// Terminal pane management — only intercepted when the terminal screen is active.
	defaultKeyTermSplitV     = "alt+v" // new pane to the right
	defaultKeyTermSplitH     = "alt+s" // new pane below
	defaultKeyTermZoom       = "alt+z" // toggle full-size
	defaultKeyTermClose      = "alt+x" // close active pane
	defaultKeyTermFocusLeft  = "alt+h" // focus the pane to the left
	defaultKeyTermFocusDown  = "alt+j" // focus the pane below
	defaultKeyTermFocusUp    = "alt+k" // focus the pane above
	defaultKeyTermFocusRight = "alt+l" // focus the pane to the right
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

// prev cycles among the session views in reverse (editor → terminal → agent →
// editor), mirroring next.
func (s screen) prev() screen {
	switch s {
	case screenTerminal:
		return screenAgent
	case screenAgent:
		return screenEditor
	default:
		return screenTerminal
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
	modeConfirmQuit // guarding ctrl+c while a hosted agent is busy
	modeConfirmStop // guarding "d" while the target worktree has a busy agent
)

// attnLevel ranks an agent's attention state, highest first when sorting.
// attnWaiting is derived live from the polled agent status and never stored;
// attnDone is an unread marker recorded when a transition is observed and
// cleared when the user views the workspace's agents.
type attnLevel int

const (
	attnNone    attnLevel = iota // nothing pending
	attnDone                     // busy → idle while unviewed (*)
	attnWaiting                  // live status "waiting" — needs input/permission (!)
)

// attnEntry is one stored unread marker (attnDone) for an agent, keyed by pid
// in Model.attention.
type attnEntry struct {
	level     attnLevel
	key       string // workspace key the agent belongs to
	startedAt int64  // unix ms of the owning process's start, from AgentStatus.StartedAt; 0 = unknown
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
	status   string    // right-hand status column
	attn     attnLevel // includes derived waiting
	num      int       // 1-based quick-jump number (first 9 agent rows)
}

// Focus order of the new-agent form's fields.
const (
	formFieldPrompt = iota
	formFieldWhere
	formFieldMode
	formFieldCount
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

	keyCycle, keyCycleBack, keyPicker        string
	keyGotoEditor, keyGotoAgent, keyGotoTerm string
	keyPalette, keyNextAgent, keyPrevAgent   string
	keyHelp, keyGlobalConfig, keyNotif       string
	keyPrompt, keyUsage                      string

	keyTermSplitV, keyTermSplitH            string
	keyTermCycle, keyTermZoom, keyTermClose string

	keyTermFocusLeft, keyTermFocusDown string
	keyTermFocusUp, keyTermFocusRight  string

	screen   screen
	current  *workspaceRef
	helpOpen bool

	// hintSeen records that the user has typed into a session at least once,
	// which retires the one-line "f1 help" hint shown beneath session bodies.
	// Reserved as a row until then (see sessionSize / sessionFooterH).
	hintSeen bool

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
	rootInput   textinput.Model

	// live agent statuses from `claude agents --json`, keyed by pid
	agentStatus     map[int]AgentStatus
	agentPrevStatus map[int]string    // pid → status from previous poll, for transition detection
	busySince       map[int]time.Time // pid → when its current busy stretch began
	pollFails       int               // consecutive status-poll failures, for the one-shot notice

	// stored unread markers (done) keyed by agent pid; waiting badges are
	// derived live from agentStatus and never stored here
	attention map[int]attnEntry

	// plan usage-limit gauge: the latest snapshot, whether one has ever
	// arrived, and a ring of five-hour utilization samples (trimmed to
	// usageHistoryWindow) that feeds the overlay's burn-rate row. The gauge
	// degrades silently — failures only leave the snapshot to go stale, which
	// hides the bar segment on its own.
	usageSnap      usage.Snapshot
	usageHave      bool
	usageOpen      bool // usage overlay visibility (agent screen only)
	usageHist      []usageSample
	usageFails     int // consecutive poll failures; tracked, never surfaced
	usageThreshold int // percent at/above which the bar segment shows

	// prompt input for the board's new-agent form
	promptInput textinput.Model

	// home workspace path/key, cached on first open for pathToKey lookups
	homeWSPath string
	homeWSKey  string

	// new-agent form (sub-state of the agent board)
	formOpen       bool
	formFocus      int  // formFieldPrompt..formFieldMode
	formLocation   int  // 0 = active worktree, 1 = home worktree
	formBackground bool // false = foreground (default), true = background

	status        string
	statusLevel   statusLevel // errors stick; info auto-expires
	statusAt      time.Time   // when a transient status was set (for auto-expiry)
	width, height int

	// groupsLoaded flips on the first applied deck load, so the picker can
	// show a scanning indicator instead of a wrong "no repos" message.
	groupsLoaded bool
}

// statusLevel classifies the footer status so styling and expiry don't rely
// on sniffing the text for the word "error".
type statusLevel int

const (
	statusInfo  statusLevel = iota // transient; auto-expires
	statusError                    // sticks until the next action clears it
)

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
	cycleBack := ctrl.CycleBackKey()
	if cycleBack == "" {
		cycleBack = defaultKeyCycleBack
	}
	gotoEditor, gotoAgent, gotoTerm := ctrl.GotoKeys()
	if gotoEditor == "" {
		gotoEditor = defaultKeyGotoEditor
	}
	if gotoAgent == "" {
		gotoAgent = defaultKeyGotoAgent
	}
	if gotoTerm == "" {
		gotoTerm = defaultKeyGotoTerm
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
	// Notif is a legacy board alias, retired by default (empty). A user-set
	// empty stays empty and never matches — key strings are never empty.
	notif := ctrl.NotifKey()
	prompt := ctrl.PromptKey()
	if prompt == "" {
		prompt = defaultKeyPrompt
	}
	usageKey := ctrl.UsageKey()
	if usageKey == "" {
		usageKey = defaultKeyUsage
	}
	termSplitV, termSplitH, termCycle, termZoom, termClose := ctrl.TermPaneKeys()
	if termSplitV == "" {
		termSplitV = defaultKeyTermSplitV
	}
	if termSplitH == "" {
		termSplitH = defaultKeyTermSplitH
	}
	// TermCycle is retired by default (empty); directional focus supersedes it.
	// A user-set empty stays empty, so no default fallback here.
	if termZoom == "" {
		termZoom = defaultKeyTermZoom
	}
	if termClose == "" {
		termClose = defaultKeyTermClose
	}
	termFocusLeft, termFocusDown, termFocusUp, termFocusRight := ctrl.TermFocusKeys()
	if termFocusLeft == "" {
		termFocusLeft = defaultKeyTermFocusLeft
	}
	if termFocusDown == "" {
		termFocusDown = defaultKeyTermFocusDown
	}
	if termFocusUp == "" {
		termFocusUp = defaultKeyTermFocusUp
	}
	if termFocusRight == "" {
		termFocusRight = defaultKeyTermFocusRight
	}

	return Model{
		ctrl: ctrl, mgr: mgr, state: state.Load(),
		keyCycle: cycle, keyCycleBack: cycleBack, keyPicker: picker,
		keyGotoEditor: gotoEditor, keyGotoAgent: gotoAgent, keyGotoTerm: gotoTerm,
		keyPalette: palette, keyNextAgent: next, keyPrevAgent: prev,
		keyHelp: help, keyGlobalConfig: globalConfig, keyNotif: notif,
		keyPrompt: prompt, keyUsage: usageKey,
		usageThreshold: ctrl.UsageThreshold(),
		keyTermSplitV:  termSplitV, keyTermSplitH: termSplitH,
		keyTermCycle: termCycle, keyTermZoom: termZoom, keyTermClose: termClose,
		keyTermFocusLeft: termFocusLeft, keyTermFocusDown: termFocusDown,
		keyTermFocusUp: termFocusUp, keyTermFocusRight: termFocusRight,
		filter: filter, nameInput: name, rootInput: rootInput,
		promptInput:     promptInput,
		focus:           focusNew,
		agentPrevStatus: map[int]string{},
		attention:       map[int]attnEntry{},
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

// statusExpireMsg is the dedicated one-shot flash-expiry tick scheduled by
// flashCmd. It carries the statusAt it was armed for so a newer flash set before
// it fires is never cleared early: only the flash that scheduled this tick may
// expire it. This is what keeps a flash on an idle deck (whose agent-status poll
// stretches to 30s) from lingering ~26s past its 4s TTL.
type statusExpireMsg struct{ at time.Time }

type statusMsg struct {
	byPid map[int]AgentStatus
	err   error
}

// usageTickMsg fires on the usage-poll timer; usageMsg carries the result of
// one plan-usage fetch.
type usageTickMsg struct{}

type usageMsg struct {
	snap usage.Snapshot
	err  error
}

// usageSample is one successful poll's five-hour utilization, kept in
// Model.usageHist so the overlay can estimate the burn rate.
type usageSample struct {
	at   time.Time
	util float64
}

// Plan-usage cadence and freshness bounds. The poll is slow (the window moves
// in minutes, not seconds); a snapshot older than usageStaleAfter no longer
// steers the bar segment, and samples beyond usageHistoryWindow no longer
// weigh on the burn-rate estimate.
const (
	usagePollInterval  = 60 * time.Second
	usageStaleAfter    = 5 * time.Minute
	usageHistoryWindow = time.Hour
)

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	if m.screen == screenSetup {
		return tea.Batch(textinput.Blink, m.repaintCmd())
	}
	return tea.Batch(m.loadCmd(), textinput.Blink, m.repaintCmd(), m.pollStatusCmd(), m.pollUsageCmd())
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

// scheduleStatusTick re-arms the poll timer at statusTickInterval.
func (m Model) scheduleStatusTick() tea.Cmd {
	return tea.Tick(m.statusTickInterval(), func(time.Time) tea.Msg { return statusTickMsg{} })
}

// pollUsageCmd runs one plan-usage fetch off the UI goroutine.
func (m Model) pollUsageCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		snap, err := usage.Fetch(ctx)
		return usageMsg{snap: snap, err: err}
	}
}

// scheduleUsageTick re-arms the usage poll at usagePollInterval.
func (m Model) scheduleUsageTick() tea.Cmd {
	return tea.Tick(usagePollInterval, func(time.Time) tea.Msg { return usageTickMsg{} })
}

// usageFresh reports whether the latest snapshot is recent enough to steer
// the bar segment; the overlay keeps showing whatever it has regardless.
func (m Model) usageFresh() bool {
	return m.usageHave && time.Since(m.usageSnap.FetchedAt) <= usageStaleAfter
}

// statusTickInterval picks the agent-poll cadence: 2s while any agent is
// active (busy/waiting), 5s while ct hosts idle sessions, and 30s when ct hosts
// nothing at all — the slow tick only keeps a lazy watch for claude sessions
// started outside ct in known worktrees, so their deck badges still appear
// (just up to 30s late) without ct spawning a subprocess every 5s for an empty
// deck.
func (m Model) statusTickInterval() time.Duration {
	interval := 5 * time.Second
	if m.mgr == nil || m.mgr.Count() == 0 {
		interval = 30 * time.Second
	}
	for _, st := range m.agentStatus {
		if st.Status == "busy" || st.Status == "waiting" {
			return 2 * time.Second
		}
	}
	return interval
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

// repaintCmd blocks until a session's screen changes, then asks for a
// re-render. WaitDirty applies the leading-edge coalescing window so a burst of
// pty output (a build, a streaming agent) collapses into a capped repaint rate
// instead of re-serialising every vt buffer as fast as frames complete, while a
// lone keystroke echo still repaints with no added latency.
func (m Model) repaintCmd() tea.Cmd {
	mgr := m.mgr
	return func() tea.Msg {
		mgr.WaitDirty()
		return dirtyMsg{}
	}
}

func (m Model) sessionSize() (int, int) {
	return m.width, max(1, m.height-barHeight-m.sessionFooterH())
}

// dismissHint retires the session help hint on the user's first keystroke into
// a session and hands the reclaimed row back to the workspace's sessions. It's
// a no-op once the hint has already been dismissed.
func (m Model) dismissHint() Model {
	if m.hintSeen {
		return m
	}
	m.hintSeen = true
	if m.current != nil {
		w, h := m.sessionSize()
		m.mgr.ResizeWorkspace(m.current.key, w, h)
	}
	return m
}

// toggleZoom maximizes the active terminal pane or restores the split layout,
// resizing the panes to match. Shared by the zoom key and a click on the bar's
// zoom toggle. A no-op without a current workspace.
func (m Model) toggleZoom() (tea.Model, tea.Cmd) {
	if m.current == nil {
		return m, nil
	}
	key := m.current.key
	w, h := m.sessionSize()
	m.mgr.ZoomTermPane(key)
	m.mgr.ResizeTermPanes(key, w, h)
	return m, nil
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
// nothing while the picker, setup, or a full-body overlay (help, board,
// usage) is shown; the editor or focused agent on their screens; and on the
// terminal screen either the focused pane (zoomed/single) or every pane in
// the split layout. Mirrors the branch structure of View.
func (m Model) visibleSessions() []*session.Session {
	if m.helpOpen || m.boardOpen || m.usageOpen || m.screen == screenPicker || m.screen == screenSetup {
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
		// Only the current workspace is resized; background ones are brought up
		// to date by Activate when they next become current.
		if m.current != nil {
			w, h := m.sessionSize()
			m.mgr.ResizeWorkspace(m.current.key, w, h)
		}
		return m, nil

	case loadedMsg:
		if msg.seq != m.ctrl.loadSeq.Load() {
			return m, nil // superseded by a newer in-flight load
		}
		if msg.err != nil {
			m.setError("load error: " + msg.err.Error())
			return m, nil
		}
		m.groups = msg.groups
		m.groupsLoaded = true
		m.recomputeMatches()
		m.recomputeActive()
		m.computeRecentRanks()
		return m, nil

	case createdMsg:
		if msg.err != nil {
			m.setError("create error: " + msg.err.Error())
			return m, m.loadCmd()
		}
		return m.activate(msg.wt.Repo, msg.wt.Name, msg.wt.Path)

	case actionDoneMsg:
		if msg.err != nil {
			m.setError(msg.err.Error())
		}
		return m, m.loadCmd()

	case dirtyMsg:
		return m, m.repaintCmd()

	case statusTickMsg:
		m.maybeExpireStatus()
		return m, m.pollStatusCmd()

	case statusExpireMsg:
		// Ignore a tick left over from a flash that a newer one has since
		// replaced (statusAt moved); otherwise the fresh flash would be cleared
		// early. maybeExpireStatus's own TTL check makes the surviving tick a
		// no-op if the status was already replaced by a non-transient one.
		if msg.at.Equal(m.statusAt) {
			m.maybeExpireStatus()
		}
		return m, nil

	case statusMsg:
		var flashTick tea.Cmd
		if msg.err != nil {
			// Surface a persistent outage once (transient, auto-expires): with
			// the poll dead, every badge/board feature silently freezes and the
			// user should know why.
			m.pollFails++
			if m.pollFails == 3 {
				flashTick = m.flashCmd("agent status unavailable (claude agents failing)")
			}
		}
		if msg.err == nil {
			m.pollFails = 0
			bell := false
			for pid, st := range msg.byPid {
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
					m.recordAttention(pid, attnDone, key, st.StartedAt)
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
				if prev != "busy" {
					continue
				}
				if last, ok := m.agentStatus[pid]; ok {
					if key, ok := m.pathToKey(last.Cwd); ok {
						m.recordAttention(pid, attnDone, key, last.StartedAt)
						bell = true
					}
				}
			}
			if bell {
				fmt.Fprint(os.Stderr, "\a")
			}
			// Track when each agent's current busy stretch began, for the
			// board's elapsed-time column. Accurate to one poll interval.
			if m.busySince == nil {
				m.busySince = map[int]time.Time{}
			}
			for pid, st := range msg.byPid {
				if st.Status == "busy" {
					if _, ok := m.busySince[pid]; !ok {
						m.busySince[pid] = time.Now()
					}
				} else {
					delete(m.busySince, pid)
				}
			}
			for pid := range m.busySince {
				if _, ok := msg.byPid[pid]; !ok {
					delete(m.busySince, pid)
				}
			}
			// Drop stored attention entries whose pid has been recycled: if the
			// polled process at pid started at a different, known time than the
			// entry's known startedAt, the pid now belongs to a different agent
			// and the marker no longer applies.
			for pid, e := range m.attention {
				if e.startedAt == 0 {
					continue
				}
				if st, ok := msg.byPid[pid]; ok && st.StartedAt != 0 && st.StartedAt != e.startedAt {
					delete(m.attention, pid)
				}
			}
			m.agentPrevStatus = make(map[int]string, len(msg.byPid))
			for pid, st := range msg.byPid {
				m.agentPrevStatus[pid] = st.Status
			}
			m.agentStatus = msg.byPid
		}
		m.maybeExpireStatus()
		return m, tea.Batch(m.scheduleStatusTick(), flashTick)

	case usageTickMsg:
		return m, m.pollUsageCmd()

	case usageMsg:
		// Not signed into Claude Code: the feature simply doesn't exist here.
		// Stop polling — no retries, no error, nothing to show.
		if errors.Is(msg.err, usage.ErrNoCredentials) {
			return m, nil
		}
		if msg.err != nil {
			// Degrade silently: the snapshot goes stale and the bar segment
			// hides on its own; keep polling at the normal cadence.
			m.usageFails++
			return m, m.scheduleUsageTick()
		}
		m.usageFails = 0
		m.usageSnap = msg.snap
		m.usageHave = true
		if w := msg.snap.FiveHour; w != nil {
			m.usageHist = append(m.usageHist, usageSample{at: msg.snap.FetchedAt, util: w.Utilization})
		}
		cutoff := time.Now().Add(-usageHistoryWindow)
		for len(m.usageHist) > 0 && m.usageHist[0].at.Before(cutoff) {
			m.usageHist = m.usageHist[1:]
		}
		return m, m.scheduleUsageTick()

	case tea.MouseClickMsg:
		return m.handleMouseClick(msg)
	case tea.MouseReleaseMsg:
		m.forwardMouse(msg)
		return m, nil
	case tea.MouseWheelMsg:
		if m.screen == screenPicker && !m.helpOpen && !m.boardOpen {
			return m.deckWheel(msg)
		}
		m.forwardMouse(msg)
		return m, nil
	case tea.MouseMotionMsg:
		m.forwardMouse(msg)
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.PasteMsg:
		// Bubble Tea v2 delivers bracketed paste as its own message type, not a
		// KeyPressMsg, so it needs routing of its own — see handlePaste.
		return m.handlePaste(msg)

	default:
		// Route everything else (notably the cursor-blink tick from
		// textinput.Blink, started in Init) to whichever text input is
		// currently focused, so its blink loop keeps re-arming itself.
		// KeyPressMsg, paste, and mouse messages never reach here — they have
		// explicit cases above.
		mm, cmd := m.routeToFocusedInputs(msg)
		return mm, cmd
	}
}

// routeToFocusedInputs forwards msg to every currently-focused textinput and
// batches their commands. It backs both the default branch (where the
// cursor-blink tick must reach the focused input so its blink loop re-arms)
// and the non-session path of the paste router.
func (m Model) routeToFocusedInputs(msg tea.Msg) (Model, tea.Cmd) {
	var cmds []tea.Cmd
	if m.filter.Focused() {
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		cmds = append(cmds, cmd)
	}
	if m.nameInput.Focused() {
		var cmd tea.Cmd
		m.nameInput, cmd = m.nameInput.Update(msg)
		cmds = append(cmds, cmd)
	}
	if m.rootInput.Focused() {
		var cmd tea.Cmd
		m.rootInput, cmd = m.rootInput.Update(msg)
		cmds = append(cmds, cmd)
	}
	if m.promptInput.Focused() {
		var cmd tea.Cmd
		m.promptInput, cmd = m.promptInput.Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

// handlePaste routes a bracketed-paste message. Pastes need the same routing
// keys get: without a case of their own they fall to the default branch and
// reach only a focused textinput, so on a session screen (where none is
// focused) the paste silently vanishes — the bug this fixes.
//
//   - On overlays (help/board/usage), setup, and the picker the paste behaves
//     like typed input: it goes to whichever textinput is focused (the picker
//     filter, the name/root/prompt inputs), and is otherwise swallowed — it
//     never leaks into a session drawn beneath an overlay.
//   - On a bare session screen it is handed to the active program through the
//     emulator's paste-aware path (Session.Paste), which honors the child's
//     bracketed-paste mode.
func (m Model) handlePaste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	onSession := m.screen != screenPicker && m.screen != screenSetup &&
		!m.helpOpen && !m.boardOpen && !m.usageOpen
	if onSession {
		if s := m.activeSession(); s != nil {
			m = m.dismissHint() // first input into a session retires the hint
			s.Paste(msg.Content)
		}
		return m, nil
	}
	mm, cmd := m.routeToFocusedInputs(msg)
	// Mirror handleNewKey: keep the repo matches in sync when the paste lands
	// in the picker's filter.
	if mm.filter.Focused() {
		mm.recomputeMatches()
	}
	return mm, cmd
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
func (m Model) recordAttention(pid int, level attnLevel, key string, startedAt int64) {
	if cur, ok := m.attention[pid]; ok && cur.level >= level {
		return
	}
	m.attention[pid] = attnEntry{level: level, key: key, startedAt: startedAt}
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

// attnSummary counts, for the bar badge: worktrees with a live-waiting agent
// and worktrees with unread completions.
func (m Model) attnSummary() (waiting, done int) {
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
		if e.level == attnDone {
			doneKeys[e.key] = true
		}
	}
	return len(waitKeys), len(doneKeys)
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

// boardStatus renders the right-hand column for an agent row: the live polled
// status.
func (m Model) boardStatus(pid int, attn attnLevel) string {
	st := m.agentStatus[pid]
	switch st.Status {
	case "busy":
		// Elapsed time changes triage: a 12-minute agent means something
		// different from a 20-second one. Shown once it's meaningful (>=5s).
		if since, ok := m.busySince[pid]; ok {
			if d := time.Since(since); d >= 5*time.Second {
				return "working · " + humanDur(d)
			}
		}
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
	// keyNotif is the legacy board alias (retired by default). When a user rebinds
	// it, it doubles as a board-close key; at its empty default it never matches
	// (key strings are never empty), so ctrl+n stays pure list-down navigation.
	if m.keyNotif != "" && msg.String() == m.keyNotif {
		m.boardOpen = false
		return m, nil
	}
	switch msg.String() {
	case "esc", "q", m.keyPalette:
		m.boardOpen = false
		return m, nil
	case m.keyPrompt:
		return m.openQuickPrompt()
	case "up", "k", "ctrl+p":
		if m.boardCursor > 0 {
			m.boardCursor--
		}
		return m, nil
	case "down", "j", "ctrl+n":
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
		m.setError("home dir error: " + err.Error())
		return m, nil
	}
	m.homeWSPath = home
	m.homeWSKey = "~/config"
	return m.activate("~", "config", home)
}

// openNewAgentForm switches the board into the new-agent form with defaults:
// active worktree (home when no workspace is active), foreground, focus on the
// prompt field. Agents are no longer manually named — claude names the session
// after its topic — so the form opens straight on the prompt.
func (m Model) openNewAgentForm() tea.Model {
	m.boardOpen = true
	m.formOpen = true
	m.formFocus = formFieldPrompt
	m.formLocation = 0
	if m.current == nil {
		m.formLocation = 1
	}
	m.formBackground = false
	m.promptInput.SetValue("")
	m.promptInput.Focus()
	return m
}

// openQuickPrompt opens the new-agent form pre-set for a background home
// worktree agent with the prompt field focused (the ctrl+y shortcut).
func (m Model) openQuickPrompt() (tea.Model, tea.Cmd) {
	mm := m.openNewAgentForm().(Model)
	mm.formLocation = 1
	mm.formBackground = true
	return mm, mm.promptInput.Focus()
}

// setFormFocus moves the new-agent form's focus to field f (wrapping), keeping
// the prompt input's focus state in sync.
func (m Model) setFormFocus(f int) (Model, tea.Cmd) {
	m.formFocus = ((f % formFieldCount) + formFieldCount) % formFieldCount
	m.promptInput.Blur()
	if m.formFocus == formFieldPrompt {
		return m, m.promptInput.Focus()
	}
	return m, nil
}

// handleBoardForm drives the new-agent form: tab/shift+tab (or ↑↓, or the
// ctrl+n/ctrl+p list-nav dialect — unused by the prompt textinput) move between
// the prompt, where, and mode fields; space or ←/→ flip the focused toggle;
// enter launches from any field.
func (m Model) handleBoardForm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.formOpen = false
		m.promptInput.Blur()
		return m, nil
	case "tab", "down", "ctrl+n":
		return m.setFormFocus(m.formFocus + 1)
	case "shift+tab", "up", "ctrl+p":
		return m.setFormFocus(m.formFocus - 1)
	case "enter":
		return m.launchAgent()
	}
	if m.formFocus == formFieldPrompt {
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

// Agents the user doesn't name get a random adjective-noun title (e.g.
// "amber-fox") — a stable, recognisable placeholder until a future change
// summarises the conversation into something descriptive.
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

// activate ensures the workspace's sessions are running and switches to the
// last screen used in that worktree (editor on first open). A brand-new worktree
// starts one fresh claude session; reopening resumes the agent pool.
func (m Model) activate(repoName, wtName, dir string) (tea.Model, tea.Cmd) {
	key := repoName + "/" + wtName
	w, h := m.sessionSize()
	ws, err := m.mgr.Activate(key, dir, m.workspaceSpecs(key), w, h)
	if err != nil {
		m.setError("open error: " + err.Error())
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
	// key dismisses it. A key that is one of ct's own reserved action keys does
	// double duty: it closes help AND performs its action, so opening help and
	// pressing e.g. the cycle key both dismisses the overlay and cycles — the
	// user shouldn't have to press it twice. We achieve that by closing help then
	// re-dispatching the same message through the normal path. The help key is
	// excluded (its "action" is toggling help, already satisfied by the close),
	// so it never re-opens what it just closed. Every non-reserved key (letters,
	// esc, enter, space, "?") only closes: re-dispatching one would leak the
	// keystroke into the embedded session drawn beneath the overlay, which must
	// never happen.
	if m.helpOpen {
		m.helpOpen = false
		if m.isReservedActionKey(msg.String()) {
			return m.handleKey(msg)
		}
		return m, nil
	}
	if msg.String() == m.keyHelp {
		m.helpOpen = true
		return m, nil
	}
	// The usage overlay is modal like the board, but read-only: it only closes
	// on esc or its own key, and swallows everything else so no keystroke
	// leaks into the session underneath.
	if m.usageOpen {
		if s := msg.String(); s == "esc" || s == m.keyUsage {
			m.usageOpen = false
		}
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
	// Direct-jump keys work from session screens and from the picker whenever a
	// workspace is active; they no-op with none.
	switch msg.String() {
	case m.keyGotoEditor:
		return m.gotoScreen(screenEditor)
	case m.keyGotoAgent:
		return m.gotoScreen(screenAgent)
	case m.keyGotoTerm:
		return m.gotoScreen(screenTerminal)
	}
	if m.screen != screenPicker {
		return m.handleSessionKey(msg)
	}
	return m.handlePicker(msg)
}

// gotoScreen jumps straight to a session view, mirroring selectTab's semantics:
// a no-op without an active workspace, and clearing the workspace's attention
// markers when landing on the agent screen.
func (m Model) gotoScreen(s screen) (tea.Model, tea.Cmd) {
	if m.current == nil {
		return m, nil
	}
	m.screen = s
	if s == screenAgent {
		m.clearWorkspaceAttention()
	}
	return m, nil
}

// isReservedActionKey reports whether s is one of ct's own reserved keys that
// should still fire when it dismisses the help overlay (see handleKey). The
// globally-reserved keys apply on every screen; keyHelp is intentionally absent
// (dismissing help already satisfies its toggle, and re-dispatching it would
// re-open the overlay). The usage key is only reserved on the agent screen and
// the terminal-pane keys only on the terminal screen — elsewhere they are
// ordinary session input, so treating them as reserved would forward the key
// into the session beneath, exactly the leak the help swallow prevents.
func (m Model) isReservedActionKey(s string) bool {
	switch s {
	case m.keyCycle, m.keyCycleBack, m.keyPicker, m.keyPalette, m.keyNotif, m.keyPrompt,
		m.keyGlobalConfig, m.keyNextAgent, m.keyPrevAgent,
		m.keyGotoEditor, m.keyGotoAgent, m.keyGotoTerm:
		return true
	}
	if s == m.keyUsage && m.screen == screenAgent {
		return true
	}
	if m.screen == screenTerminal {
		switch s {
		case m.keyTermSplitV, m.keyTermSplitH, m.keyTermCycle, m.keyTermZoom, m.keyTermClose,
			m.keyTermFocusLeft, m.keyTermFocusDown, m.keyTermFocusUp, m.keyTermFocusRight:
			return true
		}
	}
	return false
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
			m.setError(err.Error())
			return m, nil
		}
		if err := config.Save(m.configPath, abs); err != nil {
			m.setError("save error: " + err.Error())
			return m, nil
		}
		m.ctrl.SetRoot(abs)
		m.screen = screenPicker
		flashTick := m.flashCmd("config saved — welcome to caretaker!")
		return m, tea.Batch(m.loadCmd(), m.pollStatusCmd(), m.pollUsageCmd(), flashTick)
	}
	var cmd tea.Cmd
	m.rootInput, cmd = m.rootInput.Update(msg)
	m.status = ""
	return m, cmd
}

func (m Model) handleSessionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// The usage key is only reserved on the agent screen (where the gauge
	// lives); everywhere else it stays an ordinary session key and is
	// forwarded below.
	if msg.String() == m.keyUsage && m.screen == screenAgent {
		m.usageOpen = true
		return m, nil
	}
	switch msg.String() {
	case m.keyCycle:
		m.screen = m.screen.next()
		if m.screen == screenAgent && m.current != nil {
			m.clearWorkspaceAttention()
		}
		return m, nil
	case m.keyCycleBack:
		m.screen = m.screen.prev()
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
		// Refresh the deck on entry: dirty markers and worktree lists go stale
		// while you edit inside a session, and this is the moment they're read.
		return m, m.loadCmd()
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
		case m.keyTermFocusLeft:
			m.mgr.FocusTermPaneDir(key, session.FocusLeft, w, h)
			return m, nil
		case m.keyTermFocusDown:
			m.mgr.FocusTermPaneDir(key, session.FocusDown, w, h)
			return m, nil
		case m.keyTermFocusUp:
			m.mgr.FocusTermPaneDir(key, session.FocusUp, w, h)
			return m, nil
		case m.keyTermFocusRight:
			m.mgr.FocusTermPaneDir(key, session.FocusRight, w, h)
			return m, nil
		case m.keyTermZoom:
			return m.toggleZoom()
		case m.keyTermClose:
			_ = m.mgr.CloseTermPane(key)
			m.mgr.ResizeTermPanes(key, w, h)
			return m, nil
		}
	}

	if s := m.activeSession(); s != nil {
		m = m.dismissHint()
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
	// The form no longer takes a label; every agent gets an auto-generated
	// placeholder title until a future change summarises it.
	label := randomAgentTitle()
	m.formOpen = false
	m.boardOpen = false
	m.promptInput.Blur()
	w, h := m.sessionSize()

	if m.formLocation == 0 {
		// Active worktree.
		if m.current == nil {
			return m, m.flashCmd("no active workspace")
		}
		spec := m.ctrl.NewAgentSpec(label)
		sess, err := m.mgr.SpawnAgent(m.current.key, m.current.path, spec, w, h)
		if err != nil {
			m.setError("spawn error: " + err.Error())
			return m, nil
		}
		if prompt != "" {
			_, _ = sess.WriteInput([]byte(prompt + "\n"))
		}
		save := m.saveAgents(m.current.key)
		if m.formBackground {
			return m, tea.Batch(save, m.flashCmd("agent launched in background"))
		}
		m.screen = screenAgent
		m.clearWorkspaceAttention()
		return m, tea.Batch(save, m.flashCmd("agent launched"))
	}

	// Home worktree.
	home, err := m.ctrl.GlobalConfigDir()
	if err != nil {
		m.setError("home dir error: " + err.Error())
		return m, nil
	}
	homeKey := "~/config"
	m.homeWSPath = home
	m.homeWSKey = homeKey
	if _, err := m.mgr.Activate(homeKey, home, m.workspaceSpecs(homeKey), w, h); err != nil {
		m.setError("open error: " + err.Error())
		return m, nil
	}
	if err := m.ctrl.EnsureHomeDirTrusted(); err != nil {
		m.setError("trust setup error: " + err.Error())
		return m, nil
	}

	if m.formBackground {
		spec := m.ctrl.PromptAgentSpec(label)
		sess, err := m.mgr.SpawnAgent(homeKey, home, spec, w, h)
		if err != nil {
			m.setError("spawn error: " + err.Error())
			return m, nil
		}
		if prompt != "" {
			_, _ = sess.WriteInput([]byte(prompt + "\n"))
		}
		var save tea.Cmd
		if m.state != nil {
			m.state.Touch(homeKey)
			save = m.saveAgents(homeKey)
		}
		return m, tea.Batch(save, m.flashCmd("background agent launched"))
	}

	// Home + foreground: spawn an interactive agent and navigate there.
	spec := m.ctrl.NewAgentSpec(label)
	sess, err := m.mgr.SpawnAgent(homeKey, home, spec, w, h)
	if err != nil {
		m.setError("spawn error: " + err.Error())
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
	return model, tea.Batch(cmd, model.flashCmd("agent launched"))
}

// handleMouseClick switches tabs when a left-click lands on a bar icon, focuses
// the terminal pane under a click in a split layout, and otherwise forwards the
// click to the active session.
func (m Model) handleMouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	mo := msg.Mouse()
	if mo.Button == tea.MouseLeft {
		if s, ok := m.tabAt(mo.X, mo.Y); ok {
			return m.selectTab(s)
		}
		if m.notifZoneAt(mo.X, mo.Y) {
			return m.openBoard()
		}
		if m.usageZoneAt(mo.X, mo.Y) {
			m.usageOpen = true
			return m, nil
		}
		if m.paneZoomAt(mo.X, mo.Y) {
			return m.toggleZoom()
		}
		if m.screen == screenPicker && !m.helpOpen {
			if mm, cmd, ok := m.deckClick(mo.X, mo.Y); ok {
				return mm, cmd
			}
		}
		// Focus the terminal pane under the click before the click reaches the
		// program running in it. Panes aren't drawn while an overlay is up, so a
		// click then must not steal pane focus.
		if m.screen == screenTerminal && m.current != nil &&
			!m.helpOpen && !m.boardOpen && !m.usageOpen && mo.Y >= barHeight {
			w, h := m.sessionSize()
			m.mgr.FocusTermPaneAt(m.current.key, mo.X, mo.Y-barHeight, w, h)
		}
	}
	m.forwardMouse(msg)
	return m, nil
}

// deckWheel scrolls the deck section under the pointer: the wheel moves that
// section's cursor (focusing the section, mirroring what a click does), so
// the lists scroll the way every other list on screen does.
func (m Model) deckWheel(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	mo := msg.Mouse()
	delta := 1
	if mo.Button == tea.MouseWheelUp {
		delta = -1
	} else if mo.Button != tea.MouseWheelDown {
		return m, nil
	}
	if m.mode != modeNormal {
		return m, nil
	}

	by := mo.Y - barHeight
	L := m.deckLayout(m.height - barHeight)
	switch {
	case by >= 1 && by < 1+L.newContentH: // NEW box
		var cmd tea.Cmd
		if m.focus != focusNew {
			m.focus = focusNew
			cmd = m.filter.Focus()
		}
		m.newCursor = clamp(m.newCursor+delta, 0, max(0, len(m.repoMatches)-1))
		return m, cmd
	case by >= L.newOuterH+1 && by < L.newOuterH+1+L.activeContentH: // ACTIVE box
		m.focus = focusActive
		m.filter.Blur()
		m.activeCursor = clamp(m.activeCursor+delta, 0, max(0, len(m.active)-1))
		return m, nil
	}
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
// session tabs only switch when a workspace is active. Switching to the
// picker refreshes the deck, matching the picker key.
func (m Model) selectTab(s screen) (tea.Model, tea.Cmd) {
	if s == screenPicker || m.current != nil {
		wasSession := m.screen != screenPicker
		m.screen = s
		if s == screenAgent && m.current != nil {
			m.clearWorkspaceAttention()
		}
		if s == screenPicker && wasSession {
			return m, m.loadCmd()
		}
	}
	return m, nil
}

// forwardMouse relays a mouse event to the session under the pointer. In an
// unzoomed split terminal layout it hit-tests the pane rectangles and delivers
// pane-local coordinates to that pane's program, so a click or wheel scroll acts
// on the pane it lands on rather than only the focused one; events over a
// divider or the footer are dropped. Everywhere else it relays to the active
// session translated below the bar. The emulator only encodes the event if the
// program has enabled mouse reporting.
func (m Model) forwardMouse(msg tea.MouseMsg) {
	if m.screen == screenPicker {
		return
	}
	mo := msg.Mouse()
	if mo.Y < barHeight {
		return
	}
	// Target session and the pane origin to subtract from the pointer. Default:
	// the active session, offset only by the bar (bx, by stay 0).
	s := m.activeSession()
	bx, by := 0, 0
	if m.screen == screenTerminal && m.current != nil && m.current.ws != nil {
		ws := m.current.ws
		if !ws.TermZoomed && len(ws.Terms) > 1 && ws.TermLayout != nil {
			w, h := m.sessionSize()
			bounds := session.ComputePaneBounds(ws.TermLayout, 0, 0, w, h)
			idx := session.PaneAt(bounds, mo.X, mo.Y-barHeight)
			if idx < 0 || idx >= len(ws.Terms) || ws.Terms[idx] == nil {
				return // divider, footer, or outside every pane
			}
			for _, b := range bounds {
				if b.Idx == idx {
					bx, by = b.X, b.Y
					break
				}
			}
			s = ws.Terms[idx]
		}
	}
	if s == nil {
		return
	}
	shift := func(p tea.Mouse) uv.Mouse {
		return uv.Mouse{X: p.X - bx, Y: p.Y - barHeight - by, Button: p.Button, Mod: uv.KeyMod(p.Mod)}
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
	case modeConfirmQuit:
		return m.handleConfirmQuitKey(msg)
	case modeConfirmStop:
		return m.handleConfirmStopKey(msg)
	}
	if msg.String() == "ctrl+c" {
		// Quitting runs mgr.CloseAll(), which SIGKILLs every hosted pty — every
		// agent and nvim. Guard it only when a hosted agent is mid-task; the
		// common (nothing busy) case quits with no friction.
		if n := m.busyHostedAgents(""); n > 0 {
			m.mode = modeConfirmQuit
			m.status = fmt.Sprintf("%d busy agent(s) running — quit anyway? (y/n)", n)
			return m, nil
		}
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
		// j/k are left to fall through to the fuzzy filter, which owns this
		// section's input.
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
	// Shared list-nav dialect: arrows and ctrl+p/ctrl+n move, and — because the
	// ACTIVE list owns no text input — j/k do too.
	case "up", "k", "ctrl+p":
		if m.activeCursor > 0 {
			m.activeCursor--
		}
	case "down", "j", "ctrl+n":
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
			key := wsKey(it.repo.Name, it.view.WT.Name)
			// Stopping hard-kills this workspace's sessions. Guard it when the
			// worktree has a busy agent; otherwise stop instantly as before.
			if m.busyHostedAgents(key) > 0 {
				m.mode = modeConfirmStop
				m.status = fmt.Sprintf("%q has a busy agent — stop anyway? (y/n)", it.view.WT.Name)
				return m, nil
			}
			m.stopWorkspace(key)
			return m, tea.Batch(m.loadCmd(), m.flashCmd("stopped "+it.view.WT.Name))
		}
	case "x":
		if it, ok := m.selectedActive(); ok && !it.view.WT.IsMain {
			m.mode = modeConfirmRemove
			// git worktree remove --force discards uncommitted work silently, so
			// when the worktree is dirty the prompt must say so. Either way the
			// user picks whether the branch goes with it.
			if it.view.Dirty {
				m.status = fmt.Sprintf("remove worktree %q? UNCOMMITTED CHANGES will be lost. (y = remove + delete branch / b = keep branch / n)", it.view.WT.Name)
			} else {
				m.status = fmt.Sprintf("remove worktree %q? (y = remove + delete branch / b = keep branch / n)", it.view.WT.Name)
			}
		}
	case "1", "2", "3":
		// The deck badges these ranks next to the most recently opened
		// worktrees; pressing the digit jumps straight there.
		if it, ok := m.rankedActive(int(msg.String()[0] - '0')); ok {
			return m.activate(it.repo.Name, it.view.WT.Name, it.view.WT.Path)
		}
	}
	return m, nil
}

// rankedActive returns the active item badged with the given recency rank
// (1..3), if any.
func (m Model) rankedActive(rank int) (activeItem, bool) {
	for _, it := range m.active {
		if m.recentRank[wsKey(it.repo.Name, it.view.WT.Name)] == rank {
			return it, true
		}
	}
	return activeItem{}, false
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
		if name == "" {
			// Empty stays a silent no-op: dismiss the form, no error.
			m.mode = modeNormal
			m.nameInput.Blur()
			m.status = ""
			return m, m.filter.Focus()
		}
		if err := repo.ValidateWorktreeName(name); err != nil {
			// Reject client-side: keep the form open so the user can fix the
			// name, and surface the reason inline instead of letting it fail
			// later with raw git stderr (or, for "..", escape the repo).
			m.setError("invalid name: " + err.Error())
			return m, nil
		}
		m.mode = modeNormal
		m.nameInput.Blur()
		r := m.pendingRepo
		focusCmd := m.filter.Focus()
		flashTick := m.flashCmd("creating " + name + "…")
		return m, tea.Batch(focusCmd, flashTick, func() tea.Msg {
			wt, err := m.ctrl.Create(r, name, "")
			return createdMsg{wt: wt, err: err}
		})
	}
	var cmd tea.Cmd
	m.nameInput, cmd = m.nameInput.Update(msg)
	return m, cmd
}

// handleConfirmKey resolves the remove-worktree prompt: "y" removes the
// worktree and deletes its branch, "b" removes the worktree but keeps the
// branch, anything else cancels. The target is re-read from the cursor (it
// can't move while the prompt is modal), mirroring the other confirm handlers.
func (m Model) handleConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "y" || key == "b" {
		deleteBranch := key == "y"
		it, ok := m.selectedActive()
		m.mode = modeNormal
		if !ok {
			m.status = ""
			return m, nil
		}
		m.stopWorkspace(wsKey(it.repo.Name, it.view.WT.Name))
		r, wt := it.repo, it.view.WT
		var flashTick tea.Cmd
		if deleteBranch {
			flashTick = m.flashCmd("removing " + wt.Name + "…")
		} else {
			flashTick = m.flashCmd("removing " + wt.Name + " (keeping branch)…")
		}
		remove := func() tea.Msg { return actionDoneMsg{err: m.ctrl.Remove(r, wt, deleteBranch)} }
		return m, tea.Batch(remove, flashTick)
	}
	m.mode = modeNormal
	m.status = ""
	return m, nil
}

// handleConfirmQuitKey resolves the quit guard: "y" quits (running CloseAll,
// which kills every hosted pty), anything else cancels back to the picker.
func (m Model) handleConfirmQuitKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "y" {
		return m, tea.Quit
	}
	m.mode = modeNormal
	m.status = ""
	return m, nil
}

// handleConfirmStopKey resolves the stop guard: "y" stops the selected
// worktree, anything else cancels. The target is re-read from the cursor here
// (it can't move while the prompt is modal), mirroring handleConfirmKey.
func (m Model) handleConfirmStopKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "y" {
		it, ok := m.selectedActive()
		m.mode = modeNormal
		if !ok {
			m.status = ""
			return m, nil
		}
		m.stopWorkspace(wsKey(it.repo.Name, it.view.WT.Name))
		return m, tea.Batch(m.loadCmd(), m.flashCmd("stopped "+it.view.WT.Name))
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

// humanDur formats an elapsed duration compactly for status columns.
func humanDur(d time.Duration) string {
	switch {
	case d < 90*time.Second:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < 90*time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// flash sets a transient info status that the status poll auto-clears after
// transientStatusTTL.
func (m *Model) flash(s string) {
	m.status = s
	m.statusLevel = statusInfo
	m.statusAt = time.Now()
}

// flashCmd sets a transient status like flash and returns a dedicated one-shot
// tick that expires it after transientStatusTTL. The agent-status poll also
// calls maybeExpireStatus, but its cadence stretches to 30s on an idle deck, so
// relying on it alone lets flashes linger far past their TTL; this tick fires on
// its own timer regardless of deck activity. It's stamped with the current
// statusAt so a flash set before it fires won't be cleared early (see
// statusExpireMsg). Callers batch the returned cmd with any command they already
// return.
func (m *Model) flashCmd(s string) tea.Cmd {
	m.flash(s)
	at := m.statusAt
	return tea.Tick(transientStatusTTL, func(time.Time) tea.Msg {
		return statusExpireMsg{at: at}
	})
}

// setError sets a sticky error status: styled red and never auto-expired, it
// stays until the next action replaces or clears it.
func (m *Model) setError(s string) {
	m.status = s
	m.statusLevel = statusError
	m.statusAt = time.Time{}
}

// maybeExpireStatus clears a transient info status once it has been shown for
// transientStatusTTL. It's driven by the status poll, which always
// reschedules itself, so the clear lands within a poll interval.
func (m *Model) maybeExpireStatus() {
	if m.status == "" || m.mode != modeNormal || m.statusAt.IsZero() {
		return
	}
	if m.statusLevel == statusError {
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

// busyHostedAgents counts agents polled as busy whose working directory maps to
// a workspace ct still hosts, optionally filtered to a single workspace key
// (onlyKey == "" counts across every hosted workspace). It backs the quit and
// stop guards: only agents ct hosts are killed by CloseAll/Close, so agents
// running outside ct don't count.
//
// Note: agentStatus is refreshed on a poll timer and can lag reality by up to
// one poll interval, so this may briefly miss an agent that just went busy or
// still flag one that just finished. That staleness is acceptable for a
// confirmation prompt — the guard is a safety net, not a hard lock.
func (m Model) busyHostedAgents(onlyKey string) int {
	if m.mgr == nil {
		return 0
	}
	n := 0
	for _, st := range m.agentStatus {
		if st.Status != "busy" {
			continue
		}
		key, ok := m.pathToKey(st.Cwd)
		if !ok || !m.mgr.Has(key) {
			continue
		}
		if onlyKey != "" && key != onlyKey {
			continue
		}
		n++
	}
	return n
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
